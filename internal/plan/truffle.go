// Package plan turns an IR + Target into an AWS execution plan (spec §5): it
// calls truffle to resolve a GPU card into candidate instance types (and to price
// the chosen one), then hands a single resolved target to acquisition.
//
// We call truffle rather than hardcoding the card->instance map, so the seam
// (§4) holds: the card name is never inlined into a code generator. Where the
// real truffle API differs from the spec's implied shape, we follow the source
// and log the delta as a leak (§5/§10).
package plan

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	truffleaws "github.com/spore-host/truffle/pkg/aws"
	"github.com/spore-host/truffle/pkg/find"

	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/target"
)

// Candidate is one resolved instance type for a card, optionally priced.
type Candidate struct {
	Instance   string  // e.g. "g7e.2xlarge"
	Family     string  // e.g. "g7e"
	Region     string  // set when priced/searched live
	PriceUSDHr float64 // on-demand $/hr; 0 if not looked up
	Priced     bool    // did a live price lookup succeed?
}

// Resolver turns a card name into candidate instances. Backed by truffle; the
// interface keeps plan testable offline and lets a fake stand in.
type Resolver interface {
	Resolve(card string) ([]Candidate, error)
}

// Pricer looks up a live on-demand rate for an instance in a region. Backed by
// truffle's (*aws.Client).OnDemandPrice — so calque gets R_a THROUGH truffle
// rather than calling the AWS Pricing API itself (truffle owns instance+pricing).
type Pricer interface {
	OnDemandPrice(ctx context.Context, instanceType, region string) (float64, error)
}

// TruffleResolver is the offline card->candidates resolver (no AWS creds needed;
// truffle's ResolveCard reads a static catalog).
type TruffleResolver struct {
	rep *leak.Report
}

func NewTruffleResolver(rep *leak.Report) *TruffleResolver { return &TruffleResolver{rep: rep} }

// Resolve maps a card name to candidate instance types via truffle's strict
// find.ResolveCard, which returns find.ErrNoMatch rather than falling back to
// a `.*` match-all pattern (truffle#90). ResolveCard is card-oriented and
// never match-all by construction, so no separate guard is needed here.
//
// Before giving up on an ErrNoMatch, it tries normalizeMemorySuffix (calque#134):
// Modal's documented gpu="A100-80GB"/"A100-40GB" spelling doesn't resolve via
// truffle today (truffle#130 tracks the upstream fix), but the bare card name
// without the suffix does. This is calque's own translation responsibility —
// "how Modal spells a GPU name" — not truffle's, so the normalization lives
// here rather than waiting on truffle's resolver to grow Modal-specific
// vocabulary.
func (r *TruffleResolver) Resolve(card string) ([]Candidate, error) {
	instances, err := find.ResolveCard(card)
	if err != nil && errors.Is(err, find.ErrNoMatch) {
		if bare, ok := normalizeMemorySuffix(card); ok {
			if bareInstances, bareErr := find.ResolveCard(bare); bareErr == nil {
				if r.rep != nil {
					r.rep.Addf(leak.PrimGPU, leak.KindIntegrationEdge, card, 0,
						"gpu=%q doesn't resolve via truffle as spelled (truffle#130); normalized to %q, which does — using %q",
						card, bare, bare)
				}
				instances, err = bareInstances, nil
			}
		}
	}
	if err != nil {
		if errors.Is(err, find.ErrNoMatch) {
			if r.rep != nil {
				r.rep.Addf(leak.PrimAcquire, leak.KindIntegrationEdge, card, 0,
					"truffle resolved card %q to NO instances: %v", card, err)
			}
			return nil, fmt.Errorf("truffle resolved card %q to no instances: %w", card, err)
		}
		return nil, fmt.Errorf("truffle ResolveCard(%q): %w", card, err)
	}
	// instances is already sorted by ResolveCard.
	out := make([]Candidate, 0, len(instances))
	for _, it := range instances {
		out = append(out, Candidate{Instance: it, Family: instanceFamily(it)})
	}
	return out, nil
}

// memorySuffixRe matches a trailing hyphenated memory-size spelling Modal's
// gpu= strings document (e.g. "-80GB", "-40gb") — calque#134/truffle#130.
// Anchored at the end so it only strips a genuine suffix, not a memory token
// that happens to appear mid-string.
var memorySuffixRe = regexp.MustCompile(`(?i)-\d+gb$`)

