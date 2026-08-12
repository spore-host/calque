package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
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
	mu        sync.Mutex
	pending   []queuedClaim
	claimed   map[string]queuedClaim // receipt -> claim, until acked
	nextID    int
	extends   []string // receipts passed to Extend, in call order
	extendErr error
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

// Extend is a no-op stub for tests that don't specifically exercise
// heartbeating — it just records the call (calque#131) so
// TestWorker_HeartbeatExtendsVisibilityDuringLongDrain can assert it happened
// at least once during a slow DrainBatch.
func (f *fakeQueue) Extend(_ context.Context, receipt string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extends = append(f.extends, receipt)
	return f.extendErr
}

func (f *fakeQueue) extendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.extends)
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
	man              calexec.Manifest
	failed           []int
	warmHit          bool
	enterSecondsPaid float64
	occ              calexec.OccupancyRaw
}

func (f *fakeResults) Sink(_ calexec.Manifest) warm.Sink {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := warm.NewMemSink()
	f.sinks = append(f.sinks, s)
	return s
}

func (f *fakeResults) WriteSummary(_ context.Context, man calexec.Manifest, failed []int, warmHit bool, enterSecondsPaid float64, occ calexec.OccupancyRaw) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaries = append(f.summaries, writtenSummary{man: man, failed: failed, warmHit: warmHit, enterSecondsPaid: enterSecondsPaid, occ: occ})
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

// validSQSQueueName matches SQS's own QueueName constraint: up to 80
// characters from [A-Za-z0-9_-]. slugModel's output (via PoolQueueName) must
// always satisfy this, or CreateQueue fails outright (calque#129).
var validSQSQueueName = regexp.MustCompile(`^calque-pool-[a-z0-9_-]+$`)

// TestPoolQueueName_SlugsUnsafeModelChars: calque's own default/showcased
// model string "Qwen/Qwen2.5-1.5B-Instruct" (cmd/calque's --model default and
// realrun.go's canonical example) contains "/", which SQS's QueueName rejects
// — this is the exact real-world case calque#129 reports as an outright
// CreateQueue failure prior to this fix.
func TestPoolQueueName_SlugsUnsafeModelChars(t *testing.T) {
	got := PoolQueueName("Qwen/Qwen2.5-1.5B-Instruct")
	if !validSQSQueueName.MatchString(got) {
		t.Errorf("PoolQueueName(%q) = %q, does not match valid SQS QueueName pattern %s",
			"Qwen/Qwen2.5-1.5B-Instruct", got, validSQSQueueName)
	}
	if len(got) > 80 {
		t.Errorf("PoolQueueName(%q) = %q, length %d exceeds SQS's 80-char QueueName limit",
			"Qwen/Qwen2.5-1.5B-Instruct", got, len(got))
	}
}

// TestPoolQueueName_AlreadyCleanModelIsUnchanged: a model name that's already
// a valid SQS-safe token should pass through slugging harmlessly (no
// surprise mutation of names that didn't need sanitizing).
func TestPoolQueueName_AlreadyCleanModelIsUnchanged(t *testing.T) {
	got := PoolQueueName("llama-3-8b")
	want := "calque-pool-llama-3-8b"
	if got != want {
		t.Errorf("PoolQueueName(%q) = %q, want %q", "llama-3-8b", got, want)
	}
	if !validSQSQueueName.MatchString(got) {
		t.Errorf("PoolQueueName(%q) = %q, does not match valid SQS QueueName pattern %s",
			"llama-3-8b", got, validSQSQueueName)
	}
}

// TestRunQueueName_SlugsUnsafeRunIDChars (calque#145): a run id containing
// characters SQS rejects must still slug to a valid QueueName — the
// run-scoped mirror of TestPoolQueueName_SlugsUnsafeModelChars, since
// RunQueueName defensively slugs the same way even though calque's own
// runIDs are typically already SQS-safe.
func TestRunQueueName_SlugsUnsafeRunIDChars(t *testing.T) {
	got := RunQueueName("fleet/run 2026-08-11")
	if !validRunSQSQueueName.MatchString(got) {
		t.Errorf("RunQueueName(%q) = %q, does not match valid SQS QueueName pattern %s",
			"fleet/run 2026-08-11", got, validRunSQSQueueName)
	}
	if len(got) > 80 {
		t.Errorf("RunQueueName(%q) = %q, length %d exceeds SQS's 80-char QueueName limit",
			"fleet/run 2026-08-11", got, len(got))
	}
}

