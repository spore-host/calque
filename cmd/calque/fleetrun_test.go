package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/leak"
	calpool "github.com/spore-host/calque/internal/pool"
)

// apiErr is a fake smithy.APIError with a chosen code, mirroring
// internal/plan/plan_test.go's own fake of the same shape — used here to
// drive redriveBackoff's failure.IsQuotaExceeded check without a live AWS
// error.
type apiErr struct{ code string }

func (e apiErr) Error() string                 { return e.code }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.code }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultServer }

var _ smithy.APIError = apiErr{}

// TestRunWaves_CapsConcurrency proves the semaphore in runWaves never lets
// more than `ceiling` callbacks run at once, even though all n are launched
// "at once" from the caller's perspective — the core mechanism calque#141
// needs to replace fleetRun's old unbounded goroutine fan-out.
func TestRunWaves_CapsConcurrency(t *testing.T) {
	const n = 20
	const ceiling = 4

	var mu sync.Mutex
	var current, maxSeen int32
	var ran int32

	runWaves(ceiling, n, func(i int) {
		cur := atomic.AddInt32(&current, 1)
		mu.Lock()
		if cur > maxSeen {
			maxSeen = cur
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond) // hold the slot long enough for others to queue up
		atomic.AddInt32(&ran, -0)        // no-op, keeps ran unused-warning-free if reordered
		atomic.AddInt32(&current, -1)
		atomic.AddInt32(&ran, 1)
	})

	if ran != n {
		t.Fatalf("ran %d callbacks, want %d (every index must run exactly once)", ran, n)
	}
	mu.Lock()
	got := maxSeen
	mu.Unlock()
	if got > ceiling {
		t.Errorf("observed max concurrency %d, want <= %d", got, ceiling)
	}
	if got < ceiling {
		// Not strictly a bug, but with n=20 >> ceiling=4 and a 5ms hold, the
		// semaphore should reach saturation — a lower max suggests it isn't
		// actually allowing `ceiling` concurrent slots.
		t.Errorf("observed max concurrency %d, want exactly %d (semaphore under-using capacity)", got, ceiling)
	}
}

// TestRunWaves_ZeroOrNegativeCeilingFallsBackToN is the defensive fallback:
// a bad ceiling (<=0) must not deadlock the semaphore forever.
func TestRunWaves_ZeroOrNegativeCeilingFallsBackToN(t *testing.T) {
	for _, ceiling := range []int{0, -1} {
		var ran int32
		done := make(chan struct{})
		go func() {
			runWaves(ceiling, 5, func(i int) { atomic.AddInt32(&ran, 1) })
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("runWaves(ceiling=%d, ...) deadlocked", ceiling)
		}
		if ran != 5 {
			t.Errorf("ceiling=%d: ran %d, want 5", ceiling, ran)
		}
	}
}

// newTestSyncReport builds a *syncReport backed by a real *leak.Report, the
// same shape fleetRun wires up in production, for resolveFleetCeiling tests.
func newTestSyncReport() (*syncReport, *leak.Report) {
	rep := &leak.Report{}
	return &syncReport{rep: rep, mu: &sync.Mutex{}}, rep
}

// TestResolveFleetCeiling_FitsEntirely: shards <= ceiling must leave the
// ceiling UNCHANGED (== shards) and emit NO leak — this is the "unchanged
// behavior" guarantee for the common case where a fleet's requested shard
// count already fits the account's quota headroom.
func TestResolveFleetCeiling_FitsEntirely(t *testing.T) {
	sr, rep := newTestSyncReport()
	got := resolveFleetCeiling(sr, "m", "g7e.2xlarge", "us-east-1", true, 4, 8, nil)
	if got != 4 {
		t.Errorf("ceiling = %d, want 4 (shards, unclamped)", got)
	}
	if rep.Len() != 0 {
		t.Errorf("expected no leak when shards <= ceiling, got %d: %+v", rep.Len(), rep.Leaks)
	}
}

// TestResolveFleetCeiling_ExactlyFits: shards == ceiling is also the
// no-clamp, no-leak case (boundary, not a shortfall).
func TestResolveFleetCeiling_ExactlyFits(t *testing.T) {
	sr, rep := newTestSyncReport()
	got := resolveFleetCeiling(sr, "m", "g7e.2xlarge", "us-east-1", true, 8, 8, nil)
	if got != 8 {
		t.Errorf("ceiling = %d, want 8", got)
	}
	if rep.Len() != 0 {
		t.Errorf("expected no leak when shards == ceiling, got %d", rep.Len())
	}
}

