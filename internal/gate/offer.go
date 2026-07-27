package gate

import (
	"fmt"
	"strings"
)

// ReplacementOffer is a structured "call the API instead of renting a GPU"
// proposal for one script unit (§11, Gap G). It is the machine-readable form of
// the analyze-time SUGGEST BEDROCK line: it carries the exact Bedrock modelId +
// region a user would call (the InvokeHint — missing entirely before G2), the
// confidence/provenance, and — for near matches — a labeled quality caveat with
// NO quality claim (exact-match discipline, §11).
//
// An offer exists only when the gate found a Bedrock route (exact or near). The
// run path (G3) short-circuits on an EXACT offer — near matches are surfaced but
// never auto-route, because "a variant is served" is not "your model is served."
type ReplacementOffer struct {
	ModelRef   string   // the HF repo the script self-hosts
	BedrockID  string   // the Bedrock modelId to call instead
	Catalog    string   // "foundation-model" | "marketplace"
	Confidence string   // confirmed|validated|ambiguous
	Regions    []string // Bedrock regions serving it
	Exact      bool     // true => safe to auto-route (short-circuit the GPU path)
	DiffAxes   []string // near-match: labeled axes of difference (empty when Exact)
	Source     string   // "hf-bedrock-map" | "signature-heuristic"
	Evidence   string   // provenance line
}

// Offer builds a ReplacementOffer from a gate Result, or returns nil when there
// is nothing to offer (no catalog route, or identity hidden behind a mount, or a
// non-inference shape). It is the single source of truth for "should this route
// away" — both the CLI renderer (G4) and the run-path short-circuit (G3) call it,
// so they can never disagree.
//
// Exact offers require inference shape AND an exact identity (== Eligible). Near
// offers surface a candidate for a human to verify; they do NOT gate execution.
func (r Result) Offer() *ReplacementOffer {
	// No identity, or no Bedrock route at all: nothing to offer.
	if r.ModelRef == "" || r.Tier == TierNone {
		return nil
	}
	// A near match is only worth surfacing on an inference shape — a near match on
	// a training/unknown shape is not an API-call candidate.
	if r.Tier == TierNear && r.Shape != ShapeInference {
		return nil
	}
	o := &ReplacementOffer{
		ModelRef:   r.ModelRef,
		BedrockID:  r.MatchID,
		Catalog:    r.Catalog,
		Confidence: r.Confidence,
		Regions:    r.Regions,
		Exact:      r.Eligible, // exact identity AND inference shape
		Source:     r.Source,
		Evidence:   r.Evidence,
	}
	if !o.Exact {
		o.DiffAxes = r.DiffAxes
	}
	return o
}

// InvokeHint returns a one-line "here's what to call instead" hint: the Bedrock
// modelId and a concrete region to invoke it in. This is the actionable payload a
// user needs to actually route away — before G2 the tool named the model but not
// how to reach it. Region is the first serving region, or a placeholder note when
// the map didn't carry regions (signature-heuristic matches have none).
func (o *ReplacementOffer) InvokeHint() string {
	region := "<a Bedrock region>"
	if len(o.Regions) > 0 {
		region = o.Regions[0]
	}
	return fmt.Sprintf("bedrock-runtime invoke-model --model-id %s --region %s", o.BedrockID, region)
}

// Render produces the human-facing offer text for the CLI (G4). Exact offers read
// as a recommendation ("don't rent a GPU"); near offers read as a candidate to
// verify, explicitly carrying the axes of difference and NO quality claim (§11).
func (o *ReplacementOffer) Render() string {
	var b strings.Builder
	if o.Exact {
		fmt.Fprintf(&b, "SUGGEST BEDROCK: %s is served as %s", o.ModelRef, o.BedrockID)
		if o.Catalog != "" {
			fmt.Fprintf(&b, " (%s, %s)", o.Catalog, o.Confidence)
		}
		b.WriteString(" — don't rent a GPU.\n")
		fmt.Fprintf(&b, "        invoke: %s\n", o.InvokeHint())
	} else {
		fmt.Fprintf(&b, "NEAR-MATCH CANDIDATE: %s ~ %s [differs: %s]\n", o.ModelRef, o.BedrockID, strings.Join(o.DiffAxes, ", "))
		b.WriteString("        this is a CANDIDATE, not a claim — verify equivalence yourself before routing away.\n")
		fmt.Fprintf(&b, "        candidate invoke: %s\n", o.InvokeHint())
	}
	if len(o.Regions) > 0 {
		fmt.Fprintf(&b, "        Bedrock regions: %s\n", strings.Join(o.Regions, ", "))
	}
	if o.Evidence != "" {
		fmt.Fprintf(&b, "        evidence: %s (%s)\n", o.Evidence, o.Source)
	}
	return b.String()
}
