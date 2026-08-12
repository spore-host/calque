package pool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestFleetWorkerPolicyScopesToThisRunOnly is TestWorkerPolicyScopesToThisPoolOnly's
// calque#145 sibling — proves the IAM policy is scoped to the SPECIFIC run's
// queue and buckets, and grants no submitter-only actions.
func TestFleetWorkerPolicyScopesToThisRunOnly(t *testing.T) {
	policy := FleetWorkerPolicy("111122223333", "us-east-1", "run-abc", "calque-manifests", "calque-results")

	var doc map[string]any
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("FleetWorkerPolicy did not produce valid JSON: %v\n%s", err, policy)
	}

	wantQueueARN := "arn:aws:sqs:us-east-1:111122223333:calque-fleet-run-abc"
	if !strings.Contains(policy, wantQueueARN) {
		t.Errorf("policy missing scoped queue ARN %q; got %s", wantQueueARN, policy)
	}
	for _, forbidden := range []string{"sqs:SendMessage", "sqs:CreateQueue", "sqs:DeleteQueue"} {
		if strings.Contains(policy, forbidden) {
			t.Errorf("policy grants %q, which is a submitter-only action a worker must not have", forbidden)
		}
	}
	if !strings.Contains(policy, "calque-manifests") {
		t.Errorf("policy missing manifest bucket scoping; got %s", policy)
	}
	if !strings.Contains(policy, "calque-results") {
		t.Errorf("policy missing results bucket scoping; got %s", policy)
	}
	if strings.Contains(policy, "calque-fleet-run-xyz") {
		t.Errorf("policy for run %q leaked a reference to a different run's queue", "run-abc")
	}
}

// TestFleetWorkerPolicy_DoesNotCollideWithPoolPolicy: the same identifier
// used as both a model and a run id must produce policies scoped to
// DIFFERENT queue ARNs — the run/pool queue isolation proven at the naming
// layer (TestOpenRunQueue_DoesNotResolveAPoolQueue) must hold at the IAM
// layer too.
func TestFleetWorkerPolicy_DoesNotCollideWithPoolPolicy(t *testing.T) {
	shared := "shared-name"
	fleetPolicy := FleetWorkerPolicy("111122223333", "us-east-1", shared, "b1", "b2")
	poolPolicy := WorkerPolicy("111122223333", "us-east-1", shared, "b1", "b2")
	if strings.Contains(fleetPolicy, PoolQueueName(shared)) {
		t.Error("FleetWorkerPolicy referenced the POOL queue name for a shared identifier")
	}
	if strings.Contains(poolPolicy, RunQueueName(shared)) {
		t.Error("WorkerPolicy referenced the RUN queue name for a shared identifier")
	}
}

// TestBuildFleetWorkerBootstrapCommandIsShellSafe mirrors
// TestBuildWorkerBootstrapCommandIsShellSafe — every warmd-fleet argument
// must be quoted so a runner path containing a space doesn't silently
// truncate.
func TestBuildFleetWorkerBootstrapCommandIsShellSafe(t *testing.T) {
	cmd := buildFleetWorkerBootstrapCommand("run-abc", "us-east-1", "s3://bucket/artifacts", "/tmp/calque-fleet", "/opt/calque runner/runner.py", "1m", 900)
	for _, want := range []string{
		`--run-id "run-abc"`,
		`--region "us-east-1"`,
		`--runner-path "/opt/calque runner/runner.py"`,
		`--idle-timeout "1m"`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("bootstrap command missing %q; got: %s", want, cmd)
		}
	}
}

// TestBuildFleetWorkerBootstrapCommand_SyncsArtifactsBeforeInvokingWarmd
// (calque#145 slice 2): the real gotcha this fix addresses — pool mode's
// bootstrap command (buildWorkerBootstrapCommand) assumes warmd/runner.py
// are ALREADY on a pre-baked AMI and never syncs anything from S3. Fleet
// mode inherited that same bootstrap shape in slice 1, but fleetRun's
// existing single-shard path (BootstrapConfig.Command) builds+uploads
// warmd fresh every run and needs no pre-baked AMI — so the fleet worker
// bootstrap must sync artifactS3URI onto the worker BEFORE invoking
// `warmd fleet`, or every worker fails to boot (no warmd binary present).
func TestBuildFleetWorkerBootstrapCommand_SyncsArtifactsBeforeInvokingWarmd(t *testing.T) {
	cmd := buildFleetWorkerBootstrapCommand("run-abc", "us-east-1", "s3://calque-bucket/fleet/run-abc/artifacts", "/tmp/calque-fleet", "/tmp/calque-fleet/runner.py", "1m", 900)

	syncIdx := strings.Index(cmd, "aws s3 cp --recursive s3://calque-bucket/fleet/run-abc/artifacts")
	if syncIdx < 0 {
		t.Fatalf("bootstrap command does not sync the artifact S3 URI at all; got: %s", cmd)
	}
	warmdIdx := strings.Index(cmd, "warmd fleet")
	if warmdIdx < 0 {
		t.Fatalf("bootstrap command never invokes warmd fleet; got: %s", cmd)
	}
	if syncIdx > warmdIdx {
		t.Errorf("artifact sync must happen BEFORE warmd fleet is invoked (a fresh worker has no warmd binary until synced); got: %s", cmd)
	}
	if !strings.Contains(cmd, "chmod +x /tmp/calque-fleet/warmd") {
		t.Errorf("bootstrap command must chmod +x the synced warmd binary; got: %s", cmd)
	}
}