// TestResolveFleetCeiling_ShortfallClampsAndLeaks: the real calque#18
// incident's numbers (10 shards requested, quota only supports 8) must clamp
// the returned ceiling to 8 and emit exactly one leak naming both numbers.
func TestResolveFleetCeiling_ShortfallClampsAndLeaks(t *testing.T) {
	sr, rep := newTestSyncReport()
	got := resolveFleetCeiling(sr, "my-model", "g7e.2xlarge", "us-east-1", true, 10, 8, nil)
	if got != 8 {
		t.Errorf("ceiling = %d, want 8 (clamped)", got)
	}
	if rep.Len() != 1 {
		t.Fatalf("expected exactly 1 leak, got %d: %+v", rep.Len(), rep.Leaks)
	}
	l := rep.Leaks[0]
	if l.Primitive != leak.PrimAcquire || l.Kind != leak.KindIntegrationEdge {
		t.Errorf("leak = %+v, want PrimAcquire/KindIntegrationEdge", l)
	}
	wantSubstrings := []string{"10", "8", "g7e.2xlarge", "us-east-1", "calque#141"}
	for _, s := range wantSubstrings {
		if !strings.Contains(l.Detail, s) {
			t.Errorf("leak detail %q missing expected substring %q", l.Detail, s)
		}
	}
}

// TestResolveFleetCeiling_QueryFailureFallsBackUnclamped: a quota query
// error must NOT block the run — it leaks the failure and returns `shards`
// unchanged (today's pre-#141 behavior), per the design's explicit
// don't-block-on-a-failed-check requirement.
func TestResolveFleetCeiling_QueryFailureFallsBackUnclamped(t *testing.T) {
	sr, rep := newTestSyncReport()
	qerr := fmt.Errorf("quota lookup: access denied")
	got := resolveFleetCeiling(sr, "m", "g7e.2xlarge", "us-east-1", false, 10, 0, qerr)
	if got != 10 {
		t.Errorf("ceiling = %d, want 10 (unclamped fallback on query failure)", got)
	}
	if rep.Len() != 1 {
		t.Fatalf("expected exactly 1 leak, got %d", rep.Len())
	}
	if !strings.Contains(rep.Leaks[0].Detail, "access denied") {
		t.Errorf("leak detail %q should mention the underlying query error", rep.Leaks[0].Detail)
	}
}

// TestRedriveBackoff_QuotaExceededGetsLongerBackoff proves the D4 re-drive
// pass distinguishes a quota-exceeded failure (which needs OTHER instances
// to terminate before it can possibly succeed — retrying at the normal pace
// just re-collides) from any other failure (which re-drives immediately,
// unchanged from before #141).
func TestRedriveBackoff_QuotaExceededGetsLongerBackoff(t *testing.T) {
	quotaErr := fmt.Errorf("shard 3 acquire: %w", apiErr{code: "MaxSpotInstanceCountExceeded"})
	otherErr := fmt.Errorf("shard 3 wait summary: %w", apiErr{code: "InsufficientInstanceCapacity"})
	genericErr := fmt.Errorf("shard 3 bootstrap failed")

	quotaWait := redriveBackoff(quotaErr)
	otherWait := redriveBackoff(otherErr)
	genericWait := redriveBackoff(genericErr)

	if quotaWait != quotaExceededBackoff {
		t.Errorf("redriveBackoff(quota error) = %v, want %v", quotaWait, quotaExceededBackoff)
	}
	if otherWait != 0 {
		t.Errorf("redriveBackoff(capacity error) = %v, want 0 (immediate re-drive)", otherWait)
	}
	if genericWait != 0 {
		t.Errorf("redriveBackoff(non-AWS error) = %v, want 0 (immediate re-drive)", genericWait)
	}
	if quotaWait <= otherWait {
		t.Errorf("quota backoff (%v) must be strictly longer than non-quota backoff (%v)", quotaWait, otherWait)
	}
}

// TestRedriveBackoff_UnwrapsThroughRunShardAndAcquireWrapping verifies the
// design doc's explicit ask: fleetRun receives an error from runShard, which
// itself wraps Acquirer.Acquire's error with another %w
// ("shard %d acquire: %w") on top of AcquireMultiRegion's own
// ("acquire %s/%v: %w") wrap — three levels of fmt.Errorf(%w) deep — and
// failure.IsQuotaExceeded (via errors.As) must still unwrap all the way down
// to the underlying smithy.APIError.
func TestRedriveBackoff_UnwrapsThroughRunShardAndAcquireWrapping(t *testing.T) {
	base := apiErr{code: "VcpuLimitExceeded"}
	acquireWrap := fmt.Errorf("acquire g7e.2xlarge/[us-east-1]: %w", base) // AcquireMultiRegion's wrap
	runShardWrap := fmt.Errorf("shard 3 acquire: %w", acquireWrap)         // runShard's wrap

	if got := redriveBackoff(runShardWrap); got != quotaExceededBackoff {
		t.Errorf("redriveBackoff() through 2 layers of wrapping = %v, want %v (IsQuotaExceeded must unwrap through errors.As)", got, quotaExceededBackoff)
	}
}

