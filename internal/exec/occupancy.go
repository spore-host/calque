package exec

// OccupancyRaw is the JSON summary occupancy.py emits (spec §8). Kept here (not in
// measure) so the on-instance warmd binary depends only on exec/warm, not the cost
// stack. The control-plane measure step maps this into measure.OccupancySummary.
//
// MeanOccupancy is the PRIMARY occupancy K uses — the most accurate available
// source (DCGM SM-activity > nvidia-smi dmon sm% > nvidia-smi utilization.gpu).
// Metrics carries ALL sampled sources so a skeptic can compare: nvidia-smi's
// coarse utilization.gpu understates a busy GPU vs DCGM's real SM activity (§8).
type OccupancyRaw struct {
	MeanOccupancy   *float64            `json:"mean_occupancy"`   // primary, best-available; nil if unmeasured
	OccupancySource string              `json:"occupancy_source"` // which metric fed MeanOccupancy
	Metrics         map[string]*float64 `json:"metrics"`          // nvsmi_util | nvsmi_sm | dcgm_sm -> mean
	MetricSamples   map[string]int      `json:"metric_samples"`
	Samples         int                 `json:"samples"`
	Source          string              `json:"source"` // = OccupancySource (back-compat)
	IntervalS       float64             `json:"interval_s"`
	Measured        bool                `json:"measured"`
}
