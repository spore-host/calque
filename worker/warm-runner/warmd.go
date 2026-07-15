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
	Kind         string  `json:"kind"`
	EnterSeconds float64 `json:"enter_seconds"`
	Index        *int    `json:"index"`
	Seconds      float64 `json:"seconds"`
	Result       any     `json:"result"`
	Error        string  `json:"error"`
	Traceback    string  `json:"traceback"`
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
}

// Run drains items in index order, writing each successful result to the sink. It
// returns the indices that permanently FAILED (partial failure — spec §10: "3 of
// 10k items die"). An item is "settled" when it either lands in the sink or fails
// permanently; Run continues until every item is settled or restarts are exhausted.
func (s *Supervisor) Run(ctx context.Context, items []Item) ([]int, error) {
	maxRestarts := s.MaxRestarts
	if maxRestarts == 0 {
		maxRestarts = 5
	}
	done := make(map[int]bool, len(items)) // written to sink
	failed := make(map[int]bool)           // permanent per-item payload failures
	settled := func(idx int) bool { return done[idx] || failed[idx] }

	restarts := 0
	for {
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

		rn, err := startRunner(ctx, s.Python, s.Script)
		if err != nil {
			return unsettled(items, settled), fmt.Errorf("start runner: %w", err)
		}
		if err := s.warmUp(rn); err != nil {
			rn.close()
			restarts++
			if restarts > maxRestarts {
				return unsettled(items, settled), fmt.Errorf("warm-up failed after %d restarts: %w", restarts, err)
			}
			s.leak("integration_edge", fmt.Sprintf("runner warm-up failed (restart %d): %v", restarts, err))
			continue
		}

		// Drain. If the runner dies mid-drain, unsettled items simply get picked up
		// on the next loop iteration (a fresh warm runner) — re-drive is implicit.
		crashed := false
		for _, it := range pending {
			if err := rn.send(map[string]any{"kind": "item", "index": it.Index, "payload": it.Payload}); err != nil {
				crashed = true
			} else {
				var msg outMsg
				if msg, err = rn.recv(); err != nil {
					crashed = true
					s.leak("integration_edge", fmt.Sprintf("runner died mid-item at index %d: %v", it.Index, err))
				} else {
					switch msg.Kind {
					case "result":
						res := Result{Index: it.Index, Result: msg.Result, Seconds: msg.Seconds}
						if err := s.Sink.Put(ctx, res); err != nil {
							rn.close()
							return unsettled(items, settled), fmt.Errorf("sink put index %d: %w", it.Index, err)
						}
						done[it.Index] = true
					case "error":
						// Per-item payload error is a partial failure, NOT a crash: the
						// runner is still warm. Record and move on — never reload the
						// model for one bad item (that would destroy the economics).
						failed[it.Index] = true
						s.leak("unhandled_case", fmt.Sprintf("item %d failed in payload: %s", it.Index, msg.Error))
					default:
						s.leak("integration_edge", fmt.Sprintf("unexpected runner msg kind %q at index %d", msg.Kind, it.Index))
					}
				}
			}
			if crashed {
				break
			}
		}

		if crashed {
			rn.close()
			restarts++
			if restarts > maxRestarts {
				return unsettled(items, settled), fmt.Errorf("exceeded %d runner restarts", maxRestarts)
			}
			continue // re-loop: rebuilds pending from unsettled, restarts runner
		}

		// clean drain — tell the runner to flush and exit
		_ = rn.send(map[string]any{"kind": "shutdown"})
		_, _ = rn.recv() // best-effort "bye"
		rn.close()
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

func (s *Supervisor) warmUp(rn *runner) error {
	if err := rn.send(map[string]any{
		"kind": "config", "enter_body": s.Config.EnterBody,
		"method_body": s.Config.MethodBody, "method_arg": s.Config.MethodArg,
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
