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
	unit, ok := pickWarmUnit(app)
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
	unit, ok := pickWarmUnit(app)
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
	unit, ok := pickWarmUnit(app)
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

// TestCheckInvokeSupportStarmapRefuses (calque#83): the warm runner only binds
// one positional arg per item — a .starmap'd unit (tuple-splat) would crash
// every item if run silently as .map. Must refuse with a clear reason.
func TestCheckInvokeSupportStarmapRefuses(t *testing.T) {
	rep := &leak.Report{}
	fn := ir.Function{Name: "combine", Invoke: ir.InvokeStarmap}
	if err := checkInvokeSupport("script.py", fn, rep); err == nil {
		t.Fatal("expected an error refusing a .starmap'd warm unit, got nil")
	}
}

// TestCheckInvokeSupportForEachLeaksNotRefuses (calque#83): .for_each shares
// .map's single-arg signature and runs correctly — the mismatch (Modal
// discards the result, calque collects it) is a leak, not a crash, so this
// must NOT error.
func TestCheckInvokeSupportForEachLeaksNotRefuses(t *testing.T) {
	rep := &leak.Report{}
	fn := ir.Function{Name: "notify", Invoke: ir.InvokeForEach, Line: 7}
	if err := checkInvokeSupport("script.py", fn, rep); err != nil {
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
		if err := checkInvokeSupport("script.py", fn, rep); err != nil {
			t.Errorf("checkInvokeSupport(%q) must not error, got: %v", kind, err)
		}
	}
	if len(rep.Leaks) != 0 {
		t.Errorf("map/remote/none must not leak; leaks=%+v", rep.Leaks)
	}
}
