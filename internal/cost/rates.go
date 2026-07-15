// Package cost implements the cost model and crossover K (spec §9) — the headline
// deliverable. Both sides are per-second and grounded in the SAME measured
// per-item compute from a real run; they differ only in the billing model applied.
//
// The comparison must survive a hostile read (a skeptical PI or a Modal advocate
// will attack it). So: rates are dated/cited/swappable (config/rates.json), Modal
// at scale is built up honestly rather than linearly extrapolated from a tiny run,
// AWS is shown at MEASURED occupancy (not an assumed 100%), and every non-measured
// input (buy-down rates, the g7e proxy rate) is flagged.
package cost

import (
	"encoding/json"
	"fmt"
	"os"
)

// Rates is the parsed rate table. Fields mirror config/rates.json.
type Rates struct {
	Modal struct {
		Source       string             `json:"_source"`
		Fetched      string             `json:"_fetched"`
		PerSecondUSD map[string]float64 `json:"per_second_usd"`
		Multipliers  struct {
			RegionPinnedLow  float64 `json:"region_pinned_low"`
			RegionPinnedHigh float64 `json:"region_pinned_high"`
			NonPreemptible   float64 `json:"non_preemptible"`
		} `json:"multipliers"`
	} `json:"modal"`
	AWS struct {
		Source          string `json:"_source"`
		Fetched         string `json:"_fetched"`
		Region          string `json:"_region"`
		OnDemandPerHour map[string]struct {
			Rate     float64 `json:"rate"`
			Source   string  `json:"source"`
			Measured bool    `json:"measured"`
		} `json:"on_demand_per_hour_usd"`
		Buydown struct {
			SavingsPlan1yr float64 `json:"savings_plan_1yr_fraction"`
			SavingsPlan3yr float64 `json:"savings_plan_3yr_fraction"`
			Spot           float64 `json:"spot_fraction"`
			Measured       bool    `json:"measured"`
		} `json:"buydown"`
	} `json:"aws"`
	InstanceGPUCount map[string]int `json:"instance_gpu_count"`
}

// LoadRates reads and parses the rate table from a JSON path.
func LoadRates(path string) (*Rates, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rates: %w", err)
	}
	var r Rates
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse rates: %w", err)
	}
	return &r, nil
}

// ModalRate returns the per-second USD rate for the card the script ASKED FOR
// (R_m). Returns (rate, true) on a hit. The lookup normalizes a few aliases.
func (r *Rates) ModalRate(card string) (float64, bool) {
	if v, ok := r.Modal.PerSecondUSD[card]; ok {
		return v, true
	}
	// try a normalized upper form (e.g. "h100" -> "H100")
	for k, v := range r.Modal.PerSecondUSD {
		if equalFold(k, card) {
			return v, true
		}
	}
	return 0, false
}

// AWSOnDemandPerSecond returns the per-second USD rate for the instance we
// substituted TO (R_a), plus whether that rate is measured (live) or a proxy
// constant. AWS bills per-second; we divide the hourly rate by 3600.
func (r *Rates) AWSOnDemandPerSecond(instance string) (rate float64, measured bool, ok bool) {
	e, hit := r.AWS.OnDemandPerHour[instance]
	if !hit {
		return 0, false, false
	}
	return e.Rate / 3600.0, e.Measured, true
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
