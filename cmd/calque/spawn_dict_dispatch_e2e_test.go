package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// TestSpawnDictDispatchEndToEndProducesRealShards is the end-to-end
// regression guard for calque#189: it wires Parse -> ResolveSpawnCallables
// -> SpawnCallSitesReport -> BuildSpawnManifests together exactly as
// spawnRunFromScript (spawnrun.go) does, against a real fixture whose
// .spawn() receiver is a dict-of-functions Subscript (mirroring
// AI-Almanac's forecasts_app.py exactly).
//
// This test exists because the original #189 fix (SpawnCallSites'
// candidate expansion) was necessary but NOT sufficient on its own: a
// diagnostic run of this exact pipeline, built to answer "does spawn-run
// actually produce a shard for this script," found that
// ResolveSpawnCallables returned ZERO callables even after that fix — the
// picked function's own ir.Function.Invoke was still "" (not
// ir.InvokeSpawn), because invocationKinds' consider() call for the
// "spawn" case was still keyed on the empty ic.Target, never the
// candidates. SpawnCallSites (the call-SITE side) and
// ResolveSpawnCallables (the callable-DEFINITION side) are two
// independently-tested layers; the defect lived in the gap BETWEEN them,
// invisible to either layer's own isolated unit tests. Only assembling the
// real pipeline end-to-end — not just asserting on SpawnCallSites' return
// value in isolation — surfaced it.
func TestSpawnDictDispatchEndToEndProducesRealShards(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping pyast contract test")
	}
	setPyastDirEnv(t)
	script, err := filepath.Abs("../../testdata/scripts/spawn_dict_dispatch.py")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rep := &leak.Report{}
	runner, args := parse.DefaultRunner(pyastDir())

	app, err := parse.Parse(ctx, script, rep, runner, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var bundle ir.Function
	found := false
	for _, f := range app.Functions {
		if f.Name == "bundle" {
			bundle, found = f, true
		}
	}
	if !found {
		t.Fatal(`app.Functions has no "bundle" — fixture regressed`)
	}
	if bundle.Invoke != ir.InvokeSpawn {
		t.Errorf(`bundle.Invoke = %q, want %q — a dict-subscript .spawn() candidate must classify the CANDIDATE, not the empty target`, bundle.Invoke, ir.InvokeSpawn)
	}

	callables := calexec.ResolveSpawnCallables(app)
	if len(callables) != 1 || callables[0].Key != "bundle" {
		t.Fatalf("ResolveSpawnCallables = %+v, want exactly one callable keyed \"bundle\"", callables)
	}
	// calque#191: bundle(job_id, config) takes 2 real args — MethodArgs
	// must carry BOTH names, not just MethodArg's first-arg-only "job_id",
	// or runSpawnShard has nothing to splat against and silently drops
	// "config" on real hardware.
	if want := []string{"job_id", "config"}; len(callables[0].MethodArgs) != len(want) {
		t.Errorf("callables[0].MethodArgs = %v, want %v", callables[0].MethodArgs, want)
	} else {
		for i, a := range want {
			if callables[0].MethodArgs[i] != a {
				t.Errorf("callables[0].MethodArgs[%d] = %q, want %q", i, callables[0].MethodArgs[i], a)
			}
		}
	}

	sites, err := parse.SpawnCallSitesReport(ctx, script, rep, runner, args...)
	if err != nil {
		t.Fatalf("SpawnCallSitesReport: %v", err)
	}
	if len(sites) != 1 || sites[0].Target != "bundle" {
		t.Fatalf("SpawnCallSitesReport = %+v, want exactly one site targeting \"bundle\"", sites)
	}

	callSites := make([]calexec.SpawnCallSite, len(sites))
	for i, s := range sites {
		callSites[i] = calexec.SpawnCallSite{Target: s.Target, Args: s.Args}
	}
	shards := calexec.BuildSpawnManifests(callables, callSites, "s3://bucket/base", "s3://bucket/artifacts")
	if len(shards) != 1 {
		t.Fatalf("BuildSpawnManifests produced %d shard(s), want exactly 1 — this is the real, end-user-visible failure mode #189 exists to prevent: zero shards means `calque spawn-run` silently does nothing for this script. shards=%+v", len(shards), shards)
	}
	if shards[0].Key != "bundle" {
		t.Errorf("shards[0].Key = %q, want %q", shards[0].Key, "bundle")
	}
}

