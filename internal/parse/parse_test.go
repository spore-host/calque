package parse

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// runner points the test at the real pyast helper via uv. Skips if uv isn't on
// PATH — this test exercises the Python↔Go contract, which needs the helper.
func runner(t *testing.T) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping pyast contract test")
	}
	dir, err := filepath.Abs("../../tools/pyast")
	if err != nil {
		t.Fatal(err)
	}
	r, args := DefaultRunner(dir)
	return r, args
}

func TestParseMapBatch(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/map_batch_inference.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if app.Name != "map-batch-inference" {
		t.Errorf("app name = %q, want map-batch-inference", app.Name)
	}
	if len(app.Classes) != 1 {
		t.Fatalf("classes = %d, want 1", len(app.Classes))
	}
	cls := app.Classes[0]
	if cls.Name != "Batcher" || cls.GPU != "H100" {
		t.Errorf("class = {%q, gpu %q}, want {Batcher, H100}", cls.Name, cls.GPU)
	}
	if cls.EnterBody == "" {
		t.Error("Batcher @enter body is empty; warm-load-once body was dropped")
	}
	if cls.Volumes["/weights"] != "weights" {
		t.Errorf("volumes = %v, want /weights->weights", cls.Volumes)
	}
	if cls.Timeout != 1200 {
		t.Errorf("timeout = %d, want 1200", cls.Timeout)
	}
	if len(cls.Methods) != 1 || cls.Methods[0].Name != "generate" {
		t.Fatalf("methods = %+v, want one 'generate'", cls.Methods)
	}
	// generate.map(...) is called in the entrypoint, so IsMap must be true.
	if !cls.Methods[0].IsMap {
		t.Error("generate.IsMap = false; .map() call site was not detected")
	}
	// Image DSL must carry through, bodies verbatim.
	if app.Image.Base != "debian_slim" {
		t.Errorf("image base = %q, want debian_slim", app.Image.Base)
	}
	wantPip := map[string]bool{"vllm==0.6.3": true, "transformers==4.45.2": true, "huggingface_hub": true}
	for _, p := range app.Image.Pip {
		delete(wantPip, p)
	}
	if len(wantPip) != 0 {
		t.Errorf("image pip missing: %v (got %v)", wantPip, app.Image.Pip)
	}
	// A clean, well-formed script should emit no parse-stage leaks.
	if rep.Len() != 0 {
		t.Errorf("unexpected leaks on clean script: %+v", rep.Leaks)
	}
}

func TestParseVolumeCacheHasFunctionAndClass(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/volume_cache.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(app.Functions) != 1 || app.Functions[0].Name != "download_weights" {
		t.Errorf("functions = %+v, want one download_weights", app.Functions)
	}
	if app.Functions[0].GPU != "" {
		t.Errorf("download_weights gpu = %q, want empty", app.Functions[0].GPU)
	}
	if len(app.Classes) != 1 || app.Classes[0].GPU != "L4" {
		t.Errorf("class = %+v, want one with gpu L4", app.Classes)
	}
}

