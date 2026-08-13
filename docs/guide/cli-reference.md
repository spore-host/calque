# CLI reference

**Status:** Authoritative current behavior. Verified through: v0.3.1.

Every flag calque's CLI accepts, sourced directly from the `flag.NewFlagSet`
calls in `cmd/calque/*.go` (mainly `main.go`, `smoke.go`, `ramp.go`,
`pool.go`, `spawnrun.go`, `session.go`) — the code, not a hand-remembered
summary. If a flag ever seems to behave differently than described here,
the Go source at the cited file wins; please file an issue against this
page rather than silently trusting either one.

**Cost/risk gating.** calque uses three distinct confirm flags, not one,
because the risk each guards against is a different *kind*:

| Flag | Guards against | Required on |
|---|---|---|
| `--i-understand-this-spends-money` | ordinary AWS billing (acquiring an instance) | `smoke`, `real`, `ramp`, `pool create`/`scale`, `spawn-run` |
| `--i-understand-this-terminates-instances` | destroying running infrastructure other commands don't touch | `pool delete` |
| `--i-understand-shared-gpu-has-no-isolation` | a *correctness/security* risk (no hardware isolation between tenants), not spend | `session checkout --backend mps` only — `--backend mig` needs none, since MIG is hardware-isolated |

Every billable command refuses to run at all without its flag — there is
no way to accidentally launch something expensive.

## `calque analyze <script.py> [<script.py> ...]`

No flags. Runs the static passes (parse → IR, `gpu=` swap-legality guard,
Bedrock route-away gate, volume mapping, leak report) over one or more
scripts and exits. Never touches AWS. See `cmd/calque/main.go`'s
`analyze()`.

## `calque run [flags] <script.py>`

Source: `cmd/calque/main.go`'s `runCmd`. The full pipeline, run locally —
`--dry-run` (the default) means this command **never launches a billable
instance**; there is no way to make `run` spend money.

