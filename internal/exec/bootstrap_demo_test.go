package exec

import (
	"strings"
	"testing"
)

func TestBootstrapCommandShape(t *testing.T) {
	c := BootstrapConfig{BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json", Region: "us-west-2"}
	cmd := c.Command()
	for _, w := range []string{"--gpus all", "aws s3 cp", "docker pull vllm/vllm-openai", "warmd", "--manifest s3://b/runs/x/manifest.json"} {
		if !strings.Contains(cmd, w) {
			t.Errorf("missing %q in:\n%s", w, cmd)
		}
	}
}

// TestBootstrapCommandHostModeNoDepsUsesDnfFallbackToo (calque#148 follow-up)
// proves the plain host-mode path (no PipPackages) no longer assumes
// apt-get is the ONLY package manager present — confirmed live via SSM
// against calque#148's own investigation that Amazon Linux 2023 (the
// AMI spawn auto-selects for non-GPU instance types) has dnf, not
// apt-get. This is the path taken when the caller supplies no real
// deps at all — matches the smoke test's own HostMode usage.
func TestBootstrapCommandHostModeNoDepsStillProvisionsUv(t *testing.T) {
	c := BootstrapConfig{Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json", Region: "us-west-2", HostMode: true}
	cmd := c.Command()
	for _, w := range []string{
		"curl -LsSf https://astral.sh/uv/install.sh",
		"uv python install 3.12",
		"uv venv --python 3.12",
		`export PATH="/opt/calque/.venv/bin:$PATH"`,
	} {
		if !strings.Contains(cmd, w) {
			t.Errorf("no-deps host-mode command should still provision a uv-managed venv (calque#148, widened — never depend on the AMI's distro package manager for Python); missing %q in:\n%s", w, cmd)
		}
	}
	if strings.Contains(cmd, "apt-get install -y python3") || strings.Contains(cmd, "dnf install -y python3") {
		t.Errorf("no-deps host-mode command should NOT fall back to the AMI's own distro python3 anymore; got:\n%s", cmd)
	}
	// calque#200: "modal" is ALWAYS installed now, mirroring uvPythonArgv's
	// (run.go, dry-run's own mechanism) explicit design decision — even
	// the "no real --pip packages supplied" case has a real Python
	// package to install, since a real script's body routinely
	// references modal.Secret/modal.Volume/etc. directly.
	if !strings.Contains(cmd, `uv pip install --python /opt/calque/.venv/bin/python3 "modal"`) {
		t.Errorf("no-deps host-mode command must still install \"modal\" itself (calque#200); got:\n%s", cmd)
	}
	if strings.Contains(cmd, "command -v git") {
		t.Errorf("no real --pip package (git-URL or otherwise) was supplied, so the git-availability check should be skipped; got:\n%s", cmd)
	}
}

// TestBootstrapCommandHostModeWithPipInstallsUvAndPackages (calque#148
// follow-up) proves that supplying PipPackages switches host mode to the
// uv-managed venv path: uv itself installed via its own curl script (not
// a distro package manager assumption), a Python version pinned, and
// every requested package installed into that venv — closing the
// previous "dependencies must already be on the AMI" gap this issue's
// own live validation (blending_app.py's inspect_netcdf_bundle,
// `No module named 'xarray'`) hit.
func TestBootstrapCommandHostModeWithPipInstallsUvAndPackages(t *testing.T) {
	c := BootstrapConfig{
		Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json",
		Region: "us-west-2", HostMode: true, WorkerDir: "/tmp/calque",
		PipPackages: []string{"xarray", "h5netcdf"}, PythonVersion: "3.11",
	}
	cmd := c.Command()
	for _, w := range []string{
		"curl -LsSf https://astral.sh/uv/install.sh",
		"uv python install 3.11",
		"uv venv --python 3.11 /tmp/calque/.venv",
		`"xarray"`, `"h5netcdf"`,
		"uv pip install --python /tmp/calque/.venv/bin/python3",
	} {
		if !strings.Contains(cmd, w) {
			t.Errorf("missing %q in:\n%s", w, cmd)
		}
	}
	// warmd invocation must NOT hardcode a python3 in the command line —
	// the interpreter comes from the MANIFEST's own PythonBin field
	// (cmd/warmd/main.go's pyOr), which the caller (realrun.go) must set
	// to the SAME venv path this command creates.
	if strings.Contains(cmd, "command -v python3") {
		t.Errorf("the uv-managed-venv path should not also probe for a system python3; got:\n%s", cmd)
	}
	// calque#200: "modal" is always merged in alongside real --pip
	// packages, deduped/sorted (matching uvPythonArgv's exact discipline).
	if !strings.Contains(cmd, `"h5netcdf" "modal" "xarray"`) {
		t.Errorf(`expected the deduped/sorted package list "h5netcdf" "modal" "xarray" in:%s`, cmd)
	}
}

// TestBootstrapCommandHostModeModalNotDuplicatedIfExplicitlyRequested
// (calque#200) proves passing --pip modal explicitly doesn't produce a
// duplicate "modal" "modal" entry — the same map-based dedup uvPythonArgv
// already relies on.
func TestBootstrapCommandHostModeModalNotDuplicatedIfExplicitlyRequested(t *testing.T) {
	c := BootstrapConfig{
		Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json",
		Region: "us-west-2", HostMode: true, WorkerDir: "/tmp/calque",
		PipPackages: []string{"modal", "xarray"},
	}
	cmd := c.Command()
	if strings.Count(cmd, `"modal"`) != 1 {
		t.Errorf(`expected exactly one "modal" entry, got %d in:%s`, strings.Count(cmd, `"modal"`), cmd)
	}
}

// TestBootstrapCommandHostModeWithPipEnsuresGitPresent is the regression
// test for a real bug found running app.py's run_benchmark_local on real
// AWS (m6i.large, AL2023): a --pip package spec can be a git URL (momp has
// no PyPI release, only "momp @ git+https://github.com/hholb/ROMP.git@main")
// — uv pip install shells out to a real git binary for that, which AL2023
// does NOT ship by default. The bootstrap script must ensure git is present
// (apt-get-first/dnf-fallback, matching this file's existing distro-
// detection pattern) BEFORE the uv pip install line, not just uv/python3.
func TestBootstrapCommandHostModeWithPipEnsuresGitPresent(t *testing.T) {
	c := BootstrapConfig{
		Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json",
		Region: "us-west-2", HostMode: true, WorkerDir: "/tmp/calque",
		PipPackages: []string{"momp @ git+https://github.com/hholb/ROMP.git@main"}, PythonVersion: "3.11",
	}
	cmd := c.Command()
	if !strings.Contains(cmd, "command -v git") {
		t.Errorf("expected a git presence check before uv pip install; got:\n%s", cmd)
	}
	gitIdx := strings.Index(cmd, "command -v git")
	pipInstallIdx := strings.Index(cmd, "uv pip install")
	if gitIdx == -1 || pipInstallIdx == -1 || gitIdx > pipInstallIdx {
		t.Errorf("git presence check must run BEFORE uv pip install; got:\n%s", cmd)
	}
}

// TestBootstrapCommandStagesFilesBeforeWarmdRuns proves StageFiles
// downloads each URL to its exact destination path (parent dirs made
// first), in deterministic order, BEFORE warmd is invoked — needed for
// a script body that shells out to a hardcoded absolute path its
// original Docker image would have placed there (e.g. AI-Almanac's
// app.py hardcoding "/app/scripts/generate_config.py", ROMP's own
// Dockerfile convention, not anything Modal defines).
func TestBootstrapCommandStagesFilesBeforeWarmdRuns(t *testing.T) {
	c := BootstrapConfig{
		Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json",
		Region: "us-west-2", HostMode: true, WorkerDir: "/tmp/calque",
		StageFiles: map[string]string{
			"https://example.com/generate_config.py": "/app/scripts/generate_config.py",
		},
	}
	cmd := c.Command()
	for _, w := range []string{
		`sudo mkdir -p "/app/scripts"`,
		`sudo curl -LsSf "https://example.com/generate_config.py" -o "/app/scripts/generate_config.py"`,
	} {
		if !strings.Contains(cmd, w) {
			t.Errorf("missing %q in:\n%s", w, cmd)
		}
	}
	if strings.Index(cmd, "curl -LsSf \"https://example.com") > strings.Index(cmd, "AWS_REGION=") {
		t.Errorf("staged file download must happen BEFORE warmd runs; got:\n%s", cmd)
	}
}

// TestBootstrapCommandHostModeWithPipDefaultsPythonVersion proves an
// unset PythonVersion still gets SOME pinned version (not left to
// whatever the AMI's system python3 happens to be) when PipPackages is
// non-empty.
func TestBootstrapCommandHostModeWithPipDefaultsPythonVersion(t *testing.T) {
	c := BootstrapConfig{
		Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json",
		Region: "us-west-2", HostMode: true, WorkerDir: "/tmp/calque",
		PipPackages: []string{"xarray"},
	}
	cmd := c.Command()
	if !strings.Contains(cmd, "uv python install") {
		t.Errorf("expected a pinned uv python install even with PythonVersion unset; got:\n%s", cmd)
	}
}

// TestBootstrapCommandDockerModeWithECRRegistryRefLogsIn (calque#176) proves
// a docker-mode run whose RegistryRef resolved to an ECR hostname
// authenticates via `aws ecr get-login-password` (scoped to the region
// embedded in the hostname) BEFORE the docker pull, and pulls/runs THAT
// ref — not the hardcoded BaseImage default.
func TestBootstrapCommandDockerModeWithECRRegistryRefLogsIn(t *testing.T) {
	c := BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art",
		ManifestKey: "runs/x/manifest.json", Region: "us-west-2",
		RegistryRef: "123456789012.dkr.ecr.us-west-2.amazonaws.com/myrepo:latest",
	}
	cmd := c.Command()
	for _, w := range []string{
		"aws ecr get-login-password --region us-west-2",
		"sudo docker login --username AWS --password-stdin 123456789012.dkr.ecr.us-west-2.amazonaws.com",
		"sudo docker pull 123456789012.dkr.ecr.us-west-2.amazonaws.com/myrepo:latest",
	} {
		if !strings.Contains(cmd, w) {
			t.Errorf("missing %q in:\n%s", w, cmd)
		}
	}
	if strings.Contains(cmd, "vllm/vllm-openai") {
		t.Errorf("RegistryRef should override BaseImage entirely, not just add to it; got:\n%s", cmd)
	}
	loginIdx := strings.Index(cmd, "docker login")
	pullIdx := strings.Index(cmd, "docker pull")
	if loginIdx == -1 || pullIdx == -1 || loginIdx > pullIdx {
		t.Errorf("docker login must run BEFORE docker pull; got:\n%s", cmd)
	}
}

// TestBootstrapCommandDockerModeWithoutRegistryRefUnchanged (calque#176)
// proves the default (empty RegistryRef) path is byte-for-byte the same
// as before this feature existed: no docker login line at all, and the
// existing hardcoded BaseImage is still what gets pulled/run.
func TestBootstrapCommandDockerModeWithoutRegistryRefUnchanged(t *testing.T) {
	c := BootstrapConfig{BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json", Region: "us-west-2"}
	cmd := c.Command()
	if strings.Contains(cmd, "docker login") {
		t.Errorf("no RegistryRef set — must not emit a docker login step; got:\n%s", cmd)
	}
	if !strings.Contains(cmd, "docker pull vllm/vllm-openai:latest") {
		t.Errorf("empty RegistryRef must fall back to BaseImage unchanged; got:\n%s", cmd)
	}
}

// TestBootstrapCommandDockerModeWithBuildDockerfileBuildsLocally (calque#177)
// proves BuildDockerfile builds the caller-uploaded Dockerfile ON THE
// INSTANCE (from the artifact prefix's own working dir, already downloaded
// by the existing `aws s3 cp --recursive` line) instead of pulling ANY
// image, and runs the resulting tag — not RegistryRef/BaseImage.
func TestBootstrapCommandDockerModeWithBuildDockerfileBuildsLocally(t *testing.T) {
	c := BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art",
		ManifestKey: "runs/x/manifest.json", Region: "us-west-2", WorkerDir: "/opt/calque",
		BuildDockerfile: true, BuildTag: "calque-local:abc123",
	}
	cmd := c.Command()
	for _, w := range []string{
		"sudo docker build -t calque-local:abc123 -f /opt/calque/Dockerfile /opt/calque",
	} {
		if !strings.Contains(cmd, w) {
			t.Errorf("missing %q in:\n%s", w, cmd)
		}
	}
	if strings.Contains(cmd, "docker pull") {
		t.Errorf("BuildDockerfile should build, never pull; got:\n%s", cmd)
	}
	if !strings.Contains(cmd, "calque-local:abc123 run --manifest") {
		t.Errorf("docker run should use the BUILT tag, not BaseImage; got:\n%s", cmd)
	}
	buildIdx := strings.Index(cmd, "docker build")
	runIdx := strings.Index(cmd, "docker run")
	if buildIdx == -1 || runIdx == -1 || buildIdx > runIdx {
		t.Errorf("docker build must run BEFORE docker run; got:\n%s", cmd)
	}
}

// TestBootstrapCommandDockerModeWithBuildDockerfileAndECRRegistryRefLogsIn
// (calque#177) proves a build whose Dockerfile's OWN FROM line is a
// private ECR ref (RegistryRef carries that base ref through even when
// BuildDockerfile is set) still authenticates before the build, same as a
// plain pull would — docker build needs to pull that base itself.
func TestBootstrapCommandDockerModeWithBuildDockerfileAndECRRegistryRefLogsIn(t *testing.T) {
	c := BootstrapConfig{
		Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json",
		Region: "us-west-2", WorkerDir: "/opt/calque",
		RegistryRef:     "123456789012.dkr.ecr.us-west-2.amazonaws.com/base:latest",
		BuildDockerfile: true, BuildTag: "calque-local:abc123",
	}
	cmd := c.Command()
	if !strings.Contains(cmd, "aws ecr get-login-password --region us-west-2") {
		t.Errorf("missing ECR login before build; got:\n%s", cmd)
	}
	loginIdx := strings.Index(cmd, "docker login")
	buildIdx := strings.Index(cmd, "docker build")
	if loginIdx == -1 || buildIdx == -1 || loginIdx > buildIdx {
		t.Errorf("docker login must run BEFORE docker build; got:\n%s", cmd)
	}
}

// TestBootstrapCommandDockerModeWithoutRegistryRefUnchanged (calque#176)
// proves a RegistryRef pointing at a non-ECR registry (e.g. GCP Artifact
// Registry, as app.py's own real ROMP_IMAGE_URI default does) is pulled
// anonymously — same as today's BaseImage behavior — since calque has no
// credential-sourcing mechanism for it.
func TestBootstrapCommandDockerModeWithNonECRRegistryRefNoLogin(t *testing.T) {
	c := BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art",
		ManifestKey: "runs/x/manifest.json", Region: "us-west-2",
		RegistryRef: "us-central1-docker.pkg.dev/ai-almanac/almanac/romp:latest",
	}
	cmd := c.Command()
	if strings.Contains(cmd, "docker login") {
		t.Errorf("non-ECR RegistryRef must not attempt a docker login (no credential mechanism); got:\n%s", cmd)
	}
	if !strings.Contains(cmd, "sudo docker pull us-central1-docker.pkg.dev/ai-almanac/almanac/romp:latest") {
		t.Errorf("non-ECR RegistryRef should still be pulled (anonymously); got:\n%s", cmd)
	}
}

