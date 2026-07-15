#!/usr/bin/env python3
"""In-worker GPU occupancy sampler — the tach hook (spec §8).

There is no `tach` library in the spore.host org (confirmed: repo doesn't exist),
so per §8 a crude in-worker sampler is the accepted path for the spike. This polls
GPU utilization across a run and reports mean occupancy P% — one of the three
inputs K stands on (rate, occupancy, buy-down).

Prefers DCGM (dcgmi) if available; falls back to `nvidia-smi` polling. Emits one
JSON summary to stdout on stop, and (optionally) a JSONL sample stream so a
skeptic can see the raw series, not just the mean.

Runs as a sidecar next to warmd/runner.py on the acquired instance. warmd starts
it at @enter and signals stop when the drain completes; the summary is written to
S3 alongside results so `measure` can fold it into the cost model.

Usage:
  occupancy.py sample --interval 1.0 --out /tmp/occ.jsonl   # runs until SIGTERM
  # on stop, prints: {"mean_occupancy":0.83,"samples":420,"source":"nvidia-smi",...}
"""

from __future__ import annotations

import argparse
import json
import shutil
import signal
import subprocess
import sys
import time


class Sampler:
    def __init__(self, interval: float, out_path: str | None) -> None:
        self.interval = interval
        self.out_path = out_path
        self.samples: list[float] = []
        self.source = "none"
        self._stop = False

    def _read_nvidia_smi(self) -> list[float]:
        # utilization.gpu is the % of time one or more kernels was executing.
        try:
            out = subprocess.check_output(
                ["nvidia-smi", "--query-gpu=utilization.gpu", "--format=csv,noheader,nounits"],
                text=True, timeout=5,
            )
        except (subprocess.SubprocessError, OSError):
            return []
        vals = []
        for line in out.strip().splitlines():
            line = line.strip()
            if line:
                try:
                    vals.append(float(line) / 100.0)
                except ValueError:
                    pass
        return vals

    def _read_dcgm(self) -> list[float]:
        # dcgmi dmon field 203 = GPU utilization. Best-effort; many hosts lack it.
        try:
            out = subprocess.check_output(
                ["dcgmi", "dmon", "-e", "203", "-c", "1"], text=True, timeout=5
            )
        except (subprocess.SubprocessError, OSError):
            return []
        vals = []
        for line in out.strip().splitlines():
            parts = line.split()
            if parts and parts[0].isdigit():
                try:
                    vals.append(float(parts[-1]) / 100.0)
                except ValueError:
                    pass
        return vals

    def _detect(self) -> None:
        if shutil.which("dcgmi") and self._read_dcgm():
            self.source = "dcgm"
        elif shutil.which("nvidia-smi"):
            self.source = "nvidia-smi"
        else:
            # No GPU tooling (e.g. a local CPU dry-run). We still run so the
            # pipeline works end-to-end; occupancy is reported as unavailable and
            # `measure` flags K's occupancy input as unmeasured.
            self.source = "none"

    def _sample_once(self) -> None:
        if self.source == "dcgm":
            vals = self._read_dcgm()
        elif self.source == "nvidia-smi":
            vals = self._read_nvidia_smi()
        else:
            vals = []
        if vals:
            # Mean across GPUs on the box (single-node, usually 1 GPU for the spike).
            self.samples.append(sum(vals) / len(vals))

    def run(self) -> dict:
        self._detect()
        out_f = open(self.out_path, "w", encoding="utf-8") if self.out_path else None
        # perf_counter can't be pinned in tests, but this process is on the
        # instance during a real run; wall-clock here is the intent.
        while not self._stop:
            self._sample_once()
            if out_f and self.samples:
                out_f.write(json.dumps({"occ": self.samples[-1]}) + "\n")
                out_f.flush()
            time.sleep(self.interval)
        if out_f:
            out_f.close()
        return self.summary()

    def summary(self) -> dict:
        mean = sum(self.samples) / len(self.samples) if self.samples else None
        return {
            "mean_occupancy": mean,          # fraction [0,1], or None if unmeasured
            "samples": len(self.samples),
            "source": self.source,           # dcgm | nvidia-smi | none
            "interval_s": self.interval,
            "measured": mean is not None,    # feeds K's measured|proxy flag
        }

    def stop(self, *_):
        self._stop = True


def main(argv: list[str] | None = None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("sample")
    s.add_argument("--interval", type=float, default=1.0)
    s.add_argument("--out", default=None)
    args = ap.parse_args(argv)

    if args.cmd == "sample":
        sampler = Sampler(args.interval, args.out)
        signal.signal(signal.SIGTERM, sampler.stop)
        signal.signal(signal.SIGINT, sampler.stop)
        summary = sampler.run()
        json.dump(summary, sys.stdout)
        sys.stdout.write("\n")
        return 0
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
