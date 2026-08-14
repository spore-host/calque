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
var cardSwaps = map[string]cardSwap{
	// calque#178: verified on a real g7e.2xlarge spot instance (RTX PRO
	// 6000 Blackwell, 97887MiB) — built the exact Dockerfile calque itself
	// resolves for AI-Almanac's forecasts_app.py's run_forecast_inference_base
	// (nvcr.io/nvidia/pytorch:25.12-py3 base, the real git-pinned
	// earth2studio[aifs,aifsens,data,fuxi,gencast,graphcast] install), then
	// ran earth2studio's AIFS model end-to-end inside that container: real
	// model weight load, real live GFS data fetch (NOAA's public S3
	// bucket), real inference rollout via earth2studio.run.deterministic —
	// reported SUCCESS, completed in ~9s. A100-80GB has no AWS single-GPU
	// equivalent at all (p4de.24xlarge is an 8-GPU box); this substitution
	// runs the same B=1 workload on a real, cheaper single-GPU card.
	"A100-80GB": {
		To:              "RTX PRO 6000",
		VerifiedAgainst: "earth2studio AIFS deterministic inference (real GFS data), calque#178",
		VerifiedDate:    "2026-08-14",
	},
}

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