// TestBootstrapCommandSplicesCloudBucketMountLinesDockerMode (calque#91
// Workstream A) proves CloudBucketMountLines are spliced into Command()'s
// output AFTER the artifact sync but BEFORE the docker run invocation —
// the mount must be live before @enter runs.
func TestBootstrapCommandSplicesCloudBucketMountLinesDockerMode(t *testing.T) {
	c := BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art",
		ManifestKey: "runs/x/manifest.json", Region: "us-west-2",
		CloudBucketMountLines: []string{"mkdir -p /data", "mount-s3 my-bucket /data"},
	}
	cmd := c.Command()
	for _, w := range []string{"mkdir -p /data", "mount-s3 my-bucket /data"} {
		if !strings.Contains(cmd, w) {
			t.Errorf("missing %q in:\n%s", w, cmd)
		}
	}
	syncIdx := strings.Index(cmd, "aws s3 cp --recursive")
	mountIdx := strings.Index(cmd, "mount-s3 my-bucket /data")
	runIdx := strings.Index(cmd, "docker run")
	if syncIdx == -1 || mountIdx == -1 || runIdx == -1 {
		t.Fatalf("missing expected markers in:\n%s", cmd)
	}
	if syncIdx >= mountIdx || mountIdx >= runIdx {
		t.Errorf("expected order artifact-sync < cloud-bucket-mount < docker-run; got:\n%s", cmd)
	}
}

