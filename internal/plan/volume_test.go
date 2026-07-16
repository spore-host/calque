package plan

import (
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

func TestResolveVolumesDeterministicPrefix(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{
		Script: "s.py",
		Classes: []ir.Class{
			{Name: "Scorer", Volumes: map[string]string{"/models": "resnet-weights"}},
		},
		Functions: []ir.Function{
			{Name: "download", Volumes: map[string]string{"/models": "resnet-weights"}},
		},
	}
	mounts := ResolveVolumes(app, rep)
	if len(mounts) != 1 {
		t.Fatalf("mounts = %d, want 1 (same vol at same path, deduped)", len(mounts))
	}
	m := mounts[0]
	if m.Name != "resnet-weights" || m.MountPath != "/models" {
		t.Errorf("mount = %+v", m)
	}
	// Prefix must derive from the NAME (persists across runs -> cache reuse), not the run.
	if m.S3Prefix != "volumes/resnet-weights/" {
		t.Errorf("prefix = %q, want volumes/resnet-weights/", m.S3Prefix)
	}
	if got := m.URI("mybucket"); got != "s3://mybucket/volumes/resnet-weights" {
		t.Errorf("URI = %q", got)
	}
	if rep.Len() != 0 {
		t.Errorf("clean volumes should not leak: %+v", rep.Leaks)
	}
}

// TestSyncUsesSyncNotCp locks the cache-reuse semantic: staging must use
// `aws s3 sync` (delta-only, warm cache = near-noop), never `cp --recursive`
// (full re-download every run).
func TestSyncUsesSyncNotCp(t *testing.T) {
	mounts := []VolumeMount{{Name: "w", S3Prefix: "volumes/w/", MountPath: "/weights"}}
	lines := SyncCommands("b", mounts)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "aws s3 sync") {
		t.Errorf("must use `aws s3 sync` for warm-cache reuse, got:\n%s", joined)
	}
	if strings.Contains(joined, "cp --recursive") {
		t.Errorf("must NOT use cp --recursive (defeats cache reuse):\n%s", joined)
	}
	if !strings.Contains(joined, "mkdir -p /weights") {
		t.Errorf("must create the mount path:\n%s", joined)
	}
	if !strings.Contains(joined, "s3://b/volumes/w /weights") {
		t.Errorf("must sync the volume prefix to the mount path:\n%s", joined)
	}
}

// TestVolumeConflictLeaks: two different volumes at one mount path is a conflict
// we surface, not guess through.
func TestVolumeConflictLeaks(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{
		Script: "s.py",
		Classes: []ir.Class{
			{Name: "A", Volumes: map[string]string{"/mnt": "vol-one"}, Line: 5},
			{Name: "B", Volumes: map[string]string{"/mnt": "vol-two"}, Line: 9},
		},
	}
	ResolveVolumes(app, rep)
	if rep.Len() == 0 {
		t.Error("two volumes at one mount path should leak a conflict")
	}
}

func TestNoVolumesNoMounts(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{Script: "s.py", Classes: []ir.Class{{Name: "C"}}}
	if got := ResolveVolumes(app, rep); len(got) != 0 {
		t.Errorf("no volumes -> no mounts, got %+v", got)
	}
}
