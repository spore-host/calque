# GPU sharing support matrix (calque#104): live hardware verification

**Status:** Authoritative current behavior. Verified through: v0.3.1.
Resolves the g7 MIG ambiguity that NVIDIA's own
datasheets could not settle (Workstation Edition datasheet omits MIG;
developer blog confirms MIG only for Server Edition; the MIG User Guide's
Supported-GPUs table lists the card family with no edition qualifier).
Verified 2026-08-08 by acquiring one real instance per family via calque's
own `plan.Acquirer` (spot, `cmd/gpuprobe`) and running `nvidia-smi -L`,
`nvidia-smi mig -lgip`/`-lgi`, `nvidia-smi -mig 1`, and a live
`nvidia-cuda-mps-control -d` start on each — ground truth, not vendor
literature.

## The headline finding

**AWS deploys the Server Edition of both RTX PRO 4500 Blackwell (g7) and
RTX PRO 6000 Blackwell (g7e)** — `nvidia-smi -L` on both reports "Server
Edition" explicitly. The earlier datasheet research assumed g7 shipped the
Workstation Edition (matching AWS's marketing description, "1 x NVIDIA RTX
PRO 4500 GPU," which doesn't name an edition) — that assumption was WRONG.
Once the edition is known, the ambiguity dissolves: NVIDIA's own developer
blog already said Server Edition supports MIG, and live hardware confirms
it.

## Results

| Card | Instance | Edition (nvidia-smi -L) | MIG | MPS | Live profiles (mig -lgip) |
|---|---|---|---|---|---|
| L4 | g6.2xlarge | — (no edition name; consumer/OEM-style naming) | **No** — `Unable to enable MIG Mode ... Not Supported` | **Yes** — daemon started (`nvidia-cuda-mps-control -d`, pid confirmed via `ps`) | n/a — "No MIG-supported devices found" |
| L40S | g6e.2xlarge | — (no edition name) | **No** — `Unable to enable MIG Mode ... Not Supported` | **Yes** — daemon started | n/a — "No MIG-supported devices found" |
| RTX PRO 4500 Blackwell | g7.2xlarge | **Server Edition** | **Yes** — `Enabled MIG Mode for GPU ...` succeeded | **Yes** — daemon started | `1g.16gb` (+gfx/+me variants), `2g.32gb` (+gfx) — max 2 instances, matches the MIG User Guide's documented profile set for this card |
| RTX PRO 6000 Blackwell | g7e.2xlarge | **Server Edition** | **Yes** — `Enabled MIG Mode for GPU ...` succeeded | **Yes** — daemon started | `1g.24gb` (+gfx/+me/-me variants), `2g.48gb` (+gfx/+me/-me), `4g.96gb` (+gfx) — max 4 instances, matches "Universal MIG" up to 4x24GB/2x48GB/1x96GB |

Every card's driver reports `Driver Version: 595.91.07`, `CUDA Version:
13.2` — the same stock driver across all four families (via the AMI `Deep
Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 22.04) 20260804`, which AWS's
own image description explicitly lists as supporting G4dn/G5/G6/Gr6/G6e/G7/
G7e/P4d/P4de/P5/P5e/P5en/P6). No custom AMI or driver bake is needed for
either MIG or MPS on any of the four families — the stock, AWS-published
DLAMI already has MIG-capable drivers where the silicon supports it.

## SharingMode table (ready for `internal/target`)

```go
var sharingModeByFamily = map[string]SharingMode{
    "g6":  MPS, // L4:  no MIG (confirmed), MPS works
    "g6e": MPS, // L40S: no MIG (confirmed), MPS works
    "g7":  MIG, // RTX PRO 4500 Blackwell Server Edition: MIG confirmed (max 2 instances, up to 2g.32gb)
    "g7e": MIG, // RTX PRO 6000 Blackwell Server Edition: MIG confirmed (max 4 instances, up to 4g.96gb)
}
```

Every family supports MPS as a fallback (confirmed live on all four) — a
card entered as `MIG` in the table can still run in MPS mode as an explicit
opt-in (per M13's institutional-sharing design), it's just that MIG is the
DEFAULT/preferred mode where hardware-isolated slicing is available.

## Method note: MPS-before-MIG ordering

The first probe pass attempted `nvidia-smi -mig 1` before testing MPS, and
the MPS daemon start failed on that run (no process, no error in the log —
silently didn't start). Re-running with MPS tested FIRST (before any MIG
mode change, which requires a GPU reset) succeeded on every card. This is
consistent with NVIDIA's own documented behavior: enabling MIG mode
requires the GPU to be reset to take effect, and a card left in that
transitional state can reject a subsequent plain (non-MIG) CUDA context
that MPS needs. Anyone re-running this probe (or building the real MIG/MPS
switching logic in M13) should test/use MPS BEFORE toggling MIG mode on a
given boot, or reset between tests.

## What this unblocks in M13

- The per-card `SharingMode` seam issue can use the table above directly —
  no further hardware research needed, no "unconfirmed" caveat for g7.
- The MIG fixed-layout provisioning issue has real, driver-confirmed
  profile sets to pick from for both g7 and g7e (not just g7e as
  originally scoped once g7 was thought unconfirmed).
- The MPS trusted-tenant issue is now confirmed viable on all four
  families, not just "should work per NVIDIA's docs, no per-GPU
  restriction stated" — actually observed starting cleanly on every card.

## Verification cost

4 spot instances (g6.2xlarge, g6e.2xlarge, g7.2xlarge, g7e.2xlarge),
each held for the duration of one SSM diagnostic script (a few minutes),
terminated immediately after. No instances leaked (confirmed via a
post-hoc `describe-instances` sweep across the regions used). Tool used:
`cmd/gpuprobe` (calque#104), a one-off diagnostic — not part of calque's
product CLI surface.
