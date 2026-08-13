# Real-world corpus (calque#79 corpus-expansion pass)

**Status:** living index. This directory previously vendored real,
production-shaped Modal scripts fetched from GitHub to pressure-test calque
beyond AI-Almanac's original 3 scripts (calque#79). The vendored copies have
since been **removed** — several carried no license grant at all (just an
"Origin:" attribution comment), and one (`alphafast_af3_predict.py`,
RomeroLab/alphafast) was CC-BY-NC-SA 4.0, which is not compatible with
redistributing the file inside this Apache-2.0 repo. This file keeps the
triage record (what was found, which issue it produced) without the code
itself — every finding below is independently reproducible by fetching the
cited upstream URL and running it through `calque analyze`/`calque run
--dry-run` locally (no vendoring required).

Every script was run through `calque analyze <script>` and `calque run
--dry-run --n 10 <script>` (both zero-cost, zero-AWS-call — static parsing
only, per calque#79's own established methodology).

## Provenance note: how this corpus was sourced

**GitHub's code-search backend (`mcp__github__search_code` and the raw
`GET /search/code` REST endpoint) was unavailable for the entire session** —
every query returned a populated `total_count` but an always-empty `items`
array, and unthrottled retries returned `503 too many shards unavailable`.
This was confirmed on trivial, guaranteed-to-match queries too (e.g.
`addClass repo:jquery/jquery`), ruling out a query-syntax problem. Repeated
over several minutes with backoff; never recovered during this session.

