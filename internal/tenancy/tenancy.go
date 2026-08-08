// Package tenancy implements the check-out/interactive-use/check-in lifecycle
// designed in docs/tenancy-vs-session.md (calque#106): binding ONE user to
// ONE bindable unit of GPU access — a MIG GPU-instance (calque#107) or an
// MPS client-slot (calque#108) — on an instance that is ALREADY acquired and
// running.
//
// This package is deliberately generic over WHAT a Slice represents (a MIG
// instance UUID, an MPS pipe directory, anything else with a stable string
// identity) — calque#108's own issue text says this primitive should be
// "built once, in #107... reused rather than designed twice" for MIG and
// MPS, so the Slice concept carries no MIG- or MPS-specific fields. It never
// calls AWS: per the boundary statement in docs/tenancy-vs-session.md,
// tenancy operates strictly WITHIN an instance the fleet layer
// (internal/plan/acquire.go, internal/pool) already handed it.
package tenancy

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Slice is one bindable unit of GPU access on an already-acquired instance.
// ID is the concrete resource: a MIG GPU-instance UUID
// ("MIG-xxxxxxxx-xxxx-..."), an MPS client-slot identifier, or any other
// stable string handle the caller's execution layer (warmd) can bind a
// process to (e.g. via CUDA_VISIBLE_DEVICES=ID).
type Slice struct {
	ID string
}

// checkout is one live binding: which user holds which slice, and when it
// expires if never explicitly checked in.
type checkout struct {
	slice     Slice
	userID    string
	expiresAt time.Time
}

// ErrNoFreeSlice means every slice in the registry is currently checked out.
// The caller (a future scheduler, or the M12 fleet layer per
// docs/tenancy-vs-session.md's boundary statement) decides what to do next —
// wait, route to a different instance, or acquire another one. Registry
// itself never makes that call.
var ErrNoFreeSlice = errors.New("tenancy: no free slice")

// ErrSliceNotCheckedOut means CheckIn was called for a slice that isn't
// currently held — a caller bug (releasing twice, or a stale handle), not a
// contention condition.
var ErrSliceNotCheckedOut = errors.New("tenancy: slice not checked out")

// Registry tracks check-out/check-in state for one instance's fixed set of
// slices (calque#107: the slice LAYOUT itself — how many slices, what size
// each is — is chosen once at boot/prep time and does not change while the
// Registry is live; that's the "fixed-layout" half of this issue's scope).
// Safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	slices   []Slice
	held     map[string]checkout // keyed by Slice.ID
	now      func() time.Time    // overridable for deterministic TTL-expiry tests
	sweepTTL time.Duration       // 0 => no TTL enforcement (caller must CheckIn explicitly)
}

// NewRegistry builds a Registry over a fixed set of slices. ttl is the
// default check-out duration if a caller's CheckOut doesn't specify one
// (0 => no expiry; CheckIn is required to free a slice).
func NewRegistry(slices []Slice, ttl time.Duration) *Registry {
	return &Registry{slices: slices, held: map[string]checkout{}, now: time.Now, sweepTTL: ttl}
}

// CheckOut binds userID to one free slice for ttl (0 => use the Registry's
// default; still 0 after that => held until explicitly checked in). Expired
// checkouts are swept lazily on every call, so a caller never needs a
// separate background reaper for correctness (only for reclaiming slices
// promptly when nobody else is asking) — this mirrors the lazy-expiry
// pattern used throughout the codebase's own idle-timeout checks
// (e.g. taskpool.Worker.Run's idleDeadline).
func (r *Registry) CheckOut(userID string, ttl time.Duration) (Slice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepExpiredLocked()

	for _, s := range r.slices {
		if _, taken := r.held[s.ID]; taken {
			continue
		}
		effTTL := ttl
		if effTTL == 0 {
			effTTL = r.sweepTTL
		}
		var expiresAt time.Time
		if effTTL > 0 {
			expiresAt = r.now().Add(effTTL)
		}
		r.held[s.ID] = checkout{slice: s, userID: userID, expiresAt: expiresAt}
		return s, nil
	}
	return Slice{}, ErrNoFreeSlice
}

// CheckIn releases a slice back to the free pool, regardless of who holds it
// (the caller — a session-teardown path — is trusted to name the right
// slice; Registry does not itself authenticate the release).
func (r *Registry) CheckIn(sliceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.held[sliceID]; !ok {
		return fmt.Errorf("%w: %s", ErrSliceNotCheckedOut, sliceID)
	}
	delete(r.held, sliceID)
	return nil
}

// Occupancy reports how many of the registry's slices are currently held,
// after sweeping expired checkouts — for a caller (or a future metrics
// path) to observe fleet-level utilization without reaching into internals.
func (r *Registry) Occupancy() (held, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepExpiredLocked()
	return len(r.held), len(r.slices)
}

// HolderOf reports which userID currently holds sliceID, if any — for a
// caller that needs to attribute a running workload back to its user (e.g.
// cost apportionment, per docs/pool-queue-contract.md-style honesty
// discipline: attribute what's actually known, don't guess).
func (r *Registry) HolderOf(sliceID string) (userID string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepExpiredLocked()
	c, ok := r.held[sliceID]
	if !ok {
		return "", false
	}
	return c.userID, true
}

// sweepExpiredLocked removes every checkout whose TTL has passed. Caller
// must hold r.mu.
func (r *Registry) sweepExpiredLocked() {
	now := r.now()
	for id, c := range r.held {
		if !c.expiresAt.IsZero() && now.After(c.expiresAt) {
			delete(r.held, id)
		}
	}
}
