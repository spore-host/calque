package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// Per-queue worker grants (calque#226).
//
// calque-pool-worker and calque-fleet-worker are SHARED, account-wide role
// names: every pool (and every fleet run) launches its workers under the same
// role. Until calque#226 each provisioning call handed its one-queue policy to
// spawn's CreateOrGetInstanceProfile as InlinePolicyJSON. For an existing role
// spawn's updateInlinePolicy (pkg/aws/iam.go, spawn v0.131.0) writes that
// document with PutRolePolicy under the FIXED name "spawn-scoped-policy",
// which REPLACES the previous document wholesale. So `pool create --model b`
// silently revoked pool a's queue grant, and pool a's workers began failing
// ReceiveMessage with AccessDenied. Fleet runs raced the same way.
//
// calque#167 fixed the same class of race for real runs by giving each bucket
// its own role (internal/plan/iam.go roleNameForBucket). That doesn't fit here:
// a role per pool/fleet run would mint an IAM role and instance profile per
// run id, which nothing reaps. Instead the role stays shared and each queue
// gets its OWN inline policy, named deterministically from the queue ARN
// (QueueGrantPolicyName). It is written when the pool or fleet is provisioned
// and deleted when that pool is deleted or that fleet run cleans up. Every
// other inline policy on the role (spored-baseline-policy, another queue's
// grant) is left alone.

const (
	// PoolWorkerRoleName is the shared role every pool's workers run under.
	PoolWorkerRoleName = "calque-pool-worker"
	// FleetWorkerRoleName is the shared role every fleet run's workers run under.
	FleetWorkerRoleName = "calque-fleet-worker"

	// legacySharedPolicyName is the inline policy pre-calque#226 calque wrote
	// through spawn's CreateOrGetInstanceProfile (IAMRoleConfig.InlinePolicyJSON
	// lands under this name). It holds whichever queue was provisioned LAST.
	legacySharedPolicyName = "spawn-scoped-policy"

	// queueGrantPrefix marks a calque per-queue grant. Only policies with this
	// prefix are ever deleted by calque, and only the one for the queue being
	// torn down.
	queueGrantPrefix = "q-"

	// roleInlinePolicyBudget is IAM's aggregate inline-policy size limit per
	// role (whitespace excluded), quoted in the error a caller sees when a
	// grant doesn't fit.
	roleInlinePolicyBudget = 10240
)

// IAMAPI is the slice of the IAM SDK the per-queue grants need. A real
// *iam.Client satisfies it; tests use an in-memory fake.
type IAMAPI interface {
	PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, opts ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, opts ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	GetRolePolicy(ctx context.Context, in *iam.GetRolePolicyInput, opts ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	ListRolePolicies(ctx context.Context, in *iam.ListRolePoliciesInput, opts ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
}

// instanceProfileEnsurer is the one *spawnaws.Client method worker
// provisioning needs from spawn's IAM helper: create-or-reuse the shared role
// and its instance profile, with SSM and the spored baseline attached.
type instanceProfileEnsurer interface {
	CreateOrGetInstanceProfile(ctx context.Context, config spawnaws.IAMRoleConfig) (string, error)
}

// grantLog is where the legacy-policy notes go. A variable so tests can
// capture it.
var grantLog io.Writer = os.Stderr

// poolQueueARN is the ARN of a pool's model-scoped queue.
func poolQueueARN(account, region, model string) string {
	return fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, account, PoolQueueName(model))
}

// runQueueARN is the ARN of a fleet run's run-scoped queue.
func runQueueARN(account, region, runID string) string {
	return fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, account, RunQueueName(runID))
}

// QueueGrantPolicyName is the inline-policy name of the grant for one queue:
// "q-" plus the first 12 hex characters of SHA-256(queue ARN). Deterministic,
// so provisioning and teardown agree on the name without any stored state,
// and IAM-legal whatever characters the model or run id contains. The ARN,
// not the bare queue name, is hashed so the same model in two regions or two
// accounts gets two grants.
func QueueGrantPolicyName(queueARN string) string {
	h := sha256.Sum256([]byte(queueARN))
	return queueGrantPrefix + hex.EncodeToString(h[:])[:12]
}

