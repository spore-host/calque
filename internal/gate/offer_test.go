package gate

import (
	"strings"
	"testing"
)

func TestOfferExact(t *testing.T) {
	r := Result{
		ModelRef: "meta-llama/Meta-Llama-3-8B-Instruct", Shape: ShapeInference,
		Tier: TierExact, Eligible: true, MatchID: "meta.llama3-8b-instruct-v1:0",
		Catalog: "foundation-model", Confidence: "confirmed",
		Regions: []string{"us-east-1", "us-west-2"}, Source: "hf-bedrock-map",
		Evidence: "hf-bedrock-map v1: meta.llama3-8b-instruct-v1:0 (foundation-model, confirmed)",
	}
	o := r.Offer()
	if o == nil {
		t.Fatal("exact+inference must produce an offer")
	}
	if !o.Exact {
		t.Error("offer for an eligible result should be Exact")
	}
	if o.BedrockID != "meta.llama3-8b-instruct-v1:0" {
		t.Errorf("BedrockID = %q", o.BedrockID)
	}
	hint := o.InvokeHint()
	if !strings.Contains(hint, "meta.llama3-8b-instruct-v1:0") || !strings.Contains(hint, "us-east-1") {
		t.Errorf("invoke hint missing modelId or region: %q", hint)
	}
	render := o.Render()
	if !strings.Contains(render, "SUGGEST BEDROCK") || !strings.Contains(render, "don't rent a GPU") {
		t.Errorf("exact render should recommend routing away: %q", render)
	}
}

func TestOfferNearCarriesCaveatNoQualityClaim(t *testing.T) {
	r := Result{
		ModelRef: "meta-llama/Meta-Llama-3-70B-Instruct", Shape: ShapeInference,
		Tier: TierNear, Eligible: false, MatchID: "meta.llama3-8b-instruct-v1:0",
		DiffAxes: []string{"size: 70B vs 8B"}, Source: "hf-bedrock-map",
		Regions: []string{"us-east-1"},
	}
	o := r.Offer()
	if o == nil {
		t.Fatal("near+inference should surface a candidate offer")
	}
	if o.Exact {
		t.Error("near match must NOT be Exact (never auto-route)")
	}
	render := o.Render()
	if !strings.Contains(render, "CANDIDATE") || !strings.Contains(render, "verify equivalence") {
		t.Errorf("near render must be a labeled candidate, not a claim: %q", render)
	}
	if !strings.Contains(render, "size: 70B vs 8B") {
		t.Errorf("near render must carry the axis of difference: %q", render)
	}
	// Exact-match discipline: no wording that asserts equivalence/quality.
	if strings.Contains(strings.ToLower(render), "equivalent to") || strings.Contains(render, "SUGGEST BEDROCK") {
		t.Errorf("near render must make no quality/equivalence claim: %q", render)
	}
}

func TestNoOfferWhenNoRouteOrHidden(t *testing.T) {
	// No catalog route.
	if (Result{ModelRef: "foo/bar", Shape: ShapeInference, Tier: TierNone}).Offer() != nil {
		t.Error("TierNone must not produce an offer")
	}
	// Identity hidden behind a mount.
	if (Result{ModelRef: "", Shape: ShapeInference, Tier: TierExact, Eligible: true}).Offer() != nil {
		t.Error("hidden identity (no ref) must not produce an offer")
	}
	// Near match on a training shape: not an API-call candidate.
	if (Result{ModelRef: "foo/bar", Shape: ShapeTraining, Tier: TierNear}).Offer() != nil {
		t.Error("near match on a non-inference shape must not offer")
	}
}
