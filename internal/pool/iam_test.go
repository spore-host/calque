package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

const testAccount = "111122223333"

// ---- fakes (calque#226) -----------------------------------------------------

// fakeIAM is an in-memory IAMAPI with real IAM's inline-policy semantics:
// PutRolePolicy REPLACES the named document wholesale, policies are keyed by
// (role, name), and the aggregate size of a role's inline policies (whitespace
// excluded) is capped at budget bytes.
type fakeIAM struct {
	mu       sync.Mutex
	policies map[string]map[string]string // role -> name -> doc
	budget   int
	deletes  []string // "role/name" of every successful DeleteRolePolicy
	// conflicts is how many upcoming PutRolePolicy calls fail with
	// ConcurrentModification before one succeeds.
	conflicts int
	// roleTags/profileTags are the roles' and instance profiles' tags
	// (calque#227). Every role and profile is taken to exist.
	roleTags, profileTags map[string]map[string]string
}

func newFakeIAM() *fakeIAM {
	return &fakeIAM{
		policies: map[string]map[string]string{}, budget: roleInlinePolicyBudget,
		roleTags: map[string]map[string]string{}, profileTags: map[string]map[string]string{},
	}
}

func (f *fakeIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Tags: iamTagList(f.roleTags[awssdk.ToString(in.RoleName)])}}, nil
}

func (f *fakeIAM) GetInstanceProfile(_ context.Context, in *iam.GetInstanceProfileInput, _ ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := awssdk.ToString(in.InstanceProfileName)
	return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{InstanceProfileName: in.InstanceProfileName, Tags: iamTagList(f.profileTags[name])}}, nil
}

func (f *fakeIAM) TagRole(_ context.Context, in *iam.TagRoleInput, _ ...func(*iam.Options)) (*iam.TagRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	addIAMTags(f.roleTags, awssdk.ToString(in.RoleName), in.Tags)
	return &iam.TagRoleOutput{}, nil
}

func (f *fakeIAM) TagInstanceProfile(_ context.Context, in *iam.TagInstanceProfileInput, _ ...func(*iam.Options)) (*iam.TagInstanceProfileOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	addIAMTags(f.profileTags, awssdk.ToString(in.InstanceProfileName), in.Tags)
	return &iam.TagInstanceProfileOutput{}, nil
}

func iamTagList(m map[string]string) []iamtypes.Tag {
	var out []iamtypes.Tag
	for k, v := range m {
		out = append(out, iamtypes.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return out
}

func addIAMTags(into map[string]map[string]string, name string, ts []iamtypes.Tag) {
	if into[name] == nil {
		into[name] = map[string]string{}
	}
	for _, t := range ts {
		into[name][awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
	}
}

func compactLen(doc string) int {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(doc)); err != nil {
		return len(doc)
	}
	return buf.Len()
}

func (f *fakeIAM) seed(role, name, doc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.policies[role] == nil {
		f.policies[role] = map[string]string{}
	}
	f.policies[role][name] = doc
}

func (f *fakeIAM) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	role, name, doc := awssdk.ToString(in.RoleName), awssdk.ToString(in.PolicyName), awssdk.ToString(in.PolicyDocument)
	if f.conflicts > 0 {
		f.conflicts--
		return nil, &iamtypes.ConcurrentModificationException{Message: awssdk.String("The previous operation is still ongoing.")}
	}
	total := compactLen(doc)
	for n, d := range f.policies[role] {
		if n != name {
			total += compactLen(d)
		}
	}
	if total > f.budget {
		return nil, &iamtypes.LimitExceededException{
			Message: awssdk.String(fmt.Sprintf("Maximum policy size of %d bytes exceeded for role %s", f.budget, role)),
		}
	}
	if f.policies[role] == nil {
		f.policies[role] = map[string]string{}
	}
	f.policies[role][name] = doc
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *fakeIAM) DeleteRolePolicy(_ context.Context, in *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	role, name := awssdk.ToString(in.RoleName), awssdk.ToString(in.PolicyName)
	if _, ok := f.policies[role][name]; !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: awssdk.String("The role policy with name " + name + " cannot be found.")}
	}
	delete(f.policies[role], name)
	f.deletes = append(f.deletes, role+"/"+name)
	return &iam.DeleteRolePolicyOutput{}, nil
}

