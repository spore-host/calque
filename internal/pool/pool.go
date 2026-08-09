// Package pool implements calque's sticky-worker pool loop (calque#100,
// following the queue contract settled in calque#99 /
// docs/pool-queue-contract.md).
//
// Unlike spawn's pkg/taskpool (a fresh bash subprocess in a wiped workspace per
// claim — the right shape for fungible CPU tasks), a calque pool worker MUST
// keep its warm Python runner resident across claims: @enter runs once per
// worker lifetime, and every claim it serves drains against the SAME loaded
// model, which is the entire point of pooling (skip the cold-load cost on a
// warm hit). This package supplies the claim loop; spawn's taskpool.Queue
// (Claim/Ack/CreateQueue/OpenQueue) is reused verbatim underneath it — per
// docs/pool-queue-contract.md, only the execution contract changes, not the
// SQS semantics.
//
// Per the queue contract's decision 2, a pool is single-model: every claim on
// one pool's queue targets the SAME warm unit (enter/method bodies). A claim
// is "one run's item batch for this pool's model," not one item — matching
// the granularity calque's existing per-run Manifest already uses.
package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	calexec "github.com/spore-host/calque/internal/exec"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// poolQueuePrefix is prepended to every slugged model to form the final SQS
// queue name — kept as a named const so slugModel's length budget (queuePrefix
// vs. the 80-char SQS cap) stays in lockstep with PoolQueueName's own prefix.
const poolQueuePrefix = "calque-pool-"

// PoolQueueName derives the SQS queue name for a pool, keyed by MODEL (not run
// id, unlike spawn's taskpool.QueueName(runID)) — decision 2 of
// docs/pool-queue-contract.md: affinity is pool identity, and a pool's identity
// is its model, fixed for the pool's whole lifetime. SQS names allow
// [A-Za-z0-9_-] up to 80 chars; model strings (e.g. calque's own default
// "Qwen/Qwen2.5-1.5B-Instruct") routinely contain characters SQS rejects
// (namely "/"), so the model is slugged internally — callers never need to
// pre-sanitize (calque#129).
func PoolQueueName(model string) string {
	return poolQueuePrefix + slugModel(model)
}

