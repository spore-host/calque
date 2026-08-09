# Behind-the-seam register (M10 — what calque deliberately does NOT port)

**Status:** living decision record (§1/§4/§18). The spike proves Modal-shaped GPU
code runs unchanged on AWS. Everything below is **explicitly out of scope** — not
because it's hard, but because porting it would mean building the "brain" the seam
(§4) exists to defer. Each entry names the attach point a future build would touch,
so "won't-port" stays a documented decision rather than a silent gap.

The discipline throughout: **recognize and leak, never silently drop.** If a script
uses one of these, calque emits a `semantic_gap`/`scope:behind-seam` leak (§10) so
the census stays honest.

## Recognized-and-leaked in code today

| Idiom | Where it's recognized | Leak says |
|---|---|---|
| Autoscaling kwargs — `concurrency_limit`, `allow_concurrent_inputs`, `min_containers`, `max_containers`, `keep_warm`, `container_idle_timeout` (S1) | `internal/parse/parse.go` `readConfigKwargs` | "autoscaling/warm-pool config — belongs to the real brain behind the seam (§4), not ported in the spike (§1)" |
| `.map.aio()` async future (S2) | `tools/pyast/pyast.py` `visit_Call` → `parse.invocationKinds` | "async result futures / detach — deferred per §18; the spike is block-and-wait only" |
| `.spawn()` (S2, calque#88) | `tools/pyast/pyast.py` `visit_Call` → `parse.invocationKinds` | now CLASSIFIED (`ir.InvokeSpawn`, findable via `ir.App.FindFunction`/`FindClass`) but still not executed: "classified but not executed — block-and-wait fan-out … deferred per §18 (calque#97 tracks the driver)" |
| `secrets=`, `schedule=`, `region=` | `readConfigKwargs` | recorded but NOT honored (§B); a payload that needs them fails visibly |
| Serve entrypoints (§F/M9) | `pyast` `entry_kind` → `run.go` | "serve shape … execution shape deferred" (see [serve-architecture.md](serve-architecture.md)) |
| Volume mid-run `reload()` (§E/M8) | `pyast` `volume_writes` → `parse` | "mid-run re-read of a mutated volume is not reproduced" |
| `Function.from_name("other-app", "fn").remote(...)` — cross-app invocation (calque#87, decided non-goal calque#137) | `tools/pyast/pyast.py` `visit_Call` → `internal/parse/parse.go` (captures app name, object name, args) | recognized, never executed: calque runs exactly ONE script's `ir.App` per invocation, and has no path to locate/parse a SEPARATE deployed app's source — unlike `.spawn()` (calque#97), whose targets live inside the already-parsed `ir.App`. Building real orchestration would mean inventing a deployment-registry concept (name→script resolution) disproportionate to how rare this idiom is. Permanent non-goal, not a "needs design" gap. |

## Non-goals that carry NO code (would-be attach points)

- **Real card selection.** The `StubRecommender` (`internal/target/target.go`) is a
  single constant by design (§4). Real phase-detection / right-sizing is the whole
  point of the seam and is deferred. Attach point: swap `StubRecommender` for a real
  `Recommender` — the plumbing never notices.
- **Cost/latency frontier optimization.** The spike does not search the
  price/latency Pareto frontier or otherwise optimize instance choice on cost.
  Attach point: `internal/cost`.
- **MIG partitioning / fractional GPUs (institutional, M13).** UPDATED
  (calque#107): no longer a non-goal for the spike's own scope, but a
  real primitive now exists behind `internal/mig` (fixed-layout profile
  picker + boot-time provisioning script, live-hardware-verified profiles per
  `docs/gpu-sharing-support-matrix.md`) and `internal/tenancy` (check-out/
  check-in slice binding, `docs/tenancy-vs-session.md`). Still explicitly
  deferred: LIVE/dynamic reconfiguration while slices are in use, and
  cross-slice scheduling when all slices are occupied (falls back to the M12
  fleet layer acquiring another instance) — see calque#107's own scope.
- **Multi-tenant GPU sharing via MPS (institutional, M13).** UPDATED
  (calque#108): Modal declines any cross-tenant single-GPU sharing (#96) —
  calque's institutional target can accept the trade for a known/bounded
  university population, gated loudly behind an explicit opt-in
  (`internal/mps.RequireOptIn`, mirroring
  `--i-understand-this-spends-money`'s discipline for a different risk
  category). MPS gives NO per-client isolation, so the blast-radius policy is
  CONSERVATIVE by design: `internal/mps.Coordinator` restarts every sibling
  client sharing an MPS context on ANY one crash, not just the crashed
  client — implemented, not just documented. Reuses `internal/tenancy`'s
  check-out/check-in `Registry` unmodified (built generically in #107 for
  exactly this reuse). Not yet wired into any CLI path — that's process
  wiring, deferred to a follow-up once a real institutional workload needs
  it.
- **NEFF / Trainium / Inferentia.** No Neuron path; CUDA/GPU only. Attach point:
  `internal/target` card vocabulary + `internal/image` base.
- **GPU topology / multi-node / gang scheduling.** The gpu guard (§7) explicitly
  FLAGS multi-GPU/coupled units as out of single-node scope. M7's fan-out is
  embarrassingly-parallel across *independent* single-node boxes — not gang
  scheduling. Attach point: `internal/gpu` guard + a real scheduler behind the seam.
- **Fuzzy Bedrock quality scoring.** The gate (§11) is exact-match only; a near
  match is surfaced as a labeled candidate with NO quality claim (M5 `ReplacementOffer`).
  Scoring "is this variant good enough" is deferred. Attach point: `internal/gate`.

## spore.host integration threads (tracked under #2)

- **spawn has no headless-container / ECR primitive** — flagged at
  `internal/exec/bootstrap.go`; calque drives docker over SSM as a workaround
  (upstream spawn#351 / spawn#353).
- **`--detach`** is still stubbed (async detach not wired).

These live under the standing integration tracker (#2), not a milestone deliverable.
