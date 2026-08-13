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
	LogKey         string // s3 key to upload the bootstrap log to on exit (observability)
	ModelEnv       string // HF repo id passed to the container as CALQUE_MODEL (docker mode)
	// HostMode runs warmd directly on the instance host (no docker) — used by the
	// acquire-only smoke test to isolate acquisition + instance-role S3 + collect +
	// terminate from the docker/GPU/model layer. Real inference uses docker mode.
	HostMode bool
	// PipPackages are third-party Python packages the picked unit's REAL body
	// needs (calque#148 follow-up) — host-mode previously had NO dependency-
	// install step at all, just a bare `command -v python3` check + a leak
	// saying "dependencies must already be on the AMI." When non-empty,
	// installed via `uv` (curl-installed fresh every boot, not a package-
	// manager assumption — works identically on any distro, unlike the
	// apt-get-only fallback this replaces for host mode's python3 check)
	// into a fresh venv, so a real script's actual pip_install(...) list
	// (or a caller-supplied override when the script's image chain wasn't
	// statically resolvable, e.g. built via a factory function) can
	// actually run instead of NameError-ing on a missing import. Also pins
	// a specific Python version via `uv python install` rather than
	// depending on whatever python3 the AMI happens to ship.
	PipPackages []string
	// PythonVersion pins the interpreter uv installs (e.g. "3.11") to match
	// what the script's own image declared (Modal's debian_slim(python_version=)),
	// instead of depending on the AMI's system Python. Empty lets uv pick its
	// own default (currently the latest stable CPython).
	PythonVersion string
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
		"#!/bin/bash",
		// Observability: capture EVERYTHING to a log and upload it to S3 on exit —
		// success OR failure — so we can post-mortem even after the instance is
		// terminated (the bootstrap output otherwise dies with the instance).
		"exec > /tmp/calque-bootstrap.log 2>&1",
	}
	if b.LogKey != "" {
		lines = append(lines,
			fmt.Sprintf("trap 'aws s3 cp /tmp/calque-bootstrap.log s3://%s/%s || true' EXIT", b.Bucket, b.LogKey),
		)
	}
	lines = append(lines,
		"set -euxo pipefail",
		// Host prep: ensure aws cli is present (DL AMIs have it).
		"command -v aws >/dev/null || (sudo apt-get update && sudo apt-get install -y awscli)",
		fmt.Sprintf("mkdir -p %s", wd),
		// Pull tiny worker artifacts from S3 (warmd binary + python scripts).
		fmt.Sprintf("aws s3 cp --recursive %s/ %s/", art, wd),
		fmt.Sprintf("chmod +x %s/warmd", wd),
	)

	if b.HostMode {
		// Smoke test / real-AWS host-mode: run warmd directly on the host —
		// no docker, no GPU-container layer. Isolates acquisition +
		// instance-role S3 + collect + terminate from the docker/GPU/model
		// layer for the smoke test; for a real --script run, this is the
		// path that drives a picked unit's OWN parsed body (calque#79).
		if len(b.PipPackages) > 0 || b.PythonVersion != "" {
			// calque#148 follow-up: install uv fresh every boot (its own
			// installer script, NOT a distro package-manager assumption —
			// works identically on AL2023/Ubuntu/Debian, unlike the
			// apt-get-only python3 fallback this replaces) and use it to
			// pin a Python version + install the picked unit's REAL
			// third-party deps into a venv, instead of leaking "must
			// already be on the AMI" and NameError-ing on execution.
			pyVer := b.PythonVersion
			if pyVer == "" {
				pyVer = "3.12"
			}
			lines = append(lines,
				"command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh",
				`export PATH="$HOME/.local/bin:$PATH"`,
				fmt.Sprintf("uv python install %s", pyVer),
				fmt.Sprintf("uv venv --python %s %s/.venv", pyVer, wd),
			)
			if len(b.PipPackages) > 0 {
				// `uv venv` creates a plain venv (python3/pip only) — it
				// does NOT copy the uv binary itself in. Installing into
				// that venv means invoking the TOP-LEVEL uv with
				// --python pointed at the venv's interpreter, not
				// expecting a uv binary inside .venv/bin.
				quoted := make([]string, len(b.PipPackages))
				for i, p := range b.PipPackages {
					quoted[i] = fmt.Sprintf("%q", p)
				}
				lines = append(lines,
					fmt.Sprintf("uv pip install --python %s/.venv/bin/python3 %s", wd, strings.Join(quoted, " ")),
				)
			}
			// warmd itself reads the interpreter path from the MANIFEST's
			// own PythonBin field (cmd/warmd/main.go's pyOr), not an env
			// var — the caller building this ManifestBody MUST set
			// PythonBin to this exact "<wd>/.venv/bin/python3" path
			// (internal/exec.ManifestBody.PythonBin) or warmd falls back
			// to plain "python3", which was never pip-installed into.
			lines = append(lines,
				fmt.Sprintf("AWS_REGION=%s %s/warmd run --manifest %s", b.Region, wd, manifest),
			)
			return strings.Join(lines, "\n")
		}
		// No real deps needed (or none supplied) — unchanged pre-#148 path:
		// runner.py needs only python3, which DL AMIs (and most Ubuntu
		// AMIs) have.
		lines = append(lines,
			"command -v python3 >/dev/null || (sudo apt-get update && sudo apt-get install -y python3 || sudo dnf install -y python3)",
			fmt.Sprintf("AWS_REGION=%s %s/warmd run --manifest %s", b.Region, wd, manifest),
		)
		return strings.Join(lines, "\n")
	}

	// docker needs root on the DL AMI (the login user isn't in the docker group —
	// "permission denied ... docker.sock" otherwise). Run docker under sudo.
	dockerRun := []string{
		"sudo docker run --rm --gpus all",
		fmt.Sprintf("-e AWS_REGION=%s", b.Region),
		// HF cache on the host (mounted) so a re-run doesn't re-download weights.
		"-e HF_HOME=/root/.cache/huggingface -v /root/.cache/huggingface:/root/.cache/huggingface",
		fmt.Sprintf("-v %s:%s", wd, wd),
	}
	if b.ModelEnv != "" {
		// @enter reads CALQUE_MODEL to pick which HF model vLLM loads.
		dockerRun = append(dockerRun, fmt.Sprintf("-e CALQUE_MODEL=%s", b.ModelEnv))
	}
	dockerRun = append(dockerRun,
		"--entrypoint "+wd+"/warmd",
		b.BaseImage,
		"run --manifest "+manifest,
	)
	lines = append(lines,
		// Pull the base inference image (fast from within AWS). sudo: see above.
		fmt.Sprintf("sudo docker pull %s", b.BaseImage),
		// Run the worker: GPU on, artifacts mounted, AWS creds via instance role
		// (passed through by the metadata service — no keys on the command line).
		strings.Join(dockerRun, " "),
	)
	return strings.Join(lines, "\n")
}
