package main

import (
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// TestVolumeSpecsForApp_NoVolumesReturnsNilBoth proves a script with no
// modal.Volume.from_name(...) mounts (the vast majority) reproduces prior
// behavior byte-for-byte: both sync and commit come back nil, matching the
// hardcoded nil, nil this function replaces at its call sites.
func TestVolumeSpecsForApp_NoVolumesReturnsNilBoth(t *testing.T) {
	app := ir.App{}
	rep := &leak.Report{}

	sync, commit := volumeSpecsForApp(app, "my-bucket", rep)
	if sync != nil {
		t.Errorf("sync = %+v, want nil", sync)
	}
	if commit != nil {
		t.Errorf("commit = %+v, want nil", commit)
	}
}

// TestVolumeSpecsForApp_ResolvesSyncForEveryMountedVolume proves a mounted
// volume (via a @cls/@function's volumes= kwarg) gets a sync spec pointed
// at plan.ResolveVolumes' own stable S3 prefix — this is the actual gap:
// the plumbing existed but nothing called it from a real writer.
func TestVolumeSpecsForApp_ResolvesSyncForEveryMountedVolume(t *testing.T) {
	app := ir.App{
		Volumes: map[string]string{"forecast_volume": "earth2studio-cache"},
		Functions: []ir.Function{
			{Name: "run_forecast_inference", Volumes: map[string]string{"/cache": "earth2studio-cache"}},
		},
	}
	rep := &leak.Report{}

	sync, commit := volumeSpecsForApp(app, "my-bucket", rep)
	if len(sync) != 1 {
		t.Fatalf("sync = %+v, want exactly 1 spec", sync)
	}
	if sync[0].MountPath != "/cache" {
		t.Errorf("sync[0].MountPath = %q, want /cache", sync[0].MountPath)
	}
	wantURI := "s3://my-bucket/volumes/earth2studio-cache"
	if sync[0].URI != wantURI {
		t.Errorf("sync[0].URI = %q, want %q", sync[0].URI, wantURI)
	}
	if len(commit) != 0 {
		t.Errorf("commit = %+v, want none (this volume was never .commit()'d)", commit)
	}
}

// TestVolumeSpecsForApp_CommitOnlyForCommittedVolumes is the key
// var-name-vs-Modal-name reversal test: CommittedVolumes is keyed by the
// module-level Volume VAR name ("forecast_volume"), but VolumeMount.Name
// (from plan.ResolveVolumes) is the Modal volume NAME
// ("earth2studio-cache") — volumeSpecsForApp must reverse app.Volumes to
// bridge them correctly, or commit would silently never fire for any real
// script (every real Volume reference goes through from_name, so the var
// name and the Modal name are essentially always different strings).
func TestVolumeSpecsForApp_CommitOnlyForCommittedVolumes(t *testing.T) {
	app := ir.App{
		Volumes: map[string]string{
			"forecast_volume": "earth2studio-cache", // committed
			"scratch_volume":  "scratch-cache",      // never committed
		},
		Functions: []ir.Function{
			{Name: "run_forecast_inference", Volumes: map[string]string{
				"/cache":   "earth2studio-cache",
				"/scratch": "scratch-cache",
			}},
		},
		CommittedVolumes: map[string]bool{"forecast_volume": true},
	}
	rep := &leak.Report{}

	sync, commit := volumeSpecsForApp(app, "my-bucket", rep)
	if len(sync) != 2 {
		t.Fatalf("sync = %+v, want 2 (both mounted volumes get staged)", sync)
	}
	if len(commit) != 1 {
		t.Fatalf("commit = %+v, want exactly 1 (only forecast_volume was .commit()'d)", commit)
	}
	if commit[0].MountPath != "/cache" {
		t.Errorf("commit[0].MountPath = %q, want /cache (earth2studio-cache's mount, not scratch-cache's)", commit[0].MountPath)
	}
}
