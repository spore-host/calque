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
