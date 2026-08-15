# examples

Seven journeys that show calque's whole story: what it supports cleanly, what
it deliberately refuses, and what real execution looks like. The first six
cost **nothing** — no AWS instance is launched by anything through §6. The
seventh is different and clearly marked: it's a **recorded transcript** of a
real, billable AWS run, included because real execution is one of calque's
strongest proof points and deserves showing, not just describing in a
changelog. Each journey is a real command with its actual (abbreviated)
output.

> These `.py` files are byte-for-byte copies of the canonical fixtures in
> `testdata/scripts/` (which the parser tests and the spike spec reference by
> path). A guard test (`examples_test.go`) fails if a copy ever drifts. Edit the
> `testdata/` original, then re-copy — never edit a copy in isolation.

**Setup once** (from the repo root):

```
go build -o calque ./cmd/calque   # control plane
cd tools/pyast && uv sync          # Python AST helper deps, then cd back
```

The commands below assume `./calque` on your path and are run from the repo root.
`analyze`/`run` reach two best-effort network sources (the Bedrock catalog and the
`hf-bedrock-map` API); offline they print a `warn:` line and fall back — a
networkless run is not a failure.

---

## 1. Analyze a batch workload — no AWS, no dry-run

The canonical shape calque supports today: a `.map()` fan-out over prompts, a
`@cls` + `@enter` that loads the model once (warm), weights in a `Volume`.

```
./calque analyze examples/map_batch_inference.py
```

```
=== map_batch_inference.py (app "map-batch-inference") ===
  functions=0 classes=1 entrypoints=1 image.default.base="debian_slim" pip=[vllm==0.6.3 transformers==4.45.2 huggingface_hub]
  gpu[Batcher]: clean_swap requested="H100" -> RTX PRO 6000 (single-card, no coupling signal; memory-bound B=1 substitution is legal)
  volume: "weights" -> volumes/weights/ (mount /weights, delta-sync => warm-cache reuse)

--- Bedrock eligibility gate (§11) ---
  map_batch_inference.py: identity hidden (no repo id; inference shape) — cannot claim Bedrock match
...
--- leak report (§10) ---
LEAKS: 1 emitted across 1 primitives
  volume (1):
    - [semantic_gap] Batcher: model identity obscured (loaded from a path/mount, not a repo id); Bedrock identity check cannot run
```

What to notice: the `gpu=H100` → `RTX PRO 6000` substitution is judged **legal**
(single-card, no coupling); the model's identity is loaded from a mount, so the
Bedrock check honestly reports it **cannot** run rather than guessing.

---

## 2. Dry-run the full pipeline — the unchanged script, end to end

`run` defaults to `--dry-run`: it exercises every stage — parse, gate, plan, warm
execution, collect — against the unchanged script, **without launching a billable
instance.**

```
./calque run --n 20 --dry-run examples/map_batch_inference.py
```

```
parsed "map-batch-inference": 1 classes, 0 functions
entrypoint: main (selected)
warm unit: class "Batcher", method "generate", gpu asked-for "H100"
recommend+resolve: card="RTX PRO 6000" -> instance="g7e.2xlarge"
image: Dockerfile rendered, digest=ae6b11f861e02f4b (tag for ECR cache)

[DRY-RUN] not launching a billable instance; driving warm worker locally on a synthetic sample
[DRY-RUN] warm unit ran 20 items, 0 failed; @enter x1 (0.305s), mean 0.0531s/item
...
--- leak report (§10) ---
LEAKS: 2 emitted across 2 primitives
  volume (1):
    - [semantic_gap] Batcher: model identity obscured (loaded from a path/mount, not a repo id); Bedrock identity check cannot run
```

What to notice: the SAME script — same `@cls`, same `@enter`, same `.map()` — ran
through every real pipeline stage (parse → gate → plan → warm-execute → collect)
with zero code changes, using only a stand-in body locally since no GPU is involved.
`calque real`/`calque session` run this identical script's real body on a real
acquired instance.

---

## 3. Exact Bedrock route-away — don't rent a GPU at all

If a script self-hosts a model that's already served on Bedrock, the honest answer
is *call the API*. calque detects this and, on the runnable path, stops **before**
acquiring anything.

```
./calque analyze examples/bedrock_eligible.py
```

