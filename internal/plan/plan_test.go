package plan

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/smithy-go"

	spawnaws "github.com/spore-host/spawn/pkg/aws"

	"github.com/spore-host/calque/internal/leak"
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

// TestBuildSnipeTargetsSingleRegion (calque#95): a single-region call — the
// shape Acquire itself now uses via AcquireMultiRegion — must produce a
// primary snipe.Target carrying THAT region's placements/instance type/spot/
// launch config, and exactly ZERO fallbacks. This is the "existing
// single-region callers are unaffected" guarantee: Acquire's own new one-line
// body (regions=[]string{region}) must build the identical snipe.Target it
// built by hand before the #95 migration.
func TestBuildSnipeTargetsSingleRegion(t *testing.T) {
	cfg := spawnaws.LaunchConfig{Spot: true, AMI: "ami-123"}
	places := map[string][]Placement{
		"us-east-1": {{AZ: "us-east-1a", Subnet: "subnet-1"}, {AZ: "us-east-1b", Subnet: "subnet-2"}},
	}
	primary, fallbacks := buildSnipeTargets("g7e.2xlarge", []string{"us-east-1"}, places, cfg)

	if primary.InstanceType != "g7e.2xlarge" || primary.Region != "us-east-1" {
		t.Fatalf("primary = %+v, want InstanceType=g7e.2xlarge Region=us-east-1", primary)
	}
	if !primary.Spot {
		t.Errorf("primary.Spot = false, want true (from LaunchConfig)")
	}
	if primary.LaunchConfig.AMI != "ami-123" {
		t.Errorf("primary.LaunchConfig.AMI = %q, want %q", primary.LaunchConfig.AMI, "ami-123")
	}
	if len(primary.Placements) != 2 || primary.Placements[0].AZ != "us-east-1a" || primary.Placements[1].AZ != "us-east-1b" {
		t.Errorf("primary.Placements = %+v, want 2 placements for us-east-1 in order", primary.Placements)
	}
	if len(fallbacks) != 0 {
		t.Errorf("fallbacks = %+v, want none for a single-region call", fallbacks)
	}
}

// TestBuildSnipeTargetsMultiRegion (calque#95): regions beyond the first must
// become one snipe.Target each, IN ORDER, each carrying its OWN region's
// placements from placementsByRegion — proving cross-region fallback targets
// don't leak the primary region's AZ/subnet list (an AZ in one region says
// nothing about another).
func TestBuildSnipeTargetsMultiRegion(t *testing.T) {
	cfg := spawnaws.LaunchConfig{AMI: "ami-abc"}
	places := map[string][]Placement{
		"us-east-1":    {{AZ: "us-east-1a", Subnet: "subnet-a"}},
		"us-west-2":    {{AZ: "us-west-2b", Subnet: "subnet-b"}},
		"eu-central-1": {{AZ: "eu-central-1a", Subnet: "subnet-c"}},
	}
	primary, fallbacks := buildSnipeTargets("g7e.2xlarge",
		[]string{"us-east-1", "us-west-2", "eu-central-1"}, places, cfg)

	if primary.Region != "us-east-1" {
		t.Fatalf("primary.Region = %q, want us-east-1", primary.Region)
	}
	if len(fallbacks) != 2 {
		t.Fatalf("fallbacks = %+v, want exactly 2 (one per additional region)", fallbacks)
	}
	if fallbacks[0].Region != "us-west-2" || fallbacks[1].Region != "eu-central-1" {
		t.Errorf("fallback regions = [%s, %s], want [us-west-2, eu-central-1] IN ORDER",
			fallbacks[0].Region, fallbacks[1].Region)
	}
	if len(fallbacks[0].Placements) != 1 || fallbacks[0].Placements[0].AZ != "us-west-2b" {
		t.Errorf("fallbacks[0].Placements = %+v, want us-west-2's own placement, not us-east-1's", fallbacks[0].Placements)
	}
	if len(fallbacks[1].Placements) != 1 || fallbacks[1].Placements[0].AZ != "eu-central-1a" {
		t.Errorf("fallbacks[1].Placements = %+v, want eu-central-1's own placement", fallbacks[1].Placements)
	}
	for _, fb := range fallbacks {
		if fb.InstanceType != "g7e.2xlarge" {
			t.Errorf("fallback %s InstanceType = %q, want g7e.2xlarge (same as primary)", fb.Region, fb.InstanceType)
		}
		if fb.LaunchConfig.AMI != "ami-abc" {
			t.Errorf("fallback %s LaunchConfig.AMI = %q, want %q (shared base config)", fb.Region, fb.LaunchConfig.AMI, "ami-abc")
		}
	}
}

