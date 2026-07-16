package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// hfServer stands up a fake reverse-lookup API mirroring the live v1 shape, so
// the client is tested offline against real HTTP behavior (200 record / 404 miss).
func hfServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"v1","generatedAt":"2026-07-16T07:37:42Z","count":2}`))
	})
	mux.HandleFunc("/api/v1/hf/meta-llama/meta-llama-3-8b-instruct.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"hfId":"meta-llama/Meta-Llama-3-8B-Instruct","onBedrock":true,
			"regions":["us-east-1","us-west-2"],
			"bedrock":[{"modelId":"meta.llama3-8b-instruct-v1:0","catalog":"foundation-model","confidence":"confirmed","regions":["us-east-1","us-west-2"]}]}`))
	})
	mux.HandleFunc("/api/v1/hf/qwen/qwen3-32b.json", func(w http.ResponseWriter, r *http.Request) {
		// two Bedrock paths — the confirmed FM should win over the marketplace one.
		w.Write([]byte(`{"hfId":"Qwen/Qwen3-32B","onBedrock":true,"regions":["us-west-2"],
			"bedrock":[
			  {"modelId":"huggingface-reasoning-qwen3-32b","catalog":"marketplace","confidence":"validated","regions":["us-west-2"]},
			  {"modelId":"qwen.qwen3-32b-v1:0","catalog":"foundation-model","confidence":"confirmed","regions":["us-west-2"]}]}`))
	})
	// everything else -> 404 (the API's definitive "not on Bedrock")
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	return httptest.NewServer(mux)
}

func newTestClient(t *testing.T, srv *httptest.Server) *HFBedrockClient {
	t.Helper()
	c, err := NewHFBedrockClient(context.Background(), srv.URL+"/api/v1")
	if err != nil {
		t.Fatalf("NewHFBedrockClient: %v", err)
	}
	return c
}

func TestClientLookup(t *testing.T) {
	srv := hfServer(t)
	defer srv.Close()
	c := newTestClient(t, srv)
	if c.Version != "v1" {
		t.Errorf("version = %q, want v1", c.Version)
	}

	// confirmed FM -> exact, carries the bedrock id + regions.
	got := c.Lookup(context.Background(), "meta-llama/Meta-Llama-3-8B-Instruct")
	if !got.Found || got.Tier() != TierExact || got.BedrockModelID != "meta.llama3-8b-instruct-v1:0" {
		t.Errorf("llama3 = %+v, want found/exact/meta.llama3-8b-instruct-v1:0", got)
	}
	if len(got.Regions) != 2 {
		t.Errorf("regions = %v, want 2", got.Regions)
	}

	// case-insensitive URL building.
	if g := c.Lookup(context.Background(), "META-LLAMA/Meta-Llama-3-8B-Instruct"); !g.Found {
		t.Error("lookup should be case-insensitive")
	}

	// two paths -> the confirmed FM wins over the validated marketplace one.
	q := c.Lookup(context.Background(), "Qwen/Qwen3-32B")
	if q.BedrockModelID != "qwen.qwen3-32b-v1:0" || q.Confidence != "confirmed" {
		t.Errorf("qwen best path = %+v, want the confirmed FM", q)
	}

	// 404 -> clean not-found.
	if m := c.Lookup(context.Background(), "BAAI/bge-large-en-v1.5"); m.Found {
		t.Errorf("bge should be a clean 404 miss, got %+v", m)
	}
}

func TestClientCaches(t *testing.T) {
	srv := hfServer(t)
	defer srv.Close()
	hits := 0
	// wrap to count network calls
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/hf/") {
			hits++
		}
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer wrapped.Close()
	c := newTestClient(t, wrapped)
	for i := 0; i < 3; i++ {
		c.Lookup(context.Background(), "meta-llama/Meta-Llama-3-8B-Instruct")
	}
	if hits != 1 {
		t.Errorf("expected 1 network hit for 3 identical lookups, got %d", hits)
	}
}

// TestEvaluateWithHFClient: the client drives eligibility end-to-end via the gate.
func TestEvaluateWithHFClient(t *testing.T) {
	srv := hfServer(t)
	defer srv.Close()
	c := newTestClient(t, srv)
	rep := &leak.Report{}
	app := ir.App{
		Script: "test.py",
		Classes: []ir.Class{
			{Name: "Server", GPU: "H100", Line: 1,
				EnterBody: `self.llm = LLM(model="meta-llama/Meta-Llama-3-8B-Instruct")`,
				Methods:   []ir.Function{{Name: "gen", Body: "return self.llm.generate(p)"}}},
			{Name: "Custom", GPU: "A100", Line: 10,
				EnterBody: `self.m = AutoModel.from_pretrained("BAAI/bge-large-en-v1.5")`,
				Methods:   []ir.Function{{Name: "embed", Body: "return self.m(x)"}}},
		},
	}
	results, err := EvaluateWith(context.Background(), app, testCatalog, c, rep)
	if err != nil {
		t.Fatal(err)
	}
	Sort(results)
	var server, custom Result
	for _, r := range results {
		switch r.ModelRef {
		case "meta-llama/Meta-Llama-3-8B-Instruct":
			server = r
		case "BAAI/bge-large-en-v1.5":
			custom = r
		}
	}
	if !server.Eligible || server.Source != "hf-bedrock-map" || server.MatchID != "meta.llama3-8b-instruct-v1:0" {
		t.Errorf("server should be eligible via hf-bedrock-map, got %+v", server)
	}
	if custom.Eligible || custom.Tier != TierNone {
		t.Errorf("custom (not on Bedrock) should be ineligible, got %+v", custom)
	}
}
