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
	policy := RealRunPolicy("111122223333", "us-east-1", "calque-runs-bucket", nil)

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
	policy := RealRunPolicy("111122223333", "us-east-1", "calque-runs-bucket", nil)
	if strings.Contains(policy, "sqs:") {
		t.Errorf("RealRunPolicy must not grant any SQS action (no queue involved); got %s", policy)
	}
}

// TestRealRunPolicy_GrantsECRPull (calque#176) proves the instance can
// authenticate to ECR and pull image layers — needed once a --script real
// run's picked unit resolves to a from_registry/from_aws_ecr image and the
// bootstrap does `aws ecr get-login-password` before `docker pull`.
// ecr:GetAuthorizationToken must be Resource:"*" (AWS requires this for
// that specific action, it isn't resource-scopable); the layer-read
// actions are scoped to this account+region's own repos.
func TestRealRunPolicy_GrantsECRPull(t *testing.T) {
	policy := RealRunPolicy("111122223333", "us-east-1", "calque-runs-bucket", nil)

	var doc struct {
		Statement []struct {
			Effect   string   `json:"Effect"`
			Action   []string `json:"Action"`
			Resource []string `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("RealRunPolicy did not produce valid JSON: %v\n%s", err, policy)
	}

	var sawAuthToken, sawLayerRead bool
	for _, s := range doc.Statement {
		for _, a := range s.Action {
			if a == "ecr:GetAuthorizationToken" {
				sawAuthToken = true
				if len(s.Resource) != 1 || s.Resource[0] != "*" {
					t.Errorf("ecr:GetAuthorizationToken must be Resource:[\"*\"] (AWS requires this, it isn't resource-scopable); got %v", s.Resource)
				}
			}
			if a == "ecr:BatchGetImage" || a == "ecr:GetDownloadUrlForLayer" {
				sawLayerRead = true
				for _, r := range s.Resource {
					if !strings.Contains(r, "111122223333") || !strings.Contains(r, "us-east-1") {
						t.Errorf("ECR layer-read action %q should be scoped to this account/region; got resource %q", a, r)
					}
				}
			}
		}
	}
	if !sawAuthToken {
		t.Errorf("RealRunPolicy missing ecr:GetAuthorizationToken; got %s", policy)
	}
	if !sawLayerRead {
		t.Errorf("RealRunPolicy missing ecr:BatchGetImage/GetDownloadUrlForLayer; got %s", policy)
	}
}

// TestRealRunPolicy_GrantsExtraBuckets (calque#91 Workstream A) proves each
// DISTINCT bucket in extraBuckets gets its own read/write/list statements,
// scoped to THAT bucket, separate from the run's own --bucket grant — for a
// --script real run whose resolved modal.CloudBucketMount(...) mounts
// reference the script's OWN bucket(s), not calque's staging area.
func TestRealRunPolicy_GrantsExtraBuckets(t *testing.T) {
	policy := RealRunPolicy("111122223333", "us-east-1", "calque-runs-bucket", []string{"my-real-bucket", "other-real-bucket"})

	var doc map[string]any
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("RealRunPolicy did not produce valid JSON: %v\n%s", err, policy)
	}
	for _, want := range []string{"calque-runs-bucket", "my-real-bucket", "other-real-bucket"} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy missing bucket %q; got %s", want, policy)
		}
	}
	if !strings.Contains(policy, `arn:aws:s3:::my-real-bucket/*`) || !strings.Contains(policy, `arn:aws:s3:::my-real-bucket"`) {
		t.Errorf("policy missing object+bucket ARNs for my-real-bucket; got %s", policy)
	}
}

// TestRealRunPolicy_ExtraBucketsDedupedAndSkipsRunBucket proves a duplicate
// name in extraBuckets (or one matching the run's own bucket) doesn't emit
// a second, redundant statement pair.
func TestRealRunPolicy_ExtraBucketsDedupedAndSkipsRunBucket(t *testing.T) {
	policy := RealRunPolicy("111122223333", "us-east-1", "calque-runs-bucket", []string{"my-real-bucket", "my-real-bucket", "calque-runs-bucket"})
	if got := strings.Count(policy, "my-real-bucket"); got != 2 { // object ARN + bucket ARN, once each
		t.Errorf("my-real-bucket should appear exactly twice (object+bucket ARN), got %d times in %s", got, policy)
	}
	if got := strings.Count(policy, "calque-runs-bucket"); got != 2 {
		t.Errorf("calque-runs-bucket should still appear exactly twice (its own grant, not duplicated by extraBuckets), got %d times in %s", got, policy)
	}
}

// TestRealRunPolicy_NilExtraBucketsUnchanged proves the default (nil
// extraBuckets) reproduces prior behavior byte-for-byte.
func TestRealRunPolicy_NilExtraBucketsUnchanged(t *testing.T) {
	withNil := RealRunPolicy("111122223333", "us-east-1", "calque-runs-bucket", nil)
	withEmpty := RealRunPolicy("111122223333", "us-east-1", "calque-runs-bucket", []string{})
	if withNil != withEmpty {
		t.Errorf("nil and empty extraBuckets should produce identical policies:\nnil:   %s\nempty: %s", withNil, withEmpty)
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
