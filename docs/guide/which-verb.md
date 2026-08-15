# Which command should I use?

**Status:** Authoritative current behavior. Verified through: v0.4.0.

calque has one workload shape (a Modal-decorated script's warm unit, driven
over real or synthetic items) but several ways to acquire and drive
hardware for it. This page is purely "which one, when" — for the *why*
behind each design, follow the links into `docs/`.

## Decision table

| You want to... | Use | Why not the others |
|---|---|---|
| Just see if my script parses / would run, no spend | `calque analyze` then `calque run --dry-run` | Nothing else is free. |
| Confirm the AWS plumbing (acquire/bring-up/collect/terminate) works before trusting it with real money | `calque smoke` | `real`/`ramp`/`pool`/`spawn-run` all assume the plumbing already works — `smoke` is the thing that proves it, cheaply. |
| Run one real job, once, on its own dedicated instance | `calque real` (no `--shards`, no `--pool`) | `ramp` holds the instance for MULTIPLE N values, which you don't need for one run; `pool` is for repeat/shared use, which adds setup cost for a single run. |
| Run the same script at several different N values without re-paying acquisition each time | `calque ramp` | `real` acquires fresh (and pays acquisition cost) every invocation; `ramp` acquires once, holds, and drives every rung over SSM. Use this for "try N=1, then N=100, then N=1000" testing. |
| Fan a `.map()`-shaped workload out across many items faster than one instance can serial-drain them | `calque real --shards N` | This is `real`'s own fleet mode — N single-node instances acquired in parallel, sharded by item index, collected back into one ordered result set. Not a separate command. |
| Serve MANY separate runs/claims against the SAME warm model without reloading it each time | `calque pool create`, then `calque real --pool` | `real` (without `--pool`) reloads the model fresh (`@enter`'s cost) every single invocation; a pool keeps resident workers warm across claims via a model-scoped SQS queue. Worth the setup cost once you have more than a handful of runs against the same model. See [`../pool-queue-contract.md`](../pool-queue-contract.md). |
| Run a script whose Modal code uses `.spawn()`/`.get()` to fan out across DIFFERENT callables (not one `.map()`'d callable over many items) | `calque spawn-run` | `real --shards` shards ONE callable's item list across instances; `spawn-run` instead gives each DISTINCT `.spawn()`-classified callable in the script its own instance — a different fan-out shape entirely. |
| Give multiple users their own isolated (or cooperative) slice of ONE already-running GPU instance | `calque session checkout` | Nothing else touches an instance after it's already up — `session` is the only verb that doesn't itself acquire or terminate EC2 instances; it operates on hardware `real`/`ramp`/`pool`/`smoke` already stood up. See [`../tenancy-vs-session.md`](../tenancy-vs-session.md). |

## `real`'s own sub-choices, once you're driving your own script

`--script /path/to/your_script.py` makes `real` (and `ramp`) parse and drive
a real script's own body instead of the hardcoded vLLM reference. From
there:

- **Script has 0 or 1 `@app.local_entrypoint()`s, or you're fine with
  whichever callable calque's automatic scan picks:** do nothing extra.
- **Script has 2+ entrypoints and you need a specific one:** add
  `--entrypoint NAME`.
- **The callable you actually want isn't reachable through ANY
  entrypoint** — e.g. it's a sibling function only a *different*
  entrypoint invokes, the exact shape AI-Almanac's `run_benchmark_local`
  hit (its only entrypoint calls a different, GCS-backed sibling) — use
  `--function NAME` to select it directly, by name. `--function` wins over
  `--entrypoint` whenever both are given.
- **Your target function's real signature takes a single `bytes` arg**
  (e.g. `def f(input_bundle: bytes)`): use `--item-file PATH`.
- **Your target function's real signature mixes `bytes` with other typed
  args** (e.g. `def f(job_id: str, config: dict, bundle: bytes)`): use
  `--arg-file IDX=PATH` for the bytes position(s) and `--arg-json
  IDX=JSON` for everything else — every position must be covered by
  exactly one of the two.
- **The script needs env vars / secrets before `@enter` runs:** `--secret
  NAME=VALUE`, repeatable.
- **The script's own `pip_install(...)` chain wasn't statically
  resolvable** (e.g. built via a factory function calque can't trace
  through): `--pip PACKAGE` (repeatable, accepts git-URL specs too) +
  optionally `--python-version`.
- **The script's body shells out to a hardcoded absolute path** its
  original Docker image would have put something at (e.g. a generator
  script baked into a base image): `--stage-file URL=PATH` downloads it
  there first.

Full flag-by-flag detail: [`cli-reference.md`](cli-reference.md).

## A note on `ramp`/`real --shards` vs. `pool`/`session`

`ramp` and `real --shards` both **acquire and own** the EC2 instance(s)
they use for exactly one command's lifetime. `pool` and `session` are the
two primitives built for hardware that OUTLIVES a single invocation —
`pool` keeps a set of workers warm across many separate claims over time;
`session` lets several users share one already-running instance
concurrently. If you're reaching for `ramp`/`real --shards` and find
yourself wanting the instance to still be around for your NEXT invocation
too, that's the signal you actually want `pool` or `session` instead. See
[`../m12-m13-boundary.md`](../m12-m13-boundary.md) for how these two layer
on top of the base acquire/release primitive.
