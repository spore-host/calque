package cost

import (
	"fmt"
	"math"
	"strings"
)

// Measured holds the ground-truth numbers from a real run (spec §8). K stands on
// these — modeling them makes K a guess; measuring them makes K a fact a skeptic
// can reproduce.
type Measured struct {
	CardAskedFor    string  // e.g. "H100" — drives R_m (the asymmetry, §9)
	InstanceUsed    string  // e.g. "g7e.2xlarge" — drives R_a
	SecPerItem      float64 // mean warm per-item compute seconds
	Occupancy       float64 // mean GPU utilization fraction [0,1]; see OccupancyScope
	SampleItems     int     // how many items the measurement is based on
	AWSRateMeasured bool    // is R_a a live rate or a proxy constant?
	// OccupancyScope names the window Occupancy was averaged over: "inference" (item
	// work only — what this model wants, since EnterSeconds already carries the load)
	// or "whole_run" (includes the one-time @enter load, so it DOUBLE-COUNTS that idle
	// time: once as low occupancy, again as EnterSeconds). Empty => unknown/whole_run.
	// Reported in the verdict because K is only auditable if its inputs are labeled (#71).
	OccupancyScope string
	// AcquireSeconds is the lagotto/spawn time-to-acquire (idle the rectangle pays
	// for before any work) + any warm-idle; part of the AWS rectangle (§8).
	AcquireSeconds float64
	EnterSeconds   float64 // one-time warm @enter load (amortized across items)

	// WarmHit is true when THIS run's AcquireSeconds/EnterSeconds reflect a
	// shared-pool warm hit (calque#100/#101's pool worker was already resident
	// and loaded — the caller reports near-zero fixed cost for this run) rather
	// than a dedicated acquire-and-load (the pool/session/real default: this run
	// paid the FULL fixed cost alone, per docs/pool-queue-contract.md). It does
	// not change how the model computes anything — AcquireSeconds/EnterSeconds
	// are used exactly as given either way — it only changes what the verdict
	// SAYS about what those numbers mean, so a reader doesn't assume a hit's
	// near-zero fixed cost is the STEADY STATE for every run against a shared
	// pool (most runs might hit; the one that triggers a cold load pays the
	// whole fixed cost alone, and reporting every run's K as if it were that one
	// misattributes cost, exactly as reporting every run's K as if it always hit
	// warm would understate it). Zero-value (false) is today's existing
	// dedicated-acquisition behavior — no change for any pre-#102 caller.
	WarmHit bool
}

// SideCost is one side's total dollars at a given item count.
type SideCost struct {
	Label   string
	Items   int
	Dollars float64
	Detail  string
}

// Model computes both sides at scale and, if AWS ever undercuts Modal, the N
// at which that happens (K). It never linearly extrapolates Modal from a tiny
// run (spec §9): Modal-at-scale is R_m x (per-item compute x items + one-time
// enter), built up honestly.
type Model struct {
	Rates *Rates
	M     Measured
}

// modalSecondsAt returns Modal's billed seconds for n items. Modal bills
// compute-seconds only (scale-to-0), so it's per-item compute x n, plus the
// one-time @enter load (Modal charges for the warm container's load too). We do
// NOT multiply by items^anything or extrapolate from a 10-item slope — we build
// from the measured per-item second and the published per-second rate.
func (m *Model) modalSecondsAt(n int) float64 {
	return m.M.SecPerItem*float64(n) + m.M.EnterSeconds
}

// awsRectangleSecondsAt returns AWS's billed "rectangle" seconds for n items: the
// wall-clock the instance is held from launch to terminate, which AWS bills
// regardless of occupancy. At measured occupancy P, doing n items of compute c
// each takes n*c/P wall-seconds of instance time (the idle fraction 1-P is paid
// for but not computing). Plus the one-time acquire + enter overhead.
//
// Showing it at measured P (not assumed 100%) is the honest move §9 demands — it
// makes AWS look WORSE than a naive model would, which is the point: the number
// must survive a hostile read.
func (m *Model) awsRectangleSecondsAt(n int) float64 {
	p := m.M.Occupancy
	if p <= 0 {
		p = 1 // guard; a zero-occupancy measurement is a measurement bug, treat as 100%
	}
	compute := m.M.SecPerItem * float64(n)
	return compute/p + m.M.AcquireSeconds + m.M.EnterSeconds
}

// ErrNoComputeMeasured means per-item compute is ~0 — the model can't produce a
// meaningful K (both sides collapse toward the fixed overheads). This happens on a
// dry-run with a trivial stand-in body; the caller should treat K as undefined.
var ErrNoComputeMeasured = fmt.Errorf("per-item compute is ~0; K is undefined (no real measurement)")

// hasCompute reports whether the per-item measurement is substantial enough for K.
func (m *Model) hasCompute() bool { return m.M.SecPerItem >= 1e-4 }

