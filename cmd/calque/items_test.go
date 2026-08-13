package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/target"
)

// synthOf builds the standard "dry-run-item-%d"-shaped synth closure used by
// callers of realOrSyntheticItems, so tests can assert against its output.
func synthOf(i int) any { return "synth-" + string(rune('a'+i)) }

// TestRealOrSyntheticItems_UsesRealWhenLongEnough (calque#136): when the
// parsed unit's Items has at least n entries, realOrSyntheticItems must use
// them verbatim (in order) and emit NO leak.
func TestRealOrSyntheticItems_UsesRealWhenLongEnough(t *testing.T) {
	unit := warmUnit{method: ir.Function{Name: "generate", Items: []any{"a", "b", "c"}}}
	rep := &leak.Report{}

	items := realOrSyntheticItems(unit, 3, synthOf, rep)

	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}
	for i, want := range []any{"a", "b", "c"} {
		if items[i].Index != i || items[i].Payload != want {
			t.Errorf("items[%d] = %+v, want {Index:%d Payload:%v}", i, items[i], i, want)
		}
	}
	if rep.Len() != 0 {
		t.Errorf("expected no leak when real items suffice; got %+v", rep.Leaks)
	}
}

// TestRealOrSyntheticItems_FallsBackWhenShorterThanN (calque#136): real Items
// shorter than the requested n must fall back to synth for ALL n items (not a
// partial mix) and emit a leak explaining why.
func TestRealOrSyntheticItems_FallsBackWhenShorterThanN(t *testing.T) {
	unit := warmUnit{method: ir.Function{Name: "generate", Items: []any{"a", "b"}}}
	rep := &leak.Report{}

	items := realOrSyntheticItems(unit, 5, synthOf, rep)

	if len(items) != 5 {
		t.Fatalf("len(items) = %d, want 5", len(items))
	}
	for i := range items {
		want := synthOf(i)
		if items[i].Index != i || items[i].Payload != want {
			t.Errorf("items[%d] = %+v, want synthesized {Index:%d Payload:%v}", i, items[i], i, want)
		}
	}
	if rep.Len() != 1 {
		t.Fatalf("expected exactly one leak, got %d: %+v", rep.Len(), rep.Leaks)
	}
	if !strings.Contains(rep.Leaks[0].Detail, "has 2 items but --n requested 5") {
		t.Errorf("leak detail = %q, want mention of the length mismatch", rep.Leaks[0].Detail)
	}
}

// TestRealOrSyntheticItems_FallsBackWhenNil (calque#136): a nil Items (the
// script's iterable wasn't statically resolvable) must fall back to synth for
// every item and emit a leak that reads differently from the "too short"
// case (no real data at all vs. some-but-not-enough).
func TestRealOrSyntheticItems_FallsBackWhenNil(t *testing.T) {
	unit := warmUnit{method: ir.Function{Name: "generate", Items: nil}}
	rep := &leak.Report{}

	items := realOrSyntheticItems(unit, 3, synthOf, rep)

	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}
	for i := range items {
		want := synthOf(i)
		if items[i].Index != i || items[i].Payload != want {
			t.Errorf("items[%d] = %+v, want synthesized {Index:%d Payload:%v}", i, items[i], i, want)
		}
	}
	if rep.Len() != 1 {
		t.Fatalf("expected exactly one leak, got %d: %+v", rep.Len(), rep.Leaks)
	}
	if !strings.Contains(rep.Leaks[0].Detail, "wasn't statically extractable") {
		t.Errorf("leak detail = %q, want the nil-Items wording", rep.Leaks[0].Detail)
	}
}

// TestRecommendedTarget_CarriesParsedUnitsCard (calque#134): the real-AWS
// commands' shared Target-building helper must carry a parsed unit's own
// requested card (unit.method.GPU) rather than always hardcoding
// target.DefaultCard, and must still pin the caller's own --instance
// regardless of which card Recommend picked (these commands never call
// plan.FillTarget to derive Instance from Card).
func TestRecommendedTarget_CarriesParsedUnitsCard(t *testing.T) {
	unit := warmUnit{method: ir.Function{Name: "generate", GPU: "H100"}}
	tgt := recommendedTarget(unit, "p5.48xlarge")
	if tgt.Card != "H100" {
		t.Errorf("Card = %q, want %q", tgt.Card, "H100")
	}
	if tgt.Instance != "p5.48xlarge" {
		t.Errorf("Instance = %q, want %q (caller's pinned --instance)", tgt.Instance, "p5.48xlarge")
	}
}

