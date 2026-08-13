# Design note: disambiguating "session" — interactive tenancy vs. today's N-item ramp (calque#106)

**Status:** Historical design decision. Implemented by: v0.2.0 (everything
it scoped has shipped). Current user documentation: README's
"Institutional GPU sharing" section, `docs/guide/cli-reference.md`. Was
design-only when filed; every
primitive it called for has since shipped: `internal/mig` (MIG slice
provisioning), `internal/mps` (MPS trusted-tenant mode), `internal/tenancy`
(generic check-out/check-in registry), closed via #107/#108. The CLI-level
rename this note calls for (`calque session` → `calque ramp`, freeing
`session` for the new interactive-tenancy verb) shipped via #117
(`cmd/calque/ramp.go`). **The new `calque session` CLI verb (interactive
tenancy: `checkout`/`checkin`/`status`/`list`) has ALSO shipped** —
`cmd/calque/session.go` implements the full check-out/check-in lifecycle
this note describes below; see the README's "Institutional GPU sharing"
section or `docs/guide/cli-reference.md` for current usage. This doc's
value going forward is the *design rationale* for the collision/rename
decision and the lifecycle model — not a status page for whether the verb
exists.

## The collision

`cmd/calque/ramp.go` (formerly `session.go`, renamed by #117) implements
**acquire-once, hold, run-many**: one instance is acquired patiently
(acquisition is the slow, expensive part), held for an entire N-item ramp
(`rampOpts.rungs`), and terminated only at the end (`ramp.go`'s doc comment:
"amortizes the painful acquisition across every test instead of
re-acquiring per test"). It is single-tenant — one `calque ramp` invocation,
one user, one instance, for the whole ramp.

M13's institutional-sharing design needs a **different** thing: one
university user's bounded interactive occupancy of a MIG slice or MPS
client-slot on an instance that may be serving several such users
*concurrently*. Both are naturally called "a session" in everyday English —
"my session on the shared GPU," "run the ramp session" — and would collide
in docs, the CLI, and support conversations if left undisambiguated.

## Decision: rename the existing verb, keep "session" for the new concept

**`cmd/calque session` (today's N-item ramp verb) is renamed `calque ramp`.**
"Session" is freed for M13's interactive-tenancy primitive, because:

1. **"Session" is the term university users will actually reach for.** The
   primary audience for M13 (per the project's institutional-university
   pivot) thinks in terms of "starting a session on the GPU," matching how
   interactive HPC/Slurm allocations and Jupyter/notebook servers already use
   the word. Overloading it for calque's internal N-item ramp — a
   spike-only verification tool most end users never invoke — is the
   avoidable collision.
2. **"Ramp" already describes the existing verb's actual behavior precisely.**
   `sessionOpts.rungs` IS a ramp (a comma-separated N-item ramp:
   `--rungs 1,100,1000`, `session.go:167`) — the rename doesn't require
   inventing new vocabulary, it surfaces vocabulary the code already uses
   internally.
3. **The ramp verb is a lower-traffic, more spike-internal tool.** It exists
   to acquire once and run a script's unchanged execution shape over a
   ramp of item counts for verification, not as an end-user workflow.
   Renaming the less-user-facing verb is the smaller disruption.

This was a rename ONLY — `cmd/calque/ramp.go`'s behavior, flags, and
acquire-once/hold/run-many contract are UNCHANGED from the pre-rename
`session.go`. The rename shipped via #117.

## The new primitive: `calque session` (interactive tenancy)

### Lifecycle

```
check-out  →  interactive use (bounded TTL)  →  check-in / release
```

- **check-out**: bind ONE user to ONE slice (a MIG partition, or an MPS
  client-slot) on an instance that is ALREADY acquired and running — this
  primitive never calls `RunInstances`/`Acquirer.Acquire` itself (see
  boundary statement below). Check-out fails if every slice on the target
  instance is occupied; the caller's job (the M12 fleet layer, or a future
  scheduler) decides whether to wait, route to another instance, or trigger
  the fleet to acquire one more.
- **interactive use**: the user runs work against their slice for up to a
  bounded TTL — NOT a fixed N-item ramp like `calque ramp`'s rungs. A
  session's workload is open-ended/interactive (the university-user use
  case: notebook-style exploration, ad hoc inference calls), so its
  lifecycle is time-bounded, not item-count-bounded. This is the same
  batch-vs-serve distinction `docs/serve-architecture.md` already draws for
  a different reason (fixed N vs. open-ended work) — reusing that framing
  here rather than inventing a third shape.
- **check-in/release**: the user (or the TTL) ends the session; the slice
  becomes available for the next check-out. The underlying instance is
  UNAFFECTED — releasing a slice never terminates the instance, since other
  slices/sessions may still be live on it (this is the entire point of
  sharing: one instance, many sequential or concurrent sessions).

### Contrast with `calque ramp`'s model

| | `calque ramp` (renamed `session`) | `calque session` (new, tenancy) |
|---|---|---|
| Unit of work | Fixed N-item ramp (`--rungs`) | Open-ended, bounded by TTL |
| Tenancy | Single-tenant: one instance, one user | Multi-tenant: N slices, N concurrent users |
| Acquires from AWS? | Yes — `Acquirer.Acquire` directly | No — consumes an already-acquired `Instance` |
| Terminates the instance? | Yes, at the end of the ramp | No — releasing a slice never terminates the host instance |
| Purpose | Verification over an N-item ramp | Institutional interactive GPU access |

## Boundary with M12 (idle fleet)

Per #109's own framing (the M12/M13 joint reconciliation issue, itself
downstream of this one): **M12 decides WHICH/HOW MANY whole instances are
warm and idle; M13's tenancy decides how many concurrent users occupy slices
WITHIN one such instance once it's handed over.**

Concretely: `internal/plan/acquire.go`'s `Acquirer` (and M12's pool-worker
layer, `internal/pool`) is the ONLY thing that ever calls AWS to acquire or
release an EC2 instance. Tenancy's check-out/check-in primitive receives an
already-live `Instance` handle (or, in the pool case, an already-resident
`warm.Supervisor`) from that layer and operates strictly WITHIN it —
subdividing GPU access on hardware someone else acquired, never acquiring
hardware itself. A tenancy check-out failing (no free slice) is a signal for
the FLEET layer to consider acquiring another instance; tenancy itself never
makes that call.

This mirrors the M12 pool worker's own layering discipline (per
`docs/pool-queue-contract.md`): `internal/pool.Worker` never calls
`plan.Acquirer` either — it's handed an instance's worth of compute by
`calque pool create`'s cohort provisioning and operates within it. Tenancy
is the SAME shape one level further down: subdividing what the fleet layer
already provisioned, rather than provisioning it.

## What this unblocks

- **MIG slice provisioning issue**: has a named lifecycle (check-out/
  check-in) to implement against, and a clear non-goal (it does not acquire
  instances) so its scope stays bounded to "manage slices on an instance
  I'm handed."
- **MPS trusted-tenant issue**: same lifecycle, same non-goal — the two
  issues differ only in whether the slice is hardware-isolated (MIG) or
  software-cooperative (MPS), not in how a user checks in/out.
- **#109 (M12/M13 joint boundary doc)**: can cite this note's boundary
  statement directly rather than re-deriving it.
- **The `session`→`ramp` rename**: shipped (#117) — `session` was freed
  for the new interactive-tenancy verb described above.
- **The new `calque session` verb itself**: shipped — `cmd/calque/session.go`
  implements `checkout`/`checkin`/`status`/`list` per this note's lifecycle
  design.
