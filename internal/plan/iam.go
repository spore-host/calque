package plan

import (
	"context"
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
func RealRunPolicy(account, region, bucket string) string {
	obj := fmt.Sprintf("arn:aws:s3:::%s/*", bucket)
	bkt := fmt.Sprintf("arn:aws:s3:::%s", bucket)
	stmts := []string{
		fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":[%q]}`, obj),
		fmt.Sprintf(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%q]}`, bkt),
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
// no error, just a timeout). One shared role name ("calque-real-run")
// across every real run/region/bucket, matching internal/pool's own
// single-shared-role-name convention (CreateOrGetInstanceProfile is
// idempotent — a second call with a different InlinePolicyJSON updates
// the existing role's inline policy rather than erroring).
func RealRunInstanceProfile(ctx context.Context, client *spawnaws.Client, region, bucket string) (string, error) {
	account, err := client.GetAccountID(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve account id: %w", err)
	}
	profile, err := client.CreateOrGetInstanceProfile(ctx, spawnaws.IAMRoleConfig{
		RoleName:         "calque-real-run",
		TrustServices:    []string{"ec2"},
		InlinePolicyJSON: RealRunPolicy(account, region, bucket),
	})
	if err != nil {
		return "", fmt.Errorf("set up real-run IAM instance profile: %w", err)
	}
	return profile, nil
}
