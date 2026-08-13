package warm

import (
	"bytes"
	"context"
	"encoding/base64"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// Tests use the package's exported MemSink (same type the dry-run uses), so the
// dry-run path and the tests exercise identical sink behavior.
func newMemSink() *MemSink { return NewMemSink() }

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

// TestStdoutPollutionDoesNotCorruptProtocol is a regression for a real bug found
// on a live g6 GPU run: vLLM prints INFO logs to stdout ("INFO ... Initializing
// an LLM engine"), which corrupted the newline-JSON protocol frame — warmd saw
// 'I', failed to decode, and restart-looped forever without ever loading the
// model. runner.py now redirects library stdout to stderr; the protocol channel
// is a private dup of the original fd. Bodies that print to stdout must NOT break
// framing.
func TestStdoutPollutionDoesNotCorruptProtocol(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			// @enter and @method both spew to stdout the way vLLM/transformers do.
			EnterBody:  "import sys\nprint('INFO 07-16 loading model shards...')\nsys.stdout.write('WARNING raw write to stdout\\n')\nself.n = 0",
			MethodBody: "print('INFO generating for', payload)\nself.n += 1\nreturn {'ok': payload, 'n': self.n}",
			MethodArg:  "payload",
		},
	}
	failed, err := sup.Run(context.Background(), items("x", "y"))
	if err != nil {
		t.Fatalf("Run: %v (protocol corrupted by stdout prints?)", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	if sup.EnterCount != 1 {
		t.Errorf("EnterCount = %d, want 1 (stdout pollution caused a restart loop?)", sup.EnterCount)
	}
	if len(sink.results) != 2 {
		t.Errorf("results = %d, want 2 (framing broke on library stdout)", len(sink.results))
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
	defer func() { _ = exec.Command("rm", "-f", "/tmp/calque_crash_marker").Run() }()

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

// TestDrainBatchStaysWarmAcrossCalls proves the sticky-pool invariant (calque#100):
// calling DrainBatch more than once WITHOUT Close in between reuses the SAME
// resident runner — @enter runs exactly once across both calls, and state set by
// the first batch is still visible to the second. This is what lets a pool worker
// serve many claims for one model without re-paying the cold-load cost per claim.
func TestDrainBatchStaysWarmAcrossCalls(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			EnterBody:  `self.calls = 0`,
			MethodBody: "self.calls += 1\nreturn {'echo': payload, 'call': self.calls}",
			MethodArg:  "payload",
		},
	}
	defer sup.Close()

	failed1, err := sup.DrainBatch(context.Background(), items("a", "b"))
	if err != nil {
		t.Fatalf("first DrainBatch: %v", err)
	}
	if len(failed1) != 0 {
		t.Fatalf("first batch failed = %v, want none", failed1)
	}
	if sup.EnterCount != 1 {
		t.Fatalf("EnterCount after first batch = %d, want 1", sup.EnterCount)
	}

	// Second batch reuses indices 0/1 again (a pool worker's next claim is its own
	// independent item set — index scoping is per-claim, not global). What matters
	// is that EnterCount does NOT increase: the same warm runner served it.
	failed2, err := sup.DrainBatch(context.Background(), items("c", "d"))
	if err != nil {
		t.Fatalf("second DrainBatch: %v", err)
	}
	if len(failed2) != 0 {
		t.Fatalf("second batch failed = %v, want none", failed2)
	}
	if sup.EnterCount != 1 {
		t.Errorf("EnterCount after second batch = %d, want 1 (runner should have stayed resident)", sup.EnterCount)
	}
	// The call counter set by @enter and incremented by @method must have kept
	// counting across the two DrainBatch calls (3, 4), not reset (1, 2) — direct
	// witness that the SAME process (and its state) served both batches.
	r0, ok := sink.results[0]
	if !ok {
		t.Fatal("missing result for second-batch index 0")
	}
	call := int(r0.Result.(map[string]any)["call"].(float64))
	if call != 3 {
		t.Errorf("second batch's first item call=%d, want 3 (state did not persist across DrainBatch calls)", call)
	}
}

// TestRunClosesRunnerAfterOneBatch proves Run's original contract is unchanged by
// the DrainBatch/Warm/Close refactor: after Run returns, no runner is resident
// (Close was called internally) — a second Run call must warm a FRESH runner
// (EnterCount resets to 1 again on a NEW Supervisor value, proving Run doesn't leak
// a resident process across independent calls the way pool mode's DrainBatch does).
func TestRunClosesRunnerAfterOneBatch(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			EnterBody:  `self.calls = 0`,
			MethodBody: "self.calls += 1\nreturn {'echo': payload, 'call': self.calls}",
			MethodArg:  "payload",
		},
	}
	if _, err := sup.Run(context.Background(), items("a")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sup.active != nil {
		t.Error("Run left a resident runner active; want nil (Close should have run)")
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

// TestExtrasShippedAndCallable (calque#92): a .local()-referenced sibling
// function shipped via Config.Extras must be callable from @method BOTH as a
// bare call (helper(x), how real Modal code sometimes writes it) and as
// helper.local(x) (the explicit same-process form) — real scripts use both
// styles, and calque's bodies-are-payload rule means neither call site's
// verbatim text is rewritten. A second extra (helper_b) calls the first
// (helper_a), proving extras share self.globals and can call each other.
func TestExtrasShippedAndCallable(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			EnterBody:  `self.ok = True`,
			MethodBody: "bare = helper_a(payload)\nviaLocal = helper_b.local(payload)\nreturn {'bare': bare, 'via_local': viaLocal}",
			MethodArg:  "payload",
			Extras: []ExtraFunc{
				{Name: "helper_a", Args: []string{"x"}, Body: "return x + 1"},
				{Name: "helper_b", Args: []string{"y"}, Body: "return helper_a(y) * 2"},
			},
		},
	}
	failed, err := sup.Run(context.Background(), items(10))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	r, ok := sink.results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	m := r.Result.(map[string]any)
	if int(m["bare"].(float64)) != 11 {
		t.Errorf("bare helper_a(10) = %v, want 11", m["bare"])
	}
	if int(m["via_local"].(float64)) != 22 {
		t.Errorf("helper_b.local(10) (which itself calls helper_a) = %v, want 22", m["via_local"])
	}
}

