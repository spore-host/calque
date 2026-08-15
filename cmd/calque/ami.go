// calque ami: pre-bake a custom AMI with a docker image's layers already
// pulled, so a real run's bootstrap (internal/exec/bootstrap.go) doesn't pay
// a fresh multi-GB Docker Hub pull on every single boot (calque#144).
//
// Image-only baking: model WEIGHTS are deliberately NOT baked here (that
// would conflict with --model's free-form HF-repo-id design across every
// other command — calque#144's own text names this as the open policy
// question, left unresolved). Weights still download fresh via the HF Hub
// on first @enter, exactly as they do today, regardless of --ami.
//
// This is purely additive: baking an AMI changes nothing about how any
// existing command (real/ramp/fleetrun/smoke) behaves. An operator who
// wants the speed-up passes the resulting AMI id to --ami explicitly, same
// as pinning any other AMI today — there is no automatic lookup/preference
// on the run side.
//
// Built entirely from spawn's existing AMI-lifecycle primitives (exposed as
// a Go library, github.com/spore-host/spawn/pkg/aws's Client.CreateAMI/
// WaitForAMI/ListAMIs/DeleteAMI) — spawn itself has no launch->run->
// snapshot->terminate combinator, so `ami bake` IS that orchestration,
// mirroring smoke.go's own acquire/wait/act/terminate shape exactly.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/plan"
	"github.com/spore-host/calque/internal/target"
)

// bakedImageTag namespaces calque's own AMI tags, distinct from spawn's own
// spawn:* tags (which CreateAMI already sets automatically — this file adds
// ONLY these two on top, never duplicating spawn's own tagging).
const (
	bakedImageTagKey = "calque:baked-image"
	bakedAtTagKey    = "calque:baked-at"
)

func amiCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: calque ami <bake|list|delete>")
	}
	switch args[0] {
	case "bake":
		return amiBakeCmd(args[1:])
	case "list":
		return amiListCmd(args[1:])
	case "delete":
		return amiDeleteCmd(args[1:])
	default:
		return fmt.Errorf("unknown ami subcommand %q (want: bake, list, delete)", args[0])
	}
}

type amiBakeOpts struct {
	region       string
	instance     string
	baseAMI      string // AMI to bake FROM; empty => spawn auto-selects a stock DL AMI
	image        string // docker image to pre-pull
	bucket       string
	runID        string
	name         string
	ttl          string
	deadline     time.Duration
	spot         bool
	spotMaxPrice string
}

// parseAMIBakeArgs parses `calque ami bake`'s flags into an amiBakeOpts
// (plus the separate --i-understand-this-spends-money confirmation, checked
// by the caller) without launching anything — split out from amiBakeCmd so
// flag wiring is unit-testable on its own, mirroring parseSmokeArgs/
// parseRealArgs's own established split.
func parseAMIBakeArgs(args []string) (amiBakeOpts, bool, error) {
	fs := flag.NewFlagSet("ami bake", flag.ExitOnError)
	region := fs.String("region", "us-west-2", "AWS region")
	instance := fs.String("instance", "g6.2xlarge", "instance type to bake on (no GPU actually needed for a docker pull, but matches vLLM's own target family for layer/arch parity)")
	baseAMI := fs.String("ami", "", "base AMI to bake FROM; empty => spawn auto-selects a stock GPU-capable AMI")
	image := fs.String("image", "vllm/vllm-openai:latest", "docker image to pre-pull into the baked AMI")
	bucket := fs.String("bucket", "", "S3 bucket for the bake done-marker (required)")
	runID := fs.String("run-id", "", "unique bake id (required)")
	name := fs.String("name", "", "Name tag for the resulting AMI (required)")
	// TTL/deadline default generously above a realistic multi-GB docker pull:
	// spawn's own TTL enforcer terminates UNCONDITIONALLY on expiry (no
	// exception for "AMI creation in progress"), so a too-tight TTL can kill
	// the instance mid-pull, before CreateAMI ever runs.
	ttl := fs.String("ttl", "45m", "instance TTL hard cap for the bake instance (must exceed the target image's pull time — spawn's TTL enforcer terminates unconditionally on expiry)")
	deadlineMin := fs.Int("deadline-min", 30, "give up acquiring/waiting after N minutes (must also exceed the target image's pull time)")
	spot := fs.Bool("spot", false, "acquire the bake instance on the Spot market")
	spotMaxPrice := fs.String("spot-max-price", "", "spot bid cap in $/hr (empty => on-demand price)")
	confirm := fs.Bool("i-understand-this-spends-money", false, "required: launches a billable instance (briefly) to bake the AMI")
	if err := fs.Parse(args); err != nil {
		return amiBakeOpts{}, false, err
	}
	if *bucket == "" || *runID == "" || *name == "" {
		return amiBakeOpts{}, false, fmt.Errorf("usage: calque ami bake --bucket B --run-id ID --name NAME [--image %s] [--instance g6.2xlarge] [--ami AMI] [--spot] --i-understand-this-spends-money", *image)
	}
	return amiBakeOpts{
		region: *region, instance: *instance, baseAMI: *baseAMI, image: *image,
		bucket: *bucket, runID: *runID, name: *name,
		ttl: *ttl, deadline: time.Duration(*deadlineMin) * time.Minute,
		spot: *spot, spotMaxPrice: *spotMaxPrice,
	}, *confirm, nil
}

