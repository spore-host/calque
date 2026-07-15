package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/plan"
	"github.com/spore-host/calque/internal/target"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// smokeOpts controls the acquire-only smoke test — the FIRST billable action.
type smokeOpts struct {
	bucket   string
	region   string
	runID    string
	ttl      string
	deadline time.Duration
	instance string // override the resolved instance (capacity fallback, e.g. g6.2xlarge)
	ami      string // pinned AMI (spawn's GPU auto-select is broken; see spawn issue)
}

// smoke is the acquire-only smoke test (§16.1 de-risking): acquire a g7e, run
// warmd on the HOST (no docker/GPU/model) over a trivial 1-item job, confirm the
// result + summary land in S3, then TERMINATE. It validates the riskiest new
// integration — spawn acquire + instance-role S3 + collect + terminate — before
// any spend on real inference. Every step logs; termination is deferred so a
// mid-run failure never leaks a running instance.
func smoke(o smokeOpts) (err error) {
	ctx := context.Background()
	rep := &leak.Report{}
	fmt.Printf("=== calque smoke test (region=%s bucket=%s run=%s) ===\n", o.region, o.bucket, o.runID)

	// 0. cross-compile warmd for linux/amd64 (pure Go) into a temp path.
	warmdBin, err := buildWarmd(ctx)
	if err != nil {
		return fmt.Errorf("build warmd: %w", err)
	}
	fmt.Printf("[1/7] built warmd (linux/amd64): %s\n", warmdBin)

	// AWS clients
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.region))
	if err != nil {
		return err
	}
	s3c := s3.NewFromConfig(cfg)
	layout := calexec.NewLayout(o.bucket, o.runID)

	// 1. upload artifacts + a trivial manifest (1 item, host-runnable body).
	if err := calexec.UploadArtifacts(ctx, s3c, layout, warmdBin, "worker/warm-runner/runner.py", "worker/warm-runner/occupancy.py"); err != nil {
		return fmt.Errorf("upload artifacts: %w", err)
	}
	fmt.Printf("[2/7] uploaded artifacts to s3://%s/%s/\n", layout.Bucket, layout.ArtifactPfx)

	items := []warm.Item{{Index: 0, Payload: "smoke"}}
	// Trivial host-runnable body: no GPU, no model. Proves the plumbing only.
	if err := calexec.WriteManifest(ctx, s3c, layout,
		"self.ok = True", "return {'echo': item, 'host_smoke': True}", "item", "/opt/calque", items); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	fmt.Printf("[3/7] wrote manifest (1 item, host-mode body) to s3://%s/%s\n", layout.Bucket, layout.ManifestKey)

	// 2. build the host-mode bootstrap command.
	boot := calexec.BootstrapConfig{
		Bucket: o.bucket, ArtifactPrefix: layout.ArtifactPfx, ManifestKey: layout.ManifestKey,
		WorkerDir: "/opt/calque", Region: o.region, HostMode: true,
	}

	inst := o.instance
	if inst == "" {
		inst = "g7e.2xlarge"
	}

	// Price ONCE via truffle up front (this is also R_a for the cost model). Passing
	// it into the launcher makes spawn skip its own per-attempt Pricing-API lookup —
	// otherwise a long capacity wait fires hundreds of redundant price calls.
	var pricePerHr float64
	if pricer, perr := plan.NewTrufflePricer(ctx); perr == nil {
		if rate, rerr := pricer.OnDemandPrice(ctx, inst, o.region); rerr == nil {
			pricePerHr = rate
			fmt.Printf("      priced %s @ %s = $%.4f/hr (truffle; passed to spawn to skip re-lookup)\n", inst, o.region, rate)
		}
	}

	// 3. acquire via the Acquirer over spawn.Provision.
	spawnClient, err := spawnaws.NewClientWithRegion(ctx, o.region)
	if err != nil {
		return fmt.Errorf("spawn client: %w", err)
	}
	launcher := &plan.SpawnLauncher{
		Client: spawnClient, RunCmd: boot.Command(), TTL: o.ttl, OnComplete: "terminate",
		Username: "ubuntu", Timeout: 5 * time.Minute, AMI: o.ami, PricePerHour: pricePerHr,
	}
	acq := &plan.Acquirer{
		Launcher: launcher, Report: rep, Deadline: o.deadline,
		OnProgress: func(attempt int, code string, waited time.Duration) {
			fmt.Printf("      ...waiting for capacity (attempt %d, %s, %s)\n", attempt, code, waited.Round(time.Second))
		},
	}
	tgt := &target.Target{Card: target.DefaultCard, Instance: inst}
	fmt.Printf("[4/7] acquiring %s in %s (block-and-wait)...\n", tgt.Instance, o.region)
	acquired, err := acq.Acquire(ctx, tgt, o.region)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	fmt.Printf("      acquired %s (%s) in %s after %s\n", acquired.InstanceID, acquired.AvailabilityZone, acquired.Region, acquired.TimeToAcquire().Round(time.Second))

	// Deferred termination — even if collection fails, we don't leak the instance.
	defer func() {
		fmt.Printf("[7/7] terminating %s ...\n", acquired.InstanceID)
		if tErr := spawnClient.Terminate(context.Background(), acquired.Region, acquired.InstanceID); tErr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: terminate failed for %s: %v (spawn TTL %s will still reap it)\n", acquired.InstanceID, tErr, o.ttl)
			if err == nil {
				err = fmt.Errorf("terminate: %w", tErr)
			}
		} else {
			fmt.Printf("      terminated %s\n", acquired.InstanceID)
		}
	}()

	// 4. wait for warmd to write its summary to S3.
	fmt.Printf("[5/7] waiting for warmd summary at s3://%s/%s ...\n", layout.Bucket, layout.SummaryKey)
	summaryBytes, err := calexec.WaitForSummary(ctx, s3c, layout, o.deadline, 10*time.Second,
		func(elapsed time.Duration) { fmt.Printf("      ...still waiting (%s)\n", elapsed.Round(time.Second)) })
	if err != nil {
		return fmt.Errorf("wait for summary: %w", err)
	}
	var summary struct {
		EnterCount  int       `json:"enter_count"`
		PerItemSecs []float64 `json:"per_item_secs"`
		Failed      []int     `json:"failed"`
	}
	_ = json.Unmarshal(summaryBytes, &summary)
	fmt.Printf("      summary: @enter x%d, %d items timed, %d failed\n", summary.EnterCount, len(summary.PerItemSecs), len(summary.Failed))

	// 5. collect results ordered from S3.
	results, missing, err := calexec.Collect(ctx, s3c, layout.Bucket, layout.ResultPrefix, len(items))
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	fmt.Printf("[6/7] collected %d result(s) from S3, %d missing\n", len(results), len(missing))
	for _, r := range results {
		fmt.Printf("      result[%d] = %v (%.4fs)\n", r.Index, r.Result, r.Seconds)
	}

	if len(results) == len(items) && len(missing) == 0 {
		fmt.Println("\nSMOKE TEST PASSED: acquire -> host warmd -> S3 write -> ordered collect all worked on real hardware.")
	} else {
		return fmt.Errorf("smoke test incomplete: got %d/%d results", len(results), len(items))
	}
	return nil
}

// buildWarmd cross-compiles the warmd command for linux/amd64 (pure Go, CGO off).
func buildWarmd(ctx context.Context) (string, error) {
	out := "/tmp/calque-warmd-linux-amd64"
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/warmd")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s", err, b)
	}
	return out, nil
}
