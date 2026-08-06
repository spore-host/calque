# Measured runs

Auditable provenance records for crossover-K runs on **real hardware**. calque's
whole claim is a measured K, not a modeled one — so every headline number a
skeptic might challenge gets a committed record here: the exact commit, instance,
model revision, command, rate-table version, raw output, and caveats.

This is a deliberate exception to the "leak reports are reproduced, not committed"
rule (see the top-level README): a *headline provenance record* is committed so the
claim is auditable after the ephemeral run is gone.

| Run | Rung | Card / instance | Verdict | Status |
|---|---|---|---|---|
| [`2026-07-29-qwen-g7e-batch32.md`](2026-07-29-qwen-g7e-batch32.md) | N=1000, B=32 | **RTX PRO 6000** / g7e.2xlarge (spot) | STAY ON MODAL (24× faster per item) | ✅ complete (measured; occupancy `whole_run`) |
| [`2026-07-28-qwen-g7e-spot-ramp.md`](2026-07-28-qwen-g7e-spot-ramp.md) | 1/100/1000 | **RTX PRO 6000** / g7e.2xlarge (spot) | STAY ON MODAL (0.4%→12%→45% occ) | ✅ complete (measured; occupancy `whole_run`) |
| [`2026-07-16-qwen-l4-n1.md`](2026-07-16-qwen-l4-n1.md) | N=1 | L4 / g6.2xlarge | STAY ON MODAL | ✅ complete (from git; occupancy `whole_run`) |
| [`TEMPLATE-qwen-l4-n100.md`](TEMPLATE-qwen-l4-n100.md) | N≈100 | L4 / g6.2xlarge | CROSS (K≈73) | ⚠️ template — an **on-demand** N≈100 K is still unmeasured (the g7e run above is spot-rate) |

## ⚠️ Every occupancy figure above is a `whole_run` mean (#71)

All runs recorded before the #71 fix computed occupancy as busy-GPU-seconds ÷
**whole-run** wall-clock — a window that *includes the one-time `@enter` model load*
(~100–210 s), during which the GPU is idle by definition. So those figures **understate
steady-state GPU fill**, and worse, they move the *wrong way* when the workload gets
faster: the batch-32 run made inference 24× faster and its occupancy *fell* to 2%,
because a fixed 210 s load came to dominate a 15 s inference window.

**These numbers cannot be retroactively corrected.** The fix needs per-sample
timestamps correlated against warmd's inference spans, and those runs' raw samples
were never timestamped (and the host JSONL was not uploaded). Recomputing them would
mean inventing data. So they stand as recorded, explicitly labeled `whole_run`, and a
future run produces the first `inference`-scoped figures.

What this does *not* change: per-item seconds, `enter_seconds`, result counts, and
every **verdict** are unaffected. A whole-run occupancy makes AWS look *worse*
(the load idle is counted twice — once as low occupancy, again as `enter_seconds`), so
each STAY-ON-MODAL verdict was, if anything, conservative in Modal's favor.

Since #71, occupancy carries an explicit `scope`: `inference` (item work only — what K
should stand on) or `whole_run` (load-contaminated). Both are reported, and an
unlabeled figure is read as `whole_run`.

## Adding a run

Copy [`TEMPLATE-qwen-l4-n100.md`](TEMPLATE-qwen-l4-n100.md), rename it
`YYYY-MM-DD-<model>-<card>-n<N>.md`, and fill every field. A field you can't fill
is a caveat, not a blank — say so explicitly. Never round or reconstruct a raw
number from memory; paste the actual run output.

**Always record occupancy's `scope`** alongside its value (and the sample count).
An occupancy number without its averaging window is not auditable: the same run
yields 45% or 2% depending on whether the model load is inside the window.
