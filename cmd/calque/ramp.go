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
)

// rampOpts controls an acquire-once / hold / run-many session.
type rampOpts struct {
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
	spot            bool          // acquire on the Spot market (different capacity pool than on-demand)
	spotMaxPrice    string        // $/hr bid cap; "" => spawn caps at on-demand price
	prepTimeout     time.Duration // how long to wait for the one-time image pull; 0 => 30m default
	concurrency     int           // items warmd keeps in flight per rung; 0/1 => serial (occupancy knob)
	batchSize       int           // items per micro-batch (real vLLM occupancy lever); 0/1 => per-item
	// fallbackRegions, in preference order, are tried (via plan.AcquireMultiRegion,
	// calque#95) if region has no capacity — region-thin GPU families (g7e) can be
	// out of capacity EVERYWHERE in one region while another has it (see
	// docs/measured-runs/2026-07-28-qwen-g7e-spot-ramp.md). Empty => today's
	// single-region behavior, unchanged.
	fallbackRegions []string
	// script, when set, is a Modal script to parse for its REAL .map()/
	// .starmap() iterable (calque#136) — opt-in; "" (the default) reproduces
	// today's synthesized-prompt behavior exactly, since runRamp doesn't
	// otherwise parse any script at all.
	script string
}

// runRamp acquires ONE GPU instance (patiently — acquisition is the expensive,
// hard part, so we hold it), prepares it once (docker + vLLM image pull), then
// drives the whole N-ramp onto it over SSM, computing K per rung. The instance is
// held for the entire ramp and terminated only at the end. This amortizes the
// painful acquisition across every test instead of re-acquiring per test.
func runRamp(o rampOpts) (err error) {
	ctx := context.Background()
	rep := &leak.Report{}
	fmt.Printf("=== calque RAMP (acquire-once, hold, run %v) model=%s instance=%s ===\n", o.rungs, o.model, o.instance)

	// Route-away gate (§11, G3): refuse to hold a billable GPU for a model that's
	// already an exact Bedrock API call, before the (slow, expensive) acquisition.
	if printOffersAndStop(bedrockOffersForModel(ctx, o.model, rep)) {
		return nil
	}

	warmdBin, err := buildWarmd(ctx)
	if err != nil {
		return fmt.Errorf("build warmd: %w", err)
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.region))
	if err != nil {
		return err
	}
	// S3 client targets the BUCKET's region (may differ from compute --region);
	// EC2/spawn stay on --region. See NewS3ClientForBucket (cross-region 301 fix).
	s3c, err := calexec.NewS3ClientForBucket(ctx, o.bucket, o.region)
	if err != nil {
		return fmt.Errorf("s3 client for bucket %q: %w", o.bucket, err)
	}
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
		Region: o.region, LogKey: sessBase + "/prep.log", DoneKey: sessBase + "/prep.done",
	}

	// AZ sweep (offered ∩ default-subnet), per candidate region (calque#95):
	// o.region always gets its own lookup; each o.fallbackRegions entry needs the
	// SAME lookup done again in ITS OWN region — an AZ/subnet pair from one region
	// says nothing about another.
	regions := append([]string{o.region}, o.fallbackRegions...)
	placementsByRegion := make(map[string][]plan.Placement, len(regions))
	for _, r := range regions {
		regionCfg := cfg
		if r != o.region {
			var rerr error
			regionCfg, rerr = config.LoadDefaultConfig(ctx, config.WithRegion(r))
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "WARNING: could not load AWS config for fallback region %s: %v (will sweep with EC2-chosen AZ)\n", r, rerr)
				continue
			}
		}
		found, aerr := calexec.AZsForInstance(ctx, ec2.NewFromConfig(regionCfg), o.instance)
		if aerr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: AZ lookup failed for %s in %s: %v (will sweep with EC2-chosen AZ)\n", o.instance, r, aerr)
			continue
		}
		var places []plan.Placement
		for _, f := range found {
			places = append(places, plan.Placement{AZ: f.AZ, Subnet: f.Subnet})
		}
		placementsByRegion[r] = places
	}
	places := placementsByRegion[o.region]

	// calque#136/#134: parse --script (if any) ONCE, up front — its real
	// .map()/.starmap() iterable doesn't change between rungs (only how many
	// of its items each rung's own --n draws from), and its GPU card (if any)
	// is what the Target below should carry instead of always DefaultCard.
	// calque#79 Part 1 scope note: unlike real/fleetrun, ramp does NOT yet
	// ship a --script-picked unit's OWN body — ramp's SessionPrep pulls the
	// vLLM docker image unconditionally, before any script-driven host-vs-
	// docker-mode decision could apply, and --batch-size's micro-batch body
	// selection (below) would need to compose with a script-picked body too.
	// Left as a follow-up; ramp's items/card DO already reflect --script
	// (calque#134/#136), only the executed body doesn't yet.
	_, unit, _ := warmUnitForScript(ctx, o.script, "", rep)

	// calque#148: see realrun.go's identical fix — without this, the held
	// instance has no credentials for its own bootstrap's aws s3 cp/sync
	// calls, including its own failure log.
	iamProfile, err := plan.RealRunInstanceProfile(ctx, spawnClient, o.region, o.bucket)
	if err != nil {
		return fmt.Errorf("set up IAM instance profile: %w", err)
	}
	launchCfg := plan.SpawnLauncher{
		RunCmd: prep.PrepCommand(artifactPfx), TTL: o.ttl,
		OnComplete: "", // do NOT terminate on command completion — we hold the box
		Username:   "ubuntu", AMI: o.ami, PricePerHour: pricePerHr,
		IMDSv2HopLimit: 2, RootVolumeGiB: 200,
		Spot: o.spot, SpotMaxPrice: o.spotMaxPrice,
		IamInstanceProfile: iamProfile,
		RunID:              o.runID, Command: "ramp",
	}.Build()
	if o.spot {
		// Honesty (§9/§10): a spot ramp measures K against a SPOT R_a, and the box
		// can be reclaimed mid-ramp. Say so loudly and leak it, so the resulting K
		// is never read as the on-demand headline number.
		bidCap := o.spotMaxPrice
		if bidCap == "" {
			bidCap = "on-demand price"
		}
		fmt.Printf("[spot] acquiring on the SPOT market (max bid %s). NOTE: interruptible mid-ramp; "+
			"any cost verdict measured here is against a SPOT rate, not the on-demand one.\n", bidCap)
		rep.Addf(leak.PrimAcquire, leak.KindSemanticGap, "session", 0,
			"spot acquisition: R_a is a spot rate and the instance is interruptible — this is a spot-rate cost measurement, not the on-demand one")
	}
	round := 0
	lastDetail := ""
	acq := &plan.Acquirer{
		LaunchConfig: launchCfg, Report: rep, Deadline: o.acquireDeadline, Placements: places,
		OnProgress: func(attempt int, code, detail string, waited time.Duration) {
			// Print the full AWS message on the first round, whenever it CHANGES, and
			// every 10th round — so a capacity opening (message names an AZ) or a
			// non-capacity failure is visible, not just the bare code.
			round++
			if round == 1 || detail != lastDetail || round%10 == 0 {
				fmt.Printf("      ...swept %d (%s, %s): %s\n", attempt, code, waited.Round(time.Second), oneLine(detail))
				lastDetail = detail
			} else {
				fmt.Printf("      ...swept %d, no capacity (%s, %s)\n", attempt, code, waited.Round(time.Second))
			}
		},
	}
	tgt := recommendedTarget(unit, o.instance)
	var acquired plan.Acquired
	if len(o.fallbackRegions) > 0 {
		fmt.Printf("[acquire] sniping %s in %s, falling back to %v (patient — up to %s; $0 until it lands)...\n",
			o.instance, o.region, o.fallbackRegions, o.acquireDeadline)
		acquired, err = acq.AcquireMultiRegion(ctx, tgt, regions, placementsByRegion)
	} else {
		fmt.Printf("[acquire] sniping %s in %s (patient — up to %s; $0 until it lands)...\n", o.instance, o.region, o.acquireDeadline)
		acquired, err = acq.Acquire(ctx, tgt, o.region)
	}
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
	prepTimeout := o.prepTimeout
	if prepTimeout <= 0 {
		prepTimeout = 30 * time.Minute // cold vLLM image pull can exceed the old 20m
	}
	if err := waitForPrep(ctx, s3c, o.bucket, prep.DoneKey, prep.LogKey, prepTimeout); err != nil {
		return fmt.Errorf("prep (image pull) failed: %w", err)
	}
	fmt.Printf("[prep] image pulled; instance ready. Running ramp %v.\n", o.rungs)

	// Run each rung on the held instance via SSM (unit already parsed above).
	rates, _ := cost.LoadRates(o.ratesFP)
	for _, n := range o.rungs {
		if rerr := runRung(ctx, spawnClient, s3c, o, acquired, sessBase, n, rates, rep, unit); rerr != nil {
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
func runRung(ctx context.Context, sc *spawnaws.Client, s3c *s3.Client, o rampOpts,
	acq plan.Acquired, sessBase string, n int, rates *cost.Rates, rep *leak.Report, unit warmUnit) error {
	fmt.Printf("\n========== RUNG N=%d ==========\n", n)
	rungBase := fmt.Sprintf("%s/rung-%d", sessBase, n)
	layout := calexec.RunLayout{
		Bucket: o.bucket, ArtifactPfx: sessBase + "/artifacts",
		ManifestKey: rungBase + "/manifest.json", ResultPrefix: rungBase + "/results",
		SummaryKey: rungBase + "/summary.json", LogKey: rungBase + "/test.log",
		Concurrency: o.concurrency, BatchSize: o.batchSize,
	}
	// calque#136: this rung's real items when --script named one long enough
	// for THIS rung's own n, else the pre-existing synthesized placeholder —
	// unchanged when --script is unset.
	items := realOrSyntheticItems(unit, n, func(i int) any {
		return fmt.Sprintf("In one sentence, summarize why fact #%d about scientific computing matters.", i)
	}, rep)
	// Batch mode uses the LIST-shaped body + arg `prompts` (one vLLM .generate(list)
	// call per batch); the default single-item path is unchanged.
	methodBody, methodArg := realMethodBody, "prompt"
	if o.batchSize > 1 {
		methodBody, methodArg = realBatchMethodBody, "prompts"
	}
	if err := calexec.WriteManifest(ctx, s3c, layout, realEnterBody, methodBody, methodArg, hostWorkerDir, items); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	// Drive the test over SSM on the HELD instance (image already pulled). The
	// host-side occupancy JSON (with true DCGM SM-activity) lands at occKey, and its
	// raw timestamped samples at occSamplesKey — the host sampler is outside warmd's
	// container, so the INFERENCE-WINDOW re-average (#71) happens here, not in warmd.
	occKey := rungBase + "/occupancy-host.json"
	occSamplesKey := rungBase + "/occupancy-host.jsonl"
	cmd := calexec.TestRunCommand("vllm/vllm-openai:latest", hostWorkerDir, o.region, o.bucket, layout.ManifestKey, o.model, layout.LogKey, occKey, occSamplesKey)
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
		EnterSeconds   float64              `json:"enter_seconds"`
		EnterCount     int                  `json:"enter_count"`
		PerItemSecs    []float64            `json:"per_item_secs"`
		Failed         []int                `json:"failed"`
		Occupancy      calexec.OccupancyRaw `json:"occupancy"`
		InferenceSpans []calexec.Span       `json:"inference_spans"`
	}
	_ = json.Unmarshal(summaryBytes, &summary)
	// Prefer the HOST-sampled occupancy (has true DCGM SM-activity; dcgmi isn't in
	// the container). Fall back to the container sampler's value if the host file
	// is absent.
	if hb, hok := calexec.TryGetSummary(ctx, s3c, o.bucket, occKey); hok {
		var hostOcc calexec.OccupancyRaw
		if json.Unmarshal(hb, &hostOcc) == nil && hostOcc.Measured {
			if hostOcc.Scope == "" {
				hostOcc.Scope = calexec.ScopeWholeRun // host summary is a whole-life mean
			}
			summary.Occupancy = hostOcc
		}
	}
	// Re-average the HOST samples over warmd's inference spans (#71): the sampler's
	// own mean spans the ~200s @enter model load, so it understates steady-state fill
	// and moves the wrong way as inference speeds up. Falls back to the labeled
	// whole-run mean whenever windowing isn't possible — a weaker number that says so,
	// never a fabricated one.
	wholeRun := summary.Occupancy
	if sb, sok := calexec.TryGetSummary(ctx, s3c, o.bucket, occSamplesKey); sok && len(summary.InferenceSpans) > 0 {
		samples := calexec.ParseOccSamples(sb)
		if windowed, wok := calexec.OccupancyInWindows(samples, summary.InferenceSpans, wholeRun.IntervalS); wok {
			summary.Occupancy = windowed
		} else {
			fmt.Fprintf(os.Stderr, "[N=%d] NOTE: no host occupancy samples fell in the inference window "+
				"(%d parsed, %d span(s)); using the WHOLE-RUN mean (load-contaminated)\n", n, len(samples), len(summary.InferenceSpans))
		}
	} else if len(summary.InferenceSpans) == 0 {
		fmt.Fprintf(os.Stderr, "[N=%d] NOTE: warmd reported no inference spans (older worker image?); "+
			"occupancy is the WHOLE-RUN mean and includes the @enter load\n", n)
	}
	results, missing, _ := calexec.Collect(ctx, s3c, o.bucket, layout.ResultPrefix, n)
	fmt.Printf("[N=%d] @enter x%d (%.1fs), %d/%d results (%d missing), occupancy %s\n",
		n, summary.EnterCount, summary.EnterSeconds, len(results), n, len(missing), occStr(summary.Occupancy))
	fmt.Printf("[N=%d]   %s\n", n, calexec.OccScopeNote(summary.Occupancy))
	// Show BOTH scopes when they differ, so the load-vs-fill split is visible rather
	// than replaced. A large gap is the signal that load dominates this rung.
	if wholeRun.Measured && wholeRun.MeanOccupancy != nil && summary.Occupancy.Scope == calexec.ScopeInference {
		fmt.Printf("[N=%d]   (whole-run mean was %.0f%% — the difference IS the one-time %.0fs model load)\n",
			n, *wholeRun.MeanOccupancy*100, summary.EnterSeconds)
	}

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
		OccupancyScope: summary.Occupancy.ScopeOrWholeRun(),
	}}
	if v, verr := model.Verdict(100000); verr == nil {
		fmt.Printf("[N=%d] --- cost model ---\n%s", n, v)
	} else if verr == cost.ErrNoComputeMeasured {
		fmt.Printf("[N=%d] K undefined (per-item ~0)\n", n)
	}
	return nil
}

