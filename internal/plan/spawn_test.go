package plan

import "testing"

func TestSpawnLauncherBuild_TagsReachLaunchConfig(t *testing.T) {
	cfg := SpawnLauncher{
		RunCmd: "true", RunID: "run-abc", Command: "real",
	}.Build()

	want := map[string]string{
		"calque:run-id":  "run-abc",
		"calque:managed": "true",
		"calque:command": "real",
	}
	for k, v := range want {
		if got := cfg.Tags[k]; got != v {
			t.Errorf("Tags[%q] = %q, want %q", k, got, v)
		}
	}
	if _, ok := cfg.Tags["calque:created-at"]; !ok {
		t.Error("Tags[\"calque:created-at\"] missing")
	}
}

func TestSpawnLauncherBuild_NoTagsWhenRunIDEmpty(t *testing.T) {
	cfg := SpawnLauncher{RunCmd: "true"}.Build()
	if cfg.Tags != nil {
		t.Errorf("Tags = %v, want nil when RunID is empty", cfg.Tags)
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
