package exec

import (
	"reflect"
	"sort"
	"testing"

	"github.com/spore-host/calque/internal/ir"
)

// spawnFanoutApp mirrors testdata/scripts/spawn_fanout.py's shape exactly
// (worker_a/worker_b spawned from caller, which is itself .map()'d) — built
// directly as an ir.App rather than parsed, since internal/exec doesn't
// import internal/parse (mirroring shard_test.go's own offline-only style).
func spawnFanoutApp() ir.App {
	return ir.App{
		Name: "spawn-fanout",
		Functions: []ir.Function{
			{Name: "worker_a", Body: "return x * 2", ItemArg: "x", Invoke: ir.InvokeSpawn},
			{Name: "worker_b", Body: "return x + 1", ItemArg: "x", Invoke: ir.InvokeSpawn},
			{Name: "caller", Body: "...", ItemArg: "x", Invoke: ir.InvokeMap},
		},
	}
}

// TestResolveSpawnCallablesFindsOnlySpawnClassified proves the resolver
// picks up every InvokeSpawn-classified function and skips InvokeMap'd
// ones (caller must NOT appear — it's the warm unit, not a spawn target).
func TestResolveSpawnCallablesFindsOnlySpawnClassified(t *testing.T) {
	callables := ResolveSpawnCallables(spawnFanoutApp())
	if len(callables) != 2 {
		t.Fatalf("callables = %d, want 2 (worker_a, worker_b)", len(callables))
	}
	byKey := map[string]SpawnCallable{}
	for _, c := range callables {
		byKey[c.Key] = c
	}
	for _, name := range []string{"worker_a", "worker_b"} {
		if _, ok := byKey[name]; !ok {
			t.Errorf("missing spawn-classified callable %q", name)
		}
	}
	if _, ok := byKey["caller"]; ok {
		t.Error("caller (InvokeMap) must NOT appear in ResolveSpawnCallables' output")
	}
	if byKey["worker_a"].MethodBody != "return x * 2" {
		t.Errorf("worker_a.MethodBody = %q, want the function's real body", byKey["worker_a"].MethodBody)
	}
}

// TestResolveSpawnCallablesPopulatesMethodArgsMinusSelfCls (calque#191):
// a spawn-classified callable with multiple real positional args (e.g.
// AI-Almanac's forecasts_app.py's run_forecast_inference(job_id, model_id,
// config)) must carry ALL of them in MethodArgs, with self/cls stripped —
// before this, only MethodArg (the first name) was ever populated, and
// runner.py's single-param binding silently dropped the rest on real
// hardware.
func TestResolveSpawnCallablesPopulatesMethodArgsMinusSelfCls(t *testing.T) {
	app := ir.App{
		Functions: []ir.Function{
			{Name: "run_forecast_inference", Body: "...", ItemArg: "job_id", Args: []string{"job_id", "model_id", "config"}, Invoke: ir.InvokeSpawn},
		},
	}
	callables := ResolveSpawnCallables(app)
	if len(callables) != 1 {
		t.Fatalf("callables = %d, want 1", len(callables))
	}
	want := []string{"job_id", "model_id", "config"}
	if !reflect.DeepEqual(callables[0].MethodArgs, want) {
		t.Errorf("MethodArgs = %v, want %v", callables[0].MethodArgs, want)
	}
}

// TestResolveSpawnCallablesStripsSelfFromClassMethodArgs is the @app.cls
// sibling of the above: a class method's Args includes "self" (unlike a
// plain @app.function) — MethodArgs must strip it, since self is bound
// separately by runner.py's own __calque_method__(self, ...) signature,
// not part of the splat.
func TestResolveSpawnCallablesStripsSelfFromClassMethodArgs(t *testing.T) {
	app := ir.App{
		Classes: []ir.Class{
			{
				Name: "Batcher", EnterBody: "self.model = load()",
				Methods: []ir.Function{
					{Name: "generate", Body: "...", ItemArg: "a", Args: []string{"self", "a", "b"}, Invoke: ir.InvokeSpawn},
				},
			},
		},
	}
	callables := ResolveSpawnCallables(app)
	if len(callables) != 1 {
		t.Fatalf("callables = %d, want 1", len(callables))
	}
	want := []string{"a", "b"}
	if !reflect.DeepEqual(callables[0].MethodArgs, want) {
		t.Errorf("MethodArgs = %v, want %v (self must be stripped)", callables[0].MethodArgs, want)
	}
}

