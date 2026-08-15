package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// TestFreeRefsClassEndToEndDryRun (calque#147) is TestFreeRefsImportEndToEndDryRun's
// class counterpart: parse the real free_refs_class.py fixture, resolve
// Worker's picked warm unit, confirm collectLocalExtras ships the
// bare-instantiated PLAIN module-level class (`_Adder`) — then actually run
// it through the REAL warm supervisor + runner.py, asserting the per-item
// RESULT reflects it. Before this fix, this exact shape NameError'd
// unconditionally ("name '_Adder' is not defined") — the same crash hit on
// AI-Almanac's app.py (`_LogTee`).
func TestFreeRefsClassEndToEndDryRun(t *testing.T) {
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
	script, err := filepath.Abs("../../testdata/scripts/free_refs_class.py")
	if err != nil {
		t.Fatal(err)
	}
	rep := &leak.Report{}
	app, err := parse.Parse(context.Background(), script, rep, runner, runnerArgs...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	unit, ok := pickWarmUnit(app, "")
	if !ok {
		t.Fatal("pickWarmUnit failed")
	}
	if unit.method.Name != "run" {
		t.Fatalf("selected unit = %+v, want the .map()'d run", unit.method)
	}

	extras, consts, imports, classes := collectLocalExtras(app, unit, rep)
	if len(extras) != 0 || len(consts) != 0 || len(imports) != 0 {
		t.Fatalf("extras=%+v consts=%+v imports=%+v, want none", extras, consts, imports)
	}
	if len(classes) != 1 || classes[0].Name != "_Adder" {
		t.Fatalf("classes = %+v, want exactly [_Adder] (Worker.load's bare `_Adder(10)` instantiation)", classes)
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
			EnterBody: unit.class.EnterBody, MethodBody: unit.method.Body,
			MethodArg: unit.method.ItemArg, ExtraClasses: classes,
		},
	}
	items := []warm.Item{{Index: 0, Payload: 5}}
	failed, err := sup.Run(context.Background(), items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none — this is the exact NameError shape calque#147 fixes", failed)
	}

	results := sink.Results()
	r, ok := results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	want := 15.0 // _Adder(10).add(5) == 15, JSON-decoded as float64
	if got, ok := r.Result.(float64); !ok || got != want {
		t.Errorf("result = %v, want %v (proves the bare `_Adder` class instantiation resolved for real)", r.Result, want)
	}
}
