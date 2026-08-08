// Package main is warmd: the Go supervisor for the warm Python runner (spec §6).
//
// NAMING (important): spec §6 calls this component "spored", but that name is
// already taken in the spore.host ecosystem — the real `spored` is the lifecycle
// daemon that runs inside every spawn'd instance as a systemd service (TTL/idle/
// completion), and `spawn.Provision` installs it. To avoid a collision we call
// OUR component `warmd`. The layering is:
//
//	spored (systemd daemon, spore.host)   <- owns instance lifecycle
//	  └─ runs our launch command
//	       └─ warmd (this) supervises the warm Python runner
//	            └─ python runner.py holds the loaded model, drains items
//
// warmd is baked into the worker image, not a control-plane import. It:
//   - starts the long-lived Python runner,
//   - sends the @enter body ONCE (warm load),
//   - feeds work items over stdio (newline-framed JSON, serial — decision #7),
//   - writes each result to the sink keyed by input index (for ordered collect),
//   - on runner crash, restarts it (reloads @enter) and re-drives unfinished items.
//
// This "Go supervises warm Python" boundary is the riskiest plumbing in the spike.
// Every rough edge here (lifecycle, protocol, backpressure, flush) is logged as a
// leak (§10) — those are exactly the findings the spike exists to surface.
package warm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Config is the warm unit's verbatim bodies (from the parser) plus its item arg.
type Config struct {
	EnterBody  string `json:"enter_body"`
	MethodBody string `json:"method_body"`
	MethodArg  string `json:"method_arg"`
}

// Item is one unit of work, keyed by input index for ordered collection.
type Item struct {
	Index   int `json:"index"`
	Payload any `json:"payload"`
}

// Result is one completed item. Seconds is the warm per-item wall-clock (§8).
type Result struct {
	Index   int     `json:"index"`
	Result  any     `json:"result"`
	Seconds float64 `json:"seconds"`
}

// Sink receives results as they complete. S3 in production (keyed by index),
// in-memory in tests. Writing per-result keeps memory flat at 100k scale.
type Sink interface {
	Put(ctx context.Context, r Result) error
}

// Leaker records a rough edge without coupling spored to the leak package's types
// (spored is worker-side). The control plane maps these into leak.Leak records.
type Leaker interface {
	Leak(kind, detail string)
}

// runner wraps the Python subprocess and its framed stdio.
type runner struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Scanner
	enc    *json.Encoder
	closed bool
}

// wire protocol messages (must mirror runner.py)
type outMsg struct {
	Kind         string     `json:"kind"`
	EnterSeconds float64    `json:"enter_seconds"`
	Index        *int       `json:"index"`
	Seconds      float64    `json:"seconds"`
	Result       any        `json:"result"`
	Error        string     `json:"error"`
	Traceback    string     `json:"traceback"`
	Results      []batchRes `json:"results"` // set on a "batch_result" message
}

// batchRes is one item's outcome inside a "batch_result" (micro-batching). Index
// echoes the item; Error non-empty means that item failed (partial failure) while
// its batch-mates may have succeeded.
type batchRes struct {
	Index   *int    `json:"index"`
	Result  any     `json:"result"`
	Seconds float64 `json:"seconds"`
	Error   string  `json:"error"`
}

