package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// TestCollectLocalExtrasTransitiveClosure (calque#92): a picked unit's
// LocalCalls reaches a sibling two hops deep (a -> b -> c); all reachable
// plain functions must be collected, in first-seen order, deduped.
func TestCollectLocalExtrasTransitiveClosure(t *testing.T) {
	app := ir.App{Functions: []ir.Function{
		{Name: "run_blend", Body: "a = stage_one.local(x)", LocalCalls: []string{"stage_one"}},
		{Name: "stage_one", Args: []string{"y"}, Body: "return stage_two(y)", LocalCalls: []string{"stage_two"}},
		{Name: "stage_two", Args: []string{"z"}, Body: "return z + 1"},
	}}
	unit := warmUnit{method: app.Functions[0], class: syntheticClass(app.Functions[0])}
	rep := &leak.Report{}

	extras, consts, imports, _ := collectLocalExtras(app, unit, rep)
	if len(consts) != 0 {
		t.Errorf("consts = %+v, want none", consts)
	}
	if len(imports) != 0 {
		t.Errorf("imports = %+v, want none", imports)
	}
	if len(extras) != 2 {
		t.Fatalf("collectLocalExtras returned %d extras, want 2; got %+v", len(extras), extras)
	}
	if extras[0].Name != "stage_one" || extras[1].Name != "stage_two" {
		t.Errorf("extras = %+v, want [stage_one, stage_two] in first-seen order", extras)
	}
	if extras[1].Args[0] != "z" {
		t.Errorf("stage_two extra Args = %v, want [z]", extras[1].Args)
	}
}

// TestCollectLocalExtrasSkipsClassMethodAndLeaksHonestly (calque#92): a
// .local() target that resolves to a @cls method must NOT be shipped (no
// warm @enter state outside the picked unit) — it must surface as an
// honest leak instead of silently vanishing or crashing the collector.
func TestCollectLocalExtrasSkipsClassMethodAndLeaksHonestly(t *testing.T) {
	app := ir.App{
		Functions: []ir.Function{
			{Name: "run_blend", Body: "b = Batcher().score.local(a)", LocalCalls: []string{"score"}},
		},
		Classes: []ir.Class{{
			Name:    "Batcher",
			Methods: []ir.Function{{Name: "score", Body: "return x"}},
		}},
	}
	unit := warmUnit{method: app.Functions[0], class: syntheticClass(app.Functions[0])}
	rep := &leak.Report{}

	extras, consts, imports, _ := collectLocalExtras(app, unit, rep)
	if len(extras) != 0 || len(consts) != 0 || len(imports) != 0 {
		t.Errorf("collectLocalExtras shipped extras=%+v consts=%+v imports=%+v, want none (score is a @cls method)", extras, consts, imports)
	}
	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "score") && strings.Contains(l.Detail, "not shipped") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an honest 'not shipped' leak for the @cls method target; leaks=%+v", rep.Leaks)
	}
}

// TestCollectLocalExtrasRefusesUnshippableConstructAndLeaksHonestly
// (calque#151): a bare reference resolving to a module-level constant whose
// RHS is a live-Modal-control-plane construct (modal.Dict/Queue/
// NetworkFileSystem.from_name(...)) must NOT be shipped as a warm.ExtraConst
// — the runner has no live Modal credentials, so exec'ing it verbatim would
// crash with a confusing SDK auth error instead of an honest leak. Found via
// calque#150's torture-test pass (RomeroLab/alphafast's InferenceWorker,
// which bare-references a module-level modal.Dict.from_name(...) constant).
func TestCollectLocalExtrasRefusesUnshippableConstructAndLeaksHonestly(t *testing.T) {
	app := ir.App{
		Functions: []ir.Function{
			{Name: "use_dict", Body: "return data_dict.get(key)", FreeRefs: []string{"data_dict"}},
		},
		ModuleConsts: map[string]ir.ModuleConst{
			"data_dict": {
				Source:               `data_dict = modal.Dict.from_name("d", create_if_missing=True)`,
				FreeRefs:             []string{"modal"},
				UnshippableConstruct: "modal.Dict",
			},
		},
	}
	unit := warmUnit{method: app.Functions[0], class: syntheticClass(app.Functions[0])}
	rep := &leak.Report{}

	extras, consts, imports, classes := collectLocalExtras(app, unit, rep)
	if len(extras) != 0 || len(consts) != 0 || len(imports) != 0 || len(classes) != 0 {
		t.Errorf("collectLocalExtras shipped extras=%+v consts=%+v imports=%+v classes=%+v, want none (data_dict is an unshippable modal.Dict)", extras, consts, imports, classes)
	}
	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "data_dict") && strings.Contains(l.Detail, "modal.Dict") && strings.Contains(l.Detail, "not shipped") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an honest 'not shipped' leak naming data_dict and modal.Dict; leaks=%+v", rep.Leaks)
	}
}

