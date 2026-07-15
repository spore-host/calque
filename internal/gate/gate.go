// Package gate implements the Bedrock eligibility gate (spec §11): a static,
// cheap, high-signal pass that runs BEFORE recommend. If a Modal script is
// self-hosting a model that's already in the Bedrock catalog and the usage is
// plain request-response inference, the honest answer is "don't rent a GPU — call
// the API." Routing work AWAY from calque is correct and earns the tool credibility.
//
// Two ANDed static checks (no execution required):
//  1. Identity — is the model in the LIVE Bedrock catalog? (fetched at analysis time)
//  2. Shape    — is usage plain inference (vs training / fine-tune / custom checkpoint)?
//
// Exact-match discipline is the credibility floor: never silently round a custom
// checkpoint to a catalog entry. A skeptic asking "did it claim my model was on
// Bedrock?" must get a clean answer.
package gate

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// Catalog is the live Bedrock model catalog, behind an interface so the gate is
// unit-testable offline and the region/credentials concern stays at the edge.
type Catalog interface {
	// Models returns the catalog entries. Implementations fetch live (§11: "do not
	// hardcode a snapshot — it's region-gated and moves").
	Models(ctx context.Context) ([]CatalogEntry, error)
}

// CatalogEntry is the subset of a Bedrock model summary the gate needs.
type CatalogEntry struct {
	ModelID       string
	ModelName     string
	Provider      string
	InputModal    []string // e.g. ["TEXT"]
	OutputModal   []string
	ResponseTypes []string // e.g. ["ON_DEMAND"] — API-callable
}

// Shape is the usage-shape verdict for a script (check #2).
type Shape string

const (
	ShapeInference Shape = "inference" // .map over prompts / serve calling generate
	ShapeTraining  Shape = "training"  // training loop / fine-tune / custom-checkpoint enter
	ShapeUnknown   Shape = "unknown"   // no model identity found (e.g. weights behind a Volume)
)

// Result is the gate's verdict for one script.
type Result struct {
	Script   string
	ModelRef string // extracted model reference, or "" if none found
	Shape    Shape
	Tier     Tier     // best match tier across the catalog
	MatchID  string   // catalog modelId of the best match, if any
	DiffAxes []string // for near matches: labeled axes of difference (no quality claim)
	Eligible bool     // exact identity AND inference shape -> auto-suggest Bedrock, stop
}

// trainingSignals in a body/enter indicate NOT plain inference (check #2).
var trainingRe = regexp.MustCompile(`(?i)(\.backward\(|\.train\(\)|optimizer|loss\.|autocast|\bfit\(|Trainer\(|peft|lora|get_peft_model|prepare_model_for|save_pretrained|training_args|gradient|\.step\(\)\s*#?.*optim)`)

// inferenceSignals indicate plain request-response inference.
var inferenceRe = regexp.MustCompile(`(?i)(\.generate\(|\.embed|\.predict\(|\.encode\(|pipeline\(|no_grad|inference_mode|SamplingParams|\.forward\()`)

// modelRefRe extracts a model reference from a verbatim body. We scan text (not
// parse) for the common load idioms; §11 permits "pure AST + one catalog fetch",
// and a targeted text scan for the model id is the AST-equivalent here.
var modelRefRe = regexp.MustCompile(`(?i)(?:from_pretrained|LLM|AutoModel(?:\w*)|SentenceTransformer|pipeline|from_name)\s*\(\s*(?:model\s*=\s*)?["']([^"']+)["']`)

// ExtractModelRef finds the first model reference in the class's enter+methods or
// a function body. Returns "" if none (e.g. weights loaded from a mount path).
func extractModelRef(bodies ...string) string {
	for _, b := range bodies {
		if m := modelRefRe.FindStringSubmatch(b); m != nil {
			ref := strings.TrimSpace(m[1])
			// A filesystem/mount path (e.g. "/weights") is not an identity — the
			// model provenance is obscured behind a Volume. Skip it; caller leaks it.
			if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") {
				continue
			}
			return ref
		}
	}
	return ""
}

func detectShape(bodies ...string) Shape {
	joined := strings.Join(bodies, "\n")
	train := trainingRe.MatchString(joined)
	infer := inferenceRe.MatchString(joined)
	switch {
	case train:
		return ShapeTraining // training signal dominates: never route to a serve API
	case infer:
		return ShapeInference
	default:
		return ShapeUnknown
	}
}

