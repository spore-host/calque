# Design note: M12 (idle fleet) / M13 (institutional sharing) joint boundary (calque#109)

**Status:** decision record, design-only (no code). Produced so neither
milestone's implementation issues re-derive or accidentally redesign the
other's boundary later — both reference this note.

## The layering statement

**M12 decides WHICH and HOW MANY whole instances are warm and idle. M13
decides how many concurrent users occupy slices WITHIN one such instance
once it has been handed over.**

Concretely, three layers, each strictly built on the one below it:

```
Layer 0: internal/plan/acquire.go's Acquirer
         -- the ONLY thing that ever calls AWS to acquire/release an
            EC2 instance. Owns AZ-sweep, capacity retry/backoff,
            spot vs. on-demand.

Layer 1: M12's internal/pool.Worker (calque#100/#101)
         -- decides WHICH model a pool of instances stays warm with,
            and HOW MANY instances that pool holds. Calls Layer 0
            (via taskcohort/cohort, calque#101) to provision the pool;
            never calls AWS directly itself for anything beyond that.

Layer 2: M13's internal/tenancy.Registry + internal/mig / internal/mps
         -- decides how many CONCURRENT USERS occupy slices WITHIN one
            instance that Layer 1 (or, for a non-pooled dedicated run,
            Layer 0 directly) has already handed over. NEVER calls AWS.
            Consumes an already-live instance/Supervisor; subdivides
            access to hardware someone else acquired.
```

This is the SAME shape stated twice, once per milestone, and this note
exists specifically so it's stated ONCE, here, and both milestones point
at it:

- `docs/pool-queue-contract.md` (M12): "`internal/pool.Worker` never calls
  `plan.Acquirer` either — it's handed an instance's worth of compute by
  `calque pool create`'s cohort provisioning and operates within it."
- `docs/tenancy-vs-session.md` (M13): "tenancy operates strictly WITHIN an
  already-live `Instance`/`warm.Supervisor` handed to it by the fleet
  layer... A check-out failing (no free slice) signals the FLEET layer to
  consider acquiring more capacity; tenancy itself never makes that call."

