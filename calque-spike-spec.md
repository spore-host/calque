# calque — Spike Build Spec

**For:** Claude Code
**Language:** Go (control plane), Python (worker payload only)
**Status:** Spike. Prove one thing, fake everything else, log what breaks.

---

## 0. What calque is (read this first)

calque runs **Modal-shaped code at AWS scale, unchanged.**

Modal is where inference/batch code is *prototyped* — great inner loop, pay-nothing-when-idle, scale is hypothetical. AWS is where the same code *scales* — you own the rectangle, buy down the rate, and the economics flip in your favor at volume. calque is the loan-translation between the two: the same script that ran over 10 items on Modal runs over 10,000,000 on AWS **without a logic rewrite** (only a mechanical `gpu=` substitution).

A *calque* is a structure-preserving translation between languages (English "flea market" ← French *marché aux puces*). That is literally the job: translate Modal's idioms onto AWS term-by-term, structure intact, so the author doesn't notice the translation.

**We are not replacing Modal.** We are enabling Modal-shaped code to run on AWS. If an idiom doesn't translate, the correct output is a **logged leak**, not a feature we owe ourselves.

### The one number the spike exists to produce

The **crossover K**: the workload scale at which AWS becomes cheaper than Modal, computed from a **real measured run** — not modeled. Below K, the honest answer is "stay on Modal." Above K, "cross, here's the code unchanged, here's the bill." calque is a **phase detector**, not a sales funnel: it must be willing to tell the user they haven't crossed yet.

---

## 1. Scope

### In scope (build for real)
- Parse a Modal script → extract six primitives into a Go IR.
- Rewrite `gpu=` targets to `RTX PRO 6000` (with a guard, §7).
- Build the image (Modal `.image` DSL → Dockerfile → ECR).
- Acquire hardware via spore.host (truffle → candidates, lagotto → snipe, spawn → launch).
- Run the payload on the acquired instance under a **warm** Python process (§6).
- Collect results from S3.
- Measure per-item cost + occupancy (§8) — **load-bearing for K**.
- Emit the cost comparison + crossover K (§9).
- Emit a structured **leak report** (§10) — this is a primary deliverable.
- A **Bedrock eligibility gate** (§11) — static, cheap, high-signal.

### Out of scope (DO NOT BUILD — fake behind the seam, §4)
- Any real right-sizing / card-selection intelligence. The recommender returns a **constant**.
- The cost/latency frontier, MIG packing, NEFF caching, Trainium, topology verification.
- Multi-node / coupled workloads. **G6/G6e/G7/G7e only** — single-node by construction.
- `.spawn()` handles, `--detach`, async result futures. Block-and-wait only.
- Fuzzy Bedrock matching with quality scoring. (Match tiers only, labeled, no quality claim.)

**If you find yourself building selection logic, cost-optimization, or Modal-completeness, stop.** Those live behind the seam and are explicitly deferred. The spike's value is proving the *plumbing carries the semantics* and *measuring the crossover* — nothing else.

---

## 2. Target substrate (fixed)

