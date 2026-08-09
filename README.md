# calque

**Run Modal-shaped code at AWS scale, unchanged.**

Modal is where inference/batch code is *prototyped* — great inner loop, pay-nothing-when-idle.
AWS is where the same code *scales* — you own the rectangle and the economics flip at volume.
calque is the loan-translation between the two: the same script that ran over 10 items on Modal
runs over 10,000,000 on AWS **without a logic rewrite** (only a mechanical `gpu=` substitution).

**Current spike target:** batch inference using `.map()` and warm single-node GPU
workers. Unsupported Modal idioms (serve, async futures, multi-GPU/coupled, some
config kwargs) are detected and reported, never silently ignored — see the
capability matrix below.

> A *calque* is a structure-preserving translation between languages (English "flea market" ←
> French *marché aux puces*). That is the job: translate Modal's idioms onto AWS term-by-term,
> structure intact, so the author doesn't notice the translation.

**This is a spike.** It exists to prove one thing and fake everything else:

- **Prove:** the plumbing carries Modal's semantics onto AWS, and produce the **crossover K** —
  the workload scale at which AWS becomes cheaper than Modal, from a **real measured run**, not a model.
- **Fake behind the seam:** all card-selection / cost-optimization intelligence. The recommender
  returns a constant (`RTX PRO 6000`) behind an interface. See `internal/target`.

calque is a **phase detector, not a sales funnel**: below K the honest verdict is "stay on Modal."

## Quick start (zero spend)

Everything here runs locally and **launches no AWS GPU** — no credentials required.

