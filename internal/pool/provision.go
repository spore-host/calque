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

// buildWorkerBootstrapCommand emits the on-instance command that runs
// `warmd pool --model M` under the worker's IAM-scoped instance. Unlike
// spawn's buildPoolWorkerCommand, there is no restart-on-error wrapper loop
// here (calque#100's own Worker.Run already tolerates transient claim/fetch
// errors internally without exiting — a non-zero exit from `warmd pool`
// means it exhausted its own retry budget, a genuinely terminal condition
// for THIS worker, not one a shell-level restart should paper over).
func buildWorkerBootstrapCommand(model, region, runnerPath, idleTimeout string) string {
	return fmt.Sprintf(
		"warmd pool --model %q --region %q --runner-path %q --idle-timeout %q",
		model, region, runnerPath, idleTimeout,
	)
}

// CreateConfig is everything ProvisionWorkers needs to bring up one pool's
// worker cohort.
type CreateConfig struct {
	Model          string // pool identity (decision 2 of docs/pool-queue-contract.md)
	Region         string
	InstanceType   string
	Workers        int // requested worker count
	MinViable      int // <1 => 1; >Workers => Workers (mirrors spawn pool create's clamping)
	Spot           bool
	SpotMaxPrice   string
	TTL            string
	IdleTimeout    string
	ManifestBucket string
	ResultsBucket  string
	RunnerPath     string // path to runner.py baked into the worker image
	AMI            string // empty => auto-detect via GetRecommendedAMI, same as spawn pool create
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

	workerCmd := buildWorkerBootstrapCommand(cfg.Model, cfg.Region, cfg.RunnerPath, cfg.IdleTimeout)
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
	// THIS call (idempotency). The model name is stable for the pool's lifetime;
	// scale-up calls (adding more workers to an existing pool) are out of scope
	// for this issue — creating a SECOND cohort under the same model name would
	// collide with the first's entity IDs. calque#101 ships create-only, matching
	// spawn pool's own initial scope; a scale-up path is a natural follow-up.
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
