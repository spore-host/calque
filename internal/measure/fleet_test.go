package measure

import (
	"math"
	"testing"
)

func occ(f float64) OccupancySummary {
	return OccupancySummary{MeanOccupancy: &f, Samples: 100, Source: "dcgm", Measured: true}
}

// TestFleetFoldSumsOverheadsWeightsCompute (D5): acquire + enter overheads SUM
// across instances (you rent S boxes, you pay S acquisitions + S loads), while
// per-item compute + occupancy are item-count-weighted means.
func TestFleetFoldSumsOverheads(t *testing.T) {
	fleet := []Measurement{
		{
			CardAskedFor: "H100", InstanceUsed: "g6.2xlarge",
			PerItem:            PerItem{Count: 100, MeanSecs: 0.5, TotalSecs: 50},
			Occupancy:          occ(0.90),
			EnterSeconds:       30,
			AcquireWaitSeconds: 120,
		},
		{
			CardAskedFor: "H100", InstanceUsed: "g6.2xlarge",
			PerItem:            PerItem{Count: 300, MeanSecs: 0.7, TotalSecs: 210},
			Occupancy:          occ(0.70),
			EnterSeconds:       30,
			AcquireWaitSeconds: 600, // this box waited much longer for capacity
		},
	}
	agg := FleetFold(fleet)

	if agg.PerItem.Count != 400 {
		t.Errorf("total items = %d, want 400", agg.PerItem.Count)
	}
	// Overheads sum: 30+30 enter, 120+600 acquire.
	if agg.EnterSeconds != 60 {
		t.Errorf("enter = %.0f, want 60 (summed per-instance)", agg.EnterSeconds)
	}
	if agg.AcquireWaitSeconds != 720 {
		t.Errorf("acquire = %.0f, want 720 (summed per-instance)", agg.AcquireWaitSeconds)
	}
	// Item-weighted mean per-item: (0.5*100 + 0.7*300)/400 = (50+210)/400 = 0.65.
	if math.Abs(agg.PerItem.MeanSecs-0.65) > 1e-9 {
		t.Errorf("mean sec/item = %.4f, want 0.65", agg.PerItem.MeanSecs)
	}
	// Item-weighted mean occupancy: (0.90*100 + 0.70*300)/400 = (90+210)/400 = 0.75.
	frac, measured := agg.OccupancyFraction()
	if !measured || math.Abs(frac-0.75) > 1e-9 {
		t.Errorf("fleet occupancy = %.4f (measured=%v), want 0.75 measured", frac, measured)
	}
}

// TestFleetFoldOccupancyProxyIfAnyUnmeasured: if ANY instance failed to measure
// occupancy, the fleet occupancy is flagged proxy (honest measured|proxy).
func TestFleetFoldOccupancyProxyIfAnyUnmeasured(t *testing.T) {
	fleet := []Measurement{
		{PerItem: PerItem{Count: 100, MeanSecs: 0.5}, Occupancy: occ(0.9)},
		{PerItem: PerItem{Count: 100, MeanSecs: 0.5}}, // no occupancy measured
	}
	agg := FleetFold(fleet)
	if _, measured := agg.OccupancyFraction(); measured {
		t.Error("fleet occupancy should be proxy when any instance didn't measure it")
	}
}

func TestFleetFoldEmpty(t *testing.T) {
	if agg := FleetFold(nil); agg.PerItem.Count != 0 {
		t.Errorf("empty fleet should fold to a zero measurement, got %+v", agg)
	}
}