// ModalAt returns Modal's cost for n items (R_m for the card asked for).
func (m *Model) ModalAt(n int) (SideCost, error) {
	rm, ok := m.Rates.ModalRate(m.M.CardAskedFor)
	if !ok {
		return SideCost{}, fmt.Errorf("no Modal rate for card %q", m.M.CardAskedFor)
	}
	secs := m.modalSecondsAt(n)
	return SideCost{
		Label:   "Modal",
		Items:   n,
		Dollars: rm * secs,
		Detail:  fmt.Sprintf("R_m=$%.6f/s (%s) x %.0f compute-s", rm, m.M.CardAskedFor, secs),
	}, nil
}

// AWSAt returns AWS on-demand cost for n items (R_a for the substituted instance)
// at the measured occupancy.
func (m *Model) AWSAt(n int) (SideCost, bool, error) {
	ra, measured, ok := m.Rates.AWSOnDemandPerSecond(m.M.InstanceUsed)
	if !ok {
		return SideCost{}, false, fmt.Errorf("no AWS rate for instance %q", m.M.InstanceUsed)
	}
	secs := m.awsRectangleSecondsAt(n)
	return SideCost{
		Label:   "AWS on-demand",
		Items:   n,
		Dollars: ra * secs,
		Detail:  fmt.Sprintf("R_a=$%.6f/s (%s%s) x %.0f rectangle-s @ %.0f%% occ", ra, m.M.InstanceUsed, proxyTag(measured), secs, m.M.Occupancy*100),
	}, measured, nil
}

// AWSAtRung returns AWS cost at a buy-down rung (fraction of on-demand). Rungs are
// static constants for the spike (flagged). fraction<1 => cheaper than on-demand.
func (m *Model) AWSAtRung(n int, fraction float64, label string) (SideCost, error) {
	base, _, err := m.AWSAt(n)
	if err != nil {
		return SideCost{}, err
	}
	return SideCost{
		Label:   label,
		Items:   n,
		Dollars: base.Dollars * fraction,
		Detail:  fmt.Sprintf("%.0f%% of on-demand (static constant, not measured)", fraction*100),
	}, nil
}

// Crossover locates K: the smallest item count where AWS (at `fraction` of
// on-demand; 1.0 = on-demand) costs less than Modal. Because both sides are
// affine in n with AWS having the lower marginal slope (per-second rate x
// seconds-per-item), there is a single crossover; we solve it in closed form and
// return the ceiling. Returns (K, true) if a finite crossover exists, else
// (0, false) meaning AWS never wins in range (stay on Modal).
func (m *Model) Crossover(fraction float64) (int, bool, error) {
	rm, ok := m.Rates.ModalRate(m.M.CardAskedFor)
	if !ok {
		return 0, false, fmt.Errorf("no Modal rate for %q", m.M.CardAskedFor)
	}
	ra, _, ok := m.Rates.AWSOnDemandPerSecond(m.M.InstanceUsed)
	if !ok {
		return 0, false, fmt.Errorf("no AWS rate for %q", m.M.InstanceUsed)
	}
	ra *= fraction

	// Modal(n)  = rm * (c*n + e_m)
	// AWS(n)    = ra * (c*n/p + acq + e_a)
	// marginal per item: modalSlope = rm*c ; awsSlope = ra*c/p
	c := m.M.SecPerItem
	p := m.M.Occupancy
	if p <= 0 {
		p = 1
	}
	modalSlope := rm * c
	awsSlope := ra * c / p
	modalFixed := rm * m.M.EnterSeconds
	awsFixed := ra * (m.M.AcquireSeconds + m.M.EnterSeconds)

	// If AWS's marginal cost per item is >= Modal's, AWS never catches up: the
	// occupancy is too low or the rate too high. Honest answer: stay on Modal.
	if awsSlope >= modalSlope {
		return 0, false, nil
	}
	// Solve modalFixed + modalSlope*K = awsFixed + awsSlope*K
	k := (awsFixed - modalFixed) / (modalSlope - awsSlope)
	if k < 0 {
		k = 0 // AWS already cheaper at n=1
	}
	return int(math.Ceil(k)), true, nil
}

