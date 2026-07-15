// Package target is THE SEAM (spec §4): the one piece of future-proofing built
// for real. Everything downstream consumes a Target. Nothing inlines the card
// name into the code generator. The entire faked "brain" is one constant behind
// the Recommender interface. Later, StubRecommender is swapped for the real
// phase-detector and the plumbing never notices.
//
// This is the "first doesn't foreclose the second" contract. Honor it strictly:
// DO NOT add selection logic, cost-optimization, or right-sizing here. If you
// find yourself wanting to, that logic is explicitly deferred behind this seam
// (spec §1, §18).
package target

import "github.com/spore-host/calque/internal/ir"

// Target is what every downstream stage consumes. The three fields are filled at
// different stages so a later real recommender is a drop-in:
//   - Card:     the recommender's decision (stubbed to a constant).
//   - Instance: truffle fills this (card -> concrete instance type).
//   - Region:   lagotto/acquisition fills this on landing.
type Target struct {
	Card     string // e.g. "RTX PRO 6000"
	Instance string // truffle fills this: e.g. "g7e.xlarge"
	Region   string // acquisition fills this on landing
}

// Recommender maps an app + function to a Target. The real implementation is the
// deferred phase-detector; the spike ships only the stub below.
type Recommender interface {
	Recommend(app ir.App, fn ir.Function) Target
}

// DefaultCard is the spike's single constant (spec §2): the RTX PRO 6000 Blackwell
// (96GB), which truffle resolves to a g7e instance. Kept as an exported const so
// the value lives in exactly one place, not inlined at call sites.
const DefaultCard = "RTX PRO 6000"

// StubRecommender is the ENTIRE faked brain. Do not add logic here (spec §4).
type StubRecommender struct{}

// Recommend returns the constant Target. It deliberately ignores its inputs:
// the spike proves the plumbing carries the semantics, not that the choice is good.
func (StubRecommender) Recommend(_ ir.App, _ ir.Function) Target {
	return Target{Card: DefaultCard}
}

// Compile-time assertion that the stub satisfies the interface — so a signature
// drift in either breaks the build here, at the seam, rather than downstream.
var _ Recommender = StubRecommender{}