func startRunner(ctx context.Context, python string, script string) (*runner, error) {
	cmd := exec.CommandContext(ctx, python, script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // runner reports errors as structured stdout messages
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // results can be large (embeddings)
	return &runner{cmd: cmd, stdin: stdin, out: sc, enc: json.NewEncoder(stdin)}, nil
}

func (r *runner) send(v any) error { return r.enc.Encode(v) }

func (r *runner) recv() (outMsg, error) {
	if !r.out.Scan() {
		if err := r.out.Err(); err != nil {
			return outMsg{}, err
		}
		return outMsg{}, io.EOF // runner exited / crashed
	}
	var m outMsg
	if err := json.Unmarshal(r.out.Bytes(), &m); err != nil {
		return outMsg{}, fmt.Errorf("decode runner msg: %w", err)
	}
	return m, nil
}

func (r *runner) close() {
	if r.closed {
		return
	}
	r.closed = true
	_ = r.stdin.Close()
	_ = r.cmd.Wait()
}

// Supervisor drives a warm runner over a set of items with crash-restart.
type Supervisor struct {
	Python      string // interpreter, e.g. "python3"
	Script      string // path to runner.py
	Config      Config
	Sink        Sink
	Leak        Leaker
	MaxRestarts int // cap on runner restarts before giving up (0 => a sane default)

	// EnterSeconds is the measured one-time warm-load cost (§8), from the last
	// successful @enter. Load-once amortization is a real number here.
	EnterSeconds float64

	// EnterCount is how many times @enter actually ran across the whole Run.
	// On a clean run with no crashes this is 1 — the warm-once invariant made
	// observable, so the amortization claim is checkable, not asserted.
	EnterCount int

	// Concurrency is how many items to keep in flight at once (§ occupancy). 1 (or
	// 0) is the original strictly-serial send-one/recv-one path. >1 pipelines: up to
	// C items are sent before blocking, and results come back OUT OF ORDER keyed by
	// index. This raises occupancy ONLY for thread-safe per-item bodies; for vLLM's
	// offline engine (not thread-safe) it's guarded off in Run — use BatchSize there.
	Concurrency int

	// BatchSize micro-batches items: warmd sends B item payloads in ONE "batch"
	// message; runner.py calls the @method body once with a LIST of B payloads and
	// returns B results (keyed to the batch's indices). This is how vLLM actually
	// batches — a single .generate([p1..pB]) fills the GPU — so it's the real
	// occupancy lever for offline vLLM. 0/1 => one item per call (unchanged).
	BatchSize int

	// InferenceSpans are the wall-clock windows during which items were actually
	// being processed — one span per drain, starting AFTER that runner's @enter
	// returned and ending when its drain stops (clean finish or crash).
	//
	// Why a LIST and not one start/end pair (#71): a crash-restart reloads the model,
	// so a run can be [load][infer][load][infer]. A single outer start→end would
	// swallow the second load and re-contaminate the very number we're separating.
	// Occupancy is averaged over the UNION of these spans, so every load gap — first
	// or after a restart — is excluded by construction.
	InferenceSpans []Span

	// active is the resident runner surviving across DrainBatch calls (calque#100:
	// sticky pool mode). nil when no runner is currently up. Run() (the original,
	// single-batch entrypoint) is now a thin wrapper: Warm, DrainBatch, Close — so
	// its crash-restart behavior is unchanged, but pool mode can call Warm once and
	// DrainBatch repeatedly across many claims without ever calling Close between
	// them, keeping @enter's state loaded (the whole point of #99/#100).
	active *runner
}

// Span is a closed wall-clock window in unix epoch seconds (float, sub-second).
// Epoch (not monotonic) because these are correlated against a SEPARATE process's
// timestamped samples — the occupancy sampler, which may even run on the host while
// warmd runs in a container. Both read the same clock; a monotonic reading would be
// meaningless across processes.
type Span struct {
	StartUnix float64 `json:"start_unix"`
	EndUnix   float64 `json:"end_unix"`
}

// bodyIsVLLMOffline reports whether a @method body looks like it calls vLLM's
// offline engine (self.llm.generate(...) style). Heuristic, deliberately narrow:
// it gates only the concurrency-hang guard, so a false negative just means the old
// (possibly hanging) behavior and a false positive means a safe serial fallback.
func bodyIsVLLMOffline(body string) bool {
	return strings.Contains(body, ".generate(") &&
		(strings.Contains(body, "llm") || strings.Contains(body, "LLM"))
}

// maxRestarts returns the configured restart cap, defaulting to 5.
func (s *Supervisor) maxRestarts() int {
	if s.MaxRestarts == 0 {
		return 5
	}
	return s.MaxRestarts
}

// warmOnce makes exactly ONE attempt to start a runner and warm it (@enter),
// storing it as the resident s.active on success. Callers own their own retry
// budget — this never retries internally, so nesting two retry loops (Warm's and
// DrainBatch's) can't silently multiply the effective restart cap.
func (s *Supervisor) warmOnce(ctx context.Context) error {
	rn, err := startRunner(ctx, s.Python, s.Script)
	if err != nil {
		return fmt.Errorf("start runner: %w", err)
	}
	if err := s.warmUp(rn); err != nil {
		rn.close()
		return err
	}
	s.active = rn
	return nil
}

// Warm ensures a runner is resident and warmed (starts one via warmOnce if none is
// active), retrying a failed warm-up up to MaxRestarts. A no-op if already warm.
//
// This is the sticky-pool entrypoint (calque#100): a pool worker calls Warm ONCE
// at boot (or on first claim), then DrainBatch repeatedly across many claims — the
// resident runner is never torn down between them, so @enter's cost is paid once
// per worker lifetime, not once per claim. Run (below) still calls this internally
// for its own single-batch-then-shutdown contract, unchanged.
func (s *Supervisor) Warm(ctx context.Context) error {
	if s.active != nil {
		return nil
	}
	maxRestarts := s.maxRestarts()
	restarts := 0
	for {
		if err := s.warmOnce(ctx); err != nil {
			restarts++
			if restarts > maxRestarts {
				return fmt.Errorf("warm-up failed after %d restarts: %w", restarts, err)
			}
			s.leak("integration_edge", fmt.Sprintf("runner warm-up failed (restart %d): %v", restarts, err))
			continue
		}
		return nil
	}
}

// IsWarm reports whether a runner is currently resident (Warm/DrainBatch has
// started one and Close hasn't run yet). Exported so a caller outside this
// package (calque#100's pool worker) can tell whether it's safe to change
// s.Config before the FIRST warm-up without racing an already-loaded runner —
// once warm, Config changes never reach the resident process (see DrainBatch).
func (s *Supervisor) IsWarm() bool { return s.active != nil }

// Close cleanly shuts down the resident runner, if any: sends "shutdown", waits for
// the process's best-effort "bye", closes it, and clears the resident state. A
// pool worker calls this on its OWN idle-timeout/shutdown (calque#100); Run calls
// it after one clean drain, preserving its original per-call teardown contract.
func (s *Supervisor) Close() {
	if s.active == nil {
		return
	}
	_ = s.active.send(map[string]any{"kind": "shutdown"})
	_, _ = s.active.recv() // best-effort "bye"
	s.active.close()
	s.active = nil
}

// DrainBatch drains one batch of items against the resident runner (warming one via
// warmOnce first if none is active), restarting on crash and re-driving unsettled
// items — the SAME crash-restart discipline Run always had, under a single shared
// restart budget for this call. Unlike Run, DrainBatch does NOT shut the runner
// down afterward: it stays resident so a caller (pool mode) can call DrainBatch
// again for the next claim without re-paying @enter's cost. Returns the indices
// that permanently FAILED within this batch (payload errors, not crashes).
func (s *Supervisor) DrainBatch(ctx context.Context, items []Item) ([]int, error) {
	// Concurrency guard (#68): threads calling vLLM's OFFLINE LLM.generate() on one
	// shared engine race its step loop and hang — the engine isn't thread-safe; it
	// batches via a LIST passed to a single .generate() call, not concurrent calls.
	// So for a vLLM-offline body we REFUSE C>1 and fall back to serial with a loud
	// leak, rather than deadlock. (The supervisor's concurrency itself is correct and
	// stays available for genuinely thread-safe per-item bodies.) Real vLLM occupancy
	// needs micro-batching — see BatchSize / the batch path.
	if s.Concurrency > 1 && s.BatchSize <= 1 && bodyIsVLLMOffline(s.Config.MethodBody) {
		s.leak("unhandled_case", fmt.Sprintf(
			"concurrency=%d requested but the @method calls vLLM's offline LLM.generate(), which is NOT thread-safe "+
				"(concurrent calls hang the engine); falling back to SERIAL. vLLM batches via a list in ONE call — "+
				"use --batch-size to micro-batch instead of --concurrency.", s.Concurrency))
		s.Concurrency = 1
	}
	maxRestarts := s.maxRestarts()
	done := make(map[int]bool, len(items)) // written to sink
	failed := make(map[int]bool)           // permanent per-item payload failures
	settled := func(idx int) bool { return done[idx] || failed[idx] }

	restarts := 0
	for {
		if s.active == nil {
			if err := s.warmOnce(ctx); err != nil {
				restarts++
				if restarts > maxRestarts {
					return unsettled(items, settled), fmt.Errorf("warm-up failed after %d restarts: %w", restarts, err)
				}
				s.leak("integration_edge", fmt.Sprintf("runner warm-up failed (restart %d): %v", restarts, err))
				continue
			}
		}

		// Build the work list: everything not yet settled, in original index order.
		var pending []Item
		for _, it := range items {
			if !settled(it.Index) {
				pending = append(pending, it)
			}
		}
		if len(pending) == 0 {
			break
		}

		// Drain. If the runner dies mid-drain, unsettled items simply get picked up
		// on the next loop iteration (a fresh warm runner) — re-drive is implicit.
		// C=1 uses the strictly-serial send-one/recv-one drain; C>1 pipelines up to
		// C items in flight and matches OUT-OF-ORDER results by their echoed index.
		//
		// Bracket the drain to record the INFERENCE span (#71): warm-up (the @enter
		// model load) has already returned, so this window contains item work only.
		// Recorded even on crash — the GPU was genuinely busy up to the crash, and the
		// following restart's load is excluded because it lands OUTSIDE every span.
		spanStart := nowUnix()
		crashed, sinkErr := s.drain(ctx, s.active, pending, done, failed)
		s.InferenceSpans = append(s.InferenceSpans, Span{StartUnix: spanStart, EndUnix: nowUnix()})
		if sinkErr != nil {
			s.active.close()
			s.active = nil
			return unsettled(items, settled), sinkErr
		}

		if crashed {
			s.active.close()
			s.active = nil
			restarts++
			if restarts > maxRestarts {
				return unsettled(items, settled), fmt.Errorf("exceeded %d runner restarts", maxRestarts)
			}
			continue // re-loop: rebuilds pending from unsettled, re-warms at top
		}
	}

	if len(failed) == 0 {
		return nil, nil
	}
	out := make([]int, 0, len(failed))
	for i := range failed {
		out = append(out, i)
	}
	return out, nil
}

// Run drains items in index order, writing each successful result to the sink. It
// returns the indices that permanently FAILED (partial failure — spec §10: "3 of
// 10k items die"). An item is "settled" when it either lands in the sink or fails
// permanently; Run continues until every item is settled or restarts are exhausted.
//
// Run is now a thin wrapper over DrainBatch+Close (calque#100): one batch, then
// shut the runner down — its crash-restart behavior and restart budget are
// unchanged from before the sticky-pool refactor. Pool mode calls DrainBatch
// directly, without the trailing Close, to keep the runner warm across claims.
func (s *Supervisor) Run(ctx context.Context, items []Item) ([]int, error) {
	failed, err := s.DrainBatch(ctx, items)
	s.Close()
	return failed, err
}

// drain runs one warm runner over `pending`, recording settled items into done/
// failed. It returns (crashed, sinkErr): crashed=true means the runner died and
// the caller should restart + re-drive the still-unsettled items; sinkErr is a
// fatal sink failure (aborts the whole Run). C=1 => serial; C>1 => pipelined.
func (s *Supervisor) drain(ctx context.Context, rn *runner, pending []Item, done, failed map[int]bool) (bool, error) {
	if s.BatchSize > 1 {
		return s.drainBatched(ctx, rn, pending, done, failed)
	}
	if s.Concurrency > 1 {
		return s.drainConcurrent(ctx, rn, pending, done, failed)
	}
	return s.drainSerial(ctx, rn, pending, done, failed)
}

// drainBatched sends B payloads per "batch" message and expects a "batch_result"
// carrying B (index,result) pairs — the real vLLM occupancy lever: one
// .generate([p1..pB]) call fills the GPU. A crash leaves the in-flight batch's
// items unsettled for re-drive. Per-item failures inside a batch come back tagged
// so a single bad item is a partial failure, not a whole-batch loss.
func (s *Supervisor) drainBatched(ctx context.Context, rn *runner, pending []Item, done, failed map[int]bool) (bool, error) {
	b := s.BatchSize
	for start := 0; start < len(pending); start += b {
		end := start + b
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]
		idxs := make([]int, len(batch))
		payloads := make([]any, len(batch))
		for i, it := range batch {
			idxs[i] = it.Index
			payloads[i] = it.Payload
		}
		if err := rn.send(map[string]any{"kind": "batch", "indices": idxs, "payloads": payloads}); err != nil {
			return true, nil // crashed
		}
		msg, err := rn.recv()
		if err != nil {
			s.leak("integration_edge", fmt.Sprintf("runner died mid-batch (%d items): %v", len(batch), err))
			return true, nil // crashed
		}
		if msg.Kind != "batch_result" {
			// An unexpected top-level message (e.g. a fatal error) — treat as crash so
			// the whole batch re-drives on a fresh runner.
			s.leak("integration_edge", fmt.Sprintf("expected batch_result, got %q", msg.Kind))
			return true, nil
		}
		// Settle each item in the batch by its own index + per-item status.
		for _, r := range msg.Results {
			if r.Index == nil {
				s.leak("integration_edge", "batch_result item without an index")
				continue
			}
			if r.Error != "" {
				failed[*r.Index] = true
				s.leak("unhandled_case", fmt.Sprintf("item %d failed in payload: %s", *r.Index, r.Error))
				continue
			}
			res := Result{Index: *r.Index, Result: r.Result, Seconds: r.Seconds}
			if err := s.Sink.Put(ctx, res); err != nil {
				return false, fmt.Errorf("sink put index %d: %w", *r.Index, err)
			}
			done[*r.Index] = true
		}
	}
	return false, nil
}

