package main

import (
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/gate"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// TestPrintOffersAndStop locks the G3 short-circuit contract: an exact offer means
// "stop before acquiring a GPU"; no offer means "proceed to the GPU path".
func TestPrintOffersAndStop(t *testing.T) {
	if printOffersAndStop(nil) {
		t.Error("no offers must NOT stop the run (proceed to GPU path)")
	}
	exact := []*gate.ReplacementOffer{{
		ModelRef: "meta-llama/Meta-Llama-3-8B-Instruct", BedrockID: "meta.llama3-8b-instruct-v1:0",
		Exact: true, Regions: []string{"us-east-1"},
	}}
	if !printOffersAndStop(exact) {
		t.Error("an exact offer MUST stop the run before acquisition")
	}
}

// TestServeAppDetection (F2): serveApp identifies a serve-shaped app so run() leaks
// the deferred shape instead of hard-erroring.
func TestServeAppDetection(t *testing.T) {
	batch := irApp(ir.Function{Name: "gen", EntryKind: ir.EntryBatch})
	if serveApp(batch) {
		t.Error("a batch app must not be detected as serve")
	}
	serve := irApp(ir.Function{Name: "endpoint", EntryKind: ir.EntryServe})
	if !serveApp(serve) {
		t.Error("an app with a serve entry function must be detected as serve")
	}
}

func irApp(fns ...ir.Function) ir.App { return ir.App{Functions: fns} }

// TestPickWarmUnitPlainFunction (calque#79/#80): a script with no @cls at all
// must still yield a runnable warm unit from a plain @app.function, not refuse.
func TestPickWarmUnitPlainFunction(t *testing.T) {
	app := irApp(
		ir.Function{Name: "greet", Body: "return 1"},
		ir.Function{Name: "transform", Body: "return 2", IsMap: true},
	)
	unit, ok := pickWarmUnit(app, "")
	if !ok {
		t.Fatal("pickWarmUnit must select a plain function when no @cls exists")
	}
	if !unit.plainFunction {
		t.Error("plainFunction must be true for a function-only app")
	}
	// A .map'd function is preferred over a non-mapped one, mirroring the
	// class-based path's preference for a .map'd method.
	if unit.method.Name != "transform" {
		t.Errorf("selected %q, want the .map'd function %q", unit.method.Name, "transform")
	}
	if unit.class.EnterBody != "" {
		t.Error("synthesized class for a plain function must have an empty EnterBody")
	}
}

// TestPickWarmUnitPlainFunctionFallback: with no .map'd function at all, the
// first function is selected (single-call replay per §G), not a refusal.
func TestPickWarmUnitPlainFunctionFallback(t *testing.T) {
	app := irApp(ir.Function{Name: "greet", Body: "return 1"})
	unit, ok := pickWarmUnit(app, "")
	if !ok {
		t.Fatal("pickWarmUnit must fall back to the first plain function")
	}
	if unit.method.Name != "greet" || !unit.plainFunction {
		t.Errorf("unit = %+v, want the sole plain function selected", unit)
	}
}

// TestPickWarmUnitPrefersClassOverPlainFunction: a @cls+@enter unit must still
// win when both a class and plain functions exist — the class path is unchanged.
func TestPickWarmUnitPrefersClassOverPlainFunction(t *testing.T) {
	app := ir.App{
		Functions: []ir.Function{{Name: "greet", Body: "return 1"}},
		Classes: []ir.Class{{
			Name:      "Batcher",
			EnterBody: "self.model = load()",
			Methods:   []ir.Function{{Name: "generate", IsMap: true}},
		}},
	}
	unit, ok := pickWarmUnit(app, "")
	if !ok {
		t.Fatal("pickWarmUnit failed")
	}
	if unit.plainFunction {
		t.Error("a @cls+@enter unit must be preferred over any plain function")
	}
	if unit.class.Name != "Batcher" || unit.method.Name != "generate" {
		t.Errorf("unit = %+v, want the Batcher class's generate method", unit)
	}
}

// entrypointScopedFixtureApp mirrors testdata/scripts/entrypoint_scoped_invoke.py
// (calque#98's issue repro): a @cls+@enter Trainer.train_step that's .map()'d
// ONLY inside do_train's body, and a wholly unrelated plain function evaluate
// that's .remote()'d ONLY inside do_evaluate's body. Before calque#98,
// pickWarmUnit had no way to see this distinction and always picked
// train_step (the script's only .map()'d callable) regardless of which
// entrypoint was selected.
func entrypointScopedFixtureApp() ir.App {
	return ir.App{
		Classes: []ir.Class{{
			Name:      "Trainer",
			EnterBody: "self.model = \"loaded\"",
			Methods:   []ir.Function{{Name: "train_step"}}, // IsMap left false: only true for do_train's own scan
		}},
		Functions:   []ir.Function{{Name: "evaluate"}},
		Entrypoints: []ir.Function{{Name: "do_train"}, {Name: "do_evaluate"}},
		EntrypointInvokes: map[string]map[string]ir.InvokeKind{
			"do_train":    {"train_step": ir.InvokeMap},
			"do_evaluate": {"evaluate": ir.InvokeRemote},
		},
	}
}

// TestPickWarmUnitScopedToSelectedEntrypoint (calque#98): with --entrypoint
// do_evaluate resolved, pickWarmUnit must select the evaluate-related warm
// unit, NOT train_step — the exact repro from the issue. --entrypoint
// do_train must still select train_step, proving both directions are scoped
// correctly (neither entrypoint's selection leaks into the other's).
func TestPickWarmUnitScopedToSelectedEntrypoint(t *testing.T) {
	app := entrypointScopedFixtureApp()

	unit, ok := pickWarmUnit(app, "do_evaluate")
	if !ok {
		t.Fatal("pickWarmUnit(do_evaluate) failed")
	}
	if !unit.plainFunction || unit.method.Name != "evaluate" {
		t.Errorf("pickWarmUnit(do_evaluate) = %+v, want the plain function evaluate, NOT train_step", unit)
	}

	unit, ok = pickWarmUnit(app, "do_train")
	if !ok {
		t.Fatal("pickWarmUnit(do_train) failed")
	}
	if unit.plainFunction || unit.class.Name != "Trainer" || unit.method.Name != "train_step" {
		t.Errorf("pickWarmUnit(do_train) = %+v, want Trainer.train_step", unit)
	}
}

// TestPickWarmUnitUnscopedWhenNotAmbiguous (calque#98 regression guard): a
// script with 0 or 1 entrypoints must fall back to the original whole-script
// scan UNCHANGED — entrypointScoped only kicks in at 2+ entrypoints. Reusing
// entrypointScopedFixtureApp's shape but trimmed to a single entrypoint
// proves EntrypointInvokes (even if populated) is ignored in that case: the
// whole-script IsMap on train_step is what decides the outcome here, not the
// per-entrypoint map.
func TestPickWarmUnitUnscopedWhenNotAmbiguous(t *testing.T) {
	app := entrypointScopedFixtureApp()
	app.Entrypoints = []ir.Function{{Name: "do_train"}} // now unambiguous: only 1
	app.Classes[0].Methods[0].IsMap = true              // whole-script view says train_step IS mapped

	unit, ok := pickWarmUnit(app, "do_train")
	if !ok {
		t.Fatal("pickWarmUnit(do_train, 1 entrypoint) failed")
	}
	// Unscoped: the @cls+@enter unit wins outright per the original
	// class-before-function precedence, regardless of EntrypointInvokes.
	if unit.plainFunction || unit.class.Name != "Trainer" || unit.method.Name != "train_step" {
		t.Errorf("pickWarmUnit(1 entrypoint) = %+v, want the unscoped whole-script pick (Trainer.train_step)", unit)
	}
}

// TestCheckInvokeSupportStarmapRefusesWithoutRealItems (calque#83, narrowed by
// calque#93): a .starmap'd unit with NO statically-resolvable iterable
// (fn.Items nil) has no real tuple data to splat — falling back to a
// synthesized single-value placeholder would crash just as badly as never
// splatting did, so this must still refuse.
func TestCheckInvokeSupportStarmapRefusesWithoutRealItems(t *testing.T) {
	rep := &leak.Report{}
	fn := ir.Function{Name: "combine", Invoke: ir.InvokeStarmap}
	if err := checkInvokeSupport("script.py", fn, rep, false); err == nil {
		t.Fatal("expected an error refusing a .starmap'd warm unit with no real Items, got nil")
	}
}

// TestCheckInvokeSupportStarmapRunsWithRealItems (calque#93): a .starmap'd
// unit WITH real tuple data extracted at parse time (fn.Items non-empty) must
// no longer refuse — the runner can now splat real tuples. This is a leak
// (informational), not an error.
func TestCheckInvokeSupportStarmapRunsWithRealItems(t *testing.T) {
	rep := &leak.Report{}
	fn := ir.Function{
		Name: "combine", Invoke: ir.InvokeStarmap, Line: 12,
		Items: []any{[]any{float64(1), float64(2)}, []any{float64(3), float64(4)}},
	}
	if err := checkInvokeSupport("script.py", fn, rep, false); err != nil {
		t.Fatalf("checkInvokeSupport(.starmap with real Items) must not error, got: %v", err)
	}
	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "combine") && strings.Contains(l.Detail, "starmap") {
			found = true
		}
	}
	if !found {
		t.Errorf(".starmap-with-real-data should still be noted in the leak report; leaks=%+v", rep.Leaks)
	}
}

