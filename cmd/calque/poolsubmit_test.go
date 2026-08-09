package main

import (
	"io"
	"os"
	"strings"
	"testing"

	calexec "github.com/spore-host/calque/internal/exec"
	calpool "github.com/spore-host/calque/internal/pool"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written to it, so emitKForPoolClaim's printed verdict/notes
// (its only observable output — it returns no struct) can be asserted on.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out)
}

func testRealOpts(t *testing.T) realOpts {
	t.Helper()
	return realOpts{instance: "g7e.2xlarge", ratesFP: "../../config/rates.json"}
}

func testResults() []warm.Result {
	return []warm.Result{
		{Index: 0, Result: "a", Seconds: 0.5},
		{Index: 1, Result: "b", Seconds: 0.6},
	}
}

// TestEmitKForPoolClaim_MeasuredOccupancyIsUsedNotHardcoded (calque#116): once
// a pool worker's occupancy.py sampler produces a real per-claim measurement
// (internal/pool.Worker.runOne, internal/pool.Summary.Occupancy), emitK
// ForPoolClaim must fold THAT number into cost.Measured, not the pre-#116
// hardcoded Occupancy:1 (100%) full-fill placeholder. A measured occupancy
// below 100% must show up as a fraction well under 100% in the printed
// verdict, and the output must NOT contain the old "reported as 100%... not
// measured" disclaimer this fix retires for the measured case.
func TestEmitKForPoolClaim_MeasuredOccupancyIsUsedNotHardcoded(t *testing.T) {
	mean := 0.42
	summary := calpool.Summary{
		WarmHit: true, EnterSecondsPaid: 0,
		Occupancy: calexec.OccupancyRaw{
			MeanOccupancy: &mean, OccupancySource: "dcgm_sm", Samples: 10,
			Source: "dcgm_sm", IntervalS: 1.0, Measured: true, Scope: calexec.ScopeInference,
		},
	}

	out := captureStdout(t, func() {
		if err := emitKForPoolClaim(testRealOpts(t), testResults(), summary); err != nil {
			t.Fatalf("emitKForPoolClaim: %v", err)
		}
	})

	if !strings.Contains(out, "42%") {
		t.Errorf("verdict does not show the measured 42%% occupancy; got:\n%s", out)
	}
	if strings.Contains(out, "reported as 100%") {
		t.Errorf("verdict still claims occupancy is a hardcoded 100%% placeholder despite a real measurement; got:\n%s", out)
	}
	if strings.Contains(out, "occupancy 100%") {
		t.Errorf("verdict's workload line still shows 100%% occupancy despite a 42%% measurement; got:\n%s", out)
	}
	if !strings.Contains(out, "REAL per-claim measurement") {
		t.Errorf("verdict does not credit the number as a real per-claim measurement; got:\n%s", out)
	}
}

// TestEmitKForPoolClaim_UnmeasuredOccupancyFallsBackHonestly: when the
// sampler could not measure anything for this claim (Summary.Occupancy is
// zero-value, Measured=false — e.g. it failed to start, calque#116's
// best-effort fallback), emitKForPoolClaim must still run (never crash the
// claim) and must say plainly that occupancy is an unmeasured placeholder,
// not silently present the 100% fallback as a real number.
func TestEmitKForPoolClaim_UnmeasuredOccupancyFallsBackHonestly(t *testing.T) {
	summary := calpool.Summary{WarmHit: false, EnterSecondsPaid: 30}
	// Occupancy left zero-value: Measured=false, MeanOccupancy=nil.

	out := captureStdout(t, func() {
		if err := emitKForPoolClaim(testRealOpts(t), testResults(), summary); err != nil {
			t.Fatalf("emitKForPoolClaim: %v", err)
		}
	})

	if !strings.Contains(out, "unmeasured for this claim") {
		t.Errorf("verdict does not flag the fallback as unmeasured; got:\n%s", out)
	}
	if !strings.Contains(out, "occupancy 100%") {
		t.Errorf("fallback should still report the conservative 100%% placeholder in the workload line; got:\n%s", out)
	}
}
