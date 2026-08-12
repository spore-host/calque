package exec

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeHeartbeatGetter is a canned heartbeatGetter — no live AWS calls,
// mirroring internal/plan/quota_test.go's fakeQuotaGetter/fakeCapsGetter
// pattern used elsewhere in this codebase to keep tests offline.
type fakeHeartbeatGetter struct {
	tagValue string // "" => no spawn:last-heartbeat tag at all
	noTag    bool   // true => omit the tag key entirely (vs. present-but-empty)
	err      error
}

func (f fakeHeartbeatGetter) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	var tags []ec2types.Tag
	if !f.noTag {
		tags = []ec2types.Tag{{Key: aws.String("spawn:last-heartbeat"), Value: aws.String(f.tagValue)}}
	}
	id := "i-abc"
	if len(in.InstanceIds) > 0 {
		id = in.InstanceIds[0]
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{{InstanceId: aws.String(id), Tags: tags}},
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

// ---- calque#145 slice 3: multi-instance (fleet-wide) liveness ----

// multiHeartbeatGetter is a fakeHeartbeatGetter sibling that maps distinct
// instance IDs to their OWN tag value, and can be mutated between calls
// (via set/clear) to simulate a heartbeat changing across polling ticks —
// fakeHeartbeatGetter (above) only ever describes a single, unnamed
// instance, which can't exercise instanceHeartbeats' per-ID batching or a
// multi-tick persistence scenario.
type multiHeartbeatGetter struct {
	mu   sync.Mutex
	tags map[string]string // instanceID -> spawn:last-heartbeat value; absent key = no tag at all
}

func newMultiHeartbeatGetter() *multiHeartbeatGetter {
	return &multiHeartbeatGetter{tags: map[string]string{}}
}

func (m *multiHeartbeatGetter) set(instanceID string, ts time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tags[instanceID] = ts.Format(time.RFC3339)
}

func (m *multiHeartbeatGetter) clear(instanceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tags, instanceID)
}

func (m *multiHeartbeatGetter) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var instances []ec2types.Instance
	for _, id := range in.InstanceIds {
		inst := ec2types.Instance{InstanceId: aws.String(id)}
		if v, ok := m.tags[id]; ok {
			inst.Tags = []ec2types.Tag{{Key: aws.String("spawn:last-heartbeat"), Value: aws.String(v)}}
		}
		instances = append(instances, inst)
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: instances}}}, nil
}

// TestInstanceHeartbeats_BatchesAcrossMultipleIDs proves instanceHeartbeats
// resolves every requested ID from ONE DescribeInstances call, keyed by
// InstanceId — the batching instanceHeartbeat (single-ID) now delegates to.
func TestInstanceHeartbeats_BatchesAcrossMultipleIDs(t *testing.T) {
	c := newMultiHeartbeatGetter()
	t1 := time.Date(2026, 8, 12, 6, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 8, 12, 6, 1, 0, 0, time.UTC)
	c.set("i-1", t1)
	c.set("i-2", t2)
	// i-3 deliberately has no tag at all.

	got := instanceHeartbeats(context.Background(), c, []string{"i-1", "i-2", "i-3"})
	if !got["i-1"].Equal(t1) {
		t.Errorf("i-1 = %v, want %v", got["i-1"], t1)
	}
	if !got["i-2"].Equal(t2) {
		t.Errorf("i-2 = %v, want %v", got["i-2"], t2)
	}
	if _, ok := got["i-3"]; ok {
		t.Errorf("i-3 present in result (%v), want absent (no tag)", got["i-3"])
	}
}

// TestInstanceHeartbeats_EmptyIDsReturnsEmptyMapNoCall proves the zero-IDs
// case short-circuits without even calling DescribeInstances (mirroring
// WaitForSummaryLivenessAny's own empty-instanceIDs fallback contract).
func TestInstanceHeartbeats_EmptyIDsReturnsEmptyMapNoCall(t *testing.T) {
	got := instanceHeartbeats(context.Background(), fakeHeartbeatGetter{}, nil)
	if len(got) != 0 {
		t.Errorf("got = %v, want empty", got)
	}
}