func amiBakeCmd(args []string) error {
	o, confirm, err := parseAMIBakeArgs(args)
	if err != nil {
		return err
	}
	if !confirm {
		return fmt.Errorf("refusing to bake: pass --i-understand-this-spends-money (launches a billable instance briefly)")
	}
	return amiBake(o)
}

// amiBake acquires an instance, pulls the target docker image on it, snapshots
// it to a new AMI via spawnaws.Client.CreateAMI, then terminates the source
// instance explicitly (NOT via spawn's own TTL/on-complete lifecycle — a
// spawn-driven terminate before CreateAMI runs would lose the instance
// before the snapshot could be taken). Mirrors smoke.go's own acquire/wait/
// act/terminate shape exactly.
func amiBake(o amiBakeOpts) (err error) {
	ctx := context.Background()
	rep := &leak.Report{}
	fmt.Printf("=== calque ami bake (region=%s image=%s) ===\n", o.region, o.image)

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.region))
	if err != nil {
		return err
	}
	s3c, err := calexec.NewS3ClientForBucket(ctx, o.bucket, o.region)
	if err != nil {
		return fmt.Errorf("s3 client for bucket %q: %w", o.bucket, err)
	}
	doneKey := fmt.Sprintf("ami-bake/%s/done", o.runID)
	logKey := fmt.Sprintf("ami-bake/%s/bootstrap.log", o.runID)

	spawnClient, err := spawnaws.NewClientWithRegion(ctx, o.region)
	if err != nil {
		return fmt.Errorf("spawn client: %w", err)
	}
	// The bake instance needs S3 write access for its own done-marker (and
	// its own failure log) — same gap calque#148 already fixed for every
	// other real-AWS command; reuse the identical mechanism here.
	iamProfile, err := plan.RealRunInstanceProfile(ctx, spawnClient, o.region, o.bucket)
	if err != nil {
		return fmt.Errorf("set up IAM instance profile: %w", err)
	}

	bootCmd := amiBakeBootstrapCommand(o.image, o.bucket, doneKey, logKey)
	launchCfg := plan.SpawnLauncher{
		// "stop", NOT "terminate" (spawn's Build() default when OnComplete
		// is "" — would race CreateAMI, self-terminating the instance the
		// moment the bootstrap script exits, before we can snapshot it). We
		// terminate explicitly ourselves below, only after CreateAMI succeeds.
		RunCmd: bootCmd, TTL: o.ttl, OnComplete: "stop",
		Username: "ubuntu", AMI: o.baseAMI,
		Spot: o.spot, SpotMaxPrice: o.spotMaxPrice,
		IamInstanceProfile: iamProfile,
		RunID:              o.runID, Command: "ami-bake",
	}.Build()
	if o.spot {
		fmt.Printf("[spot] acquiring on the SPOT market (max bid %s).\n", orOnDemand(o.spotMaxPrice))
	}

	var places []plan.Placement
	if found, aerr := calexec.AZsForInstance(ctx, ec2.NewFromConfig(cfg), o.instance); aerr == nil {
		for _, f := range found {
			places = append(places, plan.Placement{AZ: f.AZ, Subnet: f.Subnet})
		}
	}
	acq := &plan.Acquirer{
		LaunchConfig: launchCfg, Report: rep, Deadline: o.deadline, Placements: places,
		OnProgress: func(attempt int, code, detail string, waited time.Duration) {
			fmt.Printf("      ...swept %d attempt(s), no capacity (%s, %s)\n", attempt, code, waited.Round(time.Second))
		},
	}
	tgt := &target.Target{Card: target.DefaultCard, Instance: o.instance}
	fmt.Printf("[1/5] acquiring %s in %s (block-and-wait)...\n", o.instance, o.region)
	acquired, err := acq.Acquire(ctx, tgt, o.region)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	fmt.Printf("      acquired %s (%s) after %s\n", acquired.InstanceID, acquired.AvailabilityZone, acquired.TimeToAcquire().Round(time.Second))

	// Deferred termination — even if baking fails partway, the instance
	// never leaks. Runs AFTER CreateAMI (below) on the success path since
	// CreateAMI needs the instance still running/stopped, never terminated.
	defer func() {
		fmt.Printf("[terminate] cleaning up source instance %s ...\n", acquired.InstanceID)
		if tErr := spawnClient.Terminate(context.Background(), acquired.Region, acquired.InstanceID); tErr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: terminate failed for %s: %v (TTL %s will still reap it)\n", acquired.InstanceID, tErr, o.ttl)
			if err == nil {
				err = fmt.Errorf("terminate: %w", tErr)
			}
		} else {
			fmt.Printf("      terminated %s\n", acquired.InstanceID)
		}
	}()

	fmt.Printf("[2/5] waiting for docker pull to finish (s3://%s/%s)...\n", o.bucket, doneKey)
	if err := waitForS3Key(ctx, s3c, o.bucket, doneKey, o.deadline, 10*time.Second); err != nil {
		return fmt.Errorf("wait for bake done-marker: %w", err)
	}
	fmt.Println("      docker pull complete")

	fmt.Printf("[3/5] creating AMI %q from %s...\n", o.name, acquired.InstanceID)
	amiID, err := spawnClient.CreateAMI(ctx, acquired.Region, spawnaws.CreateAMIInput{
		InstanceID:  acquired.InstanceID,
		Name:        o.name,
		Description: fmt.Sprintf("calque-baked AMI: %s pre-pulled (calque#144)", o.image),
		Tags: map[string]string{
			bakedImageTagKey: o.image,
			bakedAtTagKey:    time.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return fmt.Errorf("create AMI: %w", err)
	}
	fmt.Printf("      AMI %s created, waiting for it to become available...\n", amiID)

	fmt.Printf("[4/5] waiting for %s to become available...\n", amiID)
	if err := spawnClient.WaitForAMI(ctx, acquired.Region, amiID, 15*time.Minute); err != nil {
		return fmt.Errorf("wait for AMI: %w", err)
	}

	fmt.Printf("[5/5] done.\n\nbaked AMI: %s\n\nUse it with: calque real --ami %s ...\n", amiID, amiID)
	return nil
}

// amiBakeBootstrapCommand builds the minimal one-time setup script the bake
// instance runs: pull the target image, then write a done-marker to S3 so
// amiBake's poll loop knows the pull finished (mirrors bootstrap.go's own
// S3-log-capture-on-exit pattern for post-mortem visibility on failure).
func amiBakeBootstrapCommand(image, bucket, doneKey, logKey string) string {
	return fmt.Sprintf(`#!/bin/bash
exec > /tmp/calque-ami-bake.log 2>&1
trap 'aws s3 cp /tmp/calque-ami-bake.log s3://%s/%s || true' EXIT
set -euxo pipefail
command -v docker >/dev/null || (sudo apt-get update && sudo apt-get install -y docker.io)
sudo docker pull %s
echo done | aws s3 cp - s3://%s/%s
`, bucket, logKey, image, bucket, doneKey)
}

// waitForS3Key polls for key's existence in bucket, mirroring
// internal/exec.WaitForSummaryLiveness's polling shape but without that
// function's warmd-summary/heartbeat-liveness specifics — a bake instance
// has no heartbeat tag to check, just "does this one key exist yet."
func waitForS3Key(ctx context.Context, c *s3.Client, bucket, key string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: awsString(bucket), Key: awsString(key)})
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for s3://%s/%s", timeout, bucket, key)
		}
		time.Sleep(poll)
	}
}