// TestResultContainingRawBytesRoundTripsAsBase64 is the regression test for
// a real bug found running AI-Almanac's app.py::run_benchmark_local through
// this exact Supervisor+runner.py pair (not a synthetic body): its return
// value nests raw bytes anywhere in an arbitrary structure (files: [{data:
// path.read_bytes(), ...}, ...]) — plain json.dumps in runner.py's _emit()
// refused with "Object of type bytes is not JSON serializable", failing
// every item whose real Modal body returns bytes not at the top level (the
// INPUT side already had this exact problem solved via
// PayloadIsBase64Bytes/Base64ArgIndices; the OUTPUT side had no equivalent
// at all). The fix base64-encodes any bytes value via json.dumps' default=
// hook, so ANY nesting depth is covered, not just a known top-level shape.
func TestResultContainingRawBytesRoundTripsAsBase64(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			EnterBody:  `self.ok = True`,
			MethodBody: `return {"files": [{"filename": "out.bin", "data": bytes([0, 1, 255, 104, 105])}], "ok": True}`,
			MethodArg:  "payload",
		},
	}
	failed, err := sup.Run(context.Background(), items(1))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none (a real body returning nested bytes must not fail the item)", failed)
	}
	r, ok := sink.results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	m, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("Result = %+v (%T), want a map", r.Result, r.Result)
	}
	files, ok := m["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("files = %+v, want a 1-element list", m["files"])
	}
	file, ok := files[0].(map[string]any)
	if !ok {
		t.Fatalf("files[0] = %+v, want a map", files[0])
	}
	dataStr, ok := file["data"].(string)
	if !ok {
		t.Fatalf("data = %+v (%T), want a base64 STRING (JSON has no native bytes type)", file["data"], file["data"])
	}
	decoded, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		t.Fatalf("data %q is not valid base64: %v", dataStr, err)
	}
	want := []byte{0, 1, 255, 104, 105}
	if !bytes.Equal(decoded, want) {
		t.Errorf("decoded data = %v, want %v", decoded, want)
	}
}

