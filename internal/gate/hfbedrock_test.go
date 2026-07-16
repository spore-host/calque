package gate

import (
	"context"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// A trimmed fixture mirroring the live mapping.json shape (per-entry regions,
// confidence tiers). Kept offline so the test is hermetic.
var hfFixture = []byte(`{
  "generatedAt": "2026-07-16T07:27:40Z",
  "regions": ["us-east-1","us-east-2","us-west-2"],
  "counts": {"total": 4},
  "entries": [
    {"bedrockModelId":"meta.llama3-8b-instruct-v1:0","catalog":"foundation-model","provider":"Meta",
     "hfId":"meta-llama/Meta-Llama-3-8B-Instruct","confidence":"confirmed",
     "regions":["us-east-1","us-west-2"],"evidence":"curated override (verified)"},
    {"bedrockModelId":"google.gemma-3-12b-it","catalog":"foundation-model","provider":"Google",
     "hfId":"google/gemma-3-12b-it","confidence":"validated","regions":["us-west-2"],
     "evidence":"hf-validated: base repo exists"},
    {"bedrockModelId":"mistral.ministral-3-3b-instruct","catalog":"foundation-model","provider":"Mistral AI",
     "confidence":"ambiguous","regions":["us-east-1"],
     "evidence":"multiple HF variants, modelId can't disambiguate"},
    {"bedrockModelId":"deepseek.r1-v1:0","catalog":"foundation-model","provider":"DeepSeek",
     "hfId":"deepseek-ai/DeepSeek-R1","confidence":"confirmed","regions":["us-west-2"],
     "evidence":"model card EULA link"}
  ]
}`)

func TestParseAndLookup(t *testing.T) {
	m, err := parseHFBedrockMap(hfFixture)
	if err != nil {
		t.Fatal(err)
	}
	// ambiguous entry has no hfId -> not HF-keyable; the 3 with hfId are indexed.
	if got := m.Lookup("meta-llama/Meta-Llama-3-8B-Instruct"); !got.Found || got.Tier() != TierExact {
		t.Errorf("llama3 lookup = %+v, want found+exact", got)
	}
	if got := m.Lookup("google/gemma-3-12b-it"); got.Tier() != TierExact {
		t.Errorf("gemma (validated) should be exact, got %s", got.Confidence)
	}
	// case-insensitive: a script may lower-case the repo id.
	if got := m.Lookup("META-LLAMA/meta-llama-3-8b-INSTRUCT"); !got.Found {
		t.Error("lookup should be case-insensitive")
	}
	// a model not on Bedrock -> not found.
	if got := m.Lookup("BAAI/bge-large-en-v1.5"); got.Found {
		t.Errorf("bge should not be found, got %+v", got)
	}
	// regions carry through.
	if got := m.Lookup("deepseek-ai/DeepSeek-R1"); len(got.Regions) == 0 || got.BedrockModelID != "deepseek.r1-v1:0" {
		t.Errorf("deepseek lookup missing regions/id: %+v", got)
	}
}

// TestEvaluateWithHFMap: the authoritative source drives eligibility, records the
// evidence + source, and preserves exact-match discipline.
func TestEvaluateWithHFMap(t *testing.T) {
	m, _ := parseHFBedrockMap(hfFixture)
	rep := &leak.Report{}
	app := ir.App{
		Script: "test.py",
		Classes: []ir.Class{
			{ // confirmed on Bedrock + inference -> eligible via hf-bedrock-map
				Name: "Server", GPU: "H100", Line: 1,
				EnterBody: `self.llm = LLM(model="meta-llama/Meta-Llama-3-8B-Instruct")`,
				Methods:   []ir.Function{{Name: "gen", Body: "return self.llm.generate(p)"}},
			},
			{ // a model NOT on Bedrock -> legitimately calque's job
				Name: "Custom", GPU: "A100", Line: 10,
				EnterBody: `self.m = AutoModel.from_pretrained("BAAI/bge-large-en-v1.5")`,
				Methods:   []ir.Function{{Name: "embed", Body: "return self.m(x)"}},
			},
		},
	}
	// testCatalog (from gate_test.go) stands in for the live Bedrock catalog fallback.
	results, err := EvaluateWith(context.Background(), app, testCatalog, m, rep)
	if err != nil {
		t.Fatal(err)
	}
	Sort(results)
	// "BAAI/..." sorts before "meta-llama/..."
	var server, custom Result
	for _, r := range results {
		switch r.ModelRef {
		case "meta-llama/Meta-Llama-3-8B-Instruct":
			server = r
		case "BAAI/bge-large-en-v1.5":
			custom = r
		}
	}
	if !server.Eligible || server.Tier != TierExact {
		t.Errorf("server should be eligible+exact, got %+v", server)
	}
	if server.Source != "hf-bedrock-map" {
		t.Errorf("server match should come from hf-bedrock-map, got %q", server.Source)
	}
	if server.MatchID != "meta.llama3-8b-instruct-v1:0" || server.Evidence == "" {
		t.Errorf("server should carry bedrock id + evidence, got id=%q ev=%q", server.MatchID, server.Evidence)
	}
	if custom.Eligible || custom.Tier != TierNone {
		t.Errorf("custom (not on Bedrock) should be ineligible/none, got %+v", custom)
	}
}
