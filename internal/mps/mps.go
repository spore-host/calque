// Package mps implements calque#108's trusted-tenant shared-card mode:
// multiple warmd+runner pairs sharing ONE physical GPU via NVIDIA's
// Multi-Process Service, as the fallback for cards without MIG (g6/g6e per
// calque#104's live findings) or as an explicit opt-in even where MIG exists.
//
// # Trust boundary — read this before wiring MPS into any CLI path
//
// MPS gives NO per-client memory or fault isolation: it is a cooperative
// software scheduler over ONE CUDA context, not a hardware partition. A
// client that segfaults, OOMs, or corrupts GPU state can take down every
// OTHER client sharing that GPU's MPS control daemon — there is no boundary
// between tenants the way MIG's hardware slicing provides one.
//
// Modal's own engineering (calque#96, verified against modal.com's blog)
// declines this risk entirely: every Modal container gets a FULLY dedicated
// physical GPU, never a shared MPS context across different customers'
// workloads. That is the correct call for Modal's arbitrary-internet-tenant
// population, where one customer's buggy kernel taking down another
// customer's inference job is an unacceptable blast radius to accept on
// their behalf.
//
// calque's institutional target is different in kind, not degree: a
// university's GPU users are a KNOWN, BOUNDED population (students/
// researchers within one lab or course), not arbitrary internet tenants. An
// institution can make an informed, internal decision to accept
// cross-workload blast radius in exchange for higher utilization on a fixed
// hardware budget — that is a legitimate trade a trust boundary like Modal's
// cannot offer its own customers. This package treats that as an EXPLICIT,
// loudly-gated choice (see RequireOptIn below), never a silent default and
// never something calque decides on the operator's behalf.
package mps

import (
	"fmt"
	"strings"
	"sync"
)

// RequireOptIn is the gate every MPS-enabling code path must check before
// starting a shared-GPU worker, mirroring cmd/calque/main.go's existing
// --i-understand-this-spends-money discipline for billable actions. MPS's
// risk is categorically different (data/availability blast radius across
// tenants, not just cost), so it gets its OWN flag rather than piggybacking
// on the money-spend one — an operator who has accepted "this will bill me"
// has not thereby also accepted "this can crash a stranger's job."
const OptInFlagName = "i-understand-shared-gpu-has-no-isolation"

// ErrOptInRequired is returned by any MPS-enabling call site that receives
// optedIn=false — never proceed silently.
var ErrOptInRequired = fmt.Errorf("mps: refusing to start a shared-GPU (MPS) worker without --%s", OptInFlagName)

// RequireOptIn is a one-line guard call sites use before doing anything
// MPS-related. Kept as a function (not just documentation) so "did we check
// this" is a single grep-able call, not a scattered if-statement each site
// might get wrong independently.
func RequireOptIn(optedIn bool) error {
	if !optedIn {
		return ErrOptInRequired
	}
	return nil
}

// BlastRadiusPolicy decides what happens to SIBLING clients when one MPS
// client crashes. Per this issue's own recommendation, calque starts
// CONSERVATIVE: MPS provides no per-client fault isolation, so an optimistic
// "only restart the crashed one" policy would be claiming an isolation
// guarantee MPS does not actually make. Restarting every sibling on any
// crash is the honest response to what MPS's trust model actually is,
// even though it costs more (every sibling reloads its model) — a
// crash-then-DATA-CORRUPTION-in-a-sibling failure mode is strictly worse
// than a crash-then-extra-reload-cost one.
type BlastRadiusPolicy string

const (
	// Conservative restarts every client sharing the MPS control daemon when
	// ANY one of them crashes — the DEFAULT and only policy this package
	// currently implements a coordinator for (see Coordinator below).
	Conservative BlastRadiusPolicy = "conservative"
	// Optimistic restarts only the crashed client, leaving siblings running.
	// NOT implemented by Coordinator today — named here so a future,
	// deliberate decision to relax the policy has a documented alternative
	// to move to, rather than inventing the concept from scratch. Choosing
	// this requires evidence MPS's isolation is stronger than documented (or
	// an institution's own risk acceptance beyond this package's default),
	// not just wanting fewer reloads.
	Optimistic BlastRadiusPolicy = "optimistic"
)

// ClientID identifies one warmd+runner pair sharing a GPU's MPS context —
// e.g. "client-0", matching whatever the process-launch layer uses to
// distinguish sibling warmd invocations sharing one CUDA_MPS_PIPE_DIRECTORY.
type ClientID string

// CrashNotifier is what a warmd Supervisor calls when its resident runner
// crashes, so the Coordinator can act on the configured BlastRadiusPolicy.
// Kept as a narrow interface (not a direct Coordinator dependency in warmd)
// so warmd itself stays MPS-agnostic — it reports "I crashed," and something
// else decides what that means for siblings, mirroring the existing
// warm.Leaker seam's own narrow-interface discipline.
type CrashNotifier interface {
	NotifyCrash(client ClientID)
}

// RestartFunc is how the Coordinator actually restarts a client — injected
// so this package never imports worker/warm-runner directly (mirrors
// internal/tenancy's own AWS-free, execution-layer-agnostic design).
type RestartFunc func(client ClientID)

