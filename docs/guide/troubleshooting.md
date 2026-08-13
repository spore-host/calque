# Troubleshooting

**Status:** Authoritative current behavior. Verified through: v0.3.0.

Real, previously-hit problems, told as symptom → cause → fix. Most were
found and fixed during calque's own live-AWS validation passes (see
[`../../CHANGELOG.md`](../../CHANGELOG.md) for the full account of each);
they're collected here so the next person hits a documented answer instead
of re-discovering the cause from a bootstrap log.

This page is scoped to calque's own CLI/runtime behavior. Broader AWS
capacity/quota/spot-interruption behavior is owned by the underlying
acquisition layer (`spawn`/`lagotto`, via `internal/plan.Acquirer`) — real,
but a different codebase's territory; not covered here.

## A `smoke`/`real`/`ramp` run hangs for the whole `--deadline-min`, then fails with no bootstrap log at all

**Cause:** the instance launched with no IAM instance profile — nothing
authenticated to write to S3, including the bootstrap log's own
failure-upload step (calque#148). This was a real, previously-shipped bug
in every single-instance real-AWS launch path (`real`, `smoke`, `ramp`,
`spawn-run`, and fleetrun's dedicated-instance fallback); it's fixed as of
the commit that closed calque#148, but if you're on an older build, this is
almost certainly the cause of a total silent hang.

**Fix:** update to a build containing `internal/plan/iam.go`
(`RealRunInstanceProfile`) — every affected launch path now attaches a
scoped instance profile automatically; there is nothing you need to
configure. If you still see this on a current build, check that your AWS
credentials have IAM permission to create/attach instance profiles (the
profile itself is scoped narrowly to your run's own S3 bucket, not
account-wide).

## `calque real --script ... --pip PACKAGE` fails fast with `No module named 'xarray'` (or any other real dependency)

**Cause:** host mode (the path a `--script`-picked unit's real body runs
under) has no dependency-install step by default — your script's real
`pip_install(...)` chain isn't always statically resolvable (e.g. built via
a factory function), so calque can't always discover and install it for
you automatically.

**Fix:** pass the real dependency list explicitly:
`--pip PACKAGE --pip OTHER_PACKAGE --python-version 3.11`. `--pip` accepts
a plain PyPI name or a full spec, including a git URL when the package has
no PyPI release: `--pip "momp @ git+https://github.com/hholb/ROMP.git@main"`.

## `--pip` with a git-URL spec fails with `Git executable not found`

**Cause:** Amazon Linux 2023 — the AMI `spawn` auto-selects for non-GPU
instance types like `m6i.large` — doesn't ship `git` by default. `uv pip
install` needs a real `git` binary on `PATH` to resolve a git-URL package
spec.

**Fix:** update to a current build — the bootstrap script now ensures
`git` is present (apt-get first, dnf fallback) before any `uv pip install`
step whenever `--pip` is used. If you're pinning an older AMI yourself and
still hit this, install `git` in your own bootstrap customization, or avoid
git-URL specs for that AMI.

## `uv pip install`-related bootstrap failure: `<workerDir>/.venv/bin/uv: No such file or directory`

**Cause:** `uv venv` creates a plain venv (just `python3`/`pip`) — it does
NOT copy the `uv` binary itself into that venv. An install invocation that
expects `<venv>/bin/uv` to exist is wrong.

**Fix:** already fixed in the bootstrap script — it invokes the top-level
`uv` with `--python <venv>/bin/python3` (uv's own documented way to target
an existing venv), not a nonexistent venv-local `uv`. If you're
hand-rolling a similar bootstrap step yourself outside calque, use the same
pattern.

## A real script's function returns something with raw bytes nested in it (e.g. a list of `{filename, data: bytes}` dicts) and every item fails with `Object of type bytes is not JSON serializable`

**Cause:** the warm runner's output protocol is JSON over a pipe; plain
`json.dumps` has no way to encode a Python `bytes` object, and a real
function's return value can embed bytes anywhere in an arbitrary nested
structure (not just as the single top-level payload — e.g. a results list
where each entry's `data` field is a file's raw contents).

**Fix:** already fixed — `runner.py`'s JSON encoder now base64-encodes any
bytes value it finds, at any nesting depth, via a `json.dumps(default=...)`
hook. On a current build this should not occur; if it does, check whether
your build predates that fix.

## `calque real --item-file`/`--arg-file` and `--n` together — which one wins?

**Cause/clarification, not a bug:** `--item-file` and `--arg-file`/
`--arg-json` replace the item batch entirely with exactly one real item —
they're mutually exclusive with each other and make `--n` irrelevant for
item construction (it stays meaningful only for the vLLM-reference-body
path when no `--script`/`--item-file`/`--arg-file` is given at all).

**Fix:** if you meant to combine a real file payload with multiple
synthesized items, that's not supported — `--item-file`/`--arg-file`
always produce exactly one item today. Use one item's real data to
validate correctness first, then reason separately about scale.

## `calque real --script ...` picks the wrong function (not the one I actually wanted)

**Cause:** without `--entrypoint`/`--function`, calque auto-selects a
callable via `pickWarmUnit`'s scan (preferring a `.map()`'d `@cls`+`@enter`
unit, falling back to the first plain function it finds) — if your target
function isn't reachable through any `@app.local_entrypoint()` at all (a
real shape: one entrypoint might call a *different*, GCS-backed sibling,
leaving your intended function unreachable via entrypoint-based selection
entirely), the scan will not find it by intention, only by accident.

**Fix:** use `--function NAME` to select the exact callable by name,
bypassing the automatic scan entirely. See
[`which-verb.md`](which-verb.md#reals-own-sub-choices-once-youre-driving-your-own-script).

## A multi-entrypoint script's `--entrypoint` value is accepted but the wrong callable still runs

**Clarification, not a bug:** `--function`, when given, always takes
priority over `--entrypoint`'s own selection — `--entrypoint` is still
validated (an unknown name still errors) but no longer determines which
callable is driven once `--function` names one directly. If you passed
both and expected `--entrypoint` to win, that's the wrong mental model —
`--function` is a direct override.

## Fleet mode (`--shards N`): a shard using `.starmap()`, sibling functions/constants, or module-level imports mis-binds or `NameError`s under `calque pool`/fleet-worker-pool mode, but the identical manifest runs fine through a single dedicated instance

**Cause:** a historical bug where `Worker.runOne`'s `warm.Config`
construction (the fleet-worker-pool path) silently dropped several fields
(`MethodArgs`/`Starmap`/`Extras`/`ExtraConsts`/`ExtraImports`/
`ExtraClasses`) that the single-instance path (`runOnInstance`) already
carried correctly — calque#146/#147 fixed this by mirroring the full field
set across both paths.

**Fix:** update to a current build. If you still see a manifest behave
differently between a dedicated single-instance run and a pooled/fleet
run, that's a regression worth filing an issue for — the two paths are
supposed to be behaviorally identical.

## A bare module-level `import`, plain (non-`@app.cls`) helper class, function, or constant reference `NameError`s at `@enter`/method time despite the script parsing cleanly

**Cause:** the warm runner execs your script's `@enter`/method bodies in
an isolated globals dict — anything they reference has to be explicitly
shipped alongside them. Historically, calque shipped `.local()`-referenced
sibling functions (calque#92), then bare-referenced module-level
functions/constants (calque#139), then bare-referenced imports and plain
classes too (calque#146/#147) — each was a real, separate gap found by
running actual real-world scripts, not a hypothetical.

**Fix:** on a current build, all four (functions, constants, imports,
plain classes) are shipped automatically via transitive free-reference
resolution — you shouldn't need to do anything. If you still hit a
`NameError` for a bare reference to something module-level in your own
script, check `calque analyze`'s leak report first (it names exactly which
references it could/couldn't resolve) before assuming it's a new gap.

## I killed a run (Ctrl-C, closed the terminal, lost network) before it finished — is an instance still running?

**Cause:** calque's own clean-teardown logic (`defer ... Terminate(...)`) runs
on any NORMAL error return from the Go process, including Ctrl-C's SIGINT
in most terminals — but a hard kill (SIGKILL, terminal process killed
out-of-band, laptop sleep during a long `ramp`/fleet run) bypasses Go's
`defer` mechanism entirely. Nothing in the local process survives to run
the termination call.

**Fix:**
1. **`--ttl` is the real backstop**, not a courtesy — every acquired
   instance has a hard lifetime cap (`--ttl`, e.g. `40m` for `real`, `3h`
   for `ramp`) that spawn/spored enforce independent of whether the
   calque CLI process is even still running. If you're not in a hurry,
   waiting out the TTL costs nothing extra to fix (though you still pay
   for the compute until it expires).
2. **To find and terminate it immediately** instead of waiting:
   ```
   aws ec2 describe-instances --region YOUR-REGION \
     --filters "Name=instance-state-name,Values=running,pending" \
     --query 'Reservations[].Instances[].[InstanceId,LaunchTime,InstanceType]' \
     --output table
   ```
   calque doesn't currently tag instances with the `--run-id` you passed
   (a real gap, worth filing if it bites you), so identify the right one
   by launch time/instance type rather than a tag filter. Then:
   ```
   aws ec2 terminate-instances --region YOUR-REGION --instance-ids i-XXXXXXXX
   ```
3. **The IAM role/instance profile is NOT something you need to clean up**
   — it's a persistent, shared, reused resource across every run (see
   [`getting-started.md`](getting-started.md)'s resource-inventory note),
   not a per-run artifact left behind by an interrupted run.
4. **S3 objects under `runs/<run-id>/`** are harmless to leave — they're
   just artifacts/results, cost negligible storage, and don't represent
   ongoing spend the way a running instance does.

## Where do I actually look when something fails and this page doesn't cover it?

1. **The run's own numbered progress lines** (`[N/8] ...`) — they name
   which stage failed (build, upload, acquire, run, collect, terminate).
2. **The bootstrap log**, uploaded to
   `s3://YOUR-BUCKET/runs/<run-id>/bootstrap.log` on the instance's exit —
   uploaded on BOTH success and failure, so it's there even when the run
   never got far enough to print anything useful locally.
3. **The leak report** — a failure that isn't a hard crash usually shows up
   here first, as a `[kind] owner: detail (file:line)` line explaining
   exactly what calque couldn't do and why.
4. **[`../../CHANGELOG.md`](../../CHANGELOG.md)** — every fix above (and
   many more, including several institutional-tenancy/pool-specific ones
   not repeated here) has a full writeup with the exact commit and issue
   number.