// GetRolePolicy URL-encodes the document, as real IAM does.
func (f *fakeIAM) GetRolePolicy(_ context.Context, in *iam.GetRolePolicyInput, _ ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	role, name := awssdk.ToString(in.RoleName), awssdk.ToString(in.PolicyName)
	doc, ok := f.policies[role][name]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: awssdk.String("The role policy with name " + name + " cannot be found.")}
	}
	return &iam.GetRolePolicyOutput{RoleName: in.RoleName, PolicyName: in.PolicyName, PolicyDocument: awssdk.String(url.PathEscape(doc))}, nil
}

func (f *fakeIAM) ListRolePolicies(_ context.Context, in *iam.ListRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	return &iam.ListRolePoliciesOutput{PolicyNames: f.names(awssdk.ToString(in.RoleName))}, nil
}

func (f *fakeIAM) names(role string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.policies[role]))
	for n := range f.policies[role] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (f *fakeIAM) doc(role, name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.policies[role][name]
	return d, ok
}

// grantsQueue reports whether ANY inline policy on role grants queueARN —
// the question a worker's ReceiveMessage actually depends on, independent of
// how the grants are named.
func (f *fakeIAM) grantsQueue(role, queueARN string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.policies[role] {
		for _, q := range sqsResources(d) {
			if q == queueARN {
				return true
			}
		}
	}
	return false
}

// fakeProfiles models spawn v0.131.0's CreateOrGetInstanceProfile against
// fakeIAM: InlinePolicyJSON, when set, is written under the FIXED name
// "spawn-scoped-policy" (createIAMRole for a new role, updateInlinePolicy for
// an existing one — both PutRolePolicy, both replace), and the spored
// baseline is (re)written on every call.
type fakeProfiles struct {
	iam   *fakeIAM
	mu    sync.Mutex
	calls []spawnaws.IAMRoleConfig
}

