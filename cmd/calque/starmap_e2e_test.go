package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// TestStarmapEndToEndDryRun (calque#93) is the full pipeline proof: parse the
// real map_iterables.py fixture, select run_lit_starmap's warm unit
// (lit_starmap, a .starmap()'d plain function taking two params: a, b),
// confirm checkInvokeSupport no longer refuses it now that real tuple data
// ([[1,2],[3,4]]) was extracted at parse time (calque#136), and drive it
// through the REAL warm supervisor + runner.py exactly as dryRunWarm does —
// asserting the actual per-item SUM comes out correct, proving the tuple
// splat happened for real (not just "didn't crash").
func TestStarmapEndToEndDryRun(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping warm-runner subprocess test")
	}

	dir, err := filepath.Abs("../../tools/pyast")
	if err != nil {
		t.Fatal(err)
	}
	runner, runnerArgs := parse.DefaultRunner(dir)
	script, err := filepath.Abs("../../testdata/scripts/map_iterables.py")
	if err != nil {
		t.Fatal(err)
	}
	rep := &leak.Report{}
	app, err := parse.Parse(context.Background(), script, rep, runner, runnerArgs...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	unit, ok := pickWarmUnit(app, "run_lit_starmap")
	if !ok {
		t.Fatal("pickWarmUnit(run_lit_starmap) failed")
	}
	if unit.method.Name != "lit_starmap" || unit.method.Invoke != ir.InvokeStarmap {
		t.Fatalf("selected unit = %+v, want the .starmap()'d lit_starmap", unit.method)
	}
	if len(unit.method.Items) == 0 {
		t.Fatalf("lit_starmap.Items is empty — calque#136 real-tuple extraction regressed, this test can't prove the splat")
	}

	// checkInvokeSupport must NOT refuse now that real tuple data exists.
	if err := checkInvokeSupport(app.Script, unit.method, rep); err != nil {
		t.Fatalf("checkInvokeSupport must not refuse a .starmap unit with real Items, got: %v", err)
	}

	methodArgs := nonSelfArgs(unit.method.Args)
	if len(methodArgs) != 2 {
		t.Fatalf("nonSelfArgs(%v) = %v, want 2 params (a, b)", unit.method.Args, methodArgs)
	}

	runnerPy, err := filepath.Abs("../../worker/warm-runner/runner.py")
	if err != nil {
		t.Fatal(err)
	}
	sink := warm.NewMemSink()
	sup := &warm.Supervisor{
		Python: pythonBin(),
		Script: runnerPy,
		Sink:   sink,
		Config: warm.Config{
			EnterBody: unit.class.EnterBody, MethodBody: unit.method.Body,
			MethodArg: unit.method.ItemArg, MethodArgs: methodArgs, Starmap: true,
		},
	}
	items := realOrSyntheticItems(unit, 2, starmapAwareSynth(true, methodArgs), rep)
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}

	failed, err := sup.Run(context.Background(), items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}

	results := sink.Results()
	wantSums := map[int]float64{0: 3, 1: 7} // (1,2)->3, (3,4)->7
	for idx, want := range wantSums {
		r, ok := results[idx]
		if !ok {
			t.Fatalf("missing result for index %d", idx)
		}
		got, ok := r.Result.(float64)
		if !ok || got != want {
			t.Errorf("index %d: sum=%v, want %v (real tuple-splat binding produced the wrong value)", idx, r.Result, want)
		}
	}
}