// TestRecommendedTarget_FallsBackToDefaultCardForZeroUnit: the zero warmUnit{}
// (no --script parsed) must fall back to target.DefaultCard exactly as
// before this change — regression guard for the common no-script case.
func TestRecommendedTarget_FallsBackToDefaultCardForZeroUnit(t *testing.T) {
	tgt := recommendedTarget(warmUnit{}, "g7e.2xlarge")
	if tgt.Card != target.DefaultCard {
		t.Errorf("Card = %q, want %q (zero warmUnit must fall back to DefaultCard)", tgt.Card, target.DefaultCard)
	}
	if tgt.Instance != "g7e.2xlarge" {
		t.Errorf("Instance = %q, want %q", tgt.Instance, "g7e.2xlarge")
	}
}

// TestManifestBodyForUnit_ZeroUnitReturnsNotOK (calque#79 Part 1): the zero
// warmUnit{} (--script unset, or its parse failed) must report ok=false so
// callers (realrun.go/fleetrun.go) fall back to their existing hardcoded
// vLLM body — the regression guard for every pre-#79 caller.
func TestManifestBodyForUnit_ZeroUnitReturnsNotOK(t *testing.T) {
	rep := &leak.Report{}
	_, ok := manifestBodyForUnit(ir.App{}, warmUnit{}, rep)
	if ok {
		t.Error("manifestBodyForUnit() ok = true, want false for the zero warmUnit{}")
	}
	if rep.Len() != 0 {
		t.Errorf("expected no leak for the zero-unit case, got %d: %+v", rep.Len(), rep.Leaks)
	}
}

// TestManifestBodyForUnit_CarriesRealBodyAndArg (calque#79 Part 1): a real
// parsed unit's own EnterBody/MethodBody/ItemArg must ride through verbatim
// — the actual fix for the gap where calque real/ramp/fleetrun always drove
// a hardcoded vLLM body regardless of what --script parsed.
func TestManifestBodyForUnit_CarriesRealBodyAndArg(t *testing.T) {
	unit := warmUnit{
		class:  ir.Class{EnterBody: "self.x = 1"},
		method: ir.Function{Name: "transform", Body: "return item * 2", ItemArg: "item", Args: []string{"self", "item"}},
	}
	rep := &leak.Report{}
	body, ok := manifestBodyForUnit(ir.App{}, unit, rep)
	if !ok {
		t.Fatal("manifestBodyForUnit() ok = false, want true for a real unit")
	}
	if body.EnterBody != "self.x = 1" {
		t.Errorf("EnterBody = %q, want the unit's real @enter body", body.EnterBody)
	}
	if body.MethodBody != "return item * 2" {
		t.Errorf("MethodBody = %q, want the unit's real @method body", body.MethodBody)
	}
	if body.MethodArg != "item" {
		t.Errorf("MethodArg = %q, want %q", body.MethodArg, "item")
	}
	if body.Starmap {
		t.Error("Starmap = true, want false for a plain .map()'d unit")
	}
}

// TestManifestBodyForUnit_DefaultsItemArgWhenEmpty: a unit with no
// ItemArg set (unusual, but not statically impossible) must default to
// "item" — mirroring dryRunWarm's own identical fallback in run.go.
func TestManifestBodyForUnit_DefaultsItemArgWhenEmpty(t *testing.T) {
	unit := warmUnit{method: ir.Function{Name: "f", Body: "pass"}}
	rep := &leak.Report{}
	body, ok := manifestBodyForUnit(ir.App{}, unit, rep)
	if !ok {
		t.Fatal("manifestBodyForUnit() ok = false, want true")
	}
	if body.MethodArg != "item" {
		t.Errorf("MethodArg = %q, want the default %q", body.MethodArg, "item")
	}
}

