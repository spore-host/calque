# docs

An index of what each design doc actually answers — start here rather than
guessing from filenames.

- [`modal-compatibility-matrix.md`](modal-compatibility-matrix.md) — does
  calque support MY script? A construct-by-construct census of Modal's real
  API surface against calque's current behavior, sourced from Modal's own
  docs and a corpus of real-world usage, not a hand-picked list.
- [`porting-modal-to-aws.md`](porting-modal-to-aws.md) — I have a working
  Modal app, what do I change to run it on AWS? Task-oriented, built around
  running `calque analyze` and reading its real output.
- [`behind-the-seam-register.md`](behind-the-seam-register.md) — what calque
  deliberately does NOT port (autoscaling, async futures, secrets injection,
  mid-run volume reload, real card selection, ...) and the attach point a
  future build would touch for each.
- [`serve-architecture.md`](serve-architecture.md) — why long-lived
  request-driven entrypoints (`@web_endpoint`/`@asgi_app`/etc.) are detected
  and leaked but the server itself isn't built.
- [`tenancy-vs-session.md`](tenancy-vs-session.md) — why `calque session`
  (the acquire-once/hold/run-many verb) was renamed `calque ramp` (shipped,
  #117), and what the new `calque session` (multi-tenant MIG/MPS slice
  check-out/check-in, shipped) actually means.
- [`gpu-sharing-support-matrix.md`](gpu-sharing-support-matrix.md) — which
  GPU families/generations actually support MIG vs. MPS, verified against
  live hardware rather than assumed from datasheets.
- [`pool-queue-contract.md`](pool-queue-contract.md) — how `calque pool`'s
  sticky-worker queue keeps a warm process resident across claims instead of
  reloading a model per claim.
- [`m12-m13-boundary.md`](m12-m13-boundary.md) — where the idle-fleet layer
  (M12: which/how many whole instances are warm) ends and the
  institutional-tenancy layer (M13: how many concurrent users share one
  instance) begins.
- [`substrate-offline-test-tier.md`](substrate-offline-test-tier.md) — how
  calque tests AWS request/response handling offline (no spend, no mocks)
  using Substrate as a real-wire-protocol AWS emulator.

Project tracking otherwise lives on GitHub, not in local files:

- **Decisions & environment facts** → issues labeled `meta:decision` and the milestone descriptions.
- **Progress** → the GitHub Project board + milestones.
- **spore.host integration deltas** → issues filed upstream on the truffle / lagotto / spawn repos,
  cross-referenced from calque issues labeled `area:spore-integration`.
- **Runtime leak reports (§10)** are emitted by `internal/leak` at run time — reproduced, not committed.
