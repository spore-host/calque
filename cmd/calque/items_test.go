package main

import (
	"bytes"
	"os"
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
