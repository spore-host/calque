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
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/lagotto/pkg/failure"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	"github.com/spore-host/calque/internal/cost"
	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/gpu"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/measure"
	"github.com/spore-host/calque/internal/plan"
	calpool "github.com/spore-host/calque/internal/pool"
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
	app, unit, _ := warmUnitForScript(ctx, o.script, o.entrypoint, rep)
	allItems := realOrSyntheticItems(unit, o.n, func(i int) any {
		return fmt.Sprintf("In one sentence, summarize why fact #%d about scientific computing matters.", i)
	}, rep)
	shs := calexec.ShardItems(allItems, shards)
	// calque#79 Part 1: when --script picked a REAL warm unit, every shard
	// ships ITS OWN body instead of always driving the hardcoded vLLM
	// reference constants — computed ONCE here (parsing is shared across
	// shards) and passed into each runShard call. Unset --script (the
	// default) reproduces prior behavior byte-for-byte.
	shardBody := calexec.ManifestBody{EnterBody: realEnterBody, MethodBody: realMethodBody, MethodArg: "prompt"}
	shardHostMode := false
	if scriptBody, ok := manifestBodyForUnit(app, unit, rep); ok {
		if err := checkInvokeSupport(app.Script, unit.method, rep); err != nil {
			return err
		}
		// GPU guard parity with dry-run (run.go's swapLegal check, §7): refuse
		// a flagged multi-GPU/coupled swap for the whole fleet rather than
		// silently launching every shard on a wrong-shaped instance.
		glog := gpu.RewriteApp(app, rep)
		if !swapLegal(glog, unit.class.Name) {
			return fmt.Errorf("gpu= swap for %q is FLAGGED (multi-GPU or coupled); out of single-node scope — see leak report", unit.class.Name)
		}
		shardBody = scriptBody
		shardHostMode = true
		rep.Addf(leak.PrimEnter, leak.KindSemanticGap, app.Script, unit.method.Line,
			"fleet driving %s's OWN parsed body (calque#79), host-mode (no docker/vLLM image pull) — if the script needs non-stdlib dependencies, they must already be on the AMI", unit.method.Name)
	}
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

	// D2 (calque#145 slice 2): provision `ceiling` WORKERS once, keep them
	// warm across every shard they serve, instead of a fresh acquire+
	// bootstrap PER SHARD (today's runWaves-based fan-out, still used below
	// for D4's re-drive fallback only). Bootstrap cost (docker pull, model
	// load) now amortizes across every shard a given worker drains, closing
	// the actual gap calque#145 tracks.
	measurements := make([]measure.Measurement, len(shs))
	shardErrs := make([]error, len(shs))
	shardFailed := make([][]int, len(shs))
	sqsClient := sqs.NewFromConfig(cfg)
	// The queue must exist before any worker boots and tries to OpenRunQueue
	// it — mirrors calque pool create's own create-queue-before-provision
	// ordering (cmd/calque/pool.go's poolCreateCmd).
	if _, err := calpool.CreateRunQueue(ctx, sqsClient, o.runID, defaultVisibilityTimeout); err != nil {
		return fmt.Errorf("create run queue: %w", err)
	}
	fleetCfg := calpool.FleetCreateConfig{
		RunID: o.runID, Region: o.region, InstanceType: o.instance,
		Workers: ceiling, MinViable: ceiling, // slice 2: require ALL requested workers, not best-effort
		Spot: o.spot, SpotMaxPrice: o.spotMaxPrice,
		TTL: o.ttl, IdleTimeout: fleetWorkerIdleTimeout.String(),
		VisibilityTimeout: defaultVisibilityTimeout,
		ManifestBucket:    o.bucket, ResultsBucket: o.bucket,
		ArtifactS3URI: fmt.Sprintf("s3://%s/%s", o.bucket, sharedLayout.ArtifactPfx),
		WorkerDir:     hostWorkerDir, RunnerPath: hostWorkerDir + "/runner.py",
		AMI: o.ami,
	}
	if err := calpool.ProvisionFleetWorkers(ctx, spawnClient, fleetCfg); err != nil {
		_ = calpool.DeleteRunQueueIfExists(ctx, sqsClient, o.runID) // nothing to drain — provisioning itself failed
		return fmt.Errorf("provision fleet workers: %w", err)
	}
	fmt.Printf("[fleet] %d worker(s) ready for run %s\n", ceiling, o.runID)
	defer func() {
		fmt.Printf("[fleet] tearing down %d worker(s) for run %s...\n", ceiling, o.runID)
		if derr := calpool.DrainFleetWorkers(context.Background(), spawnClient, o.runID, o.region); derr != nil {
			fmt.Fprintf(os.Stderr, "[fleet] WARNING: drain fleet workers failed for run %s: %v (workers' own TTL will reap)\n", o.runID, derr)
		}
		if derr := calpool.DeleteRunQueueIfExists(context.Background(), sqsClient, o.runID); derr != nil {
			fmt.Fprintf(os.Stderr, "[fleet] WARNING: delete run queue failed for run %s: %v (12h retention will reap)\n", o.runID, derr)
		}
	}()

	q, err := calpool.OpenRunQueue(ctx, sqsClient, o.runID)
	if err != nil {
		return fmt.Errorf("open just-created run queue: %w", err)
	}
	// calque#145 slice 3: snapshot the fleet's real EC2 instance IDs ONCE,
	// for WaitForSummaryLivenessAny's fleet-wide staleness check below — a
	// discovery failure degrades to nil (WaitForSummaryLivenessAny's own
	// empty-instanceIDs fallback is plain timeout-only, never a false
	// "all stale"), not a fatal error; fleet size is fixed at provision
	// time, so no mid-wait re-discovery is needed (see the plan's
	// exclusions).
	var liveInstanceIDs []string
	if workers, derr := calpool.DiscoverFleetWorkerInstances(ctx, spawnClient, o.runID, o.region); derr != nil {
		fmt.Fprintf(os.Stderr, "[fleet] WARNING: discover fleet worker instances failed for run %s: %v (falling back to timeout-only liveness)\n", o.runID, derr)
	} else {
		for _, w := range workers {
			liveInstanceIDs = append(liveInstanceIDs, w.InstanceID)
		}
	}
	// Fleet-pool acquire cost is a per-RUN fixed cost (ceiling workers'
	// worth of provisioning), not attributable to any one shard — mirrors
	// poolsubmit.go's emitKForPoolClaim precedent (AcquireSeconds: 0 for a
	// pool submission). Leaked once, loudly, since — unlike a persistent
	// pool amortizing this over its whole lifetime — a fleet run's pool is
	// short-lived (one run), so the omission is proportionally bigger.
	safeRep.Addf(leak.PrimAcquire, leak.KindSemanticGap, o.model, 0,
		"fleet-pool K under-reports acquire-wait cost: %d worker(s)' real acquisition time is not folded into any shard's AcquireWaitSeconds (reported as 0, calque#145) — a known simplification, revisit if K needs to be exact for a specific run", ceiling)
	// calque#79's Volume plumbing (volumeSpecsForApp, realrun.go) — every
	// shard shares the SAME resolved mounts, since they all drive the
	// same picked unit's own body. Computed once, outside the loop.
	shardVolumeSync, shardVolumeCommit := volumeSpecsForApp(app, o.bucket, rep)
	var wg sync.WaitGroup
	for i := range shs {
		if err := calexec.WriteManifestBody(ctx, s3c, calexec.RunLayout{
			Bucket: o.bucket, ArtifactPfx: sharedLayout.ArtifactPfx,
			ManifestKey: shs[i].ManifestKey, ResultPrefix: shs[i].ResultPrefix, SummaryKey: shs[i].SummaryKey, LogKey: shs[i].LogKey,
		}, shardBody, hostWorkerDir, shs[i].Items, shardVolumeSync, shardVolumeCommit); err != nil {
			shardErrs[i] = fmt.Errorf("shard %d write manifest: %w", shs[i].ID, err)
			continue
		}
		manifestURI := fmt.Sprintf("s3://%s/%s", o.bucket, shs[i].ManifestKey)
		// Model MUST equal o.runID, matching runFleetWorker's Config.Model
		// (cmd/warmd/main.go) — a fleet worker only ever knows its run id,
		// not a separate "model." Setting this to o.model here (the
		// poolsubmit.go-style field name) would silently drop every claim:
		// Worker.runOne's affinity check (ref.Model != w.Config.Model) acks
		// and discards a mismatched claim rather than running it.
		if err := q.Submit(ctx, calpool.ClaimRef{RunID: o.runID, Model: o.runID, ManifestURI: manifestURI}); err != nil {
			shardErrs[i] = fmt.Errorf("shard %d submit claim: %w", shs[i].ID, err)
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// No LogKey here (unlike runShard's dedicated-instance case): a
			// worker's bootstrap failure is a WORKER-level concern, already
			// caught by ProvisionFleetWorkers' MinViable check before any
			// shard is even submitted — there is no per-shard bootstrap log
			// to fast-fail against once a claim is in flight.
			shardLayout := calexec.RunLayout{
				Bucket: o.bucket, ResultPrefix: shs[i].ResultPrefix, SummaryKey: shs[i].SummaryKey,
			}
			// calque#145 slice 3: fleet-wide liveness check (fails fast only
			// if EVERY worker in liveInstanceIDs has gone stale — a single
			// dead worker among survivors is not fleet death, since SQS's
			// own visibility-timeout redelivery already routes this claim
			// to a survivor).
			summaryBytes, werr := calexec.WaitForSummaryLivenessAny(ctx, s3c, ec2c, liveInstanceIDs, shardLayout, o.deadline, 15*time.Second, staleHeartbeatAfter, nil)
			if werr != nil {
				var fleetStale *calexec.ErrFleetStale
				if errors.As(werr, &fleetStale) {
					shardErrs[i] = fmt.Errorf("shard %d: fleet went unresponsive mid-run (%w)", shs[i].ID, fleetStale)
					return
				}
				shardErrs[i] = fmt.Errorf("shard %d wait summary: %w", shs[i].ID, werr)
				return
			}
			m, failed, merr := measurementFromPoolSummary(ctx, s3c, o, shs[i], summaryBytes)
			measurements[i], shardFailed[i], shardErrs[i] = m, failed, merr
		}(i)
	}
	wg.Wait()

	// D4a (calque#145 slice 3): item-level re-drive. A shard whose CLAIM
	// completed but reported some permanently-failed item indices
	// (calpool.Summary.Failed) doesn't need D4's expensive dedicated-
	// instance fallback — the pool is healthy in this case, so just
	// resubmit ONLY those indices to the SAME pool. The re-drive writes to
	// the ORIGINAL shard's ResultPrefix (a fresh manifest/summary/log key,
	// distinguished by a "-redrive" shard key), so its results merge into
	// D3's collection for free via CollectShards' existing index-keyed
	// union — zero changes needed there. One attempt only, mirroring D4's
	// own "re-drive once" semantics; any failure here (including a stale
	// fleet) falls through to shardErrs[i], which D4 below already
	// selects on unchanged.
	for i := range shs {
		if !needsItemRedrive(shardErrs[i], shardFailed[i]) {
			continue
		}
		sub := calexec.SubShard(shs[i].ID, shs[i].Items, shardFailed[i])
		redriveKey := fmt.Sprintf("%d-redrive", shs[i].ID)
		mk, _, sk, lk := calexec.ShardLayout(base, sharedLayout.ArtifactPfx, redriveKey)
		sub.ManifestKey, sub.ResultPrefix, sub.SummaryKey, sub.LogKey = mk, shs[i].ResultPrefix, sk, lk
		fmt.Fprintf(os.Stderr, "[fleet] shard %d completed with %d permanently-failed item(s); re-driving just those on the SAME pool\n", shs[i].ID, len(shardFailed[i]))
		if werr := calexec.WriteManifestBody(ctx, s3c, calexec.RunLayout{
			Bucket: o.bucket, ArtifactPfx: sharedLayout.ArtifactPfx,
			ManifestKey: sub.ManifestKey, ResultPrefix: sub.ResultPrefix, SummaryKey: sub.SummaryKey, LogKey: sub.LogKey,
		}, shardBody, hostWorkerDir, sub.Items, shardVolumeSync, shardVolumeCommit); werr != nil {
			shardErrs[i] = fmt.Errorf("shard %d item-redrive write manifest: %w", shs[i].ID, werr)
			continue
		}
		manifestURI := fmt.Sprintf("s3://%s/%s", o.bucket, sub.ManifestKey)
		if werr := q.Submit(ctx, calpool.ClaimRef{RunID: o.runID, Model: o.runID, ManifestURI: manifestURI}); werr != nil {
			shardErrs[i] = fmt.Errorf("shard %d item-redrive submit claim: %w", shs[i].ID, werr)
			continue
		}
		redriveLayout := calexec.RunLayout{Bucket: o.bucket, ResultPrefix: sub.ResultPrefix, SummaryKey: sub.SummaryKey}
		summaryBytes, werr := calexec.WaitForSummaryLivenessAny(ctx, s3c, ec2c, liveInstanceIDs, redriveLayout, o.deadline, 15*time.Second, staleHeartbeatAfter, nil)
		if werr != nil {
			shardErrs[i] = fmt.Errorf("shard %d item-redrive wait summary: %w", shs[i].ID, werr)
			continue
		}
		// The redriven items' results now live at the ORIGINAL shard's
		// ResultPrefix (sub.ResultPrefix == shs[i].ResultPrefix), so D3's
		// existing CollectShards call picks them up automatically — no
		// re-fold of measurements[i] needed; its EnterSecondsPaid/WarmHit/
		// occupancy already correctly describe the original claim's fixed
		// costs, and per-item timing is re-derived from Collect at D3
		// regardless of which claim produced which item.
		_, _, merr := measurementFromPoolSummary(ctx, s3c, o, sub, summaryBytes)
		if merr != nil {
			shardErrs[i] = fmt.Errorf("shard %d item-redrive: %w", shs[i].ID, merr)
		}
	}

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
			m, serr := runShard(ctx, s3c, ec2c, spawnClient, o, shs[i], places, pricePerHr, tgt, shardBody, shardHostMode, safeRep, shardVolumeSync, shardVolumeCommit)
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
// ceiling fleetRun should actually use (calque#141) — now the WORKER-POOL
// size D2 provisions (calque#145 slice 2; D4's re-drive fallback still
// treats it as a runWaves concurrency bound, unchanged). A query failure
// doesn't block the run — it leaks the failure and falls back to `shards`
// unclamped (today's pre-#141 behavior); a ceiling that already covers
// every requested shard is also left unclamped (no pool-size clamping
// needed); only a genuine shortfall triggers clamping, logged both as a
// leak and to stdout.
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
			"requested %d shards but %s quota headroom in %s supports %d concurrent %s instance(s); provisioning %d worker(s) to serve them (calque#141/#145)",
			shards, market, region, rawCeiling, instance, rawCeiling)
		fmt.Printf("[fleet] quota ceiling: %d concurrent (requested %d shards) — provisioning %d worker(s)\n", rawCeiling, shards, rawCeiling)
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

