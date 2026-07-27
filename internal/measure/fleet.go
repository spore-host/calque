package measure

// Multi-instance fold (spec §9/§15, Gap D5). At fleet scale, S single-node
// instances each do a slice of the .map in parallel. K must reflect what AWS
// ACTUALLY bills across the whole fleet, not one box's rectangle — otherwise a
// fleet K would understate cost by a factor of ~S in the fixed overheads.
//
// The honest fold:
//   - Per-item compute + occupancy are per-GPU properties. We take the
//     item-count-weighted mean across instances (a slow shard pulls the mean up).
//   - Acquire + @enter overhead is paid ONCE PER INSTANCE. Every instance sniped
//     capacity and loaded the model, so the fleet pays the SUM of those overheads
//     in billed instance-seconds — even though they happened concurrently in
//     wall-clock. Summing (not maxing) is the move that survives a hostile read:
//     you rent S boxes, you pay S acquisitions + S loads.
//
// The result is a single Measurement the existing cost.Model consumes unchanged.

// FleetFold combines per-instance measurements into one fleet-level Measurement.
// InstanceUsed/CardAskedFor come from the first instance (a fleet is homogeneous in
// the spike — same instance type across shards). Returns a zero Measurement for an
// empty fleet.
func FleetFold(perInstance []Measurement) Measurement {
	if len(perInstance) == 0 {
		return Measurement{}
	}
	agg := Measurement{
		CardAskedFor: perInstance[0].CardAskedFor,
		InstanceUsed: perInstance[0].InstanceUsed,
	}

	var totalItems int
	var sumWeightedSecPerItem float64 // Σ (meanSecs_i * items_i)
	var sumWeightedOcc float64        // Σ (occ_i * items_i), only over measured occ
	var occItems int                  // items backing a measured occupancy
	var sumAcquire, sumEnter, totalCompute float64
	allOccMeasured := true

	for _, m := range perInstance {
		items := m.PerItem.Count
		totalItems += items
		sumWeightedSecPerItem += m.PerItem.MeanSecs * float64(items)
		totalCompute += m.PerItem.TotalSecs
		// Acquire + enter are per-instance overheads the fleet pays in full.
		sumAcquire += m.AcquireWaitSeconds
		sumEnter += m.EnterSeconds
		if occ, ok := m.OccupancyFraction(); ok {
			sumWeightedOcc += occ * float64(items)
			occItems += items
		} else {
			allOccMeasured = false
		}
	}

	agg.PerItem = PerItem{
		Count:     totalItems,
		TotalSecs: totalCompute,
	}
	if totalItems > 0 {
		agg.PerItem.MeanSecs = sumWeightedSecPerItem / float64(totalItems)
	}
	agg.EnterSeconds = sumEnter
	agg.AcquireWaitSeconds = sumAcquire

	// Fold occupancy: item-weighted mean over the instances that measured it. If
	// ANY instance failed to measure, the fleet occupancy is a proxy (flag honest).
	if occItems > 0 {
		mean := sumWeightedOcc / float64(occItems)
		agg.Occupancy = OccupancySummary{
			MeanOccupancy: &mean,
			Samples:       occItems,
			Source:        perInstance[0].Occupancy.Source,
			Measured:      allOccMeasured,
		}
	}
	return agg
}