- **Instance families:** G7e (RTX PRO 6000 Blackwell, 96GB), G7 (RTX PRO 4500), G6e (L40S, 48GB), G6 (L4, 24GB). Single-node only.
- **Default card (the stub's constant):** `RTX PRO 6000` → resolves to a `g7e` instance via truffle.
- **Regions:** G7e is regionally thin. Acquisition *will* fail sometimes; that's why lagotto exists (§5). Do not treat `InsufficientInstanceCapacity` as a crash — it's a wait.

---

## 3. Pipeline

```
script.py
  └─► parse      decorators → IR         (shallow AST; bodies extracted verbatim)
       └─► gate  Bedrock eligibility?     (static; if hit, recommend & stop)
            └─► recommend  IR → Target    (STUB: constant behind Recommender interface)
                 └─► plan
                       truffle:  Target.Card → candidate g7e instance types + regions
                       lagotto:  snipe the target, sit and retry until acquired
                       image:    .image DSL → Dockerfile → ECR (once; cache by digest)
                       weights:  Volume → S3 prefix
                     └─► exec
                           spawn:   launch acquired instance with the image
                           [worker] spored supervises warm Python: @enter once, drain, → S3
                         └─► collect  gather from S3, ordered by input index
                              └─► measure  per-item cost + occupancy (tach hook)
                                   └─► report  cost comparison + crossover K + leaks
```

**Key framing for the parser:** decorators are *configuration*, function bodies are *payload*. The Go control plane understands the decorators. It does **not** parse or understand the function bodies — they ship to the worker verbatim and run under Python exactly as on Modal. You are re-implementing Modal's *control plane* in Go; the untouched bodies run on the worker.

---

## 4. The seam (the one piece of future-proofing — build this for real)

Everything downstream consumes a `Target`. Nothing inlines the card name into the code generator. The entire faked brain is one constant behind an interface. Later, `StubRecommender` is swapped for the real phase-detector and the plumbing never notices.

```go
// internal/target/target.go
package target

type Target struct {
    Card     string // e.g. "RTX PRO 6000"
    Instance string // truffle fills this: e.g. "g7e.xlarge"
    Region   string // lagotto fills this on acquisition
}

type Recommender interface {
    Recommend(app ir.App, fn ir.Function) Target
}

// StubRecommender is the ENTIRE faked brain. Do not add logic here.
type StubRecommender struct{}

func (StubRecommender) Recommend(_ ir.App, _ ir.Function) Target {
    return Target{Card: "RTX PRO 6000"}
}
```

This is the "first doesn't foreclose the second" contract. Honor it strictly.

---

## 5. spore.host integration (libraries — Scott owns these repos)

Import `spawn`, `truffle`, and `lagotto` as Go libraries. **You will have repo access. Read the actual API surface from the source — do not invent function signatures.** Where the real API differs from what's implied below, follow the real API and note the delta in the leak report. Scott can patch these repos same-day if the spike hits an edge, so surface integration friction explicitly rather than working around it.

- **truffle** — instance search / menu lookup. Input: a `Target.Card`. Output: candidate concrete instance types (+ regions/AZs) that satisfy it. In the spike this is a near-trivial map (`RTX PRO 6000` → `g7e.*`), but call truffle rather than hardcoding, so the seam holds.
- **lagotto** — availability sniper. Input: a **single** resolved target (instance type + region). Behavior: sit and keep retrying acquisition (on-demand for the spike) until it lands, so the caller can "go do something else." Output: a live, acquired instance handle. **Single-target for the spike** — candidate-list sniping across alternatives is brain-level selection and is deferred. lagotto is what exposes the acquisition-latency that Modal's warm pool hides; **log time-to-acquire per (card, region)** — it's free ground truth the real brain will need.
- **spawn** — bring-up. Input: an acquired instance + the built image. Behavior: launch. Only fires *after* lagotto returns a live instance.

**Execution posture:** block-and-wait. The CLI call sits, prints a live "waiting for capacity…" status while lagotto snipes, and returns when the run completes. Treat acquisition as a *future lagotto resolves*, not an inline synchronous launch — so a later `--detach` is a swap, not a rewrite. Stub `--detach` as a TODO.

---

## 6. The worker (the integration edge most likely to draw blood)

Modal's `@enter` loads the model **once per container**, then `.map()` reuses it across many items. The naive "one item = one process" mapping silently reloads the model per item and destroys the economics. **Do not do that.**

Correct shape: **spored (Go) supervises a long-lived Python runner.**

- The Python runner starts, executes the `@enter` body **once**, holds the loaded model in-process.
- spored feeds it work items over a local socket (or stdin/stdout framing — your call, note it).
- The runner processes each item with the `@method`/function body, returns the result.
- spored writes each result to S3, **keyed by input index** (for ordered collection).
- On runner crash: spored's supervision restarts it (reloads `@enter`), re-drives unfinished items.

This "Go supervises warm Python" boundary is the riskiest plumbing in the spike. Expect edges around: process lifecycle, the socket protocol, backpressure, and clean shutdown/flush. **Log every rough edge here as a leak** — these are exactly the findings the spike exists to surface.

`spored` here is baked into the worker image, not a control-plane import.

---

## 7. The `gpu=` rewrite rule + guard

Modal scripts say `gpu="H100"`, `gpu="A100"`, etc. Rewrite to `RTX PRO 6000` (96GB) — memory is "same-ish" vs 80GB A100/H100, so the model still fits.

**But the swap is only legal if the original job was memory-bound or single-card.** Guard against silently downgrading a job that genuinely needed the big card's bandwidth/interconnect:

```
if source requests > 1 GPU  (e.g. "H100:8")            → FLAG: multi-GPU, out of single-node scope
if source shows torchrun / NVLink / tensor-parallel     → FLAG: coupled, out of scope
else                                                    → substitute → RTX PRO 6000, log substitution
```

Emit a **structured substitution log**: every clean swap and every flag. The ratio (clean-swaps : flags) across a corpus is itself a finding — most Modal inference is B=1 request-response and lands in the swap-legal regime by construction, so expect it lopsided toward clean. Do not fuzzy-match or silently substitute across flags.

---

## 8. The tach hook (load-bearing — K stands on this)

Since **both Modal and AWS bill per-second**, the *only* things that decide the crossover are: rate, **occupancy**, and buy-down. Two of those require the real run. Modeling occupancy makes K a guess; measuring it makes K a fact a skeptic can reproduce. So measure it, even crudely.

Minimum viable measurement on the acquired instance:
- **Occupancy:** sample GPU utilization (DCGM counters if available; `nvidia-smi` polling as fallback) across the run → mean occupancy `P%`.
- **Per-item wall-clock:** time each item's processing in the warm runner → mean seconds/item.
- **Instance seconds held:** from lagotto-acquire timestamp to spawn-terminate timestamp (this is the *rectangle* — includes acquisition wait and idle, which is what AWS actually bills).

If a real tach library exists in the spore.host org, prefer it; otherwise a crude in-worker sampler is acceptable for the spike. Note which you used.

---

## 9. The cost model + crossover K (the headline deliverable)

**Both sides per-second.** The comparison is honest and must survive a hostile read (it will be attacked by a skeptical PI or a Modal advocate). Ground both sides in the *same measured per-item compute* from the real run; differ only in the billing model applied.

```
                rate/sec    billed seconds                       buy-down
Modal           R_m         compute-seconds only (scale-to-0)    none
AWS on-demand   R_a         launch→terminate (the rectangle)     Savings Plan / spot
```

- **Modal @ scale:** `R_m × (compute_seconds + warm_idle_seconds)`. Build from Modal's published per-second GPU rate × measured per-item compute × item count. **Do not linearly extrapolate from a 10-item run** — that understates Modal and makes the comparison attackable. Build it up honestly; it's still higher at scale.
- **AWS @ scale:** `R_a × rectangle_seconds`, where rectangle_seconds derives from measured occupancy `P` and measured seconds/item. Show the number **at the measured occupancy**, not at an assumed 100%.
- **Buy-down ladder:** show AWS on-demand (worst), and note Savings Plan / spot as lower rungs Modal structurally cannot offer. (Spot/SP rates can be static constants in the spike — flag them as such.)

**Output the crossover K:** the item count where `R_m × Modal-seconds(K)` exceeds `R_a × rectangle-seconds(K)`. State it as a boundary the user locates themselves against:

```
  Your workload:   <model>, <items>, measured <sec/item>, measured occupancy <P%>
  10 items    Modal: $X     (Modal wins — this is what Modal is for)
  <N> items   Modal: $Y   |  AWS on-demand: $Z   (at <P%> occupancy)
  Crossover:  ~K items      (on-demand);  ~K' items (Savings Plan)
  Verdict:    you are running <N>.  <N> < K → STAY ON MODAL.
                                    <N> ≥ K → CROSS. Code is unchanged; here's the bill.
```

The verdict must be willing to say **stay on Modal**. A tool that always says "cross" is a funnel, not an instrument.

---

## 10. The leak report (primary deliverable, first-class)

Every place the Modal shape doesn't carry gets a **structured** `LEAK` entry — not a code comment, an emitted record. A clean run that surfaces three ugly edges taught you more than a clean run that surfaced none.

```go
type Leak struct {
    Primitive string // "map" | "enter" | "image" | "volume" | "gpu" | "entrypoint" | "acquire"
    Kind      string // "unsupported_arg" | "semantic_gap" | "unhandled_case" | "integration_edge"
    Detail    string // human-readable: what Modal does, what we did/didn't do
    Script    string // which test script + line
}
```

Known semantics to watch and log when they bite: `.map()` result ordering at scale, partial failure (3 of 10k items die), `@enter` timing/warm-reuse, image rebuild-on-no-change (must be a cache hit), acquisition latency, any decorator arg the parser doesn't handle.

The report is the finding **whether or not the brain ever gets built**. It's simultaneously: the engineering map (which idioms are cheap vs load-bearing) and a market census (how much Modal usage is even AWS-mappable).

---

## 11. The Bedrock eligibility gate (static, cheap, high-signal)

Runs **before** recommend. If a Modal script is self-hosting a model that's already in the Bedrock catalog and the usage is plain request-response inference, the honest answer is "don't rent a GPU — call the API." This routes work *away* from calque, which is correct and is what earns the tool credibility.

Two ANDed static checks (no execution required — pure AST + one catalog fetch):

1. **Identity** — is `(model repo, revision)` in the **live** Bedrock catalog? Fetch the catalog at analysis time (do not hardcode a snapshot — it's region-gated and moves). Exact match preferred.
2. **Shape** — is usage plain inference? `.map()` over prompts or a serve entrypoint calling `.generate()` → yes. Training loop / fine-tune / custom-checkpoint `@enter` → no.

**Match tiers (not a boolean):**
- `exact` — identity hit + inference shape → **auto-suggest Bedrock, stop.**
- `near` — same family / bigger sibling / base-of-a-fine-tune → **offer, labeled by axis of difference, ranked by distance. No quality claim.** (e.g. "base Mistral available; your fine-tune's task behavior is gone — only you know if that matters.")
- `none` → fall through to hardware.

**Exact-match discipline is the credibility floor:** never silently round a custom checkpoint to a catalog entry. A skeptic checking "did it claim my model was on Bedrock?" must get a clean answer. Depth of catalog (several hundred open-weight models) makes exact hits common enough that strictness is affordable.

Emit the **Bedrock-eligible count** across the corpus — how many self-hosted jobs should have been an API call. Cheapest damning number in the instrument; requires no run.

---

## 12. Repo layout

```
/cmd/calque                 CLI entry: `calque run script.py [--detach(stub)]`
/internal/parse             decorators → IR   (python-ast-json helper, §13)
/internal/ir                the six-primitive types (§14)
/internal/gate              Bedrock eligibility (§11)
/internal/target            Recommender interface + StubRecommender  ← THE SEAM (§4)
/internal/plan              IR + Target → AWS plan; imports truffle, lagotto
/internal/image             .image DSL → Dockerfile → ECR
/internal/exec              imports spawn; collects from S3
/internal/measure           tach hook: occupancy + per-item (§8)
/internal/cost              cost model + crossover K (§9)
/internal/leak              structured leak report (§10)
/worker/spored-runner       Go supervisor + long-lived Python runner (§6)
/testdata/scripts           the three Modal test scripts (§15)
```

---

## 13. Parsing Modal (Python) from Go

Do not write a Python parser in Go for the spike. Ship a tiny Python helper that emits the decorator AST as JSON; Go consumes it. You are not testing the parser — you're testing whether the *mapping* carries. tree-sitter-python is the version-two answer; the JSON helper gets you there a day sooner.

The helper walks only what's needed: `modal.App(...)`, `@app.function(...)` / `@app.cls(...)` args (`gpu`, `volumes`, `timeout`, `image`), `@modal.enter` / `@modal.method` presence, the `.image` DSL chain (`.debian_slim()`, `.pip_install(...)`, `.uv_pip_install(...)`), `Volume.from_name(...)`, where `.map()` is called, and `@app.local_entrypoint`. **Function/method bodies are extracted verbatim as strings** — not parsed — for shipping to the worker.

---

## 14. The IR

Struct-shaped transcription of what the decorators said. Nothing clever.

```go
type App struct {
    Name       string
    Image      Image
    Functions  []Function
    Classes    []Class
    Entrypoint *Function
}
type Image struct {
    Base    string   // "debian_slim"
    Pip     []string // installed packages
}
type Function struct {
    Name    string
    GPU     string            // raw from source, e.g. "H100" — rewritten in §7
    Volumes map[string]string // mount path → volume name
    Timeout int
    IsMap   bool
    Body    string            // verbatim payload, shipped to worker
}
type Class struct {
    Name      string
    GPU       string
    Volumes   map[string]string
    EnterBody string            // runs once in the warm runner
    Methods   []Function
}
```

---

## 15. Test scripts + scale stress

Three real Modal-shaped scripts in `/testdata/scripts`, covering the six primitives:

1. **`map_batch_inference.py`** — `.map()` fan-out over prompts, `@cls` + `@enter` loads vLLM once, `Volume` weights cache. *Stresses:* fan-out translation, ordering, partial failure, warm-drain, multi-instance acquisition.
2. **`cls_serve.py`** — single `@cls` GPU serve, `@enter` load. *Stresses:* warm-pool economics (load-once amortization as a real number).
3. **`volume_cache.py`** — `Volume` weight reuse across runs. *Stresses:* S3 weight-cache plumbing, image/volume separation.

**Run each at a scale that is meaningless on Modal-the-prototype and load-bearing on AWS** (e.g. `.map()` at ≥100k items). The leaks that matter only bite at scale — ordering across 100k, partial failure, acquisition of multiple instances, warm-drain amortization. First stress case: `map_batch_inference.py` at 100k, because it exercises fan-out ordering + partial-failure + multi-instance acquisition simultaneously.

---

## 16. Success criteria

The spike **succeeds** when:

1. At least one test script, rewritten only by the mechanical `gpu=` substitution, **runs on an acquired RTX PRO 6000 on AWS and produces correct output**, with the model loaded **once** (warm) and results collected ordered from S3.
2. The run emits a **crossover K** grounded in measured per-item cost + occupancy — a number defensible under hostile read because its basis is the user's own code on real hardware.
3. The run emits a **structured leak report** cataloguing every place the Modal shape didn't carry.
4. The static pass emits **Bedrock-eligible**, **clean-swap**, and **flag** counts across the test corpus.

Success is **not** "the recommendation is good" — that's stubbed. Success is *the plumbing carries the semantics, the crossover is real, and the gaps are logged.* A run that completes and surfaces several honest leaks is a better outcome than a suspiciously clean one.

---

## 17. Integration questions to surface (do not guess)

These depend on spore.host internals you'll read from source. Where the real API contradicts this spec, follow the source and log the delta:

- **truffle:** exact signature for `Card → candidate instances`? Does it return regions/AZs?
- **lagotto:** does it take a single target or a candidate list? (Spec assumes single.) What's the acquire-handle type? How does it report progress for the live status line?
- **spawn:** launch signature; how it takes the ECR image ref; how it returns the live instance for exec.
- **tach:** does a usable occupancy/throughput library exist in the org, or is the crude in-worker sampler the path for the spike?
- **worker protocol:** socket vs stdio framing for the spored↔Python runner — pick, implement, note it.

Surface these as explicit questions/notes rather than inventing behavior. Scott can patch the spore.host repos directly if the spike needs it.

---

## 18. Explicit non-goals (guard against gold-plating)

Do **not**, in the spike, build: real card selection, the cost/latency frontier, MIG tiling, NEFF caching, Trainium/eager paths, topology verification, multi-node/gang scheduling, `.spawn()` handles, `--detach` async futures, fuzzy Bedrock quality scoring, or Modal API completeness. Each is designed-for-later and lives behind the seam. The spike proves the calque *carries the shape and computes the crossover*. That is the whole job.
