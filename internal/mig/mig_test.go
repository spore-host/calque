package mig

import (
	"errors"
	"strings"
	"testing"
)

// TestProfilesForKnownFamilies proves the catalog matches the live hardware
// findings in docs/gpu-sharing-support-matrix.md exactly — g7 and g7e each
// have their confirmed profile sets, and an unknown family returns ok=false
// rather than an empty-but-successful result.
func TestProfilesForKnownFamilies(t *testing.T) {
	g7, ok := ProfilesFor("g7")
	if !ok || len(g7) != 2 {
		t.Fatalf("ProfilesFor(g7) = %v, ok=%v; want 2 profiles", g7, ok)
	}
	g7e, ok := ProfilesFor("g7e")
	if !ok || len(g7e) != 3 {
		t.Fatalf("ProfilesFor(g7e) = %v, ok=%v; want 3 profiles", g7e, ok)
	}
	if _, ok := ProfilesFor("g6"); ok {
		t.Error("ProfilesFor(g6) = ok=true; want false (g6 has no MIG profiles — confirmed not MIG-capable)")
	}
	if _, ok := ProfilesFor("p5"); ok {
		t.Error("ProfilesFor(p5) = ok=true; want false (uncataloged family, must not silently succeed)")
	}
}

// TestPickLayoutMaximizesInstanceCount proves the "dumb" tie-break rule:
// most instances wins (more concurrent tenants), matching
// plan.PickSmallest's own deliberately-dumb philosophy.
func TestPickLayoutMaximizesInstanceCount(t *testing.T) {
	cases := []struct {
		family      string
		wantProfile string
		wantCount   int
	}{
		{"g7", "1g.16gb", 2},  // 2 instances beats 2g.32gb's 1
		{"g7e", "1g.24gb", 4}, // 4 instances beats 2g.48gb's 2 and 4g.96gb's 1
	}
	for _, c := range cases {
		p, count, err := PickLayout(c.family)
		if err != nil {
			t.Fatalf("PickLayout(%q): %v", c.family, err)
		}
		if p.Name != c.wantProfile || count != c.wantCount {
			t.Errorf("PickLayout(%q) = (%q, %d), want (%q, %d)", c.family, p.Name, count, c.wantProfile, c.wantCount)
		}
	}
}

// TestPickLayoutErrorsOnUnknownFamily proves an uncataloged family returns a
// typed error rather than a zero-value Profile that looks like a valid pick.
func TestPickLayoutErrorsOnUnknownFamily(t *testing.T) {
	_, _, err := PickLayout("g6")
	if err == nil {
		t.Fatal("PickLayout(g6) = nil error, want ErrNoProfiles (g6 has no MIG profiles)")
	}
	var wantType *ErrNoProfiles
	if !errors.As(err, &wantType) {
		t.Errorf("PickLayout(g6) error = %v (%T), want *ErrNoProfiles", err, err)
	}
}

// TestProvisionScriptCarvesExactCount proves the emitted script's -cgi
// argument lists the profile name exactly `count` times (comma-separated) —
// nvidia-smi mig -cgi's actual syntax for "create N instances of this
// profile in one call".
func TestProvisionScriptCarvesExactCount(t *testing.T) {
	script := ProvisionScript("0000:2F:00.0", Profile{Name: "1g.16gb"}, 2)
	if !strings.Contains(script, "-cgi 1g.16gb,1g.16gb -C") {
		t.Errorf("script does not carve exactly 2 instances of 1g.16gb:\n%s", script)
	}
	if !strings.Contains(script, "-mig 1") {
		t.Errorf("script does not enable MIG mode:\n%s", script)
	}
	if !strings.Contains(script, "0000:2F:00.0") {
		t.Errorf("script does not target the given GPU id:\n%s", script)
	}
	// MIG mode enable must precede the instance-carving call, or the -cgi
	// call runs against a non-MIG-mode GPU and fails.
	migIdx := strings.Index(script, "-mig 1")
	cgiIdx := strings.Index(script, "-cgi")
	if migIdx < 0 || cgiIdx < 0 || cgiIdx < migIdx {
		t.Errorf("-mig 1 must precede -cgi:\n%s", script)
	}
}

// TestProvisionScriptSingleInstance proves the count=1 case doesn't emit a
// trailing/leading comma (an off-by-one in the join logic would show up here).
func TestProvisionScriptSingleInstance(t *testing.T) {
	script := ProvisionScript("0000:2F:00.0", Profile{Name: "4g.96gb"}, 1)
	if !strings.Contains(script, "-cgi 4g.96gb -C") {
		t.Errorf("single-instance script has a stray comma or wrong profile:\n%s", script)
	}
}

func TestSliceUUIDsScriptTargetsGivenGPU(t *testing.T) {
	script := SliceUUIDsScript("0000:2B:00.0")
	if !strings.Contains(script, "0000:2B:00.0") {
		t.Errorf("script does not target the given GPU id:\n%s", script)
	}
	if !strings.Contains(script, "MIG-") {
		t.Errorf("script does not filter for MIG UUID pattern:\n%s", script)
	}
}
