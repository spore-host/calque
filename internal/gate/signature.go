package gate

import (
	"regexp"
	"strings"
)

// Signature is a normalized, comparable fingerprint of a model reference, derived
// from either a HuggingFace repo ID (what Modal scripts use) or a Bedrock model ID
// + provider (what the catalog exposes).
//
// This normalization is the ONLY bridge between the two namespaces: Bedrock
// carries no HuggingFace provenance, so a "match" is a signature comparison, NOT
// proof of identical weights. §11's credibility floor requires we never present a
// signature match as a hard identity claim — the gate labels tiers accordingly.
type Signature struct {
	Provider string // "meta", "mistral", "amazon", "anthropic", ...
	Family   string // "llama", "mistral", "titan", "bge", ...
	Version  string // "3", "3.1", "2", "" if none
	Size     string // normalized size token, e.g. "8b", "70b", "" if none
	Variant  string // "instruct" | "chat" | "base" | ""
	Raw      string // the original string, for reporting
}

var (
	sizeRe    = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*b\b`) // 8b, 70b, 3.5b
	versionRe = regexp.MustCompile(`(?i)(?:llama|mistral|mixtral|qwen|gemma|phi|titan)[-\s]?(\d+(?:[-.]\d+)?)`)
)

// providerAliases folds HF org names and Bedrock provider names to one token.
var providerAliases = map[string]string{
	"meta-llama": "meta", "meta": "meta",
	"mistralai": "mistral", "mistral ai": "mistral", "mistral": "mistral",
	"amazon": "amazon", "aws": "amazon",
	"anthropic": "anthropic",
	"cohere":    "cohere",
	"google":    "google", "deepseek-ai": "deepseek", "deepseek": "deepseek",
	"qwen": "qwen", "baai": "baai", "stabilityai": "stability", "stability ai": "stability",
	"tiiuae": "tii", "microsoft": "microsoft", "openai": "openai",
}

// knownFamilies are the family tokens we recognize inside a model name.
var knownFamilies = []string{
	"llama", "mixtral", "mistral", "ministral", "magistral", "devstral", "voxtral",
	"titan", "nova", "command", "embed", "bge", "gemma", "qwen", "phi", "falcon",
	"deepseek", "stable-diffusion", "sdxl", "jamba",
}

func foldProvider(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if v, ok := providerAliases[s]; ok {
		return v
	}
	return s
}

// NormalizeHF turns a HuggingFace repo ID ("meta-llama/Meta-Llama-3-8B") into a
// Signature. The org before "/" is the provider; the model name is tokenized.
func NormalizeHF(repo string) Signature {
	repo = strings.TrimSpace(repo)
	sig := Signature{Raw: repo}
	name := repo
	if i := strings.Index(repo, "/"); i >= 0 {
		sig.Provider = foldProvider(repo[:i])
		name = repo[i+1:]
	}
	fillFromName(&sig, name)
	return sig
}

// NormalizeBedrock turns a Bedrock (modelId, providerName) pair into a Signature.
// modelId looks like "meta.llama3-8b-instruct-v1:0"; the leading "provider." and
// trailing ":0" version suffix are stripped before tokenizing.
func NormalizeBedrock(modelID, providerName string) Signature {
	sig := Signature{Raw: modelID}
	sig.Provider = foldProvider(providerName)
	name := modelID
	if i := strings.Index(name, "."); i >= 0 {
		if sig.Provider == "" {
			sig.Provider = foldProvider(name[:i])
		}
		name = name[i+1:]
	}
	if i := strings.Index(name, ":"); i >= 0 { // drop ":0" catalog version
		name = name[:i]
	}
	name = strings.TrimSuffix(name, "-v1")
	fillFromName(&sig, name)
	return sig
}

func fillFromName(sig *Signature, name string) {
	lower := strings.ToLower(name)
	for _, f := range knownFamilies {
		if strings.Contains(lower, f) {
			sig.Family = f
			break
		}
	}
	if m := versionRe.FindStringSubmatch(lower); m != nil {
		sig.Version = strings.ReplaceAll(m[1], "-", ".")
	}
	if m := sizeRe.FindStringSubmatch(lower); m != nil {
		sig.Size = strings.ToLower(m[1]) + "b"
	}
	switch {
	case strings.Contains(lower, "instruct"):
		sig.Variant = "instruct"
	case strings.Contains(lower, "chat"):
		sig.Variant = "chat"
	default:
		sig.Variant = "base"
	}
}

// Tier is the match strength between a script's model and a catalog entry (§11).
type Tier string

const (
	TierExact Tier = "exact" // provider+family+version+size+variant all agree
	TierNear  Tier = "near"  // provider+family agree; some axis differs (labeled)
	TierNone  Tier = "none"  // no provider+family agreement
)

// Compare returns the tier and, for near matches, the axes of difference — so the
// offer can be "labeled by axis of difference" with NO quality claim (§11).
func Compare(script, cat Signature) (Tier, []string) {
	if script.Provider == "" || cat.Provider == "" ||
		script.Provider != cat.Provider || script.Family != cat.Family {
		return TierNone, nil
	}
	var diffs []string
	if script.Version != cat.Version {
		diffs = append(diffs, "version ("+orNone(script.Version)+" vs "+orNone(cat.Version)+")")
	}
	if script.Size != cat.Size {
		diffs = append(diffs, "size ("+orNone(script.Size)+" vs "+orNone(cat.Size)+")")
	}
	if script.Variant != cat.Variant {
		diffs = append(diffs, "variant ("+script.Variant+" vs "+cat.Variant+")")
	}
	if len(diffs) == 0 {
		return TierExact, nil
	}
	return TierNear, diffs
}

func orNone(s string) string {
	if s == "" {
		return "unspecified"
	}
	return s
}