// TestParsePortableConfig exercises the M6 B/C pass-through: portable kwargs land
// in ir.Config, autoscaling kwargs are recognized-and-leaked (not silently
// dropped), and the sync invocation idioms are classified beyond plain .map.
func TestParsePortableConfig(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/portable_config.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := map[string]int{}
	for i, f := range app.Functions {
		byName[f.Name] = i
	}
	tr := app.Functions[byName["transform"]]
	if tr.Config.CPU != 4 || tr.Config.MemoryMB != 8192 || tr.Config.Retries != 3 || tr.Config.Region != "us-west-2" || tr.Config.Cloud != "aws" {
		t.Errorf("transform.Config = %+v, want cpu4/mem8192/retries3/us-west-2/aws", tr.Config)
	}
	// Invocation idioms: transform is both .map and .for_each -> map wins (precedence).
	if tr.Invoke != ir.InvokeMap {
		t.Errorf("transform.Invoke = %q, want map (map beats for_each)", tr.Invoke)
	}
	cb := app.Functions[byName["combine"]]
	if cb.Invoke != ir.InvokeStarmap {
		t.Errorf("combine.Invoke = %q, want starmap (starmap beats remote)", cb.Invoke)
	}
	// keep_warm= must be recognized and leaked as behind-the-seam, not dropped.
	foundAutoscale := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "keep_warm") && strings.Contains(l.Detail, "behind the seam") {
			foundAutoscale = true
		}
	}
	if !foundAutoscale {
		t.Errorf("keep_warm= should be recognized+leaked as behind-the-seam; leaks=%+v", rep.Leaks)
	}
	// cpu=(request, limit): the request (first) element lands in Config.CPU, and
	// the dropped limit must be leaked, mirroring memory=[request,limit] (calque#77).
	bp := app.Functions[byName["bin_pack"]]
	if bp.Config.CPU != 0.25 {
		t.Errorf("bin_pack.Config.CPU = %v, want 0.25 (request element of cpu=(0.25,1))", bp.Config.CPU)
	}
	foundCPULeak := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "cpu=[request,limit]") {
			foundCPULeak = true
		}
	}
	if !foundCPULeak {
		t.Errorf("cpu=(request,limit) should leak the dropped limit; leaks=%+v", rep.Leaks)
	}
	// Two @app.local_entrypoint()s in one script must both survive (calque#78) —
	// neither the pyast collector nor the IR may collapse to just one.
	if len(app.Entrypoints) != 2 {
		t.Fatalf("Entrypoints = %d, want 2 (main, secondary)", len(app.Entrypoints))
	}
	epNames := map[string]bool{}
	for _, ep := range app.Entrypoints {
		epNames[ep.Name] = true
	}
	if !epNames["main"] || !epNames["secondary"] {
		t.Errorf("Entrypoints = %v, want {main, secondary}", epNames)
	}
	// .local() call sites must be recognized and leaked — calque ships only the
	// picked warm unit's body verbatim, so a sibling called via .local() is not
	// in scope and would NameError at runtime (calque#81).
	foundLocalLeak := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "bin_pack.local(...)") && strings.Contains(l.Detail, "NOT in scope") {
			foundLocalLeak = true
		}
	}
	if !foundLocalLeak {
		t.Errorf("bin_pack.local(...) should leak that bin_pack is not in scope; leaks=%+v", rep.Leaks)
	}
	// Current-generation autoscaling kwarg spellings (scaledown_window,
	// buffer_containers) and @modal.concurrent's kwargs (max_inputs) on a plain
	// function must route through the SAME dedicated autoscaling leak as the
	// older spellings, not the generic unmodeled-arg fallback (calque#82).
	wantAutoscale := map[string]bool{"scaledown_window": true, "buffer_containers": true, "max_inputs": true}
	for _, l := range rep.Leaks {
		for k := range wantAutoscale {
			if strings.Contains(l.Detail, `"`+k+`"`) && strings.Contains(l.Detail, "behind the seam") {
				delete(wantAutoscale, k)
			}
		}
	}
	if len(wantAutoscale) != 0 {
		t.Errorf("missing dedicated autoscaling leaks for %v; leaks=%+v", wantAutoscale, rep.Leaks)
	}
	// gpu=[...] fallback-list syntax: the first preference becomes GPU, and the
	// try-in-order semantic must be leaked as unreproduced, not hit the generic
	// "not a plain string literal" message (calque#85).
	gf := app.Functions[byName["gpu_fallback"]]
	if gf.GPU != "H100" {
		t.Errorf("gpu_fallback.GPU = %q, want %q (first preference of gpu=[H100, A100-40GB:2])", gf.GPU, "H100")
	}
	foundGPUListLeak := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "fallback-list") && strings.Contains(l.Detail, "H100") {
			foundGPUListLeak = true
		}
	}
	if !foundGPUListLeak {
		t.Errorf("gpu=[...] fallback-list should leak the unreproduced try-in-order semantic; leaks=%+v", rep.Leaks)
	}
	// cloud= (calque#91): recorded (checked above via Config.Cloud) and leaked
	// as unhonored, mirroring region=.
	foundCloudLeak := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "cloud=") && strings.Contains(l.Detail, "NOT honored") {
			foundCloudLeak = true
		}
	}
	if !foundCloudLeak {
		t.Errorf("cloud= should leak that it's recorded but not honored; leaks=%+v", rep.Leaks)
	}
}