// TestCheckInvokeSupportForEachLeaksNotRefuses (calque#83): .for_each shares
// .map's single-arg signature and runs correctly — the mismatch (Modal
// discards the result, calque collects it) is a leak, not a crash, so this
// must NOT error.
func TestCheckInvokeSupportForEachLeaksNotRefuses(t *testing.T) {
	rep := &leak.Report{}
	fn := ir.Function{Name: "notify", Invoke: ir.InvokeForEach, Line: 7}
	if err := checkInvokeSupport("script.py", fn, rep, false); err != nil {
		t.Fatalf("checkInvokeSupport(.for_each) must not error, got: %v", err)
	}
	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "notify") && strings.Contains(l.Detail, ".for_each()") {
			found = true
		}
	}
	if !found {
		t.Errorf(".for_each mismatch should be leaked; leaks=%+v", rep.Leaks)
	}
}

// TestCheckInvokeSupportMapAndRemoteAreFine: .map and .remote share the warm
// runner's single-arg, collect-a-result shape exactly — no error, no leak.
func TestCheckInvokeSupportMapAndRemoteAreFine(t *testing.T) {
	rep := &leak.Report{}
	for _, kind := range []ir.InvokeKind{ir.InvokeMap, ir.InvokeRemote, ir.InvokeNone} {
		fn := ir.Function{Name: "f", Invoke: kind}
		if err := checkInvokeSupport("script.py", fn, rep, false); err != nil {
			t.Errorf("checkInvokeSupport(%q) must not error, got: %v", kind, err)
		}
	}
	if len(rep.Leaks) != 0 {
		t.Errorf("map/remote/none must not leak; leaks=%+v", rep.Leaks)
	}
}