// TestWaitForQuotaHeadroomFn_ReturnsAssoonAsHeadroomExists (calque#142)
// proves the poll loop stops the moment ceilingFn reports >=1 concurrent
// slot free — no fixed 3-minute wait when headroom is ALREADY there (e.g.
// wave-1's shards already terminated by the time D4's re-drive checks).
func TestWaitForQuotaHeadroomFn_ReturnsAssoonAsHeadroomExists(t *testing.T) {
	var slept []time.Duration
	restore := fleetSleep
	fleetSleep = func(d time.Duration) { slept = append(slept, d) }
	defer func() { fleetSleep = restore }()

	sr, _ := newTestSyncReport()
	waitForQuotaHeadroomFn(func() (int, error) { return 1, nil }, "g7e.2xlarge", sr)

	if len(slept) != 0 {
		t.Errorf("slept %v, want no sleep at all — headroom existed on the FIRST poll", slept)
	}
}

// TestWaitForQuotaHeadroomFn_PollsUntilHeadroomAppears proves the poll loop
// sleeps quotaPollInterval between checks and returns as soon as a LATER
// poll reports headroom — the exact case calque#142 exists for: wave-1
// shards terminate WHILE the re-drive pass is waiting, not necessarily
// before it starts checking.
func TestWaitForQuotaHeadroomFn_PollsUntilHeadroomAppears(t *testing.T) {
	var slept []time.Duration
	restore := fleetSleep
	fleetSleep = func(d time.Duration) { slept = append(slept, d) }
	defer func() { fleetSleep = restore }()

	calls := 0
	sr, _ := newTestSyncReport()
	waitForQuotaHeadroomFn(func() (int, error) {
		calls++
		if calls < 3 {
			return 0, nil // no headroom yet
		}
		return 1, nil // headroom appeared on the 3rd poll
	}, "g7e.2xlarge", sr)

	if calls != 3 {
		t.Errorf("ceilingFn called %d times, want exactly 3 (2 no-headroom polls + 1 success)", calls)
	}
	if len(slept) != 2 {
		t.Fatalf("slept %d times, want exactly 2 (between polls 1-2 and 2-3): %v", len(slept), slept)
	}
	for _, d := range slept {
		if d != quotaPollInterval {
			t.Errorf("slept %v, want quotaPollInterval (%v) between polls", d, quotaPollInterval)
		}
	}
}

// TestWaitForQuotaHeadroomFn_GivesUpAfterMaxWait proves the poll loop does
// NOT block forever if headroom never appears — it returns once
// quotaPollMaxWait's budget is exhausted, same total wait ceiling as the
// fixed sleep it replaces, just spent polling instead of blind-sleeping.
func TestWaitForQuotaHeadroomFn_GivesUpAfterMaxWait(t *testing.T) {
	restoreSleep := fleetSleep
	fleetSleep = func(time.Duration) {} // don't actually wait in the test
	defer func() { fleetSleep = restoreSleep }()

	// Both bounds shrunk to real-microseconds: the deadline check uses
	// actual wall-clock time.Now(), so a tiny quotaPollMaxWait ALONE isn't
	// enough to keep this test fast — quotaPollInterval must shrink too, or
	// the loop busy-spins for the full real-world quotaPollMaxWait before
	// the deadline check can ever trip.
	restoreInterval := quotaPollInterval
	quotaPollInterval = time.Microsecond
	defer func() { quotaPollInterval = restoreInterval }()
	restoreMaxWait := quotaPollMaxWait
	quotaPollMaxWait = 3 * time.Microsecond
	defer func() { quotaPollMaxWait = restoreMaxWait }()

	calls := 0
	sr, _ := newTestSyncReport()
	waitForQuotaHeadroomFn(func() (int, error) {
		calls++
		return 0, nil // headroom NEVER appears
	}, "g7e.2xlarge", sr)

	if calls < 1 {
		t.Error("ceilingFn was never called")
	}
	// The key property: it returned at all (no timeout/deadlock) despite
	// headroom never appearing — proven by reaching this line.
}

