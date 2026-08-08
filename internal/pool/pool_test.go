package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	calexec "github.com/spore-host/calque/internal/exec"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

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
	p, err := filepath.Abs("../../worker/warm-runner/runner.py")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- fakes ------------------------------------------------------------

// fakeQueue is an in-memory Queue: claims are served in submission order, one
// at a time, and only removed on Ack — mirroring taskpool's fakeSQS closely
// enough to drive Worker deterministically without real SQS.
type fakeQueue struct {
	mu      sync.Mutex
	pending []queuedClaim
	claimed map[string]queuedClaim // receipt -> claim, until acked
	nextID  int
}

type queuedClaim struct {
	ref     ClaimRef
	receipt string
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{claimed: map[string]queuedClaim{}}
}

func (f *fakeQueue) submit(ref ClaimRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, queuedClaim{ref: ref})
}

func (f *fakeQueue) Claim(_ context.Context, _ int32) (ClaimRef, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return ClaimRef{}, "", false, nil
	}
	c := f.pending[0]
	f.pending = f.pending[1:]
	f.nextID++
	receipt := fmt.Sprintf("r%d", f.nextID)
	f.claimed[receipt] = c
	return c.ref, receipt, true, nil
}

func (f *fakeQueue) Ack(_ context.Context, receipt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claimed, receipt)
	return nil
}

func (f *fakeQueue) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending) + len(f.claimed)
}

// fakeFetcher serves manifest JSON from an in-memory map keyed by URI.
type fakeFetcher struct {
	byURI map[string][]byte
	err   error
}

func (f fakeFetcher) Fetch(_ context.Context, uri string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.byURI[uri]
	if !ok {
		return nil, fmt.Errorf("no manifest staged at %s", uri)
	}
	return b, nil
}

// fakeResults records every summary written and hands out a fresh MemSink per
// claim (matching production's per-claim result isolation).
type fakeResults struct {
	mu        sync.Mutex
	summaries []writtenSummary
	sinks     []*warm.MemSink
	writeErr  error
}

type writtenSummary struct {
	man     calexec.Manifest
	failed  []int
	warmHit bool
}

func (f *fakeResults) Sink(_ calexec.Manifest) warm.Sink {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := warm.NewMemSink()
	f.sinks = append(f.sinks, s)
	return s
}

func (f *fakeResults) WriteSummary(_ context.Context, man calexec.Manifest, failed []int, warmHit bool) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaries = append(f.summaries, writtenSummary{man: man, failed: failed, warmHit: warmHit})
	return nil
}

func (f *fakeResults) summaryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.summaries)
}

func stageManifest(t *testing.T, fetcher *fakeFetcher, uri string, man calexec.Manifest) {
	t.Helper()
	b, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.byURI == nil {
		fetcher.byURI = map[string][]byte{}
	}
	fetcher.byURI[uri] = b
}

func items(payloads ...any) []warm.Item {
	out := make([]warm.Item, len(payloads))
	for i, p := range payloads {
		out[i] = warm.Item{Index: i, Payload: p}
	}
	return out
}

// clock lets tests drive the worker's idle-timeout deterministically, mirroring
// taskpool's own test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func drainClock(t *testing.T, clk *clock, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		clk.add(2 * time.Second)
		select {
		case <-done:
			return true
		default:
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---- tests --------------------------------------------------------------

// TestWorker_StaysWarmAcrossClaims is the core sticky-pool proof (calque#100):
// two claims for the SAME model, served by ONE worker, must warm the runner
// only ONCE — @enter's cost is paid on the first claim and NOT the second.
func TestWorker_StaysWarmAcrossClaims(t *testing.T) {
	q := newFakeQueue()
	fetcher := &fakeFetcher{}
	results := &fakeResults{}
	sup := &warm.Supervisor{Python: python(t), Script: runnerScript(t)}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}

	man := calexec.Manifest{
		EnterBody:  `self.calls = 0`,
		MethodBody: "self.calls += 1\nreturn {'call': self.calls}",
		MethodArg:  "payload",
	}
	man.Items = items("a")
	stageManifest(t, fetcher, "s3://b/claim1.json", man)
	man2 := man
	man2.Items = items("b")
	stageManifest(t, fetcher, "s3://b/claim2.json", man2)

	q.submit(ClaimRef{RunID: "run-1", Model: "resnet", ManifestURI: "s3://b/claim1.json"})
	q.submit(ClaimRef{RunID: "run-2", Model: "resnet", ManifestURI: "s3://b/claim2.json"})

	w := &Worker{
		Queue: q, Fetcher: fetcher, Results: results, Supervisor: sup,
		Config: WorkerConfig{Model: "resnet", IdleTimeout: 10 * time.Second},
		now:    clk.now,
	}

	done := make(chan struct{})
	var served int
	var runErr error
	go func() { served, runErr = w.Run(context.Background()); close(done) }()

	if !drainClock(t, clk, done) {
		t.Fatal("worker did not drain within 5s")
	}
	if runErr != nil {
		t.Fatalf("Run error: %v", runErr)
	}
	if served != 2 {
		t.Fatalf("claims served = %d, want 2", served)
	}
	if q.liveCount() != 0 {
		t.Fatalf("queue not empty after both claims acked: %d remain", q.liveCount())
	}
	if sup.EnterCount != 1 {
		t.Errorf("EnterCount = %d, want 1 (second claim should have reused the resident runner)", sup.EnterCount)
	}
	if results.summaryCount() != 2 {
		t.Fatalf("summaries written = %d, want 2", results.summaryCount())
	}
	// The second claim's item must show call=2 (state persisted from claim 1's
	// call=1), proving the SAME process served both, not just EnterCount==1
	// by coincidence.
	secondSinkResults := results.sinks[1].Results()
	call := int(secondSinkResults[0].Result.(map[string]any)["call"].(float64))
	if call != 2 {
		t.Errorf("second claim's result call=%d, want 2 (runner state did not persist across claims)", call)
	}
	// calque#102: the first claim pays the cold load (warmHit=false); the
	// second reuses the resident runner (warmHit=true) — this is the exact
	// signal a submitter needs to report cost.Measured.WarmHit honestly.
	if results.summaries[0].warmHit {
		t.Error("first claim's summary reports warmHit=true, want false (it triggered the cold load)")
	}
	if !results.summaries[1].warmHit {
		t.Error("second claim's summary reports warmHit=false, want true (it reused the resident runner)")
	}
}