**Fallback used instead:** GitHub's *repository* search API
(`GET /search/repositories`, topic/README/description queries) plus direct
`get_file_contents` browsing of promising repos' file trees. This is a real
degradation from the planned strategy (`"import modal" "@app.cls"` /
`.spawn(` / `modal.Volume` / `modal.Sandbox` code-search queries, each
`-repo:modal-labs/modal-examples`) — repository search finds repos ABOUT
Modal (by topic tag, README mention, description), not scripts that USE a
specific Modal construct, so this corpus is biased toward whatever
repository-level signal surfaced a plausible candidate, not toward any
particular idiom. No `modal.Sandbox` example was found this way (repository
search surfaced no evidence for it one way or the other — this is a "didn't
find," not a "confirmed absent").

## Scripts

| Origin (owner/repo, path) | Shape | Triage outcome |
|---|---|---|
| `dcalaprice/modal-sqlcoder`, `sql_generation_inference.py` | GPU inference serve (TGI subprocess server inside a `@cls`), pre-1.0 API | **NEW finding** — [calque#138](https://github.com/spore-host/calque/issues/138): `__enter__`/`__exit__` dunder lifecycle (Modal's pre-`@modal.enter()` API) is completely unrecognized; calque refuses the script outright (`no mapped @cls+@enter warm unit found`) |
| `JacobLinCool/modal-vibevoice`, `app.py` | GPU serve (`@cls`+`@enter`+`@asgi_app` on the same class) + batch (`.remote()`-callable methods), current-era API | Covered — hits already-tracked gaps only: `gpu=` non-literal (env-var-driven), `App(image=)` app-level default leak, `.run_function()` non-literal arg (all existing, generic leaks). Multiple entrypoints correctly detected (requires `--entrypoint`, per calque#90) |
| `mharrvic/fast-audio-video-transcribe-with-whisper-and-modal`, `api/main.py` | Serve (`@asgi_app` FastAPI factory) + batch (`.starmap()` with `kwargs=`), legacy `Stub`/`NetworkFileSystem`/`Dict.new()`/`keep_warm=` API | Covered — `network_file_systems=` unmodeled-arg leak and `keep_warm=` autoscaling leak both already generic/tracked; `.starmap()` not selected as warm unit in this script so calque#83's refusal path isn't exercised. Module-level-helper NameError on dry-run is the new #139 finding (see below), not specific to this script |
| `darothen/ai-models-for-all`, `ai-models-modal/app.py` + `main.py` | Batch GPU weather-forecast pipeline (plain functions + one `@cls` with custom `__init__`/`__enter__` — NOT `@modal.enter()`), legacy `Stub`/`NetworkFileSystem` API, real `.local()` chaining | Covered — same shape category as AI-Almanac's `forecasts_app.py` (calque#79's original finding). `.local()` call correctly recognized+leaked. Hits the **NEW #139 finding** (module-level helper/constant NameError) directly: `REPO_ROOT_IMAGE`-style bare references fail dry-run |
| `billy-enrizky/model-serving`, `multi-token-prediction/deploy/modal/modal_app.py` | Serve (`@asgi_app` + `@modal.concurrent(max_inputs=1)` on a plain `@app.function`) + two batch one-shot functions (bench harness, weight-warming), current-era API, hyphenated GPU spec strings assembled dynamically | Covered — every leak maps to existing tracked gaps (`gpu=` non-literal, `add_local_dir` staging leak, autoscaling kwargs, secrets not injected, volume `.commit()` mid-run-visibility gap). Directly hits the **NEW #139 finding** (`REPO_ROOT_IMAGE` module-level constant NameError) |
| `caru-ini/modal-comfyui`, `comfyui.py` | GPU serve (`@modal.web_server` raw port) with memory-snapshot support (`enable_memory_snapshot`, two `@modal.enter(snap=True/False)`), current-era API | **NEW finding** — [calque#140](https://github.com/spore-host/calque/issues/140): image built across multiple reassigning statements (`image = image.env(...)`, later `image = image.add_local_dir(...)`) loses every earlier layer including the base; dry-run also crashes on a module-level helper (`wait_for_port`, the #139 pattern) inside `@enter` |
| `arihanv/fasthtml-modal`, `deploy.py` | Minimal serve (`@asgi_app` wrapping a third-party, non-Modal `fasthtml_app` object imported from a sibling module) | Covered by the analyze pass (zero leaks — genuinely clean); directly hits the **NEW #139 finding** on dry-run (`fasthtml_app` is a bare imported name, not a `.local()` call, so it's out of `collectLocalExtras`'s scope entirely) |

## Scripts, pass 2 (calque#150 torture-test — corpus expansion beyond AI-Almanac)

Sourced via `gh api search/code` (working again this pass, unlike pass 1's
documented GitHub code-search outage) plus direct `contents` fetches,
targeting idioms neither AI-Almanac nor pass 1 exercised: multi-app
composition, `**kwargs`-splat decorator args, `@modal.experimental.clustered`
multi-node training, `modal.Dict`/`modal.Queue` used for real hot-path
coordination (not just declared), and a function defined inside a factory
function rather than at module scope. Full triage/ranking/predicted-findings
detail lives in [calque#150](https://github.com/spore-host/calque/issues/150).

| Origin (owner/repo, path) | Shape | Triage outcome |
|---|---|---|
| `earth-mover/forecast-datacube-demo`, `modal_hrrr.py` + `src/modal_app.py` + `src/lib_modal.py` | Batch, multi-app composition (`app.include(applib)`), every `@applib.function`/`@app.function` splats its ENTIRE kwarg set from a module dict (`**MODAL_FUNCTION_KWARGS`), `modal.Cron(...)` at real production frequency | Covered — `**kwargs`-splat is handled correctly (`_decorator_kwargs`'s existing `kw.arg is None` branch emits an honest "decorator uses **kwargs splat; args not statically visible" leak, confirmed NOT a bug); `App.include` hits the existing calque#91 leak; `Cron` hits the existing calque#91 leak. `--dry-run` fails fast on a real (non-calque) missing `dask` import — expected, this pipeline needs its full real dependency chain to execute |
| `RomeroLab/alphafast`, `modal/af3_predict.py` + `modal/config.py` (CC-BY-NC-SA 4.0 — never redistributed here) | `@cls`+`@modal.enter()`+plain-`@modal.method()` GPU inference at production scale, `modal.Dict`/`Queue.from_name()` as real hot-path data transfer, non-literal bare-Name `gpu=` | **NEW finding** — [calque#151](https://github.com/spore-host/calque/issues/151): a bare reference to a module constant holding `modal.Dict`/`Queue`/`NetworkFileSystem.from_name(...)` ships verbatim (calque#139's free-reference shipping) and crashes at runtime with a confusing Modal-SDK auth error, instead of an honest leak — confirmed via a minimal synthetic repro, not just this script. `InferenceWorker.warmup` (the method that never references the Dict/Queue) dry-runs cleanly with 0 failed items once a `python` shim is on PATH (its `@enter` body shells to a literal `"python"` — a real dependency on the AlphaFast container's own environment, not a calque bug) — this was calque#150 Part E's real-AWS target, later run successfully on real AWS (see calque#150's closing comment): `0 failed, 1/1 collected, result: "warm"` |
| `tokenbender/avataRL`, `modal_train.py` | Multi-node distributed training, `@modal.experimental.clustered(n_nodes, rdma=)` stacked under `@app.function(gpu=...)`, f-string multi-GPU `gpu=` specs including an upgrade-pin+multi-GPU compound case (`f"H100!:{n_proc_per_node}"`) | **NEW finding** — [calque#152](https://github.com/spore-host/calque/issues/152): `@modal.experimental.clustered` has zero recognition anywhere in calque; a literal single-GPU `gpu=` stacked with `@modal.experimental.clustered(size=N)` passes the §7 coupling guard as `clean_swap`, silently missing that the workload is genuinely multi-node. Every occurrence in THIS script happens to also have a non-literal `gpu=` f-string masking the gap behind an existing leak — a synthetic repro was needed to isolate and confirm it. Parse/analyze only; no real-AWS attempt (needs actual multi-GPU H100/H200 nodes) |
| `phospho-app/phosphobot`, `modal/vllm/app.py` | GPU serve, THREE decorators stacked on one function (`@app.function`+`@modal.concurrent(max_inputs=)`+`@modal.web_server(port=, startup_timeout=)`) | Covered — serve-shape detection (`leaves & _SERVE_DECOS`) correctly classifies this as serve despite the 3-way stack; refused gracefully per the documented non-goal, no new gap. `gpu=f"A100-80GB:{N_GPU}"` hits the existing non-literal-gpu leak |
| `slaf-project/slaf`, `slaf/ml/distributed.py` | `@app.function(..., serialized=True, name=...)` decorating `distributed_prefetch_worker`, defined INSIDE the `create_app()` factory function rather than at module scope; `modal.Queue`/`Dict.from_name()` called inline inside the function body for real cross-worker coordination | Covered — confirmed pyast's recursive AST walk DOES find a function nested inside a factory function (was an open question in calque#150's plan); `distributed_prefetch_worker` is correctly picked as the warm unit, and its bare `import modal` reference is correctly shipped via calque#146. `--dry-run` fails fast on a real (non-calque) missing `slaf` import — expected, not attempted to resolve since real execution needs the actual `slaf` package + a live Modal Queue backend |

## Aggregate triage summary

- **7 scripts examined** (pass 1), all real/production-shaped (real
  `requirements.txt`/`pyproject.toml`+`uv.lock`, no `-demo`/`-tutorial`/
  `-example` naming, actual application logic rather than a bare
  hello-world).
- **Mix achieved**: 2 pure-batch, 3 serve+batch mixed, 2 pure-serve — a
  materially better serve-shape representation than AI-Almanac's original 3
  (all batch).
- **3 genuinely new findings**, filed as calque#138/#139/#140. None was a
  crash in calque itself — every one surfaced as an honest (if sometimes
  generic) leak or refusal, consistent with the project's "recognize and
  leak, never silently drop" discipline. #139 in particular recurred
  independently across 3 of the 7 scripts (`ai-models-for-all`,
  `model-serving`, `fasthtml-modal`), which is why it's flagged as the
  highest-leverage of the three.
- **Everything else** — GPU non-literal-spec leaks, autoscaling-kwarg leaks,
  secrets-not-injected leaks, `add_local_*` staging leaks, serve-shape
  detection, multiple-entrypoint handling, Volume `.commit()`/`.reload()`
  semantics — mapped cleanly onto rows already tracked in
  `docs/modal-compatibility-matrix.md` or issues already closed in prior
  passes (calque#76 through #98). No inflation: most of what these real
  scripts exercise, calque already handles or already knows it doesn't.

## Aggregate triage summary, pass 2 (calque#150)

- **5 scripts examined**, all real/production-shaped, deliberately targeting
  idioms neither AI-Almanac nor pass 1 exercised (multi-app composition,
  `**kwargs`-splat decorators, `@modal.experimental.clustered`, real
  Dict/Queue coordination, a function nested inside a factory function).
- **2 genuinely new findings**, filed as calque#151/#152 — both confirmed
  via a minimal synthetic repro before filing (per calque#150's Part A
  decision gate), not just inferred from the real-world script alone, since
  in both cases the real script's own occurrence happened to be masked by
  an unrelated, already-known leak (a non-literal `gpu=` in avataRL's case;
  `process_chunk` — the method that actually uses the Dict/Queue — being
  out of this pass's real-AWS scope in alphafast's case). Neither is a
  crash in calque itself — #151 is a confusing runtime error instead of an
  honest leak; #152 is a silent incorrect verdict (the more serious of the
  two, since §7's whole design premise is "never silently substitute across
  a coupling signal").
- **2 open questions from the plan resolved, both cleanly**: `**kwargs`-splat
  decorator args are already handled correctly (`_decorator_kwargs`'s
  existing `kw.arg is None` branch) — the plan's top-ranked predicted
  finding did NOT confirm as a bug. A function nested inside a factory
  function (`slaf`) IS correctly found and parsed by pyast's recursive AST
  walk — also not a bug.
- **Both #151 and #152 are now fixed** (see calque#150's closing comment) —
  #151's fix, and its real-AWS validation against `alphafast`'s
  `InferenceWorker.warmup`, is the sole real-AWS target this corpus
  produced.

## Re-running this corpus

Fetch each script fresh from its origin URL rather than vendoring a local
copy, then run it through calque's zero-cost static passes:

```
curl -sL <raw-origin-url> -o /tmp/corpus-script.py
go run ./cmd/calque analyze /tmp/corpus-script.py
go run ./cmd/calque run --dry-run --n 10 /tmp/corpus-script.py
```

The `vibevoice_asr.py`-shaped script (JacobLinCool/modal-vibevoice) has 3
`@app.local_entrypoint()`s and requires `--entrypoint main|bench|long` to
select one for the `run` (not `analyze`) path. Scripts without a `@cls`+
recognized-`@enter` warm unit (the `modal-sqlcoder`/`fast-audio-video-...`
shapes) will refuse `run --dry-run` outright (the calque#138 finding, and
the serve-only-no-batch-unit case respectively) — that refusal is itself the
expected/documented outcome for those shapes, not a bug in this harness.