// slugModel converts an arbitrary model identifier into a string safe to use
// as (part of) an SQS QueueName: lowercased, any character outside
// [a-z0-9_-] replaced with "-", runs of "-" collapsed to one, leading/
// trailing "-" trimmed, and truncated so poolQueuePrefix+slug never exceeds
// SQS's 80-char QueueName limit.
func slugModel(model string) string {
	lower := strings.ToLower(model)
	var b strings.Builder
	b.Grow(len(lower))
	lastDash := false
	for _, r := range lower {
		var out rune
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			out = r
		default:
			out = '-'
		}
		if out == '-' {
			if lastDash {
				continue // collapse consecutive '-'
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(out)
	}
	slug := strings.Trim(b.String(), "-")

	maxLen := 80 - len(poolQueuePrefix)
	if maxLen < 0 {
		maxLen = 0
	}
	if len(slug) > maxLen {
		slug = strings.TrimRight(slug[:maxLen], "-")
	}
	return slug
}

// ClaimRef is what travels on a pool's queue: a pointer to an S3-staged
// manifest fragment, not the items inline (mirrors taskpool.TaskRef's
// pointer-not-payload shape, for the same reason — SQS's 256 KiB message cap).
type ClaimRef struct {
	// RunID names the submitting run, for log/leak attribution only — claim
	// routing is entirely by which pool queue this ref was received on.
	RunID string `json:"run_id"`
	// Model is the pool's model identity. A worker checks this against its own
	// resident Config on every claim (see Worker.checkAffinity) as a defensive
	// belt-and-suspenders: single-model-per-pool means this should always
	// match what the worker is already warm with.
	Model string `json:"model"`
	// ManifestURI is the s3:// location of this claim's calexec.Manifest
	// fragment (enter/method bodies + items + result/summary keys) — reuses
	// the existing Manifest shape verbatim rather than inventing a new one.
	ManifestURI string `json:"manifest_uri"`
}

// ManifestFetcher loads a claim's manifest JSON by its S3 URI. Interface (not
// a concrete S3 client) so the worker loop is unit-testable without S3 —
// mirrors taskpool.SpecFetcher's exact seam.
type ManifestFetcher interface {
	Fetch(ctx context.Context, manifestURI string) ([]byte, error)
}

// ResultWriter persists one claim's drained results and writes its completion
// summary. Interface so the loop is testable without S3; the production
// implementation wraps calexec.S3Sink + a JSON PutObject for the summary.
type ResultWriter interface {
	// Sink returns the result sink to hand to warm.Supervisor for this claim's
	// batch — implementations key it by the manifest's own Bucket/ResultPrefix
	// so concurrent claims' results never collide.
	Sink(man calexec.Manifest) warm.Sink
	// WriteSummary persists the claim's completion record so a submitter
	// polling man.SummaryKey (via calexec.WaitForSummary, reused unmodified)
	// observes the claim as done AND knows which fixed-cost regime to feed
	// cost.Measured with (calque#102/#103): warmHit — was the resident
	// runner already warm when this claim was served; enterSecondsPaid —
	// the @enter cost THIS claim actually paid (0 on a warm hit; the
	// measured load time on a miss, since the runner just reloaded to serve
	// it).
	WriteSummary(ctx context.Context, man calexec.Manifest, failed []int, warmHit bool, enterSecondsPaid float64) error
}

// Queue is the slice of spawn's taskpool.Queue this package needs — an
// interface so tests inject a fake claim source without importing spawn's SQS
// fakes. The production Worker wires this to a real *taskpool.Queue opened via
// taskpool.OpenQueue(ctx, sqsClient, ""); calque never calls CreateQueue itself
// under a run id the way taskpool.Pool does, since a pool's queue is keyed by
// model and outlives any one run (see PoolQueueName).
type Queue interface {
	Claim(ctx context.Context, waitSeconds int32) (ref ClaimRef, receiptHandle string, ok bool, err error)
	Ack(ctx context.Context, receiptHandle string) error
}

// WorkerConfig configures one pool worker's claim loop.
type WorkerConfig struct {
	// Model is this pool's fixed model identity — every claim must match it
	// (see checkAffinity).
	Model string
	// PollWaitSeconds is the SQS long-poll wait per Claim (0..20), mirroring
	// taskpool.WorkerConfig.PollWaitSeconds.
	PollWaitSeconds int32
	// IdleTimeout is how long the worker keeps polling an empty queue before
	// it closes its resident runner and returns — sized to institutional job
	// inter-arrival time, NOT a CPU task-pool worker's short default, because
	// closing here throws away the warm model calque exists to keep loaded.
	IdleTimeout time.Duration
	// Log receives one-line progress messages; nil discards them.
	Log io.Writer
}

// Worker drains a model-scoped pool queue against a single resident
// warm.Supervisor: claim -> (warm once, if not already) -> drain the batch ->
// write results+summary -> ack -> loop, until idle past IdleTimeout, at which
// point it closes the resident runner (paying the reload cost is the NEXT
// worker's problem, not an ever-idle one's to keep paying rent on).
type Worker struct {
	Queue      Queue
	Fetcher    ManifestFetcher
	Results    ResultWriter
	Supervisor *warm.Supervisor
	Config     WorkerConfig
	// now overrides time.Now for deterministic idle-timeout tests. Nil => time.Now.
	now func() time.Time
}

// Run drives the claim loop until the queue is idle for IdleTimeout, then
// closes the resident runner and returns. Mirrors taskpool.Worker.Run's
// control-flow shape (claim/idle-check/reset-deadline) but the per-claim body
// (runOne here) drains against a RESIDENT runner instead of spawning a fresh
// subprocess — the whole point of this package.
func (w *Worker) Run(ctx context.Context) (claimsServed int, err error) {
	now := w.now
	if now == nil {
		now = time.Now
	}
	idleDeadline := now().Add(w.Config.IdleTimeout)
	defer w.Supervisor.Close()

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return claimsServed, ctxErr
		}

		ref, receipt, ok, cerr := w.Queue.Claim(ctx, w.Config.PollWaitSeconds)
		if cerr != nil {
			w.logf("claim error (continuing): %v", cerr)
			if !sleepCtx(ctx, time.Second) {
				return claimsServed, ctx.Err()
			}
			continue
		}
		if !ok {
			if now().After(idleDeadline) {
				w.logf("idle for %s with empty queue; closing resident runner and draining", w.Config.IdleTimeout)
				return claimsServed, nil
			}
			continue // long-poll already waited; loop and poll again
		}

		idleDeadline = now().Add(w.Config.IdleTimeout)
		w.runOne(ctx, ref, receipt)
		claimsServed++
	}
}