// TestBootstrapCommandSplicesCloudBucketMountLinesHostMode is the HostMode
// sibling: the mount must be live before warmd itself runs (host mode has
// no docker run invocation at all).
func TestBootstrapCommandSplicesCloudBucketMountLinesHostMode(t *testing.T) {
	c := BootstrapConfig{
		Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json",
		Region: "us-west-2", HostMode: true,
		CloudBucketMountLines: []string{"mkdir -p /data", "mount-s3 my-bucket /data"},
	}
	cmd := c.Command()
	mountIdx := strings.Index(cmd, "mount-s3 my-bucket /data")
	warmdIdx := strings.Index(cmd, "warmd run --manifest")
	if mountIdx == -1 || warmdIdx == -1 {
		t.Fatalf("missing expected markers in:\n%s", cmd)
	}
	if mountIdx > warmdIdx {
		t.Errorf("cloud-bucket-mount lines must run BEFORE warmd; got:\n%s", cmd)
	}
}

// TestBootstrapCommandNoCloudBucketMountLinesUnchanged proves the default
// (empty CloudBucketMountLines) reproduces prior behavior byte-for-byte: no
// mount-s3 anything appears at all.
func TestBootstrapCommandNoCloudBucketMountLinesUnchanged(t *testing.T) {
	c := BootstrapConfig{BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json", Region: "us-west-2"}
	cmd := c.Command()
	if strings.Contains(cmd, "mount-s3") {
		t.Errorf("no CloudBucketMountLines set — must not emit any mount-s3 reference; got:\n%s", cmd)
	}
}

// TestBootstrapCommandSplicesNFSMountLinesDockerMode (calque#91 Workstream
// B) proves NFSMountLines are spliced into Command()'s output AFTER the
// artifact sync but BEFORE the docker run invocation — mirrors
// TestBootstrapCommandSplicesCloudBucketMountLinesDockerMode exactly.
func TestBootstrapCommandSplicesNFSMountLinesDockerMode(t *testing.T) {
	c := BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art",
		ManifestKey: "runs/x/manifest.json", Region: "us-west-2",
		NFSMountLines: []string{"mkdir -p /shared", "mount -t nfs4 fs-abc123.efs.us-west-2.amazonaws.com:/ /shared"},
	}
	cmd := c.Command()
	for _, w := range []string{"mkdir -p /shared", "mount -t nfs4 fs-abc123.efs.us-west-2.amazonaws.com:/ /shared"} {
		if !strings.Contains(cmd, w) {
			t.Errorf("missing %q in:\n%s", w, cmd)
		}
	}
	syncIdx := strings.Index(cmd, "aws s3 cp --recursive")
	mountIdx := strings.Index(cmd, "mount -t nfs4")
	runIdx := strings.Index(cmd, "docker run")
	if syncIdx == -1 || mountIdx == -1 || runIdx == -1 {
		t.Fatalf("missing expected markers in:\n%s", cmd)
	}
	if syncIdx >= mountIdx || mountIdx >= runIdx {
		t.Errorf("expected order artifact-sync < nfs-mount < docker-run; got:\n%s", cmd)
	}
}

