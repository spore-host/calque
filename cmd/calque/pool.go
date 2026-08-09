// calque pool create: provision a named model pool's worker cohort (calque#101).
//
// Mirrors `spawn pool create`'s CLI shape but with MODEL, not run id, as the
// pool's identity (docs/pool-queue-contract.md decision 2), and pointing
// workers at `warmd pool` (calque#100) instead of `spored pool-worker`.
package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	calpool "github.com/spore-host/calque/internal/pool"
)

func poolCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: calque pool <create|scale|delete|status|list> --model M ...")
	}
	switch args[0] {
	case "create":
		return poolCreateCmd(args[1:])
	case "scale":
		return poolScaleCmd(args[1:])
	case "delete":
		return poolDeleteCmd(args[1:])
	case "status":
		return poolStatusCmd(args[1:])
	case "list":
		return poolListCmd(args[1:])
	default:
		return fmt.Errorf("unknown pool subcommand %q (want: create, scale, delete, status, list)", args[0])
	}
}

func poolCreateCmd(args []string) error {
	fs := flag.NewFlagSet("pool create", flag.ExitOnError)
	model := fs.String("model", "", "pool identity: every claim on this pool's queue targets this SAME warm model (required)")
	region := fs.String("region", "us-east-1", "AWS region")
	instanceType := fs.String("instance-type", "", "GPU instance type for every worker (required)")
	workers := fs.Int("workers", 1, "number of workers to request")
	minViable := fs.Int("min-viable", 1, "minimum workers that must come up for the pool to be considered ready (best-effort above this)")
	spot := fs.Bool("spot", false, "launch workers on the Spot market")
	spotMaxPrice := fs.String("spot-max-price", "", "spot bid cap in $/hr (empty => on-demand price)")
	ttl := fs.String("ttl", "12h", "hard lifetime cap per worker instance")
	idleTimeout := fs.String("idle-timeout", "30m", "how long a worker keeps its resident runner warm with an empty queue before closing it")
	manifestBucket := fs.String("manifest-bucket", "", "S3 bucket claims' manifests are staged to (required)")
	resultsBucket := fs.String("results-bucket", "", "S3 bucket workers write results+summaries to (required)")
	runnerPath := fs.String("runner-path", "", "path to runner.py in the worker image (required)")
	ami := fs.String("ami", "", "pin the AMI; empty => auto-detect (same as spawn pool create)")
	confirm := fs.Bool("i-understand-this-spends-money", false, "required: launches billable GPU instances")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" || *instanceType == "" || *manifestBucket == "" || *resultsBucket == "" || *runnerPath == "" {
		return fmt.Errorf("usage: calque pool create --model M --instance-type T --manifest-bucket B --results-bucket B --runner-path P [--workers N] [--spot] --i-understand-this-spends-money")
	}
	if !*confirm {
		return fmt.Errorf("refusing to provision: pass --i-understand-this-spends-money (launches billable GPU instances)")
	}

	ctx := context.Background()
	client, err := spawnaws.NewClientWithRegion(ctx, *region)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	if _, err := time.ParseDuration(*idleTimeout); err != nil {
		return fmt.Errorf("--idle-timeout %q: %w", *idleTimeout, err)
	}

	cfg := calpool.CreateConfig{
		Model: *model, Region: *region, InstanceType: *instanceType,
		Workers: *workers, MinViable: *minViable,
		Spot: *spot, SpotMaxPrice: *spotMaxPrice,
		TTL: *ttl, IdleTimeout: *idleTimeout,
		VisibilityTimeout: defaultVisibilityTimeout,
		ManifestBucket:    *manifestBucket, ResultsBucket: *resultsBucket,
		RunnerPath: *runnerPath, AMI: *ami,
	}

	// The queue must exist before any worker boots and tries to OpenPoolQueue it
	// (mirrors spawn pool create's own create-queue-before-provision ordering).
	sqsClient := sqs.NewFromConfig(client.Config())
	if _, err := calpool.CreatePoolQueue(ctx, sqsClient, *model, defaultVisibilityTimeout); err != nil {
		return fmt.Errorf("create pool queue for model %q: %w", *model, err)
	}

	if err := calpool.ProvisionWorkers(ctx, client, cfg); err != nil {
		return fmt.Errorf("provision workers for pool %q: %w", *model, err)
	}

	fmt.Printf("pool %q ready: requested %d worker(s) (min viable %d) on %s\n", *model, *workers, *minViable, *instanceType)
	return nil
}

