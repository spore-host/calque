package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
)

// TestSpawnDictDispatchEndToEndProducesRealShards is the end-to-end
// regression guard for calque#189: it wires Parse -> ResolveSpawnCallables
// -> SpawnCallSitesReport -> BuildSpawnManifests together exactly as
// spawnRunFromScript (spawnrun.go) does, against a real fixture whose
// .spawn() receiver is a dict-of-functions Subscript (mirroring
// AI-Almanac's forecasts_app.py exactly).
//
// This test exists because the original #189 fix (SpawnCallSites'
// candidate expansion) was necessary but NOT sufficient on its own: a
// diagnostic run of this exact pipeline, built to answer "does spawn-run
// actually produce a shard for this script," found that
// ResolveSpawnCallables returned ZERO callables even after that fix — the
// picked function's own ir.Function.Invoke was still "" (not
// ir.InvokeSpawn), because invocationKinds' consider() call for the
// "spawn" case was still keyed on the empty ic.Target, never the
// candidates. SpawnCallSites (the call-SITE side) and
// ResolveSpawnCallables (the callable-DEFINITION side) are two
// independently-tested layers; the defect lived in the gap BETWEEN them,
// invisible to either layer's own isolated unit tests. Only assembling the
// real pipeline end-to-end — not just asserting on SpawnCallSites' return
// value in isolation — surfaced it.
func TestSpawnDictDispatchEndToEndProducesRealShards(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; skipping pyast contract test")
	}
	setPyastDirEnv(t)
	script, err := filepath.Abs("../../testdata/scripts/spawn_dict_dispatch.py")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rep := &leak.Report{}
	runner, args := parse.DefaultRunner(pyastDir())

	app, err := parse.Parse(ctx, script, rep, runner, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var bundle ir.Function
	found := false
	for _, f := range app.Functions {
		if f.Name == "bundle" {
			bundle, found = f, true
		}
	}
	if !found {
		t.Fatal(`app.Functions has no "bundle" — fixture regressed`)
	}
	if bundle.Invoke != ir.InvokeSpawn {
		t.Errorf(`bundle.Invoke = %q, want %q — a dict-subscript .spawn() candidate must classify the CANDIDATE, not the empty target`, bundle.Invoke, ir.InvokeSpawn)
	}

	callables := calexec.ResolveSpawnCallables(app)
	if len(callables) != 1 || callables[0].Key != "bundle" {
		t.Fatalf("ResolveSpawnCallables = %+v, want exactly one callable keyed \"bundle\"", callables)
	}

	sites, err := parse.SpawnCallSitesReport(ctx, script, rep, runner, args...)
	if err != nil {
		t.Fatalf("SpawnCallSitesReport: %v", err)
	}
	if len(sites) != 1 || sites[0].Target != "bundle" {
		t.Fatalf("SpawnCallSitesReport = %+v, want exactly one site targeting \"bundle\"", sites)
	}

	callSites := make([]calexec.SpawnCallSite, len(sites))
	for i, s := range sites {
		callSites[i] = calexec.SpawnCallSite{Target: s.Target, Args: s.Args}
	}
	shards := calexec.BuildSpawnManifests(callables, callSites, "s3://bucket/base", "s3://bucket/artifacts")
	if len(shards) != 1 {
		t.Fatalf("BuildSpawnManifests produced %d shard(s), want exactly 1 — this is the real, end-user-visible failure mode #189 exists to prevent: zero shards means `calque spawn-run` silently does nothing for this script. shards=%+v", len(shards), shards)
	}
	if shards[0].Key != "bundle" {
		t.Errorf("shards[0].Key = %q, want %q", shards[0].Key, "bundle")
	}
}