// TestBootstrapCommandSplicesNFSMountLinesHostMode is the HostMode sibling:
// the mount must be live before warmd itself runs.
func TestBootstrapCommandSplicesNFSMountLinesHostMode(t *testing.T) {
	c := BootstrapConfig{
		Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json",
		Region: "us-west-2", HostMode: true,
		NFSMountLines: []string{"mkdir -p /shared", "mount -t nfs4 fs-abc123.efs.us-west-2.amazonaws.com:/ /shared"},
	}
	cmd := c.Command()
	mountIdx := strings.Index(cmd, "mount -t nfs4")
	warmdIdx := strings.Index(cmd, "warmd run --manifest")
	if mountIdx == -1 || warmdIdx == -1 {
		t.Fatalf("missing expected markers in:\n%s", cmd)
	}
	if mountIdx > warmdIdx {
		t.Errorf("nfs-mount lines must run BEFORE warmd; got:\n%s", cmd)
	}
}

// TestBootstrapCommandNoNFSMountLinesUnchanged proves the default (empty
// NFSMountLines) reproduces prior behavior byte-for-byte: no NFS mount
// reference appears at all.
func TestBootstrapCommandNoNFSMountLinesUnchanged(t *testing.T) {
	c := BootstrapConfig{BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json", Region: "us-west-2"}
	cmd := c.Command()
	if strings.Contains(cmd, "mount -t nfs4") {
		t.Errorf("no NFSMountLines set — must not emit any nfs4 mount reference; got:\n%s", cmd)
	}
}

// TestBootstrapCommandSplicesBothCloudBucketMountAndNFSMountLines proves
// both mount kinds can coexist in one run — order between them doesn't
// matter (per doc comment), but both must land after artifact sync and
// before docker run.
func TestBootstrapCommandSplicesBothCloudBucketMountAndNFSMountLines(t *testing.T) {
	c := BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: "b", ArtifactPrefix: "runs/x/art",
		ManifestKey: "runs/x/manifest.json", Region: "us-west-2",
		CloudBucketMountLines: []string{"mount-s3 my-bucket /data"},
		NFSMountLines:         []string{"mount -t nfs4 fs-abc123.efs.us-west-2.amazonaws.com:/ /shared"},
	}
	cmd := c.Command()
	for _, w := range []string{"mount-s3 my-bucket /data", "mount -t nfs4 fs-abc123.efs.us-west-2.amazonaws.com:/ /shared"} {
		if !strings.Contains(cmd, w) {
			t.Errorf("missing %q in:\n%s", w, cmd)
		}
	}
	syncIdx := strings.Index(cmd, "aws s3 cp --recursive")
	runIdx := strings.Index(cmd, "docker run")
	s3MountIdx := strings.Index(cmd, "mount-s3 my-bucket /data")
	nfsMountIdx := strings.Index(cmd, "mount -t nfs4")
	if syncIdx >= s3MountIdx || s3MountIdx >= runIdx || syncIdx >= nfsMountIdx || nfsMountIdx >= runIdx {
		t.Errorf("both mount kinds must land after artifact-sync and before docker-run; got:\n%s", cmd)
	}
}
