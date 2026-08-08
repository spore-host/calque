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
func (r *TruffleResolver) Resolve(card string) ([]Candidate, error) {
	instances, err := find.ResolveCard(card)
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
// the smallest candidate. Region is left for acquisition to fill (§4).
func FillTarget(t *target.Target, r Resolver) error {
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
