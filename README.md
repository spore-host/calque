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
- **plan** (§5) — truffle resolve+price, `Acquirer` over `spawn.Provision` with capacity-aware retry.
- **image / exec** — Dockerfile+digest cache; S3 sink/collector; on-instance `warmd` entrypoint.
- **full pipeline** — `calque run --dry-run` runs every stage end-to-end locally.

**Real hardware:** the acquire-only smoke path is built and gated behind `--i-understand-this-spends-money`.
First launches hit sustained real `InsufficientInstanceCapacity` on g7e/g6e/g6 in us-west-2 (spec §2
predicted g7e is regionally thin) — the deadline guard gave up cleanly, **zero spend, no leaked instances**.
A defensible measured K awaits GPU capacity + a real payload.

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
