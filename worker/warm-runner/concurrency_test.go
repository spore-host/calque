package warm

import (
	"context"
	"os/exec"
	"testing"
)

// TestConcurrentAllResultsCollected proves that with Concurrency>1 every item is
// still settled exactly once and lands in the sink keyed by index, even though the
// runner emits results OUT OF ORDER. The method sleeps a payload-dependent amount
// so completion order deliberately differs from send order.
func TestConcurrentAllResultsCollected(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python:      python(t),
		Script:      runnerScript(t),
		Sink:        sink,
		Concurrency: 4,
		Config: Config{
			// No shared-state mutation (concurrency-safe by construction); sleep so the
			// finish order is scrambled relative to index.
			EnterBody:  `import time as _t`,
			MethodBody: "import time\ntime.sleep((7 - (payload % 7)) * 0.02)\nreturn payload * 2",
			MethodArg:  "payload",
		},
	}
	n := 20
	its := make([]Item, n)
	for i := range its {
		its[i] = Item{Index: i, Payload: i}
	}
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	if sup.EnterCount != 1 {
		t.Errorf("EnterCount = %d, want 1 (warm-once must hold under concurrency)", sup.EnterCount)
	}
	if len(sink.results) != n {
		t.Fatalf("results = %d, want %d", len(sink.results), n)
	}
	for i := 0; i < n; i++ {
		r, ok := sink.results[i]
		if !ok {
			t.Fatalf("missing result for index %d", i)
		}
		if int(r.Result.(float64)) != i*2 {
			t.Errorf("index %d: result=%v, want %d (index/result mismatch under out-of-order)", i, r.Result, i*2)
		}
	}
}

// TestConcurrentPartialFailure proves that under concurrency a per-item error is a
// partial failure (reported, runner stays warm) and does NOT sink a result — while
// every other item still completes.
func TestConcurrentPartialFailure(t *testing.T) {
	sink := newMemSink()
	leaks := &capturingLeaker{}
	sup := &Supervisor{
		Python:      python(t),
		Script:      runnerScript(t),
		Sink:        sink,
		Leak:        leaks,
		Concurrency: 4,
		Config: Config{
			EnterBody:  `self.ok = True`,
			MethodBody: "if payload in (3, 7):\n    raise ValueError('bad item')\nreturn payload",
			MethodArg:  "payload",
		},
	}
	n := 10
	its := make([]Item, n)
	for i := range its {
		its[i] = Item{Index: i, Payload: i}
	}
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sup.EnterCount != 1 {
		t.Errorf("EnterCount = %d, want 1 (a bad item must not reload under concurrency)", sup.EnterCount)
	}
	if len(failed) != 2 {
		t.Fatalf("failed = %v, want exactly [3 7]", failed)
	}
	for _, idx := range failed {
		if idx != 3 && idx != 7 {
			t.Errorf("unexpected failed index %d", idx)
		}
		if _, ok := sink.results[idx]; ok {
			t.Errorf("failed index %d should not have a sink result", idx)
		}
	}
	// The 8 good items must all be present.
	if len(sink.results) != n-2 {
		t.Errorf("results = %d, want %d", len(sink.results), n-2)
	}
}

// TestConcurrentCrashReDrive proves that a crash with MULTIPLE items in flight
// re-drives ALL unsettled items on a fresh runner with no loss or duplication —
// the case the old serial re-drive (which assumed <=1 in flight) never had to face.
func TestConcurrentCrashReDrive(t *testing.T) {
	sink := newMemSink()
	leaks := &capturingLeaker{}
	sup := &Supervisor{
		Python:      python(t),
		Script:      runnerScript(t),
		Sink:        sink,
		Leak:        leaks,
		Concurrency: 4,
		Config: Config{
			// Crash the whole process the first time index 5 runs (hard os._exit, not a
			// catchable error), so several concurrently in-flight items die with it.
			EnterBody: "import os\nself.seen = os.path.exists('/tmp/calque_conc_crash')",
			MethodBody: `import os, time
if payload == 5 and not self.seen:
    open('/tmp/calque_conc_crash','w').close()
    os._exit(1)
time.sleep(0.01)
return payload * 10`,
			MethodArg: "payload",
		},
	}
	_ = exec.Command("rm", "-f", "/tmp/calque_conc_crash").Run()
	defer exec.Command("rm", "-f", "/tmp/calque_conc_crash").Run()

	n := 12
	its := make([]Item, n)
	for i := range its {
		its[i] = Item{Index: i, Payload: i}
	}
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none (re-drive must complete all)", failed)
	}
	if len(sink.results) != n {
		t.Fatalf("results = %d, want %d (lost/dup on concurrent re-drive)", len(sink.results), n)
	}
	for i := 0; i < n; i++ {
		r, ok := sink.results[i]
		if !ok {
			t.Fatalf("missing result for index %d after re-drive", i)
		}
		if int(r.Result.(float64)) != i*10 {
			t.Errorf("index %d: result=%v, want %d", i, r.Result, i*10)
		}
	}
	if sup.EnterCount < 2 {
		t.Errorf("EnterCount = %d, want >= 2 (should reload after crash)", sup.EnterCount)
	}
}
