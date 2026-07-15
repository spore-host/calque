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
	Occupancy       float64 // mean GPU utilization fraction [0,1] across the run
	SampleItems     int     // how many items the measurement is based on
	AWSRateMeasured bool    // is R_a a live rate or a proxy constant?
	// AcquireSeconds is the lagotto/spawn time-to-acquire (idle the rectangle pays
	// for before any work) + any warm-idle; part of the AWS rectangle (§8).
	AcquireSeconds float64
	EnterSeconds   float64 // one-time warm @enter load (amortized across items)
}

// SideCost is one side's total dollars at a given item count.
type SideCost struct {
	Label   string
	Items   int
	Dollars float64
	Detail  string
}

// Model computes both sides at scale and locates the crossover K. It never
// linearly extrapolates Modal from a tiny run (spec §9): Modal-at-scale is
// R_m x (per-item compute x items + one-time enter), built up honestly.
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
	fmt.Fprintf(&b, "Your workload:   %s (asked for %s -> substituted %s), %d items, measured %.3fs/item, occupancy %.0f%%\n",
		"map-batch", m.M.CardAskedFor, m.M.InstanceUsed, atItems, m.M.SecPerItem, m.M.Occupancy*100)
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
	return b.String(), nil
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
