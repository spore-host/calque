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

// TestFreeRefsEndToEndDryRun (calque#139) is the full pipeline proof for the
// issue's exact repro shapes: parse the real free_refs.py fixture, resolve
// Greeter's picked warm unit (greet, .map()'d), confirm collectLocalExtras
// ships BOTH the plain module-level helper (_format, never an @app.function,
// referenced with no .local() at all) and the module-level constant
// (GREETING, read bare inside @enter) — then actually run it through the
// REAL warm supervisor + runner.py, asserting the per-item RESULT reflects
// both: before this fix, this exact shape NameError'd on _format (or, if
// _format alone had somehow resolved, would still NameError on GREETING
// inside @enter, since a plain call-site scan not extended to bare names/
// constants never sees either).
func TestFreeRefsEndToEndDryRun(t *testing.T) {
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
	script, err := filepath.Abs("../../testdata/scripts/free_refs.py")
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
	if unit.method.Name != "greet" {
		t.Fatalf("selected unit = %+v, want the .map()'d greet", unit.method)
	}

	extras, consts, _, _ := collectLocalExtras(app, unit, rep)
	if len(extras) != 1 || extras[0].Name != "_format" {
		t.Fatalf("extras = %+v, want exactly [_format] (the plain, undecorated helper greet() bare-calls)", extras)
	}
	if len(consts) != 1 || consts[0].Name != "GREETING" {
		t.Fatalf("consts = %+v, want exactly [GREETING] (@enter's bare constant read)", consts)
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
			MethodArg: unit.method.ItemArg, Extras: extras, ExtraConsts: consts,
		},
	}
	items := []warm.Item{{Index: 0, Payload: "world"}}
	failed, err := sup.Run(context.Background(), items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none — this is the exact NameError shape calque#139 fixes", failed)
	}

	results := sink.Results()
	r, ok := results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	want := "hello, world!" // _format(name) = f"{GREETING}, {name}!"
	if got, ok := r.Result.(string); !ok || got != want {
		t.Errorf("result = %v, want %q (proves BOTH the bare helper call and the bare constant read resolved for real)", r.Result, want)
	}
}