func awsString(s string) *string { return &s }

func orOnDemand(bidCap string) string {
	if bidCap == "" {
		return "on-demand price"
	}
	return bidCap
}

type amiListOpts struct {
	region string
}

func parseAMIListArgs(args []string) (amiListOpts, error) {
	fs := flag.NewFlagSet("ami list", flag.ExitOnError)
	region := fs.String("region", "us-west-2", "AWS region")
	if err := fs.Parse(args); err != nil {
		return amiListOpts{}, err
	}
	return amiListOpts{region: *region}, nil
}

func amiListCmd(args []string) error {
	o, err := parseAMIListArgs(args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	client, err := spawnaws.NewClientWithRegion(ctx, o.region)
	if err != nil {
		return fmt.Errorf("spawn client: %w", err)
	}
	amis, err := client.ListAMIs(ctx, o.region, nil)
	if err != nil {
		return fmt.Errorf("list AMIs: %w", err)
	}
	found := 0
	for _, a := range amis {
		if _, ok := a.Tags[bakedImageTagKey]; !ok {
			continue // not a calque-baked AMI — spawn's ListAMIs returns every AMI the account owns
		}
		found++
		fmt.Printf("%s  %s  image=%s  baked=%s\n", a.AMIID, a.Name, a.Tags[bakedImageTagKey], a.Tags[bakedAtTagKey])
	}
	if found == 0 {
		fmt.Println("no calque-baked AMIs found")
	}
	return nil
}

type amiDeleteOpts struct {
	amiID  string
	region string
}

func parseAMIDeleteArgs(args []string) (amiDeleteOpts, bool, error) {
	fs := flag.NewFlagSet("ami delete", flag.ExitOnError)
	region := fs.String("region", "us-west-2", "AWS region")
	confirm := fs.Bool("i-understand-this-deletes-the-ami", false, "required: permanently deregisters the AMI and cleans up its backing snapshot")
	if err := fs.Parse(args); err != nil {
		return amiDeleteOpts{}, false, err
	}
	if fs.NArg() < 1 {
		return amiDeleteOpts{}, false, fmt.Errorf("usage: calque ami delete <ami-id> [--region R] --i-understand-this-deletes-the-ami")
	}
	return amiDeleteOpts{amiID: fs.Arg(0), region: *region}, *confirm, nil
}

func amiDeleteCmd(args []string) error {
	o, confirm, err := parseAMIDeleteArgs(args)
	if err != nil {
		return err
	}
	if !confirm {
		return fmt.Errorf("refusing to delete: pass --i-understand-this-deletes-the-ami (permanently deregisters %s and cleans up its backing snapshot)", o.amiID)
	}
	ctx := context.Background()
	client, err := spawnaws.NewClientWithRegion(ctx, o.region)
	if err != nil {
		return fmt.Errorf("spawn client: %w", err)
	}
	res, err := client.DeleteAMI(ctx, o.region, o.amiID)
	if err != nil {
		return fmt.Errorf("delete AMI %s: %w", o.amiID, err)
	}
	fmt.Printf("deleted AMI %s (snapshots deleted: %v, retained: %v)\n", res.AMIID, res.DeletedSnapshots, res.RetainedSnapshots)
	return nil
}
