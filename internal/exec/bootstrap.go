package exec

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
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
	// StageFiles downloads each key (a URL curl can fetch — http(s):// or a
	// raw.githubusercontent.com link) to its value (an ABSOLUTE destination
	// path on the instance), before warmd runs. For a real script whose body
	// shells out to a hardcoded absolute path it assumes its ORIGINAL Docker
	// image would have placed there (e.g. AI-Almanac's app.py hardcodes
	// "/app/scripts/generate_config.py" — that's the upstream ROMP image's
	// own Dockerfile convention, `WORKDIR /app` + `COPY scripts/`, not
	// anything Modal itself defines) — this stages the SAME file at the
	// SAME path without needing to build/pull that whole image. Directories
	// are created as needed; nil/empty is a no-op.
	StageFiles map[string]string
	// RegistryRef overrides BaseImage's pull target with a --script real
	// run's OWN resolved image (calque#176) — set only when the picked
	// unit's ir.Image resolved to a real "from_registry"/"from_aws_ecr" ref
	// (internal/parse.resolveCallableImage), instead of always pulling the
	// hardcoded vLLM reference image. Empty (the default) reproduces prior
	// behavior exactly: BaseImage stays what it was. When RegistryRef looks
	// like an ECR hostname (isECRHostname), Command() authenticates via
	// `aws ecr get-login-password` before the pull; a public registry (or
	// any other private registry calque has no credential-sourcing
	// mechanism for, e.g. GCP Artifact Registry) is pulled anonymously,
	// matching today's behavior for BaseImage. When BuildDockerfile is also
	// set, RegistryRef instead names the Dockerfile's OWN FROM base (if
	// any) purely for this same auth decision — docker build needs to pull
	// that base itself, same as a plain docker pull would.
	RegistryRef string
	// BuildDockerfile builds a Dockerfile (rendered by internal/image.Render,
	// uploaded to the artifact prefix by the caller via
	// internal/exec.UploadDockerfile) on the instance itself, instead of
	// pulling an already-published image (calque#177) — for a --script real
	// run whose resolved .image chain has steps beyond a bare pullable
	// from_registry/from_aws_ecr ref (e.g. blending_app.py's
	// debian_slim()...run_commands(...) chain, or a from_registry base with
	// .pip_install(...)/.add_local_file(...) layered on top). The
	// Dockerfile is already present at "<WorkerDir>/Dockerfile" by the time
	// Command() runs it — the existing `aws s3 cp --recursive` artifact
	// sync (which every docker-mode run already does) downloads it for
	// free, no separate fetch step needed. False (the default) reproduces
	// prior behavior exactly: RegistryRef/BaseImage is pulled, never built.
	BuildDockerfile bool
	// BuildTag is the tag `docker build` produces and `docker run` then
	// uses, when BuildDockerfile is set — callers should pass
	// internal/image.Digest(dockerfile) so an identical resolved chain is a
	// local `docker build` cache hit on a re-run (mirroring the same
	// content-addressing property internal/image already documents for a
	// future ECR push path). Ignored when BuildDockerfile is false.
	BuildTag string
	// CloudBucketMountLines are already-rendered shell lines (calque#91
	// Workstream A) that mount every resolved modal.CloudBucketMount(...)
	// via mountpoint-s3 — the caller (cmd/calque/realrun.go) builds these via
	// plan.MountCommands(plan.ResolveCloudBucketMounts(app, rep)), so this
	// package never needs to import internal/plan (no import-cycle risk).
	// Spliced in right after the artifact sync, before either the HostMode
	// or docker-mode run invocation — the mount must be live before @enter
	// runs, in either mode. nil/empty (the default) is a no-op, reproducing
	// prior behavior byte-for-byte for every script with no CloudBucketMount.
	CloudBucketMountLines []string
	// NFSMountLines are already-rendered shell lines (calque#91 Workstream B)
	// that mount every resolved modal.NetworkFileSystem.from_name(...) via
	// NFS against a pre-provisioned (bring-your-own) EFS filesystem — the
	// caller (cmd/calque/realrun.go) builds these after resolving the real
	// EFS filesystem ID/DNS name via internal/plan/efs.go's
	// DiscoverEFSFilesystem/ResolveMountTargetsForAZs, so this package never
	// needs to import internal/plan (no import-cycle risk) or make any AWS
	// call itself — same posture as CloudBucketMountLines above. Spliced in
	// at the SAME point as CloudBucketMountLines (order between the two
	// doesn't matter; both must land after artifact sync, before either the
	// HostMode or docker-mode run invocation — the mount must be live before
	// @enter runs, in either mode). nil/empty (the default) is a no-op,
	// reproducing prior behavior byte-for-byte for every script with no
	// NetworkFileSystem.
	NFSMountLines []string
}