// TestParseConcurrentDecoratorMergesIntoClsKwargs (calque#82): @modal.concurrent
// stacked on @app.cls is a SEPARATE decorator, not one of @app.cls's own kwargs
// — its max_inputs/target_inputs must merge into the same leak path as any other
// autoscaling knob, not vanish because visit_ClassDef only read @app.cls's kwargs.
func TestParseConcurrentDecoratorMergesIntoClsKwargs(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/concurrent_class.py")

	if _, err := Parse(context.Background(), script, rep, r, args...); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wantAutoscale := map[string]bool{"max_inputs": true, "target_inputs": true}
	for _, l := range rep.Leaks {
		for k := range wantAutoscale {
			if strings.Contains(l.Detail, `"`+k+`"`) && strings.Contains(l.Detail, "behind the seam") {
				delete(wantAutoscale, k)
			}
		}
	}
	if len(wantAutoscale) != 0 {
		t.Errorf("missing dedicated autoscaling leaks for %v (Batcher's @modal.concurrent kwargs); leaks=%+v", wantAutoscale, rep.Leaks)
	}
}

// TestParseFactoryBuiltImageIsFlagged (calque#76): a function whose image=<var>
// references an Image built by a factory function (never resolved to a chain by
// the AST walker) must get a loud leak naming it — never silently inherit
// whatever OTHER image happened to resolve, with no signal anything went wrong.
func TestParseFactoryBuiltImageIsFlagged(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/factory_image.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// render_image resolves directly, so it's the one app.Image should carry.
	if app.Image.Base != "debian_slim" {
		t.Errorf("app.Image.Base = %q, want debian_slim (render_image)", app.Image.Base)
	}
	foundGPUImageLeak := false
	foundRenderImageLeak := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "gpu_work") && strings.Contains(l.Detail, "did not resolve") {
			foundGPUImageLeak = true
		}
		if strings.Contains(l.Detail, "render:") && strings.Contains(l.Detail, "did not resolve") {
			foundRenderImageLeak = true
		}
	}
	if !foundGPUImageLeak {
		t.Errorf("gpu_work's factory-built image=_gpu_image should leak as unresolved; leaks=%+v", rep.Leaks)
	}
	if foundRenderImageLeak {
		t.Errorf("render's directly-resolved image=render_image must NOT leak; leaks=%+v", rep.Leaks)
	}
}

// TestParseServeEntryKind (F1): serve decorators are detected and carried onto the
// IR as EntryServe, so the run path can gate/leak them instead of crashing.
func TestParseServeEntryKind(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/web_serve.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	serve := 0
	for _, f := range app.Functions {
		if f.EntryKind == ir.EntryServe {
			serve++
		}
	}
	if serve != 2 {
		t.Errorf("serve entry functions = %d, want 2 (@web_endpoint + @asgi_app); funcs=%+v", serve, app.Functions)
	}
}

// TestParseVolumeCommitLeaksWriteback (E3): volume.commit()/reload() on a known
// Volume are detected and leaked (end-of-run write-back / mid-run reload gap).
func TestParseVolumeCommitLeaksWriteback(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/volume_commit.py")

	if _, err := Parse(context.Background(), script, rep, r, args...); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var sawCommit, sawReload bool
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, ".commit()") {
			sawCommit = true
		}
		if strings.Contains(l.Detail, ".reload()") {
			sawReload = true
		}
	}
	if !sawCommit || !sawReload {
		t.Errorf("volume write-back not leaked (commit=%v reload=%v); leaks=%+v", sawCommit, sawReload, rep.Leaks)
	}
}

// TestParseExitHookExcludedFromMethods (calque#86): @modal.exit() is the
// documented pair to @enter — it must NOT be mistaken for a per-item @method.
// Before the fix, it fell into the same untagged bucket as a plain method and
// could be picked as the warm unit's sole callable, running teardown on every
// item instead of once at shutdown.
func TestParseExitHookExcludedFromMethods(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/exit_hook.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(app.Classes) != 1 {
		t.Fatalf("classes = %d, want 1", len(app.Classes))
	}
	cls := app.Classes[0]
	if !cls.HasExit {
		t.Error("HasExit = false, want true (Batcher has @modal.exit())")
	}
	for _, m := range cls.Methods {
		if m.Name == "cleanup" {
			t.Errorf("cleanup (@modal.exit()) must be excluded from Methods, got %+v", cls.Methods)
		}
	}
	if len(cls.Methods) != 1 || cls.Methods[0].Name != "generate" {
		t.Errorf("Methods = %+v, want exactly [generate]", cls.Methods)
	}
	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "@modal.exit()") && strings.Contains(l.Detail, "not reproduced") {
			found = true
		}
	}
	if !found {
		t.Errorf("@modal.exit() should leak that teardown isn't reproduced; leaks=%+v", rep.Leaks)
	}
}

