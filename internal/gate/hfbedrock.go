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
// answer to the gate's core question — "is this HF model on Bedrock?" — and
// replaces the hand-rolled signature heuristic (signature.go) as the PRIMARY
// identity source when reachable.
//
// It ships as a static JSON artifact (no Go library), so we fetch + cache it.
// URL: https://scttfrdmn.github.io/hf-bedrock-map/mapping.json

// HFBedrockURL is the published, daily-refreshed mapping artifact.
const HFBedrockURL = "https://scttfrdmn.github.io/hf-bedrock-map/mapping.json"

// hfEntry is one row of mapping.json (schema pinned defensively — the artifact
// has no documented version contract, so every field is optional-tolerant).
type hfEntry struct {
	BedrockModelID string   `json:"bedrockModelId"`
	Catalog        string   `json:"catalog"` // "foundation-model" | "marketplace"
	Provider       string   `json:"provider"`
	ModelName      string   `json:"modelName"`
	HFID           string   `json:"hfId"`
	HFURL          string   `json:"hfUrl"`
	Confidence     string   `json:"confidence"` // confirmed|validated|ambiguous|proprietary|unresolved
	Regions        []string `json:"regions"`
	Evidence       string   `json:"evidence"`
}

type hfMapping struct {
	GeneratedAt string         `json:"generatedAt"`
	Regions     []string       `json:"regions"`
	Counts      map[string]int `json:"counts"`
	Entries     []hfEntry      `json:"entries"`
}

// HFBedrockMap is an indexed, HF-keyed view of the mapping. Multiple entries can
// share one hfId (e.g. a native FM row + a marketplace row) — the source
// intentionally doesn't dedup, so we keep them all.
type HFBedrockMap struct {
	GeneratedAt string
	byHF        map[string][]hfEntry // key: lowercased hfId
}

// FetchHFBedrockMap pulls and indexes the live mapping. Fetching is the consumer's
// only integration surface (no library). Caller should cache the result — it
// changes at most daily.
func FetchHFBedrockMap(ctx context.Context, url string) (*HFBedrockMap, error) {
	if url == "" {
		url = HFBedrockURL
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch hf-bedrock-map: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch hf-bedrock-map: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseHFBedrockMap(body)
}

func parseHFBedrockMap(body []byte) (*HFBedrockMap, error) {
	var m hfMapping
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse hf-bedrock-map: %w", err)
	}
	idx := &HFBedrockMap{GeneratedAt: m.GeneratedAt, byHF: map[string][]hfEntry{}}
	for _, e := range m.Entries {
		if e.HFID == "" {
			continue // ambiguous/proprietary rows may carry no hfId; not HF-keyable
		}
		k := strings.ToLower(e.HFID)
		idx.byHF[k] = append(idx.byHF[k], e)
	}
	return idx, nil
}

// HFMatch is the gate's verdict for one HF repo id against hf-bedrock-map.
type HFMatch struct {
	Found          bool
	BedrockModelID string
	Provider       string
	Confidence     string   // confirmed|validated|ambiguous
	Regions        []string // per-entry Bedrock region availability
	Evidence       string
}

// Lookup resolves a HuggingFace repo id to its best Bedrock mapping. Case-
// insensitive (HF ids are canonical-cased in the source; scripts may differ).
// When multiple entries share the hfId, the highest-confidence one wins.
func (m *HFBedrockMap) Lookup(hfID string) HFMatch {
	entries := m.byHF[strings.ToLower(strings.TrimSpace(hfID))]
	if len(entries) == 0 {
		return HFMatch{}
	}
	best := entries[0]
	for _, e := range entries[1:] {
		if confRank(e.Confidence) > confRank(best.Confidence) {
			best = e
		}
	}
	return HFMatch{
		Found: true, BedrockModelID: best.BedrockModelID, Provider: best.Provider,
		Confidence: best.Confidence, Regions: best.Regions, Evidence: best.Evidence,
	}
}

// confRank orders confidence tiers for "best match" selection. confirmed and
// validated are both "Bedrock serves this"; ambiguous is weaker; proprietary /
// unresolved are effectively non-matches for the self-hosting question.
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
	default: // unresolved / unknown
		return 0
	}
}

// Tier maps an hf-bedrock-map confidence to the gate's exact/near/none tiers (§11),
// preserving exact-match discipline: only confirmed/validated (authoritative,
// AWS-EULA-linked or HF-existence-validated) count as an eligible exact hit.
// ambiguous -> near (a variant is served, exact checkpoint unpinned; no quality
// claim). Everything else -> none.
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
