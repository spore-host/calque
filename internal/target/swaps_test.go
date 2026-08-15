package target

import "testing"

// TestCardSwapForMissEntryReturnsFalse (calque#178) proves CardSwapFor never
// guesses: a card with no table entry returns ("", false), not a fabricated
// substitute.
func TestCardSwapForMissEntryReturnsFalse(t *testing.T) {
	to, ok := CardSwapFor("H100")
	if ok {
		t.Errorf("CardSwapFor(%q) = (%q, true), want (_, false) — no entry has been verified for this card", "H100", to)
	}
	if to != "" {
		t.Errorf("CardSwapFor on a miss should return empty string, got %q", to)
	}
}

// TestCardSwapForA100_80GBReturnsRTXPro6000 (calque#178) proves the
// verified table entry: A100-80GB -> RTX PRO 6000, confirmed against a real
// g7e.2xlarge spot instance running earth2studio's AIFS model end-to-end
// (real weight load, real live GFS data, real inference rollout) — see
// swaps.go's own doc comment for the exact verification run.
func TestCardSwapForA100_80GBReturnsRTXPro6000(t *testing.T) {
	to, ok := CardSwapFor("A100-80GB")
	if !ok {
		t.Fatal("CardSwapFor(\"A100-80GB\") = (_, false), want a verified hit")
	}
	if to != "RTX PRO 6000" {
		t.Errorf("CardSwapFor(\"A100-80GB\") = %q, want %q", to, "RTX PRO 6000")
	}
}

// TestCardSwapForHitReturnsConfiguredCard proves the lookup mechanism
// itself works correctly once an entry DOES exist — exercised via a
// synthetic entry so this test doesn't depend on cardSwaps' real (and
// currently empty) contents.
func TestCardSwapForHitReturnsConfiguredCard(t *testing.T) {
	const testCard = "test-card-for-TestCardSwapForHitReturnsConfiguredCard"
	cardSwaps[testCard] = cardSwap{To: "RTX PRO 6000", VerifiedAgainst: "unit test", VerifiedDate: "2026-08-14"}
	defer delete(cardSwaps, testCard)

	to, ok := CardSwapFor(testCard)
	if !ok {
		t.Fatalf("CardSwapFor(%q) = (_, false), want a hit", testCard)
	}
	if to != "RTX PRO 6000" {
		t.Errorf("CardSwapFor(%q) = %q, want %q", testCard, to, "RTX PRO 6000")
	}
}

// TestCardSwapsOnlyContainsVerifiedEntries (calque#178) is a deliberate
// guard: it FAILS the moment an entry is added WITHOUT a VerifiedAgainst/
// VerifiedDate — forcing whoever adds a new entry to fill in real
// verification provenance, not just a card pair. Every entry currently
// present was verified on real hardware (see swaps.go's own doc comments
// per entry).
func TestCardSwapsOnlyContainsVerifiedEntries(t *testing.T) {
	for card, sw := range cardSwaps {
		if sw.VerifiedAgainst == "" || sw.VerifiedDate == "" {
			t.Errorf("cardSwaps[%q] has no VerifiedAgainst/VerifiedDate — every entry must record what real workload confirmed it, not just the card pair", card)
		}
	}
}
