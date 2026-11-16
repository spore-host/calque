package plan

import (
	"strings"
	"testing"

	"github.com/spore-host/libs/tags"
)

func TestSpawnLauncherBuild_TagsReachLaunchConfig(t *testing.T) {
	cfg := SpawnLauncher{
		RunCmd: "true", RunID: "run-abc", Command: "real",
	}.Build()

	want := map[string]string{
		"calque:run-id":  "run-abc",
		"calque:managed": "true",
		"calque:command": "real",
		// calque#227: the shared spore-host:* set, managed-by=calque.
		tags.ManagedBy: "calque",
		tags.Scope:     "run:run-abc",
		tags.Schema:    tags.SchemaVersion,
	}
	for k, v := range want {
		if got := cfg.Tags[k]; got != v {
			t.Errorf("Tags[%q] = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"calque:created-at", tags.Created} {
		if _, ok := cfg.Tags[k]; !ok {
			t.Errorf("Tags[%q] missing", k)
		}
	}
	// spawn's own keys are reserved; calque must not try to set them.
	for k := range cfg.Tags {
		if strings.HasPrefix(k, "spawn:") {
			t.Errorf("Tags carries spawn-reserved key %q", k)
		}
	}
}

// TestSpawnLauncherBuild_TaggedAsCalqueWithoutRunID (calque#227): an instance
// launched with no run id (gpuprobe) used to go out with no calque tags at
// all, so nothing could tell it was calque's. It now carries managed-by and
// calque:managed; only calque:run-id and spore-host:scope are left out.
func TestSpawnLauncherBuild_TaggedAsCalqueWithoutRunID(t *testing.T) {
	cfg := SpawnLauncher{RunCmd: "true", Command: "gpuprobe"}.Build()
	if got := cfg.Tags[tags.ManagedBy]; got != "calque" {
		t.Errorf("Tags[%q] = %q, want calque", tags.ManagedBy, got)
	}
	if got := cfg.Tags["calque:managed"]; got != "true" {
		t.Errorf("Tags[calque:managed] = %q, want true", got)
	}
	for _, k := range []string{"calque:run-id", tags.Scope} {
		if v, ok := cfg.Tags[k]; ok {
			t.Errorf("Tags[%q] = %q, want it absent without a run id", k, v)
		}
	}
}

// TestSpawnLauncherBuild_ThreadsSecurityGroupIDs (calque#91 Workstream B)
// proves SecurityGroupIDs reaches the built spawnaws.LaunchConfig — needed
// for the NFS ingress security group EnsureNFSSecurityGroup (efs.go)
// resolves to actually get attached to the launched instance.
func TestSpawnLauncherBuild_ThreadsSecurityGroupIDs(t *testing.T) {
	cfg := SpawnLauncher{RunCmd: "true", SecurityGroupIDs: []string{"sg-abc123"}}.Build()
	if len(cfg.SecurityGroupIDs) != 1 || cfg.SecurityGroupIDs[0] != "sg-abc123" {
		t.Errorf("SecurityGroupIDs = %v, want [sg-abc123]", cfg.SecurityGroupIDs)
	}
}

// TestSpawnLauncherBuild_NoSecurityGroupIDsWhenUnset proves the default
// (empty SecurityGroupIDs) reproduces prior behavior byte-for-byte: nil
// reaches spawnaws.LaunchConfig, so spawn creates its own default SG as it
// always has.
func TestSpawnLauncherBuild_NoSecurityGroupIDsWhenUnset(t *testing.T) {
	cfg := SpawnLauncher{RunCmd: "true"}.Build()
	if cfg.SecurityGroupIDs != nil {
		t.Errorf("SecurityGroupIDs = %v, want nil when unset", cfg.SecurityGroupIDs)
	}
}
