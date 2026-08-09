package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// capturingSpotLeaker records every Leak call so a test can assert on kind/detail
// without depending on stderrLeaker's stderr side effect.
type capturingSpotLeaker struct {
	mu    sync.Mutex
	kinds []string
}

func (c *capturingSpotLeaker) Leak(kind, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kinds = append(c.kinds, kind)
}

func (c *capturingSpotLeaker) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.kinds)
}

func (c *capturingSpotLeaker) first() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.kinds) == 0 {
		return ""
	}
	return c.kinds[0]
}

// withFastPoll points pollSpotInterruption at a short interval for the
// duration of a test, restoring the real value afterward.
func withFastPoll(t *testing.T, interval time.Duration) {
	t.Helper()
	orig := spotInterruptionPollInterval
	spotInterruptionPollInterval = interval
	t.Cleanup(func() { spotInterruptionPollInterval = orig })
}

// TestPollSpotInterruption_DetectsNoticeOn200 verifies that a 200 response
// from the IMDS spot/instance-action endpoint (standing in via httptest.Server,
// since the real 169.254.169.254 endpoint isn't reachable/desired in a test)
// is detected as an interruption notice and leaked with the distinct
// "spot_interruption" kind (calque#94).
func TestPollSpotInterruption_DetectsNoticeOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"action": "terminate", "time": "2026-08-09T00:00:00Z"}`))
	}))
	defer srv.Close()

	origURL := spotInterruptionURL
	spotInterruptionURL = srv.URL
	defer func() { spotInterruptionURL = origURL }()
	withFastPoll(t, 10*time.Millisecond)

	leaker := &capturingSpotLeaker{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	pollSpotInterruption(ctx, leaker)

	if leaker.count() == 0 {
		t.Fatalf("expected at least one Leak call on a 200 response, got none")
	}
	if got := leaker.first(); got != "spot_interruption" {
		t.Errorf("expected leak kind %q, got %q", "spot_interruption", got)
	}
}

// TestPollSpotInterruption_NoLeakOn404 verifies the common case — IMDS 404s
// until an interruption is actually pending — is treated as "no interruption
// yet" and does NOT trigger a leak (nor error/retry-storm).
func TestPollSpotInterruption_NoLeakOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	origURL := spotInterruptionURL
	spotInterruptionURL = srv.URL
	defer func() { spotInterruptionURL = origURL }()
	withFastPoll(t, 10*time.Millisecond)

	leaker := &capturingSpotLeaker{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	pollSpotInterruption(ctx, leaker)

	if leaker.count() != 0 {
		t.Errorf("expected no Leak calls on a 404 response, got %d", leaker.count())
	}
}

// TestPollSpotInterruption_StopsOnContextCancel verifies the poller exits
// promptly when its context is cancelled (the mechanism runOnInstance uses to
// stop the poller via defer stopInterruptPoll()).
func TestPollSpotInterruption_StopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	origURL := spotInterruptionURL
	spotInterruptionURL = srv.URL
	defer func() { spotInterruptionURL = origURL }()
	withFastPoll(t, 5*time.Millisecond)

	leaker := &capturingSpotLeaker{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		pollSpotInterruption(ctx, leaker)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pollSpotInterruption did not return within 500ms of context cancellation")
	}
}
