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
| [`2026-07-16-qwen-l4-n1.md`](2026-07-16-qwen-l4-n1.md) | N=1 | L4 / g6.2xlarge | STAY ON MODAL | ✅ complete (from git) |
| [`TEMPLATE-qwen-l4-n100.md`](TEMPLATE-qwen-l4-n100.md) | N≈100 | L4 / g6.2xlarge | CROSS (K≈73) | ⚠️ **template — needs raw output** |

## Adding a run

Copy [`TEMPLATE-qwen-l4-n100.md`](TEMPLATE-qwen-l4-n100.md), rename it
`YYYY-MM-DD-<model>-<card>-n<N>.md`, and fill every field. A field you can't fill
is a caveat, not a blank — say so explicitly. Never round or reconstruct a raw
number from memory; paste the actual run output.
