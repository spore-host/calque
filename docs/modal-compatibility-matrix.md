# Modal compatibility matrix

**Status:** living reference document. calque's goal is broad enough Modal-idiom
mimicry that real Modal code ports to AWS **unchanged**. This document is the
single most direct answer to "does calque support my script."

This document merges three research passes (2026-08-07):

1. **calque's current state** — a full read of `tools/pyast/pyast.py`,
   `internal/parse/parse.go`, `internal/ir/ir.go`, `internal/gpu/gpu.go`,
   `internal/gate/*.go`, `internal/image/dockerfile.go`, `cmd/calque/run.go`, and
   the existing `behind-the-seam-register.md`.
2. **Modal's documented API surface** — from `modal.com/docs` (guide + SDK
   reference).
3. **Real-world frequency** — from `modal-labs/modal-examples` (212 files) plus
   ~20 independent production repos found via GitHub code search.
4. **Modal's CLI surface** — from `modal.com/docs/reference/cli/*`, compared
   against calque's current CLI (`analyze`, `run`, `smoke`, `real`, `session`).

Update this table as gaps close or Modal's docs change — that's cheaper than
re-deriving the survey every time a new adopter's script surfaces a "new" gap.

**Frequency tiers** (Pass 3, rough real-world prevalence): 🔥 very common (used in
most apps or nearly universal) · 🟡 common (a first-class, recurring use case) ·
⚪ minority (recurring but not dominant) · 🧊 rare (a handful of real examples).

**calque status legend:** ✅ fully supported (parsed, represented, acted on) ·
🟨 recognized-and-leaked (detected, deliberately not honored) · ❌ silently
dropped/buggy (a real gap — should not stay this way) · ⬜ not present at all.

---