// TestDrainFleetWorkers_TerminatesOnlyThisRunsWorkers is
// TestDeletePool_TerminatesOnlyThisModelsWorkers' calque#145 sibling: proves
// DrainFleetWorkers discovers workers by the calque:fleet-run tag
// ProvisionFleetWorkers stamps, terminates every one of THIS run's workers,
// and leaves a different run's workers (same account, different run-id tag)
// untouched.
func TestDrainFleetWorkers_TerminatesOnlyThisRunsWorkers(t *testing.T) {
	launch := newFakeLaunchAPI()
	launch.seedFleet("calque-fleet-run-abc-worker-0", "i-1", "run-abc")
	launch.seedFleet("calque-fleet-run-abc-worker-1", "i-2", "run-abc")
	launch.seedFleet("calque-fleet-run-xyz-worker-0", "i-3", "run-xyz") // different run

	if err := drainFleetWorkers(context.Background(), launch, "run-abc", "us-east-1"); err != nil {
		t.Fatalf("drainFleetWorkers: %v", err)
	}
	if launch.terminatedCount() != 2 {
		t.Errorf("terminated count = %d, want 2 (only run-abc's workers)", launch.terminatedCount())
	}
	remaining, err := launch.ListInstances(context.Background(), "us-east-1", "")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(remaining) != 1 || remaining[0].InstanceID != "i-3" {
		t.Errorf("remaining instances = %+v, want only run-xyz's worker (i-3) left", remaining)
	}
}

// TestDrainFleetWorkers_NoWorkersIsNotAnError mirrors
// TestDeletePool_NoQueueIsNotAnError's spirit: draining a run with zero
// discovered workers (never provisioned, or already drained) must succeed,
// not fail.
func TestDrainFleetWorkers_NoWorkersIsNotAnError(t *testing.T) {
	launch := newFakeLaunchAPI()
	if err := drainFleetWorkers(context.Background(), launch, "run-empty", "us-east-1"); err != nil {
		t.Fatalf("drainFleetWorkers with no workers should succeed, got: %v", err)
	}
}

// TestDiscoverFleetWorkerIDs_EmptyIsNotNilError proves a fleet run with no
// live workers reports an empty (not error) result — "zero live workers" is
// a normal state, mirroring discoverPoolWorkerIDs' own contract.
func TestDiscoverFleetWorkerIDs_EmptyIsNotNilError(t *testing.T) {
	launch := newFakeLaunchAPI()
	ids, err := DiscoverFleetWorkerIDs(context.Background(), launch, "run-empty", "us-east-1")
	if err != nil {
		t.Fatalf("DiscoverFleetWorkerIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty for a run with no tagged workers", ids)
	}
}

// TestDiscoverFleetWorkerInstances_ReturnsRealInstanceIDs (calque#145
// slice 3): the actual gap DiscoverFleetWorkerIDs left open — a fleet-wide
// liveness check needs real EC2 InstanceIDs (spawn:last-heartbeat is
// stamped on the instance, not the cohort entity name), which
// DiscoverFleetWorkerIDs discards. seedFleet already stores instanceID
// keyed by name; this proves DiscoverFleetWorkerInstances surfaces it.
func TestDiscoverFleetWorkerInstances_ReturnsRealInstanceIDs(t *testing.T) {
	launch := newFakeLaunchAPI()
	launch.seedFleet("calque-fleet-run-abc-worker-0", "i-1", "run-abc")
	launch.seedFleet("calque-fleet-run-abc-worker-1", "i-2", "run-abc")
	launch.seedFleet("calque-fleet-run-xyz-worker-0", "i-3", "run-xyz") // different run

	workers, err := DiscoverFleetWorkerInstances(context.Background(), launch, "run-abc", "us-east-1")
	if err != nil {
		t.Fatalf("DiscoverFleetWorkerInstances: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("workers = %+v, want 2 (only run-abc's)", workers)
	}
	gotIDs := map[string]bool{}
	for _, w := range workers {
		gotIDs[w.InstanceID] = true
		if w.EntityID == "" {
			t.Errorf("worker %+v has empty EntityID", w)
		}
	}
	if !gotIDs["i-1"] || !gotIDs["i-2"] {
		t.Errorf("gotIDs = %v, want {i-1, i-2}", gotIDs)
	}
	if gotIDs["i-3"] {
		t.Error("DiscoverFleetWorkerInstances leaked a different run's instance ID")
	}
}

// TestDiscoverFleetWorkerInstances_EmptyIsNotAnError mirrors
// TestDiscoverFleetWorkerIDs_EmptyIsNotNilError for the instance-ID variant.
func TestDiscoverFleetWorkerInstances_EmptyIsNotAnError(t *testing.T) {
	launch := newFakeLaunchAPI()
	workers, err := DiscoverFleetWorkerInstances(context.Background(), launch, "run-empty", "us-east-1")
	if err != nil {
		t.Fatalf("DiscoverFleetWorkerInstances: %v", err)
	}
	if len(workers) != 0 {
		t.Errorf("workers = %v, want empty for a run with no tagged workers", workers)
	}
}
