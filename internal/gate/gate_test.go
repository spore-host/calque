package gate

import (
	"context"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// fakeCatalog is an offline stand-in for the live Bedrock catalog.
type fakeCatalog struct{ entries []CatalogEntry }

func (f fakeCatalog) Models(_ context.Context) ([]CatalogEntry, error) { return f.entries, nil }

var testCatalog = fakeCatalog{entries: []CatalogEntry{
	{ModelID: "meta.llama3-8b-instruct-v1:0", Provider: "Meta"},
	{ModelID: "meta.llama3-70b-instruct-v1:0", Provider: "Meta"},
	{ModelID: "mistral.mistral-7b-instruct-v0:2", Provider: "Mistral AI"},
}}

func TestCompareTiers(t *testing.T) {
	cases := []struct {
		name   string
		hf     string
		bedID  string
		bedPrv string
		want   Tier
	}{
		{"exact llama3-8b-instruct", "meta-llama/Meta-Llama-3-8B-Instruct", "meta.llama3-8b-instruct-v1:0", "Meta", TierExact},
		{"near: size differs", "meta-llama/Meta-Llama-3-70B-Instruct", "meta.llama3-8b-instruct-v1:0", "Meta", TierNear},
		{"near: variant differs (base vs instruct)", "meta-llama/Meta-Llama-3-8B", "meta.llama3-8b-instruct-v1:0", "Meta", TierNear},
		{"none: different provider", "mistralai/Mistral-7B-Instruct-v0.2", "meta.llama3-8b-instruct-v1:0", "Meta", TierNone},
		{"none: different family", "BAAI/bge-large-en-v1.5", "meta.llama3-8b-instruct-v1:0", "Meta", TierNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := Compare(NormalizeHF(c.hf), NormalizeBedrock(c.bedID, c.bedPrv))
			if got != c.want {
				t.Errorf("Compare(%q, %q) = %s, want %s", c.hf, c.bedID, got, c.want)
			}
		})
	}
}

// TestExactMatchDiscipline is the credibility floor (§11): a custom checkpoint or
// a fine-tune must NEVER be silently rounded to a catalog entry as exact.
func TestExactMatchDiscipline(t *testing.T) {
	// A fine-tune of llama3-8b, distinct repo. Must not be TierExact.
	tier, diffs := Compare(
		NormalizeHF("my-org/llama3-8b-instruct-finetuned-legal"),
		NormalizeBedrock("meta.llama3-8b-instruct-v1:0", "Meta"),
	)
	if tier == TierExact {
		t.Errorf("fine-tune matched EXACT — credibility floor breached (diffs=%v)", diffs)
	}
	// provider folds to my-org != meta, so this is actually None — the strongest
	// possible non-claim, which is correct.
	if tier != TierNone {
		t.Logf("fine-tune tier = %s (acceptable as long as not exact)", tier)
	}
}

func TestShapeDetection(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Shape
	}{
		{"vllm generate", "out = self.llm.generate([prompt], self.params)", ShapeInference},
		{"embed no_grad", "with torch.no_grad():\n  return self.model(**batch)", ShapeInference},
		{"training loss.backward", "loss = criterion(out, y); loss.backward(); optimizer.step()", ShapeTraining},
		{"peft finetune", "model = get_peft_model(base, lora_config)", ShapeTraining},
		{"opaque", "return image_id % 1000", ShapeUnknown},
	}
	for _, c := range cases {
		if got := detectShape(c.body); got != c.want {
			t.Errorf("detectShape(%q) = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestExtractModelRef(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`self.model = AutoModel.from_pretrained("BAAI/bge-large-en-v1.5")`, "BAAI/bge-large-en-v1.5"},
		{`self.llm = LLM(model="meta-llama/Meta-Llama-3-8B")`, "meta-llama/Meta-Llama-3-8B"},
		{`self.llm = LLM(model="/weights")`, ""}, // mount path -> not an identity
		{`return x * 2`, ""},
	}
	for _, c := range cases {
		if got := extractModelRef(c.body); got != c.want {
			t.Errorf("extractModelRef(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

// TestEvaluateEligibility wires the whole gate over synthetic units with a fake
// catalog: exact+inference is eligible; identity-behind-mount is a leak, not a claim.
func TestEvaluateEligibility(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{
		Script: "test.py",
		Classes: []ir.Class{
			{ // exact identity + inference -> eligible
				Name: "Server", GPU: "H100", Line: 1,
				EnterBody: `self.llm = LLM(model="meta-llama/Meta-Llama-3-8B-Instruct")`,
				Methods:   []ir.Function{{Name: "gen", Body: "return self.llm.generate(p)"}},
			},
			{ // identity behind a mount -> no claim + leak
				Name: "Hidden", GPU: "A100", Line: 10,
				EnterBody: `self.llm = LLM(model="/weights")`,
				Methods:   []ir.Function{{Name: "gen", Body: "return self.llm.generate(p)"}},
			},
		},
	}
	results, err := Evaluate(context.Background(), app, testCatalog, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	Sort(results)
	// "Hidden" sorts first (ModelRef "" < "meta-llama/...").
	if results[0].ModelRef != "" || results[0].Eligible {
		t.Errorf("hidden-identity unit should be ineligible with no ref, got %+v", results[0])
	}
	if !results[1].Eligible || results[1].Tier != TierExact {
		t.Errorf("server unit should be eligible+exact, got %+v", results[1])
	}
	c := Summarize(results)
	if c.BedrockExact != 1 || c.IdentityHidden != 1 {
		t.Errorf("census = %+v, want 1 exact + 1 hidden", c)
	}
	if rep.Len() == 0 {
		t.Error("expected a leak for the hidden-identity model")
	}
}