// TestBuildSnipeTargetsMissingRegionPlacements: a candidate region absent from
// placementsByRegion must sweep with a single EC2-chosen placement (nil/empty
// Placements) rather than erroring or silently borrowing another region's list.
func TestBuildSnipeTargetsMissingRegionPlacements(t *testing.T) {
	places := map[string][]Placement{"us-east-1": {{AZ: "us-east-1a"}}}
	_, fallbacks := buildSnipeTargets("g6.2xlarge", []string{"us-east-1", "ap-south-1"}, places, spawnaws.LaunchConfig{})
	if len(fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v, want 1", fallbacks)
	}
	if len(fallbacks[0].Placements) != 0 {
		t.Errorf("fallbacks[0].Placements = %+v, want empty (ap-south-1 has no entry in placementsByRegion)", fallbacks[0].Placements)
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
	if err := FillTarget(tgt, r, nil); err != nil {
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
	if err := FillTarget(tgt, r, nil); err != nil {
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
	if err := FillTarget(tgt, r, nil); err != nil {
		t.Fatal(err)
	}
	if tgt.SharingMode != target.MIG {
		t.Errorf("SharingMode = %q, want unchanged %q (p5 has no table entry; FillTarget must not zero it)", tgt.SharingMode, target.MIG)
	}
}

// TestFillTargetResolvesH100ToRealInstanceFamily (calque#134) proves the
// actual end-to-end fix live against truffle (no fake resolver): gpu="H100"
// must resolve to a REAL H100 instance family (p5.*), not always
// g7e.2xlarge regardless of what the script asked for — that silent
// discrepancy is exactly what this issue closes.
func TestFillTargetResolvesH100ToRealInstanceFamily(t *testing.T) {
	rep := &leak.Report{}
	tgt := &target.Target{Card: "H100"}
	if err := FillTarget(tgt, NewTruffleResolver(rep), rep); err != nil {
		t.Fatalf("FillTarget: %v", err)
	}
	if instanceFamily(tgt.Instance) != "p5" {
		t.Errorf("Instance = %q (family %q), want family p5 for card H100", tgt.Instance, instanceFamily(tgt.Instance))
	}
}

// TestFillTargetUnverifiedFamilyLeaksButSucceeds (calque#134): resolving a
// card to an instance family with no sharingModeByFamily entry (e.g. H100 ->
// p5, not one of the four hardware-verified families) must emit an
// informational leak, but FillTarget itself must still succeed — this is a
// documentation gap, not a runtime error, and nothing downstream may treat it
// as one.
func TestFillTargetUnverifiedFamilyLeaksButSucceeds(t *testing.T) {
	rep := &leak.Report{}
	r := fakeResolver{cands: []Candidate{{Instance: "p5.48xlarge", Family: "p5"}}}
	tgt := &target.Target{Card: "H100"}
	if err := FillTarget(tgt, r, rep); err != nil {
		t.Fatalf("FillTarget: %v (must succeed even for an unverified family)", err)
	}
	if tgt.Instance != "p5.48xlarge" {
		t.Errorf("Instance = %q, want p5.48xlarge", tgt.Instance)
	}
	if rep.Len() == 0 {
		t.Error("expected an informational leak noting the unverified family, got none")
	}
}

// TestFillTargetVerifiedFamilyDoesNotLeak: a resolved family that DOES have a
// sharingModeByFamily entry (e.g. g7e, DefaultCard's own family) must not
// trigger the new informational leak — it's only for the gap case.
func TestFillTargetVerifiedFamilyDoesNotLeak(t *testing.T) {
	rep := &leak.Report{}
	r := fakeResolver{cands: []Candidate{{Instance: "g7e.2xlarge", Family: "g7e"}}}
	tgt := &target.Target{Card: "RTX PRO 6000"}
	if err := FillTarget(tgt, r, rep); err != nil {
		t.Fatal(err)
	}
	if rep.Len() != 0 {
		t.Errorf("expected no leak for a verified family (g7e), got %d: %+v", rep.Len(), rep.Leaks)
	}
}

// TestResolveNormalizesMemorySuffix (calque#134/truffle#130): gpu="A100-80GB"
// doesn't resolve via truffle as spelled — Modal's documented memory-suffix
// form isn't in truffle's alias table yet — but the bare "A100" form does.
// Resolve must normalize and succeed, and leak that the normalization fired
// (visible, not silent).
func TestResolveNormalizesMemorySuffix(t *testing.T) {
	rep := &leak.Report{}
	r := NewTruffleResolver(rep)
	cands, err := r.Resolve("A100-80GB")
	if err != nil {
		t.Fatalf("Resolve(A100-80GB): %v (want success via memory-suffix normalization)", err)
	}
	if len(cands) == 0 {
		t.Fatal("no candidates for A100-80GB after normalization")
	}
	found := false
	for _, c := range rep.Leaks {
		if c.Detail != "" && containsAll(c.Detail, "A100-80GB", "A100") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a leak noting the A100-80GB -> A100 normalization, got: %+v", rep.Leaks)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
