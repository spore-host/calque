// Package measure is the tach hook (spec §8): it folds the three ground-truth
// inputs K stands on into a single measurement — because modeling occupancy makes
// K a guess, and measuring it makes K a fact a skeptic can reproduce.
//
// The three inputs:
//   - Occupancy P%       — from the in-worker sampler (occupancy.py; nvidia-smi/DCGM)
//   - Per-item wall-clock — from warmd (each Result.Seconds in the warm runner)
//   - Instance-seconds    — from the Acquirer timestamps (acquire -> terminate),
//     the AWS "rectangle" including acquisition wait and idle.
//
// No `tach` library exists in the org, so this is the crude sampler §8 permits.
// Which source was used is recorded, and any unmeasured input flips K's flag to
// proxy.
package measure

import (
	"time"
)

// OccupancySummary mirrors occupancy.py's JSON summary.
type OccupancySummary struct {
	MeanOccupancy *float64 `json:"mean_occupancy"` // nil if unmeasured
	Samples       int      `json:"samples"`
	Source        string   `json:"source"` // dcgm | nvidia-smi | none
	IntervalS     float64  `json:"interval_s"`
	Measured      bool     `json:"measured"`
}

// PerItem aggregates the per-item wall-clock series warmd collected.
type PerItem struct {
	Count     int
	TotalSecs float64
	MeanSecs  float64
}

// Aggregate computes mean seconds/item from a slice of per-item seconds.
func Aggregate(perItemSecs []float64) PerItem {
	p := PerItem{Count: len(perItemSecs)}
	for _, s := range perItemSecs {
		p.TotalSecs += s
	}
	if p.Count > 0 {
		p.MeanSecs = p.TotalSecs / float64(p.Count)
	}
	return p
}

// Measurement is the folded ground truth for one run, ready to hand to the cost
// model. It records provenance (source, sample counts) so a skeptic can audit,
// and an explicit Measured flag per input so K's measured|proxy label is honest.
type Measurement struct {
	CardAskedFor string // e.g. "H100"
	InstanceUsed string // e.g. "g7e.2xlarge"

	PerItem   PerItem
	Occupancy OccupancySummary

	// Rectangle: the instance-seconds AWS actually bills (§8).
	AcquiredAt   time.Time
	TerminatedAt time.Time
	EnterSeconds float64 // one-time warm @enter load

	// Derived
	AcquireWaitSeconds float64 // acquire attempt start -> landed (idle AWS pays for)
}

// RectangleSeconds is launch->terminate wall-clock: what AWS bills regardless of
// occupancy. Falls back to 0 if timestamps are unset (dry-run).
func (m Measurement) RectangleSeconds() float64 {
	if m.TerminatedAt.IsZero() || m.AcquiredAt.IsZero() {
		return 0
	}
	return m.TerminatedAt.Sub(m.AcquiredAt).Seconds()
}

// OccupancyFraction returns the measured occupancy, or a conservative fallback.
// If occupancy was NOT measured (source "none", e.g. a CPU dry-run), it returns
// (1.0, false) — 100% is the LEAST favorable-to-AWS assumption that still lets the
// model run, and the false flag tells the caller to mark K's occupancy as proxy.
func (m Measurement) OccupancyFraction() (frac float64, measured bool) {
	if m.Occupancy.Measured && m.Occupancy.MeanOccupancy != nil {
		return *m.Occupancy.MeanOccupancy, true
	}
	return 1.0, false
}

// FullyMeasured reports whether every input to K was measured (vs proxied). Drives
// the top-level measured|proxy flag on the emitted K.
func (m Measurement) FullyMeasured(awsRateMeasured bool) bool {
	_, occMeasured := m.OccupancyFraction()
	return occMeasured && awsRateMeasured && m.PerItem.Count > 0 && m.RectangleSeconds() > 0
}
