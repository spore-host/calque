package exec

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// jsonl builds a sample stream the way occupancy.py writes it.
func jsonl(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

func sample(ts float64, dcgm float64) string {
	return fmt.Sprintf(`{"ts":%f,"nvsmi_util":0.10,"nvsmi_sm":0.20,"dcgm_sm":%f}`, ts, dcgm)
}

func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// TestExcludesLoadWindow is the #71 regression, stated as the economics: a run whose
// GPU is IDLE during a long model load and BUSY during a short inference burst must
// report the busy figure, not the average of the two. Before the fix, occupancy was
// the whole-run mean — so the batch-32 run measured 2% while running 24x faster than
// the serial run that measured 45%.
func TestExcludesLoadWindow(t *testing.T) {
	// t=100..109: model loading, GPU idle. t=110..113: inference, GPU ~90% full.
	var lines []string
	for ts := 100.0; ts < 110; ts++ {
		lines = append(lines, sample(ts, 0.0))
	}
	for ts := 110.0; ts < 114; ts++ {
		lines = append(lines, sample(ts, 0.90))
	}
	samples := ParseOccSamples(jsonl(lines...))
	if len(samples) != 14 {
		t.Fatalf("parsed %d samples, want 14", len(samples))
	}

	// The whole-run mean (what the sampler itself reports) is badly diluted...
	var sum float64
	for _, s := range samples {
		sum += *s.DcgmSM
	}
	wholeRun := sum / float64(len(samples))
	approx(t, wholeRun, (10*0.0+4*0.90)/14, "whole-run mean")

	// ...while the inference window reports the truth: the GPU was 90% full while working.
	occ, ok := OccupancyInWindows(samples, []Span{{StartUnix: 110, EndUnix: 113}}, 1.0)
	if !ok {
		t.Fatal("OccupancyInWindows: ok=false, want a windowed measurement")
	}
	approx(t, *occ.MeanOccupancy, 0.90, "inference-window occupancy")
	if occ.Scope != ScopeInference {
		t.Errorf("Scope = %q, want %q", occ.Scope, ScopeInference)
	}
	if occ.Samples != 4 {
		t.Errorf("Samples = %d, want 4 (only in-window ticks)", occ.Samples)
	}
	// The whole point: the fix must MOVE the number, and upward here.
	if *occ.MeanOccupancy <= wholeRun {
		t.Errorf("windowed occupancy %.3f did not exceed the load-contaminated %.3f", *occ.MeanOccupancy, wholeRun)
	}
}

// TestFasterInferenceDoesNotLowerOccupancy is the inverted-signal bug itself. Two
// runs, same GPU fill while working; one does its work 10x faster. Whole-run means
// disagree wildly (the faster run looks emptier — nonsense); inference-window means
// agree. Occupancy must describe GPU fill, not how the work compares to load time.
func TestFasterInferenceDoesNotLowerOccupancy(t *testing.T) {
	build := func(inferTicks int) ([]OccSample, []Span) {
		var lines []string
		for ts := 0.0; ts < 100; ts++ { // 100s of model load, idle
			lines = append(lines, sample(ts, 0.0))
		}
		for i := 0; i < inferTicks; i++ { // inference at 80% fill
			lines = append(lines, sample(100+float64(i), 0.80))
		}
		spans := []Span{{StartUnix: 100, EndUnix: 100 + float64(inferTicks) - 1}}
		return ParseOccSamples(jsonl(lines...)), spans
	}

	slowSamples, slowSpans := build(40) // 40s of inference
	fastSamples, fastSpans := build(4)  // same work, 10x faster

	slowOcc, ok1 := OccupancyInWindows(slowSamples, slowSpans, 1.0)
	fastOcc, ok2 := OccupancyInWindows(fastSamples, fastSpans, 1.0)
	if !ok1 || !ok2 {
		t.Fatalf("windowing failed: slow=%v fast=%v", ok1, ok2)
	}
	approx(t, *slowOcc.MeanOccupancy, 0.80, "slow inference occupancy")
	approx(t, *fastOcc.MeanOccupancy, 0.80, "fast inference occupancy")

	// Whole-run means would have reported 23% vs 3% for identical GPU fill — the
	// artifact that made the batch-32 run look idle.
	mean := func(ss []OccSample) float64 {
		var s float64
		for _, x := range ss {
			s += *x.DcgmSM
		}
		return s / float64(len(ss))
	}
	if mean(fastSamples) >= mean(slowSamples) {
		t.Fatal("test setup wrong: the whole-run artifact should punish the FASTER run")
	}
}

// TestCrashRestartLoadExcluded proves the multi-span design: a crash mid-run reloads
// the model, so the run is [load][infer][load][infer]. A single outer start->end
// window would swallow that second load and re-contaminate the number; the union of
// per-drain spans excludes both loads.
func TestCrashRestartLoadExcluded(t *testing.T) {
	var lines []string
	add := func(from, to float64, v float64) {
		for ts := from; ts < to; ts++ {
			lines = append(lines, sample(ts, v))
		}
	}
	add(0, 10, 0.0)   // load #1 (idle)
	add(10, 15, 1.00) // inference #1 (full)
	add(15, 25, 0.0)  // load #2 after crash (idle)
	add(25, 30, 1.00) // inference #2 (full)
	samples := ParseOccSamples(jsonl(lines...))
	spans := []Span{{StartUnix: 10, EndUnix: 14}, {StartUnix: 25, EndUnix: 29}}

	occ, ok := OccupancyInWindows(samples, spans, 1.0)
	if !ok {
		t.Fatal("ok=false")
	}
	approx(t, *occ.MeanOccupancy, 1.00, "occupancy across two inference spans")
	if occ.Samples != 10 {
		t.Errorf("Samples = %d, want 10 (5 per span, no load ticks)", occ.Samples)
	}
	// A naive single window 10..29 would include the 10s reload and report ~50%.
	naive, _ := OccupancyInWindows(samples, []Span{{StartUnix: 10, EndUnix: 29}}, 1.0)
	if math.Abs(*naive.MeanOccupancy-1.00) < 1e-6 {
		t.Error("expected the naive single-window mean to be contaminated by the reload")
	}
}

// TestEmptyWindowIsUnmeasuredNotZero is the honesty case: if no sample lands in the
// window we must report "couldn't measure", never 0%. A fabricated 0% would flow
// into K as "GPU completely idle" and flip the verdict on a lie.
func TestEmptyWindowIsUnmeasuredNotZero(t *testing.T) {
	samples := ParseOccSamples(jsonl(sample(100, 0.9), sample(101, 0.9)))
	// Inference happened far from any sample (sampler died early, say).
	occ, ok := OccupancyInWindows(samples, []Span{{StartUnix: 500, EndUnix: 600}}, 1.0)
	if ok {
		t.Fatalf("ok=true with no in-window samples; got %v", occ)
	}
	if occ.MeanOccupancy != nil || occ.Measured {
		t.Errorf("expected a zero-value unmeasured result, got %+v", occ)
	}
}

// TestNilMetricNotCountedAsIdle proves a missing reading is skipped, not coerced to
// 0. A collector that times out must not be recorded as an idle GPU — that would
// silently drag occupancy down and understate AWS's case.
func TestNilMetricNotCountedAsIdle(t *testing.T) {
	stream := jsonl(
		`{"ts":10,"nvsmi_util":0.5,"nvsmi_sm":0.5,"dcgm_sm":1.0}`,
		`{"ts":11,"nvsmi_util":null,"nvsmi_sm":null,"dcgm_sm":null}`, // dcgmi missed this tick
		`{"ts":12,"nvsmi_util":0.5,"nvsmi_sm":0.5,"dcgm_sm":1.0}`,
	)
	occ, ok := OccupancyInWindows(ParseOccSamples(stream), []Span{{StartUnix: 10, EndUnix: 12}}, 1.0)
	if !ok {
		t.Fatal("ok=false")
	}
	approx(t, *occ.MeanOccupancy, 1.0, "occupancy ignoring the missed tick")
	if occ.Samples != 2 {
		t.Errorf("Samples = %d, want 2 (the nil tick is not a sample)", occ.Samples)
	}
}

// TestPrimaryMetricPreferenceUnchanged proves scoping changed the WINDOW only, not
// which metric K stands on: DCGM SM-activity still wins over nvidia-smi (§8), so
// before/after numbers are comparable.
func TestPrimaryMetricPreferenceUnchanged(t *testing.T) {
	occ, ok := OccupancyInWindows(ParseOccSamples(jsonl(sample(1, 0.77))), []Span{{StartUnix: 0, EndUnix: 5}}, 1.0)
	if !ok {
		t.Fatal("ok=false")
	}
	if occ.OccupancySource != "dcgm_sm" {
		t.Errorf("OccupancySource = %q, want dcgm_sm (DCGM must stay primary)", occ.OccupancySource)
	}
	approx(t, *occ.MeanOccupancy, 0.77, "primary occupancy")

	// With no DCGM, fall back to nvidia-smi dmon sm%, then utilization.gpu.
	noDcgm := jsonl(`{"ts":1,"nvsmi_util":0.30,"nvsmi_sm":0.44,"dcgm_sm":null}`)
	occ2, _ := OccupancyInWindows(ParseOccSamples(noDcgm), []Span{{StartUnix: 0, EndUnix: 5}}, 1.0)
	if occ2.OccupancySource != "nvsmi_sm" {
		t.Errorf("fallback source = %q, want nvsmi_sm", occ2.OccupancySource)
	}
	approx(t, *occ2.MeanOccupancy, 0.44, "nvsmi_sm fallback")
}

// TestTornAndUntimestampedLinesSkipped proves parsing is robust to a SIGTERM'd
// sampler (truncated final line) and to a pre-#71 sampler emitting no `ts`. Neither
// may cost us the run's measurement or slip in an unattributable sample.
func TestTornAndUntimestampedLinesSkipped(t *testing.T) {
	stream := []byte(
		sample(10, 0.9) + "\n" +
			`{"nvsmi_util":0.5,"dcgm_sm":0.5}` + "\n" + // old sampler: no ts
			"\n" +
			sample(11, 0.9) + "\n" +
			`{"ts":12,"dcgm_s`) // torn write, no trailing newline
	samples := ParseOccSamples(stream)
	if len(samples) != 2 {
		t.Fatalf("parsed %d samples, want 2 (torn + untimestamped skipped)", len(samples))
	}
	occ, ok := OccupancyInWindows(samples, []Span{{StartUnix: 10, EndUnix: 11}}, 1.0)
	if !ok {
		t.Fatal("ok=false")
	}
	approx(t, *occ.MeanOccupancy, 0.9, "occupancy from the intact lines")
}

// TestSpansSecondsMergesOverlap proves overlapping spans don't double-count time.
func TestSpansSecondsMergesOverlap(t *testing.T) {
	approx(t, SpansSeconds([]Span{{10, 20}, {30, 35}}), 15, "disjoint spans")
	approx(t, SpansSeconds([]Span{{10, 20}, {15, 25}}), 15, "overlapping spans merged")
	approx(t, SpansSeconds([]Span{{10, 20}, {12, 14}}), 10, "contained span merged")
	approx(t, SpansSeconds(nil), 0, "no spans")
}

// TestOccScopeNoteLabelsUnlabeledAsWholeRun proves an unlabeled (pre-#71) summary
// reads as whole-run — the conservative default. Silently presenting it as an
// inference-window number would overstate GPU fill.
func TestOccScopeNoteLabelsUnlabeledAsWholeRun(t *testing.T) {
	if got := OccScopeNote(OccupancyRaw{}); !strings.Contains(got, "WHOLE-RUN") {
		t.Errorf("unlabeled scope note = %q, want it flagged WHOLE-RUN", got)
	}
	if got := (OccupancyRaw{}).ScopeOrWholeRun(); got != ScopeWholeRun {
		t.Errorf("ScopeOrWholeRun() = %q, want %q", got, ScopeWholeRun)
	}
	note := OccScopeNote(OccupancyRaw{Scope: ScopeInference, Samples: 7})
	if !strings.Contains(note, "excludes") {
		t.Errorf("inference note = %q, want it to say the load is excluded", note)
	}
}
