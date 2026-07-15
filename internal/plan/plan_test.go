package plan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/target"
)

// apiErr is a fake smithy.APIError with a chosen code, to drive classify().
type apiErr struct{ code string }

func (e apiErr) Error() string                 { return e.code }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.code }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultServer }

var _ smithy.APIError = apiErr{}

// scriptedLauncher returns the queued errors in order, then succeeds.
type scriptedLauncher struct {
	errs  []error
	calls int
}

func (s *scriptedLauncher) Provision(_ context.Context, instanceType, region string) (LaunchOutcome, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) {
		return LaunchOutcome{}, s.errs[i]
	}
	return LaunchOutcome{InstanceID: "i-abc123", AvailabilityZone: region + "a", PublicIP: "1.2.3.4", State: "pending"}, nil
}

// noSleep makes retries instant so tests don't wait.
func noSleep(_ context.Context, _ time.Duration) error { return nil }

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want failureKind
	}{
		{nil, failureNone},
		{apiErr{"InsufficientInstanceCapacity"}, failureCapacity},
		{apiErr{"Server.InsufficientInstanceCapacity"}, failureCapacity},
		{apiErr{"VcpuLimitExceeded"}, failureTerminal},
		{apiErr{"UnauthorizedOperation"}, failureTerminal},
		{apiErr{"ParameterNotFound"}, failureTerminal},              // spawn AMI/SSM misconfig -> fail fast
		{apiErr{"SomeNovelCode"}, failureUnknown},                   // unknown AWS -> bounded retry
		{errors.New("connection reset"), failureUnknown},            // non-AWS -> bounded retry
		{apiErr{"WeirdInsufficientCapacityThing"}, failureCapacity}, // substring fallback
	}
	for _, c := range cases {
		if got, _ := classify(c.err); got != c.want {
			t.Errorf("classify(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestAcquireRetriesThenLands(t *testing.T) {
	rep := &leak.Report{}
	var progress []string
	acq := &Acquirer{
		Launcher: &scriptedLauncher{errs: []error{
			apiErr{"InsufficientInstanceCapacity"},
			apiErr{"InsufficientInstanceCapacity"},
		}},
		Report:       rep,
		PollInterval: time.Millisecond,
		OnProgress:   func(a int, code string, w time.Duration) { progress = append(progress, code) },
		sleep:        noSleep,
	}
	tgt := &target.Target{Card: "RTX PRO 6000", Instance: "g7e.2xlarge"}
	got, err := acq.Acquire(context.Background(), tgt, "us-west-2")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.InstanceID != "i-abc123" {
		t.Errorf("instance = %q", got.InstanceID)
	}
	if tgt.Region != "us-west-2" {
		t.Errorf("target region not filled: %q", tgt.Region)
	}
	if len(progress) != 2 {
		t.Errorf("expected 2 capacity-wait progress events, got %d", len(progress))
	}
	// time-to-acquire after retries should be logged as free ground truth (§5).
	if rep.Len() == 0 {
		t.Error("expected a time-to-acquire leak after retries")
	}
}

func TestAcquireTerminalFailsFast(t *testing.T) {
	acq := &Acquirer{
		Launcher: &scriptedLauncher{errs: []error{apiErr{"VcpuLimitExceeded"}}},
		sleep:    noSleep,
	}
	tgt := &target.Target{Card: "RTX PRO 6000", Instance: "g7e.2xlarge"}
	_, err := acq.Acquire(context.Background(), tgt, "us-west-2")
	if err == nil {
		t.Fatal("expected terminal failure to abort, got success")
	}
	// A quota error must NOT be retried into the deadline.
	if l := acq.Launcher.(*scriptedLauncher); l.calls != 1 {
		t.Errorf("terminal error should stop after 1 attempt, got %d", l.calls)
	}
}

func TestAcquireDeadline(t *testing.T) {
	// Always-capacity launcher + a deadline in the past-ish: should give up.
	base := time.Unix(1_700_000_000, 0)
	steps := 0
	acq := &Acquirer{
		Launcher:     &scriptedLauncher{errs: []error{apiErr{"InsufficientInstanceCapacity"}, apiErr{"InsufficientInstanceCapacity"}, apiErr{"InsufficientInstanceCapacity"}, apiErr{"InsufficientInstanceCapacity"}, apiErr{"InsufficientInstanceCapacity"}}},
		PollInterval: time.Second,
		Deadline:     3 * time.Second,
		sleep:        noSleep,
		now: func() time.Time {
			// advance 2s per call so the deadline trips after a couple of tries
			t := base.Add(time.Duration(steps) * 2 * time.Second)
			steps++
			return t
		},
	}
	tgt := &target.Target{Card: "RTX PRO 6000", Instance: "g7e.2xlarge"}
	_, err := acq.Acquire(context.Background(), tgt, "us-west-2")
	if err == nil {
		t.Fatal("expected deadline give-up, got success")
	}
}

// TestAcquireUnknownFailsFast: an unrecognized error must NOT loop to the
// deadline (the ParameterNotFound lesson — a config bug masqueraded as capacity
// for 18 min). It should bail after a few consecutive unknowns.
func TestAcquireUnknownFailsFast(t *testing.T) {
	// 100 unknown errors queued, but we should stop after maxUnknown (3) + 1.
	errs := make([]error, 100)
	for i := range errs {
		errs[i] = apiErr{"ParameterNotFoundLikeButUnclassified"}
	}
	l := &scriptedLauncher{errs: errs}
	acq := &Acquirer{Launcher: l, PollInterval: time.Millisecond, Deadline: time.Hour, sleep: noSleep}
	tgt := &target.Target{Card: "RTX PRO 6000", Instance: "g7e.2xlarge"}
	_, err := acq.Acquire(context.Background(), tgt, "us-west-2")
	if err == nil {
		t.Fatal("expected fail-fast on repeated unknown errors")
	}
	if l.calls > 5 { // maxUnknown=3, so ~4 calls; certainly not 100 or deadline-bound
		t.Errorf("unknown errors should fail fast, but made %d attempts", l.calls)
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
