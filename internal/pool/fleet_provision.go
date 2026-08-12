package pool

import (
	"context"
	"fmt"
	"strings"

	"github.com/spore-host/cohort"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskcohort"
)

// FleetWorkerPolicy is WorkerPolicy's run-scoped sibling (calque#145):
// mirrors its exact shape, scoped to a fleet run's OWN queue (by run id, via
// RunQueueName) instead of a pool's model-scoped one. A fleet worker never
// needs write access to the queue itself either — Submit is fleetRun's
// (the submitter's) operation, matching WorkerPolicy's own least-privilege
// split.
func FleetWorkerPolicy(account, region, runID, manifestBucket, resultsBucket string) string {
	queueARN := fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, account, RunQueueName(runID))
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

// buildFleetWorkerBootstrapCommand emits the on-instance command that runs
// `warmd fleet --run-id R` — buildWorkerBootstrapCommand's sibling for the
// fleet case (--run-id instead of --model, no separate model flag since a
// fleet worker only ever knows its run id).
//
// Unlike pool mode (whose --runner-path help text says "path to runner.py
// IN THE WORKER IMAGE" — pool workers assume a pre-baked AMI, matching
// spawn's own pool create precedent), fleetRun's existing single-shard
// path (runShard/BootstrapConfig.Command, internal/exec/bootstrap.go)
// builds+uploads warmd/runner.py/occupancy.py fresh every run and never
// requires a pre-baked AMI. artifactS3URI (calque#145 slice 2) is that
// SAME already-uploaded S3 prefix (fleetRun's existing UploadArtifacts
// call, unchanged) — syncing it here before invoking `warmd fleet`
// preserves that "no pre-baked AMI needed" property for fleet workers
// too, mirroring BootstrapConfig.Command's own sync-then-invoke shape
// (mkdir, aws s3 cp --recursive, chmod +x) rather than assuming the
// binary is already present.
func buildFleetWorkerBootstrapCommand(runID, region, artifactS3URI, workerDir, runnerPath, idleTimeout string, visibilityTimeout int) string {
	warmdCmd := fmt.Sprintf(
		"warmd fleet --run-id %q --region %q --runner-path %q --idle-timeout %q --visibility-timeout %d",
		runID, region, runnerPath, idleTimeout, visibilityTimeout,
	)
	lines := []string{
		"#!/bin/bash",
		"exec > /tmp/calque-fleet-worker-bootstrap.log 2>&1",
		"set -euxo pipefail",
		"command -v aws >/dev/null || (sudo apt-get update && sudo apt-get install -y awscli)",
		"command -v python3 >/dev/null || (sudo apt-get update && sudo apt-get install -y python3)",
		fmt.Sprintf("mkdir -p %s", workerDir),
		fmt.Sprintf("aws s3 cp --recursive %s/ %s/", strings.TrimSuffix(artifactS3URI, "/"), workerDir),
		fmt.Sprintf("chmod +x %s/warmd", workerDir),
		fmt.Sprintf("cd %s && ./%s", workerDir, warmdCmd),
	}
	return strings.Join(lines, "\n")
}

// FleetCreateConfig is ProvisionFleetWorkers' input — CreateConfig's
// run-scoped sibling (calque#145): RunID replaces Model as the identity
// every naming/tagging/policy decision keys off, and there is no
// InstanceType-implied "outlives one run" default the way CreateConfig.TTL
// defaults to 12h — a fleet run's workers are bounded by the SAME --ttl the
// caller already passes to a single-shard acquire today.
type FleetCreateConfig struct {
	RunID        string // fleet run identity (calque#145) — the ONLY identity a fleet worker/queue/policy is scoped by
	Region       string
	InstanceType string
	Workers      int // requested worker count (today: resolveFleetCeiling's output)
	MinViable    int // <1 => 1; >Workers => Workers (mirrors ProvisionWorkers' own clamping)
	Spot         bool
	SpotMaxPrice string
	TTL          string
	IdleTimeout  string
	// VisibilityTimeout MUST match the value passed to CreateRunQueue for
	// this same run — mirrors CreateConfig.VisibilityTimeout's own contract.
	VisibilityTimeout int
	ManifestBucket    string
	ResultsBucket     string
	// ArtifactS3URI is the S3 prefix fleetRun's OWN calexec.UploadArtifacts
	// call already uploaded warmd/runner.py/occupancy.py to (calque#145
	// slice 2) — synced onto each worker at boot so fleet mode, like
	// fleetRun's existing single-shard path, needs no pre-baked AMI.
	ArtifactS3URI string
	// WorkerDir is where artifacts land on the worker (e.g. "/tmp/calque"),
	// mirroring internal/exec.BootstrapConfig.WorkerDir. RunnerPath below is
	// resolved relative to this at boot ("<WorkerDir>/runner.py"), NOT a
	// pre-existing image path the way pool mode's RunnerPath is.
	WorkerDir  string
	RunnerPath string
	AMI        string
}

