package tenancy

import (
	"errors"
	"testing"
	"time"
)

func slices(ids ...string) []Slice {
	out := make([]Slice, len(ids))
	for i, id := range ids {
		out[i] = Slice{ID: id}
	}
	return out
}

// TestCheckOutBindsExactlyOneUserPerSlice proves the core exclusivity
// invariant: once a slice is checked out, a second CheckOut call must NOT
// hand out the same slice — it must move to the next free one.
func TestCheckOutBindsExactlyOneUserPerSlice(t *testing.T) {
	r := NewRegistry(slices("MIG-a", "MIG-b"), 0)
	s1, err := r.CheckOut("alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := r.CheckOut("bob", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID == s2.ID {
		t.Fatalf("both users got the same slice %q; exclusivity violated", s1.ID)
	}
	if _, err := r.CheckOut("carol", 0); !errors.Is(err, ErrNoFreeSlice) {
		t.Errorf("third CheckOut = %v, want ErrNoFreeSlice (both slices already held)", err)
	}
}

// TestCheckInFreesTheSliceForReuse proves releasing a slice makes it
// available to the NEXT CheckOut call.
func TestCheckInFreesTheSliceForReuse(t *testing.T) {
	r := NewRegistry(slices("MIG-a"), 0)
	s, err := r.CheckOut("alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CheckOut("bob", 0); !errors.Is(err, ErrNoFreeSlice) {
		t.Fatalf("expected no free slice while alice holds it, got %v", err)
	}
	if err := r.CheckIn(s.ID); err != nil {
		t.Fatal(err)
	}
	s2, err := r.CheckOut("bob", 0)
	if err != nil {
		t.Fatalf("bob should get the freed slice: %v", err)
	}
	if s2.ID != s.ID {
		t.Errorf("bob got %q, want the freed slice %q", s2.ID, s.ID)
	}
}

// TestCheckInUnknownSliceErrors proves releasing a slice that isn't
// currently held is a reported error, not a silent no-op — a caller bug
// (double-release, stale handle) should be visible.
func TestCheckInUnknownSliceErrors(t *testing.T) {
	r := NewRegistry(slices("MIG-a"), 0)
	if err := r.CheckIn("MIG-a"); !errors.Is(err, ErrSliceNotCheckedOut) {
		t.Errorf("CheckIn on a never-checked-out slice = %v, want ErrSliceNotCheckedOut", err)
	}
}

