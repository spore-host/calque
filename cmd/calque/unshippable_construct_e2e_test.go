package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// TestUnshippableDictRefEndToEndDryRun (calque#151) is the full pipeline
// proof: parse the real unshippable_dict_ref.py fixture, resolve Worker's
// picked warm unit (use_dict, .map()'d), confirm collectLocalExtras refuses
// to ship data_dict (a bare reference to a module-level
// modal.Dict.from_name(...) constant) and emits an honest leak instead —
// then actually run it through the REAL warm supervisor + runner.py,
// asserting the item fails with a plain NameError (data_dict was never
// defined), not the confusing Modal SDK auth crash this shipped-verbatim
// before the fix.
func TestUnshippableDictRefEndToEndDryRun(t *testing.T) {
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
	script, err := filepath.Abs("../../testdata/scripts/unshippable_dict_ref.py")
	if err != nil {
		t.Fatal(err)
	}
	rep := &leak.Report{}
	app, err := parse.Parse(context.Background(), script, rep, runner, runnerArgs...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	mc, ok := app.ModuleConsts["data_dict"]
	if !ok || mc.UnshippableConstruct != "modal.Dict" {
		t.Fatalf("app.ModuleConsts[data_dict] = %+v, ok=%v, want UnshippableConstruct=%q", mc, ok, "modal.Dict")
	}

	unit, ok := pickWarmUnit(app, "")
	if !ok {
		t.Fatal("pickWarmUnit failed")
	}
	if unit.method.Name != "use_dict" {
		t.Fatalf("selected unit = %+v, want the .map()'d use_dict", unit.method)
	}

	extras, consts, imports, classes := collectLocalExtras(app, unit, rep)
	if len(extras) != 0 || len(consts) != 0 || len(imports) != 0 || len(classes) != 0 {
		t.Fatalf("collectLocalExtras shipped extras=%+v consts=%+v imports=%+v classes=%+v, want none (data_dict must be refused, not shipped)", extras, consts, imports, classes)
	}
	foundLeak := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "data_dict") && strings.Contains(l.Detail, "modal.Dict") && strings.Contains(l.Detail, "not shipped") {
			foundLeak = true
		}
	}
	if !foundLeak {
		t.Errorf("expected an honest 'not shipped' leak naming data_dict and modal.Dict; leaks=%+v", rep.Leaks)
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
	items := []warm.Item{{Index: 0, Payload: "a"}}
	failed, err := sup.Run(context.Background(), items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed = %v, want exactly [0] — data_dict was never shipped, so use_dict's body must NameError", failed)
	}
}
