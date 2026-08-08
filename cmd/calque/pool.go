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
		return fmt.Errorf("usage: calque pool create --model M --instance-type T [--workers N] ...")
	}
	switch args[0] {
	case "create":
		return poolCreateCmd(args[1:])
	default:
		return fmt.Errorf("unknown pool subcommand %q (want: create)", args[0])
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
		ManifestBucket: *manifestBucket, ResultsBucket: *resultsBucket,
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
// drain time, mirroring spawn pool create's own 15-minute default.
const defaultVisibilityTimeout = 900
