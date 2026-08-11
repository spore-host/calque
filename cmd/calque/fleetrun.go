package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/lagotto/pkg/failure"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	"github.com/spore-host/calque/internal/cost"
	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/measure"
	"github.com/spore-host/calque/internal/plan"
	"github.com/spore-host/calque/internal/target"
)

// fleetRun is the multi-instance .map fan-out (spec §15, Gap D): it shards N items
// across `shards` single-node instances acquired IN PARALLEL, drives each shard's
// warmd independently, collects the UNION back into one ordered result set + one
// global missing[], re-drives a dead shard once, and folds a fleet-level K that
// sums per-instance overheads (D5). This is embarrassingly-parallel fan-out across
// independent boxes — NOT §1's forbidden multi-node/gang scheduling.
//
// It reuses the single-instance primitives verbatim: ShardItems/CollectShards
// (shard.go), the Acquirer AZ-sweep, BootstrapConfig, WaitForSummary, and the
// FleetFold cost combiner. shards<=1 falls back to the single-instance realRun.
func fleetRun(o realOpts, shards int) (err error) {
	if shards <= 1 {
		return realRun(o)
	}
	ctx := context.Background()
	rep := &leak.Report{}
	fmt.Printf("=== calque FLEET run (model=%s N=%d shards=%d region=%s instance=%s) ===\n",
		o.model, o.n, shards, o.region, o.instance)

	// Route-away gate (§11, G3): refuse to rent a FLEET for a Bedrock-exact model.
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
	s3c, err := calexec.NewS3ClientForBucket(ctx, o.bucket, o.region)
	if err != nil {
		return fmt.Errorf("s3 client for bucket %q: %w", o.bucket, err)
	}
	spawnClient, err := spawnaws.NewClientWithRegion(ctx, o.region)
	if err != nil {
		return fmt.Errorf("spawn client: %w", err)
	}

	// Artifacts are shared across shards (same warmd + scripts); upload once.
	base := "fleet/" + o.runID
	sharedLayout := calexec.RunLayout{Bucket: o.bucket, ArtifactPfx: base + "/artifacts"}
	if err := calexec.UploadArtifacts(ctx, s3c, sharedLayout, warmdBin, "worker/warm-runner/runner.py", "worker/warm-runner/occupancy.py"); err != nil {
		return fmt.Errorf("upload artifacts: %w", err)
	}
	fmt.Printf("[fleet] artifacts uploaded; sharding %d items across %d instances\n", o.n, shards)

	// D1: shard the items, each with its own manifest + result prefix. calque#136:
	// draw from --script's REAL .map()/.starmap() iterable when it's long enough,
	// else the pre-existing synthesized placeholder (unchanged when --script is
	// unset, the default).
	unit, _ := warmUnitForScript(ctx, o.script, rep)
	allItems := realOrSyntheticItems(unit, o.n, func(i int) any {
		return fmt.Sprintf("In one sentence, summarize why fact #%d about scientific computing matters.", i)
	}, rep)
	shs := calexec.ShardItems(allItems, shards)
	for i := range shs {
		mk, rp, sk, lk := calexec.ShardLayout(base, sharedLayout.ArtifactPfx, strconv.Itoa(shs[i].ID))
		shs[i].ManifestKey, shs[i].ResultPrefix, shs[i].SummaryKey, shs[i].LogKey = mk, rp, sk, lk
	}

	// Price once (homogeneous fleet) — also R_a for the cost model.
	var pricePerHr float64
	if pricer, perr := plan.NewTrufflePricer(ctx); perr == nil {
		if rate, rerr := pricer.OnDemandPrice(ctx, o.instance, o.region); rerr == nil {
			pricePerHr = rate
		}
	}
	// Shared EC2 client (stateless, safe for concurrent shard goroutines) — used
	// both for the AZ sweep below and each shard's spawn#497 liveness check.
	ec2c := ec2.NewFromConfig(cfg)
	// AZ sweep shared across shards (each acquire tries every offered AZ).
	var places []plan.Placement
	if found, aerr := calexec.AZsForInstance(ctx, ec2c, o.instance); aerr == nil {
		for _, f := range found {
			places = append(places, plan.Placement{AZ: f.AZ, Subnet: f.Subnet})
		}
	}

	if o.spot {
		// Honesty (§9/§10): a spot fleet measures K against a SPOT R_a, and any
		// shard's instance can be reclaimed mid-run. Say so loudly and leak it ONCE
		// for the whole fleet (the flag is shared fleet-wide, not per-shard), so the
		// resulting fleet K is never read as the on-demand headline number.
		bidCap := o.spotMaxPrice
		if bidCap == "" {
			bidCap = "on-demand price"
		}
		fmt.Printf("[spot] fleet acquiring on the SPOT market (max bid %s). NOTE: any shard's instance is "+
			"interruptible mid-run; any cost verdict measured here is against a SPOT rate, not the on-demand one.\n", bidCap)
		rep.Addf(leak.PrimAcquire, leak.KindSemanticGap, "fleet", 0,
			"spot acquisition: R_a is a spot rate and the instance is interruptible — this is a spot-rate cost measurement, not the on-demand one")
	}

	// calque#134: carry the script's real requested card (when --script
	// parsed one) through Recommend for every shard's Target, instead of
	// always hardcoding DefaultCard.
	tgt := recommendedTarget(unit, o.instance)

	// Pre-flight quota check (calque#141): query the account's real quota
	// headroom for o.instance/o.region/o.spot BEFORE committing to `shards`
	// concurrent acquisitions, instead of finding out via a live
	// MaxSpotInstanceCountExceeded error — the exact shape of calque#18's
	// real N=100k fleet run incident this issue tracks (2 of 10 shards failed
	// immediately against a 64-vCPU G/VT Spot quota that only fit 8 concurrent
	// g7e.2xlarge instances).
	var repMu sync.Mutex
	safeRep := &syncReport{rep: rep, mu: &repMu}
	rawCeiling, qerr := plan.QuotaCeiling(ctx, cfg, o.instance, o.region, o.spot)
	ceiling := resolveFleetCeiling(safeRep, o.model, o.instance, o.region, o.spot, shards, rawCeiling, qerr)

	// D2: launch every shard's acquire+bootstrap concurrently, bounded by the
	// quota ceiling (calque#141) — a buffered-channel semaphore instead of
	// unbounded fan-out. When ceiling >= shards this is unchanged behavior
	// (every shard launches at once); when it's less, shards launch in WAVES
	// (ceiling-many at a time, topping up as earlier shards' instances
	// terminate) instead of firing all N acquisitions in parallel and letting
	// the excess fail.
	measurements := make([]measure.Measurement, len(shs))
	shardErrs := make([]error, len(shs))
	runWaves(ceiling, len(shs), func(i int) {
		measurements[i], shardErrs[i] = runShard(ctx, s3c, ec2c, spawnClient, o, shs[i], places, pricePerHr, tgt, safeRep)
	})

	// D4: fleet-level partial-failure re-drive, through the SAME quota
	// ceiling (calque#141) rather than firing every failed shard's re-drive
	// in parallel — that mass-collision (9 failed shards re-launched at once
	// against a quota that hadn't fully freed up yet) is exactly what turned
	// a partial failure into 64,207/100,000 missing items in the real
	// incident. Kept as a SEPARATE pass after the first wave fully drains
	// (not interleaved with it) — same structure as before #141, per the
	// design's explicit recommendation against a bigger restructuring.
	var failedIdx []int
	for i := range shs {
		if shardErrs[i] != nil {
			failedIdx = append(failedIdx, i)
		}
	}
	if len(failedIdx) > 0 {
		runWaves(ceiling, len(failedIdx), func(j int) {
			i := failedIdx[j]
			if wait := redriveBackoff(shardErrs[i]); wait > 0 {
				fmt.Fprintf(os.Stderr, "[fleet] shard %d failed with a quota-exceeded error; backing off %s before re-drive so OTHER shards' instances have time to terminate (calque#141)\n", shs[i].ID, wait)
				fleetSleep(wait)
			}
			fmt.Fprintf(os.Stderr, "[fleet] shard %d failed (%v); re-driving once on a fresh instance\n", shs[i].ID, shardErrs[i])
			m, serr := runShard(ctx, s3c, ec2c, spawnClient, o, shs[i], places, pricePerHr, tgt, safeRep)
			measurements[i], shardErrs[i] = m, serr
			if serr != nil {
				safeRep.Addf(leak.PrimAcquire, leak.KindSemanticGap, o.model, 0,
					"fleet shard %d permanently failed after one re-drive (%v); its %d items surface in global missing[]", shs[i].ID, serr, len(shs[i].Items))
			}
		})
	}

	// D3: collect the union across all shards + one global missing[].
	results, missing, err := calexec.CollectShards(ctx, s3c, o.bucket, shs)
	if err != nil {
		return fmt.Errorf("collect shards: %w", err)
	}
	fmt.Printf("[fleet] collected %d/%d results across %d shards (%d missing)\n",
		len(results), o.n, len(shs), len(missing))
	if len(missing) > 0 {
		safeRep.Addf(leak.PrimMap, leak.KindSemanticGap, o.model, 0,
			"%d of %d items missing across the fleet (partial failure §10)", len(missing), o.n)
	}

	// D5: fold the per-instance measurements into one fleet K (overheads summed).
	live := measurements[:0]
	for _, m := range measurements {
		if m.PerItem.Count > 0 {
			live = append(live, m)
		}
	}
	if err := emitFleetK(o, live, pricePerHr); err != nil {
		return err
	}

	fmt.Println("\n--- leak report (§10) ---")
	rep.Summary(os.Stdout)
	return nil
}

