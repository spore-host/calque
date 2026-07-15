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

Spike, in active build. See `docs/DECISIONS.md` for the calls made and `docs/LEAKS.md`-style
structured output emitted at runtime (`internal/leak`).

## Pipeline

```
script.py
 └─ parse      decorators → IR         (shallow AST; bodies extracted verbatim)
   └─ gate     Bedrock eligibility?    (static; if hit, recommend & stop)
     └─ recommend  IR → Target         (STUB: constant behind Recommender interface)
       └─ plan   truffle: Card → candidate g7e instances
                 lagotto: snipe target, retry until acquired
                 image:   .image DSL → Dockerfile → ECR (cache by digest)
         └─ exec   spawn: launch acquired instance
                   [worker] spored supervises warm Python: @enter once, drain → S3
           └─ collect   gather from S3, ordered by input index
             └─ measure per-item cost + occupancy (tach hook)
               └─ report cost comparison + crossover K + leaks
```

The Go control plane understands **decorators** (configuration). It does **not** parse function
**bodies** (payload) — those ship to the worker verbatim and run under Python exactly as on Modal.

## Layout

See `docs/DECISIONS.md`. Directory tree follows the spike spec §12.

## Build

```
go build ./...          # control plane
cd tools/pyast && uv sync   # Python AST helper deps
```

## License

Apache 2.0. See `LICENSE`.
