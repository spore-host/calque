package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// memSink is an in-memory result sink for tests (S3 stands in here).
type memSink struct {
	mu      sync.Mutex
	results map[int]Result
}

func newMemSink() *memSink { return &memSink{results: map[int]Result{}} }

func (m *memSink) Put(_ context.Context, r Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[r.Index] = r
	return nil
}

// capturingLeaker records leaks so tests can assert rough edges were surfaced.
type capturingLeaker struct{ msgs []string }

func (c *capturingLeaker) Leak(kind, detail string) { c.msgs = append(c.msgs, kind+": "+detail) }

func python(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"python3", "python"} {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	t.Skip("no python interpreter on PATH")
	return ""
}

func runnerScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("runner.py")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func items(payloads ...any) []Item {
	out := make([]Item, len(payloads))
	for i, p := range payloads {
		out[i] = Item{Index: i, Payload: p}
	}
	return out
}

// TestWarmOnceAndOrdered proves the load-once invariant: @enter runs exactly once
// and state persists across items, and results land keyed by index.
func TestWarmOnceAndOrdered(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			// @enter sets up a call counter; each item increments it. If @enter ran
			// more than once the counter would reset — so the counter sequence is a
			// direct witness of warm-once.
			EnterBody:  `self.calls = 0`,
			MethodBody: "self.calls += 1\nreturn {'echo': payload, 'call': self.calls}",
			MethodArg:  "payload",
		},
	}
	its := items("a", "b", "c", "d")
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	if sup.EnterCount != 1 {
		t.Errorf("EnterCount = %d, want 1 (warm-once violated)", sup.EnterCount)
	}
	if len(sink.results) != 4 {
		t.Fatalf("results = %d, want 4", len(sink.results))
	}
	// Call counter must be 1,2,3,4 in index order — proves single warm state.
	for i := 0; i < 4; i++ {
		r, ok := sink.results[i]
		if !ok {
			t.Fatalf("missing result for index %d", i)
		}
		m := r.Result.(map[string]any)
		if int(m["call"].(float64)) != i+1 {
			t.Errorf("index %d: call=%v, want %d (state did not persist warm)", i, m["call"], i+1)
		}
	}
}

// TestCrashRestartReDrive is the riskiest behavior (§6): the runner dies mid-drain
// and the supervisor must restart it (reload @enter) and re-drive the unfinished
// items, with NO lost or duplicated results.
func TestCrashRestartReDrive(t *testing.T) {
	sink := newMemSink()
	leaks := &capturingLeaker{}
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Leak:   leaks,
		Config: Config{
			// The body hard-crashes the process the FIRST time it sees index 2, using
			// a marker file so the restarted runner gets past it. os._exit(1) bypasses
			// the runner's try/except — a true crash, not a structured error.
			EnterBody: "import os\nself.seen_crash = os.path.exists('/tmp/calque_crash_marker')",
			MethodBody: `import os
if payload == 2 and not self.seen_crash:
    open('/tmp/calque_crash_marker','w').close()
    os._exit(1)
return payload * 10`,
			MethodArg: "payload",
		},
	}
	// clean any stale marker
	_ = exec.Command("rm", "-f", "/tmp/calque_crash_marker").Run()
	defer exec.Command("rm", "-f", "/tmp/calque_crash_marker").Run()

	its := items(0, 1, 2, 3, 4)
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none (re-drive should complete all)", failed)
	}
	if len(sink.results) != 5 {
		t.Fatalf("results = %d, want 5 (lost items on re-drive)", len(sink.results))
	}
	got := make([]int, 0, 5)
	for i := 0; i < 5; i++ {
		r, ok := sink.results[i]
		if !ok {
			t.Fatalf("missing result for index %d after re-drive", i)
		}
		if int(r.Result.(float64)) != i*10 {
			t.Errorf("index %d: result=%v, want %d", i, r.Result, i*10)
		}
		got = append(got, i)
	}
	sort.Ints(got)
	// Must have restarted at least once (reloaded @enter) and logged the crash.
	if sup.EnterCount < 2 {
		t.Errorf("EnterCount = %d, want >= 2 (should have reloaded after crash)", sup.EnterCount)
	}
	if len(leaks.msgs) == 0 {
		t.Error("expected a leak recording the runner crash")
	}
}

// TestPartialFailureDoesNotReload proves a per-item payload error is a partial
// failure (reported), NOT a crash — the runner stays warm (@enter runs once).
func TestPartialFailureDoesNotReload(t *testing.T) {
	sink := newMemSink()
	leaks := &capturingLeaker{}
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Leak:   leaks,
		Config: Config{
			EnterBody:  `self.ok = True`,
			MethodBody: "if payload == 3:\n    raise ValueError('bad item 3')\nreturn payload",
			MethodArg:  "payload",
		},
	}
	its := items(0, 1, 2, 3, 4)
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 1 || failed[0] != 3 {
		t.Errorf("failed = %v, want [3]", failed)
	}
	if len(sink.results) != 4 {
		t.Errorf("results = %d, want 4 (all but the bad item)", len(sink.results))
	}
	if sup.EnterCount != 1 {
		t.Errorf("EnterCount = %d, want 1 (a bad ITEM must not reload the model)", sup.EnterCount)
	}
}