// Coordinator implements Conservative blast-radius handling: when any one
// registered client crashes, every OTHER registered client sharing the same
// MPS context is restarted too. This is the "no per-client isolation" trust
// model made concrete in code, not just documented. Safe for concurrent use.
type Coordinator struct {
	mu      sync.Mutex
	policy  BlastRadiusPolicy
	clients map[ClientID]RestartFunc
}

// NewCoordinator builds a Coordinator for the given policy. Only
// Conservative is implemented (see BlastRadiusPolicy's doc) — passing
// Optimistic or any other/unknown value returns an error immediately, so a
// caller that mistakenly wires up an unimplemented policy finds out at
// construction time (e.g. in a test, or at startup), not later as a panic
// deep inside NotifyCrash when a crash actually happens in production.
func NewCoordinator(policy BlastRadiusPolicy) (*Coordinator, error) {
	switch policy {
	case Conservative:
		return &Coordinator{policy: policy, clients: map[ClientID]RestartFunc{}}, nil
	case Optimistic:
		return nil, fmt.Errorf("mps: Optimistic blast-radius policy has no implementation yet (see BlastRadiusPolicy doc) — do not select it")
	default:
		return nil, fmt.Errorf("mps: unknown BlastRadiusPolicy %q", policy)
	}
}

// Register adds a client to the coordinator's crash-fanout set. restart is
// called for THIS client whenever any registered client (including this one)
// crashes, under the Conservative policy.
func (c *Coordinator) Register(id ClientID, restart RestartFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clients[id] = restart
}

// Unregister removes a client (e.g. on its own clean shutdown) so a future
// crash elsewhere does not try to restart a process that no longer exists.
func (c *Coordinator) Unregister(id ClientID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.clients, id)
}

// NotifyCrash implements CrashNotifier: crashed is the client whose warmd
// Supervisor detected a runner crash. Under Conservative, EVERY registered
// client (crashed and siblings alike) gets restarted — the crashed one
// because it crashed, the siblings because MPS gives no isolation guarantee
// that their state is uncorrupted just because their own process didn't die.
//
// This no longer needs to guard against Optimistic or an unknown policy
// value: NewCoordinator is the only constructor (policy and clients are
// unexported, so no other package can build a Coordinator via a struct
// literal), and it already rejects anything but Conservative. A
// zero-value Coordinator (e.g. `var c Coordinator`) has policy == "", which
// the switch below still treats as Conservative, matching NewCoordinator's
// only valid outcome.
func (c *Coordinator) NotifyCrash(crashed ClientID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.policy {
	case Conservative, "":
		for id, restart := range c.clients {
			restart(id)
		}
	}
}

// StartDaemonScript emits the shell command to start the MPS control daemon
// for one GPU, pinned to its own pipe/log directories so multiple GPUs on a
// multi-GPU instance (not currently one of calque's target sizes, but kept
// correct rather than assuming single-GPU) never collide. gpuIndex is the
// CUDA device ordinal (0, 1, ...), matching CUDA_VISIBLE_DEVICES's own
// indexing — distinct from mig.ProvisionScript's PCI-bus-ID addressing,
// because MPS's own tooling (nvidia-cuda-mps-control) is invoked via
// CUDA_VISIBLE_DEVICES, not nvidia-smi's -i flag.
func StartDaemonScript(gpuIndex int, pipeDir, logDir string) string {
	lines := []string{
		"#!/bin/bash",
		"set -euxo pipefail",
		fmt.Sprintf("sudo mkdir -p %s %s", pipeDir, logDir),
		fmt.Sprintf(
			"sudo sh -c 'export CUDA_VISIBLE_DEVICES=%d CUDA_MPS_PIPE_DIRECTORY=%s CUDA_MPS_LOG_DIRECTORY=%s && nohup nvidia-cuda-mps-control -d'",
			gpuIndex, pipeDir, logDir,
		),
		"echo MPS_DAEMON_STARTED",
	}
	return strings.Join(lines, "\n")
}

// StopDaemonScript emits the shell command to cleanly quit the MPS control
// daemon at pipeDir — the counterpart to StartDaemonScript, used both for
// planned teardown and as the "restart everyone" mechanism a Coordinator's
// RestartFunc would invoke (stop, then a fresh StartDaemonScript run) under
// the Conservative policy.
func StopDaemonScript(pipeDir string) string {
	return fmt.Sprintf("sudo sh -c 'export CUDA_MPS_PIPE_DIRECTORY=%s && echo quit | nvidia-cuda-mps-control'", pipeDir)
}

// ClientEnv is the environment a warmd+runner pair needs to bind to a
// specific MPS context — every client sharing one GPU uses the SAME pipeDir
// (that's what makes them share the daemon), but calque still names it here
// explicitly per client rather than relying on ambient CUDA_MPS_PIPE_DIRECTORY
// inheritance, so a process-launch layer can pass it as an explicit env map
// without depending on shell state.
func ClientEnv(pipeDir string) map[string]string {
	return map[string]string{"CUDA_MPS_PIPE_DIRECTORY": pipeDir}
}