// ErrRolePolicyBudget is returned (wrapped) when a queue grant doesn't fit in
// the role's aggregate inline-policy budget.
var ErrRolePolicyBudget = errors.New("IAM role inline-policy size limit reached")

// workerGrant is one queue's grant on a shared worker role.
type workerGrant struct {
	Role      string
	QueueARN  string
	PolicyDoc string
}

// ensureWorkerProfile resolves the shared worker role's instance profile and
// writes THIS queue's grant onto it as its own inline policy.
//
// CreateOrGetInstanceProfile is called WITHOUT InlinePolicyJSON on purpose:
// with it, spawn writes the grant under the shared "spawn-scoped-policy" name
// and the last caller wins (calque#226). Without it spawn still creates the
// role and profile if missing and attaches SSM and spored-baseline-policy,
// but touches no other inline policy.
func ensureWorkerProfile(ctx context.Context, profiles instanceProfileEnsurer, iamc IAMAPI, g workerGrant) (string, error) {
	profile, err := profiles.CreateOrGetInstanceProfile(ctx, spawnaws.IAMRoleConfig{
		RoleName:      g.Role,
		TrustServices: []string{"ec2"},
	})
	if err != nil {
		return "", fmt.Errorf("set up worker IAM instance profile: %w", err)
	}
	if err := putQueueGrant(ctx, iamc, g); err != nil {
		return "", err
	}
	noteLegacyGrant(ctx, iamc, g)
	return profile, nil
}

// putQueueGrant writes g's policy under QueueGrantPolicyName(g.QueueARN).
// Idempotent: re-provisioning (or scaling) the same pool rewrites its own
// grant and nobody else's.
func putQueueGrant(ctx context.Context, iamc IAMAPI, g workerGrant) error {
	name := QueueGrantPolicyName(g.QueueARN)
	err := retryIAM(ctx, func() error {
		_, e := iamc.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       awssdk.String(g.Role),
			PolicyName:     awssdk.String(name),
			PolicyDocument: awssdk.String(g.PolicyDoc),
		})
		return e
	})
	if err == nil {
		return nil
	}
	if isPolicySizeLimit(err) {
		grants := countQueueGrants(ctx, iamc, g.Role)
		advice := "Delete pools you no longer need (`calque pool delete`) and retry."
		if g.Role == FleetWorkerRoleName {
			advice = "Let other fleet runs finish (each removes its own grant on cleanup) and retry."
		}
		return fmt.Errorf("%w: cannot add grant %s for %s to role %s: IAM allows %d bytes of inline policy per role in total, "+
			"and the role already carries %s. %s calque does not fall back to a broader grant: %v",
			ErrRolePolicyBudget, name, g.QueueARN, g.Role, roleInlinePolicyBudget, grants, advice, err)
	}
	return fmt.Errorf("grant role %s access to %s (inline policy %s): %w", g.Role, g.QueueARN, name, err)
}

// revokeQueueGrant deletes ONLY the grant for queueARN from role. A grant that
// is already gone is success, so teardown is idempotent. It then retires the
// legacy shared policy if that policy granted nothing but this same queue.
func revokeQueueGrant(ctx context.Context, iamc IAMAPI, role, queueARN string) error {
	name := QueueGrantPolicyName(queueARN)
	err := retryIAM(ctx, func() error {
		_, e := iamc.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
			RoleName:   awssdk.String(role),
			PolicyName: awssdk.String(name),
		})
		return e
	})
	if err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("revoke role %s access to %s (inline policy %s): %w", role, queueARN, name, err)
	}
	retireLegacyGrant(ctx, iamc, role, queueARN)
	return nil
}