// TestCheckInvokeSupportMultiArgNonStarmapRefuses (calque#187): a picked
// unit with 2+ non-self/cls positional args that ISN'T .starmap()'d (e.g. a
// .spawn()-invoked function like AI-Almanac's forecasts_app.py's
// `run_forecast_inference(job_id, model_id, config)`) must refuse loudly
// instead of silently NameError'ing — the warm runner only ever binds the
// FIRST positional arg per item outside the .starmap() splat path (both
// dryRunWarm and manifestBodyForUnit), so the rest would be undefined.
// hasRealArgTuple=false: no --arg-file/--arg-json supplied a real tuple.
func TestCheckInvokeSupportMultiArgNonStarmapRefuses(t *testing.T) {
	rep := &leak.Report{}
	fn := ir.Function{Name: "run_forecast_inference", Invoke: ir.InvokeSpawn, Args: []string{"job_id", "model_id", "config"}}
	err := checkInvokeSupport("script.py", fn, rep, false)
	if err == nil {
		t.Fatal("expected an error refusing a multi-arg non-starmap unit, got nil")
	}
	for _, want := range []string{"run_forecast_inference", "job_id", "model_id", "config", "3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing expected substring %q", err.Error(), want)
		}
	}
}

// TestCheckInvokeSupportMultiArgWithRealArgTupleFine (calque#187) is the
// regression guard for the exact case that made a blanket arity refusal
// wrong: AI-Almanac's app.py's run_benchmark_local(job_id, config, bundle,
// runtime_env) takes 4 positional args, is invoked via .remote() (not
// .starmap()), yet calque#178 verified it running for real on real
// hardware — because `calque real --arg-file`/`--arg-json` supplies a REAL
// per-position tuple, bypassing the single-item-arg synthetic path
// entirely. hasRealArgTuple=true must never refuse, regardless of arity.
func TestCheckInvokeSupportMultiArgWithRealArgTupleFine(t *testing.T) {
	rep := &leak.Report{}
	fn := ir.Function{Name: "run_benchmark_local", Invoke: ir.InvokeRemote, Args: []string{"job_id", "config", "bundle", "runtime_env"}}
	if err := checkInvokeSupport("script.py", fn, rep, true); err != nil {
		t.Fatalf("checkInvokeSupport(hasRealArgTuple=true) must not refuse a multi-arg unit, got: %v", err)
	}
}

