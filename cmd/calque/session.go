package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	"github.com/spore-host/calque/internal/cost"
	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/measure"
	"github.com/spore-host/calque/internal/plan"
	"github.com/spore-host/calque/internal/target"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// sessionOpts controls an acquire-once / hold / run-many session.
type sessionOpts struct {
	bucket          string
	region          string
	runID           string
	instance        string
	ami             string
	model           string
	rungs           []int // the N-ramp to run sequentially on the held instance
	ttl             string
	acquireDeadline time.Duration
	ratesFP         string
}

// runSession acquires ONE GPU instance (patiently — acquisition is the expensive,
// hard part, so we hold it), prepares it once (docker + vLLM image pull), then
// drives the whole N-ramp onto it over SSM, computing K per rung. The instance is
// held for the entire ramp and terminated only at the end. This amortizes the
// painful acquisition across every test instead of re-acquiring per test.
func runSession(o sessionOpts) (err error) {
	ctx := context.Background()
	rep := &leak.Report{}
	fmt.Printf("=== calque SESSION (acquire-once, hold, run %v) model=%s instance=%s ===\n", o.rungs, o.model, o.instance)

	warmdBin, err := buildWarmd(ctx)
	if err != nil {
		return fmt.Errorf("build warmd: %w", err)
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.region))
	if err != nil {
		return err
	}
	s3c := s3.NewFromConfig(cfg)
	spawnClient, err := spawnaws.NewClientWithRegion(ctx, o.region)
	if err != nil {
		return fmt.Errorf("spawn client: %w", err)
	}

	// Artifacts live under the session prefix; each rung gets its own sub-layout.
	sessBase := "sessions/" + o.runID
	artifactPfx := sessBase + "/artifacts"
	if err := calexec.UploadArtifacts(ctx, s3c, calexec.RunLayout{Bucket: o.bucket, ArtifactPfx: artifactPfx},
		warmdBin, "worker/warm-runner/runner.py", "worker/warm-runner/occupancy.py"); err != nil {
		return fmt.Errorf("upload artifacts: %w", err)
	}
	fmt.Printf("[prep] artifacts uploaded to s3://%s/%s/\n", o.bucket, artifactPfx)

	// Price once (also R_a).
	var pricePerHr float64
	if pricer, perr := plan.NewTrufflePricer(ctx); perr == nil {
		if rate, rerr := pricer.OnDemandPrice(ctx, o.instance, o.region); rerr == nil {
			pricePerHr = rate
		}
	}

	// One-time prep bootstrap: docker + image pull, then idle. No workload — the
	// instance stays alive (bounded by TTL) so we can drive tests onto it via SSM.
	prep := calexec.SessionPrep{
		BaseImage: "vllm/vllm-openai:latest", Bucket: o.bucket, WorkerDir: hostWorkerDir,
		Region: o.region, LogKey: sessBase + "/prep.log",
	}

	// AZ sweep (offered ∩ default-subnet).
	var places []plan.Placement
	if found, aerr := calexec.AZsForInstance(ctx, ec2.NewFromConfig(cfg), o.instance); aerr == nil {
		for _, f := range found {
			places = append(places, plan.Placement{AZ: f.AZ, Subnet: f.Subnet})
		}
	}

	launcher := &plan.SpawnLauncher{
		Client: spawnClient, RunCmd: prep.PrepCommand(artifactPfx), TTL: o.ttl,
		OnComplete: "", // do NOT terminate on command completion — we hold the box
		Username:   "ubuntu", Timeout: 5 * time.Minute, AMI: o.ami, PricePerHour: pricePerHr,
		IMDSv2HopLimit: 2, RootVolumeGiB: 200,
	}
	acq := &plan.Acquirer{
		Launcher: launcher, Report: rep, Deadline: o.acquireDeadline, Placements: places,
		OnProgress: func(attempt int, code string, waited time.Duration) {
			fmt.Printf("      ...swept %d, no capacity (%s, %s)\n", attempt, code, waited.Round(time.Second))
		},
	}
	tgt := &target.Target{Card: target.DefaultCard, Instance: o.instance}
	fmt.Printf("[acquire] sniping %s in %s (patient — up to %s; $0 until it lands)...\n", o.instance, o.region, o.acquireDeadline)
	acquired, err := acq.Acquire(ctx, tgt, o.region)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	fmt.Printf("[acquire] LANDED %s (%s) after %s — HOLDING for the whole ramp\n", acquired.InstanceID, acquired.AvailabilityZone, acquired.TimeToAcquire().Round(time.Second))

	// Terminate ONCE, at the very end (or on any failure). This is the only place
	// the instance is released — we hold it across all rungs.
	defer func() {
		fmt.Printf("[teardown] terminating %s (all rungs done)\n", acquired.InstanceID)
		if tErr := spawnClient.Terminate(context.Background(), acquired.Region, acquired.InstanceID); tErr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: terminate failed for %s: %v (TTL %s reaps)\n", acquired.InstanceID, tErr, o.ttl)
			if err == nil {
				err = fmt.Errorf("terminate: %w", tErr)
			}
		}
	}()

	// Wait for SSM + the prep to finish (image pulled).
	fmt.Printf("[prep] waiting for SSM online + docker image pull...\n")
	if err := spawnClient.WaitForSSMOnline(ctx, acquired.Region, acquired.InstanceID, 10*time.Minute); err != nil {
		return fmt.Errorf("SSM never came online: %w", err)
	}
	if err := waitForPrep(ctx, s3c, o.bucket, prep.LogKey, 20*time.Minute); err != nil {
		return fmt.Errorf("prep (image pull) failed: %w", err)
	}
	fmt.Printf("[prep] image pulled; instance ready. Running ramp %v.\n", o.rungs)

	// Run each rung on the held instance via SSM.
	rates, _ := cost.LoadRates(o.ratesFP)
	for _, n := range o.rungs {
		if rerr := runRung(ctx, spawnClient, s3c, o, acquired, sessBase, n, rates, rep); rerr != nil {
			fmt.Fprintf(os.Stderr, "rung N=%d failed: %v (continuing to teardown)\n", n, rerr)
			// keep going to teardown; a failed rung shouldn't leak the instance
			break
		}
	}

	fmt.Println("\n--- leak report (§10) ---")
	rep.Summary(os.Stdout)
	return nil
}

