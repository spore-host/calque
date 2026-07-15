package plan

import (
	"testing"

	"github.com/spore-host/calque/internal/leak"
)

// TestTruffleResolvesDefaultCard: the spike's default card resolves to g7e via
// truffle (offline). Locks the real integration behavior we observed.
func TestTruffleResolvesDefaultCard(t *testing.T) {
	rep := &leak.Report{}
	r := NewTruffleResolver(rep)
	cands, err := r.Resolve("RTX PRO 6000")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("no candidates for RTX PRO 6000")
	}
	foundG7e := false
	for _, c := range cands {
		if c.Family == "g7e" {
			foundG7e = true
		}
	}
	if !foundG7e {
		t.Errorf("expected a g7e candidate, got %+v", cands)
	}
	// The smallest should be g7e.2xlarge (family floor — there is no g7e.xlarge).
	pick, err := PickSmallest(cands)
	if err != nil {
		t.Fatal(err)
	}
	if pick.Instance != "g7e.2xlarge" {
		t.Errorf("smallest = %q, want g7e.2xlarge", pick.Instance)
	}
}

// TestTruffleGuardsMatchAllFallback: an unresolved card must ERROR, not silently
// resolve to a `.*` match-all (truffle#90 footgun). This is the credibility guard.
func TestTruffleGuardsMatchAllFallback(t *testing.T) {
	rep := &leak.Report{}
	r := NewTruffleResolver(rep)
	_, err := r.Resolve("totally-not-a-real-gpu-9999")
	if err == nil {
		t.Fatal("expected error for unresolvable card, got nil (would be a .* match-all)")
	}
	if rep.Len() == 0 {
		t.Error("expected a leak recording the non-resolution")
	}
}
