package target

import "testing"

// TestCardSwapForMissEntryReturnsFalse (calque#178) proves CardSwapFor never
// guesses: a card with no table entry (which today is EVERY card, since
// cardSwaps ships empty pending real-hardware verification) returns
// ("", false), not a fabricated substitute.
func TestCardSwapForMissEntryReturnsFalse(t *testing.T) {
	to, ok := CardSwapFor("A100-80GB")
	if ok {
		t.Errorf("CardSwapFor(%q) = (%q, true), want (_, false) — no entry has been added yet (pending real-hardware verification, calque#178)", "A100-80GB", to)
	}
	if to != "" {
		t.Errorf("CardSwapFor on a miss should return empty string, got %q", to)
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

// TestCardSwapsEmptyPendingRealHardwareVerification (calque#178) is a
// deliberate guard: it FAILS the moment any entry is added to cardSwaps,
// forcing whoever adds one to also update/remove this test — a forcing
// function to make sure the addition is a conscious act, not an accident,
// and that whoever removes this test has actually read the "real hardware
// required" bar in swaps.go's own doc comment.
func TestCardSwapsEmptyPendingRealHardwareVerification(t *testing.T) {
	if len(cardSwaps) != 0 {
		t.Errorf("cardSwaps has %d entr(y/ies); if you just added one, confirm it was verified against real hardware (not just static analysis) per swaps.go's doc comment, then delete/update this guard test", len(cardSwaps))
	}
}
