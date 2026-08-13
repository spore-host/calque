# Getting started

This walks from a fresh clone to your first REAL, billable AWS run — the path
[`examples/README.md`](../../examples/README.md) deliberately stops short of
(it stays entirely zero-spend). If you just want to see calque's story without
touching AWS at all, start there instead; come back here when you're ready to
run something real.

## 0. Prerequisites

- **Go 1.26** (matches `go.mod`) and **Python 3** with
  [`uv`](https://docs.astral.sh/uv/) — needed for everything below, including
  the zero-spend steps.
- **For the billable steps (3 onward):** AWS credentials in your normal
  credential chain (env vars, `~/.aws/credentials`, an assumed role, etc.)
  with permission to run EC2 instances, read/write the S3 bucket you'll use,
  and create/attach the IAM instance profile calque manages for you
  (`internal/plan/iam.go` — scoped narrowly to that bucket, nothing account-wide).
  An **S3 bucket** you're willing to read/write is the only AWS resource you
  need to create yourself ahead of time; calque creates and tears down
  everything else (EC2 instances, IAM roles/profiles) per run.

Nothing below launches AWS infrastructure until step 3 explicitly says so —
and even then, every billable command refuses to run without an explicit
`--i-understand-this-spends-money` flag (see the "Cost/risk gating" note at
the top of [`cli-reference.md`](cli-reference.md) for the full taxonomy —
`pool delete` and `session checkout --backend mps` use two *different*
confirm flags, since they guard against different kinds of risk).

## 1. Build

```
git clone https://github.com/spore-host/calque && cd calque
go build -o calque ./cmd/calque      # control plane
(cd tools/pyast && uv sync)          # Python AST helper deps
```

## 2. See what calque thinks of a script — zero spend

`analyze` runs every static pass (parse → IR, `gpu=` swap-legality guard,
Bedrock route-away gate, volume mapping, leak report) and never touches AWS:

```
./calque analyze examples/map_batch_inference.py
```

Try this on your OWN Modal script too, right now — real output, not
canned:

```
./calque analyze /path/to/your_script.py
```

Read every line of its leak report. Each one is a self-contained statement
of something calque either translated, recorded-but-didn't-act-on, or
refused outright — zero leaks means the script matches calque's execution
shape exactly. [`../porting-modal-to-aws.md`](../porting-modal-to-aws.md)
walks through exactly what each construct's leak means and what to do
about it if you have a real app you're trying to port.

Then run the full pipeline locally, still with no spend:

```
./calque run --n 20 --dry-run examples/map_batch_inference.py
```

This drives the SAME script's warm unit through parse → gate → plan →
warm-execute → collect, locally, using a CPU stand-in body instead of a real
GPU. It's the closest you can get to "did this actually run" before paying
for anything.

## 3. The first billable action: `calque smoke`

Before spending anything on real inference, run the acquire-only smoke
test. It exists *specifically* to de-risk the riskiest new integration —
acquire an instance, bring it up, run a trivial one-item job on the bare
host (no docker/GPU/model), confirm the result lands in S3, then
terminate — before you trust any of that machinery with a real, more
expensive job.

```
./calque smoke --bucket YOUR-BUCKET --run-id smoke-$(date +%Y%m%d-%H%M) \
  --i-understand-this-spends-money
```

This costs a few cents and a few minutes. If it fails, it fails **before**
you've committed to a GPU instance or a real model download — read the
error and the bootstrap log it uploads to S3 either way
(`s3://YOUR-BUCKET/runs/<run-id>/bootstrap.log`); see
[`troubleshooting.md`](troubleshooting.md) if it's not obvious.

## 4. Your first real run: `calque real --n 1`

Once `smoke` succeeds, run real inference with the smallest possible N —
one prompt, one GPU, exit as soon as it's done:

```
./calque real --bucket YOUR-BUCKET --run-id real-$(date +%Y%m%d-%H%M) \
  --instance g6.2xlarge --model Qwen/Qwen2.5-1.5B-Instruct --n 1 \
  --i-understand-this-spends-money
```

This is real: it acquires a real GPU instance, loads a real model, runs
real inference, collects a real result from S3, and terminates the
instance. Watch the numbered `[N/8]` progress lines — they name each stage
(build, upload, acquire, run, collect, terminate) so a failure tells you
exactly which stage it happened in.

## 5. Running YOUR script for real, not the reference vLLM body

Steps 3–4 drove calque's own hardcoded reference body (vLLM against an HF
model) — useful for validating the AWS plumbing, but not your actual
script. To drive your OWN script's real body on real AWS, add `--script`:

```
./calque real --bucket YOUR-BUCKET --run-id myapp-$(date +%Y%m%d-%H%M) \
  --instance m6i.large --script /path/to/your_script.py \
  --i-understand-this-spends-money
```

calque parses the script, picks the warm unit it would drive (a `@cls`'s
`@enter`+method, or a plain `@app.function`), and ships **that function's
own verbatim body** to run on the instance — not a stand-in. If the
script's real signature needs more than calque's default item model can
express — real secrets, a real file's bytes as one of several positional
args, extra pip dependencies calque couldn't statically resolve, a file
your script's body expects at a hardcoded path — see `calque real`'s flags
in [`cli-reference.md`](cli-reference.md) for
`--secret`/`--item-file`/`--arg-file`/`--arg-json`/`--pip`/`--stage-file`,
and [`which-verb.md`](which-verb.md) for `--function` (selecting a specific
callable by name when it isn't reachable through any
`@app.local_entrypoint()`).

This exact path — real script, real data, real AWS hardware, result
verified byte-for-byte against a local reference run — is how calque's own
v0.3.0 milestone was proven: two functions from a real customer's Modal app
ran unmodified on real AWS and produced identical results to running them
locally. [`../../CHANGELOG.md`](../../CHANGELOG.md)'s `[0.3.0]` entry has
the full account if you want the detailed trail (including two real bugs
that live validation caught that a dry-run never would have).

## What next

- **Not sure which command to use for your workload** (one-off vs. repeated
  N-testing vs. a shared warm pool vs. `.spawn()` fan-out vs. slicing an
  already-running instance)? See [`which-verb.md`](which-verb.md).
- **Something failed and the error isn't self-explanatory?** See
  [`troubleshooting.md`](troubleshooting.md).
- **Does calque support the specific Modal construct my script uses?** See
  [`../modal-compatibility-matrix.md`](../modal-compatibility-matrix.md).
- **Every flag, for every command, exactly as the code accepts it:** see
  [`cli-reference.md`](cli-reference.md).
