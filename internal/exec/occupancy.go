package exec

// OccupancyRaw is the JSON summary occupancy.py emits (spec §8). Kept here (not in
// measure) so the on-instance warmd binary depends only on exec/warm, not the cost
// stack. The control-plane measure step maps this into measure.OccupancySummary.
type OccupancyRaw struct {
	MeanOccupancy *float64 `json:"mean_occupancy"` // nil if unmeasured (no GPU)
	Samples       int      `json:"samples"`
	Source        string   `json:"source"` // dcgm | nvidia-smi | none
	IntervalS     float64  `json:"interval_s"`
	Measured      bool     `json:"measured"`
}