// fleetWorkerIdleTimeout (calque#145 slice 2) is short, like warmd fleet's
// own default (cmd/warmd/main.go's fleetWorkerDefaultIdleTimeout) — a fleet
// run submits every shard's claim upfront (D2 above), so once a worker's
// queue goes empty there is no future submission worth waiting on the way
// a long-lived pool's IdleTimeout is sized for.
const fleetWorkerIdleTimeout = 1 * time.Minute

// runShard acquires ONE instance for a shard, writes its manifest, drives warmd,
// waits for the summary, and returns the shard's Measurement. Deferred terminate so
// a mid-run failure never leaks the instance. The returned error is non-nil when
// the shard produced no summary (so the fleet can re-drive it).
// baseTgt carries the Card (+ SharingMode) every shard shares; runShard takes
// its OWN copy since plan.Acquirer.Acquire mutates Target.Region and this is
// called from concurrent goroutines (calque#134: a shared *target.Target
// pointer across shards would race on that mutation).
func runShard(ctx context.Context, s3c *s3.Client, ec2c *ec2.Client, spawnClient *spawnaws.Client, o realOpts,
	sh calexec.Shard, places []plan.Placement, pricePerHr float64, baseTgt *target.Target, body calexec.ManifestBody, hostMode bool, rep *syncReport,
	volumeSync, volumeCommit []calexec.VolumeSyncSpec) (measure.Measurement, error) {
	shardLayout := calexec.RunLayout{
		Bucket: o.bucket, ArtifactPfx: "fleet/" + o.runID + "/artifacts",
		ManifestKey: sh.ManifestKey, ResultPrefix: sh.ResultPrefix, SummaryKey: sh.SummaryKey, LogKey: sh.LogKey,
	}
	if err := calexec.WriteManifestBody(ctx, s3c, shardLayout, body, hostWorkerDir, sh.Items, volumeSync, volumeCommit); err != nil {
		return measure.Measurement{}, fmt.Errorf("shard %d write manifest: %w", sh.ID, err)
	}
	boot := calexec.BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: o.bucket, ArtifactPrefix: shardLayout.ArtifactPfx,
		ManifestKey: shardLayout.ManifestKey, WorkerDir: hostWorkerDir, Region: o.region,
		LogKey: shardLayout.LogKey, HostMode: hostMode, ModelEnv: o.model,
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

// needsItemRedrive is D4a's shard-selection predicate (calque#145 slice 3),
// factored out as a pure function for unit-testability: a shard qualifies
// for item-level re-drive only when its D2 claim completed cleanly
// (shardErr == nil — a claim-level failure, including a stale fleet, goes
// straight to D4's dedicated-instance fallback instead) AND it reported at
// least one permanently-failed item index.
func needsItemRedrive(shardErr error, failed []int) bool {
	return shardErr == nil && len(failed) > 0
}

// measurementFromPoolSummary builds a shard's D5 measure.Measurement from a
// WORKER-POOL claim's completion summary (calque#145 slice 2) — the D2
// counterpart to runShard's own tail (used only by D4's dedicated-instance
// re-drive fallback, unchanged). warmd fleet workers write calpool.Summary
// (WarmHit/EnterSecondsPaid, internal/pool/s3.go), NOT warmd's plain
// per-run Summary (EnterSeconds/PerItemSecs) runShard parses — a real gap:
// unmarshaling a pool claim's summary into runShard's shape would silently
// zero-value every field. Per-item timing has no home in calpool.Summary at
// all (it carries no per-item series), so it's derived from the shard's own
// collected S3 results instead — mirroring poolsubmit.go's emitKForPoolClaim
// precedent exactly (calque#102).
//
// The returned []int (calque#145 slice 3) is the claim's OWN
// calpool.Summary.Failed — global item indices (warm.Item.Index, per
// DrainBatch) that permanently failed within an otherwise-completed claim.
// Non-empty alongside a nil error means the shard needs D4a's item-level
// re-drive, not D4's whole-shard dedicated-instance fallback.
func measurementFromPoolSummary(ctx context.Context, s3c *s3.Client, o realOpts, sh calexec.Shard, summaryBytes []byte) (measure.Measurement, []int, error) {
	var summary calpool.Summary
	if err := json.Unmarshal(summaryBytes, &summary); err != nil {
		return measure.Measurement{}, nil, fmt.Errorf("shard %d decode pool summary: %w", sh.ID, err)
	}
	results, _, err := calexec.Collect(ctx, s3c, o.bucket, sh.ResultPrefix, 0)
	if err != nil {
		return measure.Measurement{}, nil, fmt.Errorf("shard %d collect for measurement: %w", sh.ID, err)
	}
	perItemSecs := make([]float64, len(results))
	for i, r := range results {
		perItemSecs[i] = r.Seconds
	}
	return measurementFromPoolSummaryFields(summary, perItemSecs, o.instance), summary.Failed, nil
}

// measurementFromPoolSummaryFields is measurementFromPoolSummary's pure
// core (no ctx/S3), factored out for unit-testability — the actual D5 fix
// logic (calque#145) lives entirely here.
func measurementFromPoolSummaryFields(summary calpool.Summary, perItemSecs []float64, instance string) measure.Measurement {
	return measure.Measurement{
		CardAskedFor: "H100", InstanceUsed: instance,
		PerItem: measure.Aggregate(perItemSecs),
		Occupancy: measure.OccupancySummary{
			MeanOccupancy: summary.Occupancy.MeanOccupancy, Samples: summary.Occupancy.Samples,
			Source: summary.Occupancy.Source, Measured: summary.Occupancy.Measured,
			Scope: summary.Occupancy.ScopeOrWholeRun(),
		},
		// EnterSeconds is THE fix (calque#145): EnterSecondsPaid is 0 on a
		// warm hit (this worker was already loaded when it claimed this
		// shard) and only non-zero for whichever claim actually triggered
		// @enter. Summing this across shards (measure.FleetFold, unchanged)
		// now correctly totals to `ceiling` workers' worth of load cost,
		// not `len(shs)` shards' worth — today's bug, inherited if this
		// read summary.EnterSeconds (a dedicated single-shard process's OWN
		// full, always-nonzero paid cost) the way runShard's tail does.
		EnterSeconds: summary.EnterSecondsPaid,
		// AcquireWaitSeconds is 0 here by design — see the leak emitted
		// once in fleetRun's D2 (calque#145): the real acquire cost is a
		// per-RUN fixed cost (ceiling workers' provisioning), not
		// attributable to any single shard.
		AcquireWaitSeconds: 0,
	}
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
	fmt.Printf("\n--- cost model (§9) — FLEET of %d shard(s), MEASURED ---\n", len(perInstance))
	verdict, verr := model.Verdict(o.n)
	switch {
	case verr == cost.ErrNoComputeMeasured:
		fmt.Println("K undefined: per-item compute ~0.")
	case verr != nil:
		return fmt.Errorf("cost: %w", verr)
	default:
		fmt.Print(verdict)
	}
	// calque#145: len(perInstance) is now a SHARD count, not an instance/worker
	// count (D2 shards can share a worker) — "shard(s)" avoids implying 1:1.
	fmt.Printf("Fleet K folds %d shard(s): Σacquire=%.0fs Σenter=%.0fs across %d items @ %.0f%% occ%s.\n",
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
