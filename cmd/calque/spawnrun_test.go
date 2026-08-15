package main

import (
	"reflect"
	"testing"

	calexec "github.com/spore-host/calque/internal/exec"
)

// TestSpawnManifestBody_SingleArgUnchanged proves the overwhelmingly
// common case (a plain, single-arg spawned callable, e.g.
// testdata/scripts/spawn_fanout.py's worker_a(x)) reproduces the pre-
// calque#191 manifest byte-for-byte: no MethodArgs, no Starmap.
func TestSpawnManifestBody_SingleArgUnchanged(t *testing.T) {
	c := calexec.SpawnCallable{MethodBody: "return x * 2", MethodArg: "x", MethodArgs: []string{"x"}}
	body := spawnManifestBody(c)
	if body.MethodArg != "x" {
		t.Errorf("MethodArg = %q, want %q", body.MethodArg, "x")
	}
	if body.Starmap {
		t.Error("Starmap = true, want false for a single-arg callable")
	}
	if body.MethodArgs != nil {
		t.Errorf("MethodArgs = %v, want nil for a single-arg callable", body.MethodArgs)
	}
}

// TestSpawnManifestBody_NoArgsDefaultsToItem proves a zero-arg
// SpawnCallable (MethodArg never populated — shouldn't happen in
// practice, but ResolveSpawnCallables' own zero value) falls back to
// "item", matching every other single-arg invocation kind's default.
func TestSpawnManifestBody_NoArgsDefaultsToItem(t *testing.T) {
	body := spawnManifestBody(calexec.SpawnCallable{MethodBody: "return 1"})
	if body.MethodArg != "item" {
		t.Errorf("MethodArg = %q, want %q", body.MethodArg, "item")
	}
}

// TestSpawnManifestBody_MultiArgSetsStarmap (calque#191) is the actual
// fix under test: a callable with 2+ real args (e.g.
// run_forecast_inference(job_id, model_id, config)) must set
// Starmap=true and carry the FULL arg list — this is what routes the
// multi-arg payload spawnArgsPayload already builds through runner.py's
// existing tuple-splat mechanism instead of silently binding only the
// first name.
func TestSpawnManifestBody_MultiArgSetsStarmap(t *testing.T) {
	c := calexec.SpawnCallable{
		MethodBody: "return {'job_id': job_id}", MethodArg: "job_id",
		MethodArgs: []string{"job_id", "model_id", "config"},
	}
	body := spawnManifestBody(c)
	if !body.Starmap {
		t.Error("Starmap = false, want true for a 3-arg callable")
	}
	want := []string{"job_id", "model_id", "config"}
	if !reflect.DeepEqual(body.MethodArgs, want) {
		t.Errorf("MethodArgs = %v, want %v", body.MethodArgs, want)
	}
	if body.MethodArg != "job_id" {
		t.Errorf("MethodArg = %q, want %q (kept for backward compat / non-starmap fallback readers)", body.MethodArg, "job_id")
	}
}

// TestSpawnManifestBody_TwoArgsAlsoSplats proves the threshold is "more
// than one arg," not some higher number — 2 args is already enough to
// need the splat path.
func TestSpawnManifestBody_TwoArgsAlsoSplats(t *testing.T) {
	c := calexec.SpawnCallable{MethodBody: "...", MethodArg: "a", MethodArgs: []string{"a", "b"}}
	body := spawnManifestBody(c)
	if !body.Starmap {
		t.Error("Starmap = false, want true for a 2-arg callable")
	}
}