// resolveFleetCeiling turns a QuotaCeiling query result into the concurrency
// ceiling fleetRun should actually use (calque#141). A query failure doesn't
// block the run — it leaks the failure and falls back to `shards` unclamped
// (today's pre-#141 behavior); a ceiling that already covers every requested
// shard is also left unclamped (no waves needed); only a genuine shortfall
// triggers wave-based launching, logged both as a leak and to stdout.
func resolveFleetCeiling(rep *syncReport, model, instance, region string, spot bool, shards, rawCeiling int, qerr error) int {
	switch {
	case qerr != nil:
		rep.Addf(leak.PrimAcquire, leak.KindIntegrationEdge, model, 0,
			"quota pre-flight check failed (%v); proceeding with requested --shards %d unclamped", qerr, shards)
		return shards
	case rawCeiling < shards:
		market := "on-demand"
		if spot {
			market = "spot"
		}
		rep.Addf(leak.PrimAcquire, leak.KindIntegrationEdge, model, 0,
			"requested %d shards but %s quota headroom in %s supports %d concurrent %s instance(s); launching in waves of %d (calque#141)",
			shards, market, region, rawCeiling, instance, rawCeiling)
		fmt.Printf("[fleet] quota ceiling: %d concurrent (requested %d) — launching in waves\n", rawCeiling, shards)
		return rawCeiling
	default:
		return shards // fits entirely, no clamping needed, unchanged behavior
	}
}