// TestManifestBodyForUnit_StarmapCarriesFullArgList (calque#93): a
// .starmap()'d unit must carry Starmap=true and its FULL non-self/cls arg
// list, not just the first — the shape the runner's tuple-splat needs.
func TestManifestBodyForUnit_StarmapCarriesFullArgList(t *testing.T) {
	unit := warmUnit{
		method: ir.Function{
			Name: "combine", Body: "return a + b", ItemArg: "a",
			Args: []string{"self", "a", "b"}, Invoke: ir.InvokeStarmap,
		},
	}
	rep := &leak.Report{}
	body, ok := manifestBodyForUnit(ir.App{}, unit, rep)
	if !ok {
		t.Fatal("manifestBodyForUnit() ok = false, want true")
	}
	if !body.Starmap {
		t.Error("Starmap = false, want true for an ir.InvokeStarmap unit")
	}
	if len(body.MethodArgs) != 2 || body.MethodArgs[0] != "a" || body.MethodArgs[1] != "b" {
		t.Errorf("MethodArgs = %v, want [a b] (self/cls filtered, full remaining list)", body.MethodArgs)
	}
}

// TestManifestBodyForUnit_ShipsLocalExtrasAndLeaks (calque#92/#139): a unit
// whose body references a sibling function via .local() must have that
// sibling shipped as an Extra AND leak the shipment — the real-AWS
// counterpart to dryRunWarm's identical local-extras handling.
func TestManifestBodyForUnit_ShipsLocalExtrasAndLeaks(t *testing.T) {
	app := ir.App{
		Script:    "script.py",
		Functions: []ir.Function{{Name: "helper", Args: []string{"x"}, Body: "return x + 1"}},
	}
	unit := warmUnit{
		method: ir.Function{Name: "main", Body: "return helper.local(item)", ItemArg: "item", LocalCalls: []string{"helper"}},
	}
	rep := &leak.Report{}
	body, ok := manifestBodyForUnit(app, unit, rep)
	if !ok {
		t.Fatal("manifestBodyForUnit() ok = false, want true")
	}
	if len(body.Extras) != 1 || body.Extras[0].Name != "helper" {
		t.Fatalf("Extras = %+v, want one ExtraFunc named %q", body.Extras, "helper")
	}
	if rep.Len() != 1 {
		t.Fatalf("expected exactly one leak reporting the shipped extra, got %d: %+v", rep.Len(), rep.Leaks)
	}
	if !strings.Contains(rep.Leaks[0].Detail, "helper") {
		t.Errorf("leak detail %q should name the shipped sibling function", rep.Leaks[0].Detail)
	}
}

// TestItemFromFile_ReturnsOneItemWithExactBytes (calque real --item-file
// PATH) proves itemFromFile reads a real file's RAW bytes verbatim into a
// single Index-0 item — no encoding/interpretation on this side; base64
// only happens implicitly via encoding/json's own []byte handling once the
// manifest is marshaled (proven separately by
// TestWorker_SecretsAndPayloadBase64BytesSurviveIntoConfig in
// internal/pool, which round-trips through the real JSON path).
func TestItemFromFile_ReturnsOneItemWithExactBytes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bundle.bin"
	want := []byte{0x00, 0x01, 0xff, 'h', 'i'} // deliberately includes non-UTF8 bytes
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := itemFromFile(path)
	if err != nil {
		t.Fatalf("itemFromFile: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if items[0].Index != 0 {
		t.Errorf("Index = %d, want 0", items[0].Index)
	}
	got, ok := items[0].Payload.([]byte)
	if !ok {
		t.Fatalf("Payload type = %T, want []byte", items[0].Payload)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Payload = %v, want %v", got, want)
	}
}

// TestItemFromFile_MissingFileErrors proves a bad path fails loudly
// instead of returning a silently-empty item.
func TestItemFromFile_MissingFileErrors(t *testing.T) {
	if _, err := itemFromFile("/nonexistent/path/does-not-exist.bin"); err == nil {
		t.Error("itemFromFile on a nonexistent path returned nil error, want an error")
	}
}

