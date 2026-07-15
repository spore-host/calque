package cost

import (
	"path/filepath"
	"strings"
	"testing"
)

func loadTestRates(t *testing.T) *Rates {
	t.Helper()
	p, _ := filepath.Abs("../../config/rates.json")
	r, err := LoadRates(p)
	if err != nil {
		t.Fatalf("LoadRates: %v", err)
	}
	return r
}

func TestRatesLoad(t *testing.T) {
	r := loadTestRates(t)
	if rm, ok := r.ModalRate("H100"); !ok || rm <= 0 {
		t.Errorf("H100 modal rate = %v, ok=%v", rm, ok)
	}
	// alias fold
	if _, ok := r.ModalRate("h100"); !ok {
		t.Error("case-insensitive modal rate lookup failed")
	}
	ra, measured, ok := r.AWSOnDemandPerSecond("g7e.xlarge")
	if !ok || ra <= 0 {
		t.Errorf("g7e rate = %v ok=%v", ra, ok)
	}
	if measured {
		t.Error("g7e rate should be flagged NOT measured (proxy) until it's in the Pricing API")
	}
}

// TestCrossoverExists: with a realistic high-occupancy measurement, AWS should
// win at scale and K should be a small finite number.
func TestCrossoverExists(t *testing.T) {
	r := loadTestRates(t)
	m := &Model{Rates: r, M: Measured{
		CardAskedFor: "H100", InstanceUsed: "g7e.xlarge",
		SecPerItem: 0.5, Occupancy: 0.85, SampleItems: 100,
		AcquireSeconds: 120, EnterSeconds: 30,
	}}
	k, ok, err := m.Crossover(1.0)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a finite crossover at 85% occupancy")
	}
	// Sanity: below K Modal is cheaper, at/above K AWS is cheaper.
	below, _ := mustModalAWS(t, m, k-1)
	at, _ := mustModalAWS(t, m, k+1)
	if !(below.modal <= below.aws) {
		t.Errorf("at K-1=%d expected Modal<=AWS, got modal=%.4f aws=%.4f", k-1, below.modal, below.aws)
	}
	if !(at.aws < at.modal) {
		t.Errorf("at K+1=%d expected AWS<Modal, got modal=%.4f aws=%.4f", k+1, at.modal, at.aws)
	}
}

// TestStayOnModal: at very low occupancy the AWS rectangle is so padded with idle
// that AWS never undercuts Modal. The instrument MUST be willing to say so (§9).
func TestStayOnModal(t *testing.T) {
	r := loadTestRates(t)
	// Occupancy low enough that AWS per-item cost exceeds Modal's.
	// awsSlope = ra*c/p ; modalSlope = rm*c. Stay-on-Modal when ra/p >= rm.
	// ra(g7e/s) ~ 2.24/3600 = 6.2e-4 ; rm(H100/s)=1.097e-3. ra/p>=rm => p<=0.567.
	m := &Model{Rates: r, M: Measured{
		CardAskedFor: "H100", InstanceUsed: "g7e.xlarge",
		SecPerItem: 0.5, Occupancy: 0.10, SampleItems: 100,
		AcquireSeconds: 120, EnterSeconds: 30,
	}}
	_, ok, err := m.Crossover(1.0)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("at 10% occupancy AWS should never win on-demand; expected stay-on-Modal")
	}
	v, err := m.Verdict(100000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v, "STAY ON MODAL") {
		t.Errorf("verdict should say STAY ON MODAL at 10%% occupancy, got:\n%s", v)
	}
}

// TestVerdictFlagsProxy: the AWS side of K must be labeled proxy while g7e isn't
// in the Pricing API.
func TestVerdictFlagsProxy(t *testing.T) {
	r := loadTestRates(t)
	m := &Model{Rates: r, M: Measured{
		CardAskedFor: "H100", InstanceUsed: "g7e.xlarge",
		SecPerItem: 0.5, Occupancy: 0.85, SampleItems: 100, AcquireSeconds: 120, EnterSeconds: 30,
	}}
	v, err := m.Verdict(100000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v, "PROXY") {
		t.Errorf("verdict must flag the g7e proxy rate, got:\n%s", v)
	}
}

type pair struct{ modal, aws float64 }

func mustModalAWS(t *testing.T, m *Model, n int) (pair, pair) {
	t.Helper()
	md, err := m.ModalAt(n)
	if err != nil {
		t.Fatal(err)
	}
	aw, _, err := m.AWSAt(n)
	if err != nil {
		t.Fatal(err)
	}
	return pair{md.Dollars, aw.Dollars}, pair{}
}