// defaultVisibilityTimeout must exceed the longest expected single-claim
// drain time, mirroring spawn pool create's own 15-minute default. Also
// forwarded into each worker's `warmd pool --visibility-timeout` flag
// (calque#131) so its heartbeat interval is sized against the SAME value
// actually configured on the queue.
const defaultVisibilityTimeout = 900

// poolScaleCmd adds workers to an already-provisioned pool (calque#115)
// without disturbing the ones already running. Unlike poolCreateCmd, it does
// not touch the pool's queue — the queue already exists from the original
// `calque pool create` call.
func poolScaleCmd(args []string) error {
	fs := flag.NewFlagSet("pool scale", flag.ExitOnError)
	model := fs.String("model", "", "pool identity to scale (required)")
	region := fs.String("region", "us-east-1", "AWS region")
	instanceType := fs.String("instance-type", "", "GPU instance type for every new worker (required; should match the pool's existing workers)")
	addWorkers := fs.Int("add-workers", 1, "number of workers to add to the pool")
	spot := fs.Bool("spot", false, "launch new workers on the Spot market")
	spotMaxPrice := fs.String("spot-max-price", "", "spot bid cap in $/hr (empty => on-demand price)")
	ttl := fs.String("ttl", "12h", "hard lifetime cap per new worker instance")
	idleTimeout := fs.String("idle-timeout", "30m", "how long a new worker keeps its resident runner warm with an empty queue before closing it")
	manifestBucket := fs.String("manifest-bucket", "", "S3 bucket claims' manifests are staged to (required)")
	resultsBucket := fs.String("results-bucket", "", "S3 bucket workers write results+summaries to (required)")
	runnerPath := fs.String("runner-path", "", "path to runner.py in the worker image (required)")
	ami := fs.String("ami", "", "pin the AMI; empty => auto-detect (same as spawn pool create)")
	confirm := fs.Bool("i-understand-this-spends-money", false, "required: launches billable GPU instances")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" || *instanceType == "" || *manifestBucket == "" || *resultsBucket == "" || *runnerPath == "" {
		return fmt.Errorf("usage: calque pool scale --model M --instance-type T --manifest-bucket B --results-bucket B --runner-path P --add-workers N [--spot] --i-understand-this-spends-money")
	}
	if *addWorkers < 1 {
		return fmt.Errorf("--add-workers must be >= 1")
	}
	if !*confirm {
		return fmt.Errorf("refusing to provision: pass --i-understand-this-spends-money (launches billable GPU instances)")
	}

	ctx := context.Background()
	client, err := spawnaws.NewClientWithRegion(ctx, *region)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	if _, err := time.ParseDuration(*idleTimeout); err != nil {
		return fmt.Errorf("--idle-timeout %q: %w", *idleTimeout, err)
	}

	cfg := calpool.CreateConfig{
		Model: *model, Region: *region, InstanceType: *instanceType,
		Spot: *spot, SpotMaxPrice: *spotMaxPrice,
		TTL: *ttl, IdleTimeout: *idleTimeout,
		VisibilityTimeout: defaultVisibilityTimeout,
		ManifestBucket:    *manifestBucket, ResultsBucket: *resultsBucket,
		RunnerPath: *runnerPath, AMI: *ami,
	}

	if err := calpool.ScaleWorkers(ctx, client, cfg, *addWorkers); err != nil {
		return fmt.Errorf("scale pool %q: %w", *model, err)
	}

	fmt.Printf("pool %q scaled: requested %d additional worker(s) on %s\n", *model, *addWorkers, *instanceType)
	return nil
}

