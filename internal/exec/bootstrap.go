package exec

import (
	"fmt"
	"strings"
)

// BootstrapConfig parameterizes the on-instance launch command. For the spike we
// AVOID pre-baking a multi-GB vLLM image (local Docker + arm64->amd64 cross-build
// is slow/impractical). Instead the acquired instance assembles the worker from a
// public base image + tiny artifacts pulled from S3 — the g7e pulls both fast from
// inside AWS. (This is a spike shortcut; the .image DSL -> ECR path (§13) is built
// and digest-tested in internal/image, and becomes the path once spawn#353 lands a
// headless container primitive.)
type BootstrapConfig struct {
	BaseImage      string // e.g. "vllm/vllm-openai:latest"
	Bucket         string
	ArtifactPrefix string // s3 prefix holding warmd, runner.py, occupancy.py
	ManifestKey    string // s3 key of the work manifest
	WorkerDir      string // in-container dir for artifacts, e.g. "/opt/calque"
	Region         string
}

// Command builds the shell command the instance runs (via spawn JobArrayCommand /
// cloud-init). It: installs awscli if missing, syncs artifacts from S3, pulls the
// base image, and runs the container with the GPU, invoking warmd against the
// manifest. Idempotent-ish and logs to stdout so spored/CloudWatch capture it.
//
// NOTE: we build this string ourselves because spawn exposes no headless
// container/ECR primitive yet (spawn#351/#353). Flagged as an integration leak.
func (b BootstrapConfig) Command() string {
	wd := b.WorkerDir
	if wd == "" {
		wd = "/opt/calque"
	}
	art := fmt.Sprintf("s3://%s/%s", b.Bucket, strings.TrimSuffix(b.ArtifactPrefix, "/"))
	manifest := fmt.Sprintf("s3://%s/%s", b.Bucket, b.ManifestKey)

	lines := []string{
		"set -euxo pipefail",
		// Host prep: ensure aws + docker + nvidia runtime are present (DL AMIs have them).
		"command -v aws >/dev/null || (apt-get update && apt-get install -y awscli)",
		fmt.Sprintf("mkdir -p %s", wd),
		// Pull tiny worker artifacts from S3 (warmd binary + python scripts).
		fmt.Sprintf("aws s3 cp --recursive %s/ %s/", art, wd),
		fmt.Sprintf("chmod +x %s/warmd", wd),
		// Pull the base inference image (fast from within AWS).
		fmt.Sprintf("docker pull %s", b.BaseImage),
		// Run the worker: GPU on, artifacts mounted, AWS creds via instance role
		// (passed through by the metadata service — no keys on the command line).
		strings.Join([]string{
			"docker run --rm --gpus all",
			fmt.Sprintf("-e AWS_REGION=%s", b.Region),
			fmt.Sprintf("-v %s:%s", wd, wd),
			"--entrypoint " + wd + "/warmd",
			b.BaseImage,
			"run --manifest " + manifest,
		}, " "),
	}
	return strings.Join(lines, "\n")
}