// TestCollectLocalExtrasSelfReferenceTerminates (calque#92): a function whose
// body .local()-calls itself must not infinite-loop the collector — visited
// is checked before enqueueing, so a self-reference is a no-op re-visit.
func TestCollectLocalExtrasSelfReferenceTerminates(t *testing.T) {
	app := ir.App{Functions: []ir.Function{
		{Name: "caller", Body: "recur.local(x)", LocalCalls: []string{"recur"}},
		{Name: "recur", Args: []string{"x"}, Body: "return recur.local(x - 1)", LocalCalls: []string{"recur"}},
	}}
	unit := warmUnit{method: app.Functions[0], class: syntheticClass(app.Functions[0])}
	rep := &leak.Report{}

	done := make(chan []warm.ExtraFunc, 1)
	go func() {
		extras, _, _, _ := collectLocalExtras(app, unit, rep)
		done <- extras
	}()
	select {
	case extras := <-done:
		if len(extras) != 1 || extras[0].Name != "recur" {
			t.Errorf("extras = %+v, want exactly [recur]", extras)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("collectLocalExtras did not terminate on a self-referential .local() call")
	}
}

// TestCollectLocalExtrasCycleTerminates (calque#92): a two-function cycle
// (a -> b -> a) must resolve to exactly {a, b}, not loop forever.
func TestCollectLocalExtrasCycleTerminates(t *testing.T) {
	app := ir.App{Functions: []ir.Function{
		{Name: "entry", Body: "a.local(x)", LocalCalls: []string{"a"}},
		{Name: "a", Args: []string{"x"}, Body: "return b.local(x)", LocalCalls: []string{"b"}},
		{Name: "b", Args: []string{"x"}, Body: "return a.local(x)", LocalCalls: []string{"a"}},
	}}
	unit := warmUnit{method: app.Functions[0], class: syntheticClass(app.Functions[0])}
	rep := &leak.Report{}

	done := make(chan []warm.ExtraFunc, 1)
	go func() {
		extras, _, _, _ := collectLocalExtras(app, unit, rep)
		done <- extras
	}()
	select {
	case extras := <-done:
		if len(extras) != 2 {
			t.Fatalf("extras = %+v, want exactly 2 (a and b)", extras)
		}
		names := map[string]bool{extras[0].Name: true, extras[1].Name: true}
		if !names["a"] || !names["b"] {
			t.Errorf("extras = %+v, want {a, b}", extras)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("collectLocalExtras did not terminate on an a->b->a cycle")
	}
}

// TestCollectLocalExtrasResolvesBareImport (calque#146) is the import
// counterpart to the existing bare-constant/bare-function tests: a body
// that bare-references an imported name (e.g. `Path(...)` after `from
// pathlib import Path`) must resolve to app.ModuleImports and ship as an
// ExtraImport — previously this whole resolution target didn't exist at
// all, so it fell through to an unconditional NameError on execution.
func TestCollectLocalExtrasResolvesBareImport(t *testing.T) {
	app := ir.App{
		Functions:     []ir.Function{{Name: "run_blend", Body: "return Path('/tmp')", FreeRefs: []string{"Path"}}},
		ModuleImports: map[string]string{"Path": "from pathlib import Path"},
	}
	unit := warmUnit{method: app.Functions[0], class: syntheticClass(app.Functions[0])}
	rep := &leak.Report{}

	extras, consts, imports, _ := collectLocalExtras(app, unit, rep)
	if len(extras) != 0 || len(consts) != 0 {
		t.Errorf("extras=%+v consts=%+v, want none (Path resolves as an import, not a function/const)", extras, consts)
	}
	if len(imports) != 1 || imports[0].Name != "Path" || imports[0].Source != "from pathlib import Path" {
		t.Fatalf("imports = %+v, want exactly [{Path, \"from pathlib import Path\"}]", imports)
	}
}

// TestCollectLocalExtrasImportTransitiveThroughSiblingFunc proves an
// import reachable only through a SIBLING function's own FreeRefs (not the
// picked unit's own body) is still resolved — mirroring how
// TestCollectLocalExtrasTransitiveClosure already proves this for
// function-to-function chains.
func TestCollectLocalExtrasImportTransitiveThroughSiblingFunc(t *testing.T) {
	app := ir.App{
		Functions: []ir.Function{
			{Name: "run_blend", Body: "return _load()", LocalCalls: []string{"_load"}},
			{Name: "_load", Body: "return os.getcwd()", FreeRefs: []string{"os"}},
		},
		ModuleImports: map[string]string{"os": "import os"},
	}
	unit := warmUnit{method: app.Functions[0], class: syntheticClass(app.Functions[0])}
	rep := &leak.Report{}

	extras, _, imports, _ := collectLocalExtras(app, unit, rep)
	if len(extras) != 1 || extras[0].Name != "_load" {
		t.Fatalf("extras = %+v, want exactly [_load]", extras)
	}
	if len(imports) != 1 || imports[0].Name != "os" {
		t.Fatalf("imports = %+v, want exactly [os] (reached transitively through _load's own FreeRefs)", imports)
	}
}

// TestCollectLocalExtrasResolvesBareClass (calque#147) is the class
// counterpart to TestCollectLocalExtrasResolvesBareImport: a body that
// bare-instantiates a PLAIN module-level class (e.g. `_LogTee(...)`) must
// resolve to app.ModuleClasses and ship as an ExtraClass — previously this
// whole resolution target didn't exist at all, so it fell through to an
// unconditional NameError on execution.
func TestCollectLocalExtrasResolvesBareClass(t *testing.T) {
	app := ir.App{
		Functions:     []ir.Function{{Name: "run_blend", Body: "return _Adder(1).add(2)", FreeRefs: []string{"_Adder"}}},
		ModuleClasses: map[string]ir.ModuleClass{"_Adder": {Source: "class _Adder:\n    pass"}},
	}
	unit := warmUnit{method: app.Functions[0], class: syntheticClass(app.Functions[0])}
	rep := &leak.Report{}

	extras, consts, imports, classes := collectLocalExtras(app, unit, rep)
	if len(extras) != 0 || len(consts) != 0 || len(imports) != 0 {
		t.Errorf("extras=%+v consts=%+v imports=%+v, want none (_Adder resolves as a class)", extras, consts, imports)
	}
	if len(classes) != 1 || classes[0].Name != "_Adder" || classes[0].Source != "class _Adder:\n    pass" {
		t.Fatalf("classes = %+v, want exactly [{_Adder, \"class _Adder:\\n    pass\"}]", classes)
	}
}

// TestCollectLocalExtrasClassTransitiveThroughFreeRefs proves a class's OWN
// FreeRefs (e.g. a class-level attribute or a method referencing another
// module-level name) is enqueued through, mirroring ModuleConsts' own
// transitivity fix (calque#146.2).
func TestCollectLocalExtrasClassTransitiveThroughFreeRefs(t *testing.T) {
	app := ir.App{
		Functions: []ir.Function{
			{Name: "run_blend", Body: "return _Adder(1).add(2)", FreeRefs: []string{"_Adder"}},
		},
		ModuleClasses: map[string]ir.ModuleClass{
			"_Adder": {Source: "class _Adder:\n    pass", FreeRefs: []string{"os"}},
		},
		ModuleImports: map[string]string{"os": "import os"},
	}
	unit := warmUnit{method: app.Functions[0], class: syntheticClass(app.Functions[0])}
	rep := &leak.Report{}

	_, _, imports, classes := collectLocalExtras(app, unit, rep)
	if len(classes) != 1 || classes[0].Name != "_Adder" {
		t.Fatalf("classes = %+v, want exactly [_Adder]", classes)
	}
	if len(imports) != 1 || imports[0].Name != "os" {
		t.Fatalf("imports = %+v, want exactly [os] (reached transitively through _Adder's own FreeRefs)", imports)
	}
}

// TestUvPythonArgvAlwaysIncludesUvAndModal proves dry-run's Python command
// always goes through `uv run` — never the ambient shell's bare python3 —
// and always injects "modal" even when the picked unit's own resolved
// .image chain never itself pip_install()s Modal's SDK (a real script's
// body routinely references modal.Secret/modal.Volume/etc. directly
// without declaring modal as a dependency; found live against a real
// AI-Almanac script's dry-run).
func TestUvPythonArgvAlwaysIncludesUvAndModal(t *testing.T) {
	argv := uvPythonArgv(nil)
	if len(argv) < 2 || argv[0] != "uv" || argv[1] != "run" {
		t.Fatalf("argv = %v, want to start with [uv run ...]", argv)
	}
	if argv[len(argv)-1] != "python3" {
		t.Errorf("argv = %v, want to end with python3", argv)
	}
	foundModal := false
	for i, a := range argv {
		if a == "--with" && i+1 < len(argv) && argv[i+1] == "modal" {
			foundModal = true
		}
	}
	if !foundModal {
		t.Errorf("argv = %v, want a --with modal pair even with no resolved pip deps", argv)
	}
}

// TestUvPythonArgvInjectsResolvedPipDeps proves the picked unit's own
// resolved .image pip_install(...) list is injected via --with alongside
// modal, deduped (a package appearing in both the resolved Pip list and
// the always-included "modal" set isn't repeated) and in a deterministic
// (sorted) order so the command line is reproducible run to run.
func TestUvPythonArgvInjectsResolvedPipDeps(t *testing.T) {
	argv := uvPythonArgv([]string{"google-cloud-storage", "modal", "xarray"})
	want := []string{"uv", "run", "--with", "google-cloud-storage", "--with", "modal", "--with", "xarray", "python3"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}
}

// TestUvPythonArgvCalquePythonOverrideBypassesUv proves the CALQUE_PYTHON
// escape hatch still works exactly as before this change: when set, it's
// used as a literal interpreter path, bypassing uv entirely.
func TestUvPythonArgvCalquePythonOverrideBypassesUv(t *testing.T) {
	t.Setenv("CALQUE_PYTHON", "/opt/custom/python3.11")
	argv := uvPythonArgv([]string{"xarray"})
	want := []string{"/opt/custom/python3.11"}
	if len(argv) != 1 || argv[0] != want[0] {
		t.Errorf("argv = %v, want %v (CALQUE_PYTHON must bypass uv entirely)", argv, want)
	}
}
