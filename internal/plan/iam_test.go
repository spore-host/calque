package plan

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRealRunPolicy_ScopesToTheGivenBucketOnly (calque#148) mirrors
// internal/pool.TestWorkerPolicyScopesToThisPoolOnly for the single-
// instance real-run policy: it must grant read/write on the run's own
// bucket only, and must produce valid JSON.
func TestRealRunPolicy_ScopesToTheGivenBucketOnly(t *testing.T) {
	policy := RealRunPolicy("111122223333", "us-east-1", "calque-runs-bucket")

	var doc map[string]any
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("RealRunPolicy did not produce valid JSON: %v\n%s", err, policy)
	}
	if !strings.Contains(policy, "calque-runs-bucket") {
		t.Errorf("policy missing bucket scoping; got %s", policy)
	}
	if !strings.Contains(policy, "s3:GetObject") || !strings.Contains(policy, "s3:PutObject") {
		t.Errorf("policy missing GetObject/PutObject (artifact sync in, results/summary/bootstrap-log out); got %s", policy)
	}
	if strings.Contains(policy, "other-bucket") {
		t.Errorf("policy references a bucket it was never given; got %s", policy)
	}
}

// TestRealRunPolicy_DoesNotCollideWithPoolOrFleetPolicy proves the
// single-instance real-run policy is textually distinct from
// internal/pool's WorkerPolicy/FleetWorkerPolicy shapes for the same
// bucket — no queue ARN, since a single-instance real run has no SQS
// queue at all.
func TestRealRunPolicy_DoesNotCollideWithPoolOrFleetPolicy(t *testing.T) {
	policy := RealRunPolicy("111122223333", "us-east-1", "calque-runs-bucket")
	if strings.Contains(policy, "sqs:") {
		t.Errorf("RealRunPolicy must not grant any SQS action (no queue involved); got %s", policy)
	}
}

// TestSpawnLauncherBuild_ThreadsIamInstanceProfile (calque#148 regression
// guard) proves SpawnLauncher.Build() actually carries IamInstanceProfile
// through to the returned spawnaws.LaunchConfig — the exact field spawn's
// own Launch treats as "set an instance profile" vs. "launch with none at
// all." An empty value (the pre-#148 default) must also pass through
// unchanged, matching every pre-existing call site that hasn't been
// updated to set one.
func TestSpawnLauncherBuild_ThreadsIamInstanceProfile(t *testing.T) {
	cfg := SpawnLauncher{RunCmd: "echo hi", IamInstanceProfile: "calque-real-run"}.Build()
	if cfg.IamInstanceProfile != "calque-real-run" {
		t.Errorf("IamInstanceProfile = %q, want %q", cfg.IamInstanceProfile, "calque-real-run")
	}

	empty := SpawnLauncher{RunCmd: "echo hi"}.Build()
	if empty.IamInstanceProfile != "" {
		t.Errorf("IamInstanceProfile = %q, want empty when not set", empty.IamInstanceProfile)
	}
}
