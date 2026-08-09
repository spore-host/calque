// calque session: M16's institutional check-out/check-in vertical slice
// (calque#118/#119), implementing the new-primitive half of
// docs/tenancy-vs-session.md's design — NOT to be confused with the
// N-item ramp verb (renamed `calque ramp` by #117, cmd/calque/ramp.go).
//
// This binds ONE user to ONE slice (a MIG GPU-instance, or an MPS
// client-slot) on an instance that is ALREADY acquired and running. It
// never calls plan.Acquirer/spawn to acquire or terminate an EC2 instance
// (docs/tenancy-vs-session.md's boundary statement, restated in
// docs/m12-m13-boundary.md) — that is strictly the fleet layer's job.
//
// # Identity across separate CLI invocations
//
// internal/tenancy.Registry is an in-memory struct: a fresh `calque
// session checkout` and a later `calque session checkin` are two SEPARATE
// process invocations with no shared memory, so this file persists the
// minimal state needed to rebuild an equivalent Registry on each
// invocation — see instanceState/rebuildRegistry below. internal/tenancy
// itself is deliberately left unchanged (its CheckIn doc already explains
// why the Registry does not authenticate releases: it stays
// execution-layer-agnostic). Identity enforcement therefore lives HERE,
// at the CLI layer: checkout mints a random session token and hands it to
// the caller; checkin refuses (without ever calling Registry.CheckIn) if
// the caller's token doesn't match the one checkout minted.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/mig"
	"github.com/spore-host/calque/internal/mps"
	"github.com/spore-host/calque/internal/tenancy"
)

func sessionCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: calque session <checkout|checkin|status|list> ...")
	}
	switch args[0] {
	case "checkout":
		return sessionCheckoutCmd(args[1:])
	case "checkin":
		return sessionCheckinCmd(args[1:])
	case "status":
		return sessionStatusCmd(args[1:])
	case "list":
		return sessionListCmd(args[1:])
	default:
		return fmt.Errorf("unknown session subcommand %q (want: checkout, checkin, status, list)", args[0])
	}
}