// runOne fetches, drains, and acks a single claim. Errors are contained the
// same way taskpool.Worker.runOne contains them: a claim this worker cannot
// service is left un-acked for redelivery rather than silently dropped.
func (w *Worker) runOne(ctx context.Context, ref ClaimRef, receipt string) {
	if ref.Model != w.Config.Model {
		// Single-model-per-pool (decision 2) means this should never happen — a
		// misrouted claim is a submitter bug, not a runtime condition to recover
		// from by reloading (which would break the very warm-reuse this package
		// exists for). Leak loudly and ack anyway: NOT acking would spin this
		// claim forever across every worker in the pool, since they all share
		// the same (wrong-for-this-claim) model. The submitter's own
		// WaitForSummary times out with no summary written — a clear "this
		// claim never completed" signal, rather than a silently wrong result.
		w.logf("claim run=%s model=%q does not match this pool's model %q (dropping, submitter bug?)",
			ref.RunID, ref.Model, w.Config.Model)
		if aerr := w.Queue.Ack(ctx, receipt); aerr != nil {
			w.logf("claim run=%s: ack of mismatched claim failed: %v", ref.RunID, aerr)
		}
		return
	}

	manJSON, ferr := w.Fetcher.Fetch(ctx, ref.ManifestURI)
	if ferr != nil {
		// Fetch failed (likely transient S3 blip) — leave for redelivery rather
		// than acking a claim we never ran, mirroring taskpool.Worker.runOne's
		// spec-fetch-failure handling exactly.
		w.logf("claim run=%s: manifest fetch failed (leaving for redelivery): %v", ref.RunID, ferr)
		return
	}
	var man calexec.Manifest
	if err := json.Unmarshal(manJSON, &man); err != nil {
		// A malformed manifest can never be run; ack so it doesn't loop forever
		// (mirrors taskpool.Queue.Claim's own malformed-ref handling).
		w.logf("claim run=%s: malformed manifest (dropping): %v", ref.RunID, err)
		if aerr := w.Queue.Ack(ctx, receipt); aerr != nil {
			w.logf("claim run=%s: ack of malformed claim failed: %v", ref.RunID, aerr)
		}
		return
	}

	// Capture BEFORE DrainBatch: this is "was the runner already warm when THIS
	// claim arrived" (calque#102's WarmHit), not "is it warm now" (DrainBatch
	// always leaves it warm on a clean drain, which would make every claim
	// after the pool's first look like a hit regardless of whether IT paid a
	// reload after a crash). enterCountBefore lets us detect the (rarer) case
	// where a claim STARTED warm but a mid-drain crash forced a reload anyway —
	// that claim did pay for a load even though it wasn't a cold-start claim.
	wasWarm := w.Supervisor.IsWarm()
	enterCountBefore := w.Supervisor.EnterCount
	if !wasWarm {
		w.Supervisor.Config = warm.Config{EnterBody: man.EnterBody, MethodBody: man.MethodBody, MethodArg: man.MethodArg}
	}
	w.Supervisor.Sink = w.Results.Sink(man)

	failed, derr := w.Supervisor.DrainBatch(ctx, man.Items)
	if derr != nil {
		// The supervisor exhausted its restart budget — this claim did not
		// complete. Leave it un-acked: the resident runner is gone
		// (DrainBatch's own crash path clears s.active on a fatal exit), so the
		// NEXT claim (this one redelivered, or another) pays a fresh warm-up,
		// same as any other post-crash recovery.
		w.logf("claim run=%s: drain failed (leaving for redelivery): %v", ref.RunID, derr)
		return
	}

	// A load happened during THIS claim if EnterCount advanced, regardless of
	// whether the claim started warm (covers the started-warm-but-crashed-
	// mid-drain case above). warmHit (reported to the submitter) is about
	// whether the claim STARTED warm — those are different questions: a claim
	// can start warm, crash once, and still have paid a partial reload cost.
	paidLoad := w.Supervisor.EnterCount > enterCountBefore
	enterSecondsPaid := 0.0
	if paidLoad {
		enterSecondsPaid = w.Supervisor.EnterSeconds
	}

	if serr := w.Results.WriteSummary(ctx, man, failed, wasWarm, enterSecondsPaid); serr != nil {
		// Wrote results (if any landed before this point they're already in the
		// sink) but couldn't signal completion. Leave un-acked: a redelivery
		// re-drains against the (still warm, unaffected) resident runner and
		// results are idempotent overwrites at deterministic keys — safe to retry.
		w.logf("claim run=%s: write summary failed (leaving for redelivery): %v", ref.RunID, serr)
		return
	}

	if aerr := w.Queue.Ack(ctx, receipt); aerr != nil {
		w.logf("claim run=%s: ack failed (may redeliver a completed claim): %v", ref.RunID, aerr)
	}
}

func (w *Worker) logf(format string, a ...interface{}) {
	if w.Config.Log == nil {
		return
	}
	fmt.Fprintf(w.Config.Log, "[pool worker] "+format+"\n", a...)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
