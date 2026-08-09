# Porting a Modal app to AWS with calque (calque#133)

`docs/modal-compatibility-matrix.md` answers "does calque support construct X, in
general" — a construct-by-construct census. This doc answers the question someone
arriving with a *working Modal app* actually has: "here's my app, what do I need to
change to run it on AWS?"

**Status note:** the ✅/⚠️/🔧/❌ statuses below are hand-classified in this guide's own
table, not something `calque analyze` prints itself. `analyze` reports each script's
own findings as free text (see the next section for real output); this guide reads
that output and tells you what it means.

## 1. Run `calque analyze` on your script

```
./calque analyze your_app.py
```

This runs every static pass (parse → gpu guard → Bedrock gate → volume mapping →
leak report) with **zero AWS calls and zero cost**. Real output, from
`testdata/scripts/portable_config.py` (a fixture exercising several portable-config
idioms at once):

```
=== portable_config.py (app "portable-config") ===
  functions=5 classes=0 entrypoints=2 image.base="debian_slim" pip=[numpy]
  gpu[transform]: no_gpu requested="" (no gpu= declared)
  gpu[gpu_fallback]: clean_swap requested="H100" -> RTX PRO 6000 (single-card, no coupling signal; memory-bound B=1 substitution is legal)

--- leak report (§10) ---
LEAKS: 10 emitted across 4 primitives
  entrypoint (7):
    - [semantic_gap] transform: region= placement hint recorded but NOT honored (acquisition sweeps offered AZs) (portable_config.py:41)
    - [semantic_gap] transform: cloud= recorded but NOT honored (calque always targets AWS) (portable_config.py:41)
    - [semantic_gap] scaled: autoscaling/warm-pool config "max_inputs"=4 — belongs to the real brain behind the seam (§4)...
  gpu (1):
    - [semantic_gap] gpu_fallback: gpu=[H100 A100-40GB:2] fallback-list — using first preference "H100"...
  map (1):
    - [semantic_gap] bin_pack.local(...): runs inline in the caller's own container...
```

Every line is a `[kind] owner: detail (file:line)` leak — read them individually,
each one is a self-contained explanation of one specific thing calque either
translated, recorded-but-didn't-act-on, or refused. Zero leaks means the script
matches calque's execution shape exactly with nothing to flag.

## 2. What your Modal code uses → what happens on AWS → what you do