// waitForPrep waits for prep to finish by polling the DONE MARKER (doneKey), which
// the prep script writes only after `docker pull` succeeds — decoupled from script
// exit. On each tick it also fetches the heartbeat-streamed log so a slow/hung pull
// is observable, and on timeout it returns the log tail as the diagnostic (the old
// design returned a bare "did not complete" with nothing, because the log couldn't
// arrive until the pull it was waiting on had already finished — #18).
func waitForPrep(ctx context.Context, s3c *s3.Client, bucket, doneKey, logKey string, timeout time.Duration) error {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	end := time.After(timeout)
	for {
		// Success is the marker's existence — independent of the log upload.
		if _, ok := calexec.TryGetSummary(ctx, s3c, bucket, doneKey); ok {
			return nil
		}
		// A prep that exited WITHOUT the marker (pull failed) shows CALQUE_PREP_DONE
		// absent but the log present with an error — surface that immediately.
		if b, ok := calexec.TryGetSummary(ctx, s3c, bucket, logKey); ok && containsStr(b, "CALQUE_PREP_DONE") {
			// Log says done but marker not yet visible (S3 eventual consistency): loop.
			_ = b
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-end:
			if b, ok := calexec.TryGetSummary(ctx, s3c, bucket, logKey); ok {
				return fmt.Errorf("prep did not complete within %s; log tail:\n%s", timeout, tail(b, 2000))
			}
			return fmt.Errorf("prep did not complete within %s (no log streamed — SSM/prep may not have started)", timeout)
		case <-tick.C:
			fmt.Printf("      ...prep running (docker pull; streaming log to s3://%s/%s)\n", bucket, logKey)
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
