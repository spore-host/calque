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