// Verdict renders the §9 headline: the boundary the user locates themselves
// against, willing to say STAY ON MODAL. `atItems` is the user's actual workload.
func (m *Model) Verdict(atItems int) (string, error) {
	if !m.hasCompute() {
		return "", ErrNoComputeMeasured
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your workload:   %s (asked for %s -> substituted %s), %d items, measured %.3fs/item, occupancy %.0f%% (%s)\n",
		"map-batch", m.M.CardAskedFor, m.M.InstanceUsed, atItems, m.M.SecPerItem, m.M.Occupancy*100, m.occScopeLabel())
	if m.M.SampleItems > 0 && m.M.SampleItems < atItems {
		fmt.Fprintf(&b, "  (per-item + occupancy measured on %d items; Modal-at-scale built from that, NOT linearly extrapolated)\n", m.M.SampleItems)
	}

	// A representative ladder of scales.
	scales := ladder(atItems)
	for _, n := range scales {
		md, err := m.ModalAt(n)
		if err != nil {
			return "", err
		}
		aw, measured, err := m.AWSAt(n)
		if err != nil {
			return "", err
		}
		winner := "Modal wins"
		if aw.Dollars < md.Dollars {
			winner = "AWS wins"
		}
		fmt.Fprintf(&b, "  %-9d Modal: $%-10.2f | AWS on-demand: $%-10.2f  %s%s\n",
			n, md.Dollars, aw.Dollars, winner, proxyTag(measured))
	}

	kOD, okOD, err := m.Crossover(1.0)
	if err != nil {
		return "", err
	}
	kSP, okSP, _ := m.Crossover(m.Rates.AWS.Buydown.SavingsPlan1yr)
	if okOD {
		fmt.Fprintf(&b, "Crossover:  ~%d items (on-demand)", kOD)
		if okSP {
			fmt.Fprintf(&b, ";  ~%d items (1yr Savings Plan, static rate)", kSP)
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "Crossover:  none in range — at %.0f%% occupancy AWS's per-item cost never undercuts Modal.\n", m.M.Occupancy*100)
	}

	// The verdict must be willing to say stay on Modal (§9).
	switch {
	case !okOD:
		b.WriteString("Verdict:    STAY ON MODAL. AWS does not win at this occupancy — buy down the rate or raise occupancy first.\n")
	case atItems < kOD:
		fmt.Fprintf(&b, "Verdict:    you are running %d.  %d < K(%d) -> STAY ON MODAL. This is what Modal is for.\n", atItems, atItems, kOD)
	default:
		fmt.Fprintf(&b, "Verdict:    you are running %d.  %d >= K(%d) -> CROSS. Code is unchanged; here's the bill.\n", atItems, atItems, kOD)
	}

	// Honesty flag on the AWS side of K.
	_, measured, _ := m.Rates.AWSOnDemandPerSecond(m.M.InstanceUsed)
	flag := "measured"
	if !measured {
		flag = "PROXY (g7e not yet in AWS Pricing API; rate is a cited constant)"
	}
	fmt.Fprintf(&b, "AWS side of K: [%s]\n", flag)
	// A whole-run occupancy double-counts the load idle (once in P, once in
	// EnterSeconds), so K comes out pessimistic for AWS. Say so rather than letting
	// the reader assume the occupancy figure means steady-state fill (#71).
	// Note the "" case deliberately: an unlabeled scope IS a whole-run mean (that's
	// what pre-#71 samplers and the dry-run stand-in produce), and occScopeLabel
	// already renders it as one. Suppressing the warning only when the scope is
	// unlabeled would print "WHOLE-RUN mean" with no explanation of which way it
	// biases K — the exact reading this note exists to prevent.
	if m.M.OccupancyScope != "inference" {
		// Name the load's cost only when we measured one: "the 0s one-time load" reads
		// as a bug, and in the dry-run path (where enter is a CPU stand-in) there is no
		// honest number to quote.
		// %.0f would render a sub-second load as "0s", which reads as a bug rather than
		// a measurement, so keep a digit of precision below 1s.
		load := "the one-time @enter load"
		switch {
		case m.M.EnterSeconds >= 1:
			load = fmt.Sprintf("the %.0fs one-time @enter load", m.M.EnterSeconds)
		case m.M.EnterSeconds > 0:
			load = fmt.Sprintf("the %.2gs one-time @enter load", m.M.EnterSeconds)
		}
		fmt.Fprintf(&b, "NOTE: occupancy is a WHOLE-RUN mean (includes %s), so it\n"+
			"      understates steady-state GPU fill and makes this K pessimistic for AWS.\n", load)
	}
	fmt.Fprintf(&b, "%s\n", m.warmHitLabel())
	return b.String(), nil
}

// warmHitLabel names WHICH fixed-cost regime this K's AcquireSeconds/EnterSeconds
// came from (calque#102), mirroring occScopeLabel's discipline: an amortized-cost
// number is only auditable if the reader is told which regime produced it, not
// left to assume every run against a shared pool costs the same as this one.
func (m *Model) warmHitLabel() string {
	if m.M.WarmHit {
		return "Fixed cost regime: WARM HIT — this run reused an already-loaded pool worker " +
			"(near-zero acquire+enter); most runs against a healthy pool look like this, but the " +
			"run that triggers a cold load pays the full fixed cost alone (see a non-warm-hit K for that number)."
	}
	return "Fixed cost regime: DEDICATED ACQUISITION — this run paid the full acquire+enter cost alone " +
		"(today's default outside a shared pool; calque#100/#101)."
}

// occScopeLabel renders the occupancy window for the verdict line.
func (m *Model) occScopeLabel() string {
	switch m.M.OccupancyScope {
	case "inference":
		return "inference window; the one-time load is priced separately via enter"
	case "", "whole_run":
		return "WHOLE-RUN mean — includes the one-time model load"
	default:
		return m.M.OccupancyScope
	}
}

func ladder(at int) []int {
	base := []int{10, 100, 1000, 10000, 100000}
	// ensure the user's actual scale is represented
	seen := map[int]bool{}
	var out []int
	for _, n := range append(base, at) {
		if n > 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	// simple insertion sort (small slice)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func proxyTag(measured bool) string {
	if measured {
		return ""
	}
	return " [proxy]"
}