// setPyastDirEnv points pyastDir() (main.go) at the real tools/pyast
// directory for the duration of the test — warmUnitForScript calls
// pyastDir() internally (unlike the free_refs_*_e2e_test.go tests, which
// call parse.Parse directly with an explicit dir), so it needs the
// CALQUE_PYAST_DIR env var set rather than a parameter, since `go test`'s
// working directory is the package dir (cmd/calque), not the repo root
// pyastDir()'s "tools/pyast" relative default assumes.
func setPyastDirEnv(t *testing.T) {
	t.Helper()
	dir, err := filepath.Abs("../../tools/pyast")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CALQUE_PYAST_DIR", dir)
}

// TestWarmUnitForScript_SelectsRequestedEntrypoint is the regression test
// for the gap this session found running real/fleetrun against AI-Almanac's
// blending_app.py (7 entrypoints): warmUnitForScript previously called
// pickWarmUnit(app, "") unconditionally — real/ramp/fleetrun had NO way to
// select an entrypoint at all, unlike run --dry-run's own --entrypoint
// flag. A multi-entrypoint script either picked an arbitrary unit or (once
// resolveEntrypoint's ambiguity error existed elsewhere) would have had no
// path to supply the disambiguating name. Uses the real parser (shells out
// to pyast) against the existing entrypoint_scoped_invoke.py fixture —
// two entrypoints invoking two UNRELATED callables, the same shape
// pickWarmUnitScopedToSelectedEntrypoint already proves at the ir.App
// level; this proves the wiring from a script PATH + entrypoint STRING
// down to the same correct result.
func TestWarmUnitForScript_SelectsRequestedEntrypoint(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping pyast contract test")
	}
	setPyastDirEnv(t)
	script, err := filepath.Abs("../../testdata/scripts/entrypoint_scoped_invoke.py")
	if err != nil {
		t.Fatal(err)
	}
	rep := &leak.Report{}

	_, unit, ok := warmUnitForScript(context.Background(), script, "do_evaluate", rep)
	if !ok {
		t.Fatal("warmUnitForScript(do_evaluate) failed")
	}
	if !unit.plainFunction || unit.method.Name != "evaluate" {
		t.Errorf("warmUnitForScript(do_evaluate) = %+v, want the plain function evaluate, NOT train_step", unit)
	}

	_, unit, ok = warmUnitForScript(context.Background(), script, "do_train", rep)
	if !ok {
		t.Fatal("warmUnitForScript(do_train) failed")
	}
	if unit.plainFunction || unit.class.Name != "Trainer" || unit.method.Name != "train_step" {
		t.Errorf("warmUnitForScript(do_train) = %+v, want Trainer.train_step", unit)
	}
}

// TestWarmUnitForScript_AmbiguousEntrypointFallsBackToSyntheticWithLeak
// proves an unresolved-ambiguity script ("" requested, 2+ entrypoints)
// degrades to the SAME synthesized-placeholder fallback as a parse
// failure — a loud leak, not a silent arbitrary pick.
func TestWarmUnitForScript_AmbiguousEntrypointFallsBackToSyntheticWithLeak(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping pyast contract test")
	}
	setPyastDirEnv(t)
	script, err := filepath.Abs("../../testdata/scripts/entrypoint_scoped_invoke.py")
	if err != nil {
		t.Fatal(err)
	}
	rep := &leak.Report{}

	_, unit, ok := warmUnitForScript(context.Background(), script, "", rep)
	if ok {
		t.Fatalf("warmUnitForScript with ambiguous entrypoint = %+v, want ok=false", unit)
	}
	if rep.Len() != 1 || !strings.Contains(rep.Leaks[0].Detail, "multiple entrypoints") {
		t.Errorf("expected a single 'multiple entrypoints' leak, got %+v", rep.Leaks)
	}
}