// fleetWorkerTag is the instance tag ProvisionFleetWorkers stamps on every
// worker — poolModelTag's run-scoped sibling, and the ONLY mechanism
// DrainFleetWorkers has for finding a run's workers again (calque keeps no
// fleet registry, mirroring pool mode's own no-registry design).
const fleetWorkerTag = "calque:fleet-run"

// ProvisionFleetWorkers launches a fleet run's worker cohort via
// taskcohort + cohort — ProvisionWorkers' run-scoped sibling (calque#145),
// reusing the exact same partial-cohort/best-effort provisioning shape,
// just keyed by RunID instead of Model and pointed at `warmd fleet` instead
// of `warmd pool`.
func ProvisionFleetWorkers(ctx context.Context, client *spawnaws.Client, cfg FleetCreateConfig) error {
	if cfg.Workers < 1 {
		return fmt.Errorf("fleet: Workers must be >= 1")
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
		RoleName:         "calque-fleet-worker",
		TrustServices:    []string{"ec2"},
		InlinePolicyJSON: FleetWorkerPolicy(account, cfg.Region, cfg.RunID, cfg.ManifestBucket, cfg.ResultsBucket),
	})
	if err != nil {
		return fmt.Errorf("set up worker IAM instance profile: %w", err)
	}

	visibilityTimeout := cfg.VisibilityTimeout
	if visibilityTimeout <= 0 {
		visibilityTimeout = defaultVisibilityTimeout
	}
	workerDir := cfg.WorkerDir
	if workerDir == "" {
		workerDir = "/tmp/calque-fleet"
	}
	runnerPath := cfg.RunnerPath
	if runnerPath == "" {
		runnerPath = workerDir + "/runner.py"
	}
	workerCmd := buildFleetWorkerBootstrapCommand(cfg.RunID, cfg.Region, cfg.ArtifactS3URI, workerDir, runnerPath, cfg.IdleTimeout, visibilityTimeout)
	userData, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
		Username:       "ec2-user",
		CustomUserData: workerCmd,
	})
	if err != nil {
		return fmt.Errorf("build worker bootstrap: %w", err)
	}

	ttl := cfg.TTL
	if ttl == "" {
		ttl = "2h" // a fleet run does not outlive one run the way a pool does; no 12h backstop needed
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
			fleetWorkerTag: cfg.RunID,
			"calque:role":  "fleet-worker",
		},
	}

	act := &taskcohort.Actuator{Client: client, Region: cfg.Region, BaseConfig: base}
	obs := &taskcohort.Observer{Client: client, Region: cfg.Region}

	capacity := cohort.CapacityOnDemand
	if cfg.Spot {
		capacity = cohort.CapacitySpot
	}
	rung := cohort.Rung{InstanceType: cfg.InstanceType, CapacityModel: capacity}

	// CohortID/EntityIDs keyed by RunID — unique per fleet run (unlike a pool's
	// model identity, a run id is by construction a one-shot identity, so there
	// is no ProvisionWorkers-vs-ScaleWorkers split to mirror here: a fleet run
	// provisions its whole worker cohort exactly once, up front.
	cohortID := cohort.CohortID("calque-fleet-" + cfg.RunID)
	members := make([]cohort.EntityIntent, 0, cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		id := cohort.EntityID(fmt.Sprintf("calque-fleet-%s-worker-%d", cfg.RunID, i))
		intent, err := cohort.NewEntityIntent("fleet", id, "g1", cohortID,
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
		return fmt.Errorf("provision fleet workers: %w", err)
	}
	if !outcome.Ready {
		var details []string
		for _, m := range members {
			rec := outcome.Records[m.ID]
			if !rec.Succeeded() {
				details = append(details, fmt.Sprintf("  %s: %s", m.ID, rec.Summary()))
			}
		}
		return fmt.Errorf("fleet failed to reach min viable (%d of %d) workers:\n%s",
			mv, cfg.Workers, strings.Join(details, "\n"))
	}
	return nil
}