// validRunSQSQueueName is validSQSQueueName's run-scoped counterpart
// (calque#145) — RunQueueName's prefix differs from PoolQueueName's.
var validRunSQSQueueName = regexp.MustCompile(`^calque-fleet-[a-z0-9_-]+$`)

// TestRunQueueName_AlreadyCleanRunIDIsUnchanged mirrors
// TestPoolQueueName_AlreadyCleanModelIsUnchanged, and also asserts the
// run-scoped prefix is DISTINCT from the pool-scoped one — the two queue
// kinds must never collide under the same identifier.
func TestRunQueueName_AlreadyCleanRunIDIsUnchanged(t *testing.T) {
	got := RunQueueName("fleet-quota-verify2-20260811-0459")
	want := "calque-fleet-fleet-quota-verify2-20260811-0459"
	if got != want {
		t.Errorf("RunQueueName(%q) = %q, want %q", "fleet-quota-verify2-20260811-0459", got, want)
	}
	if got == PoolQueueName("fleet-quota-verify2-20260811-0459") {
		t.Error("RunQueueName and PoolQueueName must not collide for the same identifier")
	}
}

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
	// calque#103: EnterSecondsPaid must be >0 for the load-triggering claim
	// and exactly 0 for the claim that reused the resident runner — this is
	// the number a submitter feeds into cost.Measured.EnterSeconds for a
	// warm-hit run (near-zero, not the pool's original load time).
	if results.summaries[0].enterSecondsPaid <= 0 {
		t.Errorf("first claim's EnterSecondsPaid = %v, want >0 (it paid the load)", results.summaries[0].enterSecondsPaid)
	}
	if results.summaries[1].enterSecondsPaid != 0 {
		t.Errorf("second claim's EnterSecondsPaid = %v, want 0 (it paid no load)", results.summaries[1].enterSecondsPaid)
	}
}

