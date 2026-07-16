package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// hf-bedrock-map (github.com/scttfrdmn/hf-bedrock-map) is a daily-refreshed,
// curated + AWS-EULA-verified + HF-existence-validated mapping between Hugging
// Face repos and the models Amazon Bedrock can serve. It is the authoritative
// answer to the gate's core question — "is this HF model on Bedrock?" — and is
// the PRIMARY identity source (the signature heuristic in signature.go is the
// fallback when this is unreachable).
//
// As of v0.1.0 it exposes a versioned STATIC reverse-lookup API on GitHub Pages
// (no backend, CDN-cached): GET /api/v1/hf/{org}/{repo}.json returns 200 with a
// record when the repo is served by Bedrock, or 404 (a definitive "no"). We use
// the per-model endpoint because the gate asks about specific model refs one at a
// time; the versioned /v1/ path gives a stable-ish contract.
//
// Trade-off vs the older flat mapping.json: the API drops provider/modelName/
// hfUrl/evidence in favor of a lean HF-keyed record with per-Bedrock-path region
// info. We synthesize an evidence line from the record (§11 wants provenance).

// HFBedrockAPIBase is the versioned reverse-lookup API base (GitHub Pages, static).
const HFBedrockAPIBase = "https://scttfrdmn.github.io/hf-bedrock-map/api/v1"

// bedrockPath is one Bedrock route to an HF repo (a repo can be served by both a
// foundation-model and a marketplace entry).
type bedrockPath struct {
	ModelID    string   `json:"modelId"`
	Catalog    string   `json:"catalog"`    // "foundation-model" | "marketplace"
	Confidence string   `json:"confidence"` // confirmed|validated|ambiguous
	Regions    []string `json:"regions"`
}

// apiRecord is the per-model reverse-lookup response (GET /hf/{org}/{repo}.json).
type apiRecord struct {
	HFID      string        `json:"hfId"`      // canonical (original-case) HF id
	OnBedrock bool          `json:"onBedrock"` // true in any 200 body
	Regions   []string      `json:"regions"`   // union of serving US regions
	Bedrock   []bedrockPath `json:"bedrock"`   // every Bedrock path to this repo
}

// HFBedrockClient queries the reverse-lookup API. Results are cached in-process so
// a corpus with repeats hits the network at most once per distinct HF id.
type HFBedrockClient struct {
	Base        string
	HTTP        *http.Client
	Version     string // reported by index.json; informational
	GeneratedAt string // when the served data was last regenerated (index.json)

	cache map[string]HFMatch // key: lowercased hfId
}

// NewHFBedrockClient builds a client against the live v1 API. It probes
// index.json once for a freshness/version signal (best-effort; a failure here
// does not disable per-model lookups).
func NewHFBedrockClient(ctx context.Context, base string) (*HFBedrockClient, error) {
	if base == "" {
		base = HFBedrockAPIBase
	}
	c := &HFBedrockClient{
		Base:  strings.TrimSuffix(base, "/"),
		HTTP:  &http.Client{Timeout: 15 * time.Second},
		cache: map[string]HFMatch{},
	}
	// Freshness/version probe: confirms the API is reachable and pins the version
	// we're talking to for the report.
	ver, gen, err := c.probeIndex(ctx)
	if err != nil {
		return nil, fmt.Errorf("hf-bedrock-map API unreachable: %w", err)
	}
	c.Version, c.GeneratedAt = ver, gen
	return c, nil
}

func (c *HFBedrockClient) probeIndex(ctx context.Context) (version, generatedAt string, err error) {
	var idx struct {
		Version     string `json:"version"`
		GeneratedAt string `json:"generatedAt"`
		Count       int    `json:"count"`
	}
	if err := c.getJSON(ctx, c.Base+"/index.json", &idx); err != nil {
		return "", "", err
	}
	return idx.Version, idx.GeneratedAt, nil
}

// HFMatch is the gate's verdict for one HF repo id against hf-bedrock-map.
type HFMatch struct {
	Found          bool
	BedrockModelID string
	Catalog        string
	Confidence     string   // confirmed|validated|ambiguous
	Regions        []string // union of Bedrock regions serving the model
	Evidence       string   // synthesized provenance line
}

// Lookup resolves a HuggingFace repo id via the reverse-lookup API. The id is
// lowercased to build the URL (the API is case-insensitive; repo names keep their
// dots/dashes verbatim, so we only lowercase). A 404 is a clean "not on Bedrock".
// Results are cached per distinct id.
func (c *HFBedrockClient) Lookup(ctx context.Context, hfID string) HFMatch {
	key := strings.ToLower(strings.TrimSpace(hfID))
	if key == "" {
		return HFMatch{}
	}
	if m, ok := c.cache[key]; ok {
		return m
	}
	m := c.lookupUncached(ctx, key)
	c.cache[key] = m
	return m
}

func (c *HFBedrockClient) lookupUncached(ctx context.Context, lowerID string) HFMatch {
	url := c.Base + "/hf/" + lowerID + ".json"
	var rec apiRecord
	status, err := c.getJSONStatus(ctx, url, &rec)
	if err != nil || status == http.StatusNotFound || !rec.OnBedrock || len(rec.Bedrock) == 0 {
		return HFMatch{} // 404 or any read error => treat as "not served" (clean no)
	}
	// Pick the strongest Bedrock path (a repo may have both FM + marketplace routes).
	best := rec.Bedrock[0]
	for _, b := range rec.Bedrock[1:] {
		if confRank(b.Confidence) > confRank(best.Confidence) {
			best = b
		}
	}
	return HFMatch{
		Found:          true,
		BedrockModelID: best.ModelID,
		Catalog:        best.Catalog,
		Confidence:     best.Confidence,
		Regions:        rec.Regions,
		Evidence:       fmt.Sprintf("hf-bedrock-map v1: %s (%s, %s)", best.ModelID, best.Catalog, best.Confidence),
	}
}

// confRank orders confidence tiers for "best path" selection.
func confRank(c string) int {
	switch c {
	case "confirmed":
		return 4
	case "validated":
		return 3
	case "ambiguous":
		return 2
	case "proprietary":
		return 1
	default:
		return 0
	}
}

// Tier maps confidence to the gate's exact/near/none tiers (§11), preserving
// exact-match discipline: confirmed/validated (authoritative) -> exact; ambiguous
// (a variant is served, exact checkpoint unpinned) -> near, no quality claim;
// else none. (proprietary/unresolved never appear in the reverse API — they carry
// no HF id — so this is defensive.)
func (h HFMatch) Tier() Tier {
	if !h.Found {
		return TierNone
	}
	switch h.Confidence {
	case "confirmed", "validated":
		return TierExact
	case "ambiguous":
		return TierNear
	default:
		return TierNone
	}
}

// --- HTTP helpers ---

func (c *HFBedrockClient) getJSON(ctx context.Context, url string, v any) error {
	status, err := c.getJSONStatus(ctx, url, v)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, status)
	}
	return nil
}

// getJSONStatus GETs url and, on 200, decodes into v. Returns the HTTP status so
// callers can distinguish 404 (a real answer) from transport errors.
func (c *HFBedrockClient) getJSONStatus(ctx context.Context, url string, v any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s: %w", url, err)
	}
	return resp.StatusCode, nil
}
