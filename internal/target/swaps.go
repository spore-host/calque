package target

// cardSwaps is a curated, one-entry-at-a-time table of VERIFIED-SAFE GPU
// substitutions (calque#178) — each entry confirmed by actually running the
// real workload class on the substitute card (not inferred from a
// datasheet), mirroring sharingModeByFamily's own verification discipline
// (calque#104/#105). This is a deliberate, narrow exception to this
// package's "no selection logic" rule (spec §1/§4/§18): each entry is a
// documented FACT ("this specific swap was verified to work"), not a
// decision procedure that searches/infers swaps generally. Absence from
// this table means "not yet verified," not "impossible" — CardSwapFor never
// guesses an unentered card, same posture SharingModeFor already
// established for instance families.
//
// Applying a swap is entirely OPT-IN, gated behind calque real/ramp/
// fleetrun's --allow-card-swap flag — Recommend itself stays a pure
// pass-through (spec §4); callers consult this table separately (see
// cmd/calque/items.go's recommendedTarget) only when the operator has
// explicitly asked for it.
//
// DELIBERATELY EMPTY for now (calque#178): the motivating case
// (A100-80GB -> RTX PRO 6000, for AI-Almanac's forecasts_app.py) has NOT
// yet been verified against a real earth2studio inference run on real
// (spot) g7e hardware. Do not add an entry here from static analysis
// alone — add it only once that run has happened and produced a real,
// checked result (not just "didn't crash"), and fill VerifiedAgainst/
// VerifiedDate with what was actually run.
var cardSwaps = map[string]cardSwap{}

// cardSwap is one curated substitution: the card CardSwapFor's caller
// carried forward `To`, verified against a real workload (`VerifiedAgainst`)
// on the date given (`VerifiedDate`) — both fields exist to make an entry
// auditable, not just present.
type cardSwap struct {
	To              string
	VerifiedAgainst string
	VerifiedDate    string
}

// CardSwapFor looks up a verified-safe substitute for card (the exact
// string a script's gpu= asked for, e.g. "A100-80GB" — already stripped of
// any ":N" count suffix by the time a caller has confirmed the site is
// CleanSwap; see internal/gpu.evaluate). Returns ("", false) for any card
// with no table entry — callers must not guess; an unentered card means
// "not yet verified," never "assume it's fine."
func CardSwapFor(card string) (to string, ok bool) {
	sw, ok := cardSwaps[card]
	if !ok {
		return "", false
	}
	return sw.To, true
}
