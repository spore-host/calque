# Getting started

**Status:** Authoritative current behavior. Verified through: v0.6.0.

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
  credential chain (env vars, `~/.aws/credentials`, an assumed role, etc.).
  An **S3 bucket** you're willing to read/write is the only AWS resource you
  need to create yourself ahead of time; calque creates and tears down
  everything else (EC2 instances, IAM roles/profiles) per run.

### What IAM permissions does my caller identity need?

Two different things need permissions, and they're easy to conflate:

1. **Your own credentials** (the ones running `calque` locally) need to be
   able to: launch/terminate EC2 instances, read/write the S3 bucket, and
   create/attach an IAM role+instance profile (`iam:CreateRole`,
   `iam:CreateInstanceProfile`, `iam:AddRoleToInstanceProfile`,
   `iam:PutRolePolicy`, `iam:AttachRolePolicy`, `iam:PassRole`) — an admin
   role satisfies this trivially; a narrowly-scoped role needs all of the
   above explicitly. `calque pool` and `calque fleetrun` additionally need
   `iam:DeleteRolePolicy` (to remove a pool's or fleet run's queue grant
   when it is torn down), and use `iam:GetRolePolicy` and
   `iam:ListRolePolicies` best-effort, to migrate the pre-calque#226 shared
   policy and to explain a full role.

   **Tagging (calque#227).** calque tags everything it creates (see the
   next section), which needs: `iam:TagRole` and `iam:TagInstanceProfile`
   (tagging the role and profile at create, and bringing an existing one's
   tags up to date; the reads use `iam:GetRole` and `iam:GetInstanceProfile`,
   which the role setup already needs), `s3:PutObjectTagging` on your bucket
   (S3 refuses a tagged `PutObject` without it), `ec2:CreateTags` (which
   launching already needs), and for `calque pool`/`calque fleetrun`
   `sqs:TagQueue` and `sqs:ListQueueTags`. A missing tagging permission
   never fails a run: calque creates or writes the resource untagged and
   prints a `[tags] WARNING` naming the permission. An untagged resource
   works, but calque's cleanup will not recognise it as calque's.

   **Footprint and teardown (calque#228).** `calque footprint` only reads:
   `iam:ListRoles`, `iam:GetRole`, `iam:ListRolePolicies`,
   `iam:GetRolePolicy`, `iam:ListAttachedRolePolicies`,
   `iam:ListInstanceProfiles`, `iam:GetInstanceProfile`,
   `sqs:ListQueues`, `sqs:GetQueueAttributes`, `sqs:ListQueueTags`,
   `ec2:DescribeInstances`, `ec2:DescribeSecurityGroups`,
   `ec2:DescribeNetworkInterfaces`, `ec2:DescribeImages`,
   `ec2:DescribeSnapshots`, `ec2:DescribeRegions` (for `--all-regions`), and
   for `--bucket` `s3:ListBucket` and `s3:GetObjectTagging`.
   `calque teardown` re-reads with the same calls plus
   `iam:ListInstanceProfilesForRole`, `sqs:GetQueueUrl`,
   `ec2:DescribeSecurityGroupRules` and `s3:GetObject` (for `HeadObject`),
   and with `--mode reap` deletes with `sqs:DeleteQueue`,
   `iam:DeleteRolePolicy`, `iam:DetachRolePolicy`,
   `iam:RemoveRoleFromInstanceProfile`, `iam:DeleteInstanceProfile`,
   `iam:DeleteRole`, `ec2:RevokeSecurityGroupIngress`,
   `ec2:RevokeSecurityGroupEgress`, `ec2:DeleteSecurityGroup` and
   `s3:DeleteObject`. The teardown lease needs `dynamodb:GetItem`,
   `dynamodb:UpdateItem` and `dynamodb:DeleteItem` on the
   `spore-host-lifecycle` table in `--lease-region`. Teardown never calls
   `iam:DeletePolicy`, `ec2:TerminateInstances` or
   `ec2:DeregisterImage`.

   **The last mile (spore-host/libs#78).** After most commands calque
   spends a moment checking whether a calque teardown in your account was
   left half-done, and notes what it would do about it. It only reports:
   it deletes nothing, writes nothing to the lifecycle table, never prints
   to stdout and never changes the exit status, and it checks at most once
   every 6 h (its state is in `~/.spore-host/lifecycle.json`). It needs
   `dynamodb:GetItem` and `dynamodb:Scan` on the `spore-host-lifecycle`
   table in `us-east-1`, which none of the permissions above include; if
   it finds a stale teardown, it also uses the `calque footprint` reads
   above to list what that teardown would still delete. Without them it
   skips quietly: grant those two DynamoDB reads to enable it. Set
   `CALQUE_DEBUG=1` to see what it checked (on stderr), and
   `SPORE_LASTMILE=off` to turn it off.
2. **The EC2 instance itself** gets a SEPARATE, narrower role that calque
   creates and manages for you — you never need to hand-write this one.
   `internal/plan/iam.go`'s `RealRunPolicy` grants:
   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:PutObjectTagging"], "Resource": ["arn:aws:s3:::YOUR-BUCKET/*"]},
       {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:GetBucketLocation"], "Resource": ["arn:aws:s3:::YOUR-BUCKET"]},
       {"Effect": "Allow", "Action": ["ecr:GetAuthorizationToken"], "Resource": ["*"]},
       {"Effect": "Allow", "Action": ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"], "Resource": ["arn:aws:ecr:REGION:ACCOUNT:repository/*"]}
     ]
   }
   ```
   The S3 statements are scoped to the ONE bucket you passed via `--bucket`
   — nothing account-wide. `s3:PutObjectTagging` (calque#227) lets the
   instance tag the results and summary it writes. The ECR statements (calque#176) are granted
   unconditionally, not just when a given run's script actually pulls a
   registry image, since the role is reused across every future run
   against this bucket rather than recreated per run;
   `ecr:GetAuthorizationToken` can't be resource-scoped at all (AWS
   requires `Resource: "*"` for that specific action). If your script uses
   an inline `modal.CloudBucketMount(...)` (calque#91 Workstream A), the
   role ALSO gets the same S3 read/write/list statements — scoped to that
   mount's own bucket, separate from your `--bucket` staging area — added
   automatically per script, not something you configure. Additionally,
   `AmazonSSMManagedInstanceCore` is attached automatically (needed so
   spawn's own acquisition/liveness machinery can reach the instance) —
   this is spawn's convention, not something calque adds on top.

### What does calque actually create in my AWS account?

Per run: one EC2 instance (terminated at the end, or on error via a
deferred cleanup — see "if a run is interrupted" below), and a handful of
S3 objects under `s3://YOUR-BUCKET/runs/<run-id>/` (artifacts, manifest,
results, summary, bootstrap log). The instance carries `calque:run-id`,
`calque:managed=true`, and `calque:command` tags (calque#166) — useful for
finding it by tag filter if a run gets interrupted; see
[`troubleshooting.md`](troubleshooting.md).

**Everything calque creates is tagged** (calque#227) with the spore.host
set `spore-host:managed-by=calque`, `spore-host:scope`,
`spore-host:created` and `spore-host:schema`, plus `calque:managed=true`.
That is how `calque footprint` (calque#228) finds what calque made, and
nothing else:

| Resource | Scope | Also tagged |
|---|---|---|
| EC2 instances (real, smoke, ramp, spawn-run, fleet, ami bake, gpuprobe) | `run:<run-id>` (none for gpuprobe, which has no run id) | `calque:run-id`, `calque:command` |
| Pool worker instances | `pool:<model>` | `calque:pool-model`, `calque:role` |
| Fleet worker instances | `run:<run-id>` | `calque:fleet-run`, `calque:run-id` |
| SQS `calque-pool-<model>` | `pool:<model>` | `calque:pool-model` |
| SQS `calque-fleet-<run-id>` | `run:<run-id>` | `calque:fleet-run`, `calque:run-id` |
| IAM role + profile `calque-real-run-<hash>` | `bucket:<bucket>` | |
| IAM roles + profiles `calque-pool-worker` / `calque-fleet-worker` | `pool-workers` / `fleet-workers` | |
| Security group `calque-nfs-sg` | `vpc:<vpc-id>` | |
| Baked AMI and its snapshots | `run:<run-id>` | `calque:baked-image`, `calque:baked-at` |
| S3 objects calque writes from Go (artifacts, manifest, Dockerfile, results, summary) | `run:<run-id>` | `calque:run-id` (no `spore-host:created`: an object's LastModified says when) |

An instance calque launches through spawn also carries spawn's own
`spawn:*` tags, and spawn's TTL backstop keys on those, so it still
applies. Its `spore-host:managed-by` is `calque`, not `spawn`, because
calque owns its lifecycle (it terminates the instance, drains the fleet,
deletes the pool). An IAM role, security group or queue that an older
calque created untagged is tagged the next time calque uses it.

Not tagged yet: objects the instance writes with the AWS CLI rather than
from Go (the bootstrap log, the `ami bake` done marker and log, the
occupancy files, and `Volume` commits via `aws s3 sync`), since `aws s3 cp`
can't tag. A message retention period expires a queue's messages, never
the queue itself; a leftover queue stays until it is deleted.

**One thing that's NOT per-run**: the IAM role+instance profile itself is
created ONCE PER BUCKET and REUSED across every subsequent real run
against that SAME bucket, in every region — `CreateOrGetInstanceProfile`
is idempotent and reuses the existing role rather than recreating it. The
role name is derived from the bucket (`calque-real-run-<hash>`, calque#167)
rather than one name shared across every bucket you ever pass: `PutRolePolicy`
replaces a role's inline policy document wholesale rather than merging it,
so two overlapping runs against DIFFERENT buckets sharing one role could
otherwise race — a second run's launch could silently drop a first run's
still-in-flight bucket grant. Two runs against the SAME bucket still
correctly share one role, as before. If you audit your account after a run
and still see one of these roles, that's expected — each is a persistent,
shared-per-bucket resource, not something a `--i-understand-this-*` flag
ever tears down. `calque footprint --all-regions` lists it, and once no
live instance uses it, `calque teardown` removes it with its instance
profile (there's nothing sensitive stored in it — it's just a capability
grant, scoped to that one bucket).

**Finding and removing what calque left behind.** `calque footprint`
lists everything above, says which of it is in use, and which is litter.
Pass `--all-regions`, since an IAM role can only be called unused when no
instance in any region uses it, and `--bucket` for each bucket you ran
against, since calque keeps no record of them. Its JSON is the plan
`calque teardown` acts on: report mode (the default) says what would go,
and `--mode reap --yes` deletes it. Baked AMIs and run results are yours
and are never deleted; see `docs/guide/cli-reference.md`.

**Pool and fleet workers** (`calque pool`, `calque fleetrun`) run under two
shared roles, `calque-pool-worker` and `calque-fleet-worker`. Each pool or
fleet run gets its OWN inline policy on that role, named `q-<12 hex>` from
its queue's ARN (calque#226), granting that queue and the pool's buckets.
It is added when the pool or fleet run is provisioned and removed by
`calque pool delete` or the fleet run's cleanup; other pools' grants are
never touched. IAM caps a role's inline policies at 10,240 bytes in total,
which fits about a dozen concurrent pools (or fleet runs): each grant is
about 700 bytes, and spawn's own baseline policy on the role about 1.3 KB. Past that, provisioning fails with an error saying so
rather than falling back to a broader grant; delete pools you no longer
need. Older calque versions wrote every pool's grant into one shared
`spawn-scoped-policy`, so concurrent pools revoked each other's queue
access. If a role still has that policy, calque logs it, leaves it in
place, and removes it once the queue it grants is torn down (or a
re-provisioned pool's own grant duplicates it). A legacy policy whose
queue an older calque already deleted is not removed automatically.

### What region does calque use?

`--region` defaults differently per command: `us-east-1` for `real`/
`ramp`/`pool`/`spawn-run`, `us-west-2` for `run`/`smoke`. Pass `--region`
explicitly if you need a specific one — GPU instance family capacity
(especially for `g6e`/`g7e`) is genuinely region-dependent, so if an
acquisition hangs at the wait-for-capacity stage, trying a different
region is a real, common fix (see
[`troubleshooting.md`](troubleshooting.md)).

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

This is billable but intentionally small — check the current on-demand
price for the selected instance type in your region before proceeding. If
it fails, it fails **before** you've committed to a GPU instance or a real
model download — read the error and the bootstrap log it uploads to S3
either way (`s3://YOUR-BUCKET/runs/<run-id>/bootstrap.log`); see
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