// TestTTLExpirySweepsAutomatically proves a checkout past its TTL is treated
// as free on the NEXT CheckOut call, without a separate explicit CheckIn —
// the "bounded interactive TTL" half of docs/tenancy-vs-session.md's
// lifecycle (a session that never explicitly checks in must still expire).
func TestTTLExpirySweepsAutomatically(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := NewRegistry(slices("MIG-a"), 0)
	r.now = func() time.Time { return now }

	if _, err := r.CheckOut("alice", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CheckOut("bob", 0); !errors.Is(err, ErrNoFreeSlice) {
		t.Fatalf("expected no free slice before TTL expiry, got %v", err)
	}

	now = now.Add(2 * time.Minute) // past alice's 1-minute TTL
	s, err := r.CheckOut("bob", 0)
	if err != nil {
		t.Fatalf("bob should get alice's expired slice: %v", err)
	}
	if s.ID != "MIG-a" {
		t.Errorf("bob got %q, want the expired slice MIG-a", s.ID)
	}
}

// TestDefaultTTLAppliesWhenCheckOutOmitsOne proves the Registry-level
// default TTL (NewRegistry's second arg) is used when CheckOut passes 0.
func TestDefaultTTLAppliesWhenCheckOutOmitsOne(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := NewRegistry(slices("MIG-a"), 30*time.Second)
	r.now = func() time.Time { return now }

	if _, err := r.CheckOut("alice", 0); err != nil { // 0 => use registry default (30s)
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if _, err := r.CheckOut("bob", 0); err != nil {
		t.Fatalf("bob should get the slice after the DEFAULT ttl expired: %v", err)
	}
}

// TestOccupancyReportsHeldVsTotal proves the fleet-visibility accessor
// reflects both live checkouts and expiry sweeps.
func TestOccupancyReportsHeldVsTotal(t *testing.T) {
	r := NewRegistry(slices("MIG-a", "MIG-b", "MIG-c"), 0)
	if held, total := r.Occupancy(); held != 0 || total != 3 {
		t.Errorf("Occupancy = (%d, %d), want (0, 3)", held, total)
	}
	if _, err := r.CheckOut("alice", 0); err != nil {
		t.Fatal(err)
	}
	if held, total := r.Occupancy(); held != 1 || total != 3 {
		t.Errorf("Occupancy = (%d, %d), want (1, 3)", held, total)
	}
}

// TestExpiryHookFiresSynchronouslyOnSweep proves calque#128's fix: when
// sweepExpiredLocked reclaims a TTL-expired slice, the configured
// ExpiryHook has ALREADY been called by the time the CheckOut call that
// triggered the sweep returns (i.e. it runs synchronously under the
// Registry's lock, not fired-and-forgotten in a goroutine) — and it fires
// exactly once per expired slice, not once per sweep pass.
func TestExpiryHookFiresSynchronouslyOnSweep(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	type call struct {
		slice  Slice
		userID string
	}
	var calls []call
	r := NewRegistryWithExpiryHook(slices("MIG-a"), 0, func(slice Slice, userID string) {
		calls = append(calls, call{slice, userID})
	})
	r.now = func() time.Time { return now }

	if _, err := r.CheckOut("alice", time.Minute); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute) // past alice's 1-minute TTL

	// The hook must have fired by the time THIS CheckOut call returns.
	s, err := r.CheckOut("bob", 0)
	if err != nil {
		t.Fatalf("bob should get alice's expired slice: %v", err)
	}
	if s.ID != "MIG-a" {
		t.Errorf("bob got %q, want the expired slice MIG-a", s.ID)
	}
	if len(calls) != 1 {
		t.Fatalf("ExpiryHook fired %d times, want exactly 1", len(calls))
	}
	if calls[0].slice.ID != "MIG-a" || calls[0].userID != "alice" {
		t.Errorf("ExpiryHook called with (%+v, %q), want (MIG-a slice, alice)", calls[0].slice, calls[0].userID)
	}

	// A subsequent sweep (e.g. via Occupancy) with nothing newly expired
	// must not re-fire the hook for the already-reclaimed slice.
	r.Occupancy()
	if len(calls) != 1 {
		t.Errorf("ExpiryHook fired again on a later sweep with nothing new expired: %d calls, want 1", len(calls))
	}
}

// TestNilExpiryHookIsSafe proves NewRegistry (no hook) behaves exactly as
// before calque#128 — a nil ExpiryHook must never be called and must not
// change TTL-expiry sweep behavior.
func TestNilExpiryHookIsSafe(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := NewRegistry(slices("MIG-a"), 0) // no hook, same as every pre-existing caller
	r.now = func() time.Time { return now }

	if _, err := r.CheckOut("alice", time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	s, err := r.CheckOut("bob", 0)
	if err != nil {
		t.Fatalf("bob should get alice's expired slice: %v", err)
	}
	if s.ID != "MIG-a" {
		t.Errorf("bob got %q, want the expired slice MIG-a", s.ID)
	}
}

// TestHolderOfAttributesCorrectly proves a caller can look up which user
// holds a given slice, and gets ok=false for a free one.
func TestHolderOfAttributesCorrectly(t *testing.T) {
	r := NewRegistry(slices("MIG-a", "MIG-b"), 0)
	s, err := r.CheckOut("alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if who, ok := r.HolderOf(s.ID); !ok || who != "alice" {
		t.Errorf("HolderOf(%q) = (%q, %v), want (alice, true)", s.ID, who, ok)
	}
	if _, ok := r.HolderOf("MIG-b"); ok {
		t.Error("HolderOf on a free slice returned ok=true, want false")
	}
}