// TestWorker_ManifestExtrasStarmapImportsSurviveIntoConfig (calque#146/#147)
// is the regression test for a real bug found while implementing calque#146:
// runOne's `w.Supervisor.Config = warm.Config{EnterBody, MethodBody,
// MethodArg}` silently dropped MethodArgs/Starmap/Extras/ExtraConsts/
// ExtraImports/ExtraClasses — meaning `warmd fleet` (calque#145's whole
// worker-pool mechanism) would either mis-bind a .starmap()'d unit's args
// or NameError on any sibling function/constant/import/class a real
// script's picked unit bare-references, even though the SAME manifest ran
// fine through the single-instance runOnInstance path (cmd/warmd/main.go).
// This claim's EnterBody bare-references an imported name (`Path`) via
// ExtraImports AND a plain module-level class via ExtraClasses — proving
// both fixes, not just that the struct fields compile.
func TestWorker_ManifestExtrasStarmapImportsSurviveIntoConfig(t *testing.T) {
	q := newFakeQueue()
	fetcher := &fakeFetcher{}
	results := &fakeResults{}
	sup := &warm.Supervisor{Python: python(t), Script: runnerScript(t)}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}

	man := calexec.Manifest{
		EnterBody:    `self.base = _Wrap(Path("/tmp"))`,
		MethodBody:   "return str(self.base.value / payload)",
		MethodArg:    "payload",
		ExtraImports: []warm.ExtraImport{{Name: "Path", Source: "from pathlib import Path"}},
		ExtraClasses: []warm.ExtraClass{{Name: "_Wrap", Source: "class _Wrap:\n    def __init__(self, value):\n        self.value = value"}},
	}
	man.Items = items("world")
	stageManifest(t, fetcher, "s3://b/claim1.json", man)
	q.submit(ClaimRef{RunID: "run-1", Model: "resnet", ManifestURI: "s3://b/claim1.json"})

	w := &Worker{
		Queue: q, Fetcher: fetcher, Results: results, Supervisor: sup,
		Config: WorkerConfig{Model: "resnet", IdleTimeout: time.Second},
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
	if served != 1 {
		t.Fatalf("claims served = %d, want 1", served)
	}
	if results.summaryCount() != 1 {
		t.Fatalf("summaries written = %d, want 1", results.summaryCount())
	}
	got := results.sinks[0].Results()
	r, ok := got[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	want := "/tmp/world" // str(_Wrap(Path("/tmp")).value / "world") — only resolves if BOTH ExtraImports and ExtraClasses reached Config
	if s, ok := r.Result.(string); !ok || s != want {
		t.Errorf("result = %v, want %q — proves ExtraImports (Path) AND ExtraClasses (_Wrap) actually reached warm.Config through runOne, not silently dropped", r.Result, want)
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

// TestWorker_HeartbeatExtendsVisibilityDuringLongDrain proves calque#131's
// fix: a claim whose DrainBatch runs longer than one heartbeat interval must
// have Queue.Extend called at least once WHILE it's still draining, not just
// (accidentally) after. heartbeatInterval is overridden to a few
// milliseconds — the real 900s/3 production interval would make this test
// take minutes — mirroring queue.go's poolOpenRetryDelay pattern of a
// package var tests shrink.
func TestWorker_HeartbeatExtendsVisibilityDuringLongDrain(t *testing.T) {
	origInterval := heartbeatInterval
	heartbeatInterval = func(int) time.Duration { return 20 * time.Millisecond }
	defer func() { heartbeatInterval = origInterval }()

	q := newFakeQueue()
	fetcher := &fakeFetcher{}
	results := &fakeResults{}
	sup := &warm.Supervisor{Python: python(t), Script: runnerScript(t)}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}

	// A method body that sleeps well past several heartbeat ticks (20ms each)
	// so the heartbeat goroutine gets multiple chances to fire before
	// DrainBatch returns.
	man := calexec.Manifest{
		EnterBody:  `pass`,
		MethodBody: "import time\ntime.sleep(0.3)\nreturn {'ok': True}",
		MethodArg:  "payload",
	}
	man.Items = items("a")
	stageManifest(t, fetcher, "s3://b/claim1.json", man)
	q.submit(ClaimRef{RunID: "run-1", Model: "resnet", ManifestURI: "s3://b/claim1.json"})

	w := &Worker{
		Queue: q, Fetcher: fetcher, Results: results, Supervisor: sup,
		Config: WorkerConfig{Model: "resnet", IdleTimeout: 10 * time.Second, VisibilityTimeout: 60},
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
	if served != 1 {
		t.Fatalf("claims served = %d, want 1", served)
	}
	if q.extendCount() == 0 {
		t.Error("Queue.Extend was never called during a drain that outlasted several heartbeat intervals")
	}
}

// TestSummary_OccupancyRoundTripsThroughJSON (calque#116): Summary's new
// Occupancy field must survive a JSON marshal/unmarshal unchanged — this is
// the exact path a pool worker's WriteSummary and a submitter's
// json.Unmarshal(summaryBytes, &summary) (cmd/calque/poolsubmit.go) both take,
// so a field that silently drops on the wire would make emitKForPoolClaim
// read a zero-value Occupancy no matter what the worker measured.
func TestSummary_OccupancyRoundTripsThroughJSON(t *testing.T) {
	mean := 0.83
	want := Summary{
		Failed: []int{2, 5}, WarmHit: true, EnterSecondsPaid: 12.5,
		Occupancy: calexec.OccupancyRaw{
			MeanOccupancy: &mean, OccupancySource: "dcgm_sm", Samples: 40,
			Source: "dcgm_sm", IntervalS: 1.0, Measured: true, Scope: calexec.ScopeInference,
		},
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Summary
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Occupancy.MeanOccupancy == nil || *got.Occupancy.MeanOccupancy != mean {
		t.Errorf("MeanOccupancy = %v, want %v", got.Occupancy.MeanOccupancy, mean)
	}
	if got.Occupancy.Scope != calexec.ScopeInference {
		t.Errorf("Scope = %q, want %q", got.Occupancy.Scope, calexec.ScopeInference)
	}
	if !got.Occupancy.Measured {
		t.Error("Measured = false, want true")
	}
	if got.Occupancy.Samples != 40 {
		t.Errorf("Samples = %d, want 40", got.Occupancy.Samples)
	}
	// The pre-existing fields must still round-trip too — this field addition
	// must not have disturbed them.
	if got.WarmHit != want.WarmHit || got.EnterSecondsPaid != want.EnterSecondsPaid || len(got.Failed) != 2 {
		t.Errorf("pre-existing fields changed across round-trip: got %+v", got)
	}
}