## A. App / module-level constructs

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `modal.App(name, ...)` | Deployment/namespace unit; functions don't run just by being deployed. | 🔥 | ✅ `pyast.py:238-240` → `ir.App.Name` | None known. | — |
| `App(image=..., secrets=..., volumes=...)` (app-level defaults) | Inherited by every Function/Cls unless overridden. | ⚪ | 🟨 `pyast.py:242-245` — recorded as a helper leak, never wired into `ir.App.Image` etc. | A function relying on the app-level default (not setting its own `image=`) silently gets no image at all in calque today. | not yet filed |
| `@app.function(...)` | Registers an independently-autoscaled serverless function pool. | 🔥 | ✅ (as config carrier) / see §F for execution-shape gap | — | — |
| `@app.cls(...)` | Same kwarg surface as `.function`, plus lifecycle hooks + method pooling. | 🔥 | ✅ | — | — |
| `@app.local_entrypoint(name=None)` | Runs **locally**, not in a container; kicks off `.remote()`/`.map()` calls. Multiple entrypoints need `modal run file.py::fn`. | 🔥 | ✅ (fixed 2026-08-07, calque#78 — multiple entrypoints in one script all preserved) | — | closed |
| `@app.server(...)` | Registers HTTP-only server classes; no `.remote()` support. | 🧊 | ⬜ | — | not yet filed (low priority) |
| `App.include(...)` / `.deploy()` / `.run()` | Multi-app composition, deploy strategies, ephemeral-vs-deployed lifecycle. | ⚪ | ⬜ | calque has no concept of "deployed" vs. "ephemeral" — its execution model is closer to always-ephemeral. Real scripts calling `App.run(detach=True)` won't be recognized. | [#91](https://github.com/spore-host/calque/issues/91) |
| `modal.Stub` (deprecated → hard error since Modal 1.0) | Old name for `App`. | 🧊 (legacy only) | ⬜ | Any script still using this is already broken against current Modal — not calque's problem. | — |

---

## B. Decorators (function/class shape)

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `@app.function(...)` on a plain function | The base serverless unit. | 🔥 (2x as prevalent as `@app.cls`, 118 vs 57 files in modal-examples) | ✅ (fixed 2026-08-07, calque#80 — `pickWarmUnit` now selects a plain function, preferring a `.map()`'d one, else the first, when no `@cls`+`@enter` unit exists; leaks that no warm-reuse economics exist to amortize; verified end-to-end against all three real AI-Almanac scripts, which previously refused outright) | Was blocking 100% of real scripts surveyed; now runs (leaks surface the OTHER already-tracked gaps instead — image resolution, cpu/memory sizing, secrets, etc.). | closed |
| `@app.cls(...)` + `@modal.enter()` (no `.map()`) | Holds loaded state (model, DB connection) for `.remote()`-called or web-endpoint-served inference. | 🟡 (very common in GPU-serving apps, frequently paired with `@asgi_app` rather than `.map()`) | ✅ recognized, but `pickWarmUnit` only *selects* it as the runnable shape when a `.map()`'d method exists or falls back to "first method" — a `@cls`+`@enter` used purely for `.remote()` calls (no `.map()` at all) is still selected via the fallback, so this mostly works today. | — | — |
| `@app.cls` + `@modal.enter()` + `.map()` (calque's original target shape — a plain `@app.function` is also runnable, see §A above) | "Load model once, batch-score many." | ⚪ (~5-10% of files; plain-function `.map()` is at least as common even among `.map()` users — 16/26 vs 10/26 files) | ✅ | This is not the dominant shape it was built around — see §A row above. | — |
| `@modal.enter(snap=False)` | Runs once per container at startup, before any input. `snap=True` marks pre-snapshot code (see §I memory snapshots). | 🔥 (wherever `@cls` is used) | ✅ body carried as `ir.Class.EnterBody`, actually run once by `warmd`. `snap=` kwarg itself unrecognized (falls through to generic "unmodeled arg"). | Memory-snapshot semantics (`snap=True` vs default) aren't distinguished — low-risk since calque doesn't do container snapshotting at all. | — |
| `@modal.exit()` | Runs on container shutdown; gets a grace period on preemption specifically for cleanup. | 🟡 (paired with `@enter` wherever teardown matters) | ✅ (fixed 2026-08-07, calque#86) recognized in `visit_ClassDef`, excluded from `cls.Methods` (confirmed via a live repro: before the fix, an exit-only class was picked as the warm unit's sole method by `pickWarmUnit`'s fallback; after the fix, `run()` correctly refuses with "no mapped @cls+@enter warm unit found" instead). Teardown itself is leaked as unreproduced (`ir.Class.HasExit`). | The warm supervisor has no shutdown-hook concept — teardown logic itself still doesn't run, just no longer silently misclassified. | closed |
| `@modal.method(is_generator=None)` | Converts an instance method into an invokable Modal Function scoped to the class. | 🔥 (wherever `@cls` is used) | ✅ | `is_generator=` kwarg not specifically recognized. | — |
| `@modal.batched(max_batch_size=, wait_ms=)` | Dynamic input batching; all inputs/outputs must be equal-length lists; at most one batched method per class. | 🧊 (2/212 files in modal-examples; not found in independent-repo sample) | ⬜ | — | [#91](https://github.com/spore-host/calque/issues/91) |
| `@modal.concurrent(max_inputs=, target_inputs=None)` | **Replaces the deprecated `allow_concurrent_inputs=N` kwarg** (v0.73.148) — now a separate decorator, not a function kwarg. Sync functions get separate OS threads (must be thread-safe); async get coroutines on one thread. | 🟡 (a common tuning pattern in production) | ✅ (fixed 2026-08-07, calque#82) `_describe_fn` already captured every decorator's kwargs on a plain function, so `max_inputs`/`target_inputs` just needed adding to `autoscalingKwargs`. The class-level case needed a real fix: `visit_ClassDef` only read `@app.cls`'s OWN kwargs — a separate `@modal.concurrent(...)` stacked on the same class was invisible; now merged into `cls_kwargs`. | — | closed |
| `@app.batched`/`max_batch_size=` | See `@modal.batched` above (same construct, different framing in the original audit). | 🧊 | ⬜ | — | dup of above |
| Web decorators: `@modal.fastapi_endpoint` (renamed from `@modal.web_endpoint`, v0.73.89), `@modal.asgi_app()`, `@modal.wsgi_app()`, `@modal.web_server(port)` | Long-lived, request-driven, no fixed N, autoscaling-driven termination — fundamentally different execution model from batch `.map()`. | 🟡 (a first-class use case — own top-level directory in modal-examples, ~19% of files) | 🟨 detected via `_SERVE_DECOS` (matches both old and new decorator names by trailing attribute), sets `entry_kind: "serve"`; `run.go` refuses gracefully with a leak, the long-lived server is never built (by design — see `docs/serve-architecture.md`). | Working as intended per the project's own documented scope decision. | — |

---

## C. Function/class config kwargs

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `gpu=` (single string, e.g. `"H100"`) | Card selection. | 🔥 | ✅ drives the actual clean-swap/flag-multi/flag-couple decision. | — | — |
| `gpu=` newer type strings: `L40S`, `H200`, `B200`, `B300` (space-separated forms) | Card types added since calque's `gpu.go` was written. | 🟡 (growing as newer hardware ships) | ✅ verified 2026-08-07 (calque#85) — `internal/gpu/gpu.go`'s card field is an opaque string by design (no local validation), and truffle's `find.ResolveCard` already resolves all four to correct instance families (`H200`→p5e, `L40S`→g6e, `B200`→p6-b200/p6e-gb200, `B300`→p6-b300) — confirmed live, no gap here. | — | closed (no fix needed) |
| `gpu=` **hyphenated/suffixed** spec strings: `RTX-PRO-6000`, `RTX-PRO-4500`, `A100-80GB`, `H100!`, `B200+` | Modal's own documented spec-string convention uses hyphens for multi-word names and `!`/`+` suffixes for upgrade-pin/opt-in behavior. | 🟡 | ❌ **upstream truffle bug, not calque's.** Confirmed live: `find.ResolveCard("RTX-PRO-6000")` and `("A100-80GB")` both fail to resolve (`resolved to no GPU`) even though the space-separated forms (`"RTX PRO 6000"`, `"A100 80GB"`) work — truffle's tokenizer splits on whitespace only, so a hyphenated multi-word card arrives as one unmatched token. Notably affects calque's OWN default target card: calque's `StubRecommender` defaults to RTX PRO 6000, but a real script spelling it Modal's actual way (`gpu="RTX-PRO-6000"`) would fail to resolve. | Filed upstream: [truffle#130](https://github.com/spore-host/truffle/issues/130). | tracked upstream |
| `gpu="L40"` (distinct from `L40S`) | L40 (plain) and L40S are physically distinct NVIDIA chips — AWS's `g6` (L40) vs `g6e` (L40S) instance families reflect the distinction directly, not a naming variant. | ⚪ | ❌ **upstream truffle bug, not calque's.** Confirmed live: `find.ResolveCard("L40")` incorrectly resolves to `g6e.*` (L40S's family) via an alias (`"l40": "l40s"`) — a caller asking for the cheaper/different L40 chip silently gets routed to L40S instead, with no signal anything was substituted. | Filed upstream: [truffle#129](https://github.com/spore-host/truffle/issues/129). | tracked upstream |
| `gpu="H100:8"` (multi-GPU) | >1 card, same physical machine (NVLink-class). | ⚪ | ✅ `FlagMulti` → refuses (by design, §7 guard). | — | — |
| `gpu=["H100", "A100-40GB:2"]` (fallback-list syntax) | Modal tries types in list order. | 🧊 | ✅ (fixed 2026-08-07, calque#85) `readConfigKwargs` now decodes the list, takes the first (highest-preference) entry as `gpu=`, and leaks that the try-in-order-until-available semantic isn't reproduced (no live availability probe at parse time) — instead of the generic "not a plain string literal" message. | calque picks statically; it doesn't probe live availability the way Modal's real fallback does. | closed |
| `cpu=` (plain number or `(request, limit)` tuple) | Physical cores; tuple limit is a throttle, not OOM-kill. | 🔥 | ✅ (fixed 2026-08-07, calque#77 — tuple form now leaks the dropped limit, mirroring `memory=`) | Recorded but not used for instance sizing (deliberate, behind the seam). | closed |
| `memory=` (plain int MB or tuple) | MiB; tuple limit is a **hard OOM-kill** ceiling (different failure mode than CPU's throttle). | 🔥 | ✅ recorded+leaked correctly. | Same sizing-deferred caveat as `cpu=`. | — |
| `retries=` (plain int or `modal.Retries(...)`) | Per-input retry cap; plain int = fixed delay, object = exponential backoff. | ⚪ | ✅ (plain int) wired into the warm supervisor's crash-restart cap — a genuine reliability knob that's honored. `Retries(...)` object form: recognized+leaked (falls back to default cap). | Exponential-backoff semantics not reproduced even when leaked — acceptable per behind-the-seam scope. | — |
| `secrets=` | List of `Secret` objects, injected as env vars in list order (later overrides earlier on key clash). | 🔥 | 🟨 recorded, explicitly NOT injected — leaked clearly ("a payload needing them will fail"). | Working as intended per documented scope. | — |
| `schedule=` (bare cron string) | — | ⚪ | 🟨 recorded, not honored, leaked. | See §H — the *object* forms (`modal.Cron`/`modal.Period`) aren't recognized at all, only a bare string kwarg. | [#91](https://github.com/spore-host/calque/issues/91) |
| `region=` / `cloud=` | Placement hints. | ⚪ | 🟨 both recorded+leaked (`cloud=` fixed 2026-08-07, calque#91 — mirrors `region=`'s pattern exactly: `ir.Config.Cloud`, dedicated "recorded but NOT honored" leak). | calque always targets AWS regardless of `cloud=`'s value — recorded for visibility, not acted on (a script requesting GCP/OCI isn't rejected, just silently run against AWS anyway, same posture as every other portable-but-unhonored kwarg). | closed |
| Autoscaling kwargs, old spellings: `concurrency_limit`, `allow_concurrent_inputs`, `min_containers`, `max_containers`, `keep_warm`, `container_idle_timeout` | Warm-pool/scaling config. | 🟡 | 🟨 explicit named set (`autoscalingKwargs` in `internal/parse/parse.go`), each gets a dedicated "behind the seam" leak. | — | — |
| Autoscaling kwargs, **current spellings**: `min_containers` (was `keep_warm`), `max_containers` (was `concurrency_limit`), `scaledown_window` (was `container_idle_timeout`), `buffer_containers` | Same knobs, renamed at Modal 1.0 (v0.73.76). | 🟡 (real scripts will use EITHER era depending on when they were written) | ✅ (fixed 2026-08-07, calque#82) `scaledown_window`/`buffer_containers` added to `autoscalingKwargs`; `min_containers`/`max_containers` were already present from an earlier pass. | — | closed |
| `image=<var>` | Per-function image override. | 🔥 | ✅ structural no-op at the kwarg level; resolved at the app level (`resolveImage`). Dangling refs (e.g. built via a factory function) now loudly leaked (fixed 2026-08-07, calque#76). | — | closed |
| Non-literal / `**kwargs` splat on any decorator | — | 🧊 | 🟨 generic fallback leak — the safety net; nothing silently dropped at this layer. | — | — |

---

## D. Image DSL

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `Image.debian_slim()` | Default base. | 🔥 | ✅ | — | — |
| `Image.from_registry(...)` | Pull an existing image; `linux/amd64` required. | 🟡 | ✅ | — | — |
| `Image.from_dockerfile(...)` | Direct Dockerfile ingestion. | ⚪ | 🟨 (fixed 2026-08-07, calque#84) `resolveBase` now names the specific unstaged local path in the leak (e.g. `from_dockerfile("./Dockerfile.custom"): calque does not read/stage this local Dockerfile`) instead of the generic "unknown image base" message. Still defaults to CUDA — calque can't read/inline an arbitrary local Dockerfile's content, same limitation as `add_local_*`. | Leak is now specific and actionable; the underlying default-base substitution is unavoidable without local-file staging (a separate, bigger capability). | closed |
| `Image.from_aws_ecr(...)` | Pull from ECR; `secret=` carries IAM/OIDC. | ⚪ | ✅ + emits an `integration_edge` leak noting IAM pull-permission needs. | — | — |
| `Image.from_gcp_artifact_registry(...)` | GCP equivalent. | 🧊 | ⬜ | — | not yet filed (low priority) |
| `Image.micromamba()` | Conda-alternative base. | 🧊 | ✅ (fixed 2026-08-07, calque#84) resolves to `mambaorg/micromamba:latest` (the closest stock equivalent) instead of silently defaulting to CUDA/debian, with a leak noting kwargs like `python_version=` aren't captured by `_walk_image_chain`'s positional-only arg collection. | No GPU/CUDA variant of this base exists — a GPU payload built on it needs its own CUDA install, same limitation Modal's own `micromamba()` base has. | closed |
| `.pip_install(...)` / `.uv_pip_install(...)` | Package install layers. | 🔥 | ✅ | — | — |
| `.pip_install_from_requirements(...)` / `.poetry_install_from_file(...)` | File-based install. | ⚪ | ✅ (with a leaked caveat: calque doesn't stage the local file into the build context). | — | — |
| `.apt_install(...)` / `.run_commands(...)` / `.dockerfile_commands(...)` / `.env(...)` / `.workdir(...)` / `.entrypoint(...)` | Standard Dockerfile-equivalent verbs. | 🔥 | ✅ | — | — |
| `.add_local_dir/.add_local_file/.add_local_python_source(...)` (renamed from `.copy_local_dir`/`.copy_local_file`/`Mount.from_local_python_packages`, v0.66.40-v0.67.28) | Ship local source into the image. | 🔥 | 🟨 renders a `COPY` line but leaks that calque doesn't stage the local path — build fails unless the caller stages it themselves. | Old names (`copy_local_dir` etc.) aren't in calque's `_IMAGE_STEPS` set at all — a pre-1.0 script using them silently fails the "is this an image chain" heuristic if that's the only step present. | not yet filed (verify old-name coverage) |
| `modal.Mount` (fully removed at Modal 1.0; `mount=`/`context_mount=`/`copy_mount` all gone) | — | 🧊 (legacy only) | ⬜ | Scripts predating 1.0 may still use this — not worth building for, but worth a dedicated "this construct was removed upstream" leak if seen, rather than a generic one. | not yet filed (low priority) |
| `.run_function(fn)` | Runs arbitrary Python at build time on a full remote worker (GPU/volumes/secrets available). | 🧊 | 🟨 explicitly not reproduced — leaked, no Dockerfile line emitted. | Correct given no direct AWS-build-time equivalent. | — |
| Unknown/other `.method(...)` step | — | 🧊 | 🟨 catch-all: Dockerfile comment + leak. | — | — |

---

## E. Volumes / Storage

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `modal.Volume.from_name(...)` + `volumes={mount: vol}` | **Not a live shared filesystem** — snapshot-at-container-start, explicit `.commit()`/`.reload()` for cross-container visibility, last-write-wins on concurrent same-file writes (documented, expected data loss). | 🔥 | ✅ maps to a deterministic S3 prefix, real delta-sync before `@enter`, real end-of-run commit write-back. | calque's model (sync-before-run, commit-after-run) matches Modal's snapshot-at-start semantics reasonably well for the common case; **mid-run `.reload()`** (re-sync during execution) is correctly leaked as unreproduced. | — |
| `.commit()` / `.reload()` call sites | End-of-run persistence / mid-run re-read. | 🔥 (wherever Volumes are used) | ✅ `.commit()` honored as real end-of-run write-back. 🟨 `.reload()` leaked as unreproduced. | — | — |
| `modal.NetworkFileSystem` (deprecated, being removed) | **Live-shared** filesystem — no commit/reload cycle, closer to EFS/NFS than Volume's snapshot model. | 🧊 (deprecated, Modal steers users to Volume) | ⬜ | If a real script still uses this, calque's Volume→S3-prefix mapping is the WRONG model (S3 has no live-shared-write semantics) — this would need an EFS-shaped mapping instead, not a Volume-shaped one. | [#91](https://github.com/spore-host/calque/issues/91) |
| `modal.CloudBucketMount` | Direct S3/R2/GCS mount via `mountpoint-s3` — no append writes, no seek+write, must open in truncate mode, no rename. | 🧊 | ⬜ | A script using this directly against real S3 is a DIFFERENT (and more restrictive) primitive than Volume — calque's Volume mapping doesn't cover it. | [#91](https://github.com/spore-host/calque/issues/91) |
| `modal.Dict` | Distributed KV store, cloudpickle values, 7-day inactivity TTL, capped `.len()` at 100,000. | 🧊 | ⬜ | — | [#91](https://github.com/spore-host/calque/issues/91) |
| `modal.Queue` | FIFO **per-partition only**, 24h partition auto-expiry. | 🧊 | ⬜ | — | [#91](https://github.com/spore-host/calque/issues/91) |

---

## F. Invocation idioms

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `.map(iterable, order_outputs=True, return_exceptions=False)` | **`order_outputs=True` by default** — results returned in input order, not completion order (Modal buffers internally). Hard cap 1000 concurrent inputs/call. | ⚪ (real minority even among `.map()` users vs. plain-function `.map()`, but still calque's core supported shape) | ✅ highest-precedence idiom, drives `pickWarmUnit` selection and the actual warm-runner execution. | calque's own spec (§10) already flags the ordering-at-scale question as a leak to watch — this confirms it's a real, documented Modal contract to replicate, not a hypothetical concern. | — |
| `.starmap(iterable_of_tuples)` | Same as `.map` but tuple-splat args. | 🧊 | ✅ (fixed 2026-08-07, calque#83) if a `.starmap`'d function is ever the SELECTED warm unit, `run()` now refuses with a clear reason instead of silently running it as `.map()` — confirmed via a live repro that doing so crashed every item with a raw `NameError` and no indication why. | Refuses rather than supports; full tuple-splat execution would need the worker protocol to bind multiple args, a bigger change not attempted here. | closed ([#93](https://github.com/spore-host/calque/issues/93) tracks full support) |
| `.for_each(iterable, ignore_exceptions=False)` | Side-effect-only, no result collection, but still blocks until all complete. | 🧊 | ✅ (fixed 2026-08-07, calque#83) shares `.map`'s single-arg signature, so it runs correctly through the warm unit — the only mismatch (Modal discards the result, calque collects it) is now leaked explicitly rather than silently unhonored. | Milder than `.starmap`: nothing crashes, so this is a leak, not a refusal. | closed |
| `.remote(*args, **kwargs)` | Single blocking call. | 🔥 | ✅ shares `.map`'s exact single-arg, collect-a-result signature — no actual execution mismatch exists (confirmed while fixing calque#83; the original framing of this as a gap was incorrect). calque already drives N synthetic items regardless of how many times the script itself calls `.remote()`. | — | closed (no fix needed) |
| `.local(*args, **kwargs)` | Runs in the CALLER's own process/container — no new container, only locally-available resources apply. | 🧊 (rare — mostly same-class intra-container calls or entrypoint-local testing, NOT general pipeline chaining) | ✅ (fixed 2026-08-08, calque#92) a `.local()`-referenced plain `@app.function` sibling is now resolved transitively and SHIPPED alongside the picked warm unit's body (not just leaked) — both `helper(x)` and `helper.local(x)` call-site styles work unmodified. A `.local()` call resolving to a `@cls` method is deliberately left unsupported (would need its own warm `@enter` state) and still leaks honestly instead of NameError-ing silently. Verified against a fixture mirroring `blending_app.py`'s real chaining shape. | Real-AWS execution paths (`real`/`session`/`fleetrun`) still drive hardcoded reference bodies rather than an arbitrary parsed script — this fix applies to `--dry-run` only so far. | [#81](https://github.com/spore-host/calque/issues/81) (recognition) + [#92](https://github.com/spore-host/calque/issues/92) (shipping, both closed) |
| `.spawn(*args, **kwargs)` → `FunctionCall` handle | Non-blocking; `FunctionCall.object_id` is a persistable string, reconstructable via `.from_id()` from a different process; results retrievable for 7 days post-completion. | ⚪ (~7% of files; common at web/bot/CLI boundaries — "trigger and poll") | 🟨 (fixed 2026-08-08, calque#97) the block-and-wait fan-out driver now exists (`cmd/calque/spawnrun.go`) and is live-verified on real AWS: every `.spawn()`'d callable found via `ir.App.FindFunction`/`FindClass` gets its own shard (own `EnterBody`/`MethodBody`), acquired and run in parallel, collected via a string-keyed collector, with one re-drive on failure. | calque still doesn't reproduce Modal's real decoupled contract (persistable handle, 7-day retention, cross-process `.get()`) — confirmed as explicitly out of scope, matching §18. The driver is block-and-wait fan-out only, by design. | [#88](https://github.com/spore-host/calque/issues/88) (classification) + [#97](https://github.com/spore-host/calque/issues/97) (driver, both closed) |
| `.spawn_map(*input_iterators)` | Fire-all without waiting; even Modal itself has no clean in-SDK result-collection API for this yet. | 🧊 | ⬜ | — | not yet filed (low priority — even upstream is unfinished here) |
| `Function.from_name(...)` / `Cls.from_name(...)` (cross-app invocation) | Look up an already-deployed Function/Cls by name from a separate app/process. | ⚪ in curated examples (~5%) but **structurally essential in real external-consumer production code** — anything outside the defining app must use this. | 🟨 (fixed 2026-08-07, calque#87) recognized and leaked distinctly, naming the looked-up app/object when the args are plain string literals (e.g. `Function.from_name("almanac-blending", "score_live_forecast_bundle")`) — verified against the real `forecasts_app.py` call site. Carefully guarded to Function/Cls specifically so `Volume.from_name`/`Secret.from_name` (unrelated constructs sharing the same method name) aren't misclassified as cross-app invocation — confirmed this guard was NEEDED via a live false-positive against the same real script before narrowing it. | Recognition-only; actually orchestrating a call into a separately-deployed app is a real design gap calque doesn't have a story for yet (needs a persistent-deployment concept — see §M). | [#87](https://github.com/spore-host/calque/issues/87) closed (recognition); full orchestration remains a design gap, not re-filed separately since #87's own body already scoped this as future work |
| `.map.aio(...)` / other `.aio` async variants | Coroutine variant of any blocking method. | ⚪ | 🟨 `.map.aio`/`.starmap.aio` detected, leaked as deferred (same bucket as `.spawn`). Other `.aio` variants (`.remote.aio`, `.get.aio`, etc.) — not specifically detected. | — | not yet filed (low priority, narrow) |

---

## G. Execution shape / entrypoints (calque's own dispatch logic)

| Construct | calque behavior today | Frequency of the underlying real shape | Risk / gap |
|---|---|---|---|
| `@cls`+`@enter`+`.map()`'d method | The only shape `pickWarmUnit` (`cmd/calque/run.go`) selects without a fallback heuristic. | ⚪ minority (~5-10% of real scripts) | Working as designed, but the design targets a minority shape. |
| `@cls`+`@enter`, no `.map()`'d method | Falls back to "first method" — runnable, but an arbitrary pick if there's real ambiguity; no leak for this specific fallback. | 🟡 common | Acceptable for now; could use a leak noting the fallback was used. |
| `@cls`, no `@enter` | Skipped entirely as a warm-unit candidate; separately leaked ("@cls has no @enter"). | 🧊 | Correct — a class with no warm-load-once body genuinely doesn't fit the model. |
| Plain `@app.function`, no `@cls` anywhere | ✅ (closed, calque#80) `pickWarmUnit` selects the `.map()`'d function if any, else the first, wrapping it in a synthesized zero-value `ir.Class` so `dryRunWarm`'s existing `unit.class.*` reads need no changes. Also fixed `swapLegal` to accept `gpu.NoGPU` (a plain CPU function) — it previously treated "no gpu= declared" as an illegal swap, identical to a flagged multi-GPU/coupled one, invisible until a GPU-free plain function became reachable. | 🔥 **the most common real shape**, and the one that was blocking every AI-Almanac script | Verified against `testdata/scripts/plain_function.py` and a fresh clone of all three real AI-Almanac scripts — all now run past warm-unit selection. |
| Serve-shaped app, no batch warm unit | Detected, leaked as deferred, Bedrock route-away still runs, returns cleanly (no error). | 🟡 | Working as designed — documented non-goal (`docs/serve-architecture.md`). |

---

## H. Scheduling

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `schedule=` bare string kwarg | — | ⚪ | 🟨 recorded, leaked as unhonored (no scheduler in the spike). | — | — |
| `modal.Cron(cron_string, timezone=None)` | Standard 5-field cron; requires the app to be **deployed** to activate (no effect on ephemeral runs). | ⚪ (~5% of files; when present, OFTEN the entire app — e.g. earth-mover/forecast-datacube-demo uses only plain functions + Cron) | ⬜ Not recognized as a distinct construct — only the bare-string `schedule=` kwarg case is handled, and `modal.Cron(...)` is a call expression, not a literal, so it likely falls into the "unmodeled decorator arg" fallback rather than the dedicated `schedule=` leak. | Real scheduled-pipeline apps (a real, recurring shape per Pass 3) get a worse leak than intended. | [#91](https://github.com/spore-host/calque/issues/91) |
| `modal.Period(days=, hours=, ...)` | Fixed-interval, **deployment-anchored, not wall-clock-anchored** — resets on every redeploy. | 🧊 | ⬜ same gap as `modal.Cron`. | — | [#91](https://github.com/spore-host/calque/issues/91) |

---

## I. Networking / Web

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `@modal.fastapi_endpoint` / `@modal.asgi_app` / `@modal.wsgi_app` / `@modal.web_server` | Long-lived, request-driven, autoscaling-terminated — see §B. | 🟡 | 🟨 detected, deferred by design. | — | — |
| `modal.forward(port, unencrypted=False)` (Tunnels) | Exposes a live container TCP port publicly. | 🧊 | ⬜ | — | not yet filed (low priority) |
| `modal.Proxy` | Static outbound IP; must be provisioned via Dashboard first, referenced via `.from_name`. | 🧊 | ⬜ | — | not yet filed (low priority) |
| `cloud=` / `region=` / `routing_region=` | See §C. | ⚪ | 🟨/⬜ partial — see §C row. | — | see §C |

---

## J. Sandboxes

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `modal.Sandbox.create(...)` / `.exec()` / `.terminate()` | **Fundamentally different execution model**: a long-lived, explicitly-managed container (create once, `exec()` repeatedly), no autoscaling/warm-pool abstraction at all — closer to `asyncio.subprocess.Process` than to a Function pool. | 🟡 and **growing** — concentrated in modal-examples but disproportionately common in independent agent/code-execution products (Anthropic, OpenAI, LangChain, PostHog, HF smolagents all use it) | ⬜ **Not present at all.** | Flagged as a real, growing gap — but deliberately NOT attempted in this pass. It's a different execution model from everything calque does today (batch-warm-unit or request-driven-serve); needs its own design pass, not a bolt-on. | [#89](https://github.com/spore-host/calque/issues/89) |

---

## K. Secrets

| Construct | Modal semantics | Frequency | calque status | Behavior difference / risk | Tracking |
|---|---|---|---|---|---|
| `secrets=[Secret.from_name(...), ...]` | List of Secret objects, injected as env vars, list-order precedence. | 🔥 | 🟨 see §C — recorded, not injected, clearly leaked. | Working as designed. | — |
| `Secret.from_dict(...)` / `Secret.from_dotenv(...)` / `Secret.from_local_environ(...)` | Alternate construction forms. | ⚪ | ⬜ (not distinguished from the generic `secrets=` case — all become `{"__unparsed__": ...}` markers at the AST layer) | Low risk — the effect (not injected, leaked) is the same regardless of construction form. | — |

---

## L. Multi-node / clustered (explicit non-goal, confirmed real)

| Construct | Modal semantics | Frequency | calque status |
|---|---|---|---|
| `@modal.experimental.clustered(size=N, rdma=False)` | Gang-scheduled multi-node; whole-cluster restart on any single-node preemption. | 🧊 (Beta) | 🟨 the underlying signal (multi-GPU `gpu="H100:8"` + coupling-signal body regex) is exactly what calque's §7 guard flags as `FlagCouple`/`FlagMulti` and refuses. Confirms the guard's target is real and documented — no further action needed; this is explicitly out of scope by design (§1). |

---

## M. CLI surface

Modal's `modal` CLI has a core local-dev triad (`run`/`deploy`/`serve`) plus
`shell` for interactive debugging, and a long tail of remote resource
management (`secret`/`volume`/`nfs`/`environment`) and observability
(`app logs`/`container exec`) commands that are about administering Modal's
own control plane, not about running code — out of scope for a tool whose job
is porting *code*, not managing a Modal workspace.

| Modal command | Purpose | calque equivalent | Gap |
|---|---|---|---|
| `modal run <file>[::entrypoint] [args]` | Ephemeral run; `::entrypoint` selects which `@app.local_entrypoint()` to invoke when a file has several; passes through arbitrary CLI args to it. | `calque run [--n N] [--region R] [--dry-run] [--entrypoint NAME] <script.py>` | 🟨 (fixed 2026-08-07, calque#90) `--entrypoint <name>` validates against `app.Entrypoints`, auto-selects when there's exactly one, and requires explicit selection when 2+ exist (mirroring Modal's own "ambiguous, pick one" posture) — verified via live repros for all four cases (none/one/many/wrong-name). Does NOT yet steer which callable `pickWarmUnit` selects ([#98](https://github.com/spore-host/calque/issues/98)) or pass through arguments — both confirmed gaps, not silent ones. |
| `modal deploy <file>` | Publishes a persistent app (survives disconnect); `--strategy rolling\|recreate`. | none | Legitimate scope difference — AWS has no equivalent to Modal's redeploy-in-place model; calque's execution is closer to always-ephemeral. Not a gap to close, just a documented difference. |
| `modal serve <file>` | Hot-reload dev server for web endpoints. | none | Follows from calque not building the long-lived server at all (documented non-goal, `docs/serve-architecture.md`) — consistent, not a new gap. |
| `modal shell [ref]` | Interactive shell inside a container matching a function's image/mounts/volumes, or attaching to a live sandbox. | none | No calque equivalent for interactively debugging a port — worth considering once basic execution-shape gaps (backlog #1-#7) are closed, since debugging-the-port is exactly what an adopter mid-migration needs. |
| `modal secret`/`volume`/`nfs`/`environment`/`app`/`container`/`profile`/`config`/`token` | Remote resource management, observability, auth. | none | Out of scope by design — these manage Modal's own control plane; calque's job is running code, not administering a Modal workspace. |

---

## Kwarg/API rename table (Modal 1.0 migration, and earlier)

Real scripts in the wild will contain **either era** of spelling depending on
when they were written. calque should recognize both, routing both to the same
dedicated leak/behavior rather than letting the newer spelling fall through to
a generic "unmodeled arg" message.

| Old name | Current name | Introduced | calque recognizes old? | calque recognizes new? |
|---|---|---|---|---|
| `keep_warm` | `min_containers` | v0.73.76 | ✅ | ❌ |
| `concurrency_limit` | `max_containers` | v0.73.76 | ✅ | ❌ |
| `container_idle_timeout` | `scaledown_window` | v0.73.76 | ✅ | ❌ |
| `_experimental_buffer_containers` | `buffer_containers` | — | ❌ | ❌ |
| `allow_concurrent_inputs=N` (kwarg) | `@modal.concurrent(max_inputs=N)` (decorator) | v0.73.148 | ❌ | ❌ |
| `max_inputs` (old: cap before recycle) | `single_use_containers=True` | — | ❌ | ❌ |
| `modal.gpu.H100()` (object API) | `gpu="H100"` (string API) | v0.73.31 | n/a — calque only ever supported the string form | — |
| `.lookup()` | `.from_name()` | v0.72.56 | n/a — calque doesn't call this API itself | — |
| `.resolve()` | `.hydrate()` | v0.72.39 | n/a | — |
| `modal.web_endpoint` | `modal.fastapi_endpoint` | v0.73.89 | ✅ (matched by trailing decorator name, catches both) | ✅ |
| `Image.copy_local_dir`/`copy_local_file` | `Image.add_local_dir`/`add_local_file` | v0.66.40 | ❌ (not in `_IMAGE_STEPS`) | ✅ |
| `Mount.from_local_python_packages` | `Image.add_local_python_source` | v0.67.28 | n/a (Mount never supported) | ✅ |
| `modal.Mount` / `mount=` / `context_mount=` / `Image.copy_mount` | `add_local_*` + auto context inference | removed at v1.0 | ⬜ never supported | — |
| `@modal.build` | `modal.Volume` or `Image.run_function` | v0.72.17 | n/a | ✅ (`run_function` supported, leaked) |
| Custom `Cls.__init__` | `modal.parameter()` + `@modal.enter` | v0.74.0 | ⬜ `modal.parameter()` not recognized at all | — |
| `modal.Stub` | `modal.App` | hard error since v1.0 | n/a — scripts using this are already broken upstream | — |

---

## Prioritized backlog (sequenced by Pass 3 frequency, highest-leverage first)

1. ~~**Plain `@app.function` as a runnable warm unit**~~ — closed the actual
   blocker in calque#79. [#80](https://github.com/spore-host/calque/issues/80) (closed 2026-08-07)
2. ~~**`.local()` recognition**~~ — closed. [#81](https://github.com/spore-host/calque/issues/81)
   (closed 2026-08-07); full body-inlining tracked separately as
   [#92](https://github.com/spore-host/calque/issues/92).
3. ~~**Newer autoscaling-kwarg spellings + `@modal.concurrent`**~~ — closed.
   [#82](https://github.com/spore-host/calque/issues/82) (closed 2026-08-07)
4. ~~**`.starmap`/`.for_each`/`.remote` execution parity**~~ — closed.
   [#83](https://github.com/spore-host/calque/issues/83) (closed 2026-08-07);
   full `.starmap` tuple-splat execution (vs. today's safe refusal) needs a
   real input-data-reading prerequisite, tracked separately as
   [#93](https://github.com/spore-host/calque/issues/93).
5. ~~**`Image.micromamba()`/`from_dockerfile()` base-resolution bug**~~ — closed.
   [#84](https://github.com/spore-host/calque/issues/84) (closed 2026-08-07)
6. ~~**GPU spec string + fallback-list coverage**~~ — closed.
   [#85](https://github.com/spore-host/calque/issues/85) (closed 2026-08-07).
   `gpu=[...]` list syntax fixed in calque; space-separated newer type
   strings (`L40S`/`H200`/`B200`/`B300`) already worked via truffle. Found
   two real upstream truffle bugs along the way (hyphenated/suffixed spec
   strings failing entirely; `L40`/`L40S` conflation) — filed as
   [truffle#129](https://github.com/spore-host/truffle/issues/129) and
   [truffle#130](https://github.com/spore-host/truffle/issues/130).
7. ~~**`@modal.exit()` recognition**~~ — closed.
   [#86](https://github.com/spore-host/calque/issues/86) (closed 2026-08-07)
8. ~~**`Function.from_name`/`Cls.from_name` cross-app invocation**~~ —
   recognition closed. [#87](https://github.com/spore-host/calque/issues/87)
   (closed 2026-08-07). Actually orchestrating a call into a separately-
   deployed app is a real design gap that remains — needs a persistent-
   deployment concept calque doesn't have yet (§M).
9. ~~**`.spawn()`+`.get()` classification**~~ — closed.
   [#88](https://github.com/spore-host/calque/issues/88) (closed 2026-08-07).
   The actual fan-out driver (related to calque's existing
   `internal/exec/shard.go` fan-out, keyed by callable identity rather than
   item index) is tracked separately as
   [#97](https://github.com/spore-host/calque/issues/97) — needs real-AWS
   verification, no offline test path.
10. **`modal.Sandbox`** — tracked, explicitly deferred; different execution
    model entirely. [#89](https://github.com/spore-host/calque/issues/89)
11. ~~**`calque run --entrypoint <name>`**~~ — closed (validate+inform
    scope). [#90](https://github.com/spore-host/calque/issues/90) (closed
    2026-08-07). Confirmed via a live repro that `--entrypoint` doesn't yet
    STEER which callable `pickWarmUnit` selects (no call-site-to-entrypoint
    attribution exists) — tracked as
    [#98](https://github.com/spore-host/calque/issues/98). Argument
    passthrough also not attempted.
12. Lower-priority/rare: `modal.Dict`/`Queue`, `@modal.batched`,
    `modal.NetworkFileSystem`, `modal.CloudBucketMount`, `modal.Cron`/`Period`
    object forms, `cloud=`, `App.include`/`.deploy`/`.run` lifecycle nuances.
    [#91](https://github.com/spore-host/calque/issues/91)

Not individually filed (genuinely low-priority/narrow; revisit if real usage
surfaces): `@app.server`, `App.include`-equivalent lifecycle nuances beyond
what #91 covers, `Image.from_gcp_artifact_registry`, old-name coverage for
`.copy_local_dir`/`.copy_local_file`, `modal.Mount` (removed upstream too),
`.spawn_map` (unfinished even in Modal itself), other `.aio` variants beyond
`.map.aio`/`.starmap.aio`, `modal.forward`/`modal.Proxy`, `modal shell`-
equivalent interactive debugging.
