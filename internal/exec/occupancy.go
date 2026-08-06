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

	// Scope names the averaging WINDOW this mean covers: ScopeWholeRun (sampler
	// lifetime, includes the one-time @enter model load) or ScopeInference (item work
	// only). The same run yields very different occupancy per scope, so a number
	// without its scope isn't auditable — and the load-contaminated whole-run mean
	// moves the wrong way when work gets faster (#71). Empty means whole_run: that's
	// what a pre-#71 sampler emitted, and reading it as steady-state is the bug.
	Scope string `json:"scope,omitempty"`
}

// ScopeOrWholeRun returns the declared scope, defaulting to ScopeWholeRun for
// summaries written before the scope field existed. Conservative on purpose: an
// unlabeled number IS a whole-run mean, and labeling it as inference-window would
// overstate GPU fill.
func (o OccupancyRaw) ScopeOrWholeRun() string {
	if o.Scope == "" {
		return ScopeWholeRun
	}
	return o.Scope
}
