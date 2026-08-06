package exec

import (
	"fmt"
	"strings"
)

// SessionPrep is the ONE-TIME bootstrap for a held instance: it prepares docker
// and pulls the vLLM image, but runs NO workload — the instance then idles,
// alive, while we drive each test onto it over SSM. This is the "acquire once,
// hold, run many" pattern: the expensive part (acquisition + image pull) is paid
// once and amortized across the whole test ramp, instead of re-acquiring per test.
type SessionPrep struct {
	BaseImage string
	Bucket    string
	WorkerDir string
	Region    string
	LogKey    string // one-time prep log (streamed periodically + on exit)
	DoneKey   string // success marker, written ONLY after the pull completes (see below)
}

// PrepCommand is the JobArrayCommand for the held instance: sync warmd+scripts,
// pull the vLLM image, and exit 0. spored keeps the box alive after (bounded by
// TTL + idle timeout). Runs under the login user; docker needs sudo (DL AMI).
//
// Completion signalling (fixes the prep-visibility bug, #18): the OLD design
// uploaded the log via `trap ... EXIT` and the waiter polled for that log — but the
// script only exits when `docker pull` returns, so a slow pull meant the log never
// appeared before the waiter's timeout, yielding a bare "prep did not complete"
// with ZERO diagnostics. Now:
//   - a background HEARTBEAT streams the growing log to S3 every 20s, so a slow or
//     hung pull is observable in near-real-time (not only on exit);
//   - a distinct DoneKey marker is written ONLY after the pull succeeds — the
//     waiter keys success off that marker, decoupling "done" from "script exited".
//
// The EXIT trap still flushes the final log (success or failure) for the tail.
func (p SessionPrep) PrepCommand(artifactPrefix string) string {
	wd := p.WorkerDir
	if wd == "" {
		wd = "/tmp/calque"
	}
	art := fmt.Sprintf("s3://%s/%s", p.Bucket, strings.TrimSuffix(artifactPrefix, "/"))
	lines := []string{
		"#!/bin/bash",
		"exec > /tmp/calque-prep.log 2>&1",
	}
	if p.LogKey != "" {
		up := fmt.Sprintf("aws s3 cp /tmp/calque-prep.log s3://%s/%s || true", p.Bucket, p.LogKey)
		// Heartbeat: stream the growing log to S3 every 20s so a slow pull is visible
		// while it's still running — not only on exit. Best-effort.
		lines = append(lines, fmt.Sprintf("( while true; do sleep 20; %s; done ) & HEARTBEAT=$!", up))
		// One EXIT trap: stop the heartbeat and flush the final log (captures the
		// failure tail too). A single trap per signal — bash keeps only the last.
		lines = append(lines, fmt.Sprintf("trap 'kill $HEARTBEAT 2>/dev/null || true; %s' EXIT", up))
	}
	lines = append(lines,
		"set -euxo pipefail",
		"command -v aws >/dev/null || (sudo apt-get update && sudo apt-get install -y awscli)",
		fmt.Sprintf("mkdir -p %s", wd),
		fmt.Sprintf("aws s3 cp --recursive %s/ %s/", art, wd),
		fmt.Sprintf("chmod +x %s/warmd", wd),
		// Pull the image ONCE now, so every subsequent SSM-driven test starts fast.
		fmt.Sprintf("sudo docker pull %s", p.BaseImage),
		"echo CALQUE_PREP_DONE",
	)
	if p.DoneKey != "" {
		// Success marker: written only if we got here (pull succeeded). Under
		// `set -e`, any earlier failure exits before this line, so the marker's
		// mere existence is a reliable success signal — independent of when/whether
		// the log upload lands.
		lines = append(lines, fmt.Sprintf("echo CALQUE_PREP_DONE | aws s3 cp - s3://%s/%s", p.Bucket, p.DoneKey))
	}
	return strings.Join(lines, "\n")
}

// TestRunCommand is the per-test command driven over SSM on the HELD instance: run
// warmd-in-docker against a specific manifest. The instance is already prepped
// (image pulled), so this is just the container run. Output goes to a per-run log
// uploaded to S3. warmd writes results + summary to S3 as before.
//
// occSamplesKey (#71) uploads the sampler's RAW timestamped JSONL, not just its
// summary. It's needed because in this path the sampler runs on the HOST (for dcgmi)
// while warmd runs in the container: warmd therefore cannot see these samples and
// cannot do its own inference-window re-average. The control plane does it instead,
// pairing this stream with the inference spans warmd reports in its summary. Empty
// key => no upload (the whole-run mean stays the only number, labeled as such).
func TestRunCommand(baseImage, workerDir, region, bucket, manifestKey, modelEnv, logKey, occKey, occSamplesKey string) string {
	wd := workerDir
	if wd == "" {
		wd = "/tmp/calque"
	}
	manifest := fmt.Sprintf("s3://%s/%s", bucket, manifestKey)
	docker := []string{
		"sudo docker run --rm --gpus all",
		fmt.Sprintf("-e AWS_REGION=%s", region),
		"-e HF_HOME=/root/.cache/huggingface -v /root/.cache/huggingface:/root/.cache/huggingface",
		fmt.Sprintf("-v %s:%s", wd, wd),
	}
	if modelEnv != "" {
		docker = append(docker, fmt.Sprintf("-e CALQUE_MODEL=%s", modelEnv))
	}
	docker = append(docker, "--entrypoint "+wd+"/warmd", baseImage, "run --manifest "+manifest)

	lines := []string{
		"#!/bin/bash",
		"exec > /tmp/calque-test.log 2>&1",
		fmt.Sprintf("trap 'aws s3 cp /tmp/calque-test.log s3://%s/%s || true' EXIT", bucket, logKey),
		"set -uxo pipefail", // not -e: we manage the sampler lifecycle around the run
	}
	if occKey != "" {
		// Run the occupancy sampler ON THE HOST (not in the container): dcgmi lives
		// on the host (the vLLM image lacks it), so host placement is what unlocks
		// true DCGM SM-activity. Start it in the background, run the container, then
		// SIGTERM the sampler and upload its JSON summary to S3.
		lines = append(lines,
			fmt.Sprintf("python3 %s/occupancy.py sample --interval 1.0 --out /tmp/calque-occ.jsonl > /tmp/calque-occ.json 2>/dev/null & OCC=$!", wd),
		)
		lines = append(lines, "set +e", strings.Join(docker, " "), "RC=$?", "set -e")
		lines = append(lines,
			"kill -TERM $OCC 2>/dev/null || true",
			"wait $OCC 2>/dev/null || true",
			fmt.Sprintf("aws s3 cp /tmp/calque-occ.json s3://%s/%s || true", bucket, occKey),
		)
		if occSamplesKey != "" {
			// Raw per-tick samples: the control plane re-averages these over warmd's
			// inference spans to get load-free occupancy (#71).
			lines = append(lines,
				fmt.Sprintf("aws s3 cp /tmp/calque-occ.jsonl s3://%s/%s || true", bucket, occSamplesKey))
		}
		lines = append(lines, "exit $RC")
	} else {
		lines = append(lines, "set -e", strings.Join(docker, " "))
	}
	return strings.Join(lines, "\n")
}
