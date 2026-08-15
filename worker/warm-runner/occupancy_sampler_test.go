package warm

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sleepMS is a small polling sleep helper.
func sleepMS(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// The sampler's JSONL contract is what the Go windowing (internal/exec/occwindow.go)
// stands on: every line needs a `ts`, and a metric that didn't report must be null
// rather than a repeat of the last value. These tests run the real occupancy.py.
//
// No GPU here, so nvidia-smi/dcgmi are absent and every metric is null — which is
// exactly what makes the TIMESTAMP contract testable in CI: the ts must be present
// and monotonic even when no metric reports.

func occupancyScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("occupancy.py")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// runSampler runs occupancy.py briefly, SIGTERMs it, and returns (summary, jsonl).
func runSampler(t *testing.T, interval string) (map[string]any, []string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "occ.jsonl")
	cmd := exec.Command(python(t)[0], occupancyScript(t), "sample", "--interval", interval, "--out", out)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sampler: %v", err)
	}
	// Let a few ticks land, then ask it to stop the way warmd does.
	waitForLines(t, out, 3)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal sampler: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("sampler exited badly: %v", err)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &summary); err != nil {
		t.Fatalf("summary not JSON (%q): %v", stdout.String(), err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	var lines []string
	for _, ln := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	return summary, lines
}

// waitForLines polls until the JSONL has at least n lines (or the test times out).
func waitForLines(t *testing.T, path string, n int) {
	t.Helper()
	deadline := 20 // ~10s at 500ms
	for i := 0; i < deadline; i++ {
		if b, err := os.ReadFile(path); err == nil {
			if len(strings.Split(strings.TrimSpace(string(b)), "\n")) >= n {
				return
			}
		}
		sleepMS(500)
	}
	t.Fatalf("sampler never wrote %d lines to %s", n, path)
}

// TestSamplerEmitsTimestamps is the #71 contract: without `ts` the control plane
// cannot attribute a sample to the inference window, and occupancy is stuck being a
// whole-run (load-contaminated) mean.
func TestSamplerEmitsTimestamps(t *testing.T) {
	_, lines := runSampler(t, "0.2")
	if len(lines) < 2 {
		t.Fatalf("got %d sample lines, want >= 2", len(lines))
	}
	var prev float64
	for i, ln := range lines {
		var s struct {
			TS float64 `json:"ts"`
		}
		if err := json.Unmarshal([]byte(ln), &s); err != nil {
			t.Fatalf("line %d not JSON: %v (%q)", i, err, ln)
		}
		if s.TS <= 0 {
			t.Fatalf("line %d has no usable ts: %q", i, ln)
		}
		if s.TS < 1_600_000_000 {
			t.Errorf("line %d ts=%v is not a unix epoch second (must share warmd's basis)", i, s.TS)
		}
		if i > 0 && s.TS < prev {
			t.Errorf("line %d ts went backwards: %v < %v", i, s.TS, prev)
		}
		prev = s.TS
	}
}

// TestSamplerReportsNullNotStaleValues proves a metric that didn't report is null on
// that tick. The old code wrote series[-1] — the last KNOWN value — so a missed tick
// silently duplicated a stale reading into the stream the windowed mean is computed
// from, fabricating a measurement. With no GPU present, every metric must be null.
func TestSamplerReportsNullNotStaleValues(t *testing.T) {
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		t.Skip("GPU host: this test asserts the no-GPU null contract")
	}
	_, lines := runSampler(t, "0.2")
	for i, ln := range lines {
		var s map[string]any
		if err := json.Unmarshal([]byte(ln), &s); err != nil {
			t.Fatalf("line %d not JSON: %v", i, err)
		}
		for _, k := range []string{"nvsmi_util", "nvsmi_sm", "dcgm_sm"} {
			v, present := s[k]
			if !present {
				t.Errorf("line %d missing key %q (the window parser expects all three)", i, k)
				continue
			}
			if v != nil {
				t.Errorf("line %d: %s = %v with no GPU present; want null (never a stale repeat)", i, k, v)
			}
		}
	}
}

// TestSamplerSummaryDeclaresWholeRunScope proves the sampler labels its own mean as
// whole-run. Its lifetime spans the model load, so an unlabeled number invites
// exactly the misreading #71 is about.
func TestSamplerSummaryDeclaresWholeRunScope(t *testing.T) {
	summary, _ := runSampler(t, "0.2")
	if got := summary["scope"]; got != "whole_run" {
		t.Errorf("summary scope = %v, want \"whole_run\"", got)
	}
	// Back-compat: the fields the Go OccupancyRaw already reads must survive.
	for _, k := range []string{"mean_occupancy", "occupancy_source", "metrics", "samples", "measured"} {
		if _, ok := summary[k]; !ok {
			t.Errorf("summary lost field %q", k)
		}
	}
}
