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
	if tr.Config.CPU != 4 || tr.Config.MemoryMB != 8192 || tr.Config.Retries != 3 || tr.Config.Region != "us-west-2" {
		t.Errorf("transform.Config = %+v, want cpu4/mem8192/retries3/us-west-2", tr.Config)
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