// TestSpawnDictDispatchMultiArgBindingEndToEnd (calque#191) is the
// strongest available proof for the actual arg-binding fix: it drives
// bundle(job_id, config) — the SAME callable/fixture as the test above —
// through the REAL warm supervisor + runner.py subprocess (not just
// inspecting the built ManifestBody's fields) and asserts the ACTUAL
// RETURNED VALUES for BOTH job_id and config are correct. Before #191,
// runSpawnShard's manifest carried only MethodArg="job_id" with no
// Starmap/MethodArgs — config would have compiled into
// __calque_method__'s signature as undefined, a real NameError on real
// hardware. This proves the fix the same way TestStarmapEndToEndDryRun
// proves .starmap()'s own splat: real subprocess, real computed result.
func TestSpawnDictDispatchMultiArgBindingEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping warm-runner subprocess test")
	}
	setPyastDirEnv(t)
	script, err := filepath.Abs("../../testdata/scripts/spawn_dict_dispatch.py")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rep := &leak.Report{}
	runner, args := parse.DefaultRunner(pyastDir())

	app, err := parse.Parse(ctx, script, rep, runner, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	callables := calexec.ResolveSpawnCallables(app)
	if len(callables) != 1 {
		t.Fatalf("callables = %+v, want exactly 1", callables)
	}
	callable := callables[0]

	body := spawnManifestBody(callable, nil, nil, nil, nil)
	if !body.Starmap || len(body.MethodArgs) != 2 {
		t.Fatalf("spawnManifestBody(%+v) = %+v, want Starmap=true with 2 MethodArgs", callable, body)
	}

	runnerPy, err := filepath.Abs("../../worker/warm-runner/runner.py")
	if err != nil {
		t.Fatal(err)
	}
	sink := warm.NewMemSink()
	sup := &warm.Supervisor{
		Python: uvPythonArgv(nil),
		Script: runnerPy,
		Sink:   sink,
		Config: warm.Config{
			EnterBody: body.EnterBody, MethodBody: body.MethodBody,
			MethodArg: body.MethodArg, MethodArgs: body.MethodArgs, Starmap: body.Starmap,
		},
	}
	// One real spawn call site's args: job_id="real-job-42", config="cfg-abc".
	items := []warm.Item{{Index: 0, Payload: []any{"real-job-42", "cfg-abc"}}}
	failed, err := sup.Run(ctx, items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none — a multi-arg spawn callable must not NameError on its own real args", failed)
	}
	results := sink.Results()
	r, ok := results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	got, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %v (%T), want a dict with job_id/config", r.Result, r.Result)
	}
	if got["job_id"] != "real-job-42" {
		t.Errorf(`result["job_id"] = %v, want "real-job-42"`, got["job_id"])
	}
	if got["config"] != "cfg-abc" {
		t.Errorf(`result["config"] = %v, want "cfg-abc" — THIS is the calque#191 regression: before the fix, config was never bound at all`, got["config"])
	}
}

// TestSpawnSiblingHelperEndToEnd (calque#198) is the strongest available
// proof for the sibling-function-resolution fix: it drives
// spawn_sibling_helper.py's run_bundle(job_id, tag) — which delegates to
// a private module-level _bundle_impl(job_id, tag), mirroring AI-
// Almanac's forecasts_app.py's EXACT real shape
// (run_season_forecast_bundle -> _season_bundle_impl) — through the REAL
// pipeline: Parse -> ResolveSpawnCallables -> warmUnitForSpawnCallable ->
// collectLocalExtras -> spawnManifestBody -> the real warm.Supervisor +
// runner.py subprocess. Asserts the ACTUAL RETURNED VALUE, not just that
// the manifest carries an Extras field. Before #198, this failed with a
// real `NameError: name '_bundle_impl' is not defined` — confirmed live
// against forecasts_app.py's own run_season_forecast_bundle/
// _season_bundle_impl pair on real AWS hardware.
func TestSpawnSiblingHelperEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping warm-runner subprocess test")
	}
	setPyastDirEnv(t)
	script, err := filepath.Abs("../../testdata/scripts/spawn_sibling_helper.py")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rep := &leak.Report{}
	runner, args := parse.DefaultRunner(pyastDir())

	app, err := parse.Parse(ctx, script, rep, runner, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	callables := calexec.ResolveSpawnCallables(app)
	if len(callables) != 1 || callables[0].Key != "run_bundle" {
		t.Fatalf("callables = %+v, want exactly one keyed \"run_bundle\"", callables)
	}
	callable := callables[0]

	unit, ok := warmUnitForSpawnCallable(app, callable)
	if !ok {
		t.Fatal("warmUnitForSpawnCallable: ok = false, want true")
	}
	extras, extraConsts, extraImports, extraClasses := collectLocalExtras(app, unit, rep)
	if len(extras) != 1 || extras[0].Name != "_bundle_impl" {
		t.Fatalf("extras = %+v, want exactly one named \"_bundle_impl\"", extras)
	}

	body := spawnManifestBody(callable, extras, extraConsts, extraImports, extraClasses)

	runnerPy, err := filepath.Abs("../../worker/warm-runner/runner.py")
	if err != nil {
		t.Fatal(err)
	}
	sink := warm.NewMemSink()
	sup := &warm.Supervisor{
		Python: uvPythonArgv(nil),
		Script: runnerPy,
		Sink:   sink,
		Config: warm.Config{
			EnterBody: body.EnterBody, MethodBody: body.MethodBody, MethodArg: body.MethodArg,
			MethodArgs: body.MethodArgs, Starmap: body.Starmap, Extras: body.Extras,
		},
	}
	items := []warm.Item{{Index: 0, Payload: []any{"real-job-99", "season-2025"}}}
	failed, err := sup.Run(ctx, items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none — run_bundle delegates to _bundle_impl, which must now be shipped and resolvable", failed)
	}
	results := sink.Results()
	r, ok := results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	got, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %v (%T), want a dict with job_id/tag", r.Result, r.Result)
	}
	if got["job_id"] != "real-job-99" {
		t.Errorf(`result["job_id"] = %v, want "real-job-99"`, got["job_id"])
	}
	if got["tag"] != "season-2025" {
		t.Errorf(`result["tag"] = %v, want "season-2025"`, got["tag"])
	}
}
