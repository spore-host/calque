# Measured run — Qwen2.5-1.5B on g7e.2xlarge (RTX PRO 6000), N-ramp 1/100/1000

**First measured K on the intended card (RTX PRO 6000), via a spot acquisition.**
The prior headline run used an L4 (`g6.2xlarge`) capacity fallback; this run landed
the card the spike actually targets. Verdict at every rung: **STAY ON MODAL** — at
these single-stream occupancies AWS's per-item cost never undercuts Modal, which is
the phase detector working, not a failure.

> **Rate caveat:** this is a **SPOT** acquisition, so `R_a` is a spot rate and the
> instance is interruptible. This K is therefore *not* the on-demand headline K —
> it answers "does AWS win at this occupancy on a spot g7e," and the honest answer
> at ≤45% occupancy is no. calque emitted this caveat as a leak at run time.

> **Occupancy scope caveat (#71, added 2026-08-06):** every occupancy figure in this
> record is a **`whole_run`** mean — averaged over the entire rung *including the
> ~173 s one-time `@enter` model load*, when the GPU is idle by definition. It
> therefore **understates steady-state GPU fill**. It cannot be recomputed: this run's
> samples were not timestamped, so windowing them now would mean inventing data.
> Per-item seconds, `enter_seconds`, result counts, and the STAY-ON-MODAL verdicts are
> unaffected — and since a whole-run occupancy double-counts the load idle (once as low
> occupancy, again as `enter_seconds`), these verdicts were conservative in Modal's
> favor, not AWS's. Runs after #71 report `scope: inference`.

## Provenance

| Field | Value |
|---|---|
| Recording commit | `933a33c` (`feat(session): spot acquisition + prep-visibility fix`) |
| Run date | 2026-07-28 (UTC) |
| Run id | `g7e-spot-use1-20260728b` |
| Region / AZ | us-east-1 / us-east-1c |
| Instance | `g7e.2xlarge` **spot** (acquired in 17s, 2 attempts) |
| GPU | NVIDIA RTX PRO 6000 (the intended card) |
| Model | `Qwen/Qwen2.5-1.5B-Instruct` |
| AMI | `ami-0f29bf1fbe374da5f` (DL Base OSS NVIDIA Driver, Ubuntu 24.04, 2026-07-24) |
| Bucket | `calque-spike-942542972736-use1` (the bucket the `spored-instance-role` S3 policy grants) |
| Command | `calque session --bucket … --run-id g7e-spot-use1-20260728b --ami ami-0f29bf1fbe374da5f --instance g7e.2xlarge --region us-east-1 --rungs 1,100,1000 --ttl 3h --spot --prep-timeout-min 30 --i-understand-this-spends-money` |

## Measured results (from each rung's `summary.json` + host `occupancy-host.json`)

Occupancy is reported dual-metric; **DCGM SM-activity is the accurate source**
(host-sampled), nvsmi is shown for comparison. All values `measured`, `enter_count=1`.

All occupancy columns are `scope: whole_run` (they include the `@enter` load — see the
scope caveat above).

| Rung | @enter load (s) | per-item (s) | occupancy DCGM (host, `whole_run`) | occupancy nvsmi | results | verdict |
|---|---|---|---|---|---|---|
| N=1    | 207.39 | 0.404 | 0.4% | 0.4% | 1/1, 0 missing | STAY ON MODAL |
| N=100  | 172.37 | 0.358 (mean) | **12.2%** | 18.1% | 100/100, 0 missing | STAY ON MODAL |
| N=1000 | 172.89 | 0.359 (mean) | **45.5%** | 62.3% | 1000/1000, 0 missing | STAY ON MODAL |

Notes:
- **RTX PRO 6000 per-item ≈ 0.36–0.40 s** — roughly 4–5× faster than the L4's 1.70 s
  (2026-07-16 N=1 run).
- **Occupancy climbs with N** (0.4% → 12% → 45%) — but read this carefully. It is
  *mostly an artifact of the averaging window*: as N grows, the fixed ~173 s load is a
  smaller share of the run, so a `whole_run` mean rises even if GPU fill during
  inference is unchanged. The real load-amortization economics live in `enter_seconds`
  ÷ N, which the K math already handles. Since #71, the occupancy column measures fill
  during inference only, and it should be roughly *flat* across these rungs (same
  single-stream B=1 work at every N) rather than climbing.
- **Ordered collection held**: 0 missing at every rung, including 1000/1000.

## K at N=1000 (spot R_a, 45% occupancy)

Cost projection printed by the run (Modal vs AWS spot on-demand-equivalent):

```
  10        Modal: $0.19  | AWS: $0.19   AWS wins
  100       Modal: $0.23  | AWS: $0.25   Modal wins
  1000      Modal: $0.58  | AWS: $0.92   Modal wins
  10000     Modal: $4.13  | AWS: $7.56   Modal wins
  100000    Modal: $39.58 | AWS: $73.96  Modal wins
Crossover:  none in range — at 45% occupancy AWS's per-item cost never undercuts Modal.
Verdict:    STAY ON MODAL.
```

## Reproduction — 2026-07-29 (cross-region: eu-central-1 compute, us-east-1 bucket)

Reproduced the next day on a different spot region (us-east-1 on-demand *and* spot
were tight; eu-central-1 had spot capacity). Run `g7e-spot-euc1-20260729c`,
instance `i-04802d44ace0a2d91`, acquired in 4s. This run also **validated the
cross-region S3 fix** (commits `3fadcfe` + `64172a3`): compute in eu-central-1,
artifacts/results in the us-east-1 bucket the IAM role grants.

| Rung | @enter (s) | per-item (s) | occupancy DCGM | results | vs 07-28 |
|---|---|---|---|---|---|
| N=1    | 212.7 | 0.405 | 0% | 1/1, 0 missing | 0.404 ✓ |
| N=100  | 177.1 | 0.362 | 12% | 100/100, 0 missing | 0.358 ✓ |
| N=1000 | 177.7 | 0.365 | 39% | 1000/1000, 0 missing | 0.359 ✓ |

Per-item and occupancy reproduce within noise; verdict STAY ON MODAL at every rung.
(Two cross-region bugs were fixed to get here: the first attempt that day 301'd on
the control-plane artifact upload, the second on warmd's manifest read. This third
run is the clean one.)

## Caveats

- **Spot rate, interruptible** — see the banner above. An on-demand K needs on-demand
  capacity, which was exhausted region-wide at run time (#18); spot was the only way
  to land a g7e.
- **Occupancy is single-stream (B=1)** — one prompt at a time leaves the GPU largely
  idle even at N=1000 (45%). Raising occupancy (batching, concurrency) or buying down
  the rate (Savings Plan / reserved) is what would move the verdict — the tool says so.
  Micro-batching was tried next and did make inference **24× faster**
  ([`2026-07-29-qwen-g7e-batch32.md`](2026-07-29-qwen-g7e-batch32.md)) — and exposed
  that this occupancy metric was measuring the wrong window (#71).
- **Occupancy scope is `whole_run`** and not recomputable — see the caveat at the top.
- **Why the earlier attempts failed** (context, now fixed): (1) calque couldn't request
  spot; (2) a prep-visibility bug hid the real failure, which was (3) instances lacking
  S3 write to a fresh bucket outside the role policy. All resolved in `933a33c`; this
  run is the clean end-to-end result.
