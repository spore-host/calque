package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/calque/internal/mps"
)

// TestCheckoutReturnsValidSliceAndToken proves checkoutSlice hands back a
// non-empty slice ID belonging to the derived layout, plus a non-empty
// session token distinct from the slice ID (calque#118/#119).
func TestCheckoutReturnsValidSliceAndToken(t *testing.T) {
	dir := t.TempDir()
	sliceID, token, err := checkoutSlice(dir, "i-abc123", "alice", "mps", time.Hour, "g7e.2xlarge", 4)
	if err != nil {
		t.Fatal(err)
	}
	if sliceID == "" {
		t.Error("checkoutSlice returned an empty slice ID")
	}
	if token == "" {
		t.Error("checkoutSlice returned an empty session token")
	}
	if token == sliceID {
		t.Error("session token must not equal the slice ID")
	}

	// A second checkout for a different user must land on a DIFFERENT slice
	// (mirrors tenancy.Registry's own exclusivity invariant, now exercised
	// through the CLI-layer persistence wrapper).
	sliceID2, _, err := checkoutSlice(dir, "i-abc123", "bob", "mps", time.Hour, "g7e.2xlarge", 4)
	if err != nil {
		t.Fatal(err)
	}
	if sliceID2 == sliceID {
		t.Fatalf("bob got the same slice %q as alice; exclusivity violated", sliceID)
	}
}