// TestCheckInvokeSupportSingleArgNonStarmapFine proves the arity guard only
// fires on 2+ args — the overwhelmingly common single-arg case (every
// existing .map()/.remote()/plain-function corpus script) must stay
// unaffected, byte-for-byte unchanged from before calque#187.
func TestCheckInvokeSupportSingleArgNonStarmapFine(t *testing.T) {
	rep := &leak.Report{}
	for _, kind := range []ir.InvokeKind{ir.InvokeMap, ir.InvokeRemote, ir.InvokeNone, ir.InvokeSpawn} {
		fn := ir.Function{Name: "f", Invoke: kind, Args: []string{"self", "item"}}
		if err := checkInvokeSupport("script.py", fn, rep, false); err != nil {
			t.Errorf("checkInvokeSupport(%q, 1 non-self arg) must not error, got: %v", kind, err)
		}
	}
}

// TestResolveEntrypointNoneDefined: a script with no @app.local_entrypoint()
// has nothing to select — not an error, unless one was explicitly requested.
func TestResolveEntrypointNoneDefined(t *testing.T) {
	app := ir.App{Script: "s.py"}
	name, err := resolveEntrypoint(app, "")
	if err != nil || name != "" {
		t.Errorf("resolveEntrypoint(no entrypoints, \"\") = (%q, %v), want (\"\", nil)", name, err)
	}
	if _, err := resolveEntrypoint(app, "main"); err == nil {
		t.Error("resolveEntrypoint(no entrypoints, \"main\") should error — nothing to select")
	}
}

// TestResolveEntrypointSingleAutoSelects: exactly one entrypoint is selected
// automatically without --entrypoint (calque#90).
func TestResolveEntrypointSingleAutoSelects(t *testing.T) {
	app := ir.App{Script: "s.py", Entrypoints: []ir.Function{{Name: "main"}}}
	name, err := resolveEntrypoint(app, "")
	if err != nil || name != "main" {
		t.Errorf("resolveEntrypoint(1 entrypoint, \"\") = (%q, %v), want (\"main\", nil)", name, err)
	}
}

// TestResolveEntrypointAmbiguousRequiresSelection (calque#90): 2+ entrypoints
// with no --entrypoint must error, mirroring Modal's own "ambiguous, pick one"
// posture — never silently run whichever pickWarmUnit happens to find.
func TestResolveEntrypointAmbiguousRequiresSelection(t *testing.T) {
	app := ir.App{Script: "s.py", Entrypoints: []ir.Function{{Name: "do_train"}, {Name: "do_evaluate"}}}
	if _, err := resolveEntrypoint(app, ""); err == nil {
		t.Error("resolveEntrypoint(2 entrypoints, \"\") should error — ambiguous")
	}
	name, err := resolveEntrypoint(app, "do_evaluate")
	if err != nil || name != "do_evaluate" {
		t.Errorf("resolveEntrypoint(2 entrypoints, %q) = (%q, %v), want (%q, nil)", "do_evaluate", name, err, "do_evaluate")
	}
	if _, err := resolveEntrypoint(app, "nonexistent"); err == nil {
		t.Error("resolveEntrypoint(unknown name) should error")
	}
}