| Your Modal code | On AWS through calque | Status | What you do |
|---|---|---|---|
| `gpu="H100"` | Swapped to the one supported card (`RTX PRO 6000`) via the `gpu[fn]: clean_swap` line. | ✅ | Nothing, for single-card B=1 workloads. `gpu=[...]` fallback-lists pick the first entry only (no live-availability probe). Multi-GPU (`"H100:8"`) and coupled/tensor-parallel bodies are refused outright (🛑 `flag_multi`/`flag_couple`) — out of single-node scope, not silently downgraded. |
| `Secret.from_name(...)` in `secrets=[...]` | Recorded in the leak report (`secrets={"__unparsed__": "[api_key]"}`), never injected as an env var. | 🔧 | You provide the equivalent secret yourself — an AWS Secrets Manager entry, SSM parameter, or a plain env var baked into the image/launch config — and reference it from your script's body directly, since calque's worker won't inject it for you. |
| `Volume.from_name(...)` + `volumes={mount: vol}` | Maps to a deterministic S3 prefix, delta-synced to the mount path before `@enter`, committed back after the run (`volume: "name" -> s3://.../ (mount ..., delta-sync => warm-cache reuse)`). | ✅ | Nothing for the common case — sync-before-run/commit-after-run matches Modal's snapshot-at-start semantics closely enough that repeated runs reuse cached weights automatically. Mid-run `.reload()`/`.commit()` (re-syncing WHILE a container runs) isn't reproduced — restructure to load once, at start. |
| `Image.add_local_dir()`/`.add_local_file()`/`.add_local_python_source()` | A `COPY` line is rendered into the generated Dockerfile. | 🔧 | **You must stage the local path yourself** before the image builds — calque doesn't copy your local source tree into the build context automatically. Old pre-1.0 names (`copy_local_dir` etc.) aren't recognized at all; rename to the current API first. |
| App-level `image=`/`secrets=`/`volumes=` defaults (set once on `modal.App(...)`, inherited by every function) | Recorded as a leak, never applied to any function that doesn't set its own. | ❌ | **Make every function's config explicit.** A function relying on the app-level default silently gets no image/secrets/volumes at all today — this is the sharpest gap in this table; don't assume inheritance works. |
| `.map(iterable)` | Native — this is calque's core supported shape (`@cls`+`@enter`, or a plain `@app.function`, driven through the warm supervisor). | ✅ | Nothing. This is the shape calque was built around. |
| `.spawn()` + `.get()` | A real block-and-wait fan-out driver (`calque spawn-run`) exists and is live-verified on AWS — each spawned callable gets its own shard, run in parallel, collected by ID. | ⚠️ | Understand the semantic gap: calque's driver is **block-and-wait**, not Modal's real decoupled contract (a persistable `FunctionCall` handle, reconstructable via `.from_id()` from a different process, 7-day result retention). If your script depends on retrieving results from a genuinely separate later process, that's not reproduced. |
| `Function.from_name(...).remote(...)` (cross-app invocation) | Recognized and leaked, naming the looked-up app/object — never executed. | ❌ | This is a **permanent, decided non-goal** (calque#137), not a pending gap: calque runs exactly one script's IR per invocation and has no path to locate a separately-deployed app's source. If your app depends on calling into another deployed app, that call won't run — restructure so both halves are reachable from one script, or run them as two independent calque invocations that you glue together yourself. |
| `@web_endpoint`/`@asgi_app`/`@wsgi_app` (serve entrypoints) | Detected and leaked as a deferred execution shape; a *served* model that's also an exact Bedrock match still routes away before any GPU is touched. | ❌ | The long-lived HTTP server itself isn't built. If your script's only entrypoint is a web endpoint (no `.map()`/batch shape underneath it), `calque run` has nothing to execute — see `docs/serve-architecture.md`. |
| `cpu=`/`memory=` (plain value or `(request, limit)` tuple) | Recorded in `ir.Config`, printed in leaks, **never used to size the actual instance**. | 🔧 | Pick your own instance type explicitly via `--instance` on whichever run command you use; calque won't right-size for you (deliberate — the real sizing "brain" is behind the seam, `internal/target`). |

## 3. Shapes calque actually runs today

- `@app.cls` + `@modal.enter()` + `.map()` — the shape calque was originally built
  around: load once, batch-score many.
- A plain `@app.function`, with or without `.map()` — supported as a fallback
  (calque#80); no `@enter` means no once-per-container load to amortize, but the
  function still runs.
- `.spawn()` + `.get()` fan-out over multiple *distinct* callables in the **same**
  script — via `calque spawn-run` (calque#97).

If your script's structure doesn't match one of these (e.g. it's built entirely
on cross-app calls, or a `.local()`-chained pipeline of plain functions with no
`.map()` anywhere), run `calque analyze` first — the leak report will tell you
exactly which shape it fell into and why.

## 4. Shapes it recognizes but refuses, or doesn't recognize at all

See `docs/modal-compatibility-matrix.md`'s legend (✅/🛑/🟨/❌/⬜) for the full
construct-by-construct census — in particular the 🛑 rows (multi-GPU, `.starmap()`)
where refusing loudly is the *correct* behavior, not a gap. `docs/behind-the-seam-register.md`
lists every deliberate non-goal and its attach point, for anything you find missing
that turns out to be intentional rather than unbuilt.

## 5. Where to file a gap

If `calque analyze` on your real script surfaces something not covered above or in
the compatibility matrix, check the matrix's own "Tracking" column first — it links
an open issue for most known gaps. If nothing matches, that's a new finding worth
filing against [spore-host/calque](https://github.com/spore-host/calque/issues) with
your script's exact `analyze` output attached.
