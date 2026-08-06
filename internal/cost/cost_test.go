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
	ra, measured, ok := r.AWSOnDemandPerSecond("g7e.2xlarge")
	if !ok || ra <= 0 {
		t.Errorf("g7e rate = %v ok=%v", ra, ok)
	}
	if !measured {
		t.Error("g7e.2xlarge rate IS live in the Pricing API; should be flagged measured")
	}
}

// TestCrossoverExists: with a realistic high-occupancy measurement, AWS should
// win at scale and K should be a small finite number.
func TestCrossoverExists(t *testing.T) {
	r := loadTestRates(t)
	// g7e.2xlarge/s = 3.36312/3600 = 9.34e-4; H100/s = 1.097e-3. Stay-on-Modal
	// threshold is p <= ra/rm = 0.851, so pick 0.95 to be clearly above it.
	m := &Model{Rates: r, M: Measured{
		CardAskedFor: "H100", InstanceUsed: "g7e.2xlarge",
		SecPerItem: 0.5, Occupancy: 0.95, SampleItems: 100,
		AcquireSeconds: 120, EnterSeconds: 30,
	}}
	k, ok, err := m.Crossover(1.0)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a finite crossover at 95% occupancy")
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
	// ra(g7e.2xlarge/s)=3.36312/3600=9.34e-4 ; rm(H100/s)=1.097e-3 => p<=0.851.
	m := &Model{Rates: r, M: Measured{
		CardAskedFor: "H100", InstanceUsed: "g7e.2xlarge",
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

// TestVerdictFlagsMeasured: g7e.2xlarge IS live in the Pricing API, so the AWS
// side of K must be labeled measured (the flag mechanism still fires — it just
// reports the honest state). A proxy rate would flip this to PROXY.
func TestVerdictFlagsMeasured(t *testing.T) {
	r := loadTestRates(t)
	m := &Model{Rates: r, M: Measured{
		CardAskedFor: "H100", InstanceUsed: "g7e.2xlarge",
		SecPerItem: 0.5, Occupancy: 0.95, SampleItems: 100, AcquireSeconds: 120, EnterSeconds: 30,
	}}
	v, err := m.Verdict(100000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v, "measured") || strings.Contains(v, "PROXY") {
		t.Errorf("verdict AWS side should be [measured], not proxy, got:\n%s", v)
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

// TestVerdictLabelsOccupancyScope proves K declares WHICH window its occupancy came
// from (#71). The same 45% means two different things depending on scope, and a
// whole-run figure additionally double-counts the load idle (once as low occupancy,
// again as EnterSeconds) — so the verdict must warn that such a K is pessimistic for
// AWS rather than let the reader assume steady-state fill.
func TestVerdictLabelsOccupancyScope(t *testing.T) {
	r := loadTestRates(t)
	base := Measured{
		CardAskedFor: "H100", InstanceUsed: "g7e.2xlarge",
		SecPerItem: 0.5, Occupancy: 0.45, SampleItems: 1000,
		AcquireSeconds: 20, EnterSeconds: 210,
	}

	wholeRun := base
	wholeRun.OccupancyScope = "whole_run"
	v, err := (&Model{Rates: r, M: wholeRun}).Verdict(100000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v, "WHOLE-RUN") {
		t.Errorf("whole-run K must label its occupancy window, got:\n%s", v)
	}
	if !strings.Contains(v, "pessimistic for AWS") {
		t.Errorf("whole-run K must warn it is pessimistic for AWS (load idle double-counted), got:\n%s", v)
	}

	infer := base
	infer.OccupancyScope = "inference"
	v2, err := (&Model{Rates: r, M: infer}).Verdict(100000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v2, "inference window") {
		t.Errorf("inference-scoped K must say so, got:\n%s", v2)
	}
	if strings.Contains(v2, "pessimistic for AWS") {
		t.Errorf("inference-scoped K must NOT carry the whole-run caveat, got:\n%s", v2)
	}

	// An UNLABELED measurement (pre-#71 artifact) must read as whole-run, never be
	// silently presented as an inference-window number.
	v3, err := (&Model{Rates: r, M: base}).Verdict(100000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v3, "WHOLE-RUN") {
		t.Errorf("unlabeled occupancy must default to the WHOLE-RUN label, got:\n%s", v3)
	}
}