// noteLegacyGrant handles a role still carrying the pre-calque#226
// "spawn-scoped-policy". If that document is exactly the grant just written
// under the per-queue name, nothing depends on the legacy copy and it is
// deleted. Otherwise it may be the only grant some pre-#226 pool's workers
// still hold, so it is left in place and logged.
//
// Best-effort throughout: a failure to read or delete the legacy policy is
// logged, never returned, since the new grant is already in place.
func noteLegacyGrant(ctx context.Context, iamc IAMAPI, g workerGrant) {
	doc, ok := readLegacyGrant(ctx, iamc, g.Role)
	if !ok {
		return
	}
	if sameJSON(doc, g.PolicyDoc) {
		deleteLegacyGrant(ctx, iamc, g.Role, "it duplicates "+QueueGrantPolicyName(g.QueueARN))
		return
	}
	queues := sqsResources(doc)
	_, _ = fmt.Fprintf(grantLog, "[iam] role %s still carries the legacy shared inline policy %q (granting %s), written by calque before calque#226. "+
		"Left in place: a pool or fleet created by an older calque may still depend on it. It is removed when that queue's pool is deleted.\n",
		g.Role, legacySharedPolicyName, describeQueues(queues))
}

// retireLegacyGrant deletes the legacy shared policy once the queue it grants
// has been torn down. The legacy document only ever granted ONE queue (the
// last one provisioned before calque#226); once that queue is deleted, no
// worker can use the grant, so removing it can't break a live pool. A legacy
// policy granting any OTHER queue is left alone.
func retireLegacyGrant(ctx context.Context, iamc IAMAPI, role, deletedQueueARN string) {
	doc, ok := readLegacyGrant(ctx, iamc, role)
	if !ok {
		return
	}
	queues := sqsResources(doc)
	if len(queues) == 1 && queues[0] == deletedQueueARN {
		deleteLegacyGrant(ctx, iamc, role, "the only queue it granted, "+deletedQueueARN+", is gone")
		return
	}
	_, _ = fmt.Fprintf(grantLog, "[iam] role %s still carries the legacy shared inline policy %q (granting %s); left in place, it does not belong to %s.\n",
		role, legacySharedPolicyName, describeQueues(queues), deletedQueueARN)
}

// readLegacyGrant returns the legacy policy document, or ok=false if the role
// has none (or it can't be read, which is logged).
func readLegacyGrant(ctx context.Context, iamc IAMAPI, role string) (string, bool) {
	out, err := iamc.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
		RoleName:   awssdk.String(role),
		PolicyName: awssdk.String(legacySharedPolicyName),
	})
	if err != nil {
		if !isNoSuchEntity(err) {
			_, _ = fmt.Fprintf(grantLog, "[iam] could not check role %s for the legacy shared inline policy %q (left as is): %v\n",
				role, legacySharedPolicyName, err)
		}
		return "", false
	}
	doc := awssdk.ToString(out.PolicyDocument)
	// GetRolePolicy returns the document URL-encoded (RFC 3986).
	if decoded, derr := url.PathUnescape(doc); derr == nil {
		doc = decoded
	}
	return doc, true
}

func deleteLegacyGrant(ctx context.Context, iamc IAMAPI, role, why string) {
	_, err := iamc.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
		RoleName:   awssdk.String(role),
		PolicyName: awssdk.String(legacySharedPolicyName),
	})
	if err != nil && !isNoSuchEntity(err) {
		_, _ = fmt.Fprintf(grantLog, "[iam] could not remove the legacy shared inline policy %q from role %s (left in place): %v\n",
			legacySharedPolicyName, role, err)
		return
	}
	_, _ = fmt.Fprintf(grantLog, "[iam] removed the legacy shared inline policy %q from role %s: %s.\n", legacySharedPolicyName, role, why)
}

