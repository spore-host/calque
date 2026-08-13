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

// TestRoleNameForBucket_DifferentBucketsGetDifferentRoles (calque#167) proves
// the actual fix: two real runs against different buckets no longer share
// one mutable role whose inline policy PutRolePolicy replaces wholesale on
// every launch (a race — a second run's launch could silently drop a first
// run's still-in-flight bucket grant). Same bucket must still resolve to the
// same name (needed for CreateOrGetInstanceProfile's reuse, not recreate).
func TestRoleNameForBucket_DifferentBucketsGetDifferentRoles(t *testing.T) {
	a := roleNameForBucket("bucket-a")
	b := roleNameForBucket("bucket-b")
	if a == b {
		t.Fatalf("roleNameForBucket(%q) == roleNameForBucket(%q) == %q, want distinct names", "bucket-a", "bucket-b", a)
	}
	if got := roleNameForBucket("bucket-a"); got != a {
		t.Errorf("roleNameForBucket(%q) = %q on second call, want stable %q", "bucket-a", got, a)
	}
}

// TestRoleNameForBucket_IsIAMNameLegal (calque#167) — IAM role names allow
// only [\w+=,.@-]{1,64}; a bucket name may contain dots (legal in S3, and
// this exact combination — a dotted bucket name embedded verbatim behind a
// "calque-real-run-" prefix — was the case that motivated hashing instead
// of embedding the bucket name directly).
func TestRoleNameForBucket_IsIAMNameLegal(t *testing.T) {
	name := roleNameForBucket("my.bucket.with.dots-and-things_2026")
	if len(name) > 64 {
		t.Errorf("role name %q is %d chars, IAM's limit is 64", name, len(name))
	}
	const legal = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+=,.@-_"
	for _, r := range name {
		if !strings.ContainsRune(legal, r) {
			t.Errorf("role name %q contains IAM-illegal character %q", name, r)
		}
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
