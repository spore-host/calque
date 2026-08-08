# Design note: calque's sticky-worker queue contract (M12/#99)

**Status:** spike, decision record. Answers the three questions #99 posed
before any M12 implementation issue (#100-103) starts. No code in this pass.

## The core incompatibility this resolves

spawn's `pkg/taskpool` (`worker.go:125-141`, `exec.go:42-82`) gives every
claimed queue message a brand-new bash subprocess in a wiped workspace —
`Worker.runOne` acquires a clean `WorkspaceProvider` dir per task and resets it
after, unconditionally (`worker.go:128-141`). That's the opposite of calque's
`warm.Supervisor` invariant: `@enter` runs exactly once per process lifetime
(`runner.py:76-79`, `warmd.go`'s `warmUp`/`EnterCount`), and many items drain
against the same loaded model. A calque pool worker must NOT get a fresh
process per claim — it must claim, stay warm, and claim again.

## Decision 1: message granularity — one item, or one run's item batch?

**Decision: one run's item batch per claim, not one item per claim.**

Today's per-run manifest (`internal/exec/orchestrate.go`'s `Manifest`/
`WriteManifest`) already carries `Items []warm.Item` as a batch, and
`warm.Supervisor.Run` already drains a `[]Item` to completion in one call
(`warmd.go:211`). Splitting to one-item-per-SQS-message would mean either (a)
warmd re-entering its own drain loop per message (defeating the pipelining/
batching `Concurrency`/`BatchSize` levers that already exist and are tuned for
whole-batch throughput, `warmd.go:162-174`), or (b) building a second,
finer-grained protocol just for pool mode. Neither pays for itself: the
actual bottleneck this feature targets is `@enter`'s cold-load cost, not
per-item dispatch overhead — same rationale spawn's own pool doc gives for
why `taskpool` exists at all (`queue.go:1-9`, "per-job cost is stage+run
only"). A pool claim message is "here is a batch of N items for model M,"
matching the granularity `Manifest` already uses.

Message body mirrors `taskpool.TaskRef`'s pointer-not-payload shape
(`queue.go:60-67`) for the same reason (SQS's 256 KiB cap): a claim message
carries a pointer to an S3-staged manifest fragment (reusing `RunLayout`/
`WriteManifest`'s existing S3-manifest machinery), not the items inline.

```go
// PoolClaimRef mirrors taskpool.TaskRef's pointer shape.
type PoolClaimRef struct {
    RunID      string `json:"run_id"`
    Model      string `json:"model"`      // pool affinity key — see decision 2
    ManifestURI string `json:"manifest_uri"` // s3:// pointer to this claim's item batch
}
```

## Decision 2: single-model-per-pool for v1, or cross-model matching?

**Decision: single-model-per-pool for v1. No "is an already-warm worker
carrying my model" lookup across a heterogeneous fleet.**

Reasoning, in order of weight:

1. **Modal's own architecture doesn't do this either.** #96's research
   (Modal's "truly serverless GPUs" engineering post) found Modal's actual
   answer to "many users, few GPUs" is fleet-level scheduling across a large
   shared buffer of idle GPUs — each container/replica still gets its own
   dedicated physical GPU at any instant, and Modal's fleet is implicitly
   partitioned per model/image since a container can only be warm with the
   one image it booted from. There's no cross-model warm-matching to port
   because Modal doesn't do cross-model warm-matching.
2. **It avoids inventing a scheduling problem the project doesn't need yet.**
   A heterogeneous fleet needs an eviction policy (what unloads when GPU
   memory is full and a different model's claim arrives), which is real
   distributed-systems work with no existing calque primitive to build on.
   Single-model-per-pool makes affinity trivial: `PoolClaimRef.Model` equals
   the pool's queue name, full stop — a worker only ever claims from its own
   pool's queue.
3. **It matches spawn's own `QueueName(runID)` convention exactly.** Just as
   `taskpool.Queue` is one queue per pipeline run
   (`QueueName(runID) = "spawn-pool-" + runID`, `queue.go:69-84`), a calque
   pool queue is one queue per model: `PoolQueueName(model) = "calque-pool-" +
   slug(model)`. Reuse the identical `CreateQueue`/`OpenQueue` idempotent-
   create-or-resolve pattern verbatim.

Multi-model-per-instance sharing (one GPU, several models time-sliced or
MIG/MPS-partitioned) is a **different feature** — that's M13 (institutional
GPU sharing), which subdivides one already-acquired instance below the fleet
layer. M12 stays "one warm model per pool instance, N instances per pool,"
matching the idle-fleet design's own stated v1 scope.

## Decision 3: how a worker announces "I am warm with model X"

**Decision: implicitly, by which pool queue it claims from — no separate
announce/heartbeat message.**

Because affinity is pool-identity (decision 2), a worker never needs to
broadcast "I am warm with model X" to a scheduler for routing purposes: it
booted already knowing its pool's model (baked into its launch user-data,
same as `spawn pool create`'s `--instances`/AMI selection bakes in the task
type). The *submitter* side (`calque run --pool <name>`, per issue #103)
resolves which queue to submit to by pool name, not by asking workers what
they're loaded with. This sidesteps building any liveness/announce protocol
in v1 — a worker's only externally-visible state is "claiming from queue
Q or not," which SQS's own `ApproximateNumberOfMessages`/`Depth`
(`queue.go:246-264`) already exposes for monitoring, reusing that verbatim
rather than inventing a second status channel.

If a worker crashes mid-batch, the claimed message simply isn't acked
(mirrors `taskpool.Worker.runOne`'s ack-only-after-completion-record
discipline, `worker.go:162-167`) and SQS redelivers after the visibility
timeout to another pool worker — which, since every worker in the pool is
loaded with the SAME model (decision 2), can pick it up with no affinity
check needed.

## What this unblocks

- **#100 (done)** — implemented as `internal/pool` (`Worker`/`Queue`/
  `ManifestFetcher`/`ResultWriter`) plus `warm.Supervisor.Warm`/`DrainBatch`/
  `Close`/`IsWarm` (the sticky-runner primitives, `worker/warm-runner/warmd.go`)
  and a new `warmd pool --model <name>` CLI mode (`cmd/warmd/main.go`). The
  queue itself (`internal/pool/queue.go`'s `SQSQueue`) mirrors
  `taskpool.Queue`'s Claim/Ack/CreateQueue/OpenQueue LOGIC against spawn's
  `taskpool.SQSAPI` interface rather than importing the type verbatim, because
  `taskpool.Queue` is hardcoded to a run-scoped name + forced 12h retention —
  wrong for a pool queue that's model-scoped and outlives any one run (see the
  queue.go doc comment for the full reasoning). `PoolClaimRef` above shipped as
  `pool.ClaimRef`, carrying an S3 manifest-fragment pointer that reuses
  `calexec.Manifest`'s existing shape verbatim rather than inventing a new one.
- **#101** (provision via taskcohort/cohort): pool creation now has a fixed
  meaning — `calque pool create --model M` maps 1:1 to one `PoolQueueName(M)`
  queue and one cohort of instances, mirroring `spawn pool create`'s IAM/AMI
  pattern but with model, not task type, as the identity key.
- **#102** (cost-model amortization): "warm hit" now has a precise
  definition — a claim served without a fresh `@enter` (i.e. the batch drains
  against an already-warm `warmd` process from a prior claim), which is
  directly observable via `warmd`'s own `EnterCount` (`warmd.go:160`,
  already incremented once per actual `@enter` call).
- **#103 (done)** — `calque real --pool` submits the run's manifest to
  `OpenPoolQueue(model)` and waits on the SAME `calexec.WaitForSummary`
  every other run path already uses, then folds the claim's own reported
  `Summary.WarmHit`/`EnterSecondsPaid` into `cost.Model` (not a fresh
  acquisition's numbers) — the honest fixed cost for a pool-submitted run is
  whatever the CLAIM paid, per #102. `--pool` and `--shards>1` are mutually
  exclusive (a pool submission is one claim against an already-provisioned
  pool, not a fleet acquisition); `--ami` is not required in `--pool` mode
  (the pool's workers already have one baked in at `calque pool create`
  time). Occupancy sampling in pool mode is explicitly out of scope for this
  pass — flagged in the emitted K's notes, not silently omitted.

## Explicitly deferred (not blocking M12)

- Cross-model warm-worker matching / heterogeneous fleets (a candidate
  future feature, not needed for v1 — revisit only if a real institutional
  workload demonstrably needs it).
- Any liveness/announce protocol beyond SQS's own queue-depth signal.
- Eviction policy for GPU memory when a pool's model changes (doesn't arise
  under single-model-per-pool — a pool's model is fixed at `pool create`
  time for its whole lifetime; changing it means creating a new pool).
