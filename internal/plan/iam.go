package plan

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// RealRunPolicy builds the scoped inline IAM policy a single-instance real
// run's instance profile needs (calque#148) — mirrors internal/pool's
// WorkerPolicy/FleetWorkerPolicy shape (the only place in this codebase
// that set up an instance profile correctly before this issue), minus the
// SQS statements: a single-instance real run has no queue, just its own
// run's artifacts/manifest/results/summary/bootstrap-log, all under one
// bucket. Read is needed for the artifact/manifest sync; write for
// results, the summary, and the bootstrap log upload.
//
// calque#176: also grants ECR pull permissions, needed when a --script
// real run's picked unit resolves to a from_registry/from_aws_ecr image
// and the instance authenticates via `aws ecr get-login-password` before
// `docker pull` (internal/exec.BootstrapConfig.Command). Granted
// unconditionally (not just when the current run's image needs it) —
// mirrors how the S3 statements above are granted regardless of whether
// THIS run's script writes results, since the role is reused across
// every future run against this bucket (calque#167), not recreated per
// run. ecr:GetAuthorizationToken is NOT resource-scopable (AWS requires
// Resource: "*" for this action, per AWS's own ECR IAM reference); the
// actual image-layer read actions are scoped to account+region, not to
// one specific repo, since the resolved registry ref varies per script
// and isn't known when the role is created.
func RealRunPolicy(account, region, bucket string) string {
	obj := fmt.Sprintf("arn:aws:s3:::%s/*", bucket)
	bkt := fmt.Sprintf("arn:aws:s3:::%s", bucket)
	ecrRepos := fmt.Sprintf("arn:aws:ecr:%s:%s:repository/*", region, account)
	stmts := []string{
		fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":[%q]}`, obj),
		fmt.Sprintf(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%q]}`, bkt),
		`{"Effect":"Allow","Action":["ecr:GetAuthorizationToken"],"Resource":["*"]}`,
		fmt.Sprintf(`{"Effect":"Allow","Action":["ecr:BatchGetImage","ecr:GetDownloadUrlForLayer"],"Resource":[%q]}`, ecrRepos),
	}
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(stmts, ",") + `]}`
}

// RealRunInstanceProfile resolves (creating if needed) the IAM instance
// profile a single-instance real run's launched instance needs (calque#148)
// — WITHOUT this, spawn's own Launch never sets any instance profile at
// all (confirmed: launchConfig.IamInstanceProfile is a pure passthrough
// with zero implicit default), so the instance has no credentials for the
// `aws s3 cp`/`aws s3 sync` calls its own bootstrap script makes — not
// even for uploading its OWN bootstrap log on failure, which is why a
// bootstrap failure on this path was previously totally silent (no log,
// no error, just a timeout).
//
// The role name is PER-BUCKET (calque#167), not one shared "calque-real-run"
// name across every bucket as originally shipped. Reason: spawn's
// CreateOrGetInstanceProfile updates an existing role's inline policy via
// IAM PutRolePolicy, which REPLACES the named policy document wholesale —
// confirmed by reading spawn's own updateInlinePolicy (pkg/aws/iam.go) —
// not a union/merge. Two overlapping real runs against different buckets
// sharing one role name would race: run B's launch replaces the
// "spawn-scoped-policy" document with bucket-B-only grants while run A's
// instance may still be mid-flight making S3 calls against bucket A — IAM
// policy evaluation is live, so A's very next s3:GetObject/PutObject call
// would then fail with AccessDenied, non-deterministically, depending on
// exactly when B's launch lands relative to A's S3 calls. Scoping the role
// name to the bucket (roleNameForBucket) makes two different-bucket runs
// use two different, never-colliding roles by construction — no
// read-modify-write race to reason about at all, at the cost of one
// IAM role per DISTINCT bucket ever passed to --bucket (bounded by how
// many buckets a caller actually uses, not by run count: two runs against
// the SAME bucket still correctly share one role, as before).
func RealRunInstanceProfile(ctx context.Context, client *spawnaws.Client, region, bucket string) (string, error) {
	account, err := client.GetAccountID(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve account id: %w", err)
	}
	profile, err := client.CreateOrGetInstanceProfile(ctx, spawnaws.IAMRoleConfig{
		RoleName:         roleNameForBucket(bucket),
		TrustServices:    []string{"ec2"},
		InlinePolicyJSON: RealRunPolicy(account, region, bucket),
	})
	if err != nil {
		return "", fmt.Errorf("set up real-run IAM instance profile: %w", err)
	}
	return profile, nil
}

// roleNameForBucket derives a stable, IAM-name-legal role name from a
// bucket name (calque#167) — IAM role names allow only
// [\w+=,.@-]{1,64}, so a bucket name containing dots (legal in S3, not
// in an IAM role name in combination with our own prefix safely) is
// hashed rather than embedded verbatim. Same bucket always yields the
// same name (needed for CreateOrGetInstanceProfile's reuse-not-recreate
// behavior); different buckets are collision-free in practice (SHA-256,
// truncated to 16 hex chars — far more than enough entropy for the
// number of distinct buckets any one caller will ever pass).
func roleNameForBucket(bucket string) string {
	h := sha256.Sum256([]byte(bucket))
	return fmt.Sprintf("calque-real-run-%x", h[:8])
}