// DiscoverFleetWorkerIDs lists every instance currently tagged as belonging
// to this fleet run (calque:fleet-run == runID) and returns their
// EntityIDs — discoverPoolWorkerIDs' run-scoped sibling. Used by
// DrainFleetWorkers for teardown, which wants EntityIDs (Reconciler.Drain's
// own argument type) and every state (stopped/pending included, not just
// running) so a not-yet-terminated worker in ANY state still gets drained.
// For a LIVENESS check's instance-ID list, use DiscoverFleetWorkerInstances
// instead (calque#145 slice 3) — this function discards the real EC2
// InstanceID a liveness check needs (spawn:last-heartbeat is stamped on the
// instance, not the cohort entity name).
func DiscoverFleetWorkerIDs(ctx context.Context, client taskcohort.LaunchAPI, runID, region string) ([]cohort.EntityID, error) {
	insts, err := client.ListInstances(ctx, region, "")
	if err != nil {
		return nil, fmt.Errorf("list instances for fleet run %q: %w", runID, err)
	}
	ids := make([]cohort.EntityID, 0, len(insts))
	for _, in := range insts {
		if in.Tags[fleetWorkerTag] == runID {
			ids = append(ids, cohort.EntityID(in.Name))
		}
	}
	return ids, nil
}

// LiveFleetWorker pairs a fleet worker's cohort EntityID (its Name tag)
// with its real EC2 InstanceID (calque#145 slice 3) — DiscoverFleetWorkerIDs
// only keeps the former, but a fleet-wide liveness check needs the latter to
// read spawn:last-heartbeat, which is stamped on the INSTANCE, not the name.
type LiveFleetWorker struct {
	EntityID   cohort.EntityID
	InstanceID string
}

// DiscoverFleetWorkerInstances is DiscoverFleetWorkerIDs' liveness-check
// sibling (calque#145 slice 3): keeps the InstanceID ListInstances already
// returns instead of discarding it. Filtered to RUNNING instances only —
// unlike DiscoverFleetWorkerIDs/DrainFleetWorkers, which deliberately want
// every state for teardown, a liveness check only cares about instances
// that could still be alive to claim work; a stopped/terminated instance
// is definitionally not a candidate worker.
func DiscoverFleetWorkerInstances(ctx context.Context, client taskcohort.LaunchAPI, runID, region string) ([]LiveFleetWorker, error) {
	insts, err := client.ListInstances(ctx, region, "running")
	if err != nil {
		return nil, fmt.Errorf("list running instances for fleet run %q: %w", runID, err)
	}
	out := make([]LiveFleetWorker, 0, len(insts))
	for _, in := range insts {
		if in.Tags[fleetWorkerTag] == runID {
			out = append(out, LiveFleetWorker{EntityID: cohort.EntityID(in.Name), InstanceID: in.InstanceID})
		}
	}
	return out, nil
}

// DrainFleetWorkers terminates every worker instance belonging to a fleet
// run (calque#145) — DeletePool's drain half, WITHOUT the queue-deletion
// half: a fleet run's queue is deleted separately by the caller (via
// DeleteRunQueueIfExists), since fleetRun's own D3/D5 stages still need the
// queue's SQS client in scope for other reasons at a different point in the
// run than worker teardown.
func DrainFleetWorkers(ctx context.Context, client *spawnaws.Client, runID, region string) error {
	return drainFleetWorkers(ctx, client, runID, region)
}

func drainFleetWorkers(ctx context.Context, client taskcohort.LaunchAPI, runID, region string) error {
	ids, err := DiscoverFleetWorkerIDs(ctx, client, runID, region)
	if err != nil {
		return fmt.Errorf("discover fleet %q workers: %w", runID, err)
	}
	if len(ids) == 0 {
		return nil
	}
	act := &taskcohort.Actuator{Client: client, Region: region}
	r := cohort.NewReconciler(act, nil, nil, nil, nil, nil)
	if err := r.Drain(ctx, ids); err != nil {
		return fmt.Errorf("terminate fleet %q workers: %w", runID, err)
	}
	return nil
}
