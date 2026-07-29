package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
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

	// D1: shard the items, each with its own manifest + result prefix.
	allItems := make([]warm.Item, o.n)
	for i := range allItems {
		allItems[i] = warm.Item{Index: i, Payload: fmt.Sprintf("In one sentence, summarize why fact #%d about scientific computing matters.", i)}
	}
	shs := calexec.ShardItems(allItems, shards)
	for i := range shs {
		mk, rp, sk, lk := calexec.ShardLayout(base, sharedLayout.ArtifactPfx, shs[i].ID)
		shs[i].ManifestKey, shs[i].ResultPrefix, shs[i].SummaryKey, shs[i].LogKey = mk, rp, sk, lk
	}

	// Price once (homogeneous fleet) — also R_a for the cost model.
	var pricePerHr float64
	if pricer, perr := plan.NewTrufflePricer(ctx); perr == nil {
		if rate, rerr := pricer.OnDemandPrice(ctx, o.instance, o.region); rerr == nil {
			pricePerHr = rate
		}
	}
	// AZ sweep shared across shards (each acquire tries every offered AZ).
	var places []plan.Placement
	if found, aerr := calexec.AZsForInstance(ctx, ec2.NewFromConfig(cfg), o.instance); aerr == nil {
		for _, f := range found {
			places = append(places, plan.Placement{AZ: f.AZ, Subnet: f.Subnet})
		}
	}

	// D2: launch every shard's acquire+bootstrap IN PARALLEL. Each shard is fully
	// independent; a leak.Report is shared, so guard it with a mutex.
	var repMu sync.Mutex
	safeRep := &syncReport{rep: rep, mu: &repMu}
	measurements := make([]measure.Measurement, len(shs))
	shardErrs := make([]error, len(shs))
	var wg sync.WaitGroup
	for i := range shs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, serr := runShard(ctx, s3c, spawnClient, o, shs[i], places, pricePerHr, safeRep)
			measurements[i] = m
			shardErrs[i] = serr
		}(i)
	}
	wg.Wait()

	// D4: fleet-level partial-failure re-drive. A shard whose instance never
	// produced a summary is re-driven ONCE onto a fresh acquire; if it still fails,
	// its items surface in the global missing[] (below) rather than being lost.
	for i := range shs {
		if shardErrs[i] != nil {
			fmt.Fprintf(os.Stderr, "[fleet] shard %d failed (%v); re-driving once on a fresh instance\n", shs[i].ID, shardErrs[i])
			m, serr := runShard(ctx, s3c, spawnClient, o, shs[i], places, pricePerHr, safeRep)
			measurements[i], shardErrs[i] = m, serr
			if serr != nil {
				safeRep.Addf(leak.PrimAcquire, leak.KindSemanticGap, o.model, 0,
					"fleet shard %d permanently failed after one re-drive (%v); its %d items surface in global missing[]", shs[i].ID, serr, len(shs[i].Items))
			}
		}
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

// runShard acquires ONE instance for a shard, writes its manifest, drives warmd,
// waits for the summary, and returns the shard's Measurement. Deferred terminate so
// a mid-run failure never leaks the instance. The returned error is non-nil when
// the shard produced no summary (so the fleet can re-drive it).
func runShard(ctx context.Context, s3c *s3.Client, spawnClient *spawnaws.Client, o realOpts,
	sh calexec.Shard, places []plan.Placement, pricePerHr float64, rep *syncReport) (measure.Measurement, error) {
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
	launcher := &plan.SpawnLauncher{
		Client: spawnClient, RunCmd: boot.Command(), TTL: o.ttl, OnComplete: "terminate",
		Username: "ubuntu", Timeout: 5 * time.Minute, AMI: o.ami, PricePerHour: pricePerHr,
		IMDSv2HopLimit: 2, RootVolumeGiB: 200,
	}
	acq := &plan.Acquirer{Launcher: launcher, Report: rep.rep, Deadline: o.deadline, Placements: places}
	tgt := &target.Target{Card: target.DefaultCard, Instance: o.instance}
	acquired, err := acq.Acquire(ctx, tgt, o.region)
	if err != nil {
		return measure.Measurement{}, fmt.Errorf("shard %d acquire: %w", sh.ID, err)
	}
	fmt.Printf("[fleet] shard %d landed %s (%s) after %s\n", sh.ID, acquired.InstanceID, acquired.AvailabilityZone, acquired.TimeToAcquire().Round(time.Second))
	defer func() {
		if tErr := spawnClient.Terminate(context.Background(), acquired.Region, acquired.InstanceID); tErr != nil {
			fmt.Fprintf(os.Stderr, "[fleet] WARNING: shard %d terminate failed for %s: %v (TTL reaps)\n", sh.ID, acquired.InstanceID, tErr)
		}
	}()

	summaryBytes, err := calexec.WaitForSummary(ctx, s3c, shardLayout, o.deadline, 15*time.Second, nil)
	if err != nil {
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
		fmt.Println("\n--- crossover K (§9) — FLEET ---\nno instance produced a measurement; K undefined")
		return nil
	}
	agg := measure.FleetFold(perInstance)
	occFrac, occMeasured := agg.OccupancyFraction()
	_, awsMeasured, _ := rates.AWSOnDemandPerSecond(o.instance)
	model := &cost.Model{Rates: rates, M: cost.Measured{
		CardAskedFor: agg.CardAskedFor, InstanceUsed: agg.InstanceUsed, SecPerItem: agg.PerItem.MeanSecs,
		Occupancy: occFrac, SampleItems: agg.PerItem.Count, AWSRateMeasured: awsMeasured,
		AcquireSeconds: agg.AcquireWaitSeconds, EnterSeconds: agg.EnterSeconds,
	}}
	fmt.Printf("\n--- crossover K (§9) — FLEET of %d instances, MEASURED ---\n", len(perInstance))
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
