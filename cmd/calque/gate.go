package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spore-host/calque/internal/gate"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// bedrockOffers runs the eligibility gate over an app and returns the EXACT
// replacement offers (§11, Gap G). This is the route-away short-circuit used by
// the runnable pipeline (run/real/session) BEFORE any GPU is acquired: if a unit
// self-hosts a model that's already an exact Bedrock API call, renting a GPU is
// the wrong answer. It mirrors analyze()'s gate wiring (main.go) — live catalog +
// authoritative hf-bedrock-map, both best-effort — but returns only the actionable
// exact offers rather than printing the full census.
//
// Best-effort by design: if neither identity source is reachable we return no
// offers and the caller proceeds to the GPU path (the gate must never block a run
// just because the network is down).
func bedrockOffers(ctx context.Context, app ir.App, rep *leak.Report) []*gate.ReplacementOffer {
	cat, err := gate.NewLiveCatalog(ctx, bedrockRegion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: Bedrock catalog unavailable (%v); skipping route-away gate\n", err)
		return nil
	}
	var hf gate.HFLookup
	if hm, herr := gate.NewHFBedrockClient(ctx, ""); herr == nil {
		hf = hm
	}
	results, err := gate.EvaluateWith(ctx, app, cat, hf, rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: route-away gate failed (%v); proceeding to GPU path\n", err)
		return nil
	}
	var offers []*gate.ReplacementOffer
	for _, r := range results {
		if o := r.Offer(); o != nil && o.Exact {
			offers = append(offers, o)
		}
	}
	return offers
}

// bedrockOffersForModel gates a bare HF model ref (the --model flag on real/
// session, which have no script to parse) by synthesizing a minimal inference
// unit around it and reusing the full gate. This enforces the "--model must NOT
// be on Bedrock" contract those commands document: if someone points a real GPU
// run at a model that's already a Bedrock API call, we catch it before acquiring.
func bedrockOffersForModel(ctx context.Context, modelRef string, rep *leak.Report) []*gate.ReplacementOffer {
	if modelRef == "" {
		return nil
	}
	app := ir.App{
		Script: "--model " + modelRef,
		Classes: []ir.Class{{
			Name: "cli-model", GPU: "H100",
			EnterBody: fmt.Sprintf("self.llm = LLM(model=%q)", modelRef),
			Methods:   []ir.Function{{Name: "generate", Body: "return self.llm.generate(prompt)"}},
		}},
	}
	return bedrockOffers(ctx, app, rep)
}

// printOffersAndStop renders exact offers and reports whether the caller should
// stop before acquiring a GPU. Returns true when there is at least one exact
// offer — the credibility short-circuit (§11): "call the API instead."
func printOffersAndStop(offers []*gate.ReplacementOffer) bool {
	if len(offers) == 0 {
		return false
	}
	fmt.Println("\n--- Bedrock route-away (§11) — NOT renting a GPU ---")
	for _, o := range offers {
		fmt.Printf("  %s", o.Render())
	}
	fmt.Println("This model is already an exact Bedrock API call; acquisition skipped.")
	return true
}