// TestParseCrossAppFromNameLeaksNotVolumeOrSecret (calque#87):
// Function.from_name/Cls.from_name (cross-app invocation) must be recognized
// and leaked distinctly — but Volume.from_name/Secret.from_name (unrelated,
// already-handled constructs sharing the same method name) must NOT be
// misclassified as cross-app invocation.
func TestParseCrossAppFromNameLeaksNotVolumeOrSecret(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/cross_app.py")

	if _, err := Parse(context.Background(), script, rep, r, args...); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var sawFunctionFromName, sawClsFromName, sawVolumeOrSecretMisfire bool
	for _, l := range rep.Leaks {
		switch {
		case strings.Contains(l.Detail, `Function.from_name("other-app", "remote_worker")`):
			sawFunctionFromName = true
		case strings.Contains(l.Detail, `Cls.from_name("other-app", "RemoteBatcher")`):
			sawClsFromName = true
		case strings.Contains(l.Detail, "weights-cache") || strings.Contains(l.Detail, "api-key"):
			sawVolumeOrSecretMisfire = true
		case strings.Contains(l.Detail, "cross-app invocation") && !strings.Contains(l.Detail, "other-app"):
			sawVolumeOrSecretMisfire = true
		}
	}
	if !sawFunctionFromName {
		t.Errorf("Function.from_name should leak cross-app invocation; leaks=%+v", rep.Leaks)
	}
	if !sawClsFromName {
		t.Errorf("Cls.from_name should leak cross-app invocation; leaks=%+v", rep.Leaks)
	}
	if sawVolumeOrSecretMisfire {
		t.Errorf("Volume.from_name/Secret.from_name must NOT be misclassified as cross-app invocation; leaks=%+v", rep.Leaks)
	}
}

// TestParseSpawnClassifiedAndFindable (calque#88): .spawn()'d callables get
// ir.InvokeSpawn and are findable via ir.App.FindFunction — classified so a
// future fan-out driver (calque#97) can locate them, but NOT executed
// (caller, the .map()'d function, must still win warm-unit selection over
// the merely-spawned workers per rank precedence).
func TestParseSpawnClassifiedAndFindable(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/spawn_fanout.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, name := range []string{"worker_a", "worker_b"} {
		fn, ok := app.FindFunction(name)
		if !ok {
			t.Fatalf("FindFunction(%q) = false, want true", name)
		}
		if fn.Invoke != ir.InvokeSpawn {
			t.Errorf("%s.Invoke = %q, want %q", name, fn.Invoke, ir.InvokeSpawn)
		}
	}
	caller, ok := app.FindFunction("caller")
	if !ok {
		t.Fatal(`FindFunction("caller") = false, want true`)
	}
	if caller.Invoke != ir.InvokeMap {
		t.Errorf("caller.Invoke = %q, want %q (InvokeMap must beat InvokeSpawn in rank)", caller.Invoke, ir.InvokeMap)
	}
	if _, ok := app.FindFunction("does_not_exist"); ok {
		t.Error(`FindFunction("does_not_exist") = true, want false`)
	}
	sawA, sawB := false, false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "worker_a.spawn(...)") && strings.Contains(l.Detail, "classified but not executed") {
			sawA = true
		}
		if strings.Contains(l.Detail, "worker_b.spawn(...)") && strings.Contains(l.Detail, "classified but not executed") {
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Errorf("both spawn call sites should leak 'classified but not executed'; leaks=%+v", rep.Leaks)
	}
}