// drainSerial is the original strictly-serial drain: send one item, block for its
// result, repeat. Preserved verbatim so C=1 behavior (and its tests) are unchanged.
func (s *Supervisor) drainSerial(ctx context.Context, rn *runner, pending []Item, done, failed map[int]bool) (bool, error) {
	for _, it := range pending {
		if err := rn.send(map[string]any{"kind": "item", "index": it.Index, "payload": it.Payload}); err != nil {
			return true, nil // crashed
		}
		msg, err := rn.recv()
		if err != nil {
			s.leak("integration_edge", fmt.Sprintf("runner died mid-item at index %d: %v", it.Index, err))
			return true, nil // crashed
		}
		if sinkErr := s.settle(ctx, msg, it.Index, done, failed); sinkErr != nil {
			return false, sinkErr
		}
	}
	return false, nil
}

// drainConcurrent pipelines up to Concurrency items in flight. It sends items as
// slots free up and reads results as they arrive — OUT OF ORDER — matching each to
// its work item by the index echoed in the message. On a mid-flight crash, every
// item that hasn't landed in the sink is simply left unsettled: the caller's
// re-drive loop picks up ALL of them on a fresh runner (so N in-flight at crash is
// safe, unlike a design that assumed ≤1). Backpressure caps memory + honors the
// runner's pool size: we never have more than C outstanding.
func (s *Supervisor) drainConcurrent(ctx context.Context, rn *runner, pending []Item, done, failed map[int]bool) (bool, error) {
	c := s.Concurrency
	next := 0     // index into pending of the next item to send
	inFlight := 0 // items sent but not yet settled
	for next < len(pending) || inFlight > 0 {
		// Fill the pipeline up to C in flight.
		for inFlight < c && next < len(pending) {
			it := pending[next]
			if err := rn.send(map[string]any{"kind": "item", "index": it.Index, "payload": it.Payload}); err != nil {
				return true, nil // crashed while sending
			}
			inFlight++
			next++
		}
		// Read one completed result (any index). A recv error is a crash: the
		// inFlight items are left unsettled and re-driven on the next runner.
		msg, err := rn.recv()
		if err != nil {
			s.leak("integration_edge", fmt.Sprintf("runner died with %d items in flight: %v", inFlight, err))
			return true, nil // crashed
		}
		if msg.Index == nil {
			s.leak("integration_edge", fmt.Sprintf("runner msg %q without an index under concurrency", msg.Kind))
			continue
		}
		if sinkErr := s.settle(ctx, msg, *msg.Index, done, failed); sinkErr != nil {
			return false, sinkErr
		}
		inFlight--
	}
	return false, nil
}