**Prerequisites:** Go 1.26 (matches `go.mod`), Python 3, and [`uv`](https://docs.astral.sh/uv/).
AWS credentials are needed only for real (billable) runs, not for anything below.

```
git clone https://github.com/spore-host/calque && cd calque
go build -o calque ./cmd/calque      # control plane
(cd tools/pyast && uv sync)          # Python AST helper deps
```

**Analyze a Modal script** (static passes — GPU-swap legality, Bedrock gate, leaks):

```
./calque analyze examples/map_batch_inference.py
```

```
  gpu[Batcher]: clean_swap requested="H100" -> RTX PRO 6000 (single-card, no coupling signal; memory-bound B=1 substitution is legal)
  volume: "weights" -> volumes/weights/ (mount /weights, delta-sync => warm-cache reuse)
...
--- leak report (§10) ---
LEAKS: 1 emitted across 1 primitives
```

**Produce a crossover K** (full pipeline, dry-run — the default):

```
./calque run --n 100 --dry-run examples/map_batch_inference.py
```

```
[DRY-RUN] not launching a billable instance; driving warm worker locally on a synthetic sample
--- crossover K (§9) ---
Verdict:    you are running 100.  100 >= K(0) -> CROSS. Code is unchanged; here's the bill.

*** DRY-RUN K IS NOT DEFENSIBLE ***
Per-item seconds and occupancy are SYNTHETIC (stand-in body, no GPU). A K that
survives a hostile read requires the real payload on an acquired RTX PRO 6000 (§16.1).
```

That's the whole idea in two commands: a mechanical `gpu=` swap, then an honest K —
stamped **not defensible** here precisely because no real GPU ran. See
[`examples/`](examples/) for four annotated journeys (analyze, dry-run K, Bedrock
route-away, and an unsupported-workload refusal), each with real output.

> **Notes.** `analyze`/`run` reach two best-effort network sources (the Bedrock
> catalog and the `hf-bedrock-map` API); offline they print a `warn:` line and fall
> back — a networkless run is not a failure. The binary finds the Python helper
> relative to the repo; to run a copied/`go install`ed binary out of tree, set
> `CALQUE_PYAST_DIR` to the `tools/pyast` path.

## Status

Spike, in active build. Tracking lives on GitHub (Issues / Projects / milestones), not local files.
**Phase 2 (Modal-idiom porting, milestones M5–M10) is merged.**

Verification is layered — offline (unit-tested, no spend), live (run on a real
acquired GPU), and at scale (the rung that only bites at volume). The matrix reads
them apart:

| Capability | Offline tested | Live verified | Scale verified |
|---|---|---|---|
| Parse → IR, leak reporting | ✅ | n/a | ✅ corpus census |
| `gpu=` swap guard (§7) | ✅ | ✅ | ✅ corpus |
| Bedrock gate + route-away (§11) | ✅ | ✅ (live catalog) | n/a |
| Idiom pass-through (§C) | ✅ | n/a | n/a |
| Single-instance warm run (§6) | ✅ | ✅ (L4, Qwen2.5-1.5B) | ✅ N=100 |
| Cost + crossover K (§9) | ✅ | ✅ (all `[measured]`) | ✅ N=100 |
| Volume sync + commit write-back (§3/§15) | ✅ | ✅ (sync) | — |
| Multi-instance fan-out / fleet K (§15) | ✅ | ⏳ pending capacity | ⏳ pending N=100k ([#18](https://github.com/spore-host/calque/issues/18)) |
| Serve entrypoints (§F) | detect + leak only | ❌ not built | ❌ |

✅ done · ⏳ blocked/pending · ❌ deliberately not built (see `docs/behind-the-seam-register.md`).

The per-capability detail follows; nothing is removed, only sequenced under the matrix.

**Built, tested, and verified (no spend):**
- `parse → IR → seam` — pyast helper, six-primitive IR, `StubRecommender` (constant behind an interface).
- **Bedrock gate + route-away** (§11) — live catalog, `exact`/`near`/`none` tiers, proven in both
  directions. A model that's already an exact Bedrock API call yields a structured `ReplacementOffer`
  (Bedrock `modelId` + region invoke hint) and **short-circuits the runnable path *before* a GPU is
  acquired** — renting a GPU for a served model is the wrong answer. Near matches surface as candidates
  to verify, with the axes of difference and *no* quality claim (exact-match discipline).
- **`gpu=` guard** (§7) — clean-swap vs multi-GPU/coupled flags, adversarial fixture.
- **idiom pass-through** (§C) — image verbs (incl. `from_aws_ecr`); portable kwargs (`cpu=`/`memory=`
  recorded-not-acted behind the seam, `retries=` wired to the warm supervisor); synchronous invocation
  kinds `.map`/`.starmap`/`.for_each`/`.remote`; async `.spawn`/`.map.aio` recognized-and-leaked.
- **warm worker** (`warmd` + `runner.py`, §6) — `@enter` once, crash-restart re-drive, partial-failure.
- **cost + crossover K** (§9) — rate asymmetry (`R_m` for card asked-for vs `R_a` for card substituted-to),
  willing to say *stay on Modal*, `measured | proxy` flag.
- **plan** (§5) — truffle resolve+price, `Acquirer` over `spawn.Provision` with capacity-aware
  AZ-sweep retry.
- **image / exec** — Dockerfile+digest cache; S3 sink/collector; on-instance `warmd` entrypoint.
- **volumes** (§3/§15) — `Volume.from_name` → stable S3 prefix, delta-synced to the mount path
  before `@enter` (warm-cache reuse; image/volume separation). `volume.commit()` persists as an
  **end-of-run write-back** (reverse S3 sync after `@method` drains); mid-run `.reload()`/`.commit()`
  is leaked as a semantic gap the spike doesn't reproduce.
- **multi-instance `.map` fan-out** (§15) — shard N items across S single-node instances acquired **in
  parallel**, drive each shard's `warmd` independently, collect the **union** back into one globally
  ordered result set + one global `missing[]`, re-drive a dead shard once, fold a **fleet-level K** that
  sums per-instance overheads. Embarrassingly parallel across independent boxes — *not* §1's forbidden
  multi-node/gang scheduling. Core is unit-tested offline; `calque real --shards N` wires it.
- **serve entrypoints** (§F) — `@web_endpoint`/`@asgi_app`/`@wsgi_app` are **detected and leaked** as a
  deferred shape (the spike measures batch + K); a served *Bedrock* model still routes away. The
  long-lived server is not built — see `docs/serve-architecture.md`.
- **full pipeline** — `calque run --dry-run` runs every stage locally; `calque session` acquires one
  GPU and runs an N-ramp on it; `calque real --shards N` fans out across a fleet.

**Real measured crossover K — achieved on a live GPU.** A real run (Qwen2.5-1.5B on an L4, all
`[measured]`, no proxies) produced the headline number:

- N=100: `@enter` ran **once** (102.7s load), **1.583s/item**, **59% measured occupancy** → **K ≈ 73 items**
  on-demand (~18 with a Savings Plan); verdict at 100k = **CROSS**.
- N=1: same load amortized over one item → 5% occupancy → **STAY ON MODAL**.

> **Occupancy scope (#71):** those occupancy percentages are `whole_run` means — averaged over
> the *whole* rung including the one-time model load, so they understate steady-state GPU fill
> and are not recomputable (those runs' samples were not timestamped). Occupancy now carries an
> explicit `scope` (`inference` vs `whole_run`); see
> [`docs/measured-runs/README.md`](docs/measured-runs/README.md). Per-item seconds, load times,
> and the verdicts are unaffected.

The N=1↔N=100 contrast is the phase detector working: same code, same model, honest verdict at each
scale (§9). Getting real inference end-to-end surfaced five genuine deployment findings, each caught
fast and fixed: worker dir `/opt`→`/tmp`, docker needs `sudo`, IMDSv2 hop-limit 2 for container creds,
200 GiB root volume for the vLLM image, and vLLM's stdout logs colliding with the warm-worker JSON
protocol (the §6 "socket draws blood" edge — now isolated + regression-tested).

**Corpus census (§16.4)** across the test scripts: Bedrock 1 exact-eligible / 1 self-hosted / 4
identity-hidden; gpu guard 4 clean-swaps / 1 multi-GPU flag / 1 coupled flag / 1 no-gpu.

## Pipeline

```
script.py
 └─ parse      decorators → IR         (shallow AST; bodies extracted verbatim)
   └─ gate     Bedrock exact match?    (route away: print offer & stop BEFORE any GPU)
     └─ recommend  IR → Target         (STUB: constant behind Recommender interface)
       └─ plan   truffle: Card → candidate g7e instances (+ live price = R_a)
                 acquire: block-and-wait retry over spawn.Provision until landed
                 image:   .image DSL → Dockerfile → ECR (cache by digest)
         └─ exec   spawn.Provision launches + brings up the instance(s)
                   [worker] warmd supervises warm Python: @enter once, drain → S3
                   [--shards N] shard items → N instances in parallel → union collect
           └─ collect   gather from S3, ordered by GLOBAL input index (+ global missing[])
             └─ measure per-item cost + occupancy (tach hook); fleet K sums per-box overhead
               └─ report cost comparison + crossover K + leaks
```

The Go control plane understands **decorators** (configuration). It does **not** parse function
**bodies** (payload) — those ship to the worker verbatim and run under Python exactly as on Modal.

> **Acquisition seam** (confirmed with spore.host, spawn#351/lagotto#73): `spawn.Provision` *owns*
> `RunInstances` (acquire + bring-up in one shot); calque owns the block-and-wait retry loop. This is
> the inverse of the spec's implied "lagotto acquires → spawn brings up," and is the real model.

## CLI

```
calque analyze <script.py> [...]                      # static passes (gate, gpu, leaks, census)
calque run [--n N] [--region R] [--dry-run] <script.py>   # full pipeline → crossover K (dry-run default)
```

The three commands below acquire billable GPU hardware and are gated behind an explicit
`--i-understand-this-spends-money` flag — they refuse to launch without it:

```
calque smoke   --bucket B --run-id ID [--region R] [--ttl 30m] \
      --i-understand-this-spends-money                          # acquire-only smoke test
calque real    --bucket B --run-id ID [--ami AMI] [--instance g6.2xlarge] \
      [--model HF_REPO] [--n 1] [--shards 1] \                  # single instance, or --shards N to fan out
      --i-understand-this-spends-money                          #   across a fleet (§15)
calque session --bucket B --run-id ID [--ami AMI] [--instance g7e.2xlarge] \
      [--rungs 1,100,1000] \                                    # acquire once, hold, run every rung on it
      --i-understand-this-spends-money
```

The route-away gate runs on `run`/`real`/`session` too: if the model (or `--model`) is already an exact
Bedrock API call, calque prints the offer and stops **before** acquiring anything.

## Design notes

Decisions that shape what the spike *doesn't* do live in `docs/` as durable records:

- [`docs/serve-architecture.md`](docs/serve-architecture.md) — why serve entrypoints are detected +
  leaked but the long-lived server is not built (§16 success is batch + a measured K).
- [`docs/behind-the-seam-register.md`](docs/behind-the-seam-register.md) — every §1/§4/§18 non-goal
  with the attach point a future build would touch, so "won't-port" stays a documented decision, not a
  silent gap.

## Layout

Directory tree follows the spike spec §12, with one rename: the spec's worker
supervisor is called `spored`, but that name is already the spore.host lifecycle
daemon (systemd service on every instance). Ours is **`warmd`** (`worker/warm-runner/`),
which runs *under* the real spored. Project tracking lives on GitHub (Issues /
Projects / milestones), not in local files.

## Build

```
go build ./...          # control plane
cd tools/pyast && uv sync   # Python AST helper deps
```

## License

Apache 2.0. See `LICENSE`.