// normalizeMemorySuffix strips a trailing "-80GB"/"-40GB"-shaped memory
// suffix from card, returning the bare form and true if a suffix was found —
// it does NOT itself check whether the bare form resolves; the caller tries
// that and only uses the normalization on success, keeping this a narrow,
// specific fix for the one confirmed-real gap (truffle#130's memory-suffix
// spelling) rather than a general fuzzy-matching layer.
func normalizeMemorySuffix(card string) (bare string, ok bool) {
	if !memorySuffixRe.MatchString(card) {
		return "", false
	}
	return memorySuffixRe.ReplaceAllString(card, ""), true
}

// instanceFamily extracts the family prefix from an instance type string, e.g.
// "g7e.2xlarge" -> "g7e". ResolveCard returns bare instance types with no
// separate family list, so we derive it structurally rather than via a second
// truffle call.
func instanceFamily(instance string) string {
	if i := strings.IndexByte(instance, '.'); i >= 0 {
		return instance[:i]
	}
	return instance
}

// PickSmallest chooses the smallest candidate instance as the single resolved
// target for the spike (single-node, §2). "Smallest" = fewest leading-number
// vCPUs by name heuristic; for g7e that's g7e.2xlarge (the family's floor). Real
// right-sizing is deferred behind the seam (§1) — this is deliberately dumb.
func PickSmallest(cands []Candidate) (Candidate, error) {
	if len(cands) == 0 {
		return Candidate{}, fmt.Errorf("no candidates to pick from")
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if sizeRank(c.Instance) < sizeRank(best.Instance) {
			best = c
		}
	}
	return best, nil
}

// FillTarget resolves a Target's Instance from its Card via the resolver, picking
// the smallest candidate. Region is left for acquisition to fill (§4). rep may
// be nil (e.g. in tests that don't care about leaks); when non-nil, an
// instance family with no live-verified SharingMode entry is noted via rep
// (calque#134) — informational, not an error, since FillTarget still succeeds.
func FillTarget(t *target.Target, r Resolver, rep *leak.Report) error {
	cands, err := r.Resolve(t.Card)
	if err != nil {
		return err
	}
	pick, err := PickSmallest(cands)
	if err != nil {
		return err
	}
	t.Instance = pick.Instance
	// calque#105: SharingMode is a hardware FACT about the resolved instance,
	// not a decision — refresh it here now that the REAL instance (not the
	// stub's DefaultCard-only guess) is known. Leaves whatever the
	// Recommender set if this instance family has no table entry yet, rather
	// than silently clearing it to the zero value.
	if mode, ok := target.SharingModeFor(t.Instance); ok {
		t.SharingMode = mode
	} else if rep != nil {
		rep.Addf(leak.PrimGPU, leak.KindIntegrationEdge, t.Card, 0,
			"instance family %q (resolved from card %q) has no live-verified MIG/MPS sharing-mode data (docs/gpu-sharing-support-matrix.md covers g6/g6e/g7/g7e only) — run continues, SharingMode left unset",
			pick.Family, t.Card)
	}
	return nil
}

// Price fills a candidate's live on-demand rate via truffle's pricer. This is how
// calque sources R_a (the AWS rate for the substituted card) — through truffle,
// live, rather than from a hardcoded constant.
func Price(ctx context.Context, p Pricer, c *Candidate, region string) error {
	rate, err := p.OnDemandPrice(ctx, c.Instance, region)
	if err != nil {
		return fmt.Errorf("truffle OnDemandPrice(%s, %s): %w", c.Instance, region, err)
	}
	c.PriceUSDHr = rate
	c.Region = region
	c.Priced = true
	return nil
}

// NewTrufflePricer builds a live truffle pricing client (needs AWS creds).
func NewTrufflePricer(ctx context.Context) (Pricer, error) {
	return truffleaws.NewClient(ctx)
}

// sizeRank is a crude ordering of instance sizes by name so PickSmallest is
// deterministic. Lower = smaller. Unknown sizes sort last.
func sizeRank(instance string) int {
	// e.g. "g7e.2xlarge" -> "2xlarge"
	size := instance
	for i := 0; i < len(instance); i++ {
		if instance[i] == '.' {
			size = instance[i+1:]
			break
		}
	}
	order := map[string]int{
		"medium": 0, "large": 1, "xlarge": 2, "2xlarge": 3, "4xlarge": 4,
		"8xlarge": 5, "12xlarge": 6, "16xlarge": 7, "24xlarge": 8, "48xlarge": 9,
	}
	if r, ok := order[size]; ok {
		return r
	}
	return 100
}