// runWaves runs n index-callbacks (indices [0,n)) with at most ceiling
// running concurrently, via a buffered-channel semaphore — the wave
// mechanism calque#141 needs both for the initial D2 fan-out and the D4
// re-drive pass, so it's shared rather than duplicated.
//
// This works because runShard's own deferred spawnClient.Terminate releases
// the instance BEFORE its goroutine returns — releasing the semaphore there
// naturally means "the next queued index launches once this instance tears
// down," i.e. exactly the wave behavior (launch ceiling-many now, top up as
// earlier ones finish/free capacity) without runShard itself needing to
// change at all.
//
// ceiling <= 0 is treated as n (no clamping) — a defensive fallback; callers
// (resolveFleetCeiling) already guarantee a positive ceiling on every path,
// but a semaphore sized 0 would deadlock forever, which is worse than simply
// not clamping.
func runWaves(ceiling, n int, run func(i int)) {
	if ceiling <= 0 || ceiling > n {
		ceiling = n
	}
	sem := make(chan struct{}, ceiling)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			run(i)
		}(i)
	}
	wg.Wait()
}

// quotaExceededBackoff is how long the D4 re-drive pass waits before
// re-driving a shard whose failure was specifically a quota-exceeded error
// (calque#141) — a quota wall clears when OTHER instances terminate, not
// through retrying at the normal pace, so re-driving immediately (like a
// non-quota failure does) just re-collides with the same wall. This is the
// exact failure mode that turned calque#18's real incident into
// 64,207/100,000 missing items: 9 failed shards re-driven at once against a
// quota that hadn't fully freed up yet.
const quotaExceededBackoff = 3 * time.Minute