// TestSpawnCallSitesCapturesStringAndNumericArgs (calque#112): SpawnCallSites
// exposes .spawn()'s best-effort call-site args, extracted via pyast.py's
// _spawn_arg_str. Discovered via LIVE real-AWS spawn-run verification: a
// numeric literal (worker.spawn(5)) silently captured NO arg at all under
// the original _const_str-based extraction (string-only by design, correct
// for from_name's needs but wrong for .spawn()'s actual per-call payload) —
// indistinguishable from a variable reference like worker.spawn(x). This
// fixture's literal_caller exercises the fix; caller's variable-arg spawns
// (worker_a.spawn(x)/worker_b.spawn(x)) must still correctly capture NO arg
// (a genuine "can't resolve a variable" case, not a bug).
func TestSpawnCallSitesCapturesStringAndNumericArgs(t *testing.T) {
	r, args := runner(t)
	script, _ := filepath.Abs("../../testdata/scripts/spawn_fanout.py")

	sites, err := SpawnCallSites(context.Background(), script, r, args...)
	if err != nil {
		t.Fatalf("SpawnCallSites: %v", err)
	}

	var sawNumericArg, sawVariableArgsAsNil bool
	varArgNilCount := 0
	for _, s := range sites {
		if s.Target == "worker_a" && len(s.Args) == 1 {
			if s.Args[0] != nil && *s.Args[0] == "5" {
				sawNumericArg = true
			}
			if s.Args[0] == nil {
				varArgNilCount++
			}
		}
	}
	// worker_a is spawned twice: once with a numeric literal (5, from
	// literal_caller) and once with a variable (x, from caller) — both call
	// sites appear, so we can't just count "worker_a" entries; distinguish
	// by whether the arg resolved.
	sawVariableArgsAsNil = varArgNilCount >= 1
	if !sawNumericArg {
		t.Errorf("no worker_a call site captured the numeric literal \"5\"; sites=%+v", sites)
	}
	if !sawVariableArgsAsNil {
		t.Errorf("expected at least one worker_a call site with a nil arg (the variable-arg spawn from caller); sites=%+v", sites)
	}
}

// TestInvocationKindsPartitionsByEntrypoint (calque#98): a unit test on
// invocationKinds itself (no pyast subprocess, so it always runs even
// without uv on PATH) — proves the returned per-entrypoint map correctly
// filters call-site evidence by entrypoint while leaving the pre-existing
// whole-script map exactly as it was (the regression guard for 0/1-
// entrypoint scripts, which never consult the per-entrypoint map at all).
func TestInvocationKindsPartitionsByEntrypoint(t *testing.T) {
	out := pyOut{
		InvokeCalls: []pyInvokeCall{
			{Target: "train_step", Kind: "map", Entrypoint: "do_train"},
			{Target: "evaluate", Kind: "remote", Entrypoint: "do_evaluate"},
			{Target: "helper", Kind: "remote", Entrypoint: ""}, // module-level / no entrypoint
		},
	}
	rep := &leak.Report{}
	whole, byEP := invocationKinds(out, "s.py", rep)

	// Whole-script view: every call site counts, regardless of entrypoint.
	if whole["train_step"] != ir.InvokeMap {
		t.Errorf(`whole["train_step"] = %q, want %q`, whole["train_step"], ir.InvokeMap)
	}
	if whole["evaluate"] != ir.InvokeRemote {
		t.Errorf(`whole["evaluate"] = %q, want %q`, whole["evaluate"], ir.InvokeRemote)
	}
	if whole["helper"] != ir.InvokeRemote {
		t.Errorf(`whole["helper"] = %q, want %q`, whole["helper"], ir.InvokeRemote)
	}

	// Per-entrypoint view: each entrypoint sees ONLY its own call sites.
	if byEP["do_train"]["train_step"] != ir.InvokeMap {
		t.Errorf(`byEP["do_train"]["train_step"] = %q, want %q`, byEP["do_train"]["train_step"], ir.InvokeMap)
	}
	if _, ok := byEP["do_train"]["evaluate"]; ok {
		t.Errorf(`byEP["do_train"] must not contain "evaluate"; got %+v`, byEP["do_train"])
	}
	if byEP["do_evaluate"]["evaluate"] != ir.InvokeRemote {
		t.Errorf(`byEP["do_evaluate"]["evaluate"] = %q, want %q`, byEP["do_evaluate"]["evaluate"], ir.InvokeRemote)
	}
	if _, ok := byEP["do_evaluate"]["train_step"]; ok {
		t.Errorf(`byEP["do_evaluate"] must not contain "train_step"; got %+v`, byEP["do_evaluate"])
	}
	// A call site with no entrypoint ("" — module level) must not create a
	// "" entry in the per-entrypoint map; it only ever contributes to whole.
	if _, ok := byEP[""]; ok {
		t.Errorf(`byEP must not have a "" key for module-level call sites; got %+v`, byEP)
	}
}

