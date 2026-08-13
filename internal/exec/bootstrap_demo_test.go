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
func TestBootstrapCommandHostModeNoDepsUsesDnfFallbackToo(t *testing.T) {
	c := BootstrapConfig{Bucket: "b", ArtifactPrefix: "runs/x/art", ManifestKey: "runs/x/manifest.json", Region: "us-west-2", HostMode: true}
	cmd := c.Command()
	if !strings.Contains(cmd, "dnf install") {
		t.Errorf("no-deps host-mode command should fall back to dnf (AL2023 has no apt-get) as well as apt-get; got:\n%s", cmd)
	}
	if strings.Contains(cmd, "uv") {
		t.Errorf("no-deps host-mode command should NOT install uv at all (nothing to pip-install); got:\n%s", cmd)
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
