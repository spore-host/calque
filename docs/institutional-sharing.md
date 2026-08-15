# Institutional GPU sharing

**Status:** Authoritative current behavior. Verified through: v0.4.0.

calque's core job is Modal-shaped workload → AWS execution — a real script
runs unchanged on real AWS hardware. **Institutional GPU sharing is an
extension on top of that core**, not a separate product: once a workload
is on infrastructure you control, an institution can make utilization and
trust decisions Modal reasonably can't make for arbitrary public tenants.
This page is the landing point for that extension's deeper material — the
README's own "Institutional GPU sharing" section covers the same ground
at a summary level; start there if you haven't, come here for the design
detail behind each piece.

## The three primitives

- **Warm pools** (`calque pool`) — a model-scoped SQS queue with resident
  workers that keep a loaded model warm across separate claims, instead of
  paying `@enter`'s load cost per run. Design rationale, the queue
  contract, and why a calque pool worker can't use spawn's own
  `pkg/taskpool` unmodified: [`pool-queue-contract.md`](pool-queue-contract.md).
- **MIG slice provisioning** (`internal/mig`) — hardware-partitions a
  supported card (g7/g7e's RTX PRO 4500/6000, both Server Edition on AWS)
  into isolated slices. Which GPU families/generations actually support
  MIG vs. MPS, verified against live hardware rather than assumed from
  datasheets: [`gpu-sharing-support-matrix.md`](gpu-sharing-support-matrix.md).
- **MPS trusted-tenant sharing** (`internal/mps`) — cooperative
  multi-process sharing for cards without MIG (g6/g6e's L4/L40S), for a
  known/bounded population rather than arbitrary tenants. Gated behind its
  own `--i-understand-shared-gpu-has-no-isolation` consent flag, distinct
  from the ordinary spend gate, since the risk (no hardware isolation) is
  a different kind than "this costs money."

## The check-out/check-in lifecycle (`calque session`)

Both MIG slices and MPS client-slots are checked in/out via `calque
session` on an instance someone else already acquired — `session` never
launches or terminates an EC2 instance itself. Why this verb is named
`session` (and not the older N-item ramp verb, which was renamed to
`calque ramp` to free the name up), and the full check-out/check-in
lifecycle design: [`tenancy-vs-session.md`](tenancy-vs-session.md).

## How the layers fit together

Institutional sharing sits on top of two more foundational layers, not
beside them:

- **Layer 0**: `internal/plan.Acquirer` — the only thing that ever calls
  AWS to acquire/release an EC2 instance.
- **Layer 1 (M12 — idle fleet)**: `internal/pool.Worker` — decides WHICH
  model a pool of instances stays warm with, and HOW MANY instances that
  pool holds.
- **Layer 2 (M13 — institutional tenancy)**: `internal/tenancy.Registry` +
  `internal/mig`/`internal/mps` — decides how many CONCURRENT USERS occupy
  slices WITHIN one instance Layer 1 (or Layer 0 directly, for a
  non-pooled dedicated run) has already handed over. Never calls AWS.

Full layering statement and why it's drawn exactly there:
[`m12-m13-boundary.md`](m12-m13-boundary.md).

## Full flag/subcommand reference

Every flag for `calque pool`/`calque session` exactly as the code accepts
them: [`guide/cli-reference.md`](guide/cli-reference.md). Which command
fits your situation: [`guide/which-verb.md`](guide/which-verb.md).