```
--- Bedrock eligibility gate (§11) ---
  bedrock_eligible.py: SUGGEST BEDROCK: meta-llama/Meta-Llama-3-8B-Instruct is served as meta.llama3-8b-instruct-v1:0 (foundation-model, confirmed) — don't rent a GPU.
        invoke: bedrock-runtime invoke-model --model-id meta.llama3-8b-instruct-v1:0 --region us-east-1
        Bedrock regions: us-east-1, us-west-2
        evidence: hf-bedrock-map v1: meta.llama3-8b-instruct-v1:0 (foundation-model, confirmed)
```

What to notice: the offer is **actionable** — the exact `modelId` and a concrete
region to invoke. (This journey needs network to reach the Bedrock map; offline
the gate falls back to a signature heuristic and won't make an exact claim.)

---

## 4. An unsupported workload — the graceful refusal

calque supports single-node GPU work. Multi-GPU and tensor-parallel/coupled
workloads are **out of scope — and it says so**, rather than silently doing the
wrong substitution.

```
./calque analyze examples/multi_gpu_train.py
```

```
  gpu[finetune]: flag_multi requested="H100:8" (requests >1 GPU (H100:8); multi-GPU is out of single-node scope)
  gpu[Sharded]: flag_couple requested="A100" (body shows coupling signal "tensor_parallel"; coupled/tensor-parallel is out of scope)
...
--- leak report (§10) ---
LEAKS: 4 emitted across 2 primitives
  gpu (2):
    - [semantic_gap] finetune: requests >1 GPU (H100:8); multi-GPU is out of single-node scope — NOT substituted
    - [semantic_gap] Sharded: body shows coupling signal "tensor_parallel"; coupled/tensor-parallel is out of scope — NOT substituted
```

What to notice: the `gpu=` swap is **NOT substituted**, and each refusal is a
structured leak with the reason. calque's graceful refusals are part of the
product: an omission you can see is a decision, not a bug.

---

## 5. A Volume-cached model, reused across runs — no AWS, no dry-run

A second common shape alongside `.map()` batch inference: a plain
`@app.function` that populates a `Volume` once, and a `@cls`+`@enter` that
reads from it every run — the model weights persist across separate
invocations instead of reloading from the image each time.

```
./calque analyze examples/volume_cache.py
```

```
=== volume_cache.py (app "volume-cache") ===
  functions=1 classes=1 entrypoints=1 image.default.base="debian_slim" pip=[torch==2.4.1 torchvision==0.19.1]
  gpu[download_weights]: no_gpu requested="" (no gpu= declared)
  gpu[Scorer]: clean_swap requested="L4" -> RTX PRO 6000 (single-card, no coupling signal; memory-bound B=1 substitution is legal)
  volume: "weights" -> volumes/weights/ (mount /models, delta-sync => warm-cache reuse)
...
--- leak report (§10) ---
LEAKS: 1 emitted across 1 primitives
  volume (1):
    - [semantic_gap] Scorer: model identity obscured (loaded from a path/mount, not a repo id); Bedrock identity check cannot run
```

What to notice: a plain `@app.function` (`download_weights`, no `@cls`) and a
`@cls`+`@enter` (`Scorer`) coexist in the same script and are both
recognized correctly; the `Volume` maps to a stable S3 prefix that's
delta-synced before `@enter` runs, so a second run reuses the already-warm
cache instead of rebuilding it.

---

## 6. Cross-app invocation — not currently supported, honestly leaked

Not everything gets ported. `Function.from_name(...)`/`Cls.from_name(...)`
look up an **already-deployed separate Modal app** by name — calque has no
notion of a separately-deployed app to call into, so this is not currently
supported. The point of this journey is what calque does INSTEAD of
silently ignoring it or crashing.

```
./calque analyze examples/cross_app.py
```

```
=== cross_app.py (app "cross-app") ===
  functions=1 classes=0 entrypoints=1 image.default.base="" pip=[]
  gpu[caller]: no_gpu requested="" (no gpu= declared)
  volume: "weights" -> volumes/weights/ (mount /weights, delta-sync => warm-cache reuse)
...
--- leak report (§10) ---
LEAKS: 3 emitted across 2 primitives
  entrypoint (1):
    - [semantic_gap] caller: secrets={"__unparsed__": "[api_key]"} recorded but NOT injected in the spike; a payload needing them will fail
  map (2):
    - [semantic_gap] Function.from_name("other-app", "remote_worker"): cross-app invocation of an already-deployed separate app — calque has no notion of a separately-deployed app to call into; not reproduced
    - [semantic_gap] Cls.from_name("other-app", "RemoteBatcher"): cross-app invocation of an already-deployed separate app — calque has no notion of a separately-deployed app to call into; not reproduced
```

What to notice: `Volume.from_name`/`Secret.from_name` on the same script are
handled correctly and produce **no** leak — only the two genuinely
unsupported `Function.from_name`/`Cls.from_name` calls do. Each leak names
the exact call site and says precisely why it isn't reproduced, rather than
lumping every `*.from_name(...)` call together or dropping the gap
silently. See
[`../docs/behind-the-seam-register.md`](../docs/behind-the-seam-register.md)
for the full list of non-goals like this one, with the attach point a
future build would touch.

---

## 7. A real script, real AWS hardware, a real result — this one costs money

> **This journey is NOT zero-spend.** It's a recorded transcript of an
> actual billable run (`calque real`, ~$0.10/hr `m6i.large`, a few
> minutes), included as evidence rather than something meant to be
> re-run casually. If you want to run this yourself, read
> [`../docs/guide/getting-started.md`](../docs/guide/getting-started.md)
> first — it covers the AWS prerequisites and the `smoke`-before-`real`
> de-risking step this transcript skips for brevity.

Everything above proves calque parses, guards, and locally dry-runs a
script faithfully. This journey proves the last mile: `AI-Almanac`'s real,
unmodified `blending_app.py` — a script from a real customer's Modal app,
never modified for calque — ran its own `inspect_netcdf_bundle` function on
a real AWS instance, against a real netCDF climate-data file
(`--item-file`, since the function's real signature takes `bytes`), and
returned the exact same result a local reference execution of the same
function did.

```
./calque real --bucket YOUR-BUCKET --run-id blending-demo \
  --instance m6i.large --script blending_app.py \
  --entrypoint inspect_local_netcdfs --item-file 1998.nc \
  --i-understand-this-spends-money
```

```
=== calque REAL GPU run (model=Qwen/Qwen2.5-1.5B-Instruct N=1 region=us-east-1 instance=m6i.large) ===
[1/8] built warmd (linux/amd64)
[2/8] uploaded artifacts
[3/8] wrote manifest (1 items, inspect_netcdf_bundle's own @enter+@method)
      priced m6i.large @ us-east-1 = $0.0960/hr (truffle)
[4/8] acquiring m6i.large in us-east-1 (block-and-wait, AZ-sweep)...
      acquired i-0c9e1996a0c0d4357 (us-east-1a) after 3s
[5/8] waiting for warmd summary (vLLM load + 1 generations)...
      ...running (15s)
      ...running (30s)
[6/8] summary: @enter x1 (0.1s load), 1 items, 0 failed, occupancy unmeasured (none)
[7/8] collected 1/1 results (0 missing)
      sample result[0]: dims: {day: 46, lat: 5, lon: 5, time: 26}, coords: [lat, lon, time, day], data_vars.tp.shape: [26, 46, 5, 5]

[8/8] terminating i-0c9e1996a0c0d4357 ...
      terminated i-0c9e1996a0c0d4357
```

What to notice: `0 failed`, and the `dims`/`coords`/`data_vars.tp.shape`
values are **byte-identical** to running `inspect_netcdf_bundle` locally
against the same file — the real function, unmodified, on real hardware,
against real data, produced the real answer. The instance terminated
cleanly afterward. `CHANGELOG.md`'s `[0.3.0]` entry has the full account,
including two real bugs this exact validation pass found and fixed along
the way (a missing IAM instance profile, and host-mode's missing
dependency-install step) — the kind of finding that only shows up once you
actually run something for real, which is the whole reason this journey
exists.

---

## Where to go next

- Ready to spend real money and run something for real?
  [`../docs/guide/getting-started.md`](../docs/guide/getting-started.md) picks up
  exactly where this page leaves off — first billable smoke test, then a real AWS run.
- The full pipeline, CLI, and design notes: [`../README.md`](../README.md).
- What calque deliberately does *not* port, with attach points:
  [`../docs/behind-the-seam-register.md`](../docs/behind-the-seam-register.md).
- Every flag, for every command: [`../docs/guide/cli-reference.md`](../docs/guide/cli-reference.md).
