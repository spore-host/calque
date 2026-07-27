package main

import (
	"testing"

	"github.com/spore-host/calque/internal/gate"
	"github.com/spore-host/calque/internal/ir"
)

// TestPrintOffersAndStop locks the G3 short-circuit contract: an exact offer means
// "stop before acquiring a GPU"; no offer means "proceed to the GPU path".
func TestPrintOffersAndStop(t *testing.T) {
	if printOffersAndStop(nil) {
		t.Error("no offers must NOT stop the run (proceed to GPU path)")
	}
	exact := []*gate.ReplacementOffer{{
		ModelRef: "meta-llama/Meta-Llama-3-8B-Instruct", BedrockID: "meta.llama3-8b-instruct-v1:0",
		Exact: true, Regions: []string{"us-east-1"},
	}}
	if !printOffersAndStop(exact) {
		t.Error("an exact offer MUST stop the run before acquisition")
	}
}

// TestServeAppDetection (F2): serveApp identifies a serve-shaped app so run() leaks
// the deferred shape instead of hard-erroring.
func TestServeAppDetection(t *testing.T) {
	batch := irApp(ir.Function{Name: "gen", EntryKind: ir.EntryBatch})
	if serveApp(batch) {
		t.Error("a batch app must not be detected as serve")
	}
	serve := irApp(ir.Function{Name: "endpoint", EntryKind: ir.EntryServe})
	if !serveApp(serve) {
		t.Error("an app with a serve entry function must be detected as serve")
	}
}

func irApp(fns ...ir.Function) ir.App { return ir.App{Functions: fns} }
