package warm

import (
	"context"
	"strings"
	"testing"
)

// TestBatchAllResultsCollected proves micro-batching: the body is called once per
// batch with a LIST, returns a list, and every item lands in the sink keyed by its
// own index. Batch size need not divide the item count (last batch is short).
func TestBatchAllResultsCollected(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python:    python(t),
		Script:    runnerScript(t),
		Sink:      sink,
		BatchSize: 4,
		Config: Config{
			// Batch-shaped body: `prompts` is the LIST of payloads; return a LIST.
			EnterBody:  `self.n = 0`,
			MethodBody: "self.n += 1\nreturn [{'echo': p, 'batch': self.n} for p in prompts]",
			MethodArg:  "prompts",
		},
	}
	n := 10 // 4 + 4 + 2
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
		t.Errorf("EnterCount = %d, want 1", sup.EnterCount)
	}
	if len(sink.results) != n {
		t.Fatalf("results = %d, want %d", len(sink.results), n)
	}
	for i := 0; i < n; i++ {
		r, ok := sink.results[i]
		if !ok {
			t.Fatalf("missing result for index %d", i)
		}
		if int(r.Result.(map[string]any)["echo"].(float64)) != i {
			t.Errorf("index %d: echo mismatch %v", i, r.Result)
		}
	}
	// 10 items / batch 4 => 3 batches => self.n reaches 3 (one increment per batch).
	last := sink.results[n-1].Result.(map[string]any)
	if int(last["batch"].(float64)) != 3 {
		t.Errorf("batch counter = %v, want 3 (proves ONE call per batch, not per item)", last["batch"])
	}
}

// TestBatchWholeBatchFailure proves a body exception fails every item in that
// batch as a partial failure (not a runner crash), while other batches succeed.
func TestBatchWholeBatchFailure(t *testing.T) {
	sink := newMemSink()
	leaks := &capturingLeaker{}
	sup := &Supervisor{
		Python:    python(t),
		Script:    runnerScript(t),
		Sink:      sink,
		Leak:      leaks,
		BatchSize: 3,
		Config: Config{
			EnterBody: `self.ok = True`,
			// The batch containing payload 4 raises; others return normally.
			MethodBody: "if 4 in prompts:\n    raise ValueError('bad batch')\nreturn [p * 10 for p in prompts]",
			MethodArg:  "prompts",
		},
	}
	n := 9 // batches: [0,1,2] [3,4,5] [6,7,8]; middle batch fails
	its := make([]Item, n)
	for i := range its {
		its[i] = Item{Index: i, Payload: i}
	}
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sup.EnterCount != 1 {
		t.Errorf("EnterCount = %d, want 1 (a bad batch must not reload)", sup.EnterCount)
	}
	// Indices 3,4,5 fail; the other 6 succeed.
	if len(failed) != 3 {
		t.Fatalf("failed = %v, want the 3 items of the bad batch", failed)
	}
	for _, idx := range failed {
		if idx < 3 || idx > 5 {
			t.Errorf("unexpected failed index %d (want 3,4,5)", idx)
		}
	}
	if len(sink.results) != 6 {
		t.Errorf("results = %d, want 6", len(sink.results))
	}
}

// TestBatchWrongReturnCount proves a body that returns the wrong number of results
// (not aligned to inputs) fails the batch's items with a clear error rather than
// silently mis-keying results.
func TestBatchWrongReturnCount(t *testing.T) {
	sink := newMemSink()
	leaks := &capturingLeaker{}
	sup := &Supervisor{
		Python:    python(t),
		Script:    runnerScript(t),
		Sink:      sink,
		Leak:      leaks,
		BatchSize: 4,
		Config: Config{
			EnterBody:  `pass`,
			MethodBody: "return [1]  # wrong: one result for a batch of many",
			MethodArg:  "prompts",
		},
	}
	its := make([]Item, 4)
	for i := range its {
		its[i] = Item{Index: i, Payload: i}
	}
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 4 {
		t.Fatalf("failed = %v, want all 4 (misaligned return)", failed)
	}
	if len(sink.results) != 0 {
		t.Errorf("results = %d, want 0 (nothing should be mis-keyed)", len(sink.results))
	}
	joined := strings.Join(leaks.msgs, " ")
	if !strings.Contains(joined, "aligned") {
		t.Errorf("expected a leak explaining the misaligned batch return; got %v", leaks.msgs)
	}
}

// TestConcurrencyGuardVLLMFallsBackToSerial proves the guard (#68): a vLLM-offline
// @method body under Concurrency>1 falls back to SERIAL with a leak, instead of
// hanging on vLLM's non-thread-safe engine. The run still completes correctly.
func TestConcurrencyGuardVLLMFallsBackToSerial(t *testing.T) {
	sink := newMemSink()
	leaks := &capturingLeaker{}
	sup := &Supervisor{
		Python:      python(t),
		Script:      runnerScript(t),
		Sink:        sink,
		Leak:        leaks,
		Concurrency: 8,
		Config: Config{
			// Looks like vLLM offline: self.llm.generate(...). No real vLLM here — a
			// stand-in that would be UNSAFE to run in threads if we didn't guard.
			EnterBody:  "self.llm = object()",
			MethodBody: "return self.llm.generate([prompt])  # vLLM-offline shape",
			MethodArg:  "prompt",
		},
	}
	// The stand-in generate() would AttributeError; that's fine — we're asserting the
	// guard flipped to serial and left a leak, not the body's success.
	_, _ = sup.Run(context.Background(), items(0, 1, 2))
	if sup.Concurrency != 1 {
		t.Errorf("guard did not fall back to serial: Concurrency = %d", sup.Concurrency)
	}
	joined := strings.Join(leaks.msgs, " ")
	if !strings.Contains(joined, "thread-safe") && !strings.Contains(joined, "SERIAL") {
		t.Errorf("expected a leak about the vLLM concurrency fallback; got %v", leaks.msgs)
	}
}
