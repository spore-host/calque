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

// TestFreeRefsImportEndToEndDryRun (calque#146) is TestFreeRefsEndToEndDryRun's
// import counterpart: parse the real free_refs_import.py fixture, resolve
// Worker's picked warm unit, confirm collectLocalExtras ships the
// bare-referenced module-level import (`Path`, from `from pathlib import
// Path`) — then actually run it through the REAL warm supervisor +
// runner.py, asserting the per-item RESULT reflects it. Before this fix,
// this exact shape NameError'd unconditionally ("name 'Path' is not
// defined") — the same crash hit on all 3 AI-Almanac scripts.
func TestFreeRefsImportEndToEndDryRun(t *testing.T) {
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
	script, err := filepath.Abs("../../testdata/scripts/free_refs_import.py")
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

	extras, consts, imports, _ := collectLocalExtras(app, unit, rep)
	if len(extras) != 0 || len(consts) != 0 {
		t.Fatalf("extras=%+v consts=%+v, want none", extras, consts)
	}
	if len(imports) != 1 || imports[0].Name != "Path" {
		t.Fatalf("imports = %+v, want exactly [Path] (Worker.load's bare `Path(...)` reference)", imports)
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
			MethodArg: unit.method.ItemArg, ExtraImports: imports,
		},
	}
	items := []warm.Item{{Index: 0, Payload: "world"}}
	failed, err := sup.Run(context.Background(), items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none — this is the exact NameError shape calque#146 fixes", failed)
	}

	results := sink.Results()
	r, ok := results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	want := "/tmp/world" // str(Path("/tmp") / "world")
	if got, ok := r.Result.(string); !ok || got != want {
		t.Errorf("result = %v, want %q (proves the bare `Path` import reference resolved for real)", r.Result, want)
	}
}