// TestWarmUnitForScriptFn_FunctionSelectsByNameRegardlessOfEntrypoint proves
// --function picks the named callable directly, bypassing pickWarmUnit's
// automatic scan entirely — needed for AI-Almanac's app.py, where the only
// @app.local_entrypoint() invokes run_benchmark (the GCS-backed sibling),
// never run_benchmark_local; pickWarmUnit's scan alone has no way to reach
// run_benchmark_local at all. Reuses entrypoint_scoped_invoke.py's `evaluate`
// (only ever invoked from do_evaluate) to prove the SAME shape: selecting it
// by --function name works even without passing --entrypoint at all.
func TestWarmUnitForScriptFn_FunctionSelectsByNameRegardlessOfEntrypoint(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping pyast contract test")
	}
	setPyastDirEnv(t)
	script, err := filepath.Abs("../../testdata/scripts/entrypoint_scoped_invoke.py")
	if err != nil {
		t.Fatal(err)
	}
	rep := &leak.Report{}

	// No --entrypoint at all (would otherwise be ambiguous, 2 entrypoints) —
	// --function must still resolve directly to evaluate.
	_, unit, ok := warmUnitForScriptFn(context.Background(), script, "", "evaluate", rep)
	if !ok {
		t.Fatalf("warmUnitForScriptFn(--function evaluate) failed; leaks: %+v", rep.Leaks)
	}
	if !unit.plainFunction || unit.method.Name != "evaluate" {
		t.Errorf("warmUnitForScriptFn(--function evaluate) = %+v, want the plain function evaluate", unit)
	}
	if rep.Len() != 0 {
		t.Errorf("expected no leak when --function resolves cleanly; got %+v", rep.Leaks)
	}
}

// TestWarmUnitForScriptFn_UnknownFunctionFallsBackToSyntheticWithLeak proves
// a --function name that matches nothing degrades to the same synthesized-
// placeholder fallback as a parse failure, loudly.
func TestWarmUnitForScriptFn_UnknownFunctionFallsBackToSyntheticWithLeak(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping pyast contract test")
	}
	setPyastDirEnv(t)
	script, err := filepath.Abs("../../testdata/scripts/entrypoint_scoped_invoke.py")
	if err != nil {
		t.Fatal(err)
	}
	rep := &leak.Report{}

	_, unit, ok := warmUnitForScriptFn(context.Background(), script, "", "does_not_exist", rep)
	if ok {
		t.Fatalf("warmUnitForScriptFn(--function does_not_exist) = %+v, want ok=false", unit)
	}
	if rep.Len() != 1 || !strings.Contains(rep.Leaks[0].Detail, "does_not_exist") {
		t.Errorf("expected a single leak naming the missing function, got %+v", rep.Leaks)
	}
}

// TestPickWarmUnitByName_FindsPlainFunctionByName and its sibling below
// exercise pickWarmUnitByName directly (no parser involved) against
// synthetic ir.App values, covering both callable shapes it must resolve:
// a plain @app.function and an @cls method.
func TestPickWarmUnitByName_FindsPlainFunctionByName(t *testing.T) {
	app := ir.App{Functions: []ir.Function{
		{Name: "run_benchmark", Args: []string{"job_id", "config", "outputs_bucket"}},
		{Name: "run_benchmark_local", Args: []string{"job_id", "config", "input_bundle", "runtime_env"}},
	}}

	unit, ok := pickWarmUnitByName(app, "run_benchmark_local")
	if !ok {
		t.Fatal("pickWarmUnitByName(run_benchmark_local) failed")
	}
	if !unit.plainFunction || unit.method.Name != "run_benchmark_local" {
		t.Errorf("pickWarmUnitByName(run_benchmark_local) = %+v, want the plain function run_benchmark_local", unit)
	}
}

func TestPickWarmUnitByName_FindsClsMethodByName(t *testing.T) {
	app := ir.App{Classes: []ir.Class{
		{Name: "Trainer", EnterBody: "self.model = 1", Methods: []ir.Function{
			{Name: "train_step", Args: []string{"self", "batch"}},
		}},
	}}

	unit, ok := pickWarmUnitByName(app, "train_step")
	if !ok {
		t.Fatal("pickWarmUnitByName(train_step) failed")
	}
	if unit.plainFunction || unit.class.Name != "Trainer" || unit.method.Name != "train_step" {
		t.Errorf("pickWarmUnitByName(train_step) = %+v, want Trainer.train_step", unit)
	}
}