// TestWorker_MismatchedModelClaimIsAckedNotRun: a claim whose Model doesn't
// match this pool's configured model must be dropped (acked, not executed) —
// per docs/pool-queue-contract.md decision 2, this should never happen under
// correct submission, but a worker must not spin on it forever, and must not
// silently execute the wrong workload against its resident model either.
func TestWorker_MismatchedModelClaimIsAckedNotRun(t *testing.T) {
	q := newFakeQueue()
	fetcher := &fakeFetcher{}
	results := &fakeResults{}
	sup := &warm.Supervisor{Python: python(t), Script: runnerScript(t)}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}

	q.submit(ClaimRef{RunID: "run-1", Model: "bert", ManifestURI: "s3://b/wrong.json"})

	w := &Worker{
		Queue: q, Fetcher: fetcher, Results: results, Supervisor: sup,
		Config: WorkerConfig{Model: "resnet", IdleTimeout: time.Second},
		now:    clk.now,
	}

	done := make(chan struct{})
	go func() { _, _ = w.Run(context.Background()); close(done) }()

	if !drainClock(t, clk, done) {
		t.Fatal("worker did not drain")
	}
	if q.liveCount() != 0 {
		t.Fatalf("mismatched claim was not acked/dropped: %d remain", q.liveCount())
	}
	if results.summaryCount() != 0 {
		t.Fatalf("summaries written = %d, want 0 (claim should never have run)", results.summaryCount())
	}
	if sup.IsWarm() {
		t.Error("supervisor warmed up for a mismatched-model claim; want untouched")
	}
}

// TestWorker_FetchFailureLeavesClaimForRedelivery: a manifest fetch failure
// (transient S3 blip) must NOT ack the claim — mirrors taskpool.Worker's own
// spec-fetch-failure handling exactly (leave for redelivery, don't lose work).
func TestWorker_FetchFailureLeavesClaimForRedelivery(t *testing.T) {
	q := newFakeQueue()
	fetcher := &fakeFetcher{err: fmt.Errorf("s3 blip")}
	results := &fakeResults{}
	sup := &warm.Supervisor{Python: python(t), Script: runnerScript(t)}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}

	q.submit(ClaimRef{RunID: "run-1", Model: "resnet", ManifestURI: "s3://b/x.json"})

	w := &Worker{
		Queue: q, Fetcher: fetcher, Results: results, Supervisor: sup,
		Config: WorkerConfig{Model: "resnet", IdleTimeout: time.Second},
		now:    clk.now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = w.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if q.liveCount() != 1 {
		t.Fatalf("claim should remain for redelivery after fetch failure; liveCount = %d", q.liveCount())
	}
}

// TestWorker_IdleDrainClosesResidentRunner: an empty queue drains the worker
// after IdleTimeout, and Close runs (no resident runner left) — the pool's
// scale-to-zero path.
func TestWorker_IdleDrainClosesResidentRunner(t *testing.T) {
	q := newFakeQueue()
	sup := &warm.Supervisor{Python: python(t), Script: runnerScript(t)}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}

	w := &Worker{
		Queue: q, Fetcher: &fakeFetcher{}, Results: &fakeResults{}, Supervisor: sup,
		Config: WorkerConfig{Model: "resnet", IdleTimeout: 10 * time.Second},
		now:    clk.now,
	}

	done := make(chan struct{})
	var served int
	go func() { served, _ = w.Run(context.Background()); close(done) }()

	if !drainClock(t, clk, done) {
		t.Fatal("idle worker did not drain")
	}
	if served != 0 {
		t.Fatalf("claims served = %d on empty queue, want 0", served)
	}
	if sup.IsWarm() {
		t.Error("supervisor still warm after idle-drain; Close should have run")
	}
}