// settle records one runner message (result or error) for the given index. A
// "result" lands in the sink and marks done; an "error" is a per-item partial
// failure (the runner stays warm — never reload for one bad item). Returns a
// non-nil error only on a fatal sink failure.
func (s *Supervisor) settle(ctx context.Context, msg outMsg, index int, done, failed map[int]bool) error {
	switch msg.Kind {
	case "result":
		res := Result{Index: index, Result: msg.Result, Seconds: msg.Seconds}
		if err := s.Sink.Put(ctx, res); err != nil {
			return fmt.Errorf("sink put index %d: %w", index, err)
		}
		done[index] = true
	case "error":
		failed[index] = true
		s.leak("unhandled_case", fmt.Sprintf("item %d failed in payload: %s", index, msg.Error))
	default:
		s.leak("integration_edge", fmt.Sprintf("unexpected runner msg kind %q at index %d", msg.Kind, index))
	}
	return nil
}

func (s *Supervisor) warmUp(rn *runner) error {
	conc := s.Concurrency
	if conc < 1 {
		conc = 1
	}
	if err := rn.send(map[string]any{
		"kind": "config", "enter_body": s.Config.EnterBody,
		"method_body": s.Config.MethodBody, "method_arg": s.Config.MethodArg,
		"concurrency": conc,
	}); err != nil {
		return err
	}
	if m, err := rn.recv(); err != nil || m.Kind != "configured" {
		return fmt.Errorf("config not acked (kind=%q err=%v)", m.Kind, err)
	}
	if err := rn.send(map[string]any{"kind": "enter"}); err != nil {
		return err
	}
	m, err := rn.recv()
	if err != nil {
		return err
	}
	if m.Kind == "error" {
		return fmt.Errorf("@enter failed: %s", m.Error)
	}
	if m.Kind != "ready" {
		return fmt.Errorf("expected ready, got %q", m.Kind)
	}
	s.EnterSeconds = m.EnterSeconds
	s.EnterCount++
	return nil
}