// TestResolveSpawnCallablesHandlesClasses proves an @app.cls's spawn-
// classified method resolves with the CLASS's EnterBody (once-per-container
// load) plus that specific method's own Body — mirroring how pickWarmUnit
// already treats a class: EnterBody runs once, the method drains items.
func TestResolveSpawnCallablesHandlesClasses(t *testing.T) {
	app := ir.App{
		Classes: []ir.Class{
			{
				Name: "Batcher", GPU: "H100", EnterBody: "self.model = load()",
				Methods: []ir.Function{
					{Name: "generate", Body: "return self.model(x)", ItemArg: "x", Invoke: ir.InvokeSpawn},
				},
			},
		},
	}
	callables := ResolveSpawnCallables(app)
	if len(callables) != 1 {
		t.Fatalf("callables = %d, want 1", len(callables))
	}
	c := callables[0]
	if c.Key != "generate" || !c.IsClass {
		t.Errorf("callable = %+v, want Key=generate IsClass=true", c)
	}
	if c.EnterBody != "self.model = load()" {
		t.Errorf("EnterBody = %q, want the class's @enter body", c.EnterBody)
	}
	if c.GPU != "H100" {
		t.Errorf("GPU = %q, want H100 (inherited from the owning class)", c.GPU)
	}
}

// TestResolveOneSpawnCallableMatchesResolveSpawnCallables proves the
// single-name FindFunction/FindClass-based resolver (this issue's own named
// entry point) returns the SAME data as the full-app scan, for a name that
// exists — the two must agree, not just both compile.
func TestResolveOneSpawnCallableMatchesResolveSpawnCallables(t *testing.T) {
	app := spawnFanoutApp()
	all := ResolveSpawnCallables(app)
	var wantA SpawnCallable
	for _, c := range all {
		if c.Key == "worker_a" {
			wantA = c
		}
	}
	got, err := ResolveOneSpawnCallable(app, "worker_a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantA) {
		t.Errorf("ResolveOneSpawnCallable(worker_a) = %+v, want %+v (must match ResolveSpawnCallables)", got, wantA)
	}
}

// TestResolveOneSpawnCallableErrorsOnUnknownName proves a name that's
// neither a Function nor a Class returns a typed error, not a zero value
// that looks like a found-but-empty callable.
func TestResolveOneSpawnCallableErrorsOnUnknownName(t *testing.T) {
	_, err := ResolveOneSpawnCallable(spawnFanoutApp(), "does_not_exist")
	if err == nil {
		t.Fatal("want an error for an unknown callable name")
	}
	if e, ok := err.(*ErrCallableNotFound); !ok || e.Name != "does_not_exist" {
		t.Errorf("err = %v (%T), want *ErrCallableNotFound{Name: does_not_exist}", err, err)
	}
}

func strp(s string) *string { return &s }

// TestBuildSpawnManifestsOnePerCallable proves each spawn-classified
// callable gets its OWN NamedShard with ITS OWN body — replacing
// fleetrun.go's shared realEnterBody/realMethodBody constants, per this
// issue's own "Do" section — and that a call site's argument becomes that
// shard's single item payload.
func TestBuildSpawnManifestsOnePerCallable(t *testing.T) {
	callables := ResolveSpawnCallables(spawnFanoutApp())
	sites := []SpawnCallSite{
		{Target: "worker_a", Args: []*string{strp("5")}},
		{Target: "worker_b", Args: []*string{strp("5")}},
	}
	shards := BuildSpawnManifests(callables, sites, "runs/x", "runs/x/artifacts")
	if len(shards) != 2 {
		t.Fatalf("shards = %d, want 2 (one per spawned callable)", len(shards))
	}
	byKey := map[string]NamedShard{}
	for _, sh := range shards {
		byKey[sh.Key] = sh
	}
	for _, name := range []string{"worker_a", "worker_b"} {
		sh, ok := byKey[name]
		if !ok {
			t.Fatalf("missing shard for %q", name)
		}
		if len(sh.Items) != 1 || sh.Items[0].Payload != "5" {
			t.Errorf("%s items = %+v, want one item with payload \"5\"", name, sh.Items)
		}
		if sh.ManifestKey == "" || sh.ResultPrefix == "" {
			t.Errorf("%s shard has empty S3 keys: %+v", name, sh)
		}
	}
	// Distinct namespaces — the two callables must not collide in S3.
	if byKey["worker_a"].ManifestKey == byKey["worker_b"].ManifestKey {
		t.Error("worker_a and worker_b share a manifest key; namespaces must be distinct")
	}
}