// sqsResources returns the SQS ARNs a policy document grants, sorted and
// de-duplicated. An unparsable document yields nil, which the callers treat
// as "can't tell", i.e. leave it alone.
func sqsResources(doc string) []string {
	var p struct {
		Statement []struct {
			Resource json.RawMessage `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, s := range p.Statement {
		var many []string
		if err := json.Unmarshal(s.Resource, &many); err != nil {
			var one string
			if err := json.Unmarshal(s.Resource, &one); err != nil {
				continue
			}
			many = []string{one}
		}
		for _, r := range many {
			if strings.HasPrefix(r, "arn:aws:sqs:") {
				seen[r] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func describeQueues(queues []string) string {
	if len(queues) == 0 {
		return "no recognisable SQS queue"
	}
	return strings.Join(queues, ", ")
}

// sameJSON reports whether two JSON documents are equal ignoring formatting.
func sameJSON(a, b string) bool {
	var va, vb any
	if json.Unmarshal([]byte(a), &va) != nil || json.Unmarshal([]byte(b), &vb) != nil {
		return false
	}
	ja, _ := json.Marshal(va)
	jb, _ := json.Marshal(vb)
	return string(ja) == string(jb)
}

// countQueueGrants describes how many per-queue grants the role carries, for
// the size-limit message. Best-effort: on a listing error it says so.
func countQueueGrants(ctx context.Context, iamc IAMAPI, role string) string {
	var grants int
	legacy := false
	in := &iam.ListRolePoliciesInput{RoleName: awssdk.String(role)}
	for {
		out, err := iamc.ListRolePolicies(ctx, in)
		if err != nil {
			return "an unknown number of queue grants (could not list them: " + err.Error() + ")"
		}
		for _, n := range out.PolicyNames {
			switch {
			case strings.HasPrefix(n, queueGrantPrefix):
				grants++
			case n == legacySharedPolicyName:
				legacy = true
			}
		}
		if !out.IsTruncated || out.Marker == nil {
			break
		}
		in.Marker = out.Marker
	}
	s := fmt.Sprintf("%d per-queue grant(s) (%s*)", grants, queueGrantPrefix)
	if legacy {
		s += fmt.Sprintf(" plus the legacy shared policy %q", legacySharedPolicyName)
	}
	return s
}

// isPolicySizeLimit recognises IAM's aggregate-size rejection:
// LimitExceeded "Maximum policy size of 10240 bytes exceeded for role ...".
func isPolicySizeLimit(err error) bool {
	var lee *iamtypes.LimitExceededException
	if errors.As(err, &lee) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && strings.HasPrefix(apiErr.ErrorCode(), "LimitExceeded") {
		return true
	}
	return strings.Contains(err.Error(), "Maximum policy size")
}

// iamRetryBackoff is the base delay between retries; a variable so tests
// don't sleep.
var iamRetryBackoff = 500 * time.Millisecond

// retryIAM retries fn on throttling and ConcurrentModification, which IAM
// documents as wait-and-retry conditions and which concurrent provisioning
// against one shared role can hit. Mirrors spawn's own retryIAM backoff
// (500 ms x attempt, 5 attempts), but stops early if ctx is done.
func retryIAM(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		if err = fn(); err == nil || !isRetryableIAM(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt) * iamRetryBackoff):
		}
	}
	return err
}

func isRetryableIAM(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return strings.HasPrefix(code, "Throttling") || strings.HasPrefix(code, "ConcurrentModification") ||
			strings.HasPrefix(code, "RequestLimitExceeded")
	}
	return false
}

func isNoSuchEntity(err error) bool {
	var nse *iamtypes.NoSuchEntityException
	if errors.As(err, &nse) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && strings.HasPrefix(apiErr.ErrorCode(), "NoSuchEntity")
}

// RevokeFleetWorkerGrant deletes a fleet run's queue grant from the shared
// fleet worker role (calque#226). fleetRun calls it during cleanup, after the
// run's workers are drained and its queue deleted. Idempotent.
func RevokeFleetWorkerGrant(ctx context.Context, client *spawnaws.Client, runID, region string) error {
	account, err := client.GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("resolve account id: %w", err)
	}
	return revokeFleetWorkerGrant(ctx, iam.NewFromConfig(client.Config()), account, runID, region)
}

func revokeFleetWorkerGrant(ctx context.Context, iamc IAMAPI, account, runID, region string) error {
	return revokeQueueGrant(ctx, iamc, FleetWorkerRoleName, runQueueARN(account, region, runID))
}
