package plan

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/spore-host/calque/internal/target"
)

// apiErr is a fake smithy.APIError with a chosen code, to drive errorCode().
type apiErr struct{ code string }

func (e apiErr) Error() string                 { return e.code }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.code }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultServer }

var _ smithy.APIError = apiErr{}

// TestErrorCodeExtractsThroughWrapping proves errorCode unwraps to the
// underlying smithy.APIError the same way the old local classify() did —
// this is the one piece of Acquirer's own logic (calque#106's migration to
// lagotto/pkg/snipe.Snipe) that stays LOCAL and pure enough to test offline;
// Snipe's own retry/backoff/AZ-sweep/deadline/classify behavior is covered by
// lagotto/pkg/snipe's own test suite (TestSnipe_AcquiresAfterCapacityRetries,
// TestSnipe_TerminalStopsImmediately, TestSnipe_UnknownFailureCappedThenGivesUp,
// TestSnipe_DeadlineReached, TestSnipe_SubnetPerPlacement, etc. — 16 tests,
// confirmed by inspection to cover the equivalent guarantees calque's own
// now-deleted TestAcquireRetriesThenLands/TestAcquireSweepsAZs/
// TestAcquireDeadline/TestAcquireUnknownFailsFast/TestAcquireTerminalFailsFast
// exercised), not re-tested here — the same trust boundary calque already
// extends to spawn.launcher.Provision itself.
func TestErrorCodeExtractsThroughWrapping(t *testing.T) {
	cases := []struct {
		err    error
		wantOK bool
		want   string
	}{
		{nil, false, ""},
		{apiErr{"InsufficientInstanceCapacity"}, true, "InsufficientInstanceCapacity"},
		{fmt.Errorf("wrapped: %w", apiErr{"VcpuLimitExceeded"}), true, "VcpuLimitExceeded"},
		{errors.New("connection reset"), false, ""},
	}
	for _, c := range cases {
		got, ok := errorCode(c.err)
		if ok != c.wantOK || got != c.want {
			t.Errorf("errorCode(%v) = (%q, %v), want (%q, %v)", c.err, got, ok, c.want, c.wantOK)
		}
	}
}

// fakeResolver / TestFillTarget: card -> smallest candidate.
type fakeResolver struct{ cands []Candidate }

func (f fakeResolver) Resolve(_ string) ([]Candidate, error) { return f.cands, nil }

func TestPickSmallestAndFill(t *testing.T) {
	r := fakeResolver{cands: []Candidate{
		{Instance: "g7e.48xlarge", Family: "g7e"},
		{Instance: "g7e.2xlarge", Family: "g7e"},
		{Instance: "g7e.8xlarge", Family: "g7e"},
	}}
	tgt := &target.Target{Card: "RTX PRO 6000"}
	if err := FillTarget(tgt, r); err != nil {
		t.Fatal(err)
	}
	if tgt.Instance != "g7e.2xlarge" {
		t.Errorf("picked %q, want smallest g7e.2xlarge", tgt.Instance)
	}
}

// TestFillTargetRefreshesSharingModeFromRealInstance (calque#105): FillTarget
// must set SharingMode from the ACTUAL resolved instance's family, overriding
// whatever provisional value the Recommender stub set for DefaultCard alone —
// this matters once a caller resolves a DIFFERENT card than DefaultCard (a
// future real Recommender's job) or overrides --instance to a different
// family than the stub assumed.
func TestFillTargetRefreshesSharingModeFromRealInstance(t *testing.T) {
	r := fakeResolver{cands: []Candidate{{Instance: "g6.2xlarge", Family: "g6"}}}
	// Simulate a stub that guessed g7e's mode (MIG) before the real instance
	// (g6, MPS-only) was known.
	tgt := &target.Target{Card: "L4", SharingMode: target.MIG}
	if err := FillTarget(tgt, r); err != nil {
		t.Fatal(err)
	}
	if tgt.SharingMode != target.MPS {
		t.Errorf("SharingMode = %q, want %q (g6's real, hardware-verified mode) — FillTarget did not refresh it from the resolved instance", tgt.SharingMode, target.MPS)
	}
}

// TestFillTargetLeavesSharingModeUntouchedForUnknownFamily: a family with no
// table entry must not silently clear whatever the Recommender set — an
// unentered family is a gap to surface (a future leak), not something
// FillTarget should paper over by zeroing a previously-set value.
func TestFillTargetLeavesSharingModeUntouchedForUnknownFamily(t *testing.T) {
	r := fakeResolver{cands: []Candidate{{Instance: "p5.48xlarge", Family: "p5"}}}
	tgt := &target.Target{Card: "H100", SharingMode: target.MIG}
	if err := FillTarget(tgt, r); err != nil {
		t.Fatal(err)
	}
	if tgt.SharingMode != target.MIG {
		t.Errorf("SharingMode = %q, want unchanged %q (p5 has no table entry; FillTarget must not zero it)", tgt.SharingMode, target.MIG)
	}
}