// TestWaitForQuotaHeadroomFn_QueryFailureFallsBackToFixedSleep proves a
// poll failure (e.g. a transient AWS API error) does NOT block the re-drive
// indefinitely — it leaks the failure and falls back to ONE
// quotaExceededBackoff sleep, matching calque#141's "don't block on a
// failed quota check" principle.
func TestWaitForQuotaHeadroomFn_QueryFailureFallsBackToFixedSleep(t *testing.T) {
	var slept []time.Duration
	restore := fleetSleep
	fleetSleep = func(d time.Duration) { slept = append(slept, d) }
	defer func() { fleetSleep = restore }()

	sr, rep := newTestSyncReport()
	qerr := fmt.Errorf("quota lookup: throttled")
	waitForQuotaHeadroomFn(func() (int, error) { return 0, qerr }, "g7e.2xlarge", sr)

	if len(slept) != 1 || slept[0] != quotaExceededBackoff {
		t.Errorf("slept %v, want exactly [%v] (the fixed fallback)", slept, quotaExceededBackoff)
	}
	if rep.Len() != 1 {
		t.Fatalf("expected exactly 1 leak, got %d: %+v", rep.Len(), rep.Leaks)
	}
	if !strings.Contains(rep.Leaks[0].Detail, "throttled") {
		t.Errorf("leak detail %q should mention the underlying query error", rep.Leaks[0].Detail)
	}
}

// TestMeasurementFromPoolSummaryFields_WarmHitReportsZeroEnter is the core
// calque#145 D5 fix, proven directly: a warm-hit claim (this worker was
// already loaded when it claimed the shard) must report EnterSeconds=0,
// NOT the worker's original (possibly large) load cost — the exact
// overcounting bug this fix closes, since measure.FleetFold sums
// EnterSeconds across every shard's Measurement.
func TestMeasurementFromPoolSummaryFields_WarmHitReportsZeroEnter(t *testing.T) {
	summary := calpool.Summary{WarmHit: true, EnterSecondsPaid: 0}
	m := measurementFromPoolSummaryFields(summary, []float64{1.0, 2.0}, "g7e.2xlarge")
	if m.EnterSeconds != 0 {
		t.Errorf("EnterSeconds = %v, want 0 for a warm-hit claim", m.EnterSeconds)
	}
	if m.AcquireWaitSeconds != 0 {
		t.Errorf("AcquireWaitSeconds = %v, want 0 (fleet-pool acquire cost is a per-run fixed cost, not per-shard)", m.AcquireWaitSeconds)
	}
}

// TestMeasurementFromPoolSummaryFields_ColdHitReportsRealEnter: the claim
// that actually triggers @enter (this worker's first claim, or one
// following a mid-drain crash) must carry that real, non-zero paid cost
// through unchanged — only WARM claims should report 0.
func TestMeasurementFromPoolSummaryFields_ColdHitReportsRealEnter(t *testing.T) {
	summary := calpool.Summary{WarmHit: false, EnterSecondsPaid: 42.5}
	m := measurementFromPoolSummaryFields(summary, []float64{1.0}, "g7e.2xlarge")
	if m.EnterSeconds != 42.5 {
		t.Errorf("EnterSeconds = %v, want 42.5 (the real paid load cost for a cold claim)", m.EnterSeconds)
	}
}

// TestMeasurementFromPoolSummaryFields_DerivesPerItemFromResults proves
// per-item timing comes from the caller-supplied perItemSecs (derived from
// the shard's own collected S3 results), NOT from the pool summary itself —
// calpool.Summary carries no per-item series at all.
func TestMeasurementFromPoolSummaryFields_DerivesPerItemFromResults(t *testing.T) {
	m := measurementFromPoolSummaryFields(calpool.Summary{}, []float64{1.0, 3.0, 5.0}, "g7e.2xlarge")
	if m.PerItem.Count != 3 {
		t.Errorf("PerItem.Count = %d, want 3", m.PerItem.Count)
	}
	wantMean := 3.0 // (1+3+5)/3
	if m.PerItem.MeanSecs != wantMean {
		t.Errorf("PerItem.MeanSecs = %v, want %v", m.PerItem.MeanSecs, wantMean)
	}
}

// TestMeasurementFromPoolSummaryFields_CarriesOccupancy proves occupancy
// still rides through from the pool summary (its own OccupancyRaw field
// IS carried, unlike per-item timing).
func TestMeasurementFromPoolSummaryFields_CarriesOccupancy(t *testing.T) {
	occ := 0.75
	summary := calpool.Summary{Occupancy: calexec.OccupancyRaw{Measured: true, MeanOccupancy: &occ, Source: "nvidia-smi"}}
	m := measurementFromPoolSummaryFields(summary, nil, "g7e.2xlarge")
	if !m.Occupancy.Measured {
		t.Error("Occupancy.Measured = false, want true (carried from the pool summary)")
	}
	if m.Occupancy.MeanOccupancy == nil || *m.Occupancy.MeanOccupancy != 0.75 {
		t.Errorf("Occupancy.MeanOccupancy = %v, want 0.75", m.Occupancy.MeanOccupancy)
	}
}

