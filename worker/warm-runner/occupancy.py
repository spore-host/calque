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
    """Samples GPU occupancy from MULTIPLE sources each tick so we can compare and
    pick the most accurate (spec §8). nvidia-smi's utilization.gpu is coarse — it's
    "% of time >=1 kernel ran" over the poll window, not real SM/compute occupancy,
    and it understates a busy GPU (observed: dmon sm=100% while utilization.gpu read
    low). DCGM's SM-activity (DCP field 1002) is the truer measure. We collect BOTH
    (plus nvidia-smi dmon sm%) and report each series; `primary` prefers DCGM.
    """

    # metric key -> collector; each returns a per-tick fraction [0,1] or None.
    def __init__(self, interval: float, out_path: str | None) -> None:
        self.interval = interval
        self.out_path = out_path
        # one list of per-tick fractions per metric
        self.series: dict[str, list[float]] = {
            "nvsmi_util": [],   # nvidia-smi utilization.gpu (coarse; the old default)
            "nvsmi_sm": [],     # nvidia-smi dmon sm% (better: SM activity)
            "dcgm_sm": [],      # dcgmi dmon SM active (DCP 1002; truest when present)
        }
        self.have = {"nvsmi": bool(shutil.which("nvidia-smi")), "dcgmi": bool(shutil.which("dcgmi"))}
        self._stop = False

    @staticmethod
    def _mean_of_csv_percents(out: str) -> float | None:
        vals = []
        for line in out.strip().splitlines():
            line = line.strip()
            if line:
                try:
                    vals.append(float(line) / 100.0)
                except ValueError:
                    pass
        return sum(vals) / len(vals) if vals else None

    def _nvsmi_util(self) -> float | None:
        try:
            out = subprocess.check_output(
                ["nvidia-smi", "--query-gpu=utilization.gpu", "--format=csv,noheader,nounits"],
                text=True, timeout=5)
        except (subprocess.SubprocessError, OSError):
            return None
        return self._mean_of_csv_percents(out)

    def _dmon_sm(self, tool: str) -> float | None:
        # `<tool> dmon -c 1` prints a header (2 lines starting with '#') then one
        # row per GPU. For nvidia-smi the 2nd column is sm%. Parse numeric rows and
        # average the sm column across GPUs.
        try:
            args = [tool, "dmon", "-c", "1"] + (["-s", "u"] if tool == "nvidia-smi" else ["-e", "1002"])
            out = subprocess.check_output(args, text=True, timeout=6)
        except (subprocess.SubprocessError, OSError):
            return None
        vals = []
        for line in out.strip().splitlines():
            parts = line.split()
            if not parts or not parts[0].lstrip("-").isdigit():
                continue  # skip '#' headers
            # nvidia-smi dmon -s u: cols = gpu sm mem enc dec ... -> sm is index 1
            # dcgmi dmon -e 1002:   cols = GPU <value>            -> value is last
            try:
                v = float(parts[1]) if tool == "nvidia-smi" else float(parts[-1])
                vals.append(v / 100.0)
            except (ValueError, IndexError):
                pass
        return sum(vals) / len(vals) if vals else None

    def _sample_once(self) -> None:
        if self.have["nvsmi"]:
            u = self._nvsmi_util()
            if u is not None:
                self.series["nvsmi_util"].append(u)
            sm = self._dmon_sm("nvidia-smi")
            if sm is not None:
                self.series["nvsmi_sm"].append(sm)
        if self.have["dcgmi"]:
            d = self._dmon_sm("dcgmi")
            if d is not None:
                self.series["dcgm_sm"].append(d)

    def run(self) -> dict:
        out_f = open(self.out_path, "w", encoding="utf-8") if self.out_path else None
        while not self._stop:
            self._sample_once()
            if out_f:
                last = {k: (v[-1] if v else None) for k, v in self.series.items()}
                out_f.write(json.dumps(last) + "\n")
                out_f.flush()
            time.sleep(self.interval)
        if out_f:
            out_f.close()
        return self.summary()

    def summary(self) -> dict:
        def mean(xs: list[float]) -> float | None:
            return sum(xs) / len(xs) if xs else None

        means = {k: mean(v) for k, v in self.series.items()}
        counts = {k: len(v) for k, v in self.series.items()}
        # Primary occupancy: prefer DCGM SM-activity, then nvidia-smi dmon sm%, then
        # the coarse utilization.gpu — most-accurate-available. This is what K uses.
        primary_key = None
        for k in ("dcgm_sm", "nvsmi_sm", "nvsmi_util"):
            if means[k] is not None:
                primary_key = k
                break
        primary = means[primary_key] if primary_key else None
        return {
            "mean_occupancy": primary,        # fraction [0,1] — K uses this (best available)
            "occupancy_source": primary_key or "none",  # which metric fed mean_occupancy
            "metrics": means,                 # ALL metrics, for comparison/audit
            "metric_samples": counts,
            "source": primary_key or "none",  # back-compat with the Go OccupancyRaw
            "samples": counts.get(primary_key, 0) if primary_key else 0,
            "interval_s": self.interval,
            "measured": primary is not None,  # feeds K's measured|proxy flag
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