// redriveBackoff returns how long to wait before re-driving a shard that
// failed with err, or 0 for no wait. Only a quota-exceeded failure
// (lagotto/pkg/failure.IsQuotaExceeded) gets the longer backoff; any other
// failure re-drives immediately, unchanged from before #141.
func redriveBackoff(err error) time.Duration {
	if failure.IsQuotaExceeded(err) {
		return quotaExceededBackoff
	}
	return 0
}

// fleetSleep sleeps for d before a backed-off re-drive. A package var so
// tests can inject a fake to observe the backoff decision without actually
// waiting minutes — mirroring internal/pool/queue.go's poolOpenRetryDelay
// and internal/pool/pool.go's heartbeatInterval package-var-for-testability
// pattern already used elsewhere in this codebase.
var fleetSleep = time.Sleep

// runShard acquires ONE instance for a shard, writes its manifest, drives warmd,
// waits for the summary, and returns the shard's Measurement. Deferred terminate so
// a mid-run failure never leaks the instance. The returned error is non-nil when
// the shard produced no summary (so the fleet can re-drive it).
// baseTgt carries the Card (+ SharingMode) every shard shares; runShard takes
// its OWN copy since plan.Acquirer.Acquire mutates Target.Region and this is
// called from concurrent goroutines (calque#134: a shared *target.Target
// pointer across shards would race on that mutation).
func runShard(ctx context.Context, s3c *s3.Client, ec2c *ec2.Client, spawnClient *spawnaws.Client, o realOpts,
	sh calexec.Shard, places []plan.Placement, pricePerHr float64, baseTgt *target.Target, rep *syncReport) (measure.Measurement, error) {
	shardLayout := calexec.RunLayout{
		Bucket: o.bucket, ArtifactPfx: "fleet/" + o.runID + "/artifacts",
		ManifestKey: sh.ManifestKey, ResultPrefix: sh.ResultPrefix, SummaryKey: sh.SummaryKey, LogKey: sh.LogKey,
	}
	if err := calexec.WriteManifest(ctx, s3c, shardLayout, realEnterBody, realMethodBody, "prompt", hostWorkerDir, sh.Items); err != nil {
		return measure.Measurement{}, fmt.Errorf("shard %d write manifest: %w", sh.ID, err)
	}
	boot := calexec.BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: o.bucket, ArtifactPrefix: shardLayout.ArtifactPfx,
		ManifestKey: shardLayout.ManifestKey, WorkerDir: hostWorkerDir, Region: o.region,
		LogKey: shardLayout.LogKey, HostMode: false, ModelEnv: o.model,
	}
	launchCfg := plan.SpawnLauncher{
		RunCmd: boot.Command(), TTL: o.ttl, OnComplete: "terminate",
		Username: "ubuntu", AMI: o.ami, PricePerHour: pricePerHr,
		IMDSv2HopLimit: 2, RootVolumeGiB: 200,
		Spot: o.spot, SpotMaxPrice: o.spotMaxPrice,
	}.Build()
	acq := &plan.Acquirer{LaunchConfig: launchCfg, Report: rep.rep, Deadline: o.deadline, Placements: places}
	tgt := *baseTgt // per-shard copy — Acquire mutates Region; avoid a cross-goroutine race
	tgt.Instance = o.instance
	acquired, err := acq.Acquire(ctx, &tgt, o.region)
	if err != nil {
		return measure.Measurement{}, fmt.Errorf("shard %d acquire: %w", sh.ID, err)
	}
	fmt.Printf("[fleet] shard %d landed %s (%s) after %s\n", sh.ID, acquired.InstanceID, acquired.AvailabilityZone, acquired.TimeToAcquire().Round(time.Second))
	defer func() {
		if tErr := spawnClient.Terminate(context.Background(), acquired.Region, acquired.InstanceID); tErr != nil {
			fmt.Fprintf(os.Stderr, "[fleet] WARNING: shard %d terminate failed for %s: %v (TTL reaps)\n", sh.ID, acquired.InstanceID, tErr)
		}
	}()

	// spawn#497 (spored v0.100.0+): also check spawn:last-heartbeat, so a
	// genuinely hung/gone shard fails fast instead of dead-waiting the whole
	// deadline — see realrun.go's identical wiring for the incident this closes.
	summaryBytes, err := calexec.WaitForSummaryLiveness(ctx, s3c, ec2c, acquired.InstanceID, shardLayout, o.deadline, 15*time.Second, staleHeartbeatAfter, nil)
	if err != nil {
		var stale *calexec.ErrInstanceStale
		if errors.As(err, &stale) {
			return measure.Measurement{}, fmt.Errorf("shard %d went unresponsive mid-run (%w)", sh.ID, stale)
		}
		var bf *calexec.ErrBootstrapFailed
		if errors.As(err, &bf) {
			return measure.Measurement{}, fmt.Errorf("shard %d bootstrap failed", sh.ID)
		}
		return measure.Measurement{}, fmt.Errorf("shard %d wait summary: %w", sh.ID, err)
	}
	var summary struct {
		EnterSeconds float64              `json:"enter_seconds"`
		PerItemSecs  []float64            `json:"per_item_secs"`
		Occupancy    calexec.OccupancyRaw `json:"occupancy"`
	}
	_ = json.Unmarshal(summaryBytes, &summary)

	return measure.Measurement{
		CardAskedFor: "H100", InstanceUsed: o.instance,
		PerItem: measure.Aggregate(summary.PerItemSecs),
		Occupancy: measure.OccupancySummary{
			MeanOccupancy: summary.Occupancy.MeanOccupancy, Samples: summary.Occupancy.Samples,
			Source: summary.Occupancy.Source, Measured: summary.Occupancy.Measured,
			Scope: summary.Occupancy.ScopeOrWholeRun(),
		},
		AcquiredAt: acquired.AcquiredAt, TerminatedAt: time.Now(),
		EnterSeconds: summary.EnterSeconds, AcquireWaitSeconds: acquired.TimeToAcquire().Seconds(),
	}, nil
}