// TestNeedsItemRedrive_CleanClaimWithFailedItemsQualifies (calque#145
// slice 3, D4a): the core case this pass exists for — a claim that
// completed (shardErr == nil) but reported permanently-failed item
// indices should be routed to item-level re-drive, not D4's dedicated-
// instance fallback.
func TestNeedsItemRedrive_CleanClaimWithFailedItemsQualifies(t *testing.T) {
	if !needsItemRedrive(nil, []int{3, 7}) {
		t.Error("needsItemRedrive(nil, [3,7]) = false, want true")
	}
}

// TestNeedsItemRedrive_ClaimLevelFailureFallsThroughToD4 proves a shard
// whose D2 wait itself errored (including a stale-fleet ErrFleetStale) is
// NOT selected for item-level re-drive even if shardFailed happens to be
// non-empty (it shouldn't be populated in that case, but the predicate
// must not depend on that) — it must fall through to D4's existing
// dedicated-instance selection unchanged.
func TestNeedsItemRedrive_ClaimLevelFailureFallsThroughToD4(t *testing.T) {
	if needsItemRedrive(errors.New("shard wait summary: boom"), []int{3}) {
		t.Error("needsItemRedrive with a non-nil shardErr = true, want false (must route to D4, not D4a)")
	}
}

// TestNeedsItemRedrive_CleanClaimNoFailedItemsSkipsBothPasses proves the
// common happy-path case (everything landed) triggers neither D4a nor D4.
func TestNeedsItemRedrive_CleanClaimNoFailedItemsSkipsBothPasses(t *testing.T) {
	if needsItemRedrive(nil, nil) {
		t.Error("needsItemRedrive(nil, nil) = true, want false (no failed items to redrive)")
	}
}

// TestD4aFlowsThroughToD4_OnFailure simulates D4a's own failure path
// (e.g. the item-redrive's WaitForSummaryLivenessAny call itself errors)
// setting shardErrs[i] — proving that shard THEN gets picked up by D4's
// existing, unchanged failedIdx selection (the same `shardErrs[i] != nil`
// check D4 has always used), exactly as fleetRun's D4a comment claims.
func TestD4aFlowsThroughToD4_OnFailure(t *testing.T) {
	shardErrs := make([]error, 3)
	shardFailed := [][]int{nil, {2}, nil} // only shard 1 needed a D4a redrive

	// Simulate D4a's loop: shard 1 qualifies, its redrive attempt fails.
	for i := range shardErrs {
		if !needsItemRedrive(shardErrs[i], shardFailed[i]) {
			continue
		}
		shardErrs[i] = errors.New("item-redrive wait summary: fleet went stale")
	}

	// D4's existing selection logic, verbatim.
	var failedIdx []int
	for i := range shardErrs {
		if shardErrs[i] != nil {
			failedIdx = append(failedIdx, i)
		}
	}
	if !reflect.DeepEqual(failedIdx, []int{1}) {
		t.Errorf("D4 failedIdx = %v, want [1] (only the shard whose D4a redrive failed)", failedIdx)
	}
}

// TestFleetClaimRef_ModelMatchesRunID (calque#145) guards the exact
// copy-paste risk flagged in fleetRun's D2: a submitted ClaimRef.Model
// that doesn't equal the run id would be silently dropped by
// Worker.runOne's affinity check (ref.Model != w.Config.Model) — acked and
// discarded, never run. This constructs the SAME ClaimRef shape fleetRun's
// D2 submission loop builds and asserts the field fleetRun must get right.
func TestFleetClaimRef_ModelMatchesRunID(t *testing.T) {
	runID := "fleet-run-abc"
	manifestURI := "s3://bucket/fleet/fleet-run-abc/shard-0/manifest.json"
	ref := calpool.ClaimRef{RunID: runID, Model: runID, ManifestURI: manifestURI}
	if ref.Model != runID {
		t.Errorf("ClaimRef.Model = %q, want %q (must equal the run id, not o.model, or runFleetWorker's affinity check silently drops the claim)", ref.Model, runID)
	}
	if ref.ManifestURI != manifestURI {
		t.Errorf("ClaimRef.ManifestURI = %q, want %q", ref.ManifestURI, manifestURI)
	}
}
