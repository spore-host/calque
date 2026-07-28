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

| Rung | @enter load (s) | per-item (s) | occupancy DCGM (host) | occupancy nvsmi | results | verdict |
|---|---|---|---|---|---|---|
| N=1    | 207.39 | 0.404 | 0.4% | 0.4% | 1/1, 0 missing | STAY ON MODAL |
| N=100  | 172.37 | 0.358 (mean) | **12.2%** | 18.1% | 100/100, 0 missing | STAY ON MODAL |
| N=1000 | 172.89 | 0.359 (mean) | **45.5%** | 62.3% | 1000/1000, 0 missing | STAY ON MODAL |

Notes:
- **RTX PRO 6000 per-item ≈ 0.36–0.40 s** — roughly 4–5× faster than the L4's 1.70 s
  (2026-07-16 N=1 run).
- **Occupancy climbs with N** (0.4% → 12% → 45%) as the one-time ~173 s model load
  amortizes across more items — the load-amortization economics the ramp exists to
  measure.
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

## Caveats

- **Spot rate, interruptible** — see the banner above. An on-demand K needs on-demand
  capacity, which was exhausted region-wide at run time (#18); spot was the only way
  to land a g7e.
- **Occupancy is single-stream (B=1)** — one prompt at a time leaves the GPU largely
  idle even at N=1000 (45%). Raising occupancy (batching, concurrency) or buying down
  the rate (Savings Plan / reserved) is what would move the verdict — the tool says so.
- **Why the earlier attempts failed** (context, now fixed): (1) calque couldn't request
  spot; (2) a prep-visibility bug hid the real failure, which was (3) instances lacking
  S3 write to a fresh bucket outside the role policy. All resolved in `933a33c`; this
  run is the clean end-to-end result.