// ecrHostname matches an ECR registry hostname, e.g.
// "123456789012.dkr.ecr.us-east-1.amazonaws.com" — the account id + region
// are embedded in the hostname itself, which is what `aws ecr
// get-login-password --region <region>` needs to know to fetch a token
// scoped to the right account/region.
var ecrHostname = regexp.MustCompile(`^(\d{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com`)

// isECRHostname reports whether ref's registry host is an ECR endpoint, and
// if so, the region embedded in that hostname (calque#176). A ref with no
// registry host at all (e.g. "vllm/vllm-openai:latest", Docker Hub's
// implicit default) or a non-ECR host (Docker Hub, GCP Artifact Registry,
// etc.) is never treated as ECR — those either need no auth (public) or an
// auth mechanism calque doesn't have (leaked separately by the caller).
func isECRHostname(ref string) (region string, ok bool) {
	host := ref
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	m := ecrHostname.FindStringSubmatch(host)
	if m == nil {
		return "", false
	}
	return m[2], true
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
	if len(b.StageFiles) > 0 {
		urls := make([]string, 0, len(b.StageFiles))
		for u := range b.StageFiles {
			urls = append(urls, u)
		}
		sort.Strings(urls) // deterministic script output regardless of map iteration order
		for _, u := range urls {
			dest := b.StageFiles[u]
			lines = append(lines,
				fmt.Sprintf("sudo mkdir -p %q", filepath.Dir(dest)),
				fmt.Sprintf("sudo curl -LsSf %q -o %q", u, dest),
			)
		}
	}

	// calque#91 Workstream A: mount every resolved modal.CloudBucketMount(...)
	// via mountpoint-s3 BEFORE either the HostMode or docker-mode run
	// invocation below — the mount must be live before @enter runs, in
	// either mode. Empty (the default) is a no-op.
	if len(b.CloudBucketMountLines) > 0 {
		lines = append(lines, b.CloudBucketMountLines...)
	}
	// calque#91 Workstream B: mount every resolved
	// modal.NetworkFileSystem.from_name(...) via NFS against a
	// pre-provisioned EFS filesystem, same splice point/rationale as
	// CloudBucketMountLines above. Empty (the default) is a no-op.
	if len(b.NFSMountLines) > 0 {
		lines = append(lines, b.NFSMountLines...)
	}

	if b.HostMode {
		// Smoke test / real-AWS host-mode: run warmd directly on the host —
		// no docker, no GPU-container layer. Isolates acquisition +
		// instance-role S3 + collect + terminate from the docker/GPU/model
		// layer for the smoke test; for a real --script run, this is the
		// path that drives a picked unit's OWN parsed body (calque#79).
		//
		// ALWAYS provisions a uv-managed venv (calque#148, widened): never
		// depends on the AMI's own distro package manager for Python at
		// all — an apt-get/dnf `python3` install was the pre-existing
		// fallback for the no-deps case, replaced entirely so every
		// host-mode run's interpreter is uv-managed regardless of whether
		// --pip/--python-version were supplied.
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
			// A --pip package spec can be a git URL (e.g. "momp @
			// git+https://github.com/hholb/ROMP.git@main", for a package
			// with no PyPI release) — `uv pip install` shells out to a
			// real `git` binary for that, which AL2023 (the AMI spawn
			// auto-selects for non-GPU instance types like m6i.large)
			// does NOT ship by default, unlike aws/dnf. Found live: a
			// git-URL --pip spec failed fast with "Git executable not
			// found" on an otherwise-correct uv/venv/pip-install
			// sequence.
			lines = append(lines,
				"command -v git >/dev/null || (sudo apt-get update && sudo apt-get install -y git || sudo dnf install -y git)",
			)
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
		// to plain "python3", which was never provisioned by this venv.
		// This is now UNCONDITIONAL (calque#148, widened) — the venv
		// always exists, even with zero --pip packages.
		//
		// The venv's bin/ is ALSO prepended to PATH before launching
		// warmd — not for warmd itself, but for the picked unit's OWN
		// body if it shells out to a BARE command name (e.g.
		// subprocess.run(["momp-run", ...]), AI-Almanac's app.py) —
		// warmd -> runner.py -> that subprocess call all inherit this
		// same process environment/PATH, so a console-script entry
		// point installed into the venv (via a package like `momp`,
		// not just a plain pip package name) resolves correctly
		// without the body needing to know the venv's absolute path.
		lines = append(lines,
			fmt.Sprintf(`export PATH="%s/.venv/bin:$PATH"`, wd),
			fmt.Sprintf("AWS_REGION=%s %s/warmd run --manifest %s", b.Region, wd, manifest),
		)
		return strings.Join(lines, "\n")
	}

	// calque#176: a --script real run whose picked unit resolved to a real
	// from_registry/from_aws_ecr image pulls THAT image instead of the
	// hardcoded vLLM reference (b.BaseImage stays the vLLM default when
	// RegistryRef is unset — every existing corpus run's docker-mode
	// behavior is unchanged).
	pullImage := b.BaseImage
	if b.RegistryRef != "" {
		pullImage = b.RegistryRef
	}
	if region, ok := isECRHostname(pullImage); ok {
		// Authenticate before the pull/build — docker itself has no notion
		// of IAM instance-role credentials; `aws ecr get-login-password`
		// exchanges them for a short-lived ECR token via
		// ecr:GetAuthorizationToken (internal/plan.RealRunPolicy grants
		// this), piped straight into `docker login` without ever touching
		// disk. Needed for BuildDockerfile too when the Dockerfile's own
		// FROM line is a private ECR ref (RegistryRef names that base in
		// that case — see BuildDockerfile's doc).
		lines = append(lines,
			fmt.Sprintf("aws ecr get-login-password --region %s | sudo docker login --username AWS --password-stdin %s",
				region, strings.SplitN(pullImage, "/", 2)[0]),
		)
	}

	// runImage is what `docker run` below actually invokes — either the
	// pulled reference (today's #176 behavior) or the tag `docker build`
	// just produced (calque#177).
	runImage := pullImage
	if b.BuildDockerfile {
		// calque#177: the picked unit's resolved .image chain has steps
		// beyond a bare pullable ref (e.g. .pip_install/.run_commands
		// layered on a from_registry base, or a from-scratch
		// debian_slim()... chain with no publishable image at all) — build
		// it ON THIS INSTANCE from the Dockerfile the caller already
		// uploaded to the artifact prefix (downloaded for free by the `aws
		// s3 cp --recursive` line above, alongside warmd/runner.py). No
		// ECR round-trip, no ambient Docker requirement on the CALLER's
		// machine, no second/throwaway instance.
		runImage = b.BuildTag
		lines = append(lines,
			fmt.Sprintf("sudo docker build -t %s -f %s/Dockerfile %s", b.BuildTag, wd, wd),
		)
	} else {
		lines = append(lines,
			// Pull the base inference image (fast from within AWS). sudo: see above.
			fmt.Sprintf("sudo docker pull %s", pullImage),
		)
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
		runImage,
		"run --manifest "+manifest,
	)
	lines = append(lines,
		// Run the worker: GPU on, artifacts mounted, AWS creds via instance role
		// (passed through by the metadata service — no keys on the command line).
		strings.Join(dockerRun, " "),
	)
	return strings.Join(lines, "\n")
}