// sessionStateDir is where per-instance session layout+holds are persisted
// (see instanceState). Overridable via CALQUE_SESSION_STATE_DIR, mirroring
// main.go's own pyastDir()/CALQUE_PYAST_DIR convention.
func sessionStateDir() string {
	if d := os.Getenv("CALQUE_SESSION_STATE_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "calque-sessions")
}

func sessionCheckoutCmd(args []string) error {
	fs := flag.NewFlagSet("session checkout", flag.ExitOnError)
	instanceID := fs.String("instance-id", "", "EC2 instance ID of the ALREADY-RUNNING instance to check out a slice on (required)")
	user := fs.String("user", "", "user identity to bind the slice to (required)")
	backend := fs.String("backend", "", "slice backend: mig (hardware-isolated) or mps (cooperative, no isolation) (required)")
	ttlStr := fs.String("ttl", "2h", "bounded interactive TTL; the slice is reclaimed automatically if never explicitly checked in")
	instanceType := fs.String("instance-type", "g7e.2xlarge", "instance type; used ONLY to size the MIG layout via internal/mig's live-verified profile catalog when --backend mig")
	slots := fs.Int("slots", 4, "number of MPS client-slots to model on this instance when --backend mps (MPS has no fixed hardware layout; this is an operator choice, see internal/mps's package doc)")
	confirm := fs.Bool(mps.OptInFlagName, false, "required when --backend mps: MPS gives no per-client isolation (see internal/mps package doc); NOT required for --backend mig")
	spot := fs.Bool("spot", false, "the underlying instance was acquired on the Spot market — surfaces the compounding blast-radius risk to concurrent tenants at checkout time (calque#119)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *instanceID == "" || *user == "" || *backend == "" {
		return fmt.Errorf("usage: calque session checkout --instance-id ID --user U --backend mig|mps [--ttl 2h]")
	}
	if *backend != "mig" && *backend != "mps" {
		return fmt.Errorf("--backend must be %q or %q, got %q", "mig", "mps", *backend)
	}
	if *backend == "mps" {
		// MIG is hardware-isolated and needs no opt-in; MPS gives no
		// per-client isolation and is gated behind its own consent flag
		// (mirroring internal/mps's own RequireOptIn convention).
		if err := mps.RequireOptIn(*confirm); err != nil {
			return err
		}
	}
	ttl, err := time.ParseDuration(*ttlStr)
	if err != nil {
		return fmt.Errorf("--ttl %q: %w", *ttlStr, err)
	}

	sliceID, token, err := checkoutSlice(sessionStateDir(), *instanceID, *user, *backend, ttl, *instanceType, *slots)
	if err != nil {
		return err
	}
	fmt.Printf("checked out slice %s for user %q on instance %s (backend=%s, ttl=%s)\n", sliceID, *user, *instanceID, *backend, ttl)
	fmt.Printf("session-token: %s\n", token)
	fmt.Printf("release with: calque session checkin --slice %s --session-token %s\n", sliceID, token)

	// calque#119: --spot is operator-supplied (checkout deliberately never
	// calls EC2 describe, per this file's own scope boundary, so it has no
	// other way to know the instance's market type). If the instance IS
	// spot AND at least one other tenant already holds a slice here, a
	// reclaim ends every one of those sessions at once, not just this new
	// one — the existing per-RUN spot leak (ramp.go/realrun.go/smoke.go/
	// fleetrun.go) assumes single-tenant framing and can't say this, since
	// tenancy checkout necessarily happens later, as a separate invocation.
	if *spot {
		held, _, serr := sessionStatus(sessionStateDir(), *instanceID)
		if serr == nil && held > 1 {
			others := held - 1
			fmt.Printf("[spot] WARNING: this instance was acquired on spot — a reclaim would end %d other concurrent tenant session(s) on this instance, not just this one\n", others)
			rep := &leak.Report{}
			rep.Addf(leak.PrimAcquire, leak.KindSemanticGap, "session", 0,
				"spot acquisition: this instance was acquired on spot — a reclaim would end %d other concurrent tenant session(s) on this instance, not just this one", others)
			rep.Summary(os.Stdout)
		}
	}
	return nil
}

func sessionCheckinCmd(args []string) error {
	fs := flag.NewFlagSet("session checkin", flag.ExitOnError)
	sliceID := fs.String("slice", "", "slice ID returned by checkout (required)")
	token := fs.String("session-token", "", "session token returned by checkout (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sliceID == "" || *token == "" {
		return fmt.Errorf("usage: calque session checkin --slice ID --session-token T")
	}
	if err := checkinSlice(sessionStateDir(), *sliceID, *token); err != nil {
		return err
	}
	fmt.Printf("checked in slice %s\n", *sliceID)
	return nil
}

func sessionStatusCmd(args []string) error {
	fs := flag.NewFlagSet("session status", flag.ExitOnError)
	instanceID := fs.String("instance-id", "", "EC2 instance ID to report occupancy for (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *instanceID == "" {
		return fmt.Errorf("usage: calque session status --instance-id ID")
	}
	held, total, err := sessionStatus(sessionStateDir(), *instanceID)
	if err != nil {
		return err
	}
	fmt.Printf("instance %s: %d/%d slices held\n", *instanceID, held, total)
	return nil
}

func sessionListCmd(args []string) error {
	fs := flag.NewFlagSet("session list", flag.ExitOnError)
	instanceID := fs.String("instance-id", "", "EC2 instance ID to list slices for (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *instanceID == "" {
		return fmt.Errorf("usage: calque session list --instance-id ID")
	}
	reports, err := sessionList(sessionStateDir(), *instanceID)
	if err != nil {
		return err
	}
	for _, r := range reports {
		if r.Held {
			fmt.Printf("%s  held  user=%s\n", r.SliceID, r.UserID)
		} else {
			fmt.Printf("%s  free\n", r.SliceID)
		}
	}
	return nil
}

// holdRecord is one persisted checkout: who holds the slice, the session
// token checkin must present to release it (see this file's package doc),
// and when it auto-expires (zero => held until explicitly checked in).
type holdRecord struct {
	UserID    string    `json:"user_id"`
	Token     string    `json:"session_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// instanceState is the on-disk, per-instance record of a fixed slice
// layout (chosen once, at first checkout — calque#107/#108's own
// "fixed-layout" discipline applied at the CLI layer) plus whichever
// slices are currently held. It is the persistence substrate a fresh
// tenancy.Registry is rebuilt from on every CLI invocation (see
// rebuildRegistry).
type instanceState struct {
	InstanceID string                `json:"instance_id"`
	Backend    string                `json:"backend"` // "mig" or "mps"
	Slices     []string              `json:"slices"`  // fixed layout, in stable order
	Holds      map[string]holdRecord `json:"holds"`
}

func (st *instanceState) pruneExpired(now time.Time) {
	for id, h := range st.Holds {
		if !h.ExpiresAt.IsZero() && now.After(h.ExpiresAt) {
			delete(st.Holds, id)
		}
	}
}

func statePath(stateDir, instanceID string) string {
	return filepath.Join(stateDir, instanceID+".json")
}

// loadState returns (nil, nil) if no state file exists yet for instanceID —
// a caller-distinguishable "nothing checked out here yet," not an error.
func loadState(stateDir, instanceID string) (*instanceState, error) {
	b, err := os.ReadFile(statePath(stateDir, instanceID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session state for instance %s: %w", instanceID, err)
	}
	var st instanceState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse session state for instance %s: %w", instanceID, err)
	}
	if st.Holds == nil {
		st.Holds = map[string]holdRecord{}
	}
	return &st, nil
}

func saveState(stateDir string, st *instanceState) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create session state dir %s: %w", stateDir, err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session state: %w", err)
	}
	if err := os.WriteFile(statePath(stateDir, st.InstanceID), b, 0o600); err != nil {
		return fmt.Errorf("write session state for instance %s: %w", st.InstanceID, err)
	}
	return nil
}

// findStateBySlice scans every instance's state file in stateDir for one
// whose fixed layout contains sliceID. Checkin (per this milestone's CLI
// shape, calque#118/#119) takes only --slice/--session-token, not
// --instance-id, so this is the glue that recovers which instance's
// Registry a slice ID belongs to — safe because deriveSliceLayout mints
// slice IDs prefixed with their owning instance ID, so collisions across
// instances cannot occur.
func findStateBySlice(stateDir, sliceID string) (instanceID string, st *instanceState, err error) {
	entries, rerr := os.ReadDir(stateDir)
	if errors.Is(rerr, os.ErrNotExist) {
		return "", nil, nil
	}
	if rerr != nil {
		return "", nil, fmt.Errorf("read session state dir %s: %w", stateDir, rerr)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		candidate, lerr := loadState(stateDir, id)
		if lerr != nil || candidate == nil {
			continue
		}
		for _, s := range candidate.Slices {
			if s == sliceID {
				return id, candidate, nil
			}
		}
	}
	return "", nil, nil
}

// deriveSliceLayout decides "which slices exist on instance X" — the one
// piece of glue this vertical slice needs to connect internal/mig's and
// internal/mps's slice topology to a tenancy.Registry. It does NOT
// provision anything (no nvidia-smi, no SSM): per docs/tenancy-vs-session.md's
// boundary statement, checkout binds users to slices that already exist;
// actually carving them (mig.ProvisionScript / mps.StartDaemonScript) is a
// separate, already-shipped concern this command never touches.
//
// Slice IDs are prefixed with instanceID so they stay globally unique
// across every instance's state file (see findStateBySlice).
func deriveSliceLayout(instanceID, backend, instanceType string, mpsSlots int) ([]string, error) {
	switch backend {
	case "mig":
		family := instanceType
		if i := strings.IndexByte(instanceType, '.'); i >= 0 {
			family = instanceType[:i]
		}
		_, count, err := mig.PickLayout(family, 0)
		if err != nil {
			return nil, fmt.Errorf("derive MIG slice layout for instance-type %q: %w", instanceType, err)
		}
		ids := make([]string, count)
		for i := range ids {
			ids[i] = fmt.Sprintf("%s-mig-slot-%d", instanceID, i)
		}
		return ids, nil
	case "mps":
		if mpsSlots < 1 {
			return nil, fmt.Errorf("--slots must be >= 1 for backend=mps, got %d", mpsSlots)
		}
		ids := make([]string, mpsSlots)
		for i := range ids {
			ids[i] = fmt.Sprintf("%s-mps-slot-%d", instanceID, i)
		}
		return ids, nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want mig or mps)", backend)
	}
}

// rebuildRegistry constructs a fresh tenancy.Registry matching st's
// persisted holds, so this CLI-process invocation can drive it through
// tenancy's own CheckOut/CheckIn/Occupancy/HolderOf logic (TTL sweep
// included) rather than re-deriving that logic here.
//
// Registry's public API has no "restore" primitive — CheckOut always hands
// back the FIRST still-free slice in the Registry's own slice-list order.
// So the Registry is built with every currently-held slice listed BEFORE
// every free one (in st.Slices' stable order within each group), then each
// held slice is replayed via CheckOut in that same order: at the moment
// slice i's hold replays, every held slice before it has already been
// re-checked-out, so slice i is exactly the first slice still free —
// itself. This reconstructs the persisted (slice -> holder) mapping
// exactly, using only Registry's existing, unmodified API.
func rebuildRegistry(st *instanceState) (*tenancy.Registry, error) {
	var held, free []string
	for _, id := range st.Slices {
		if _, ok := st.Holds[id]; ok {
			held = append(held, id)
		} else {
			free = append(free, id)
		}
	}
	ordered := make([]tenancy.Slice, 0, len(st.Slices))
	for _, id := range append(append([]string{}, held...), free...) {
		ordered = append(ordered, tenancy.Slice{ID: id})
	}
	reg := tenancy.NewRegistry(ordered, 0)

	for _, id := range held {
		h := st.Holds[id]
		var ttl time.Duration
		if !h.ExpiresAt.IsZero() {
			ttl = time.Until(h.ExpiresAt)
			if ttl <= 0 {
				// Callers must pruneExpired before rebuilding; a non-positive
				// remainder here means that step was skipped — fail loudly
				// rather than silently re-issuing an already-expired hold.
				return nil, fmt.Errorf("rebuildRegistry: hold on slice %q already expired but was not pruned", id)
			}
		}
		if _, err := reg.CheckOut(h.UserID, ttl); err != nil {
			return nil, fmt.Errorf("rebuild registry: replay checkout for slice %q: %w", id, err)
		}
	}
	return reg, nil
}

// newSessionToken mints the random token checkout hands the caller and
// checkin must present back — the CLI-layer identity check that
// internal/tenancy.CheckIn deliberately does not perform itself (see this
// file's package doc).
func newSessionToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// errWrongSessionToken is returned (wrapped) when checkin's token doesn't
// match the one checkout minted. Registry.CheckIn is never called in this
// path — refusal happens strictly before that call (see checkinSlice).
var errWrongSessionToken = errors.New("session: wrong session token")

// checkoutSlice binds user to one free slice on instanceID, deriving and
// persisting instanceID's fixed slice layout on first use. Never calls
// plan.Acquirer — instanceID must already be a running instance (this
// command's whole scope boundary, docs/tenancy-vs-session.md).
func checkoutSlice(stateDir, instanceID, user, backend string, ttl time.Duration, instanceType string, mpsSlots int) (sliceID, token string, err error) {
	st, err := loadState(stateDir, instanceID)
	if err != nil {
		return "", "", err
	}
	if st == nil {
		slices, derr := deriveSliceLayout(instanceID, backend, instanceType, mpsSlots)
		if derr != nil {
			return "", "", derr
		}
		st = &instanceState{InstanceID: instanceID, Backend: backend, Slices: slices, Holds: map[string]holdRecord{}}
	} else if st.Backend != backend {
		// The layout is fixed at first checkout (mirroring internal/mig's/
		// internal/mps's own "chosen once" discipline) — switching backends
		// on a live instance would silently invalidate every slice ID a
		// prior checkout already handed out.
		return "", "", fmt.Errorf("instance %s already has a %q-backend session layout; checkout requested %q (layout is fixed at first checkout, matching internal/mig/internal/mps's own fixed-layout-at-boot discipline)", instanceID, st.Backend, backend)
	}

	st.pruneExpired(time.Now())

	reg, err := rebuildRegistry(st)
	if err != nil {
		return "", "", err
	}
	slice, err := reg.CheckOut(user, ttl)
	if err != nil {
		return "", "", fmt.Errorf("checkout on instance %s: %w", instanceID, err)
	}
	tok, err := newSessionToken()
	if err != nil {
		return "", "", err
	}
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	st.Holds[slice.ID] = holdRecord{UserID: user, Token: tok, ExpiresAt: expiresAt}
	if err := saveState(stateDir, st); err != nil {
		return "", "", err
	}
	return slice.ID, tok, nil
}

// checkinSlice validates token against the persisted hold BEFORE ever
// touching Registry: a mismatched token returns errWrongSessionToken and
// Registry.CheckIn is not called (state is left exactly as it was) — see
// this file's package doc for why identity enforcement lives here rather
// than in internal/tenancy.
func checkinSlice(stateDir, sliceID, token string) error {
	instanceID, st, err := findStateBySlice(stateDir, sliceID)
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("slice %s is not known (no recorded session layout contains it)", sliceID)
	}
	st.pruneExpired(time.Now())

	hold, ok := st.Holds[sliceID]
	if !ok {
		return fmt.Errorf("slice %s is not currently checked out on instance %s", sliceID, instanceID)
	}
	if hold.Token != token {
		return fmt.Errorf("%w: refusing to check in slice %s", errWrongSessionToken, sliceID)
	}

	reg, err := rebuildRegistry(st)
	if err != nil {
		return err
	}
	if err := reg.CheckIn(sliceID); err != nil {
		return fmt.Errorf("checkin slice %s on instance %s: %w", sliceID, instanceID, err)
	}
	delete(st.Holds, sliceID)
	return saveState(stateDir, st)
}

// sessionStatus wraps Registry.Occupancy() for instanceID's persisted layout.
func sessionStatus(stateDir, instanceID string) (held, total int, err error) {
	st, err := loadState(stateDir, instanceID)
	if err != nil {
		return 0, 0, err
	}
	if st == nil {
		return 0, 0, fmt.Errorf("no session state for instance %s (run `calque session checkout` first)", instanceID)
	}
	st.pruneExpired(time.Now())
	reg, err := rebuildRegistry(st)
	if err != nil {
		return 0, 0, err
	}
	held, total = reg.Occupancy()
	return held, total, nil
}

// sliceReport is one slice's reported state for `calque session list`.
type sliceReport struct {
	SliceID string
	Held    bool
	UserID  string // valid only if Held
}

// sessionList reports every slice in instanceID's persisted layout via
// Registry.HolderOf(), in stable layout order.
func sessionList(stateDir, instanceID string) ([]sliceReport, error) {
	st, err := loadState(stateDir, instanceID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("no session state for instance %s (run `calque session checkout` first)", instanceID)
	}
	st.pruneExpired(time.Now())
	reg, err := rebuildRegistry(st)
	if err != nil {
		return nil, err
	}
	out := make([]sliceReport, 0, len(st.Slices))
	for _, id := range st.Slices {
		who, ok := reg.HolderOf(id)
		out = append(out, sliceReport{SliceID: id, Held: ok, UserID: who})
	}
	return out, nil
}
