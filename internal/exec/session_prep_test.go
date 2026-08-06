package exec

import (
	"strings"
	"testing"
)

// TestSessionPrepCompletionSignalling guards the fix for the prep-visibility bug
// (#18): the done marker must be written AFTER the pull, a heartbeat must stream
// the log while the pull runs, and there must be exactly one EXIT trap (bash keeps
// only the last, so a second trap would silently drop the heartbeat kill).
func TestSessionPrepCompletionSignalling(t *testing.T) {
	p := SessionPrep{
		BaseImage: "vllm/vllm-openai:latest", Bucket: "b", WorkerDir: "/tmp/calque",
		Region: "us-east-1", LogKey: "sessions/s/prep.log", DoneKey: "sessions/s/prep.done",
	}
	cmd := p.PrepCommand("sessions/s/artifacts")

	// Done marker uploaded to the DoneKey.
	if !strings.Contains(cmd, "s3://b/sessions/s/prep.done") {
		t.Errorf("prep does not write the done marker:\n%s", cmd)
	}
	// Heartbeat streams the log while running.
	if !strings.Contains(cmd, "while true; do sleep 20;") {
		t.Errorf("prep has no heartbeat loop:\n%s", cmd)
	}
	// Exactly one EXIT trap — a second would clobber the first in bash.
	if n := strings.Count(cmd, "trap "); n != 1 {
		t.Errorf("want exactly 1 trap, got %d:\n%s", n, cmd)
	}

	// Ordering: the done marker MUST come after the docker pull, or a slow/failed
	// pull could still signal success. `set -e` + this ordering is the guarantee.
	pull := strings.Index(cmd, "docker pull")
	done := strings.LastIndex(cmd, "prep.done")
	if pull < 0 || done < 0 || done < pull {
		t.Errorf("done marker (idx %d) must come after docker pull (idx %d):\n%s", done, pull, cmd)
	}
}

// TestSessionPrepNoDoneKeyOmitsMarker: an empty DoneKey (older callers) must not
// emit a marker line — back-compat.
func TestSessionPrepNoDoneKeyOmitsMarker(t *testing.T) {
	p := SessionPrep{BaseImage: "img", Bucket: "b", LogKey: "k/prep.log"}
	cmd := p.PrepCommand("k/art")
	if strings.Contains(cmd, "prep.done") {
		t.Errorf("no DoneKey set but marker emitted:\n%s", cmd)
	}
}

// TestTestRunCommandUploadsRawSamples proves the per-rung command uploads the
// sampler's RAW timestamped JSONL, not just its summary (#71). In the session path
// the sampler runs on the HOST (dcgmi isn't in the vLLM image) while warmd runs in
// the container, so warmd cannot see these samples — the control plane does the
// inference-window re-average, and it needs this file to do it.
func TestTestRunCommandUploadsRawSamples(t *testing.T) {
	cmd := TestRunCommand("img", "/tmp/calque", "us-east-1", "buk", "runs/x/manifest.json",
		"Qwen/Qwen2.5-1.5B-Instruct", "runs/x/test.log", "runs/x/occ.json", "runs/x/occ.jsonl")

	if !strings.Contains(cmd, "s3://buk/runs/x/occ.jsonl") {
		t.Errorf("command does not upload the raw sample stream:\n%s", cmd)
	}
	if !strings.Contains(cmd, "/tmp/calque-occ.jsonl") {
		t.Errorf("command does not reference the sampler's --out path:\n%s", cmd)
	}
	// The upload must happen AFTER the sampler is stopped, or the file is truncated.
	stop := strings.Index(cmd, "kill -TERM $OCC")
	up := strings.Index(cmd, "occ.jsonl s3://")
	if stop < 0 || up < 0 || up < stop {
		t.Errorf("raw samples must upload after the sampler is SIGTERM'd:\n%s", cmd)
	}
	// And the container's exit code must still be what the command returns.
	if !strings.Contains(cmd, "exit $RC") {
		t.Errorf("command lost its exit-code propagation:\n%s", cmd)
	}
}

// TestTestRunCommandNoSamplesKeyOmitsUpload proves the upload is opt-in: an empty key
// yields no raw-sample copy (back-compat with callers that only want the summary).
func TestTestRunCommandNoSamplesKeyOmitsUpload(t *testing.T) {
	cmd := TestRunCommand("img", "/tmp/calque", "us-east-1", "buk", "m.json", "", "log", "occ.json", "")
	if strings.Contains(cmd, ".jsonl s3://") {
		t.Errorf("no samples key given, but the command still uploads a JSONL:\n%s", cmd)
	}
}