// TestBuildSpawnManifestsMultipleCallSitesBecomeMultipleItems proves a
// callable spawned more than once (e.g. inside a loop) gets ONE shard with
// MULTIPLE items — not multiple shards — since they share the same
// EnterBody/MethodBody and only differ by argument.
func TestBuildSpawnManifestsMultipleCallSitesBecomeMultipleItems(t *testing.T) {
	callables := ResolveSpawnCallables(spawnFanoutApp())
	sites := []SpawnCallSite{
		{Target: "worker_a", Args: []*string{strp("1")}},
		{Target: "worker_a", Args: []*string{strp("2")}},
		{Target: "worker_a", Args: []*string{strp("3")}},
	}
	shards := BuildSpawnManifests(callables, sites, "runs/x", "runs/x/artifacts")
	var got []NamedShard
	for _, sh := range shards {
		if sh.Key == "worker_a" {
			got = append(got, sh)
		}
	}
	if len(got) != 1 {
		t.Fatalf("worker_a shards = %d, want exactly 1 (all call sites fold into one shard)", len(got))
	}
	if len(got[0].Items) != 3 {
		t.Fatalf("worker_a items = %d, want 3", len(got[0].Items))
	}
	var payloads []string
	for _, it := range got[0].Items {
		payloads = append(payloads, it.Payload.(string))
	}
	sort.Strings(payloads)
	if !reflect.DeepEqual(payloads, []string{"1", "2", "3"}) {
		t.Errorf("payloads = %v, want [1 2 3]", payloads)
	}
}

// TestBuildSpawnManifestsSkipsCallableWithNoCallSites proves a
// spawn-classified callable with NO matching call site (defensive: should
// not normally happen) produces no shard rather than an empty, useless one.
func TestBuildSpawnManifestsSkipsCallableWithNoCallSites(t *testing.T) {
	callables := ResolveSpawnCallables(spawnFanoutApp())
	sites := []SpawnCallSite{{Target: "worker_a", Args: []*string{strp("1")}}}
	shards := BuildSpawnManifests(callables, sites, "runs/x", "runs/x/artifacts")
	if len(shards) != 1 {
		t.Fatalf("shards = %d, want 1 (worker_b has no call sites, must be skipped)", len(shards))
	}
	if shards[0].Key != "worker_a" {
		t.Errorf("shard = %+v, want worker_a", shards[0])
	}
}

// TestSpawnArgsPayloadShapes proves the payload conversion matches warmd's
// single-positional-arg protocol for the common (0 or 1 arg) cases, and
// falls back to a list for the uncommon (2+ arg) case rather than dropping
// data silently.
func TestSpawnArgsPayloadShapes(t *testing.T) {
	if p := spawnArgsPayload(nil); p != nil {
		t.Errorf("spawnArgsPayload(nil) = %v, want nil", p)
	}
	if p := spawnArgsPayload([]*string{strp("x")}); p != "x" {
		t.Errorf("spawnArgsPayload(1 arg) = %v, want the bare string \"x\"", p)
	}
	p := spawnArgsPayload([]*string{strp("a"), strp("b")})
	list, ok := p.([]string)
	if !ok || !reflect.DeepEqual(list, []string{"a", "b"}) {
		t.Errorf("spawnArgsPayload(2 args) = %v (%T), want []string{a, b}", p, p)
	}
}
