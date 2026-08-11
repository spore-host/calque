// calque real --pool: submit a run's item batch to an existing model pool
// instead of self-acquiring a dedicated instance (calque#103).
//
// The pool's identity IS the model (docs/pool-queue-contract.md decision 2:
// single-model-per-pool) — there is no separate "pool name" to pass, --pool
// simply means "route this run through the pool already provisioned for
// --model instead of calling plan.Acquirer.Acquire directly." Default stays
// today's dedicated-acquire behavior (opt-in), matching nf-spawn's own
// pool.enabled rollout precedent.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/spore-host/calque/internal/cost"
	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/measure"
	calpool "github.com/spore-host/calque/internal/pool"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// realRunViaPool submits o's item batch to the model pool named after
// o.model, waits for the claim's completion summary, collects results, and
// emits K using the pool's OWN reported fixed-cost regime (calque#102's
// WarmHit/EnterSecondsPaid) rather than a fresh acquisition's numbers — this
// run's fixed cost is whatever the CLAIM actually paid, not what a dedicated
// run would have paid.
func realRunViaPool(o realOpts) error {
	ctx := context.Background()
	rep := &leak.Report{}
	fmt.Printf("=== calque REAL GPU run via POOL (model=%s N=%d region=%s) ===\n", o.model, o.n, o.region)

	if printOffersAndStop(bedrockOffersForModel(ctx, o.model, rep)) {
		return nil
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.region))
	if err != nil {
		return err
	}
	s3c, err := calexec.NewS3ClientForBucket(ctx, o.bucket, o.region)
	if err != nil {
		return fmt.Errorf("s3 client for bucket %q: %w", o.bucket, err)
	}
	layout := calexec.NewLayout(o.bucket, o.runID)

	items := make([]warm.Item, o.n)
	for i := range items {
		items[i] = warm.Item{Index: i, Payload: fmt.Sprintf("In one sentence, summarize why fact #%d about scientific computing matters.", i)}
	}
	if err := calexec.WriteManifest(ctx, s3c, layout, realEnterBody, realMethodBody, "prompt", hostWorkerDir, items); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	fmt.Printf("[1/4] wrote manifest (%d real prompts, vLLM @enter+@method)\n", o.n)

	sqsClient := sqs.NewFromConfig(cfg)
	q, err := calpool.OpenPoolQueue(ctx, sqsClient, o.model)
	if err != nil {
		return fmt.Errorf("open pool queue for model %q (has `calque pool create --model %s` been run?): %w", o.model, o.model, err)
	}
	manifestURI := fmt.Sprintf("s3://%s/%s", layout.Bucket, layout.ManifestKey)
	if err := q.Submit(ctx, calpool.ClaimRef{RunID: o.runID, Model: o.model, ManifestURI: manifestURI}); err != nil {
		return fmt.Errorf("submit claim to pool %q: %w", o.model, err)
	}
	fmt.Printf("[2/4] submitted claim %s to pool %q\n", o.runID, o.model)

	fmt.Printf("[3/4] waiting for a pool worker to claim + drain (up to %s)...\n", o.deadline)
	summaryBytes, err := calexec.WaitForSummary(ctx, s3c, layout, o.deadline, 15*time.Second,
		func(elapsed time.Duration) { fmt.Printf("      ...waiting (%s)\n", elapsed.Round(time.Second)) })
	if err != nil {
		var bf *calexec.ErrBootstrapFailed
		if errors.As(err, &bf) {
			return fmt.Errorf("pool worker bootstrap failed (see its own logs): %s", bf.Error())
		}
		return fmt.Errorf("wait for summary: %w", err)
	}
	var summary calpool.Summary
	if uerr := json.Unmarshal(summaryBytes, &summary); uerr != nil {
		return fmt.Errorf("decode pool summary: %w", uerr)
	}
	fmt.Printf("[4/4] claim complete: warm_hit=%v enter_seconds_paid=%.1f %d failed\n",
		summary.WarmHit, summary.EnterSecondsPaid, len(summary.Failed))

	results, missing, err := calexec.Collect(ctx, s3c, layout.Bucket, layout.ResultPrefix, len(items))
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	fmt.Printf("      collected %d/%d results (%d missing)\n", len(results), len(items), len(missing))

	if err := emitKForPoolClaim(o, results, summary); err != nil {
		return err
	}

	fmt.Println("\n--- leak report (§10) ---")
	rep.Summary(os.Stdout)
	return nil
}

// emitKForPoolClaim builds a per-item second series from the collected
// results (a pool claim's Summary carries no PerItemSecs — Sink is the only
// per-item timing source, so we derive it from warm.Result.Seconds) and folds
// the claim's OWN warm-hit fixed cost into cost.Model, rather than a fresh
// acquisition's AcquireSeconds/EnterSeconds — the honest fixed cost for THIS
// run is whatever the claim reported, not what a dedicated run would have
// measured (calque#102). Occupancy (calque#116) is likewise THIS claim's own
// measurement — the pool worker now runs occupancy.py scoped to just this
// claim's DrainBatch window (internal/pool.Worker.runOne) — rather than the
// hardcoded full-fill placeholder pool mode used before the sampler existed.
func emitKForPoolClaim(o realOpts, results []warm.Result, summary calpool.Summary) error {
	rates, err := cost.LoadRates(o.ratesFP)
	if err != nil {
		return fmt.Errorf("rates: %w", err)
	}
	perItem := make([]float64, len(results))
	for i, r := range results {
		perItem[i] = r.Seconds
	}
	pi := measure.Aggregate(perItem)
	// AcquireSeconds is 0 for a pool submission: there is no acquire-wait THIS
	// run paid — the instance was already up, provisioned once for the whole
	// pool (calque#101), not per-run. That fixed cost is real but belongs to
	// the pool's lifetime, not any single claim's K.
	_, awsMeasured, _ := rates.AWSOnDemandPerSecond(o.instance)

	// occFrac/occMeasured mirror measure.Measurement.OccupancyFraction's own
	// conservative fallback (internal/measure/measure.go): an unmeasured claim
	// reports occupancy as 1.0 (least favorable to AWS, so K stays honestly
	// pessimistic rather than silently optimistic) with occScope left as the
	// claim's own reported scope so the verdict's labeling discipline (#71)
	// still holds even in that fallback case.
	occFrac := 1.0
	occMeasured := false
	if summary.Occupancy.Measured && summary.Occupancy.MeanOccupancy != nil {
		occFrac = *summary.Occupancy.MeanOccupancy
		occMeasured = true
	}
	occScope := summary.Occupancy.ScopeOrWholeRun()

	model := &cost.Model{Rates: rates, M: cost.Measured{
		CardAskedFor: "H100", InstanceUsed: o.instance, SecPerItem: pi.MeanSecs,
		Occupancy: occFrac, SampleItems: pi.Count, AWSRateMeasured: awsMeasured,
		AcquireSeconds: 0, EnterSeconds: summary.EnterSecondsPaid,
		OccupancyScope: occScope,
		WarmHit:        summary.WarmHit,
	}}
	fmt.Println("\n--- cost model (§9) — POOL claim ---")
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
		fmt.Println("NOTE: occupancy unmeasured for this claim (sampler unavailable or no samples landed in its " +
			"inference window) — Occupancy is reported as 100% as a conservative placeholder, not a measurement.")
	} else {
		fmt.Printf("This K's occupancy is grounded in a REAL per-claim measurement: %.0f%% (%s).\n", occFrac*100, occScope)
		fmt.Printf("  %s\n", calexec.OccScopeNote(summary.Occupancy))
	}
	return nil
}