// TestStarmapSplatsRealTuples (calque#93): a .starmap()'d unit's method takes
// TWO positional params (a, b); each item's payload is a real 2-element
// tuple, e.g. [3, 4]. Config.Starmap=true + MethodArgs=["a","b"] must compile
// __calque_method__(self, a, b) and call it as fn(self.state, *payload) —
// proven by asserting the actual SUM comes out correct for every item, not
// just that nothing crashed (a bug that bound only `a` and left `b` unbound
// would NameError, but a bug that silently bound the wrong values could still
// "succeed" with a wrong sum — this test would catch both).
func TestStarmapSplatsRealTuples(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			EnterBody:  `self.ok = True`,
			MethodBody: "return a + b",
			MethodArg:  "a",
			MethodArgs: []string{"a", "b"},
			Starmap:    true,
		},
	}
	its := items([]any{1, 2}, []any{3, 4}, []any{10, 20})
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	if len(sink.results) != 3 {
		t.Fatalf("results = %d, want 3", len(sink.results))
	}
	wantSums := []float64{3, 7, 30}
	for i, want := range wantSums {
		r, ok := sink.results[i]
		if !ok {
			t.Fatalf("missing result for index %d", i)
		}
		got, ok := r.Result.(float64)
		if !ok || got != want {
			t.Errorf("index %d: sum=%v, want %v (tuple-splat bound the wrong values, or didn't splat at all)", i, r.Result, want)
		}
	}
}

// TestBase64ArgIndicesDecodesOnlyMarkedTuplePositions (calque real
// --arg-file/--arg-json) proves a Starmap payload tuple mixing a raw-bytes
// position with plain literal positions round-trips correctly: Go's
// encoding/json auto-base64-encodes the []byte element when the item is
// sent to the runner over the JSON wire protocol, and Base64ArgIndices
// tells the runner exactly which tuple position to decode back to bytes
// before splatting — every other position passes through unchanged. This
// is the mechanism run_benchmark_local's real signature (job_id: str,
// config: dict, input_bundle: bytes) needs, since payload_is_base64_bytes
// alone only covers a WHOLE payload being bytes, not one position of a
// multi-arg tuple.
func TestBase64ArgIndicesDecodesOnlyMarkedTuplePositions(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			EnterBody:        `self.ok = True`,
			MethodBody:       "return [job_id, len(bundle), bundle[:2].hex()]",
			MethodArg:        "job_id",
			MethodArgs:       []string{"job_id", "bundle"},
			Starmap:          true,
			Base64ArgIndices: []int{1},
		},
	}
	rawBytes := []byte{0x00, 0x01, 0xff, 'h', 'i'}
	its := items([]any{"job-42", rawBytes})
	failed, err := sup.Run(context.Background(), its)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	r, ok := sink.results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	got, ok := r.Result.([]any)
	if !ok || len(got) != 3 {
		t.Fatalf("Result = %+v (%T), want a 3-element list", r.Result, r.Result)
	}
	if got[0] != "job-42" {
		t.Errorf("job_id = %v, want %q (non-base64 position must pass through unchanged)", got[0], "job-42")
	}
	if n, ok := got[1].(float64); !ok || int(n) != len(rawBytes) {
		t.Errorf("len(bundle) = %v, want %d (base64 position must decode back to the real bytes, not stay a string)", got[1], len(rawBytes))
	}
	if got[2] != "0001" {
		t.Errorf("bundle[:2].hex() = %v, want %q (confirms real byte VALUES, not just length, survived the decode)", got[2], "0001")
	}
}

// TestNonStarmapUnaffectedBySplatFields is a regression guard (calque#93): a
// unit that leaves Starmap unset (the default, every existing map/for_each/
// remote caller) must keep binding a single positional value even when
// MethodArgs happens to be populated — Starmap, not MethodArgs' mere
// presence, is what gates the splat path. This is the exact shape a caller
// would produce if it always threaded the full arg list but only some units
// are actually starmap'd.
func TestNonStarmapUnaffectedBySplatFields(t *testing.T) {
	sink := newMemSink()
	sup := &Supervisor{
		Python: python(t),
		Script: runnerScript(t),
		Sink:   sink,
		Config: Config{
			EnterBody:  `self.ok = True`,
			MethodBody: "return payload",
			MethodArg:  "payload",
			MethodArgs: []string{"payload"}, // present, but Starmap is false
			Starmap:    false,
		},
	}
	// A list-shaped payload must NOT be splatted when Starmap is false — it
	// must come through as the single bound value, unchanged.
	failed, err := sup.Run(context.Background(), items([]any{1, 2}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	r, ok := sink.results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	got, ok := r.Result.([]any)
	if !ok || len(got) != 2 {
		t.Errorf("result = %#v, want the list [1,2] bound whole (not splatted) since Starmap=false", r.Result)
	}
}
