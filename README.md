# calque

**Run Modal-shaped code at AWS scale, unchanged.**

Modal is where inference/batch code is *prototyped* — great inner loop, pay-nothing-when-idle.
AWS is where the same code *scales* — you own the rectangle and the economics flip at volume.
calque is the loan-translation between the two: the same script that ran over 10 items on Modal
runs over 10,000,000 on AWS **without a logic rewrite** (only a mechanical `gpu=` substitution).

> A *calque* is a structure-preserving translation between languages (English "flea market" ←
> French *marché aux puces*). That is the job: translate Modal's idioms onto AWS term-by-term,
> structure intact, so the author doesn't notice the translation.

**This is a spike.** It exists to prove one thing and fake everything else:

- **Prove:** the plumbing carries Modal's semantics onto AWS, and produce the **crossover K** —
  the workload scale at which AWS becomes cheaper than Modal, from a **real measured run**, not a model.
- **Fake behind the seam:** all card-selection / cost-optimization intelligence. The recommender
  returns a constant (`RTX PRO 6000`) behind an interface. See `internal/target`.

calque is a **phase detector, not a sales funnel**: below K the honest verdict is "stay on Modal."

## Status

Spike, in active build. Tracking lives on GitHub (Issues / Projects / milestones), not local files.

**Built, tested, and verified (no spend):**
- `parse → IR → seam` — pyast helper, six-primitive IR, `StubRecommender` (constant behind an interface).
- **Bedrock gate** (§11) — live catalog, `exact`/`near`/`none` tiers, proven in both directions.
- **`gpu=` guard** (§7) — clean-swap vs multi-GPU/coupled flags, adversarial fixture.
- **warm worker** (`warmd` + `runner.py`, §6) — `@enter` once, crash-restart re-drive, partial-failure.
- **cost + crossover K** (§9) — rate asymmetry (`R_m` for card asked-for vs `R_a` for card substituted-to),
  willing to say *stay on Modal*, `measured | proxy` flag.
- **plan** (§5) — truffle resolve+price, `Acquirer` over `spawn.Provision` with capacity-aware
  AZ-sweep retry.
- **image / exec** — Dockerfile+digest cache; S3 sink/collector; on-instance `warmd` entrypoint.
- **volumes** (§3/§15) — `Volume.from_name` → stable S3 prefix, delta-synced to the mount path
  before `@enter` (warm-cache reuse; image/volume separation).
- **full pipeline** — `calque run --dry-run` runs every stage locally; `calque session` acquires one
  GPU and runs an N-ramp on it.

**Real measured crossover K — achieved on a live GPU.** A real run (Qwen2.5-1.5B on an L4, all
`[measured]`, no proxies) produced the headline number:

- N=100: `@enter` ran **once** (102.7s load), **1.583s/item**, **59% measured occupancy** → **K ≈ 73 items**
  on-demand (~18 with a Savings Plan); verdict at 100k = **CROSS**.
- N=1: same load amortized over one item → 5% occupancy → **STAY ON MODAL**.

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
   └─ gate     Bedrock eligibility?    (static; if hit, recommend & stop)
     └─ recommend  IR → Target         (STUB: constant behind Recommender interface)
       └─ plan   truffle: Card → candidate g7e instances (+ live price = R_a)
                 acquire: block-and-wait retry over spawn.Provision until landed
                 image:   .image DSL → Dockerfile → ECR (cache by digest)
         └─ exec   spawn.Provision launches + brings up the instance
                   [worker] warmd supervises warm Python: @enter once, drain → S3
           └─ collect   gather from S3, ordered by input index
             └─ measure per-item cost + occupancy (tach hook)
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
calque run [--n N] [--dry-run] <script.py>            # full pipeline → crossover K
calque smoke --bucket B --run-id ID \                 # acquire-only real-hardware smoke test
      --i-understand-this-spends-money                #   (gated; launches a billable instance)
```

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
