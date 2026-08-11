package exec

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeHeartbeatGetter is a canned heartbeatGetter — no live AWS calls,
// mirroring internal/plan/quota_test.go's fakeQuotaGetter/fakeCapsGetter
// pattern used elsewhere in this codebase to keep tests offline.
type fakeHeartbeatGetter struct {
	tagValue string // "" => no spawn:last-heartbeat tag at all
	noTag    bool   // true => omit the tag key entirely (vs. present-but-empty)
	err      error
}

func (f fakeHeartbeatGetter) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	var tags []ec2types.Tag
	if !f.noTag {
		tags = []ec2types.Tag{{Key: aws.String("spawn:last-heartbeat"), Value: aws.String(f.tagValue)}}
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{{Tags: tags}},
		}},
	}, nil
}

// TestInstanceHeartbeat_ParsesRFC3339Tag: the real spawn#497 shape — a
// present, well-formed spawn:last-heartbeat tag parses to its timestamp.
func TestInstanceHeartbeat_ParsesRFC3339Tag(t *testing.T) {
	want := time.Date(2026, 8, 11, 5, 44, 0, 0, time.UTC)
	c := fakeHeartbeatGetter{tagValue: want.Format(time.RFC3339)}
	got, ok := instanceHeartbeat(context.Background(), c, "i-abc")
	if !ok {
		t.Fatal("instanceHeartbeat() ok = false, want true")
	}
	if !got.Equal(want) {
		t.Errorf("instanceHeartbeat() = %v, want %v", got, want)
	}
}

// TestInstanceHeartbeat_MissingTagIsNotAnError: an older spawn (pre-v0.100.0)
// or an instance that hasn't ticked yet has no tag at all — this must report
// "no signal" (ok=false), not be conflated with staleness.
func TestInstanceHeartbeat_MissingTagIsNotAnError(t *testing.T) {
	c := fakeHeartbeatGetter{noTag: true}
	_, ok := instanceHeartbeat(context.Background(), c, "i-abc")
	if ok {
		t.Error("instanceHeartbeat() ok = true, want false for a missing tag")
	}
}

// TestInstanceHeartbeat_UnparsableTagIsNotAnError: a malformed tag value
// (shouldn't happen from spored itself, but must not panic/crash a caller)
// also reports "no signal" rather than erroring.
func TestInstanceHeartbeat_UnparsableTagIsNotAnError(t *testing.T) {
	c := fakeHeartbeatGetter{tagValue: "not-a-timestamp"}
	_, ok := instanceHeartbeat(context.Background(), c, "i-abc")
	if ok {
		t.Error("instanceHeartbeat() ok = true, want false for an unparsable tag")
	}
}

// TestInstanceHeartbeat_DescribeInstancesErrorIsNotFatal: a transient
// DescribeInstances failure (throttling, permissions) must degrade to "no
// signal," not propagate as an error — WaitForSummaryLiveness's whole design
// is that a liveness check failing to observe anything falls back to
// timeout-only behavior, never turns an unrelated AWS hiccup into a false
// staleness failure.
func TestInstanceHeartbeat_DescribeInstancesErrorIsNotFatal(t *testing.T) {
	c := fakeHeartbeatGetter{err: context.DeadlineExceeded}
	_, ok := instanceHeartbeat(context.Background(), c, "i-abc")
	if ok {
		t.Error("instanceHeartbeat() ok = true, want false when DescribeInstances itself errors")
	}
}

// TestHeartbeatStale_NeverSeenIsNotStale: lastSeen's zero value (no
// heartbeat ever observed at all) must NOT be treated as staleness — an
// older spawn, or one that simply hasn't ticked yet, shouldn't fail a run
// that's otherwise progressing fine via the summary/bootstrap-log poll.
func TestHeartbeatStale_NeverSeenIsNotStale(t *testing.T) {
	now := time.Date(2026, 8, 11, 6, 0, 0, 0, time.UTC)
	if heartbeatStale(now, time.Time{}, 5*time.Minute) {
		t.Error("heartbeatStale() = true, want false when no heartbeat has ever been observed")
	}
}

// TestHeartbeatStale_RecentIsNotStale: a heartbeat seen within staleAfter is
// still fresh.
func TestHeartbeatStale_RecentIsNotStale(t *testing.T) {
	now := time.Date(2026, 8, 11, 6, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-2 * time.Minute)
	if heartbeatStale(now, lastSeen, 5*time.Minute) {
		t.Error("heartbeatStale() = true, want false for a 2m-old heartbeat under a 5m staleAfter")
	}
}

// TestHeartbeatStale_OldIsStale: the real calque#141/#142/#143 scenario —
// an instance that WAS ticking has now gone silent for longer than
// staleAfter, and must be reported stale so a caller fails fast instead of
// dead-waiting the full deadline.
func TestHeartbeatStale_OldIsStale(t *testing.T) {
	now := time.Date(2026, 8, 11, 6, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-10 * time.Minute)
	if !heartbeatStale(now, lastSeen, 5*time.Minute) {
		t.Error("heartbeatStale() = false, want true for a 10m-old heartbeat under a 5m staleAfter")
	}
}

// TestHeartbeatStale_ExactlyAtBoundaryIsNotStale: staleAfter is an exclusive
// upper bound (elapsed > staleAfter, not >=) — an exact match hasn't yet
// exceeded the window.
func TestHeartbeatStale_ExactlyAtBoundaryIsNotStale(t *testing.T) {
	now := time.Date(2026, 8, 11, 6, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-5 * time.Minute)
	if heartbeatStale(now, lastSeen, 5*time.Minute) {
		t.Error("heartbeatStale() = true, want false exactly at the staleAfter boundary")
	}
}

// TestErrInstanceStale_ErrorMessageDistinguishesNeverSeen: the two ErrInstanceStale
// shapes (never observed at all vs. observed-then-went-quiet) should produce
// distinguishable messages for a human reading a failed run's error.
func TestErrInstanceStale_ErrorMessageDistinguishesNeverSeen(t *testing.T) {
	never := &ErrInstanceStale{InstanceID: "i-abc"}
	if got := never.Error(); got == "" {
		t.Fatal("Error() returned empty string")
	}
	seen := &ErrInstanceStale{InstanceID: "i-abc", LastHeartbeat: time.Date(2026, 8, 11, 5, 55, 0, 0, time.UTC)}
	if never.Error() == seen.Error() {
		t.Error("never-observed and observed-then-stale should produce different error messages")
	}
}