func TestPickWarmUnitByName_UnknownNameReturnsNotOK(t *testing.T) {
	app := ir.App{Functions: []ir.Function{{Name: "run_benchmark"}}}

	if _, ok := pickWarmUnitByName(app, "does_not_exist"); ok {
		t.Error("pickWarmUnitByName(does_not_exist) = ok=true, want false")
	}
}

// TestItemFromArgs_BuildsTupleWithFileBytesAndJSONLiterals proves
// itemFromArgs assembles a SINGLE item whose Payload is a tuple mixing raw
// file bytes (position covered by --arg-file) with plain JSON literals
// (positions covered by --arg-json) in the correct order, and reports
// exactly which positions are base64 bytes — the shape run_benchmark_local's
// real signature (job_id: str, config: dict, input_bundle: bytes,
// runtime_env: dict | None) needs.
func TestItemFromArgs_BuildsTupleWithFileBytesAndJSONLiterals(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bundle.bin"
	wantBytes := []byte{0x00, 0x01, 0xff, 'h', 'i'}
	if err := os.WriteFile(path, wantBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	items, base64Indices, err := itemFromArgs(
		map[int]string{2: path},
		map[int]string{0: `"job-42"`, 1: `{"model_name": "romp"}`},
	)
	if err != nil {
		t.Fatalf("itemFromArgs: %v", err)
	}
	if len(items) != 1 || items[0].Index != 0 {
		t.Fatalf("items = %+v, want one Index-0 item", items)
	}
	tuple, ok := items[0].Payload.([]any)
	if !ok || len(tuple) != 3 {
		t.Fatalf("Payload = %+v (%T), want a 3-element []any", items[0].Payload, items[0].Payload)
	}
	if tuple[0] != "job-42" {
		t.Errorf("tuple[0] = %v, want %q", tuple[0], "job-42")
	}
	cfg, ok := tuple[1].(map[string]any)
	if !ok || cfg["model_name"] != "romp" {
		t.Errorf("tuple[1] = %v, want a map with model_name=romp", tuple[1])
	}
	gotBytes, ok := tuple[2].([]byte)
	if !ok || !bytes.Equal(gotBytes, wantBytes) {
		t.Errorf("tuple[2] = %v (%T), want %v", tuple[2], tuple[2], wantBytes)
	}
	if len(base64Indices) != 1 || base64Indices[0] != 2 {
		t.Errorf("base64Indices = %v, want [2]", base64Indices)
	}
}

// TestItemFromArgs_GapInIndicesErrors proves a missing position (0 and 2
// given, 1 skipped) fails loudly instead of silently mis-binding the
// shipped body's real positional args.
func TestItemFromArgs_GapInIndicesErrors(t *testing.T) {
	if _, _, err := itemFromArgs(map[int]string{2: "/tmp/x"}, map[int]string{0: `"a"`}); err == nil {
		t.Error("itemFromArgs with a gap at index 1 returned nil error, want an error")
	}
}

// TestItemFromArgs_DoubleCoveredIndexErrors proves the same index given via
// BOTH --arg-file and --arg-json is a caller error, not a silent
// last-one-wins pick.
func TestItemFromArgs_DoubleCoveredIndexErrors(t *testing.T) {
	if _, _, err := itemFromArgs(map[int]string{0: "/tmp/x"}, map[int]string{0: `"a"`}); err == nil {
		t.Error("itemFromArgs with index 0 double-covered returned nil error, want an error")
	}
}

// TestItemFromArgs_BadJSONErrors proves an invalid JSON literal fails
// loudly with a clear cause instead of an opaque downstream JSON error once
// the manifest is written.
func TestItemFromArgs_BadJSONErrors(t *testing.T) {
	if _, _, err := itemFromArgs(nil, map[int]string{0: "not valid json"}); err == nil {
		t.Error("itemFromArgs with invalid JSON returned nil error, want an error")
	}
}
