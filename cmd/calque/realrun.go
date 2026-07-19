package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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

// realOpts controls a real GPU inference run — the headline-K vehicle.
type realOpts struct {
	bucket   string
	region   string
	runID    string
	instance string
	ami      string
	model    string // HF repo id, e.g. "Qwen/Qwen2.5-1.5B-Instruct"
	n        int    // number of prompts
	ttl      string
	deadline time.Duration
	ratesFP  string
}

// The real warm-unit bodies: actual vLLM. @enter loads the model ONCE; the
// method generates for one prompt. These mirror what map_batch_inference.py's
// @cls does — real B=1 inference, the swap-legal regime. warmd exec's them
// verbatim inside the vLLM container (runner.py wraps them as we tested).
const (
	realEnterBody = `import os
from vllm import LLM, SamplingParams
self.llm = LLM(model=os.environ["CALQUE_MODEL"], dtype="float16", gpu_memory_utilization=0.85, max_model_len=2048)
self.params = SamplingParams(temperature=0.7, max_tokens=128)`

	realMethodBody = `out = self.llm.generate([prompt], self.params)
return out[0].outputs[0].text`
)

// realRun acquires a GPU, runs real vLLM inference over N prompts under the warm
// runner (model loaded once), collects results + the tach summary from S3, folds
// them into the cost model, and emits the crossover K (§9). Deferred terminate so
// a mid-run failure never leaks the instance.
func realRun(o realOpts) (err error) {
	ctx := context.Background()
	rep := &leak.Report{}
	fmt.Printf("=== calque REAL GPU run (model=%s N=%d region=%s instance=%s) ===\n", o.model, o.n, o.region, o.instance)

	warmdBin, err := buildWarmd(ctx)
	if err != nil {
		return fmt.Errorf("build warmd: %w", err)
	}
	fmt.Printf("[1/8] built warmd (linux/amd64)\n")

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.region))
	if err != nil {
		return err
	}
	s3c := s3.NewFromConfig(cfg)
	layout := calexec.NewLayout(o.bucket, o.runID)

	if err := calexec.UploadArtifacts(ctx, s3c, layout, warmdBin, "worker/warm-runner/runner.py", "worker/warm-runner/occupancy.py"); err != nil {
		return fmt.Errorf("upload artifacts: %w", err)
	}
	fmt.Printf("[2/8] uploaded artifacts\n")

	// N real prompts. The warm runner loads the model once (@enter) then drains these.
	items := make([]warm.Item, o.n)
	for i := range items {
		items[i] = warm.Item{Index: i, Payload: fmt.Sprintf("In one sentence, summarize why fact #%d about scientific computing matters.", i)}
	}
	if err := calexec.WriteManifest(ctx, s3c, layout, realEnterBody, realMethodBody, "prompt", hostWorkerDir, items); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	fmt.Printf("[3/8] wrote manifest (%d real prompts, vLLM @enter+@method)\n", o.n)

	// Docker-mode bootstrap: pull vLLM, run --gpus all, pass the model via env, warmd
	// drives the warm runner inside the container. CALQUE_MODEL is read by @enter.
	boot := calexec.BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: o.bucket, ArtifactPrefix: layout.ArtifactPfx,
		ManifestKey: layout.ManifestKey, WorkerDir: hostWorkerDir, Region: o.region,
		LogKey: layout.LogKey, HostMode: false, ModelEnv: o.model,
	}

	// Price once via truffle (also R_a).
	var pricePerHr float64
	if pricer, perr := plan.NewTrufflePricer(ctx); perr == nil {
		if rate, rerr := pricer.OnDemandPrice(ctx, o.instance, o.region); rerr == nil {
			pricePerHr = rate
			fmt.Printf("      priced %s @ %s = $%.4f/hr (truffle)\n", o.instance, o.region, rate)
		}
	}

	// AZ sweep (offered AZs w/ default subnet).
	var places []plan.Placement
	if found, aerr := calexec.AZsForInstance(ctx, ec2.NewFromConfig(cfg), o.instance); aerr == nil {
		for _, f := range found {
			places = append(places, plan.Placement{AZ: f.AZ, Subnet: f.Subnet})
		}
	}

	spawnClient, err := spawnaws.NewClientWithRegion(ctx, o.region)
	if err != nil {
		return fmt.Errorf("spawn client: %w", err)
	}
	launcher := &plan.SpawnLauncher{
		Client: spawnClient, RunCmd: boot.Command(), TTL: o.ttl, OnComplete: "terminate",
		Username: "ubuntu", Timeout: 5 * time.Minute, AMI: o.ami, PricePerHour: pricePerHr,
		IMDSv2HopLimit: 2,   // warmd runs INSIDE docker; needs IMDS creds one hop away
		RootVolumeGiB:  200, // vLLM image + weights blow past spawn's 20 GiB default
	}
	acq := &plan.Acquirer{
		Launcher: launcher, Report: rep, Deadline: o.deadline, Placements: places,
		OnProgress: func(attempt int, code, detail string, waited time.Duration) {
			fmt.Printf("      ...swept %d attempt(s), no capacity (%s, %s)\n", attempt, code, waited.Round(time.Second))
		},
	}
	tgt := &target.Target{Card: target.DefaultCard, Instance: o.instance}
	fmt.Printf("[4/8] acquiring %s in %s (block-and-wait, AZ-sweep)...\n", o.instance, o.region)
	acquired, err := acq.Acquire(ctx, tgt, o.region)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	fmt.Printf("      acquired %s (%s) after %s\n", acquired.InstanceID, acquired.AvailabilityZone, acquired.TimeToAcquire().Round(time.Second))

	defer func() {
		fmt.Printf("[8/8] terminating %s ...\n", acquired.InstanceID)
		if tErr := spawnClient.Terminate(context.Background(), acquired.Region, acquired.InstanceID); tErr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: terminate failed for %s: %v (TTL %s will reap)\n", acquired.InstanceID, tErr, o.ttl)
			if err == nil {
				err = fmt.Errorf("terminate: %w", tErr)
			}
		} else {
			fmt.Printf("      terminated %s\n", acquired.InstanceID)
		}
	}()

	// Wait for warmd's summary. Real vLLM model load + N generations takes minutes
	// (model download + load), so allow a generous window bounded by the deadline.
	fmt.Printf("[5/8] waiting for warmd summary (vLLM load + %d generations)...\n", o.n)
	summaryBytes, err := calexec.WaitForSummary(ctx, s3c, layout, o.deadline, 15*time.Second,
		func(elapsed time.Duration) { fmt.Printf("      ...running (%s)\n", elapsed.Round(time.Second)) })
	if err != nil {
		// Fast-failure: the bootstrap exited without a summary — its log tells us why.
		var bf *calexec.ErrBootstrapFailed
		if errors.As(err, &bf) {
			fmt.Fprintf(os.Stderr, "BOOTSTRAP FAILED (fast-detected) — log tail:\n%s\n", tail([]byte(bf.BootstrapLog), 2500))
			return fmt.Errorf("bootstrap failed on the instance (see log above)")
		}
		if logBytes, lerr := getS3(ctx, s3c, o.bucket, layout.LogKey); lerr == nil {
			fmt.Fprintf(os.Stderr, "--- bootstrap.log (tail) ---\n%s\n", tail(logBytes, 2500))
		}
		return fmt.Errorf("wait for summary: %w", err)
	}
	var summary struct {
		EnterSeconds float64              `json:"enter_seconds"`
		EnterCount   int                  `json:"enter_count"`
		PerItemSecs  []float64            `json:"per_item_secs"`
		Failed       []int                `json:"failed"`
		Occupancy    calexec.OccupancyRaw `json:"occupancy"`
	}
	_ = json.Unmarshal(summaryBytes, &summary)
	fmt.Printf("[6/8] summary: @enter x%d (%.1fs load), %d items, %d failed, occupancy %s\n",
		summary.EnterCount, summary.EnterSeconds, len(summary.PerItemSecs), len(summary.Failed), occStr(summary.Occupancy))

	results, missing, err := calexec.Collect(ctx, s3c, layout.Bucket, layout.ResultPrefix, len(items))
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	fmt.Printf("[7/8] collected %d/%d results (%d missing)\n", len(results), len(items), len(missing))
	if len(results) > 0 {
		fmt.Printf("      sample result[0]: %.120q\n", fmt.Sprint(results[0].Result))
	}

	// Fold measured ground truth into the cost model and emit K.
	if err := emitK(o, summary.PerItemSecs, summary.EnterSeconds, summary.Occupancy, acquired, pricePerHr); err != nil {
		return err
	}

	fmt.Println("\n--- leak report (§10) ---")
	rep.Summary(os.Stdout)
	return nil
}