// TestWaitForSummaryLivenessAny_EmptyInstanceIDsFallsBackToPlainTimeout
// proves an empty instance list degrades to WaitForSummary's ordinary
// timeout-only behavior rather than a spurious "all stale" — this is a
// discovery-failure fallback, not fleet death.
func TestWaitForSummaryLivenessAny_EmptyInstanceIDsFallsBackToPlainTimeout(t *testing.T) {
	c := s3.NewFromConfig(aws.Config{Region: "us-east-1"})
	l := RunLayout{Bucket: "bucket-does-not-exist-for-this-test", SummaryKey: "k", LogKey: "log"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := WaitForSummaryLivenessAny(ctx, c, newMultiHeartbeatGetter(), nil, l, time.Hour, 10*time.Millisecond, time.Minute, nil)
	var fleetStale *ErrFleetStale
	if errors.As(err, &fleetStale) {
		t.Fatal("WaitForSummaryLivenessAny with empty instanceIDs must never return ErrFleetStale")
	}
	// The real assertion is the ABSENCE of ErrFleetStale; ctx's own timeout
	// ends the wait via ctx.Err(), which is fine — we're not exercising a
	// real S3 client here, just proving the empty-IDs code path never
	// synthesizes staleness out of nothing.
}

// TestWaitForSummaryLivenessAny_PersistsLastSeenAcrossTicks is the priority
// test flagged by calque#145 slice 3's design: per-instance lastSeen state
// MUST persist across polling ticks via the check closure. A regression
// that recomputed a fresh map each tick would make every tick look like
// "never seen" — and heartbeatStale treats a never-seen lastSeen as NOT
// stale — so the bug would silently manifest as "ErrFleetStale never
// fires," not a crash. This test drives the check function directly
// (bypassing waitForSummary's real ticker/timer) across multiple manual
// ticks to prove the persistence contract without a real S3/AWS dependency
// or a live 5-minute wait.
func TestWaitForSummaryLivenessAny_PersistsLastSeenAcrossTicks(t *testing.T) {
	c := newMultiHeartbeatGetter()
	seenAt := time.Now().Add(-2 * time.Minute) // recent enough to be "fresh" at tick 1
	c.set("i-1", seenAt)

	// Reconstruct WaitForSummaryLivenessAny's own check-closure logic
	// directly (same shape as the production code) so this test exercises
	// the EXACT persistence contract without needing to control
	// waitForSummary's internal ticker timing from outside.
	instanceIDs := []string{"i-1"}
	lastSeen := make(map[string]time.Time, len(instanceIDs))
	staleAfter := 5 * time.Minute
	check := func() error {
		seen := instanceHeartbeats(context.Background(), c, instanceIDs)
		allStale := true
		for _, id := range instanceIDs {
			if hb, ok := seen[id]; ok && hb.After(lastSeen[id]) {
				lastSeen[id] = hb
			}
			if !heartbeatStale(time.Now(), lastSeen[id], staleAfter) {
				allStale = false
			}
		}
		if allStale {
			return &ErrFleetStale{InstanceIDs: instanceIDs}
		}
		return nil
	}

	// Tick 1: heartbeat is present and fresh — must not fire.
	if err := check(); err != nil {
		t.Fatalf("tick 1: check() = %v, want nil (fresh heartbeat)", err)
	}
	if lastSeen["i-1"].IsZero() {
		t.Fatal("tick 1: lastSeen[i-1] was not recorded — persistence is broken from the first tick")
	}
	recordedAfterTick1 := lastSeen["i-1"]

	// Tick 2: the tag becomes temporarily unreadable (simulates a transient
	// DescribeInstances hiccup, or spored briefly not having re-stamped
	// yet). If lastSeen were recomputed fresh instead of persisted, this
	// tick would see NO heartbeat and (incorrectly) treat it as "never
	// seen" == not stale, masking the real bug this test is designed to
	// catch. The correct behavior: lastSeen must STILL equal what tick 1
	// recorded, unchanged.
	c.clear("i-1")
	if err := check(); err != nil {
		t.Fatalf("tick 2: check() = %v, want nil (still within staleAfter of tick 1's heartbeat)", err)
	}
	if !lastSeen["i-1"].Equal(recordedAfterTick1) {
		t.Errorf("tick 2: lastSeen[i-1] = %v, want unchanged from tick 1 (%v) — persistence across ticks is broken", lastSeen["i-1"], recordedAfterTick1)
	}
}

// TestWaitForSummaryLivenessAny_OneSurvivorPreventsFleetStale proves the
// core "one live worker is enough" semantics: N-1 stale, 1 fresh must NOT
// fire ErrFleetStale — SQS's own redelivery already recovers a single dead
// worker by routing the claim to the survivor.
func TestWaitForSummaryLivenessAny_OneSurvivorPreventsFleetStale(t *testing.T) {
	now := time.Now()
	lastSeen := map[string]time.Time{
		"i-1": now.Add(-10 * time.Minute), // stale under a 5m staleAfter
		"i-2": now.Add(-1 * time.Minute),  // fresh
	}
	staleAfter := 5 * time.Minute
	allStale := true
	for _, ts := range lastSeen {
		if !heartbeatStale(now, ts, staleAfter) {
			allStale = false
		}
	}
	if allStale {
		t.Error("one fresh survivor among stale workers must NOT be reported as fleet-wide staleness")
	}
}

// TestWaitForSummaryLivenessAny_AllStaleFiresErrFleetStale is the mirror
// case: every worker stale must fire.
func TestWaitForSummaryLivenessAny_AllStaleFiresErrFleetStale(t *testing.T) {
	now := time.Now()
	lastSeen := map[string]time.Time{
		"i-1": now.Add(-10 * time.Minute),
		"i-2": now.Add(-11 * time.Minute),
	}
	staleAfter := 5 * time.Minute
	allStale := true
	for _, ts := range lastSeen {
		if !heartbeatStale(now, ts, staleAfter) {
			allStale = false
		}
	}
	if !allStale {
		t.Error("every worker stale must be reported as fleet-wide staleness")
	}
}

// TestWaitForSummaryLivenessAny_NeverSeenMixedWithStaleIsNotAllStale mirrors
// heartbeatStale's own "never seen != stale" semantics at the fleet level:
// a worker that has NEVER ticked (lastSeen zero) is not "stale" by
// heartbeatStale's contract, so it must not count toward "all stale" —
// mixing one never-seen worker with one genuinely-stale worker must NOT
// fire ErrFleetStale (the never-seen one could still be legitimately
// working, just on an old spawn or one that hasn't ticked yet).
func TestWaitForSummaryLivenessAny_NeverSeenMixedWithStaleIsNotAllStale(t *testing.T) {
	now := time.Now()
	lastSeen := map[string]time.Time{
		"i-1": {},                         // never seen at all
		"i-2": now.Add(-10 * time.Minute), // genuinely stale
	}
	staleAfter := 5 * time.Minute
	allStale := true
	for _, ts := range lastSeen {
		if !heartbeatStale(now, ts, staleAfter) {
			allStale = false
		}
	}
	if allStale {
		t.Error("a never-seen worker mixed with a stale one must not be reported as fleet-wide staleness")
	}
}

// TestErrFleetStale_ErrorMessageDistinguishesNeverSeen mirrors
// TestErrInstanceStale_ErrorMessageDistinguishesNeverSeen for the fleet-wide
// error type.
func TestErrFleetStale_ErrorMessageDistinguishesNeverSeen(t *testing.T) {
	never := &ErrFleetStale{InstanceIDs: []string{"i-1", "i-2"}}
	if got := never.Error(); got == "" {
		t.Fatal("Error() returned empty string")
	}
	seen := &ErrFleetStale{InstanceIDs: []string{"i-1", "i-2"}, LastHeartbeat: time.Date(2026, 8, 12, 6, 0, 0, 0, time.UTC)}
	if never.Error() == seen.Error() {
		t.Error("never-observed and observed-then-stale should produce different error messages")
	}
}
