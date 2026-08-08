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

import (
	"strings"

	"github.com/spore-host/calque/internal/ir"
)

// Target is what every downstream stage consumes. The four fields are filled
// at different stages so a later real recommender is a drop-in:
//   - Card:        the recommender's decision (stubbed to a constant).
//   - Instance:     truffle fills this (card -> concrete instance type).
//   - Region:       lagotto/acquisition fills this on landing.
//   - SharingMode:  looked up from the card's instance family once Instance is
//     known (calque#105) — what kind of multi-tenant sharing (if any) the
//     resolved card supports. Read by M13's institutional-sharing design
//     (plan sizing, gpu guard, warmd process topology); NOT selection logic
//     itself (spec §4's seam discipline still holds — this is a hardware FACT
//     about the resolved card, not a decision about whether/how to use it).
type Target struct {
	Card        string // e.g. "RTX PRO 6000"
	Instance    string // truffle fills this: e.g. "g7e.2xlarge"
	Region      string // acquisition fills this on landing
	SharingMode SharingMode
}

// SharingMode is what kind of multi-tenant GPU sharing a card supports,
// verified against real hardware (calque#104,
// docs/gpu-sharing-support-matrix.md) rather than assumed from datasheets —
// the g7 entry in that doc exists specifically because a datasheet-only
// answer was wrong (AWS ships the Server Edition, not the Workstation
// Edition the earlier research assumed).
type SharingMode string

const (
	// Dedicated means no multi-tenant sharing: one workload owns the whole
	// card. Not currently assigned to any of calque's four target families
	// (all four support at least MPS) — reserved for a future family where
	// neither MIG nor MPS is viable, or as an explicit opt-out.
	Dedicated SharingMode = "dedicated"
	// MIG means the card supports NVIDIA Multi-Instance GPU: hardware-level
	// partitioning into isolated slices, each with its own memory/fault
	// boundary. The default/preferred mode when available (M13's
	// institutional-sharing design) — a card entered as MIG can still run in
	// MPS mode as an explicit opt-in; MIG is just the safer default.
	MIG SharingMode = "mig"
	// MPS means NVIDIA Multi-Process Service: software-level cooperative
	// sharing, no hardware isolation between clients. Viable on every one of
	// calque's four target families (confirmed live on all four,
	// docs/gpu-sharing-support-matrix.md) — the fallback for cards without
	// MIG, or an explicit trusted-tenant choice on a MIG-capable card.
	MPS SharingMode = "mps"
)

// sharingModeByFamily is the static lookup table populated from LIVE hardware
// verification (calque#104), not vendor datasheets — see
// docs/gpu-sharing-support-matrix.md for the full method and results. Keyed
// by instance family (the truffle-resolved instance type's prefix before the
// first '.', e.g. "g7e.2xlarge" -> "g7e"), mirroring
// internal/plan/truffle.go's own instanceFamily helper (not imported directly
// — this package stays self-contained per its own seam discipline, spec §4).
var sharingModeByFamily = map[string]SharingMode{
	"g6":  MPS, // L4: no MIG ("Not Supported" on live hardware); MPS confirmed working
	"g6e": MPS, // L40S: no MIG ("Not Supported" on live hardware); MPS confirmed working
	"g7":  MIG, // RTX PRO 4500 Blackwell SERVER EDITION (AWS's actual SKU): MIG confirmed live, max 2 instances up to 2g.32gb
	"g7e": MIG, // RTX PRO 6000 Blackwell Server Edition: MIG confirmed live, max 4 instances up to 4g.96gb
}

// SharingModeFor looks up SharingMode for a truffle-resolved instance type
// (e.g. "g7e.2xlarge"). Returns ("", false) for a family with no explicit
// entry — callers must not silently default; an unentered family means the
// table hasn't been extended for it yet, and that's a fact worth surfacing
// (e.g. via a leak), not papering over with a guessed value. Exported so any
// caller holding a resolved instance string — not just plan.FillTarget, which
// calls this directly once truffle resolves Target.Instance — can look this
// up without re-deriving the family-extraction logic.
func SharingModeFor(instance string) (SharingMode, bool) {
	family := instance
	if i := strings.IndexByte(instance, '.'); i >= 0 {
		family = instance[:i]
	}
	mode, ok := sharingModeByFamily[family]
	return mode, ok
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

// defaultCardFamily is DefaultCard's known truffle-resolved instance family
// (g7e — see FillTarget's callers, which all resolve DefaultCard to a
// g7e.2xlarge). StubRecommender uses this to fill SharingMode WITHOUT
// resolving an instance itself (Instance is still filled later, by
// FillTarget/truffle, unchanged) — this is one more piece of the same single
// hardcoded constant the stub already ships (spec §4's "entire faked brain is
// one constant" discipline), not a second decision.
const defaultCardFamily = "g7e"

// StubRecommender is the ENTIRE faked brain. Do not add logic here (spec §4).
type StubRecommender struct{}

// Recommend returns the constant Target. It deliberately ignores its inputs:
// the spike proves the plumbing carries the semantics, not that the choice is good.
func (StubRecommender) Recommend(_ ir.App, _ ir.Function) Target {
	// defaultCardFamily always has an entry — enforced by
	// TestDefaultCardFamilyHasSharingModeEntry, not a runtime fallback here.
	mode := sharingModeByFamily[defaultCardFamily]
	return Target{Card: DefaultCard, SharingMode: mode}
}

// Compile-time assertion that the stub satisfies the interface — so a signature
// drift in either breaks the build here, at the seam, rather than downstream.
var _ Recommender = StubRecommender{}
