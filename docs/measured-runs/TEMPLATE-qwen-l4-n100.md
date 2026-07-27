# Measured run — Qwen2.5-1.5B, N≈100 — TEMPLATE (needs raw output)

> **⚠️ This is a template, not yet a record.** The README cites headline N≈100
> numbers (K ≈ 73 items on-demand, ~18 with a Savings Plan; 1.583 s/item; 59%
> measured occupancy; verdict CROSS at 100k). Those numbers are **claimed but not
> yet backed by a committed raw artifact** — they live in the run operator's
> records. Fill every `<FILL: …>` below from the actual run output, then remove
> this banner and rename to `YYYY-MM-DD-qwen-l4-n100.md`. Do not reconstruct a raw
> number from memory — paste it.

**Claimed verdict: CROSS (K ≈ 73 items on-demand).** The N≈100 run amortizes the
one-time model load across enough items to raise occupancy past the point where
owning the AWS rectangle beats Modal's per-second billing.

## Provenance

| Field | Value |
|---|---|
| Recording commit | `<FILL: SHA of the commit that records this run>` |
| Commit / run date | `<FILL: YYYY-MM-DD>` |
| Run id | `<FILL: e.g. real-n100-XXXXXX>` |
| Region | `<FILL: e.g. us-east-1>` |
| Instance | `<FILL: e.g. g6.2xlarge>` |
| GPU | `<FILL: e.g. NVIDIA L4>` |
| Model | `Qwen/Qwen2.5-1.5B-Instruct` (`<FILL: exact HF revision / commit hash>`) |
| N (items) | `<FILL: actual N, ~100>` |
| Rate table | `config/rates.json` @ `<FILL: which entry + its dated source line>` |

## Command

```
calque real --bucket <FILL> --run-id <FILL> --ami <FILL: pinned AMI> \
  --instance <FILL> --model Qwen/Qwen2.5-1.5B-Instruct --n <FILL> \
  --region <FILL> --i-understand-this-spends-money
```

(If run via the acquire-once ramp instead, record the `calque session … --rungs …`
command actually used.)

## Raw measurements

Paste the run's actual measurement block verbatim:

```
@enter_count  = <FILL: expect 1 — warm-once>
enter_seconds = <FILL: e.g. 102.7>
per_item      = <FILL: claimed 1.583 s>
occupancy     = <FILL: claimed 59% — state measured (nvidia-smi/DCGM) + sample count>
missing[]     = <FILL: any dropped item indices, or "none">
```

## Crossover K

Paste the `--- crossover K (§9) ---` block verbatim, then record:

- K (on-demand): `<FILL: claimed ~73 items>`
- K (1-yr Savings Plan): `<FILL: claimed ~18 items>`
- Verdict at your N and at 100k: `<FILL: e.g. CROSS at 100k>`
- AWS side of K: `<FILL: [measured] | [proxy]>`

## Caveats

- **Occupancy metric:** state whether occupancy is nvidia-smi or DCGM SM-activity,
  and the sample count. (Project history notes nvidia-smi can misreport; DCGM is the
  accurate source — record which one this is.)
- **Card substitution:** if this ran on an L4 (`g6.2xlarge`) rather than the intended
  RTX PRO 6000 (g7e) due to capacity, say so — the K is honest for the card it ran on.
- **`[proxy]` inputs:** list any input flagged proxy rather than measured.
- Anything you could not fill above is itself a caveat — name it here rather than
  leaving a silent blank.
