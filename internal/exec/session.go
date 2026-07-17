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
	LogKey    string // one-time prep log
}

// PrepCommand is the JobArrayCommand for the held instance: sync warmd+scripts,
// pull the vLLM image, and exit 0. spored keeps the box alive after (bounded by
// TTL + idle timeout). Runs under the login user; docker needs sudo (DL AMI).
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
		lines = append(lines, fmt.Sprintf("trap 'aws s3 cp /tmp/calque-prep.log s3://%s/%s || true' EXIT", p.Bucket, p.LogKey))
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
	return strings.Join(lines, "\n")
}

// TestRunCommand is the per-test command driven over SSM on the HELD instance: run
// warmd-in-docker against a specific manifest. The instance is already prepped
// (image pulled), so this is just the container run. Output goes to a per-run log
// uploaded to S3. warmd writes results + summary to S3 as before.
func TestRunCommand(baseImage, workerDir, region, bucket, manifestKey, modelEnv, logKey, occKey string) string {
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
			"exit $RC",
		)
	} else {
		lines = append(lines, "set -e", strings.Join(docker, " "))
	}
	return strings.Join(lines, "\n")
}
