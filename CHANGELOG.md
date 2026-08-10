# Changelog

All notable changes to **calque** are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
— read `0.x` as "the supported contract can still shift release to release,"
per [semver.org](https://semver.org/#spec-item-4).

## [Unreleased]

### Fixed

- **calque now passes a script's real requested GPU card to truffle**
  (calque#134): `target.StubRecommender.Recommend` used to ignore its `fn
  ir.Function` argument entirely and always return the hardcoded
  `DefaultCard` constant ("RTX PRO 6000") — every script's `gpu=` request,
  whatever it asked for, silently resolved to the same g7e instance. It now
  carries `fn.GPU` through (falling back to `DefaultCard` only when no
  `gpu=` was declared at all), and every `cmd/calque` real-AWS call site
  that used to hardcode `target.DefaultCard` now calls `Recommend` on its
  parsed script's warm unit when one was parsed (`--script`), unchanged
  otherwise. `internal/plan.TruffleResolver.Resolve` also now normalizes
  Modal's documented `-80GB`/`-40GB` memory-suffix spelling (e.g.
  `gpu="A100-80GB"`) before the bare card fails to resolve via truffle
  (truffle#130) — this closes calque's own side of that gap independent of
  truffle's fix, leaking when the normalization fires. Resolving to an
  instance family with no live-verified MIG/MPS sharing-mode entry
  (`docs/gpu-sharing-support-matrix.md` covers g6/g6e/g7/g7e only) now
  emits an informational leak in `plan.FillTarget` rather than staying
  silent — the run still succeeds; this is a documentation gap, not an
  error.

### Added

- **`.starmap()` tuple-splat execution** (calque#93): a `.starmap()`'d warm
  unit now actually RUNS instead of refusing, when the script's real iterable
  was statically resolved at parse time (calque#136) — the warm runner
  (`worker/warm-runner/runner.py`) binds every one of the callable's
  positional params and splats each item's tuple (`fn(self.state, *payload)`)
  rather than binding only the first arg. `checkInvokeSupport`
  (`cmd/calque/run.go`) narrows its refusal to the one case that's still
  genuinely unsafe: a `.starmap`'d unit with no statically-resolvable
  iterable at all (nothing real to splat).

## [0.1.0] - 2026-08-09

First versioned release ([#61](https://github.com/spore-host/calque/issues/61)).
calque is still a **spike**: it exists to prove Modal-shaped GPU code runs
unchanged on AWS, not to be a finished product. This tag marks the point
where the supported-idiom surface and the recognize-and-leak discipline
around it are stable enough to be worth naming.

### Supported today

- **Parse → IR → seam** (`tools/pyast` → `internal/parse` → `internal/ir`):
  a six-primitive transcription of a Modal script's decorators; bodies are
  carried verbatim as payload, never interpreted.
- **Two runnable warm-unit shapes**: `@app.cls` + `@modal.enter()` +
  `.map()`'d method (the original target shape), and a plain `@app.function`
  with no `@cls` at all (~2x as prevalent in real Modal code) — both drive
  the same warm worker (`warmd` + `runner.py`, §6): `@enter` runs once,
  crash-restart re-drives unfinished items, partial per-item failure never
  reloads the model.
- **`.local()`-chained sibling functions**: a plain `@app.function` calling
  another via `.local()` (in-container pipeline chaining, confirmed on a real
  external adopter's script) is resolved transitively and shipped alongside
  the picked warm unit — not just leaked. A `.local()` call into a `@cls`
  method is left unsupported and leaked honestly instead.
- **`gpu=` swap guard** (§7): clean-swap vs. multi-GPU/coupled — a flagged
  swap refuses rather than silently mis-substituting.
- **Bedrock route-away gate** (§11): a model that's already an exact Bedrock
  API call short-circuits **before** a GPU is acquired.
- **Idiom pass-through** (§C): image DSL verbs, portable config kwargs
  (`cpu=`/`memory=`/`retries=`/`secrets=`/`schedule=`/`region=`/`cloud=`),
  synchronous invocation kinds `.map`/`.starmap`/`.for_each`/`.remote`,
  `.spawn()` classified + block-and-wait fan-out driver, cross-app
  `Function.from_name`/`Cls.from_name` recognized-and-leaked.
- **Acquisition**: block-and-wait single-target capacity acquire via
  `lagotto/pkg/snipe.Snipe` (AZ-sweep, capacity retry/backoff, deadline).
- **Volumes** (§3/§15): `Volume.from_name` → S3 prefix, delta-synced before
  `@enter`; `volume.commit()` persisted as an end-of-run write-back.
- **Multi-instance `.map()` fan-out** (§15): shard N items across S
  independently-acquired instances, collect the ordered union, re-drive one
  dead shard.
- **Tenancy**: fixed-layout MIG slice provisioning and MPS trusted-tenant
  sharing for non-MIG cards, plus sticky-worker pool mode (`calque real
  --pool`) keeping a warm process resident across claims.

### Deliberately not built (recognized-and-leaked, not silently dropped)

Serve entrypoints (`@web_endpoint`/`@asgi_app`/`@wsgi_app`/`@web_server`),
autoscaling kwargs (`concurrency_limit`, `keep_warm`, `min`/`max_containers`),
multi-GPU/coupled `gpu=` swaps, mid-run `Volume.reload()`/`.commit()`, and
everything else tracked in
[`docs/behind-the-seam-register.md`](docs/behind-the-seam-register.md) and
[`docs/modal-compatibility-matrix.md`](docs/modal-compatibility-matrix.md).
The discipline: if a script uses an idiom calque doesn't act on, it emits a
structured leak naming the gap — never a silent drop or a mysterious crash.

### Known gaps

- Real-AWS execution paths (`calque real`/`session`/`fleetrun`) still drive
  hardcoded reference bodies rather than an arbitrary parsed script's picked
  unit for some features (e.g. `.local()` sibling shipping is dry-run-only
  today, [#92](https://github.com/spore-host/calque/issues/92)).
- N=100k multi-instance rung not yet run at real scale
  ([#18](https://github.com/spore-host/calque/issues/18)).