// emitFleetK folds the per-instance measurements (D5) and emits one fleet K.
func emitFleetK(o realOpts, perInstance []measure.Measurement, priceHr float64) error {
	rates, err := cost.LoadRates(o.ratesFP)
	if err != nil {
		return fmt.Errorf("rates: %w", err)
	}
	if len(perInstance) == 0 {
		fmt.Println("\n--- cost model (§9) — FLEET ---\nno instance produced a measurement; K undefined")
		return nil
	}
	agg := measure.FleetFold(perInstance)
	occFrac, occMeasured := agg.OccupancyFraction()
	_, awsMeasured, _ := rates.AWSOnDemandPerSecond(o.instance)
	model := &cost.Model{Rates: rates, M: cost.Measured{
		CardAskedFor: agg.CardAskedFor, InstanceUsed: agg.InstanceUsed, SecPerItem: agg.PerItem.MeanSecs,
		Occupancy: occFrac, SampleItems: agg.PerItem.Count, AWSRateMeasured: awsMeasured,
		AcquireSeconds: agg.AcquireWaitSeconds, EnterSeconds: agg.EnterSeconds,
		OccupancyScope: agg.Occupancy.ScopeOrWholeRun(),
	}}
	fmt.Printf("\n--- cost model (§9) — FLEET of %d instances, MEASURED ---\n", len(perInstance))
	verdict, verr := model.Verdict(o.n)
	switch {
	case verr == cost.ErrNoComputeMeasured:
		fmt.Println("K undefined: per-item compute ~0.")
	case verr != nil:
		return fmt.Errorf("cost: %w", verr)
	default:
		fmt.Print(verdict)
	}
	fmt.Printf("Fleet K folds %d instances: Σacquire=%.0fs Σenter=%.0fs across %d items @ %.0f%% occ%s.\n",
		len(perInstance), agg.AcquireWaitSeconds, agg.EnterSeconds, agg.PerItem.Count, occFrac*100, proxyNote(occMeasured))
	return nil
}

func proxyNote(measured bool) string {
	if measured {
		return ""
	}
	return " (occupancy PROXY — some shard didn't measure)"
}

// syncReport guards a leak.Report for concurrent shard goroutines.
type syncReport struct {
	rep *leak.Report
	mu  *sync.Mutex
}

func (s *syncReport) Addf(p leak.Primitive, k leak.Kind, script string, line int, format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rep.Addf(p, k, script, line, format, args...)
}