// --- helpers ---

// nowUnix is the wall clock as a float epoch second — the shared time basis
// between warmd's spans and the sampler's per-tick `ts`.
func nowUnix() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func (s *Supervisor) leak(kind, detail string) {
	if s.Leak != nil {
		s.Leak.Leak(kind, detail)
	}
}

// unsettled returns, in index order, the items neither written to the sink nor
// permanently failed — the honest "did not complete" set for an early return.
func unsettled(items []Item, settled func(int) bool) []int {
	var out []int
	for _, it := range items {
		if !settled(it.Index) {
			out = append(out, it.Index)
		}
	}
	return out
}

// MemSink is an in-memory result sink: results keyed by index, plus the per-item
// wall-clock series for the tach hook (§8). Used by the dry-run and tests; the
// real run uses an S3 sink. Safe for the serial supervisor (single writer).
type MemSink struct {
	results map[int]Result
	order   []int
}

// NewMemSink builds an empty in-memory sink.
func NewMemSink() *MemSink { return &MemSink{results: map[int]Result{}} }

// Put records a result.
func (m *MemSink) Put(_ context.Context, r Result) error {
	if _, seen := m.results[r.Index]; !seen {
		m.order = append(m.order, r.Index)
	}
	m.results[r.Index] = r
	return nil
}

// Seconds returns the per-item wall-clock series (in completion order) for
// measure.Aggregate.
func (m *MemSink) Seconds() []float64 {
	out := make([]float64, 0, len(m.order))
	for _, idx := range m.order {
		out = append(out, m.results[idx].Seconds)
	}
	return out
}

// Results exposes the keyed results (for ordered collection / assertions).
func (m *MemSink) Results() map[int]Result { return m.results }

// Count reports how many results landed.
func (m *MemSink) Count() int { return len(m.results) }
