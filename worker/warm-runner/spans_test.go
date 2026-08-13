package warm

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestInferenceSpanExcludesEnterLoad is the supervisor half of #71: the recorded
// span must start AFTER @enter returns, so the model-load window (during which the
// GPU is idle by definition) is outside it. @enter here sleeps a distinctive amount;
// the span must not contain that time.
func TestInferenceSpanExcludesEnterLoad(t *testing.T) {
	const loadSecs = 0.6
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			// A slow @enter stands in for a ~200s real model load.
			EnterBody:  "import time\ntime.sleep(0.6)\nself.ok = True",
			MethodBody: "return payload",
			MethodArg:  "payload",
		},
	}
	t0 := nowUnix()
	if _, err := sup.Run(context.Background(), items(1, 2, 3)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sup.InferenceSpans) != 1 {
		t.Fatalf("InferenceSpans = %d, want 1 (one clean drain)", len(sup.InferenceSpans))
	}
	sp := sup.InferenceSpans[0]
	if sp.StartUnix < t0+loadSecs {
		t.Errorf("span starts %.3fs after Run began, but @enter alone took %.1fs — the load is INSIDE the window",
			sp.StartUnix-t0, loadSecs)
	}
	if sp.EndUnix < sp.StartUnix {
		t.Errorf("span ends before it starts: %+v", sp)
	}
	// Three trivial items: the span must be short — nowhere near the load time.
	if d := sp.EndUnix - sp.StartUnix; d > loadSecs {
		t.Errorf("span duration %.3fs exceeds the @enter load %.1fs; load leaked into the window", d, loadSecs)
	}
}

// TestInferenceSpansPerDrainOnCrash proves a crash-restart yields MULTIPLE spans,
// with the reload gap falling between them. A single outer window would swallow that
// second model load and re-contaminate occupancy — the exact bug being fixed.
func TestInferenceSpansPerDrainOnCrash(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Leak:   &capturingLeaker{},
		Config: Config{
			// Reload takes a beat, so the inter-span gap is measurable.
			EnterBody: "import os, time\ntime.sleep(0.3)\nself.seen = os.path.exists('/tmp/calque_span_marker')",
			MethodBody: `import os
if payload == 2 and not self.seen:
    open('/tmp/calque_span_marker','w').close()
    os._exit(1)
return payload * 10`,
			MethodArg: "payload",
		},
	}
	_ = removeFile("/tmp/calque_span_marker")
	defer func() { _ = removeFile("/tmp/calque_span_marker") }()

	failed, err := sup.Run(context.Background(), items(0, 1, 2, 3))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	if sup.EnterCount < 2 {
		t.Fatalf("EnterCount = %d, want >= 2 (no crash happened; test is not exercising the path)", sup.EnterCount)
	}
	if len(sup.InferenceSpans) < 2 {
		t.Fatalf("InferenceSpans = %d, want >= 2 (one per drain)", len(sup.InferenceSpans))
	}
	// The reload happened BETWEEN spans: span[0] must end before span[1] begins, with
	// a gap at least as large as the reload sleep.
	gap := sup.InferenceSpans[1].StartUnix - sup.InferenceSpans[0].EndUnix
	if gap < 0.25 {
		t.Errorf("gap between spans = %.3fs, want >= ~0.3s (the reload must fall OUTSIDE both spans)", gap)
	}
}

// TestInferenceSpanBatched proves the batch path records a span too (it's the path
// whose occupancy was most badly misread — batch-32 reported 2%).
func TestInferenceSpanBatched(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python:    python(t),
		Script:    runnerScript(t),
		Sink:      sink,
		BatchSize: 4,
		Config: Config{
			EnterBody:  "import time\ntime.sleep(0.4)\nself.ok = True",
			MethodBody: "return [p for p in prompts]",
			MethodArg:  "prompts",
		},
	}
	its := make([]Item, 8)
	for i := range its {
		its[i] = Item{Index: i, Payload: i}
	}
	t0 := nowUnix()
	if _, err := sup.Run(context.Background(), its); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sup.InferenceSpans) != 1 {
		t.Fatalf("InferenceSpans = %d, want 1", len(sup.InferenceSpans))
	}
	if sup.InferenceSpans[0].StartUnix < t0+0.4 {
		t.Error("batched span includes the @enter load")
	}
}

// TestNowUnixIsWallClockSeconds guards the time basis: spans are correlated against
// a SEPARATE process's `time.time()` samples, so this must be epoch seconds — not
// monotonic, not nanoseconds.
func TestNowUnixIsWallClockSeconds(t *testing.T) {
	got := nowUnix()
	want := float64(time.Now().Unix())
	if got < want-5 || got > want+5 {
		t.Errorf("nowUnix() = %v, want ~%v (epoch SECONDS, shared basis with occupancy.py)", got, want)
	}
}

// removeFile deletes a path, ignoring absence (test helper).
func removeFile(p string) error { return os.Remove(p) }