// runRung drives one N-value test onto the held instance over SSM and emits its K.
func runRung(ctx context.Context, sc *spawnaws.Client, s3c *s3.Client, o sessionOpts,
	acq plan.Acquired, sessBase string, n int, rates *cost.Rates, rep *leak.Report) error {
	fmt.Printf("\n========== RUNG N=%d ==========\n", n)
	rungBase := fmt.Sprintf("%s/rung-%d", sessBase, n)
	layout := calexec.RunLayout{
		Bucket: o.bucket, ArtifactPfx: sessBase + "/artifacts",
		ManifestKey: rungBase + "/manifest.json", ResultPrefix: rungBase + "/results",
		SummaryKey: rungBase + "/summary.json", LogKey: rungBase + "/test.log",
	}
	items := make([]warm.Item, n)
	for i := range items {
		items[i] = warm.Item{Index: i, Payload: fmt.Sprintf("In one sentence, summarize why fact #%d about scientific computing matters.", i)}
	}
	if err := calexec.WriteManifest(ctx, s3c, layout, realEnterBody, realMethodBody, "prompt", hostWorkerDir, items); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	// Drive the test over SSM on the HELD instance (image already pulled).
	cmd := calexec.TestRunCommand("vllm/vllm-openai:latest", hostWorkerDir, o.region, o.bucket, layout.ManifestKey, o.model, layout.LogKey)
	fmt.Printf("[N=%d] running warmd-in-docker over SSM (model load once, %d items)...\n", n, n)
	// SSM RunShellScript blocks until the command finishes or the timeout; give it
	// room for model load (~2min) + N generations.
	ssmTimeout := 15*time.Minute + time.Duration(n)*2*time.Second
	if _, err := sc.RunShellScript(ctx, acq.Region, acq.InstanceID, cmd, ssmTimeout); err != nil {
		// Even on SSM error, the summary may exist; try to collect. Log the test log.
		fmt.Fprintf(os.Stderr, "[N=%d] SSM run returned error: %v\n", n, err)
	}

	summaryBytes, ok := calexec.TryGetSummary(ctx, s3c, o.bucket, layout.SummaryKey)
	if !ok {
		if lb, lok := calexec.TryGetSummary(ctx, s3c, o.bucket, layout.LogKey); lok {
			fmt.Fprintf(os.Stderr, "[N=%d] test log tail:\n%s\n", n, tail(lb, 1500))
		}
		return fmt.Errorf("no summary for rung N=%d", n)
	}
	var summary struct {
		EnterSeconds float64              `json:"enter_seconds"`
		EnterCount   int                  `json:"enter_count"`
		PerItemSecs  []float64            `json:"per_item_secs"`
		Failed       []int                `json:"failed"`
		Occupancy    calexec.OccupancyRaw `json:"occupancy"`
	}
	_ = json.Unmarshal(summaryBytes, &summary)
	results, missing, _ := calexec.Collect(ctx, s3c, o.bucket, layout.ResultPrefix, n)
	fmt.Printf("[N=%d] @enter x%d (%.1fs), %d/%d results (%d missing), occupancy %s\n",
		n, summary.EnterCount, summary.EnterSeconds, len(results), n, len(missing), occStr(summary.Occupancy))

	// Emit K for this rung.
	pi := measure.Aggregate(summary.PerItemSecs)
	occFrac := 1.0
	if summary.Occupancy.Measured && summary.Occupancy.MeanOccupancy != nil {
		occFrac = *summary.Occupancy.MeanOccupancy
	}
	_, awsMeasured, _ := rates.AWSOnDemandPerSecond(o.instance)
	model := &cost.Model{Rates: rates, M: cost.Measured{
		CardAskedFor: "H100", InstanceUsed: o.instance, SecPerItem: pi.MeanSecs,
		Occupancy: occFrac, SampleItems: pi.Count, AWSRateMeasured: awsMeasured,
		AcquireSeconds: acq.TimeToAcquire().Seconds(), EnterSeconds: summary.EnterSeconds,
	}}
	if v, verr := model.Verdict(100000); verr == nil {
		fmt.Printf("[N=%d] --- crossover K ---\n%s", n, v)
	} else if verr == cost.ErrNoComputeMeasured {
		fmt.Printf("[N=%d] K undefined (per-item ~0)\n", n)
	}
	return nil
}

// waitForPrep polls for the prep log (uploaded on prep-script exit) and checks it
// signals success.
func waitForPrep(ctx context.Context, s3c *s3.Client, bucket, logKey string, timeout time.Duration) error {
	deadline := time.Now
	_ = deadline
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	end := time.After(timeout)
	for {
		if b, ok := calexec.TryGetSummary(ctx, s3c, bucket, logKey); ok {
			if containsStr(b, "CALQUE_PREP_DONE") {
				return nil
			}
			return fmt.Errorf("prep exited without success; log tail:\n%s", tail(b, 1500))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-end:
			return fmt.Errorf("prep did not complete within %s", timeout)
		case <-tick.C:
			fmt.Printf("      ...prep running (docker pull)\n")
		}
	}
}

func containsStr(b []byte, s string) bool {
	return len(b) >= len(s) && (indexOf(b, s) >= 0)
}
func indexOf(b []byte, s string) int {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}