| Flag | Default | Meaning |
|---|---|---|
| `--n` | `100000` | Item count the cost verdict is located against. |
| `--region` | `us-west-2` | AWS region used for acquisition/pricing context (dry-run still prices as if it would launch here). |
| `--dry-run` | `true` | Drive the warm worker locally on a synthetic sample instead of launching. Always `true` today — dropping it is gated pending explicit authorization. |
| `--rates` | `config/rates.json` | Path to the dated rate table used for the cost verdict. |
| `--entrypoint` | `""` | Which `@app.local_entrypoint()` to select when the script has more than one (mimics `modal run file.py::entrypoint`, calque#90). Required if the script has 2+ entrypoints and this is left unset — see [`which-verb.md`](which-verb.md) for how entrypoint selection interacts with `--function` on `real`. |

## `calque smoke [flags]` — billable

Source: `cmd/calque/smoke.go`'s `parseSmokeArgs`. The **first billable
action** — acquires an instance, runs `warmd` on the bare host (no
docker/GPU/model) over a trivial one-item job, confirms the result lands in
S3, then terminates. Exists specifically to de-risk acquire → bring-up →
collect → terminate before spending on real inference; see
[`getting-started.md`](getting-started.md).

| Flag | Default | Meaning |
|---|---|---|
| `--bucket` | *(required)* | S3 bucket for artifacts/results. |
| `--region` | `us-west-2` | AWS region. |
| `--run-id` | *(required)* | Unique run id, e.g. `smoke-20260101-1200`. |
| `--ttl` | `30m` | Instance TTL hard cap — spawn reaps the instance at this even mid-run. |
| `--deadline-min` | `20` | Give up ONLY the acquire/wait-for-capacity phase after N minutes. |
| `--instance` | `""` → `g7e.2xlarge` | Override the resolved instance type (capacity fallback). |
| `--ami` | `""` | Pin the AMI; empty lets spawn auto-select. |
| `--spot` | `false` | Acquire on the Spot market instead of on-demand. |
| `--spot-max-price` | `""` | Spot bid cap in $/hr; empty caps at the on-demand price. |
| `--i-understand-this-spends-money` | `false` | **Required.** Refuses to launch without it. |

## `calque real [flags]` — billable

Source: `cmd/calque/main.go`'s `parseRealArgs`. Runs real inference/compute
against acquired capacity — a single instance by default, a fleet with
`--shards N`, or an existing warm pool with `--pool`.

| Flag | Default | Meaning |
|---|---|---|
| `--bucket` | *(required)* | S3 bucket. |
| `--region` | `us-east-1` | AWS region. |
| `--run-id` | *(required)* | Unique run id. |
| `--instance` | `g6.2xlarge` | GPU instance type. |
| `--ami` | `""` | Pin the AMI; empty auto-selects (verified working on g6/g6e/g7/g7e, calque#75). |
| `--model` | `Qwen/Qwen2.5-1.5B-Instruct` | HF model repo id — must NOT be an exact Bedrock match, or the route-away gate stops the run before any spend. |
| `--n` | `1` | Number of prompts/items to drive through the hardcoded vLLM reference body (ignored once `--script` picks a real unit with its own real iterable — see `--script` below). |
| `--shards` | `1` | Fan `.map()` out across N single-node instances acquired in parallel (fleet mode, §15). `1` = single instance. Mutually exclusive with `--pool`. |
| `--ttl` | `40m` | Hard cap on **each shard's whole runtime** (acquire + bootstrap + work) — not just acquisition. For a fleet run, set this comfortably above your expected per-shard work time. |
| `--deadline-min` | `40` | Give up ONLY the acquire/wait-for-capacity phase after N minutes — unrelated to `--ttl`. |
| `--rates` | `config/rates.json` | Rate table path. |
| `--pool` | `false` | Submit to an existing warm pool (`calque pool create --model M`) instead of self-acquiring a dedicated instance (calque#103). |
| `--spot` | `false` | Acquire on the Spot market. |
| `--spot-max-price` | `""` | Spot bid cap in $/hr. |
| `--script` | `""` | Parse a real Modal script and drive **its own parsed body** instead of the hardcoded vLLM reference — see [`getting-started.md`](getting-started.md) for a worked example. Also unlocks `.map()`/`.starmap()` real-iterable extraction (calque#136). Empty reproduces the pre-existing synthesized-prompt behavior exactly. |
| `--entrypoint` | `""` | Which `@app.local_entrypoint()` to select when `--script` has 2+ (mimics `modal run file.py::entrypoint`, calque#90). Required if ambiguous. |
| `--function` | `""` | Drive one specific `@app.function`/`@cls` method **by name**, bypassing automatic entrypoint/`.map()`-preference selection entirely. Takes priority over `--entrypoint`. Needed when the target callable isn't reachable through any entrypoint at all (e.g. a sibling function only a *different* entrypoint invokes). See [`which-verb.md`](which-verb.md). |
| `--secret NAME=VALUE` | none, repeatable | Injects an environment variable into the runner's process before `@enter` runs — the generic counterpart to Modal's `secrets=[...]`. `realrun.go` leaks which of the script's own declared secret names weren't covered by a `--secret` you passed. |
| `--item-file PATH` | `""` | The file's raw bytes become the **single** real item driven through the picked unit's body — for a signature like `def f(input_bundle: bytes)`. Mutually exclusive with `--n`'s synthesized/literal items and with `--arg-file`/`--arg-json`. |
| `--arg-file IDX=PATH` | none, repeatable | The picked unit's real signature is a tuple of positional args; position `IDX`'s value becomes `PATH`'s raw bytes (base64-round-tripped). For a signature that mixes bytes with non-bytes args — e.g. `def f(job_id: str, config: dict, bundle: bytes)` — where `--item-file`'s single-whole-payload-is-bytes model doesn't apply. Pair with `--arg-json` for the other positions; **every** position from 0 up to the highest index given must be covered by exactly one of `--arg-file`/`--arg-json`, or the run refuses. |
| `--arg-json IDX=JSON` | none, repeatable | Position `IDX`'s value is this literal JSON value (string/number/object/etc.), unmarshaled and passed through unchanged. The non-bytes sibling of `--arg-file`. |
| `--pip PACKAGE` | none, repeatable | Third-party Python package to `uv install` on the instance before running a `--script`-picked unit's real body (calque#148) — needed when the script's own `pip_install(...)` chain wasn't statically resolvable (e.g. built via a factory function). Accepts a plain PyPI name or a full `uv`-style spec, including a git URL (`"momp @ git+https://github.com/hholb/ROMP.git@main"`). |
| `--python-version X.Y` | `""` | Python version for `uv` to install on the instance — only meaningful alongside `--pip`. Empty lets `uv` pick its own current default. |
| `--stage-file URL=PATH` | none, repeatable | Downloads `URL` to the absolute `PATH` on the instance (creating parent directories) **before** `warmd` runs — for a script body that shells out to a hardcoded absolute path its original Docker image would have placed there. |
| `--i-understand-this-spends-money` | `false` | **Required.** |

**Flag interactions worth knowing up front:**
- `--item-file` and `--arg-file`/`--arg-json` are mutually exclusive — pick one payload model per run.
- `--pool` and `--shards N` (N>1) don't compose — a pooled run always targets one resident worker; use `--shards` for a self-acquired fleet instead.
- `--function` silently wins over `--entrypoint` when both are given; `--entrypoint` is still validated (a nonexistent name still errors), it just no longer determines which callable gets driven once `--function` names one directly.

## `calque ramp [flags]` — billable

Source: `cmd/calque/main.go`'s `rampCmd`. Acquires **one** instance
patiently (accepting a long acquire wait, since it only has to succeed
once), holds it, then drives a whole N-ramp across it over SSM — the
efficient way to test several values of N without re-paying acquisition
every time.

| Flag | Default | Meaning |
|---|---|---|
| `--bucket` | *(required)* | S3 bucket. |
| `--region` | `us-east-1` | AWS region. |
| `--run-id` | *(required)* | Unique session id. |
| `--instance` | `g7e.2xlarge` | GPU instance type to hold for the whole ramp. |
| `--ami` | `""` | Pin the AMI; empty auto-selects. |
| `--model` | `Qwen/Qwen2.5-1.5B-Instruct` | HF model repo id (must NOT be an exact Bedrock match). |
| `--rungs` | `1,100,1000` | Comma-separated N-ramp to run sequentially on the held instance. |
| `--ttl` | `3h` | Instance TTL hard cap, held across the **whole** ramp — size generously. |
| `--acquire-deadline-min` | `180` | Patient acquisition window in minutes ($0 spent until capacity actually lands). |
| `--rates` | `config/rates.json` | Rate table path. |
| `--spot` | `false` | Acquire on the Spot market. |
| `--spot-max-price` | `""` | Spot bid cap in $/hr. |
| `--fallback-regions` | `""` | Comma-separated regions to try, in order, if `--region` has no capacity (calque#95). Empty = single-region only. |
| `--prep-timeout-min` | `30` | Minutes to wait for the one-time docker image pull before giving up. |
| `--concurrency` | `1` | Items in flight per rung, for THREAD-SAFE bodies only (guarded off for vLLM-offline — use `--batch-size` there instead). |
| `--batch-size` | `1` | Items per micro-batch — one vLLM `.generate(list)` call fills the GPU; the real occupancy lever. `1` = per-item. |
| `--script` | `""` | Parse a real Modal script for its real `.map()`/`.starmap()` iterable (calque#136), same semantics as `real`'s `--script`. Note `ramp` has no `--entrypoint`/`--function`/`--secret`/`--item-file`/`--pip` equivalents today — those are `real`-only (see [`which-verb.md`](which-verb.md) for when that matters). |
| `--i-understand-this-spends-money` | `false` | **Required.** Holds a billable GPU for potentially hours. |

## `calque pool <subcommand>`

Source: `cmd/calque/pool.go`. A model-scoped SQS queue with resident
workers that keep a loaded model warm across separate claims instead of
paying `@enter`'s load cost every run. See
[`docs/pool-queue-contract.md`](../pool-queue-contract.md) for the design
rationale.

### `calque pool create [flags]` — billable

| Flag | Default | Meaning |
|---|---|---|
| `--model` | *(required)* | Pool identity — every claim on this pool's queue targets this SAME warm model. |
| `--region` | `us-east-1` | AWS region. |
| `--instance-type` | *(required)* | GPU instance type for every worker. |
| `--workers` | `1` | Number of workers to request. |
| `--min-viable` | `1` | Minimum workers that must come up for the pool to be considered ready (best-effort above this). |
| `--spot` | `false` | Launch workers on the Spot market. |
| `--spot-max-price` | `""` | Spot bid cap in $/hr. |
| `--ttl` | `12h` | Hard lifetime cap per worker instance. |
| `--idle-timeout` | `30m` | How long a worker keeps its resident runner warm with an empty queue before closing it. |
| `--manifest-bucket` | *(required)* | S3 bucket claims' manifests are staged to. |
| `--results-bucket` | *(required)* | S3 bucket workers write results+summaries to. |
| `--runner-path` | *(required)* | Path to `runner.py` in the worker image. |
| `--ami` | `""` | Pin the AMI; empty auto-detects (same as spawn pool create). |
| `--i-understand-this-spends-money` | `false` | **Required.** |

### `calque pool scale [flags]` — billable

Same shape as `create`, with `--add-workers` (default `1`) instead of
`--workers`/`--min-viable`. `--instance-type` should match the pool's
existing workers.

### `calque pool delete [flags]` — destructive

| Flag | Default | Meaning |
|---|---|---|
| `--model` | *(required)* | Pool identity to delete. |
| `--region` | `us-east-1` | AWS region. |
| `--i-understand-this-terminates-instances` | `false` | **Required.** Terminates EVERY running worker instance in this pool and deletes its queue. |

### `calque pool status [flags]` / `calque pool list [flags]` — read-only

`--model` (required), `--region` (`us-east-1`). No confirm flag — these
never touch billing. `list` reports on the one named `--model` pool, not a
registry of every pool that exists — calque keeps no such registry (see
the doc comment in `pool.go` for why).

## `calque spawn-run [flags] <script.py>` — billable

Source: `cmd/calque/spawnrun.go`. Block-and-wait `.spawn()`/`.get()`
fan-out — one instance per distinct `.spawn()`-classified callable in the
script, typically plain CPU functions rather than GPU inference.

| Flag | Default | Meaning |
|---|---|---|
| `--bucket` | *(required)* | S3 bucket. |
| `--region` | `us-east-1` | AWS region. |
| `--run-id` | *(required)* | Unique run id. |
| `--instance` | `m7i.large` | Instance type for every spawned callable — a homogeneous fleet. |
| `--ami` | *(required)* | Pinned AMI — spawn's GPU auto-select workaround doesn't apply to CPU instance types, and CPU-type auto-select has its own known issues, so pin explicitly. |
| `--ttl` | `20m` | Instance TTL hard cap per spawned callable's instance. |
| `--deadline-min` | `15` | Give up acquiring/waiting after N minutes. |
| `--rates` | `config/rates.json` | Accepted for CLI symmetry with `real`/`session`; unused by `spawn-run` today. |
| `--i-understand-this-spends-money` | `false` | **Required.** Launches one billable instance PER spawned callable. |

## `calque session <subcommand>`

Source: `cmd/calque/session.go`. Checks a MIG slice or MPS client-slot
in/out on an instance **someone else already acquired** — `session` never
launches or terminates an EC2 instance itself. See
[`docs/tenancy-vs-session.md`](../tenancy-vs-session.md) for the scope
boundary and why this verb was renamed from the old N-ramp command (now
`calque ramp`).

### `calque session checkout [flags]`

| Flag | Default | Meaning |
|---|---|---|
| `--instance-id` | *(required)* | EC2 instance ID of the ALREADY-RUNNING instance to check out a slice on. |
| `--user` | *(required)* | User identity to bind the slice to. |
| `--backend` | *(required)* | `mig` (hardware-isolated) or `mps` (cooperative, no isolation). |
| `--ttl` | `2h` | Bounded interactive TTL — the slice is reclaimed automatically if never explicitly checked in. |
| `--instance-type` | `g7e.2xlarge` | Used ONLY to size the MIG layout via `internal/mig`'s live-verified profile catalog, when `--backend mig`. |
| `--slots` | `4` | Number of MPS client-slots to model on this instance, when `--backend mps` (MPS has no fixed hardware layout — this is an operator choice). |
| `--i-understand-shared-gpu-has-no-isolation` | `false` | **Required when `--backend mps`.** Not required for `--backend mig`. |
| `--spot` | `false` | The underlying instance was acquired on the Spot market — surfaces the compounding blast-radius risk to concurrent tenants at checkout time (calque#119); informational, never itself calls EC2. |

### `calque session checkin [flags]`

`--slice` (required, slice ID returned by checkout), `--session-token`
(required, token returned by checkout — checkin refuses without an exact
match, and does NOT release the slice on mismatch).

### `calque session status [flags]` / `calque session list [flags]`

`--instance-id` (required). Read-only — reports live occupancy and
per-slice holders.