// Evaluate runs the gate over an app. It fetches the catalog once and evaluates
// each GPU-bearing unit (class or function). Emits leaks for the honest gaps
// (identity behind a mount, unknown shape). Returns one Result per unit.
func Evaluate(ctx context.Context, app ir.App, cat Catalog, rep *leak.Report) ([]Result, error) {
	entries, err := cat.Models(ctx)
	if err != nil {
		return nil, err
	}
	catSigs := make([]struct {
		sig   Signature
		entry CatalogEntry
	}, 0, len(entries))
	for _, e := range entries {
		catSigs = append(catSigs, struct {
			sig   Signature
			entry CatalogEntry
		}{NormalizeBedrock(e.ModelID, e.Provider), e})
	}

	var results []Result

	evalUnit := func(name string, line int, bodies ...string) {
		ref := extractModelRef(bodies...)
		shape := detectShape(bodies...)
		res := Result{Script: app.Script, ModelRef: ref, Shape: shape, Tier: TierNone}

		if ref == "" {
			// Common in practice: map_batch loads LLM(model="/weights"). Identity is
			// obscured behind the Volume, so the gate CANNOT make a claim (§11 floor).
			rep.Addf(leak.PrimVolume, leak.KindSemanticGap, app.Script, line,
				"%s: model identity obscured (loaded from a path/mount, not a repo id); Bedrock identity check cannot run", name)
			results = append(results, res)
			return
		}

		scriptSig := NormalizeHF(ref)
		best := TierNone
		var bestDiffs []string
		var bestID string
		for _, cs := range catSigs {
			tier, diffs := Compare(scriptSig, cs.sig)
			if tierRank(tier) > tierRank(best) {
				best, bestDiffs, bestID = tier, diffs, cs.entry.ModelID
				if best == TierExact {
					break
				}
			}
		}
		res.Tier, res.DiffAxes, res.MatchID = best, bestDiffs, bestID

		// Eligible ONLY on exact identity AND inference shape (both ANDed checks pass).
		res.Eligible = best == TierExact && shape == ShapeInference

		switch {
		case best == TierExact && shape == ShapeTraining:
			// Identity hit but training shape: correctly NOT eligible. Worth noting —
			// the model exists on Bedrock but the usage (fine-tune) needs the GPU.
			rep.Addf(leak.PrimEnter, leak.KindSemanticGap, app.Script, line,
				"%s: %q matches Bedrock %s but usage is training/fine-tune; NOT an API call", name, ref, bestID)
		case best == TierNone && shape == ShapeInference:
			// Self-hosted inference of a model not on Bedrock: legitimately calque's job.
		case shape == ShapeUnknown:
			rep.Addf(leak.PrimEntrypoint, leak.KindUnhandledCase, app.Script, line,
				"%s: usage shape indeterminate (neither clear inference nor training signal)", name)
		}
		results = append(results, res)
	}

	for _, c := range app.Classes {
		bodies := []string{c.EnterBody}
		for _, m := range c.Methods {
			bodies = append(bodies, m.Body)
		}
		evalUnit(c.Name, c.Line, bodies...)
	}
	for _, f := range app.Functions {
		// Functions with no gpu= are plumbing (e.g. download_weights); skip them.
		if f.GPU == "" {
			continue
		}
		evalUnit(f.Name, f.Line, f.Body)
	}
	return results, nil
}

func tierRank(t Tier) int {
	switch t {
	case TierExact:
		return 2
	case TierNear:
		return 1
	default:
		return 0
	}
}

// Census summarizes gate results across a corpus (spec §11: emit the
// Bedrock-eligible count — "cheapest damning number in the instrument").
type Census struct {
	Units          int `json:"units"`            // GPU-bearing units evaluated
	BedrockExact   int `json:"bedrock_exact"`    // exact identity + inference -> eligible
	BedrockNear    int `json:"bedrock_near"`     // near match, offered with labeled diffs
	SelfHostedOnly int `json:"self_hosted_only"` // no catalog match; legitimately calque's job
	IdentityHidden int `json:"identity_hidden"`  // model ref obscured (behind a mount)
}

func Summarize(results []Result) Census {
	c := Census{Units: len(results)}
	for _, r := range results {
		switch {
		case r.ModelRef == "":
			c.IdentityHidden++
		case r.Eligible:
			c.BedrockExact++
		case r.Tier == TierNear:
			c.BedrockNear++
		default:
			c.SelfHostedOnly++
		}
	}
	return c
}

// Sort orders results deterministically for stable reporting.
func Sort(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Script != results[j].Script {
			return results[i].Script < results[j].Script
		}
		return results[i].ModelRef < results[j].ModelRef
	})
}
