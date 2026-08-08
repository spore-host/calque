package mps

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// TestRequireOptInRefusesByDefault proves the opt-in gate blocks any
// MPS-enabling call site that hasn't explicitly set it — mirrors
// cmd/calque/main.go's --i-understand-this-spends-money discipline for a
// DIFFERENT risk category (availability/blast-radius, not billing).
func TestRequireOptInRefusesByDefault(t *testing.T) {
	if err := RequireOptIn(false); !errors.Is(err, ErrOptInRequired) {
		t.Errorf("RequireOptIn(false) = %v, want ErrOptInRequired", err)
	}
	if err := RequireOptIn(true); err != nil {
		t.Errorf("RequireOptIn(true) = %v, want nil", err)
	}
}

// TestConservativeCoordinatorRestartsEveryClientOnAnyCrash is the core
// blast-radius proof (calque#108): MPS gives no per-client isolation, so
// under the Conservative policy, ONE client crashing must restart EVERY
// registered client sharing that MPS context — including itself and every
// sibling, not just the one that actually crashed.
func TestConservativeCoordinatorRestartsEveryClientOnAnyCrash(t *testing.T) {
	c := NewCoordinator(Conservative)

	var mu sync.Mutex
	restarted := map[ClientID]int{}
	record := func(id ClientID) func(ClientID) {
		return func(_ ClientID) {
			mu.Lock()
			defer mu.Unlock()
			restarted[id]++
		}
	}

	c.Register("client-0", record("client-0"))
	c.Register("client-1", record("client-1"))
	c.Register("client-2", record("client-2"))

	c.NotifyCrash("client-1") // only client-1 actually crashed

	mu.Lock()
	defer mu.Unlock()
	for _, id := range []ClientID{"client-0", "client-1", "client-2"} {
		if restarted[id] != 1 {
			t.Errorf("client %q restarted %d times, want exactly 1 (conservative fanout must restart every sibling, not just the crashed one)", id, restarted[id])
		}
	}
}

// TestUnregisterExcludesClientFromFutureFanout proves a cleanly-shut-down
// client (e.g. it finished its work and checked in via tenancy.CheckIn) is
// not spuriously "restarted" after it's gone.
func TestUnregisterExcludesClientFromFutureFanout(t *testing.T) {
	c := NewCoordinator(Conservative)
	calls := 0
	c.Register("client-0", func(ClientID) { calls++ })
	c.Register("client-1", func(ClientID) { calls++ })

	c.Unregister("client-0")
	c.NotifyCrash("client-1")

	if calls != 1 {
		t.Errorf("calls = %d, want 1 (client-0 was unregistered and must not be restarted)", calls)
	}
}

// TestOptimisticPolicyPanicsRatherThanSilentlyNoOp proves selecting the
// unimplemented Optimistic policy fails LOUDLY (a panic a test catches
// immediately) rather than silently doing nothing, which would look like a
// working isolation guarantee MPS does not actually provide.
func TestOptimisticPolicyPanicsRatherThanSilentlyNoOp(t *testing.T) {
	c := NewCoordinator(Optimistic)
	c.Register("client-0", func(ClientID) {})

	defer func() {
		if r := recover(); r == nil {
			t.Error("NotifyCrash under Optimistic policy did not panic; want a loud failure, not a silent no-op")
		}
	}()
	c.NotifyCrash("client-0")
}

// TestStartDaemonScriptPinsPipeAndLogDirs proves the emitted script scopes
// the MPS daemon to explicit, GPU-specific directories rather than relying
// on ambient defaults that would collide across multiple GPUs on one
// instance.
func TestStartDaemonScriptPinsPipeAndLogDirs(t *testing.T) {
	script := StartDaemonScript(0, "/tmp/mps-0/pipe", "/tmp/mps-0/log")
	if !strings.Contains(script, "CUDA_VISIBLE_DEVICES=0") {
		t.Errorf("script does not pin the GPU index:\n%s", script)
	}
	if !strings.Contains(script, "CUDA_MPS_PIPE_DIRECTORY=/tmp/mps-0/pipe") {
		t.Errorf("script does not pin the pipe directory:\n%s", script)
	}
	if !strings.Contains(script, "CUDA_MPS_LOG_DIRECTORY=/tmp/mps-0/log") {
		t.Errorf("script does not pin the log directory:\n%s", script)
	}
	if !strings.Contains(script, "mkdir -p /tmp/mps-0/pipe /tmp/mps-0/log") {
		t.Errorf("script does not create the pinned directories before starting the daemon:\n%s", script)
	}
}

// TestStopDaemonScriptTargetsPipeDir proves the stop command addresses the
// SAME pipe directory a start command used, so it quits the right daemon on
// a multi-GPU instance rather than whichever one ambient env vars point at.
func TestStopDaemonScriptTargetsPipeDir(t *testing.T) {
	script := StopDaemonScript("/tmp/mps-0/pipe")
	if !strings.Contains(script, "CUDA_MPS_PIPE_DIRECTORY=/tmp/mps-0/pipe") {
		t.Errorf("stop script does not target the given pipe dir:\n%s", script)
	}
	if !strings.Contains(script, "echo quit") {
		t.Errorf("stop script does not send the quit command:\n%s", script)
	}
}

// TestClientEnvCarriesThePipeDir proves the per-client env map a
// process-launch layer would pass to warmd names the SAME pipe directory
// its daemon was started with — this is what makes several warmd processes
// actually SHARE one MPS context rather than each starting/using a private
// default.
func TestClientEnvCarriesThePipeDir(t *testing.T) {
	env := ClientEnv("/tmp/mps-0/pipe")
	if env["CUDA_MPS_PIPE_DIRECTORY"] != "/tmp/mps-0/pipe" {
		t.Errorf("ClientEnv = %v, want CUDA_MPS_PIPE_DIRECTORY=/tmp/mps-0/pipe", env)
	}
}