**The join point is `internal/plan/acquire.go`'s `Acquirer` (Layer 0) — full
stop.** Nothing in M13's code (`internal/mig`, `internal/mps`,
`internal/tenancy`) imports `internal/plan` or any AWS SDK package; this is
enforced structurally the same way `internal/cohort`'s own import-discipline
comment enforces it for that package ("This package MUST NOT import: any AWS
SDK package... cohort deals only in the interfaces declared in ports.go").

Two composed cases this layering produces, both correct without any special
casing:

1. **Tenancy under M12's pool** (the institutional-at-scale case): Layer 1
   provisions N instances warm with one model; each instance's `warm.Supervisor`
   is handed to Layer 2, which further subdivides it into MIG slices or MPS
   client-slots for concurrent university users. A pool worker's own
   claim-serving loop (calque#100) is orthogonal to this — a pool worker
   claims batches for ONE model; tenancy subdivides ONE instance's GPU
   across MULTIPLE interactive users. These can compose (a pool instance
   whose GPU is ALSO MIG-sliced) or not (a dedicated `calque session`/`ramp`
   instance that's tenancy-subdivided without ever going through a pool) —
   Layer 2 does not care which.
2. **Tenancy under a dedicated acquisition** (the simpler case, and the one
   M13's issues #107/#108 actually built and tested against): Layer 0
   acquires ONE instance directly (e.g. via `calque session`, per
   `docs/tenancy-vs-session.md`'s renamed-to-`ramp` N-item ramp path, or a
   future dedicated interactive-session command), and Layer 2 subdivides
   that single instance's GPU across concurrent users — no pool involved at
   all.

## Cost attribution

`internal/cost/cost.go`'s `Measured`/`Model` currently assumes ONE workload
per instance-hour — even M12's own `WarmHit` field (calque#102) only
addresses SEQUENTIAL sharing (does THIS claim pay the full fixed cost, or
does it reuse an already-warm worker from a PRIOR claim on the SAME model).
M13's question is different in kind: how does ONE instance's hourly cost get
apportioned across N users occupying it CONCURRENTLY (multiple MIG slices or
MPS client-slots live at the same time)?

**Decision: not attempted in v1. Flag as a leak, per the project's own
leak-report discipline (`internal/leak`), rather than fabricating a
per-user number.**

Reasoning:

- The project's own established pattern (M12's `WarmHit` design, and the
  occupancy-scope labeling in `cost.Model.Verdict` going all the way back to
  #71) is: when a number's honest computation isn't yet built, LABEL the gap
  explicitly rather than compute a plausible-looking but unjustified number.
  A naive "divide instance-hour cost by N concurrent slices" is tempting but
  wrong in an easily-demonstrable way: slices aren't necessarily equal-sized
  (g7e's MIG layout mixes profile sizes if a future layout allows it, though
  #107's fixed-layout picker currently always chooses one uniform profile
  per card) and occupancy varies per slice (an idle MIG slice someone
  checked out but isn't using yet shouldn't be billed the same as one under
  active load) — an even split silently asserts both are false.
- `internal/tenancy.Registry.Occupancy()`/`HolderOf()` (calque#107) already
  expose the RAW data a future apportionment scheme would need (which user
  held which slice, for how long) — this note's job is to say "not
  computed yet," not to say "the data doesn't exist." A future
  implementation issue can build pro-rata-by-wall-clock-occupancy
  apportionment directly on top of these accessors without any new
  instrumentation.
- Fabricating a number now, only to replace it later, would follow the
  project's own explicitly-rejected pattern (the sampler's whole-run vs.
  inference-window occupancy split, #71, exists BECAUSE an earlier
  under-labeled number was later found to be systematically biased — the
  project's institutional memory here argues strongly for "leak it, don't
  guess it" on a brand-new attribution question).

**Suggested leak record** (for whichever implementation issue eventually
runs a tenancy-subdivided instance through `cost.Model`):
`leak.Addf(leak.PrimAcquire, leak.KindSemanticGap, ..., "N concurrent
tenancy slices shared 1 instance-hour; per-user cost apportionment is NOT
computed (calque#109) — this cost reflects the WHOLE instance, not this
user's share of it")`.

## Compounding blast radius: cross-link #94 and #95

Both #94 (spot interruption handling) and #95 (multi-region capacity search)
are Layer-0/Layer-1 (fleet) concerns that a tenancy-subdivided instance's
MULTIPLE concurrent users are ALL exposed to simultaneously, in a way that
compounds M13's own already-accepted trust-boundary risk (calque#108's MPS
blast radius: one client's crash can take down every sibling sharing that
MPS context) with a SEPARATE, fleet-layer failure mode:

- **Spot interruption (#94):** if the underlying instance is on the spot
  market and gets reclaimed, EVERY MIG slice's session and EVERY MPS
  client's session on that instance ends at once — not just one user's.
  This is a strictly larger blast radius than #108's MPS-crash scenario
  (which at least stays within one physical GPU's clients); a spot reclaim
  takes the WHOLE instance, MIG's hardware isolation between slices
  notwithstanding — MIG protects tenants from EACH OTHER's software faults,
  not from AWS reclaiming the underlying hardware. An institutional
  deployment accepting spot for cost reasons is implicitly accepting this
  compounded risk across however many concurrent users a shared instance
  serves, not just the one user a dedicated (non-shared) spot instance would
  have affected. This should be surfaced explicitly wherever a future
  implementation issue wires tenancy on top of a spot-acquired instance —
  the existing spot leak (`session.go`'s "spot acquisition: R_a is a spot
  rate and the instance is interruptible" leak, see `realOpts`/`sessionOpts`
  callers) is per-RUN; a tenancy-aware version needs to say "interruptible,
  AND N other users' sessions are equally exposed," not just restate the
  single-tenant framing.
- **Multi-region search (#95):** currently out of scope (single-region AZ
  sweep only, per #95's own gap description). Not a NEW risk for M13, but
  worth naming here: if #95 ever ships cross-region fallback, a
  tenancy-subdivided instance's users are pinned to WHATEVER region the
  fleet layer happened to land the underlying instance in — a MIG slice or
  MPS client-slot has no region-independence of its own; it lives and dies
  with its host instance's placement. Any future scheduler that migrates a
  pool across regions (a real M12-side possibility once #95 exists) would
  need to either drain tenancy check-outs first or accept the same
  all-sessions-end-at-once blast radius spot interruption already has.

## What this unblocks

- M13 is now fully specified end-to-end: hardware verification (#104) →
  target seam (#105) → naming/lifecycle (#106) → MIG (#107) → MPS (#108) →
  this joint boundary doc (#109). No open design questions remain for the
  milestone's own scope.
- A future cost-apportionment implementation issue has a concrete starting
  point (`tenancy.Registry`'s existing accessors) and an explicit
  "leak it, don't guess it" mandate for what NOT to fabricate.
- A future tenancy-on-spot implementation issue has the compounding-blast-
  radius framing to build its leak message from, rather than reinventing
  the argument.
