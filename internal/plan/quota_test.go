package plan

import (
	"context"
	"errors"
	"testing"

	truffleaws "github.com/spore-host/truffle/pkg/aws"
	truffleQuotas "github.com/spore-host/truffle/pkg/quotas"
)

// fakeQuotaGetter is a canned quotaGetter — no live AWS calls, mirroring the
// Resolver/Pricer fakes truffle_test.go and plan_test.go already use to keep
// this package's tests offline.
type fakeQuotaGetter struct {
	info *truffleQuotas.QuotaInfo
	err  error
}

func (f fakeQuotaGetter) GetQuotas(_ context.Context, _ string) (*truffleQuotas.QuotaInfo, error) {
	return f.info, f.err
}

// fakeCapsGetter is a canned capsGetter.
type fakeCapsGetter struct {
	caps *truffleaws.Capabilities
	err  error
}

func (f fakeCapsGetter) GetCapabilities(_ context.Context, _, _ string) (*truffleaws.Capabilities, error) {
	return f.caps, f.err
}

// TestQuotaCeiling_SpotHeadroomDividedByVCPUs mirrors the real calque#141
// incident's numbers: a G/VT Spot quota of 64 vCPUs, already 0 in use, and a
// g7e.2xlarge at 8 vCPUs each — the ceiling should be exactly 8.
func TestQuotaCeiling_SpotHeadroomDividedByVCPUs(t *testing.T) {
	q := fakeQuotaGetter{info: &truffleQuotas.QuotaInfo{
		Spot:      map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 64},
		SpotUsage: map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 0},
	}}
	c := fakeCapsGetter{caps: &truffleaws.Capabilities{Found: true, VCPUs: 8}}

	got, err := quotaCeiling(context.Background(), q, c, "g7e.2xlarge", "us-east-1", true)
	if err != nil {
		t.Fatalf("quotaCeiling() error = %v", err)
	}
	if got != 8 {
		t.Errorf("quotaCeiling() = %d, want 8", got)
	}
}

// TestQuotaCeiling_UsageReducesHeadroom: the real incident's SECOND failure
// mode — 8 already-running instances (64 vCPUs of usage) against the same 64
// vCPU quota leaves 0 headroom, i.e. 0 concurrent MORE instances fit.
func TestQuotaCeiling_UsageReducesHeadroom(t *testing.T) {
	q := fakeQuotaGetter{info: &truffleQuotas.QuotaInfo{
		Spot:      map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 64},
		SpotUsage: map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 64},
	}}
	c := fakeCapsGetter{caps: &truffleaws.Capabilities{Found: true, VCPUs: 8}}

	got, err := quotaCeiling(context.Background(), q, c, "g7e.2xlarge", "us-east-1", true)
	if err != nil {
		t.Fatalf("quotaCeiling() error = %v", err)
	}
	if got != 0 {
		t.Errorf("quotaCeiling() = %d, want 0", got)
	}
}

// TestQuotaCeiling_UsageOverQuotaClampsToZero: usage can transiently exceed
// quota (e.g. a quota just lowered, or a race with another caller's launch) —
// headroom must clamp to 0 rather than go negative and confuse a caller doing
// shards > ceiling arithmetic.
func TestQuotaCeiling_UsageOverQuotaClampsToZero(t *testing.T) {
	q := fakeQuotaGetter{info: &truffleQuotas.QuotaInfo{
		Spot:      map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 32},
		SpotUsage: map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 64},
	}}
	c := fakeCapsGetter{caps: &truffleaws.Capabilities{Found: true, VCPUs: 8}}

	got, err := quotaCeiling(context.Background(), q, c, "g7e.2xlarge", "us-east-1", true)
	if err != nil {
		t.Fatalf("quotaCeiling() error = %v", err)
	}
	if got != 0 {
		t.Errorf("quotaCeiling() = %d, want 0 (clamped, not negative)", got)
	}
}

// TestQuotaCeiling_OnDemandUsesOnDemandMaps: spot=false must read
// OnDemand/Usage, not Spot/SpotUsage.
func TestQuotaCeiling_OnDemandUsesOnDemandMaps(t *testing.T) {
	q := fakeQuotaGetter{info: &truffleQuotas.QuotaInfo{
		OnDemand: map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 32},
		Usage:    map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 16},
		// Spot maps populated too, with different numbers, to prove they're unused here.
		Spot:      map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 999},
		SpotUsage: map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 0},
	}}
	c := fakeCapsGetter{caps: &truffleaws.Capabilities{Found: true, VCPUs: 8}}

	got, err := quotaCeiling(context.Background(), q, c, "g7e.2xlarge", "us-east-1", false)
	if err != nil {
		t.Fatalf("quotaCeiling() error = %v", err)
	}
	if got != 2 { // (32-16)/8 = 2
		t.Errorf("quotaCeiling() = %d, want 2", got)
	}
}

// TestQuotaCeiling_QuotaLookupErrorPropagates: a quota API failure must
// surface as an error, not a silent 0 — the caller (fleetRun) treats an error
// distinctly from a genuine 0-headroom ceiling (leaking-and-unclamping vs.
// leaking-and-clamping-to-0).
func TestQuotaCeiling_QuotaLookupErrorPropagates(t *testing.T) {
	sentinel := errors.New("service quotas: access denied")
	q := fakeQuotaGetter{err: sentinel}
	c := fakeCapsGetter{caps: &truffleaws.Capabilities{Found: true, VCPUs: 8}}

	_, err := quotaCeiling(context.Background(), q, c, "g7e.2xlarge", "us-east-1", true)
	if !errors.Is(err, sentinel) {
		t.Errorf("quotaCeiling() error = %v, want it to wrap %v", err, sentinel)
	}
}

// TestQuotaCeiling_CapabilitiesNotFoundErrors: an instance type truffle can't
// find (bad spelling, not offered in region) must error rather than silently
// dividing by zero or returning a bogus ceiling.
func TestQuotaCeiling_CapabilitiesNotFoundErrors(t *testing.T) {
	q := fakeQuotaGetter{info: &truffleQuotas.QuotaInfo{
		Spot:      map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 64},
		SpotUsage: map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 0},
	}}
	c := fakeCapsGetter{caps: &truffleaws.Capabilities{Found: false}}

	_, err := quotaCeiling(context.Background(), q, c, "not-a-real-type", "us-east-1", true)
	if err == nil {
		t.Fatal("expected error for not-found capabilities, got nil")
	}
}

// TestQuotaCeiling_CapabilitiesErrorPropagates: a DescribeInstanceTypes
// failure (throttling, permissions) must surface, not silently zero out.
func TestQuotaCeiling_CapabilitiesErrorPropagates(t *testing.T) {
	sentinel := errors.New("ec2: throttled")
	q := fakeQuotaGetter{info: &truffleQuotas.QuotaInfo{
		Spot:      map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 64},
		SpotUsage: map[truffleQuotas.QuotaFamily]int32{truffleQuotas.FamilyG: 0},
	}}
	c := fakeCapsGetter{err: sentinel}

	_, err := quotaCeiling(context.Background(), q, c, "g7e.2xlarge", "us-east-1", true)
	if !errors.Is(err, sentinel) {
		t.Errorf("quotaCeiling() error = %v, want it to wrap %v", err, sentinel)
	}
}
