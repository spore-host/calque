package target

import (
	"testing"

	"github.com/spore-host/calque/internal/ir"
)

// TestEveryTruffleResolvableFamilyHasSharingMode is the table-driven test the
// issue asks for (calque#105): every instance family calque's four GPU
// targets can resolve to must have an EXPLICIT SharingMode entry — no silent
// default. Mirrors docs/gpu-sharing-support-matrix.md's live-hardware
// findings (calque#104) exactly, so a future change to the table that drops
// a family is caught here, not downstream.
func TestEveryTruffleResolvableFamilyHasSharingMode(t *testing.T) {
	want := map[string]SharingMode{
		"g6":  MPS,
		"g6e": MPS,
		"g7":  MIG,
		"g7e": MIG,
	}
	for family, wantMode := range want {
		mode, ok := sharingModeByFamily[family]
		if !ok {
			t.Errorf("family %q has no SharingMode entry, want %q", family, wantMode)
			continue
		}
		if mode != wantMode {
			t.Errorf("family %q SharingMode = %q, want %q", family, mode, wantMode)
		}
	}
	if len(sharingModeByFamily) != len(want) {
		t.Errorf("sharingModeByFamily has %d entries, want exactly %d (%v) — an untracked family was added without a test update",
			len(sharingModeByFamily), len(want), sharingModeByFamily)
	}
}

func TestSharingModeForExtractsFamilyFromInstanceType(t *testing.T) {
	cases := []struct {
		instance string
		want     SharingMode
		wantOK   bool
	}{
		{"g7e.2xlarge", MIG, true},
		{"g7e.48xlarge", MIG, true}, // same family, different size -> same mode
		{"g7.2xlarge", MIG, true},
		{"g6.2xlarge", MPS, true},
		{"g6e.2xlarge", MPS, true},
		{"p5.48xlarge", "", false}, // no entry for this family (yet) — must NOT silently default
		{"malformed-no-dot", "", false},
	}
	for _, c := range cases {
		got, ok := SharingModeFor(c.instance)
		if ok != c.wantOK || got != c.want {
			t.Errorf("SharingModeFor(%q) = (%q, %v), want (%q, %v)", c.instance, got, ok, c.want, c.wantOK)
		}
	}
}

// TestDefaultCardFamilyHasSharingModeEntry guards the invariant
// StubRecommender.Recommend relies on: defaultCardFamily must always be a
// valid key into sharingModeByFamily, since Recommend reads it directly
// (not via the ok-checked SharingModeFor) — if this ever drifted, Recommend
// would silently return the zero-value SharingMode instead of failing loudly.
func TestDefaultCardFamilyHasSharingModeEntry(t *testing.T) {
	if _, ok := sharingModeByFamily[defaultCardFamily]; !ok {
		t.Fatalf("defaultCardFamily %q has no entry in sharingModeByFamily; StubRecommender.Recommend would silently return the zero-value SharingMode", defaultCardFamily)
	}
}

// TestStubRecommenderFillsSharingMode proves the stub's SharingMode fill
// (calque#105) without adding real selection logic to the stub — it's
// still one hardcoded constant's worth of "brain" (spec §4), just now
// including this axis.
func TestStubRecommenderFillsSharingMode(t *testing.T) {
	tgt := StubRecommender{}.Recommend(ir.App{}, ir.Function{})
	if tgt.Card != DefaultCard {
		t.Errorf("Card = %q, want %q", tgt.Card, DefaultCard)
	}
	want := sharingModeByFamily[defaultCardFamily]
	if tgt.SharingMode != want {
		t.Errorf("SharingMode = %q, want %q (defaultCardFamily=%q)", tgt.SharingMode, want, defaultCardFamily)
	}
}
