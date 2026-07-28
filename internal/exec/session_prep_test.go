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
