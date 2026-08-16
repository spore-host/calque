package main

import (
	"reflect"
	"testing"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/ir"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// TestSpawnManifestBody_SingleArgUnchanged proves the overwhelmingly
// common case (a plain, single-arg spawned callable, e.g.
// testdata/scripts/spawn_fanout.py's worker_a(x)) reproduces the pre-
// calque#191 manifest byte-for-byte: no MethodArgs, no Starmap.
func TestSpawnManifestBody_SingleArgUnchanged(t *testing.T) {
	c := calexec.SpawnCallable{MethodBody: "return x * 2", MethodArg: "x", MethodArgs: []string{"x"}}
	body := spawnManifestBody(c, nil, nil, nil, nil)
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
	body := spawnManifestBody(calexec.SpawnCallable{MethodBody: "return 1"}, nil, nil, nil, nil)
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
	body := spawnManifestBody(c, nil, nil, nil, nil)
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
	body := spawnManifestBody(c, nil, nil, nil, nil)
	if !body.Starmap {
		t.Error("Starmap = false, want true for a 2-arg callable")
	}
}

// TestSpawnManifestBody_CarriesExtras (calque#198) proves the resolved
// sibling functions/constants/imports/classes actually land on the
// built ManifestBody — the wiring runSpawnShard depends on to fix the
// real "name '_season_bundle_impl' is not defined" failure found live
// against AI-Almanac's forecasts_app.py.
func TestSpawnManifestBody_CarriesExtras(t *testing.T) {
	extras := []warm.ExtraFunc{{Name: "_season_bundle_impl", Args: []string{"job_id"}, Body: "return job_id"}}
	consts := []warm.ExtraConst{{Name: "SOME_CONST", Source: "SOME_CONST = 1"}}
	imports := []warm.ExtraImport{{Name: "Path", Source: "from pathlib import Path"}}
	classes := []warm.ExtraClass{{Name: "Helper", Source: "class Helper: pass"}}
	body := spawnManifestBody(calexec.SpawnCallable{MethodBody: "return _season_bundle_impl(job_id)", MethodArg: "job_id"}, extras, consts, imports, classes)
	if !reflect.DeepEqual(body.Extras, extras) {
		t.Errorf("Extras = %+v, want %+v", body.Extras, extras)
	}
	if !reflect.DeepEqual(body.ExtraConsts, consts) {
		t.Errorf("ExtraConsts = %+v, want %+v", body.ExtraConsts, consts)
	}
	if !reflect.DeepEqual(body.ExtraImports, imports) {
		t.Errorf("ExtraImports = %+v, want %+v", body.ExtraImports, imports)
	}
	if !reflect.DeepEqual(body.ExtraClasses, classes) {
		t.Errorf("ExtraClasses = %+v, want %+v", body.ExtraClasses, classes)
	}
}

// TestWarmUnitForSpawnCallable_PlainFunction (calque#198) proves a
// plain-@app.function spawn callable resolves to a warmUnit whose
// method carries the REAL ir.Function (with its own LocalCalls/FreeRefs
// intact) — what collectLocalExtras actually walks.
func TestWarmUnitForSpawnCallable_PlainFunction(t *testing.T) {
	app := ir.App{
		Functions: []ir.Function{
			{Name: "run_season_forecast_bundle", Body: "return _season_bundle_impl(job_id)", FreeRefs: []string{"_season_bundle_impl"}, Invoke: ir.InvokeSpawn},
		},
	}
	callable := calexec.SpawnCallable{Key: "run_season_forecast_bundle"}
	unit, ok := warmUnitForSpawnCallable(app, callable)
	if !ok {
		t.Fatal("warmUnitForSpawnCallable: ok = false, want true")
	}
	if !unit.plainFunction || unit.method.Name != "run_season_forecast_bundle" {
		t.Errorf("unit = %+v, want plainFunction=true method.Name=run_season_forecast_bundle", unit)
	}
	if len(unit.method.FreeRefs) != 1 || unit.method.FreeRefs[0] != "_season_bundle_impl" {
		t.Errorf("unit.method.FreeRefs = %v, want [_season_bundle_impl] — collectLocalExtras needs this to resolve the sibling", unit.method.FreeRefs)
	}
}

// TestWarmUnitForSpawnCallable_ClassMethod proves an @app.cls method
// spawn callable resolves to a warmUnit carrying BOTH the owning class
// (for EnterBody/EnterFreeRefs) and the specific method.
func TestWarmUnitForSpawnCallable_ClassMethod(t *testing.T) {
	app := ir.App{
		Classes: []ir.Class{
			{
				Name: "Batcher", EnterBody: "self.model = load()",
				Methods: []ir.Function{
					{Name: "generate", Body: "return self.model(x)", Invoke: ir.InvokeSpawn},
				},
			},
		},
	}
	callable := calexec.SpawnCallable{Key: "generate", IsClass: true}
	unit, ok := warmUnitForSpawnCallable(app, callable)
	if !ok {
		t.Fatal("warmUnitForSpawnCallable: ok = false, want true")
	}
	if unit.class.Name != "Batcher" || unit.method.Name != "generate" {
		t.Errorf("unit = %+v, want class.Name=Batcher method.Name=generate", unit)
	}
}

// TestWarmUnitForSpawnCallable_UnknownKeyReturnsFalse proves a callable
// naming neither a Function nor a Class method returns false — the
// caller's own defensive fallback (skip extras resolution, ship the
// verbatim body only) rather than a panic or a zero-value silent match.
func TestWarmUnitForSpawnCallable_UnknownKeyReturnsFalse(t *testing.T) {
	if _, ok := warmUnitForSpawnCallable(ir.App{}, calexec.SpawnCallable{Key: "does_not_exist"}); ok {
		t.Error("warmUnitForSpawnCallable: ok = true for an unknown key, want false")
	}
}