// TestParseEntrypointScopedInvokes (calque#98): the fixture's two entrypoints
// each invoke a wholly DIFFERENT callable (do_train -> Trainer.train_step via
// .map(), do_evaluate -> evaluate via .remote()) — app.EntrypointInvokes must
// attribute each call site to the entrypoint whose body it was found in, not
// fold both into one whole-script union. The pre-existing whole-script Invoke
// field on each Function/method (used by 0/1-entrypoint scripts) must also
// still carry both idioms unchanged — this is the regression guard.
func TestParseEntrypointScopedInvokes(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/entrypoint_scoped_invoke.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(app.Entrypoints) != 2 {
		t.Fatalf("Entrypoints = %d, want 2 (do_train, do_evaluate)", len(app.Entrypoints))
	}

	trainEvidence := app.EntrypointInvokes["do_train"]
	if trainEvidence["train_step"] != ir.InvokeMap {
		t.Errorf(`EntrypointInvokes["do_train"]["train_step"] = %q, want %q`, trainEvidence["train_step"], ir.InvokeMap)
	}
	if _, ok := trainEvidence["evaluate"]; ok {
		t.Errorf(`EntrypointInvokes["do_train"] must NOT contain "evaluate"; got %+v`, trainEvidence)
	}

	evalEvidence := app.EntrypointInvokes["do_evaluate"]
	if evalEvidence["evaluate"] != ir.InvokeRemote {
		t.Errorf(`EntrypointInvokes["do_evaluate"]["evaluate"] = %q, want %q`, evalEvidence["evaluate"], ir.InvokeRemote)
	}
	if _, ok := evalEvidence["train_step"]; ok {
		t.Errorf(`EntrypointInvokes["do_evaluate"] must NOT contain "train_step"; got %+v`, evalEvidence)
	}

	// Whole-script fields (what 0/1-entrypoint scripts rely on) must still see
	// BOTH idioms, unaffected by the new per-entrypoint partitioning.
	if len(app.Classes) != 1 || len(app.Classes[0].Methods) != 1 || !app.Classes[0].Methods[0].IsMap {
		t.Errorf("Trainer.train_step.IsMap must still be true (whole-script view); classes=%+v", app.Classes)
	}
	evalFn, ok := app.FindFunction("evaluate")
	if !ok || evalFn.Invoke != ir.InvokeRemote {
		t.Errorf("evaluate.Invoke = %+v (found=%v), want %q (whole-script view)", evalFn, ok, ir.InvokeRemote)
	}
}

// TestLocalCallsPopulatedFromFixture (calque#92): each function/method's own
// LocalCalls carries exactly the sibling names ITS OWN body references via
// .local() — run_blend reaches both a plain function (stage_one) and a @cls
// method (score) directly, and stage_one itself reaches stage_two one hop
// deeper (proving the field is per-function, not a whole-script union).
func TestLocalCallsPopulatedFromFixture(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/local_chain.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	runBlend, ok := app.FindFunction("run_blend")
	if !ok {
		t.Fatal(`FindFunction("run_blend") = false, want true`)
	}
	wantRunBlend := map[string]bool{"stage_one": true, "score": true}
	if len(runBlend.LocalCalls) != len(wantRunBlend) {
		t.Errorf("run_blend.LocalCalls = %v, want %v", runBlend.LocalCalls, wantRunBlend)
	}
	for _, lc := range runBlend.LocalCalls {
		if !wantRunBlend[lc] {
			t.Errorf("run_blend.LocalCalls contains unexpected %q", lc)
		}
	}

	stageOne, ok := app.FindFunction("stage_one")
	if !ok {
		t.Fatal(`FindFunction("stage_one") = false, want true`)
	}
	if len(stageOne.LocalCalls) != 1 || stageOne.LocalCalls[0] != "stage_two" {
		t.Errorf("stage_one.LocalCalls = %v, want [stage_two]", stageOne.LocalCalls)
	}
	if len(stageOne.Args) == 0 || stageOne.Args[0] != "y" {
		t.Errorf("stage_one.Args = %v, want first arg %q", stageOne.Args, "y")
	}

	stageTwo, ok := app.FindFunction("stage_two")
	if !ok {
		t.Fatal(`FindFunction("stage_two") = false, want true`)
	}
	if len(stageTwo.LocalCalls) != 0 {
		t.Errorf("stage_two.LocalCalls = %v, want empty (it calls nothing via .local())", stageTwo.LocalCalls)
	}
}
