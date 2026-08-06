package exec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Occupancy scopes. The scope is part of the measurement, not a presentation
// detail: the same GPU, same run, yields wildly different occupancy depending on
// the averaging window, so a number without its scope is not auditable.
const (
	// ScopeWholeRun averages every sample the sampler took — INCLUDING the one-time
	// @enter model load, during which the GPU is idle by definition. This is what
	// calque reported before #71, and it understates steady-state fill; worse, it
	// moves the WRONG WAY when the workload gets faster (batch-32 ran 24x faster and
	// reported 2%, because a fixed ~210s load dominated a 15s inference window).
	ScopeWholeRun = "whole_run"

	// ScopeInference averages only samples inside warmd's inference spans — after
	// @enter returned, while items were actually being processed. This answers the
	// question the cost model needs: while the GPU was doing the work, how full was
	// it? Load cost is not discarded, it's accounted separately (enter_seconds, which
	// the K math already amortizes) — so the two economic effects stop being conflated.
	ScopeInference = "inference"
)

// OccSample is one timestamped tick from occupancy.py's JSONL stream. A nil metric
// means that collector didn't report on that tick (absent tool, timeout) — nil is
// SKIPPED, never coerced to 0, because a missing reading is not an idle GPU.
type OccSample struct {
	TS        float64  `json:"ts"`
	NvsmiUtil *float64 `json:"nvsmi_util"`
	NvsmiSM   *float64 `json:"nvsmi_sm"`
	DcgmSM    *float64 `json:"dcgm_sm"`
}

// Span mirrors warm.Span: a closed wall-clock window in unix epoch seconds.
// Duplicated here (rather than imported) to keep this package free of a dependency
// direction that would drag the supervisor into the control-plane cost path.
type Span struct {
	StartUnix float64 `json:"start_unix"`
	EndUnix   float64 `json:"end_unix"`
}

// ParseOccSamples reads occupancy.py's JSONL sample stream. Malformed lines are
// skipped rather than failing the whole parse: this stream is diagnostic telemetry
// written by a process that can be SIGTERM'd mid-line, so a torn final line is
// expected and must not cost us the run's measurement. Samples without a usable
// timestamp are dropped — they can't be attributed to a window.
func ParseOccSamples(jsonl []byte) []OccSample {
	var out []OccSample
	sc := bufio.NewScanner(bytes.NewReader(jsonl))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var s OccSample
		if err := json.Unmarshal(line, &s); err != nil {
			continue // torn/garbage line — skip, don't fail
		}
		if s.TS <= 0 {
			continue // pre-#71 sampler (no ts) or corrupt: unattributable
		}
		out = append(out, s)
	}
	return out
}

// OccupancyInWindows recomputes occupancy over the UNION of `spans` — the
// inference-only measurement (#71).
//
// It returns a fresh OccupancyRaw carrying per-metric means, the sample counts they
// rest on, and Scope=ScopeInference. `ok` is false when no sample falls inside any
// span, in which case the caller MUST keep the whole-run number and say so rather
// than invent one: an empty window means we didn't measure inference occupancy, and
// the honest report is "unmeasured", not "0%".
//
// A sample is inside a span if its tick START is within [start, end]. Ticks are
// stamped at their start and describe the interval forward from there, so a tick
// beginning inside the window measured work in the window.
func OccupancyInWindows(samples []OccSample, spans []Span, intervalS float64) (OccupancyRaw, bool) {
	if len(samples) == 0 || len(spans) == 0 {
		return OccupancyRaw{}, false
	}
	sums := map[string]float64{}
	counts := map[string]int{}
	add := func(key string, v *float64) {
		if v == nil {
			return // missing reading != idle GPU
		}
		sums[key] += *v
		counts[key]++
	}
	for _, s := range samples {
		if !inAnySpan(s.TS, spans) {
			continue
		}
		add("nvsmi_util", s.NvsmiUtil)
		add("nvsmi_sm", s.NvsmiSM)
		add("dcgm_sm", s.DcgmSM)
	}
	metrics := map[string]*float64{}
	for _, k := range []string{"nvsmi_util", "nvsmi_sm", "dcgm_sm"} {
		if counts[k] == 0 {
			continue
		}
		mean := sums[k] / float64(counts[k])
		metrics[k] = &mean
	}
	if len(metrics) == 0 {
		return OccupancyRaw{}, false
	}
	// Same primary preference as the sampler: DCGM SM-activity is the truest source,
	// then nvidia-smi dmon sm%, then the coarse utilization.gpu (§8). Keeping the
	// preference identical means switching scope changes the WINDOW only, never which
	// metric K stands on — so the before/after comparison is apples-to-apples.
	primaryKey := ""
	for _, k := range []string{"dcgm_sm", "nvsmi_sm", "nvsmi_util"} {
		if metrics[k] != nil {
			primaryKey = k
			break
		}
	}
	primary := *metrics[primaryKey]
	return OccupancyRaw{
		MeanOccupancy:   &primary,
		OccupancySource: primaryKey,
		Metrics:         metrics,
		MetricSamples:   counts,
		Samples:         counts[primaryKey],
		Source:          primaryKey,
		IntervalS:       intervalS,
		Measured:        true,
		Scope:           ScopeInference,
	}, true
}

// inAnySpan reports whether ts falls in any span (inclusive bounds).
func inAnySpan(ts float64, spans []Span) bool {
	for _, sp := range spans {
		if ts >= sp.StartUnix && ts <= sp.EndUnix {
			return true
		}
	}
	return false
}

// SpansSeconds is the total wall-clock covered by the spans, with OVERLAPS MERGED
// so shared time is never double-counted. Spans shouldn't overlap today (drains are
// sequential), but merging makes that an invariant of the arithmetic rather than an
// assumption a future concurrent drain could silently violate.
func SpansSeconds(spans []Span) float64 {
	if len(spans) == 0 {
		return 0
	}
	cp := make([]Span, len(spans))
	copy(cp, spans)
	sort.Slice(cp, func(i, j int) bool { return cp[i].StartUnix < cp[j].StartUnix })
	total := 0.0
	cur := cp[0]
	for _, sp := range cp[1:] {
		if sp.StartUnix > cur.EndUnix { // disjoint: bank the current span
			total += cur.EndUnix - cur.StartUnix
			cur = sp
			continue
		}
		if sp.EndUnix > cur.EndUnix { // overlapping: extend
			cur.EndUnix = sp.EndUnix
		}
	}
	return total + (cur.EndUnix - cur.StartUnix)
}

// OccScopeNote renders the one-line provenance a reader needs to interpret an
// occupancy number: which window it covers and how many samples back it. Printed
// next to every occupancy figure so a scope is never implicit.
func OccScopeNote(o OccupancyRaw) string {
	switch o.Scope {
	case ScopeInference:
		return fmt.Sprintf("inference-window occupancy (excludes the one-time @enter load), %d samples", o.Samples)
	case ScopeWholeRun, "":
		return fmt.Sprintf("WHOLE-RUN occupancy (includes the @enter model load — understates steady-state fill), %d samples", o.Samples)
	default:
		return fmt.Sprintf("occupancy scope %q, %d samples", o.Scope, o.Samples)
	}
}
