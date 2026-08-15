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

// TestPlainFunctionManifestBodyEndToEnd (calque#79 Part 1) is the full
// pipeline proof for the gap #79 actually closes: parse the real
// plain_function.py fixture (a plain @app.function, no @cls — AI-Almanac's
// exact shape), select its warm unit, build the real-AWS ManifestBody via
// manifestBodyForUnit, and drive it through the REAL warm supervisor +
// runner.py — asserting the ACTUAL COMPUTED RESULT (x*2) reflects
// plain_function.py's own transform(x), not the hardcoded vLLM
// realEnterBody/realMethodBody constants calque real/fleetrun used to
// drive regardless of what --script parsed.
func TestPlainFunctionManifestBodyEndToEnd(t *testing.T) {
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
	script, err := filepath.Abs("../../testdata/scripts/plain_function.py")
	if err != nil {
		t.Fatal(err)
	}
	rep := &leak.Report{}
	app, err := parse.Parse(context.Background(), script, rep, runner, runnerArgs...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	unit, ok := pickWarmUnit(app, "main")
	if !ok {
		t.Fatal("pickWarmUnit(main) failed")
	}
	if unit.method.Name != "transform" || !unit.plainFunction {
		t.Fatalf("selected unit = %+v, want the plain-function transform", unit.method)
	}

	body, ok := manifestBodyForUnit(app, unit, rep)
	if !ok {
		t.Fatal("manifestBodyForUnit() ok = false, want true for a real parsed unit")
	}
	if body.MethodBody != "return x * 2" {
		t.Fatalf("MethodBody = %q, want plain_function.py's real transform body, not a vLLM constant", body.MethodBody)
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
			EnterBody: body.EnterBody, MethodBody: body.MethodBody, MethodArg: body.MethodArg,
			MethodArgs: body.MethodArgs, Starmap: body.Starmap, Extras: body.Extras, ExtraConsts: body.ExtraConsts,
		},
	}
	items := []warm.Item{{Index: 0, Payload: 21.0}}
	failed, err := sup.Run(context.Background(), items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	results := sink.Results()
	r, ok := results[0]
	if !ok {
		t.Fatal("missing result for index 0")
	}
	got, ok := r.Result.(float64)
	if !ok || got != 42.0 {
		t.Errorf("result = %v, want 42 (21*2 — proves manifestBodyForUnit shipped the script's REAL transform(x), not a hardcoded stand-in)", r.Result)
	}
}
