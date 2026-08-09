package pool

import (
	"context"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/spore-host/cohort"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskcohort"
)

// WorkerPolicy builds the scoped inline IAM policy a calque pool worker's
// instance profile needs, mirroring spawn's poolWorkerPolicy shape
// (cmd/pool.go) but scoped to THIS pool's own resources: SQS on the model
// queue only, S3 read on the manifest bucket + read/write on the results
// bucket. A worker never needs write access to the queue itself (Submit is
// the SUBMITTER's operation, not the worker's) — mirrors spawn's own
// least-privilege split.
func WorkerPolicy(account, region, model, manifestBucket, resultsBucket string) string {
	queueARN := fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, account, PoolQueueName(model))
	manifestObj := fmt.Sprintf("arn:aws:s3:::%s/*", manifestBucket)
	manifestBkt := fmt.Sprintf("arn:aws:s3:::%s", manifestBucket)
	resultsObj := fmt.Sprintf("arn:aws:s3:::%s/*", resultsBucket)
	resultsBkt := fmt.Sprintf("arn:aws:s3:::%s", resultsBucket)

	stmts := []string{
		fmt.Sprintf(`{"Effect":"Allow","Action":["sqs:GetQueueUrl","sqs:ReceiveMessage","sqs:DeleteMessage","sqs:GetQueueAttributes"],"Resource":[%q]}`, queueARN),
		fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject"],"Resource":[%q]}`, manifestObj),
		fmt.Sprintf(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%q]}`, manifestBkt),
		fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":[%q]}`, resultsObj),
		fmt.Sprintf(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%q]}`, resultsBkt),
	}
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(stmts, ",") + `]}`
}

// defaultVisibilityTimeout mirrors cmd/calque/pool.go's own
// defaultVisibilityTimeout (900s / 15min) — this package's fallback when a
// caller's CreateConfig.VisibilityTimeout is left at its zero value, so a
// worker's calque#131 heartbeat interval is still sensibly sized even if the
// caller forgot to set it (rather than heartbeating every 0s).
const defaultVisibilityTimeout = 900

// buildWorkerBootstrapCommand emits the on-instance command that runs
// `warmd pool --model M` under the worker's IAM-scoped instance. Unlike
// spawn's buildPoolWorkerCommand, there is no restart-on-error wrapper loop
// here (calque#100's own Worker.Run already tolerates transient claim/fetch
// errors internally without exiting — a non-zero exit from `warmd pool`
// means it exhausted its own retry budget, a genuinely terminal condition
// for THIS worker, not one a shell-level restart should paper over).
func buildWorkerBootstrapCommand(model, region, runnerPath, idleTimeout string, visibilityTimeout int) string {
	return fmt.Sprintf(
		"warmd pool --model %q --region %q --runner-path %q --idle-timeout %q --visibility-timeout %d",
		model, region, runnerPath, idleTimeout, visibilityTimeout,
	)
}

// CreateConfig is everything ProvisionWorkers needs to bring up one pool's
// worker cohort.
type CreateConfig struct {
	Model        string // pool identity (decision 2 of docs/pool-queue-contract.md)
	Region       string
	InstanceType string
	Workers      int // requested worker count
	MinViable    int // <1 => 1; >Workers => Workers (mirrors spawn pool create's clamping)
	Spot         bool
	SpotMaxPrice string
	TTL          string
	IdleTimeout  string
	// VisibilityTimeout MUST match the value passed to CreatePoolQueue for
	// this same model (the actual SQS queue attribute) — ProvisionWorkers
	// only forwards it into each worker's `warmd pool --visibility-timeout`
	// flag so runOne's calque#131 heartbeat interval is sized correctly; it
	// does not itself configure the queue.
	VisibilityTimeout int
	ManifestBucket    string
	ResultsBucket     string
	RunnerPath        string // path to runner.py baked into the worker image
	AMI               string // empty => auto-detect via GetRecommendedAMI, same as spawn pool create
}

// ProvisionWorkers launches a pool's worker cohort via taskcohort + cohort —
// reused UNMODIFIED per docs/pool-queue-contract.md and calque#101's own
// scope: taskcohort.Actuator/Observer/Classifier have zero GPU-specific
// assumptions (confirmed by inspection), so calque points them at a
// `warmd pool` bootstrap instead of spawn's `spored pool-worker`, and
// otherwise reuses the exact partial-cohort/best-effort provisioning shape
// spawn's own `pool create` uses for its fungible CPU workers.
func ProvisionWorkers(ctx context.Context, client *spawnaws.Client, cfg CreateConfig) error {
	if cfg.Workers < 1 {
		return fmt.Errorf("pool: Workers must be >= 1")
	}
	mv := cfg.MinViable
	if mv < 1 {
		mv = 1
	}
	if mv > cfg.Workers {
		mv = cfg.Workers
	}

	ami := cfg.AMI
	if ami == "" {
		a, err := client.GetRecommendedAMI(ctx, cfg.Region, cfg.InstanceType)
		if err != nil {
			return fmt.Errorf("auto-detect worker AMI for %s in %s: %w", cfg.InstanceType, cfg.Region, err)
		}
		ami = a
	}

	account, err := client.GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("resolve account id: %w", err)
	}
	iamProfile, err := client.CreateOrGetInstanceProfile(ctx, spawnaws.IAMRoleConfig{
		RoleName:         "calque-pool-worker",
		TrustServices:    []string{"ec2"},
		InlinePolicyJSON: WorkerPolicy(account, cfg.Region, cfg.Model, cfg.ManifestBucket, cfg.ResultsBucket),
	})
	if err != nil {
		return fmt.Errorf("set up worker IAM instance profile: %w", err)
	}

	visibilityTimeout := cfg.VisibilityTimeout
	if visibilityTimeout <= 0 {
		visibilityTimeout = defaultVisibilityTimeout
	}
	workerCmd := buildWorkerBootstrapCommand(cfg.Model, cfg.Region, cfg.RunnerPath, cfg.IdleTimeout, visibilityTimeout)
	userData, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
		Username:       "ec2-user",
		CustomUserData: workerCmd,
	})
	if err != nil {
		return fmt.Errorf("build worker bootstrap: %w", err)
	}

	ttl := cfg.TTL
	if ttl == "" {
		ttl = "12h" // a pool outlives one run; a generous-but-bounded backstop, not a per-run cap
	}

	base := spawnaws.LaunchConfig{
		InstanceType:       cfg.InstanceType,
		Region:             cfg.Region,
		AMI:                ami,
		IamInstanceProfile: iamProfile,
		Spot:               cfg.Spot,
		SpotMaxPrice:       cfg.SpotMaxPrice,
		TTL:                ttl,
		IdleTimeout:        cfg.IdleTimeout,
		OnComplete:         "terminate",
		UserData:           launcher.EncodeLinuxUserData(userData),
		Tags: map[string]string{
			"calque:pool-model": cfg.Model,
			"calque:role":       "pool-worker",
		},
	}

	act := &taskcohort.Actuator{Client: client, Region: cfg.Region, BaseConfig: base}
	obs := &taskcohort.Observer{Client: client, Region: cfg.Region}

	capacity := cohort.CapacityOnDemand
	if cfg.Spot {
		capacity = cohort.CapacitySpot
	}
	rung := cohort.Rung{InstanceType: cfg.InstanceType, CapacityModel: capacity}

	// CohortID must be unique per provisioning call, but stable across retries of
	// THIS call (idempotency). The model name is stable for the pool's lifetime.
	// ProvisionWorkers itself only ever creates a FRESH cohort numbered from
	// worker-0 — calque#101 shipped create-only, matching spawn pool's own
	// initial scope. Scale-up (adding workers to an already-provisioned pool)
	// is ScaleWorkers, below (calque#115): it reuses this SAME CohortID but
	// numbers only its new members starting from the pool's observed current
	// size, so it never collides with entities ProvisionWorkers already made.
	cohortID := cohort.CohortID("calque-pool-" + cfg.Model)
	members := make([]cohort.EntityIntent, 0, cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		id := cohort.EntityID(fmt.Sprintf("calque-pool-%s-worker-%d", cfg.Model, i))
		intent, err := cohort.NewEntityIntent("pool", id, "g1", cohortID,
			cohort.RungPlacement{Rung: rung, Chain: []cohort.Rung{rung}}, "")
		if err != nil {
			return fmt.Errorf("build worker %d intent: %w", i, err)
		}
		members = append(members, intent)
	}

	c, err := cohort.NewPartialCohort(cohortID, members, cohort.DefaultBudget(), mv, nil)
	if err != nil {
		return fmt.Errorf("build worker cohort: %w", err)
	}

	r := cohort.NewReconciler(act, obs, taskcohort.Classifier{}, nil, nil, nil)
	outcome, err := r.Reconcile(ctx, c)
	if err != nil {
		return fmt.Errorf("provision pool workers: %w", err)
	}
	if !outcome.Ready {
		var details []string
		for _, m := range members {
			rec := outcome.Records[m.ID]
			if !rec.Succeeded() {
				details = append(details, fmt.Sprintf("  %s: %s", m.ID, rec.Summary()))
			}
		}
		return fmt.Errorf("pool failed to reach min viable (%d of %d) workers:\n%s",
			mv, cfg.Workers, strings.Join(details, "\n"))
	}
	return nil
}

// currentWorkerCount discovers how many workers already exist in a pool's
// cohort by probing the deterministic calque-pool-<model>-worker-<i> naming
// scheme sequentially, starting at i=0, until the Observer reports
// cohort.StateUnknown for an ID.
//
// There is no bulk "how many members does cohort X have" call anywhere in
// cohort's or taskcohort's API (confirmed by reading cohort@v0.2.0's
// ports.go/cohort.go/reconcile.go and spawn's pkg/taskcohort/adapter.go):
// cohort.Observer.Observe only accepts an explicit list of EntityIDs to check.
// Per Observer's documented contract, a miss is ALWAYS StateUnknown, never
// StateAbsent — so the first StateUnknown ID reliably marks "one past the
// last existing worker," which is exactly the boundary ScaleWorkers needs to
// continue entity-ID numbering from.
func currentWorkerCount(ctx context.Context, obs *taskcohort.Observer, model string) (int, error) {
	const maxProbe = 10000 // sanity backstop; no realistic pool approaches this size
	for n := 0; n < maxProbe; n++ {
		id := cohort.EntityID(fmt.Sprintf("calque-pool-%s-worker-%d", model, n))
		result, err := obs.Observe(ctx, []cohort.EntityID{id})
		if err != nil {
			return 0, fmt.Errorf("observe worker %d: %w", n, err)
		}
		if len(result) == 0 || result[0].State == cohort.StateUnknown {
			return n, nil
		}
	}
	return 0, fmt.Errorf("pool %q: exceeded worker probe cap (%d) while counting current workers", model, maxProbe)
}

// ScaleWorkers adds addWorkers new workers to an EXISTING pool cohort under
// cfg.Model, without disturbing the workers already running (calque#115).
//
// ProvisionWorkers' own doc comment flagged scale-up as out of scope for
// calque#101: naively re-running it with a larger cfg.Workers would restart
// entity-ID numbering at worker-0, colliding with — and, per cohort's
// Reconcile contract, re-Launching — every already-live worker under the
// same names.
//
// ScaleWorkers instead:
//
//  1. Uses currentWorkerCount to probe the SAME Observer primitive
//     ProvisionWorkers already constructs, to find the cohort's current size.
//  2. Builds EntityIntents ONLY for the new workers, numbered starting at that
//     observed count (current=3, addWorkers=2 => worker-3, worker-4).
//  3. Reconciles a PARTIAL cohort containing ONLY those new members.
//     cohort.Reconciler unconditionally drives every listed Member through
//     Phase 1 (Launch) with no "already running, skip" branch (reconcile.go),
//     so including the pool's pre-existing workers in this call's member list
//     would re-issue Launch against them. Nothing in cohort's Cohort/
//     Reconciler surface distinguishes "add these to an existing set" from
//     "reconcile exactly this set" — the delta-only member list here is what
//     makes that distinction, by construction, the caller's responsibility.
//
// MinViable for the new members is 1: matching ProvisionWorkers' own
// best-effort/eventual philosophy (ask for N, accept whichever came up) rather
// than treating a partial scale-up as an outright failure. The pool's
// pre-existing workers are untouched either way — this call only ever adds.
func ScaleWorkers(ctx context.Context, client *spawnaws.Client, cfg CreateConfig, addWorkers int) error {
	if addWorkers < 1 {
		return fmt.Errorf("pool: addWorkers must be >= 1")
	}

	ami := cfg.AMI
	if ami == "" {
		a, err := client.GetRecommendedAMI(ctx, cfg.Region, cfg.InstanceType)
		if err != nil {
			return fmt.Errorf("auto-detect worker AMI for %s in %s: %w", cfg.InstanceType, cfg.Region, err)
		}
		ami = a
	}

	account, err := client.GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("resolve account id: %w", err)
	}
	iamProfile, err := client.CreateOrGetInstanceProfile(ctx, spawnaws.IAMRoleConfig{
		RoleName:         "calque-pool-worker",
		TrustServices:    []string{"ec2"},
		InlinePolicyJSON: WorkerPolicy(account, cfg.Region, cfg.Model, cfg.ManifestBucket, cfg.ResultsBucket),
	})
	if err != nil {
		return fmt.Errorf("set up worker IAM instance profile: %w", err)
	}

	visibilityTimeout := cfg.VisibilityTimeout
	if visibilityTimeout <= 0 {
		visibilityTimeout = defaultVisibilityTimeout
	}
	workerCmd := buildWorkerBootstrapCommand(cfg.Model, cfg.Region, cfg.RunnerPath, cfg.IdleTimeout, visibilityTimeout)
	userData, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
		Username:       "ec2-user",
		CustomUserData: workerCmd,
	})
	if err != nil {
		return fmt.Errorf("build worker bootstrap: %w", err)
	}

	ttl := cfg.TTL
	if ttl == "" {
		ttl = "12h" // matches ProvisionWorkers' own default backstop
	}

	base := spawnaws.LaunchConfig{
		InstanceType:       cfg.InstanceType,
		Region:             cfg.Region,
		AMI:                ami,
		IamInstanceProfile: iamProfile,
		Spot:               cfg.Spot,
		SpotMaxPrice:       cfg.SpotMaxPrice,
		TTL:                ttl,
		IdleTimeout:        cfg.IdleTimeout,
		OnComplete:         "terminate",
		UserData:           launcher.EncodeLinuxUserData(userData),
		Tags: map[string]string{
			"calque:pool-model": cfg.Model,
			"calque:role":       "pool-worker",
		},
	}

	act := &taskcohort.Actuator{Client: client, Region: cfg.Region, BaseConfig: base}
	obs := &taskcohort.Observer{Client: client, Region: cfg.Region}

	return scaleWorkersCore(ctx, act, obs, cfg.Model, cfg.InstanceType, cfg.Spot, addWorkers)
}

// scaleWorkersCore is ScaleWorkers' delta-numbering + reconcile choreography,
// factored out so it can be exercised in tests against a fake LaunchAPI (the
// same seam taskcohort.Actuator/Observer are already built on) instead of a
// real *spawnaws.Client, which has no test seam of its own — mirroring why
// ProvisionWorkers itself has no direct unit test today (see
// provision_test.go), only its side-effect-free helpers do.
func scaleWorkersCore(ctx context.Context, act *taskcohort.Actuator, obs *taskcohort.Observer, model, instanceType string, spot bool, addWorkers int) error {
	// Same CohortID ProvisionWorkers builds — a scale-up EXPANDS the existing
	// cohort, it must never mint a second one under the same model name (that
	// would collide with the first's entity IDs, exactly what ProvisionWorkers'
	// doc comment warned about).
	cohortID := cohort.CohortID("calque-pool-" + model)

	current, err := currentWorkerCount(ctx, obs, model)
	if err != nil {
		return fmt.Errorf("observe current worker count for pool %q: %w", model, err)
	}

	capacity := cohort.CapacityOnDemand
	if spot {
		capacity = cohort.CapacitySpot
	}
	rung := cohort.Rung{InstanceType: instanceType, CapacityModel: capacity}

	// ONLY the new members go into this cohort. cohort.Reconciler unconditionally
	// drives every listed Member through Launch (reconcile.go's reconcileEntity),
	// so including the pool's pre-existing worker-0..worker-(current-1) here
	// would re-issue Launch against them. Numbering starts at `current`, so these
	// IDs are guaranteed disjoint from every entity ProvisionWorkers (or an
	// earlier ScaleWorkers call) already created.
	members := make([]cohort.EntityIntent, 0, addWorkers)
	for i := current; i < current+addWorkers; i++ {
		id := cohort.EntityID(fmt.Sprintf("calque-pool-%s-worker-%d", model, i))
		intent, err := cohort.NewEntityIntent("pool", id, "g1", cohortID,
			cohort.RungPlacement{Rung: rung, Chain: []cohort.Rung{rung}}, "")
		if err != nil {
			return fmt.Errorf("build worker %d intent: %w", i, err)
		}
		members = append(members, intent)
	}

	c, err := cohort.NewPartialCohort(cohortID, members, cohort.DefaultBudget(), 1, nil)
	if err != nil {
		return fmt.Errorf("build scale-up cohort: %w", err)
	}

	r := cohort.NewReconciler(act, obs, taskcohort.Classifier{}, nil, nil, nil)
	outcome, err := r.Reconcile(ctx, c)
	if err != nil {
		return fmt.Errorf("scale pool workers: %w", err)
	}
	if !outcome.Ready {
		var details []string
		for _, m := range members {
			rec := outcome.Records[m.ID]
			if !rec.Succeeded() {
				details = append(details, fmt.Sprintf("  %s: %s", m.ID, rec.Summary()))
			}
		}
		return fmt.Errorf("pool scale-up failed to bring up any of %d requested new worker(s) starting at worker-%d:\n%s",
			addWorkers, current, strings.Join(details, "\n"))
	}
	return nil
}

// poolModelTag is the instance tag ProvisionWorkers stamps on every worker
// (see CreateConfig's base.Tags in ProvisionWorkers above) — the ONLY
// mechanism DeletePool/PoolStatus have for finding a pool's workers again.
// There is no separate pool registry: a pool's membership is discovered by
// re-listing spawn-managed instances and filtering on this tag, exactly the
// way ProvisionWorkers's own taskcohort.Observer discovers state by re-listing
// and matching on the Name tag.
const poolModelTag = "calque:pool-model"

// discoverPoolWorkerIDs lists every instance currently tagged as belonging to
// this pool (calque:pool-model == model) and returns their EntityIDs — the
// worker Name tag ProvisionWorkers assigned each one
// ("calque-pool-<model>-worker-<i>"). Returns an empty (non-nil) slice, not an
// error, when the pool has no workers: "zero live workers" is a normal state
// for a pool that was scaled down or never finished provisioning, not a fault.
//
// Takes taskcohort.LaunchAPI (the interface, not the concrete *spawnaws.Client)
// so it — and everything built on it below — is unit-testable against an
// in-memory fake, the same seam taskcohort's own adapter_test.go uses for
// ProvisionWorkers's Actuator/Observer.
func discoverPoolWorkerIDs(ctx context.Context, client taskcohort.LaunchAPI, model, region string) ([]cohort.EntityID, error) {
	insts, err := client.ListInstances(ctx, region, "")
	if err != nil {
		return nil, fmt.Errorf("list instances for pool %q: %w", model, err)
	}
	ids := make([]cohort.EntityID, 0, len(insts))
	for _, in := range insts {
		if in.Tags[poolModelTag] == model {
			ids = append(ids, cohort.EntityID(in.Name))
		}
	}
	return ids, nil
}

// DeletePool tears down a pool completely (calque#130): every worker instance
// is terminated AND the pool's SQS queue is deleted. This is the operation
// internal/pool/queue.go's own CreatePoolQueue comment promises but that,
// until now, calque had no command for ("its lifetime is 'as long as the
// pool exists,' ended by an explicit `calque pool delete`, not time").
//
// Cohort teardown: cohort (v0.2.0) has no "scale a cohort to zero" or
// "terminate cohort" constructor — NewPartialCohort/NewMPICohort/
// NewSerialCohort all reject an empty member list, because Reconcile's whole
// job is converging a set of entities INTO existence, not tearing one down.
// The primitive that DOES exist for exactly this is Reconciler.Drain(ctx,
// ids []EntityID): it is already exported (reconcile.go), and Reconcile
// itself calls it internally on every failure path to terminate survivors
// so nothing idles and bills. Drain does exactly what a teardown needs: call
// Actuator.Terminate on each named entity, best-effort (continues past a
// single instance's Terminate error so one bad instance can't block the
// rest), returning the last error if any occurred. DeletePool reuses it
// directly instead of re-deriving Terminate's fan-out.
//
// The pool's members are rediscovered via discoverPoolWorkerIDs rather than
// reconstructed from a stored CreateConfig.Workers count — calque keeps no
// pool registry, so "how many workers does this pool have right now" is only
// ever answered by asking the provider, exactly as ProvisionWorkers's own
// Observer does for state.
//
// DeletePool itself is a thin wrapper around deletePool that resolves the
// SQS client from client.Config() — the one step that needs the concrete
// *spawnaws.Client rather than an interface. All the actual logic lives in
// deletePool, which takes taskcohort.LaunchAPI + taskpool.SQSAPI so tests can
// fake both without real AWS.
func DeletePool(ctx context.Context, client *spawnaws.Client, model, region string) error {
	return deletePool(ctx, client, sqs.NewFromConfig(client.Config()), model, region)
}

func deletePool(ctx context.Context, client taskcohort.LaunchAPI, sqsClient SQSAPI, model, region string) error {
	ids, err := discoverPoolWorkerIDs(ctx, client, model, region)
	if err != nil {
		return fmt.Errorf("discover pool %q workers: %w", model, err)
	}

	var drainErr error
	if len(ids) > 0 {
		act := &taskcohort.Actuator{Client: client, Region: region}
		r := cohort.NewReconciler(act, nil, nil, nil, nil, nil)
		if err := r.Drain(ctx, ids); err != nil {
			drainErr = fmt.Errorf("terminate pool %q workers: %w", model, err)
		}
	}

	if err := DeletePoolQueueIfExists(ctx, sqsClient, model); err != nil {
		if drainErr != nil {
			return fmt.Errorf("%w (also failed to delete queue: %v)", drainErr, err)
		}
		return fmt.Errorf("delete pool %q queue: %w", model, err)
	}
	return drainErr
}

// Status is the answer to "what is this pool doing right now" — the read side
// of pool lifecycle management, filled in by PoolStatus. There is no
// authoritative pool registry (see discoverPoolWorkerIDs); both fields are
// live snapshots re-derived from the provider on every call, not cached
// state, so two calls in a row can legitimately disagree.
type Status struct {
	// Model is the pool identity this status describes, echoed back for
	// display convenience (callers already know it — they supplied it).
	Model string
	// WorkerCount is the number of this pool's workers currently observed as
	// StateRunning — i.e. actually up and able to claim work, not merely
	// launching or stopped. A worker mid-launch is real infrastructure spend
	// but not yet claim-capacity, which is what an operator checking "is my
	// pool ready" actually wants to know.
	WorkerCount int
	// QueueDepth is claims currently outstanding on the pool's queue: visible
	// (waiting to be claimed) plus in-flight (claimed, not yet acked) —
	// ApproximateNumberOfMessages + ApproximateNumberOfMessagesNotVisible.
	// Both count as "not yet done," which is the operator-relevant question
	// ("how much work is still in front of my pool"), not just "how much is
	// sitting unclaimed."
	QueueDepth int
	// QueueExists reports whether the pool's queue was found at all — false
	// for a pool that was deleted (or never created), so a caller can
	// distinguish "0 depth, healthy and idle" from "0 depth, there's no
	// queue here."
	QueueExists bool
}

// PoolStatus reports a pool's current worker count and queue depth
// (calque#130's status/list primitive). Both figures are point-in-time
// snapshots from the provider (EC2 DescribeInstances + SQS
// GetQueueAttributes), each individually eventually-consistent — fine for a
// human-facing status line, not a claim it should be used as a scheduling
// input.
//
// Like DeletePool, PoolStatus is a thin wrapper resolving the SQS client from
// client.Config(); poolStatus carries the actual logic against the
// taskcohort.LaunchAPI/taskpool.SQSAPI interfaces for testability.
func PoolStatus(ctx context.Context, client *spawnaws.Client, model, region string) (Status, error) {
	return poolStatus(ctx, client, sqs.NewFromConfig(client.Config()), model, region)
}

func poolStatus(ctx context.Context, client taskcohort.LaunchAPI, sqsClient SQSAPI, model, region string) (Status, error) {
	status := Status{Model: model}

	ids, err := discoverPoolWorkerIDs(ctx, client, model, region)
	if err != nil {
		return Status{}, fmt.Errorf("discover pool %q workers: %w", model, err)
	}
	if len(ids) > 0 {
		obs := &taskcohort.Observer{Client: client, Region: region}
		observations, err := obs.Observe(ctx, ids)
		if err != nil {
			return Status{}, fmt.Errorf("observe pool %q workers: %w", model, err)
		}
		for _, o := range observations {
			if o.State == cohort.StateRunning {
				status.WorkerCount++
			}
		}
	}

	visible, inFlight, exists, err := poolQueueDepth(ctx, sqsClient, model)
	if err != nil {
		return Status{}, fmt.Errorf("query pool %q queue depth: %w", model, err)
	}
	status.QueueExists = exists
	status.QueueDepth = visible + inFlight
	return status, nil
}

// poolQueueDepth looks up a pool's queue depth by model name, tolerating the
// queue not existing (exists=false, no error) — a status/list call must not
// fail, or block on OpenPoolQueue's up-to-~27s eventual-consistency retry
// loop, just because a pool was never created or was already deleted; that
// retry budget exists to wait out a FRESH CreatePoolQueue's propagation lag,
// which has no bearing here. A single GetQueueUrl lookup (same as
// DeletePoolQueueIfExists) is the right check for "does it exist right now."
func poolQueueDepth(ctx context.Context, client SQSAPI, model string) (visible, inFlight int, exists bool, err error) {
	name := PoolQueueName(model)
	out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: awssdk.String(name)})
	if err != nil {
		if isNonExistentQueue(err) {
			return 0, 0, false, nil
		}
		return 0, 0, false, fmt.Errorf("resolve pool queue %s: %w", name, err)
	}
	q := &SQSQueue{client: client, url: awssdk.ToString(out.QueueUrl)}
	visible, inFlight, err = q.Depth(ctx)
	if err != nil {
		return 0, 0, true, err
	}
	return visible, inFlight, true, nil
}
