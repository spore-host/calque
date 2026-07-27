# examples

Four journeys that show calque's whole story **without spending a cent** — no AWS
GPU is launched by anything on this page. Each is a real command with its actual
(abbreviated) output.

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
  functions=0 classes=1 entrypoint=true image.base="debian_slim" pip=[vllm==0.6.3 transformers==4.45.2 huggingface_hub]
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

## 2. Dry-run and inspect the crossover K

`run` defaults to `--dry-run`: it exercises every stage locally over a synthetic
sample and produces the crossover K — **without launching a billable instance.**

```
./calque run --n 100 --dry-run examples/map_batch_inference.py
```

```
recommend+resolve: card="RTX PRO 6000" -> instance="g7e.2xlarge"
image: Dockerfile rendered, digest=ae6b11f861e02f4b (tag for ECR cache)

[DRY-RUN] not launching a billable instance; driving warm worker locally on a synthetic sample
[DRY-RUN] warm unit ran 50 items, 0 failed; @enter x1 (0.305s), mean 0.0543s/item

--- crossover K (§9) ---
Your workload:   map-batch (asked for H100 -> substituted g7e.2xlarge), 100 items, ...
  ...
Verdict:    you are running 100.  100 >= K(0) -> CROSS. Code is unchanged; here's the bill.
AWS side of K: [measured]

*** DRY-RUN K IS NOT DEFENSIBLE ***
Per-item seconds and occupancy are SYNTHETIC (stand-in body, no GPU). A K that
survives a hostile read requires the real payload on an acquired RTX PRO 6000 (§16.1).
```

What to notice: calque **refuses to pretend**. The dry-run K is stamped NOT
DEFENSIBLE because the per-item timings are synthetic — a real K needs a real GPU.
This is the "phase detector, not a sales funnel" discipline in action.

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

## Where to go next

- The full pipeline, CLI, and design notes: [`../README.md`](../README.md).
- What calque deliberately does *not* port, with attach points:
  [`../docs/behind-the-seam-register.md`](../docs/behind-the-seam-register.md).
- Running for real (billable, gated behind `--i-understand-this-spends-money`):
  see the CLI section of the top-level README.
