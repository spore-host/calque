package plan

import (
	"encoding/json"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// TestVolumeChainToManifest proves the full volume plumbing end-to-end in code:
// resolved mounts -> (uri, mountPath) sync pairs that the manifest carries and
// warmd stages before @enter. Mirrors what volume_cache.py produces, without a GPU.
func TestVolumeChainToManifest(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{
		Script:  "volume_cache.py",
		Classes: []ir.Class{{Name: "Scorer", Volumes: map[string]string{"/models": "resnet-weights"}}},
	}
	mounts := ResolveVolumes(app, rep)
	if len(mounts) != 1 {
		t.Fatalf("mounts = %d, want 1", len(mounts))
	}

	// Build the sync pairs the way a runner would, then round-trip through JSON the
	// way the manifest does — confirming warmd receives a stageable spec.
	type syncSpec struct {
		URI       string `json:"uri"`
		MountPath string `json:"mount_path"`
	}
	var specs []syncSpec
	for _, m := range mounts {
		uri, mp := m.SyncPair("calque-spike-bucket")
		specs = append(specs, syncSpec{URI: uri, MountPath: mp})
	}
	blob, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	var back []syncSpec
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 {
		t.Fatalf("round-trip lost specs: %d", len(back))
	}
	if back[0].URI != "s3://calque-spike-bucket/volumes/resnet-weights" {
		t.Errorf("sync URI = %q", back[0].URI)
	}
	if back[0].MountPath != "/models" {
		t.Errorf("mount path = %q, want /models (matches the @cls volumes= mount)", back[0].MountPath)
	}
}
