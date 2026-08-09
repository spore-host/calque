package main

import (
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
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