func (p *fakeProfiles) CreateOrGetInstanceProfile(ctx context.Context, cfg spawnaws.IAMRoleConfig) (string, error) {
	p.mu.Lock()
	p.calls = append(p.calls, cfg)
	p.mu.Unlock()
	if cfg.InlinePolicyJSON != "" {
		if _, err := p.iam.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName: awssdk.String(cfg.RoleName), PolicyName: awssdk.String(legacySharedPolicyName),
			PolicyDocument: awssdk.String(cfg.InlinePolicyJSON),
		}); err != nil {
			return "", err
		}
	}
	if _, err := p.iam.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: awssdk.String(cfg.RoleName), PolicyName: awssdk.String("spored-baseline-policy"),
		PolicyDocument: awssdk.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["ec2:DescribeTags"],"Resource":"*"}]}`),
	}); err != nil {
		return "", err
	}
	return cfg.RoleName, nil
}

func captureGrantLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := grantLog
	grantLog = &buf
	t.Cleanup(func() { grantLog = prev })
	return &buf
}

func poolCfg(model string) CreateConfig {
	return CreateConfig{Model: model, Region: "us-east-1", ManifestBucket: "manifests-" + model, ResultsBucket: "results-" + model}
}

// ---- tests ------------------------------------------------------------------

// TestQueueGrantPolicyName_Deterministic: provisioning and teardown must
// derive the same name independently (no stored state), and different queues
// must never share one.
func TestQueueGrantPolicyName_Deterministic(t *testing.T) {
	arn := poolQueueARN(testAccount, "us-east-1", "resnet")
	a, b := QueueGrantPolicyName(arn), QueueGrantPolicyName(arn)
	if a != b {
		t.Fatalf("same ARN gave two names: %q vs %q", a, b)
	}
	// Pinned: a change here renames every live grant, so teardown by a newer
	// calque would miss grants written by an older one.
	if want := "q-c49f973559c5"; a != want {
		t.Errorf("QueueGrantPolicyName(%s) = %q, want %q", arn, a, want)
	}
	if !regexp.MustCompile(`^q-[0-9a-f]{12}$`).MatchString(a) {
		t.Errorf("%q is not q-<12 hex>", a)
	}
	others := []string{
		poolQueueARN(testAccount, "us-east-1", "bert"),
		poolQueueARN(testAccount, "us-west-2", "resnet"),
		poolQueueARN("444455556666", "us-east-1", "resnet"),
		runQueueARN(testAccount, "us-east-1", "resnet"),
	}
	for _, o := range others {
		if QueueGrantPolicyName(o) == a {
			t.Errorf("%s and %s share grant name %q", arn, o, a)
		}
	}
}

// TestEnsureWorkerProfile_ConcurrentPoolsKeepBothGrants is the calque#226
// regression: two pools on different queues provisioned at the same time
// under the shared calque-pool-worker role must BOTH end up with their
// queue granted. Before the fix the second write replaced the first.
func TestEnsureWorkerProfile_ConcurrentPoolsKeepBothGrants(t *testing.T) {
	captureGrantLog(t)
	fi := newFakeIAM()
	profiles := &fakeProfiles{iam: fi}

	models := []string{"resnet", "bert"}
	var wg sync.WaitGroup
	errs := make([]error, len(models))
	for i, m := range models {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			_, errs[i] = ensureWorkerProfile(context.Background(), profiles, fi, poolWorkerGrant(testAccount, poolCfg(m)))
		}(i, m)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensureWorkerProfile(%s): %v", models[i], err)
		}
	}

	for _, m := range models {
		arn := poolQueueARN(testAccount, "us-east-1", m)
		if !fi.grantsQueue(PoolWorkerRoleName, arn) {
			t.Errorf("pool %s lost its queue grant: role policies = %v", m, fi.names(PoolWorkerRoleName))
		}
		doc, ok := fi.doc(PoolWorkerRoleName, QueueGrantPolicyName(arn))
		if !ok {
			t.Errorf("pool %s has no %s policy", m, QueueGrantPolicyName(arn))
			continue
		}
		if want := WorkerPolicy(testAccount, "us-east-1", m, "manifests-"+m, "results-"+m); doc != want {
			t.Errorf("pool %s grant = %s, want %s", m, doc, want)
		}
	}
	if _, ok := fi.doc(PoolWorkerRoleName, "spored-baseline-policy"); !ok {
		t.Error("spored-baseline-policy is missing")
	}
	if _, ok := fi.doc(PoolWorkerRoleName, legacySharedPolicyName); ok {
		t.Errorf("the shared %q policy was written; per-queue grants must not use it", legacySharedPolicyName)
	}
	for _, c := range profiles.calls {
		if c.InlinePolicyJSON != "" || c.PolicyFile != "" || len(c.Policies) > 0 {
			t.Errorf("CreateOrGetInstanceProfile got a policy (%+v); spawn would write it under the shared name", c)
		}
		if c.RoleName != PoolWorkerRoleName {
			t.Errorf("role = %q, want %q", c.RoleName, PoolWorkerRoleName)
		}
	}
}

// TestDeletePool_KeepsOtherPoolsGrant: deleting pool A removes A's grant and
// ONLY A's; pool B's grant, the spored baseline, and an unrelated inline
// policy someone else put on the role all survive.
func TestDeletePool_KeepsOtherPoolsGrant(t *testing.T) {
	captureGrantLog(t)
	fi := newFakeIAM()
	profiles := &fakeProfiles{iam: fi}
	for _, m := range []string{"resnet", "bert"} {
		if _, err := ensureWorkerProfile(context.Background(), profiles, fi, poolWorkerGrant(testAccount, poolCfg(m))); err != nil {
			t.Fatal(err)
		}
	}
	fi.seed(PoolWorkerRoleName, "operator-extra", `{"Version":"2012-10-17","Statement":[]}`)

	launch := newFakeLaunchAPI()
	launch.seed("calque-pool-resnet-worker-0", "i-1", "resnet")
	launch.seed("calque-pool-bert-worker-0", "i-2", "bert")
	sqsc := newFakeSQS()
	sqsc.seedQueue(PoolQueueName("resnet"), 0, 0)
	sqsc.seedQueue(PoolQueueName("bert"), 0, 0)

	if err := deletePool(context.Background(), launch, sqsc, fi, testAccount, "resnet", "us-east-1"); err != nil {
		t.Fatalf("deletePool: %v", err)
	}

	resnet := poolQueueARN(testAccount, "us-east-1", "resnet")
	bert := poolQueueARN(testAccount, "us-east-1", "bert")
	if fi.grantsQueue(PoolWorkerRoleName, resnet) {
		t.Error("resnet's grant survived its own pool delete")
	}
	if !fi.grantsQueue(PoolWorkerRoleName, bert) {
		t.Error("deleting resnet removed bert's grant")
	}
	want := []string{"operator-extra", QueueGrantPolicyName(bert), "spored-baseline-policy"}
	sort.Strings(want)
	if got := fi.names(PoolWorkerRoleName); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("role policies after delete = %v, want %v", got, want)
	}
	if strings.Join(fi.deletes, ",") != PoolWorkerRoleName+"/"+QueueGrantPolicyName(resnet) {
		t.Errorf("DeleteRolePolicy calls = %v, want only resnet's grant", fi.deletes)
	}
}

// TestDeletePool_QueueDeleteFailsKeepsGrant: if the queue can't be deleted the
// pool is only half torn down; its grant stays so a retry is consistent.
func TestDeletePool_QueueDeleteFailsKeepsGrant(t *testing.T) {
	captureGrantLog(t)
	fi := newFakeIAM()
	if _, err := ensureWorkerProfile(context.Background(), &fakeProfiles{iam: fi}, fi, poolWorkerGrant(testAccount, poolCfg("resnet"))); err != nil {
		t.Fatal(err)
	}
	sqsc := newFakeSQS()
	sqsc.seedQueue(PoolQueueName("resnet"), 0, 0)
	sqsc.failDelete = true

	if err := deletePool(context.Background(), newFakeLaunchAPI(), sqsc, fi, testAccount, "resnet", "us-east-1"); err == nil {
		t.Fatal("deletePool should fail when the queue delete fails")
	}
	if !fi.grantsQueue(PoolWorkerRoleName, poolQueueARN(testAccount, "us-east-1", "resnet")) {
		t.Error("grant was revoked although the queue still exists")
	}
}

// TestFleetCleanup_RemovesOnlyItsOwnPolicy: a fleet run's cleanup deletes its
// own run-scoped grant from calque-fleet-worker and nothing else, including
// a concurrent run's grant.
func TestFleetCleanup_RemovesOnlyItsOwnPolicy(t *testing.T) {
	captureGrantLog(t)
	fi := newFakeIAM()
	profiles := &fakeProfiles{iam: fi}
	for _, run := range []string{"run-abc", "run-xyz"} {
		cfg := FleetCreateConfig{RunID: run, Region: "us-east-1", ManifestBucket: "b", ResultsBucket: "b"}
		if _, err := ensureWorkerProfile(context.Background(), profiles, fi, fleetWorkerGrant(testAccount, cfg)); err != nil {
			t.Fatal(err)
		}
	}
	before := fi.names(FleetWorkerRoleName)

	if err := revokeFleetWorkerGrant(context.Background(), fi, testAccount, "run-abc", "us-east-1"); err != nil {
		t.Fatalf("revokeFleetWorkerGrant: %v", err)
	}

	abc := runQueueARN(testAccount, "us-east-1", "run-abc")
	xyz := runQueueARN(testAccount, "us-east-1", "run-xyz")
	if fi.grantsQueue(FleetWorkerRoleName, abc) {
		t.Error("run-abc's grant survived its cleanup")
	}
	if !fi.grantsQueue(FleetWorkerRoleName, xyz) {
		t.Error("run-abc's cleanup removed run-xyz's grant")
	}
	after := fi.names(FleetWorkerRoleName)
	if len(after) != len(before)-1 {
		t.Errorf("cleanup removed %d policies (%v -> %v), want exactly 1", len(before)-len(after), before, after)
	}

	// Idempotent: a second cleanup (or one after provisioning failed before
	// the grant was written) is not an error.
	if err := revokeFleetWorkerGrant(context.Background(), fi, testAccount, "run-abc", "us-east-1"); err != nil {
		t.Errorf("second revoke should be a no-op, got %v", err)
	}
}

// TestPutQueueGrant_SizeLimitIsSurfaced: when the role's aggregate inline
// policy budget is full, the error says so (and what to do), and nothing
// broader is written in its place.
func TestPutQueueGrant_SizeLimitIsSurfaced(t *testing.T) {
	captureGrantLog(t)
	fi := newFakeIAM()
	profiles := &fakeProfiles{iam: fi}
	first := poolWorkerGrant(testAccount, poolCfg("resnet"))
	// Room for the baseline and exactly one pool grant.
	fi.budget = compactLen(first.PolicyDoc) + 200

	if _, err := ensureWorkerProfile(context.Background(), profiles, fi, first); err != nil {
		t.Fatalf("first pool should fit: %v", err)
	}
	before := fi.names(PoolWorkerRoleName)

	_, err := ensureWorkerProfile(context.Background(), profiles, fi, poolWorkerGrant(testAccount, poolCfg("bert")))
	if err == nil {
		t.Fatal("second pool should exceed the role's inline-policy budget")
	}
	if !errors.Is(err, ErrRolePolicyBudget) {
		t.Errorf("error does not wrap ErrRolePolicyBudget: %v", err)
	}
	for _, want := range []string{PoolWorkerRoleName, "10240 bytes", "calque pool delete", "1 per-queue grant", "Maximum policy size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if after := fi.names(PoolWorkerRoleName); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("role policies changed on a rejected grant: %v -> %v", before, after)
	}
}

// TestLegacySharedPolicy covers migration from the pre-calque#226 shared
// "spawn-scoped-policy".
func TestLegacySharedPolicy(t *testing.T) {
	resnet := poolCfg("resnet")
	resnetARN := poolQueueARN(testAccount, "us-east-1", "resnet")
	legacyResnet := WorkerPolicy(testAccount, "us-east-1", "resnet", resnet.ManifestBucket, resnet.ResultsBucket)

	t.Run("other queue: kept and logged", func(t *testing.T) {
		logs := captureGrantLog(t)
		fi := newFakeIAM()
		fi.seed(PoolWorkerRoleName, legacySharedPolicyName, legacyResnet)
		if _, err := ensureWorkerProfile(context.Background(), &fakeProfiles{iam: fi}, fi, poolWorkerGrant(testAccount, poolCfg("bert"))); err != nil {
			t.Fatal(err)
		}
		if _, ok := fi.doc(PoolWorkerRoleName, legacySharedPolicyName); !ok {
			t.Error("legacy policy granting another pool's queue was removed")
		}
		if !strings.Contains(logs.String(), "legacy") || !strings.Contains(logs.String(), resnetARN) {
			t.Errorf("legacy policy not logged with its queue: %q", logs.String())
		}
	})

	t.Run("identical to the new grant: removed", func(t *testing.T) {
		captureGrantLog(t)
		fi := newFakeIAM()
		fi.seed(PoolWorkerRoleName, legacySharedPolicyName, legacyResnet)
		if _, err := ensureWorkerProfile(context.Background(), &fakeProfiles{iam: fi}, fi, poolWorkerGrant(testAccount, resnet)); err != nil {
			t.Fatal(err)
		}
		if _, ok := fi.doc(PoolWorkerRoleName, legacySharedPolicyName); ok {
			t.Error("legacy policy duplicating the new per-queue grant was kept")
		}
		if !fi.grantsQueue(PoolWorkerRoleName, resnetARN) {
			t.Error("resnet lost its grant")
		}
	})

	t.Run("its queue deleted: removed", func(t *testing.T) {
		captureGrantLog(t)
		fi := newFakeIAM()
		fi.seed(PoolWorkerRoleName, legacySharedPolicyName, legacyResnet)
		if err := revokeQueueGrant(context.Background(), fi, PoolWorkerRoleName, resnetARN); err != nil {
			t.Fatal(err)
		}
		if _, ok := fi.doc(PoolWorkerRoleName, legacySharedPolicyName); ok {
			t.Error("legacy policy kept after the only queue it granted was deleted")
		}
	})

	t.Run("another queue deleted: kept", func(t *testing.T) {
		captureGrantLog(t)
		fi := newFakeIAM()
		fi.seed(PoolWorkerRoleName, legacySharedPolicyName, legacyResnet)
		if err := revokeQueueGrant(context.Background(), fi, PoolWorkerRoleName, poolQueueARN(testAccount, "us-east-1", "bert")); err != nil {
			t.Fatal(err)
		}
		if _, ok := fi.doc(PoolWorkerRoleName, legacySharedPolicyName); !ok {
			t.Error("legacy policy removed by deleting a queue it does not grant")
		}
	})
}

// TestPutQueueGrant_RetriesConcurrentModification: two pools provisioned at
// once against one role can collide; IAM says wait and retry.
func TestPutQueueGrant_RetriesConcurrentModification(t *testing.T) {
	prev := iamRetryBackoff
	iamRetryBackoff = time.Millisecond
	t.Cleanup(func() { iamRetryBackoff = prev })

	fi := newFakeIAM()
	fi.conflicts = 2
	g := poolWorkerGrant(testAccount, poolCfg("resnet"))
	if err := putQueueGrant(context.Background(), fi, g); err != nil {
		t.Fatalf("putQueueGrant should retry past ConcurrentModification: %v", err)
	}
	if !fi.grantsQueue(PoolWorkerRoleName, g.QueueARN) {
		t.Error("grant not written after retries")
	}
}