// poolDeleteCmd tears a pool down completely (calque#130): every worker
// instance is terminated AND the pool's SQS queue is deleted. Gated behind
// its own confirmation flag distinct from poolCreateCmd's — delete is just
// as billing-relevant as create (it's the operation that STOPS billable
// instances from running, but it does so by unconditionally terminating
// them, a destructive, no-undo action that deserves its own explicit gate
// rather than piggybacking on create's launch-confirmation flag).
func poolDeleteCmd(args []string) error {
	fs := flag.NewFlagSet("pool delete", flag.ExitOnError)
	model := fs.String("model", "", "pool identity to delete (required)")
	region := fs.String("region", "us-east-1", "AWS region")
	confirm := fs.Bool("i-understand-this-terminates-instances", false, "required: terminates every running worker instance in this pool and deletes its queue")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" {
		return fmt.Errorf("usage: calque pool delete --model M --i-understand-this-terminates-instances")
	}
	if !*confirm {
		return fmt.Errorf("refusing to delete: pass --i-understand-this-terminates-instances (terminates every running worker in pool %q and deletes its queue)", *model)
	}

	ctx := context.Background()
	client, err := spawnaws.NewClientWithRegion(ctx, *region)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	if err := calpool.DeletePool(ctx, client, *model, *region); err != nil {
		return fmt.Errorf("delete pool %q: %w", *model, err)
	}

	fmt.Printf("pool %q deleted: workers terminated, queue removed\n", *model)
	return nil
}

// poolStatusCmd reports one pool's live worker count and queue depth
// (calque#130).
func poolStatusCmd(args []string) error {
	fs := flag.NewFlagSet("pool status", flag.ExitOnError)
	model := fs.String("model", "", "pool identity to report on (required)")
	region := fs.String("region", "us-east-1", "AWS region")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" {
		return fmt.Errorf("usage: calque pool status --model M")
	}

	ctx := context.Background()
	client, err := spawnaws.NewClientWithRegion(ctx, *region)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	status, err := calpool.PoolStatus(ctx, client, *model, *region)
	if err != nil {
		return fmt.Errorf("status for pool %q: %w", *model, err)
	}
	printPoolStatus(status)
	return nil
}

// poolListCmd prints the status of one named pool. calque keeps no pool
// registry (see internal/pool/provision.go's discoverPoolWorkerIDs comment):
// a pool's only observable identity is its --model-tagged instances and its
// model-named SQS queue, both of which require already knowing the model to
// look up. There is no provider-side "list every calque pool that exists"
// primitive to enumerate ALL pools without that key, so `list`, for now, is
// scoped down to exactly what `status` already does for one named pool —
// this is NOT a stand-in for a real multi-pool listing; it exists as the
// forward-compatible command name/shape for when a pool registry (or a
// well-known tag scan across ALL calque:pool-model values) makes a true
// "list every pool" implementable.
func poolListCmd(args []string) error {
	fs := flag.NewFlagSet("pool list", flag.ExitOnError)
	model := fs.String("model", "", "pool identity to report on (required; calque keeps no pool registry to enumerate all pools without one — see doc comment)")
	region := fs.String("region", "us-east-1", "AWS region")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" {
		return fmt.Errorf("usage: calque pool list --model M (no pool registry exists yet to list ALL pools; see poolListCmd's doc comment)")
	}

	ctx := context.Background()
	client, err := spawnaws.NewClientWithRegion(ctx, *region)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	status, err := calpool.PoolStatus(ctx, client, *model, *region)
	if err != nil {
		return fmt.Errorf("list pool %q: %w", *model, err)
	}
	printPoolStatus(status)
	return nil
}

func printPoolStatus(s calpool.Status) {
	if !s.QueueExists {
		fmt.Printf("pool %q: workers=%d queue=<none>\n", s.Model, s.WorkerCount)
		return
	}
	fmt.Printf("pool %q: workers=%d queue-depth=%d\n", s.Model, s.WorkerCount, s.QueueDepth)
}