func emitK(o realOpts, perItem []float64, enterSec float64, occ calexec.OccupancyRaw, acq plan.Acquired, priceHr float64) error {
	rates, err := cost.LoadRates(o.ratesFP)
	if err != nil {
		return fmt.Errorf("rates: %w", err)
	}
	pi := measure.Aggregate(perItem)
	m := measure.Measurement{
		CardAskedFor: "H100", // map_batch asked for H100; R_m uses that (asymmetry §9)
		InstanceUsed: o.instance,
		PerItem:      pi,
		Occupancy: measure.OccupancySummary{
			MeanOccupancy: occ.MeanOccupancy, Samples: occ.Samples, Source: occ.Source, Measured: occ.Measured,
		},
		AcquiredAt: acq.AcquiredAt, TerminatedAt: time.Now(), EnterSeconds: enterSec,
		AcquireWaitSeconds: acq.TimeToAcquire().Seconds(),
	}
	occFrac, occMeasured := m.OccupancyFraction()
	_, awsMeasured, _ := rates.AWSOnDemandPerSecond(o.instance)
	model := &cost.Model{Rates: rates, M: cost.Measured{
		CardAskedFor: m.CardAskedFor, InstanceUsed: m.InstanceUsed, SecPerItem: pi.MeanSecs,
		Occupancy: occFrac, SampleItems: pi.Count, AWSRateMeasured: awsMeasured,
		AcquireSeconds: m.AcquireWaitSeconds, EnterSeconds: enterSec,
	}}
	fmt.Println("\n--- crossover K (§9) — MEASURED on real GPU ---")
	verdict, err := model.Verdict(100000)
	switch {
	case err == cost.ErrNoComputeMeasured:
		fmt.Println("K undefined: per-item compute ~0 (unexpected for real inference — check results).")
	case err != nil:
		return fmt.Errorf("cost: %w", err)
	default:
		fmt.Print(verdict)
	}
	if !occMeasured {
		fmt.Println("NOTE: occupancy unmeasured (nvidia-smi found no samples) — K's occupancy input is a proxy.")
	} else {
		fmt.Printf("This K is grounded in a REAL measured run: %d items @ %.3fs/item, %.0f%% occupancy on %s.\n",
			pi.Count, pi.MeanSecs, occFrac*100, o.instance)
	}
	return nil
}

func occStr(o calexec.OccupancyRaw) string {
	if !o.Measured || o.MeanOccupancy == nil {
		return fmt.Sprintf("unmeasured (%s)", o.Source)
	}
	// Show the primary + every metric collected, so nvidia-smi's coarse
	// utilization.gpu can be compared against DCGM SM-activity (§8, Scott's note).
	s := fmt.Sprintf("%.0f%% [primary=%s]", *o.MeanOccupancy*100, o.OccupancySource)
	if len(o.Metrics) > 0 {
		parts := ""
		for _, k := range []string{"dcgm_sm", "nvsmi_sm", "nvsmi_util"} {
			if v, ok := o.Metrics[k]; ok && v != nil {
				parts += fmt.Sprintf(" %s=%.0f%%", k, *v*100)
			}
		}
		s += " {" + parts + " }"
	}
	return s
}

func getS3(ctx context.Context, c *s3.Client, bucket, key string) ([]byte, error) {
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	b := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, e := out.Body.Read(tmp)
		b = append(b, tmp[:n]...)
		if e != nil {
			break
		}
	}
	return b, nil
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "..." + string(b[len(b)-n:])
}

// oneLine collapses a multi-line/verbose AWS error to a single trimmed log line.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}