// TestCheckinWithRightTokenSucceeds proves checkin with the token checkout
// minted succeeds and actually releases the slice via Registry.CheckIn —
// verified behaviorally: after checkin, the slice reports free, and a NEW
// checkout can claim it again (impossible if the underlying Registry still
// held it).
func TestCheckinWithRightTokenSucceeds(t *testing.T) {
	dir := t.TempDir()
	sliceID, token, err := checkoutSlice(dir, "i-abc123", "alice", "mig", time.Hour, "g7e.2xlarge", 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := checkinSlice(dir, sliceID, token); err != nil {
		t.Fatalf("checkin with the correct token failed: %v", err)
	}

	reports, err := sessionList(dir, "i-abc123")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reports {
		if r.SliceID == sliceID && r.Held {
			t.Fatalf("slice %s still reports held after a successful checkin", sliceID)
		}
	}

	// The freed slice must be available to a fresh checkout (proves
	// Registry.CheckIn actually ran, not just that state bookkeeping was
	// cleared client-side).
	sliceID2, _, err := checkoutSlice(dir, "i-abc123", "bob", "mig", time.Hour, "g7e.2xlarge", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sliceID2 != sliceID {
		t.Errorf("bob got %q, want the freed slice %q back", sliceID2, sliceID)
	}
}

// TestCheckinWithWrongTokenRefusedAndDoesNotCheckIn proves a checkin with a
// wrong token is refused with errWrongSessionToken and does NOT release the
// slice (i.e. Registry.CheckIn is never reached) — verified behaviorally by
// confirming the slice still reports held, under the SAME user, afterward.
func TestCheckinWithWrongTokenRefusedAndDoesNotCheckIn(t *testing.T) {
	dir := t.TempDir()
	sliceID, _, err := checkoutSlice(dir, "i-abc123", "alice", "mps", time.Hour, "g7e.2xlarge", 4)
	if err != nil {
		t.Fatal(err)
	}

	err = checkinSlice(dir, sliceID, "not-the-real-token")
	if err == nil {
		t.Fatal("checkin with a wrong token succeeded, want refusal")
	}
	if !errors.Is(err, errWrongSessionToken) {
		t.Errorf("checkin error = %v, want errWrongSessionToken", err)
	}

	reports, lerr := sessionList(dir, "i-abc123")
	if lerr != nil {
		t.Fatal(lerr)
	}
	found := false
	for _, r := range reports {
		if r.SliceID == sliceID {
			found = true
			if !r.Held {
				t.Fatalf("slice %s was released despite a wrong-token checkin (Registry.CheckIn must not have been called)", sliceID)
			}
			if r.UserID != "alice" {
				t.Errorf("slice %s holder = %q, want alice (unchanged)", sliceID, r.UserID)
			}
		}
	}
	if !found {
		t.Fatalf("slice %s missing from session list after refused checkin", sliceID)
	}
}

// TestCheckinUnknownSliceErrors proves checking in a slice ID that was never
// checked out (or belongs to no known instance state) is a reported error.
func TestCheckinUnknownSliceErrors(t *testing.T) {
	dir := t.TempDir()
	if err := checkinSlice(dir, "no-such-slice", "any-token"); err == nil {
		t.Fatal("checkin on an unknown slice succeeded, want an error")
	}
}

// TestSessionStatusReportsHeldVsTotal proves sessionStatus wraps
// Registry.Occupancy() correctly through the persisted-layout rebuild.
func TestSessionStatusReportsHeldVsTotal(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := checkoutSlice(dir, "i-abc123", "alice", "mps", time.Hour, "g7e.2xlarge", 3); err != nil {
		t.Fatal(err)
	}

	held, total, err := sessionStatus(dir, "i-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 (--slots 3)", total)
	}
	if held != 1 {
		t.Errorf("held = %d, want 1", held)
	}
}

// TestSessionStatusUnknownInstanceErrors proves status on an instance with
// no recorded checkout is a reported error, not a fabricated 0/0.
func TestSessionStatusUnknownInstanceErrors(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := sessionStatus(dir, "i-never-checked-out"); err == nil {
		t.Fatal("sessionStatus on an unknown instance succeeded, want an error")
	}
}

// TestSessionListReportsHolderPerSlice proves sessionList reports the
// correct holder per slice via Registry.HolderOf(), and free=ok=false for
// untouched slices.
func TestSessionListReportsHolderPerSlice(t *testing.T) {
	dir := t.TempDir()
	sliceID, _, err := checkoutSlice(dir, "i-abc123", "alice", "mps", time.Hour, "g7e.2xlarge", 2)
	if err != nil {
		t.Fatal(err)
	}

	reports, err := sessionList(dir, "i-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("sessionList returned %d slices, want 2", len(reports))
	}
	var sawHeld, sawFree bool
	for _, r := range reports {
		if r.SliceID == sliceID {
			sawHeld = true
			if !r.Held || r.UserID != "alice" {
				t.Errorf("slice %s = (held=%v, user=%q), want (true, alice)", r.SliceID, r.Held, r.UserID)
			}
		} else {
			sawFree = true
			if r.Held {
				t.Errorf("slice %s reports held, want free", r.SliceID)
			}
		}
	}
	if !sawHeld || !sawFree {
		t.Fatalf("expected one held and one free slice, got %+v", reports)
	}
}

// TestCheckoutMIGLayoutSizedFromProfile proves --backend mig derives its
// slice count from internal/mig's live-verified profile catalog (g7e's
// "most tenants" default profile is 4x1g.24gb, i.e. 4 slices) rather than an
// arbitrary constant.
func TestCheckoutMIGLayoutSizedFromProfile(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := checkoutSlice(dir, "i-abc123", "alice", "mig", time.Hour, "g7e.2xlarge", 0); err != nil {
		t.Fatal(err)
	}
	_, total, err := sessionStatus(dir, "i-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("MIG layout for g7e.2xlarge has %d slices, want 4 (matches internal/mig.PickLayout's g7e default)", total)
	}
}

// TestCheckoutBackendMismatchOnSameInstanceErrors proves the slice layout
// (and hence backend) is fixed at first checkout — a later checkout
// requesting a DIFFERENT backend on the same already-laid-out instance must
// be refused, not silently reinterpreted.
func TestCheckoutBackendMismatchOnSameInstanceErrors(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := checkoutSlice(dir, "i-abc123", "alice", "mig", time.Hour, "g7e.2xlarge", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkoutSlice(dir, "i-abc123", "bob", "mps", time.Hour, "g7e.2xlarge", 4); err == nil {
		t.Fatal("checkout with a different backend on an already-laid-out instance succeeded, want an error")
	}
}

// TestCheckoutNoFreeSliceErrors proves exhausting every slice on an
// instance surfaces tenancy.ErrNoFreeSlice-driven refusal through the CLI
// wrapper, rather than silently overcommitting.
func TestCheckoutNoFreeSliceErrors(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := checkoutSlice(dir, "i-abc123", "alice", "mps", time.Hour, "g7e.2xlarge", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkoutSlice(dir, "i-abc123", "bob", "mps", time.Hour, "g7e.2xlarge", 1); err == nil {
		t.Fatal("checkout on a fully-occupied 1-slot instance succeeded, want ErrNoFreeSlice")
	}
}

// TestSessionCheckoutSpotWarnsWhenOtherTenantsHeld proves calque#119: when
// --spot is set and a SECOND user checks out a slice on an instance that
// already has one held, sessionCheckoutCmd prints the compounding
// blast-radius warning naming the other concurrent tenant(s).
func TestSessionCheckoutSpotWarnsWhenOtherTenantsHeld(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CALQUE_SESSION_STATE_DIR", dir)

	// alice checks out first (no other tenants yet, so no warning expected
	// regardless of --spot).
	out := captureStdout(t, func() {
		if err := sessionCheckoutCmd([]string{
			"--instance-id", "i-abc123", "--user", "alice", "--backend", "mps", "--slots", "4", "--spot",
			"--" + mps.OptInFlagName,
		}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "WARNING") {
		t.Errorf("alice's checkout (first tenant) warned about other tenants, want no warning:\n%s", out)
	}

	// bob checks out second, with --spot set: alice's slice is now an
	// "other" concurrent tenant, so the warning must fire.
	out = captureStdout(t, func() {
		if err := sessionCheckoutCmd([]string{
			"--instance-id", "i-abc123", "--user", "bob", "--backend", "mps", "--slots", "4", "--spot",
			"--" + mps.OptInFlagName,
		}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "spot") {
		t.Errorf("bob's checkout (second tenant, --spot) did not warn, want a spot blast-radius warning:\n%s", out)
	}
	if !strings.Contains(out, "1 other concurrent tenant session") {
		t.Errorf("warning did not name the other-tenant count (want 1 other), got:\n%s", out)
	}
	if !strings.Contains(out, "LEAKS:") {
		t.Errorf("warning did not include an emitted leak.Report summary, got:\n%s", out)
	}
}

// TestSessionCheckoutSpotNoWarningForSoleTenant proves the common case —
// only one tenant holds a slice on the instance — never fires the warning,
// even with --spot set (there is no "other" tenant to name yet).
func TestSessionCheckoutSpotNoWarningForSoleTenant(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CALQUE_SESSION_STATE_DIR", dir)

	out := captureStdout(t, func() {
		if err := sessionCheckoutCmd([]string{
			"--instance-id", "i-solo", "--user", "alice", "--backend", "mps", "--slots", "4", "--spot",
			"--" + mps.OptInFlagName,
		}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "WARNING") {
		t.Errorf("sole-tenant checkout with --spot warned about other tenants, want none:\n%s", out)
	}
}

// TestSessionCheckoutNoSpotFlagNeverWarns proves that without --spot, the
// warning never fires even when a second tenant is present — checkout has
// no other way to know the instance's market type, so it must not guess.
func TestSessionCheckoutNoSpotFlagNeverWarns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CALQUE_SESSION_STATE_DIR", dir)

	captureStdout(t, func() {
		if err := sessionCheckoutCmd([]string{
			"--instance-id", "i-nospot", "--user", "alice", "--backend", "mps", "--slots", "4",
			"--" + mps.OptInFlagName,
		}); err != nil {
			t.Fatal(err)
		}
	})
	out := captureStdout(t, func() {
		if err := sessionCheckoutCmd([]string{
			"--instance-id", "i-nospot", "--user", "bob", "--backend", "mps", "--slots", "4",
			"--" + mps.OptInFlagName,
		}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "WARNING") {
		t.Errorf("checkout without --spot warned about other tenants, want none:\n%s", out)
	}
}

// TestCheckoutExpiredHoldIsReclaimed proves a checkout past its TTL is
// treated as free by a LATER checkout, exercising the pruneExpired +
// rebuildRegistry path end to end (the CLI-layer counterpart to
// tenancy.Registry's own lazy-sweep behavior).
func TestCheckoutExpiredHoldIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	sliceID, _, err := checkoutSlice(dir, "i-abc123", "alice", "mps", 10*time.Millisecond, "g7e.2xlarge", 1)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	sliceID2, _, err := checkoutSlice(dir, "i-abc123", "bob", "mps", time.Hour, "g7e.2xlarge", 1)
	if err != nil {
		t.Fatalf("bob should reclaim alice's expired slice: %v", err)
	}
	if sliceID2 != sliceID {
		t.Errorf("bob got %q, want the expired slice %q back", sliceID2, sliceID)
	}
}
