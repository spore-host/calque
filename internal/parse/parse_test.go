package parse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// TestParsePerFunctionImageResolution (calque#174) proves each callable
// resolves its OWN image=<var> against the script's known image chains,
// instead of resolveImage's pre-#174 behavior of picking ONE image variable
// for the WHOLE script regardless of who referenced it. special_fn/SpecialCls
// explicitly declare image=special_image and must get numpy (NOT torch, the
// App-level default) even though torch happens to be the lexicographically
// later name (a case resolveImage's old "prefer literal 'image', else
// lexicographically first" pick could have silently gotten wrong). plain_fn/
// PlainCls declare no image= of their own and must inherit the App-level
// default (calque#168's mechanism, extended to image= by #174) — including
// through the class -> method chain for PlainCls's own method.
func TestParsePerFunctionImageResolution(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/per_function_image.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	specialFn, ok := app.FindFunction("special_fn")
	if !ok {
		t.Fatal("special_fn not found")
	}
	if got := specialFn.Image.Pip; len(got) != 1 || got[0] != "numpy" {
		t.Errorf("special_fn.Image.Pip = %v, want [numpy] (its OWN image=special_image)", got)
	}

	plainFn, ok := app.FindFunction("plain_fn")
	if !ok {
		t.Fatal("plain_fn not found")
	}
	if got := plainFn.Image.Pip; len(got) != 1 || got[0] != "torch" {
		t.Errorf("plain_fn.Image.Pip = %v, want [torch] (inherited App-level default)", got)
	}

	var specialCls, plainCls *ir.Class
	for i := range app.Classes {
		switch app.Classes[i].Name {
		case "SpecialCls":
			specialCls = &app.Classes[i]
		case "PlainCls":
			plainCls = &app.Classes[i]
		}
	}
	if specialCls == nil || plainCls == nil {
		t.Fatalf("classes = %+v, want SpecialCls and PlainCls", app.Classes)
	}
	if got := specialCls.Image.Pip; len(got) != 1 || got[0] != "numpy" {
		t.Errorf("SpecialCls.Image.Pip = %v, want [numpy] (its OWN image=special_image)", got)
	}
	if got := plainCls.Image.Pip; len(got) != 1 || got[0] != "torch" {
		t.Errorf("PlainCls.Image.Pip = %v, want [torch] (inherited App-level default)", got)
	}
	if len(plainCls.Methods) != 1 {
		t.Fatalf("PlainCls.Methods = %+v, want exactly one (run)", plainCls.Methods)
	}
	if got := plainCls.Methods[0].Image.Pip; len(got) != 1 || got[0] != "torch" {
		t.Errorf("PlainCls.run.Image.Pip = %v, want [torch] (App -> class -> method chain)", got)
	}
}

// TestParseAppLevelDefaultsInherited (calque#168) proves App(volumes=...,
// secrets=...) actually reaches a Function/Class declaring neither — before
// this fix, both were silently dropped with NO leak at all. Also proves a
// callable with its OWN volumes= is NOT overwritten by the App-level
// default, and that class-level inheritance chains correctly down to the
// class's own method (App -> class -> method, extending the pre-existing
// class -> method fallback one level up).
func TestParseAppLevelDefaultsInherited(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/app_level_defaults.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(app.DefaultVolumes) != 1 || app.DefaultVolumes["/weights"] != "weights" {
		t.Errorf("app.DefaultVolumes = %+v, want {/weights: weights}", app.DefaultVolumes)
	}
	if len(app.DefaultSecrets) == 0 {
		t.Errorf("app.DefaultSecrets = %+v, want non-empty", app.DefaultSecrets)
	}

	plainFn, ok := app.FindFunction("plain_fn")
	if !ok {
		t.Fatal("plain_fn not found")
	}
	if plainFn.Volumes["/weights"] != "weights" {
		t.Errorf("plain_fn.Volumes = %+v, want to inherit {/weights: weights} from the App", plainFn.Volumes)
	}
	if len(plainFn.Config.Secrets) == 0 {
		t.Errorf("plain_fn.Config.Secrets = %+v, want to inherit the App's secrets", plainFn.Config.Secrets)
	}

	overriddenFn, ok := app.FindFunction("overridden_fn")
	if !ok {
		t.Fatal("overridden_fn not found")
	}
	if _, has := overriddenFn.Volumes["/weights"]; has {
		t.Errorf("overridden_fn.Volumes = %+v, must NOT pick up the App default when it declares its own", overriddenFn.Volumes)
	}
	if overriddenFn.Volumes["/own"] == "" {
		t.Errorf("overridden_fn.Volumes = %+v, want its own {/own: own-cache} preserved", overriddenFn.Volumes)
	}

	if len(app.Classes) != 1 {
		t.Fatalf("classes = %+v, want exactly one (Scorer)", app.Classes)
	}
	scorer := app.Classes[0]
	if scorer.Volumes["/weights"] != "weights" {
		t.Errorf("Scorer.Volumes = %+v, want to inherit {/weights: weights} from the App", scorer.Volumes)
	}
	if len(scorer.Methods) != 1 || scorer.Methods[0].Name != "score" {
		t.Fatalf("Scorer.Methods = %+v, want exactly one (score)", scorer.Methods)
	}
	if scorer.Methods[0].Volumes["/weights"] != "weights" {
		t.Errorf("score.Volumes = %+v, want to inherit {/weights: weights} via Scorer (App -> class -> method)", scorer.Methods[0].Volumes)
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

// TestParseTrivialFactoryImageResolves (calque#175, extending calque#76):
// a factory function with no control flow and a single unconditional
// return — the real shape AI-Almanac's blending_app.py uses — must resolve
// its image chain directly, with ZERO leak. Contrast with
// TestParseFactoryBuiltImageIsFlagged's gpu_work, whose factory branches
// and must still leak.
func TestParseTrivialFactoryImageResolves(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/factory_image_trivial.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	worker, ok := app.FindFunction("worker")
	if !ok {
		t.Fatal("worker not found")
	}
	if worker.Image.Base != "debian_slim" {
		t.Errorf("worker.Image.Base = %q, want debian_slim (inlined from the _image() factory)", worker.Image.Base)
	}
	if len(worker.Image.Pip) == 0 || worker.Image.Pip[0] != "uv" {
		t.Errorf("worker.Image.Pip = %v, want [uv google-cloud-storage] (inlined chain's real pip_install steps)", worker.Image.Pip)
	}
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "worker") && strings.Contains(l.Detail, "did not resolve") {
			t.Errorf("worker's trivial factory-built image must resolve with NO leak; got: %s", l.Detail)
		}
	}
}

// TestParseForLoopExpandedImagesResolve (calque#179) proves a module-level
// `for k, v in D.items(): @app.function(...) def f(...): ...` loop —
// mirroring AI-Almanac's forecasts_app.py's real per-env
// run_forecast_inference/warm_model_weights pairing — expands into ONE
// ir.Function per (registered name, resolved image) pair, not a single
// mis-resolved entry. Asserts BOTH statements per loop iteration
// (do_inference_*/do_warm_*) resolve, each with the correct per-env f-string
// substitution folded into its image's run_commands step, and that gpu=
// (an unrelated literal, unaffected by this whole change) still flows
// through per-function.
func TestParseForLoopExpandedImagesResolve(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/factory_image_loop.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cases := []struct {
		fnName     string
		wantRunCmd string
	}{
		{"do_inference_alpha", "uv pip install 'examplepkg[a1,a2]'"},
		{"do_warm_alpha", "uv pip install 'examplepkg[a1,a2]'"},
		{"do_inference_beta", "uv pip install 'examplepkg[b1]'"},
		{"do_warm_beta", "uv pip install 'examplepkg[b1]'"},
	}
	for _, c := range cases {
		fn, ok := app.FindFunction(c.fnName)
		if !ok {
			t.Errorf("function %q not found", c.fnName)
			continue
		}
		if fn.GPU != "A100-80GB" {
			t.Errorf("%s.GPU = %q, want %q (unrelated literal, must pass through unaffected)", c.fnName, fn.GPU, "A100-80GB")
		}
		if fn.Image.Base != "debian_slim" {
			t.Errorf("%s.Image.Base = %q, want debian_slim", c.fnName, fn.Image.Base)
		}
		found := false
		for _, s := range fn.Image.Steps {
			if s.Method == "run_commands" {
				for _, a := range s.Args {
					if a == c.wantRunCmd {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("%s's image run_commands should contain %q (per-env f-string substitution); steps=%+v", c.fnName, c.wantRunCmd, fn.Image.Steps)
		}
	}
	// do_inference_alpha and do_inference_beta must NOT share the same image
	// (each env's f-string-substituted spec differs) — a regression here
	// would mean the loop expansion picked one image for every iteration.
	alpha, _ := app.FindFunction("do_inference_alpha")
	beta, _ := app.FindFunction("do_inference_beta")
	if len(alpha.Image.Steps) > 0 && len(beta.Image.Steps) > 0 {
		lastAlpha := alpha.Image.Steps[len(alpha.Image.Steps)-1]
		lastBeta := beta.Image.Steps[len(beta.Image.Steps)-1]
		if len(lastAlpha.Args) > 0 && len(lastBeta.Args) > 0 && lastAlpha.Args[0] == lastBeta.Args[0] {
			t.Errorf("alpha and beta resolved to the SAME image content — per-iteration substitution did not happen")
		}
	}
	for _, l := range rep.Leaks {
		for _, name := range []string{"do_inference_alpha", "do_warm_alpha", "do_inference_beta", "do_warm_beta"} {
			if strings.Contains(l.Detail, name) && strings.Contains(l.Detail, "did not resolve") {
				t.Errorf("%s's for-loop-expanded image must resolve with NO leak; got: %s", name, l.Detail)
			}
		}
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

// TestParseDunderLifecycleRecognizedAndExcludedFromMethods (calque#138):
// Modal's pre-1.0 class lifecycle API — bare __enter__/__exit__ dunders, no
// decorator at all — must be recognized exactly like @modal.enter()/
// @modal.exit() are, and excluded from Methods for the identical calque#86
// reason: a load-once/teardown hook must never be eligible for pickWarmUnit's
// per-item "fall back to first method" heuristic.
func TestParseDunderLifecycleRecognizedAndExcludedFromMethods(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/dunder_lifecycle.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(app.Classes) != 1 {
		t.Fatalf("classes = %d, want 1", len(app.Classes))
	}
	cls := app.Classes[0]
	if cls.EnterBody == "" {
		t.Error("EnterBody = \"\", want non-empty (Batcher has bare __enter__)")
	}
	if !cls.HasExit {
		t.Error("HasExit = false, want true (Batcher has bare __exit__)")
	}
	for _, m := range cls.Methods {
		if m.Name == "__enter__" || m.Name == "__exit__" {
			t.Errorf("%s must be excluded from Methods, got %+v", m.Name, cls.Methods)
		}
	}
	if len(cls.Methods) != 1 || cls.Methods[0].Name != "generate" {
		t.Errorf("Methods = %+v, want exactly [generate]", cls.Methods)
	}
	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "legacy __exit__") && strings.Contains(l.Detail, "not reproduced") {
			found = true
		}
	}
	if !found {
		t.Errorf("legacy __exit__ should leak that teardown isn't reproduced, distinctly from @modal.exit(); leaks=%+v", rep.Leaks)
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

// TestFreeRefsPopulatedFromFixture (calque#139): each function/method's own
// FreeRefs carries the bare (non-.local()-suffixed) module-level names ITS
// OWN body references — `load`'s @enter body bare-reads GREETING, `greet`'s
// body bare-calls the plain (never-decorated) helper _format, and `stray`'s
// body must NOT capture GREETING at all (its own comprehension shadows the
// module constant with a same-named loop variable — proving this is real
// scope tracking, not a naive name match). App.ModuleConsts/ModuleFuncs must
// also carry the resolvable shapes.
func TestFreeRefsPopulatedFromFixture(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/free_refs.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(app.Classes) != 1 {
		t.Fatalf("app.Classes = %d, want 1", len(app.Classes))
	}
	cls := app.Classes[0]
	if len(cls.EnterFreeRefs) != 1 || cls.EnterFreeRefs[0] != "GREETING" {
		t.Errorf("cls.EnterFreeRefs = %v, want [GREETING]", cls.EnterFreeRefs)
	}

	var greet, stray ir.Function
	for _, m := range cls.Methods {
		switch m.Name {
		case "greet":
			greet = m
		case "stray":
			stray = m
		}
	}
	if len(greet.FreeRefs) != 1 || greet.FreeRefs[0] != "_format" {
		t.Errorf("greet.FreeRefs = %v, want [_format]", greet.FreeRefs)
	}
	if len(stray.FreeRefs) != 0 {
		t.Errorf("stray.FreeRefs = %v, want empty (GREETING is shadowed by its own comprehension loop var)", stray.FreeRefs)
	}

	if mc, ok := app.ModuleConsts["GREETING"]; !ok || !strings.Contains(mc.Source, `"hello"`) {
		t.Errorf(`app.ModuleConsts["GREETING"] = %+v, ok=%v, want a source line containing "hello"`, mc, ok)
	}
	mf, ok := app.ModuleFuncs["_format"]
	if !ok {
		t.Fatal(`app.ModuleFuncs["_format"] missing — want the plain, undecorated helper captured`)
	}
	if len(mf.Args) != 1 || mf.Args[0] != "name" {
		t.Errorf("ModuleFuncs[_format].Args = %v, want [name]", mf.Args)
	}
	if len(mf.FreeRefs) != 1 || mf.FreeRefs[0] != "GREETING" {
		t.Errorf("ModuleFuncs[_format].FreeRefs = %v, want [GREETING] (the helper itself reads the module constant)", mf.FreeRefs)
	}
}

// TestFreeRefsResolveModuleImports (calque#146) is TestFreeRefsPopulatedFromFixture's
// import counterpart: a bare reference to a module-level `from X import Y`
// name must resolve via App.ModuleImports — the THIRD free-reference target,
// closing the gap calque#139's own fix explicitly left open ("deliberately
// does NOT attempt to resolve imports").
func TestFreeRefsResolveModuleImports(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/free_refs_import.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(app.Classes) != 1 {
		t.Fatalf("app.Classes = %d, want 1", len(app.Classes))
	}
	cls := app.Classes[0]
	if len(cls.EnterFreeRefs) != 1 || cls.EnterFreeRefs[0] != "Path" {
		t.Errorf("cls.EnterFreeRefs = %v, want [Path]", cls.EnterFreeRefs)
	}
	src, ok := app.ModuleImports["Path"]
	if !ok {
		t.Fatal(`app.ModuleImports["Path"] missing — want the "from pathlib import Path" statement captured`)
	}
	if !strings.Contains(src, "pathlib") || !strings.Contains(src, "Path") {
		t.Errorf(`app.ModuleImports["Path"] = %q, want it to contain "from pathlib import Path"`, src)
	}
	// app.ModuleImports is the lookup TABLE of every module-level import
	// (mirroring ModuleConsts/ModuleFuncs) — it's collectLocalExtras
	// (cmd/calque) that decides what's actually SHIPPED, based on FreeRefs.
	// "modal" is imported too (never bare-referenced from inside a body in
	// this fixture — only used via the @app.cls/@modal.enter decorators,
	// which resolve structurally, not via FreeRefs) and IS present here,
	// proving this table isn't filtered to "already known to be referenced"
	// — the filtering happens downstream, exactly like ModuleConsts/
	// ModuleFuncs.
	if _, ok := app.ModuleImports["modal"]; !ok {
		t.Error(`app.ModuleImports["modal"] missing, want present — ModuleImports captures every module-level import, unfiltered`)
	}
}

// TestFreeRefsResolveModuleClasses (calque#147) is TestFreeRefsResolveModuleImports'
// class counterpart: a bare instantiation of a PLAIN (non-@app.cls)
// module-level class must resolve via App.ModuleClasses — the FOURTH
// free-reference target, closing the gap left open after calque#139/#146
// (functions/constants/imports).
func TestFreeRefsResolveModuleClasses(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/free_refs_class.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(app.Classes) != 1 {
		t.Fatalf("app.Classes = %d, want 1 (only Worker, the @app.cls — _Adder must NOT be double-collected there)", len(app.Classes))
	}
	cls := app.Classes[0]
	if cls.Name != "Worker" {
		t.Errorf("app.Classes[0].Name = %q, want Worker", cls.Name)
	}
	if len(cls.EnterFreeRefs) != 1 || cls.EnterFreeRefs[0] != "_Adder" {
		t.Errorf("cls.EnterFreeRefs = %v, want [_Adder]", cls.EnterFreeRefs)
	}
	mc, ok := app.ModuleClasses["_Adder"]
	if !ok {
		t.Fatal(`app.ModuleClasses["_Adder"] missing — want the plain helper class captured`)
	}
	if !strings.Contains(mc.Source, "def add(self, x)") {
		t.Errorf(`app.ModuleClasses["_Adder"].Source = %q, want it to contain the add method`, mc.Source)
	}
	// Worker (the @app.cls) must NOT also appear in ModuleClasses — only
	// PLAIN classes are collected there, proving _is_app_cls's exclusion
	// works, not just that _Adder happens to be found.
	if _, ok := app.ModuleClasses["Worker"]; ok {
		t.Error(`app.ModuleClasses["Worker"] present, want absent — Worker is @app.cls, already modeled structurally in app.Classes`)
	}
}

// TestParseScheduleObjectForms (calque#91): schedule=modal.Cron(...)/modal.Period(...)
// object forms must be recognized structurally (via pyast's __schedule__ marker),
// not fall into the generic __unparsed__ mangling that used to leave
// ir.Config.Schedule holding stringified JSON garbage. Also guards the pre-existing
// bare-string schedule= case against regression.
func TestParseScheduleObjectForms(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/schedule_object.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cronJob, ok := app.FindFunction("cron_job")
	if !ok {
		t.Fatal(`FindFunction("cron_job") = false, want true`)
	}
	if cronJob.Config.Schedule != "0 * * * *" {
		t.Errorf("cron_job.Config.Schedule = %q, want %q", cronJob.Config.Schedule, "0 * * * *")
	}

	periodJob, ok := app.FindFunction("period_job")
	if !ok {
		t.Fatal(`FindFunction("period_job") = false, want true`)
	}
	if periodJob.Config.Schedule != "1d6h" {
		t.Errorf("period_job.Config.Schedule = %q, want %q", periodJob.Config.Schedule, "1d6h")
	}

	// Regression guard: the pre-existing bare-string schedule= case must still work.
	bareJob, ok := app.FindFunction("bare_string_job")
	if !ok {
		t.Fatal(`FindFunction("bare_string_job") = false, want true`)
	}
	if bareJob.Config.Schedule != "0 0 * * *" {
		t.Errorf("bare_string_job.Config.Schedule = %q, want %q", bareJob.Config.Schedule, "0 0 * * *")
	}

	// All three must fire the dedicated schedule= leak (semantic gap: recorded but
	// not honored) — the object-form cases must say so explicitly, and NONE of the
	// three may instead fire the generic "unmodeled decorator arg" fallback leak.
	wantScheduleLeak := map[string]bool{"cron_job": true, "period_job": true, "bare_string_job": true}
	wantObjectFormNote := map[string]bool{"cron_job": true, "period_job": true}
	for _, l := range rep.Leaks {
		for name := range wantScheduleLeak {
			if strings.Contains(l.Detail, name+":") && strings.Contains(l.Detail, "schedule=") && strings.Contains(l.Detail, "recorded but NOT honored") {
				delete(wantScheduleLeak, name)
				if strings.Contains(l.Detail, "recognized from modal.Cron/Period object form") {
					delete(wantObjectFormNote, name)
				}
			}
		}
		if strings.Contains(l.Detail, "unmodeled decorator arg") && strings.Contains(l.Detail, "schedule") {
			t.Errorf("schedule= must not hit the generic unmodeled-decorator-arg fallback; leak=%+v", l)
		}
	}
	if len(wantScheduleLeak) != 0 {
		t.Errorf("missing dedicated schedule= leak for %v; leaks=%+v", wantScheduleLeak, rep.Leaks)
	}
	if len(wantObjectFormNote) != 0 {
		t.Errorf("object-form schedule= leaks must note recognition for %v; leaks=%+v", wantObjectFormNote, rep.Leaks)
	}
}

// TestParseRareConstructsAreTaggedNotSilent (calque#91): modal.Dict/Queue.
// from_name(...) and App.include(...) must each fire a DISTINCT, named leak
// — before this fix, all four (Dict/Queue/NetworkFileSystem/App.include)
// vanished entirely (no visit_Assign branch matched them). None of Dict/
// Queue/App.include are modeled; this only proves each is now a clean grep
// hit instead of silence.
//
// modal.CloudBucketMount is NOT asserted here anymore (calque#91 Workstream
// A): it moved from "recognized but not modeled" to a REAL, resolved S3
// mount — rare_constructs.py's own CloudBucketMount("my-bucket", secret=None)
// usage has a literal bucket_name and an explicit secret=None (a no-op, not a
// real secret= request), so it now resolves cleanly with ZERO leak at all,
// the same as an ordinary Volume mount. See
// TestParseCloudBucketMountResolves for the positive (modeled) case, using
// testdata/scripts/cloud_bucket_mount.py instead.
//
// modal.NetworkFileSystem is likewise NOT asserted here anymore (calque#91
// Workstream B): its from_name(...) binding is now structurally tracked
// (mirroring Volume's own zero-leak-on-just-the-binding posture), so
// rare_constructs.py's unused shared_fs var emits no leak at all. See
// TestParseNetworkFileSystemResolves for the positive (real EFS mount) case,
// using testdata/scripts/network_file_system.py instead.
func TestParseRareConstructsAreTaggedNotSilent(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/rare_constructs.py")

	if _, err := Parse(context.Background(), script, rep, r, args...); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	wantTag := map[string]bool{
		"modal.Dict":  true,
		"modal.Queue": true,
		"App.include": true,
	}
	for _, l := range rep.Leaks {
		for tag := range wantTag {
			if strings.Contains(l.Detail, `flagged "`+tag+`"`) {
				delete(wantTag, tag)
			}
		}
		if strings.Contains(l.Detail, `flagged "modal.NetworkFileSystem"`) {
			t.Errorf("modal.NetworkFileSystem.from_name(...) binding alone must not leak anymore (calque#91 Workstream B: structurally tracked, same posture as Volume) — got: %+v", l)
		}
	}
	if len(wantTag) != 0 {
		t.Errorf("missing distinct leak tag for %v; leaks=%+v", wantTag, rep.Leaks)
	}
}

// TestParseCloudBucketMountResolves (calque#91 Workstream A) proves the
// POSITIVE case: a real modal.CloudBucketMount(...) used inline as a
// volumes= value, with a literal bucket_name/key_prefix/read_only, resolves
// to ir.Function.CloudBucketMounts — a real S3 mount via mountpoint-s3 — not
// a leak. testdata/scripts/cloud_bucket_mount.py is the fixture.
func TestParseCloudBucketMountResolves(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/cloud_bucket_mount.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	fn, ok := app.FindFunction("use_bucket_mount")
	if !ok {
		t.Fatalf("function %q not found in parsed app", "use_bucket_mount")
	}
	mount, ok := fn.CloudBucketMounts["/data"]
	if !ok {
		t.Fatalf("CloudBucketMounts[%q] missing; got %+v", "/data", fn.CloudBucketMounts)
	}
	if mount.BucketName != "my-real-bucket" {
		t.Errorf("BucketName = %q, want %q", mount.BucketName, "my-real-bucket")
	}
	if mount.KeyPrefix != "foo/" {
		t.Errorf("KeyPrefix = %q, want %q", mount.KeyPrefix, "foo/")
	}
	if !mount.ReadOnly {
		t.Error("ReadOnly = false, want true")
	}
	if fn.Volumes != nil {
		t.Errorf("Volumes = %+v, want nil (this mount is a CloudBucketMount, not an ordinary Volume)", fn.Volumes)
	}
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "CloudBucketMount") {
			t.Errorf("unexpected CloudBucketMount leak for a fully-resolved literal mount: %+v", l)
		}
	}
}

// TestParseNetworkFileSystemResolves (calque#91 Workstream B) proves the
// POSITIVE case: a real modal.NetworkFileSystem.from_name(...) used as a
// network_file_systems= value resolves to ir.Function.NetworkFileSystems —
// a real (bring-your-own) EFS mount — not a leak.
// testdata/scripts/network_file_system.py is the fixture.
func TestParseNetworkFileSystemResolves(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/network_file_system.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	fn, ok := app.FindFunction("use_shared_fs")
	if !ok {
		t.Fatalf("function %q not found in parsed app", "use_shared_fs")
	}
	mount, ok := fn.NetworkFileSystems["/shared"]
	if !ok {
		t.Fatalf("NetworkFileSystems[%q] missing; got %+v", "/shared", fn.NetworkFileSystems)
	}
	if mount.Name != "shared-fs" {
		t.Errorf("Name = %q, want %q", mount.Name, "shared-fs")
	}
	if fn.Volumes != nil {
		t.Errorf("Volumes = %+v, want nil (this mount is a NetworkFileSystem, not an ordinary Volume)", fn.Volumes)
	}
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "NetworkFileSystem") || strings.Contains(l.Detail, "network_file_systems") {
			t.Errorf("unexpected NetworkFileSystem leak for a fully-resolved mount: %+v", l)
		}
	}
}

// TestParseNetworkFileSystemClassAndMethodInherit (calque#91 Workstream B)
// proves a @cls's own network_file_systems= is inherited by a @method
// declaring none of its own — mirrors the existing CloudBucketMounts/Volumes
// class->method inheritance in buildClass.
func TestParseNetworkFileSystemClassAndMethodInherit(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	dir := t.TempDir()
	script := dir + "/nfs_class.py"
	src := `
import modal

app = modal.App("nfs-class")

shared_fs = modal.NetworkFileSystem.from_name("shared-fs")


@app.cls(network_file_systems={"/shared": shared_fs})
class Worker:
    @modal.method()
    def run(self, x):
        return x
`
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(app.Classes) != 1 {
		t.Fatalf("classes = %d, want 1", len(app.Classes))
	}
	cls := app.Classes[0]
	if cls.NetworkFileSystems["/shared"].Name != "shared-fs" {
		t.Errorf("class NetworkFileSystems = %+v, want /shared->shared-fs", cls.NetworkFileSystems)
	}
	if len(cls.Methods) != 1 {
		t.Fatalf("methods = %d, want 1", len(cls.Methods))
	}
	if cls.Methods[0].NetworkFileSystems["/shared"].Name != "shared-fs" {
		t.Errorf("method should inherit class's NetworkFileSystems, got %+v", cls.Methods[0].NetworkFileSystems)
	}
}

// TestParseNetworkFileSystemCreateIfMissingLeaks (calque#91 Workstream B)
// proves create_if_missing=True leaks distinctly (calque never auto-creates
// an EFS filesystem — bring-your-own only), while the mount itself still
// resolves normally (the leak is purely additive, mirroring
// TestParseModalBatchedDecoratorLeaks' "leak is additive, not a blocker"
// shape).
func TestParseNetworkFileSystemCreateIfMissingLeaks(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/network_file_system_create_if_missing.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	fn, ok := app.FindFunction("use_shared_fs")
	if !ok {
		t.Fatalf("function %q not found in parsed app", "use_shared_fs")
	}
	if fn.NetworkFileSystems["/shared"].Name != "shared-fs" {
		t.Errorf("mount should still resolve despite create_if_missing=True, got %+v", fn.NetworkFileSystems)
	}

	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "create_if_missing=True") && strings.Contains(l.Detail, "bring-your-own EFS") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a create_if_missing=True leak naming bring-your-own EFS; leaks=%+v", rep.Leaks)
	}
}

// TestParseModalBatchedDecoratorLeaks (calque#91): @modal.batched(...) had
// ZERO recognition at all before this fix — unlike its four from_name/
// CloudBucketMount siblings tested above (TestParseRareConstructsAreTaggedNotSilent),
// it fell through completely unnoticed, no leak, no tag. Proves it now gets
// the SAME distinguishable leak treatment, AND that the leak is purely
// additive: the decorated function still resolves normally (still findable,
// still runnable) — batching itself is out of scope, only the leak is new.
func TestParseModalBatchedDecoratorLeaks(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/batched_function.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "modal.batched") && (strings.Contains(l.Detail, "batching") || strings.Contains(l.Detail, "coalescing")) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a leak mentioning modal.batched + batching/coalescing; leaks=%+v", rep.Leaks)
	}

	if _, ok := app.FindFunction("process"); !ok {
		t.Error(`function "process" not found in parsed app — the @modal.batched leak must be additive, not a refusal to parse the function`)
	}
}

// TestParseMapIterables (calque#136): a real .map()/.starmap() iterable that
// pyast could statically resolve (a literal list, a literal list of tuples,
// or a range()) must land in ir.Function.Items; an unresolvable one (a
// variable reference) must leave Items nil so callers fall back cleanly to
// their synthesized placeholder instead of erroring.
func TestParseMapIterables(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/map_iterables.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	fn := func(name string) ir.Function {
		f, ok := app.FindFunction(name)
		if !ok {
			t.Fatalf("function %q not found in parsed app", name)
		}
		return f
	}

	litMap := fn("lit_map")
	wantLitMap := []any{float64(1), float64(2), float64(3)}
	if !reflect.DeepEqual(litMap.Items, wantLitMap) {
		t.Errorf("lit_map.Items = %#v, want %#v", litMap.Items, wantLitMap)
	}

	litStarmap := fn("lit_starmap")
	wantStarmap := []any{
		[]any{float64(1), float64(2)},
		[]any{float64(3), float64(4)},
	}
	if !reflect.DeepEqual(litStarmap.Items, wantStarmap) {
		t.Errorf("lit_starmap.Items = %#v, want %#v", litStarmap.Items, wantStarmap)
	}

	litRange := fn("lit_range")
	wantRange := []any{float64(0), float64(1), float64(2), float64(3), float64(4)}
	if !reflect.DeepEqual(litRange.Items, wantRange) {
		t.Errorf("lit_range.Items = %#v, want %#v", litRange.Items, wantRange)
	}

	litUnresolvable := fn("lit_unresolvable")
	if litUnresolvable.Items != nil {
		t.Errorf("lit_unresolvable.Items = %#v, want nil (variable reference is not statically resolvable)", litUnresolvable.Items)
	}
}

// TestParseProgressiveImageMergesAcrossStatements (calque#140): an image built
// across MULTIPLE statements that reassign the same variable name — a real
// pattern (found in caru-ini/modal-comfyui) for conditionally/progressively
// appended build steps — must chain-extend the earlier statement's resolved
// base+steps, not have each later `image = image.step(...)` statement
// overwrite the whole record with a base-less one and discard everything
// already captured. testdata/scripts/progressive_image.py has 4 reassignments
// of `image`: one fully-resolved base statement, then three more
// `image = image.<step>(...)` statements (one of them nested inside an `if`,
// proving the merge isn't scoped to only module-top-level assigns).
func TestParseProgressiveImageMergesAcrossStatements(t *testing.T) {
	r, args := runner(t)
	rep := &leak.Report{}
	script, _ := filepath.Abs("../../testdata/scripts/progressive_image.py")

	app, err := Parse(context.Background(), script, rep, r, args...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// The ORIGINAL base (from statement 1) must survive every later reassignment.
	if app.Image.Base != "debian_slim" {
		t.Errorf("app.Image.Base = %q, want debian_slim (from the first statement, must survive later reassignments)", app.Image.Base)
	}
	if app.Image.Unresolved {
		t.Error("app.Image.Unresolved = true, want false — base was resolved in statement 1 and must not be clobbered by later image=image.step(...) statements")
	}

	// Every step from every statement must be present, IN ORDER: statement 1's
	// debian_slim/pip_install/apt_install, then statement 2's env, then the
	// conditional statement 3's add_local_file, then statement 4's run_commands.
	wantMethods := []string{
		"debian_slim", "pip_install", "apt_install", "env", "add_local_file", "run_commands",
	}
	gotMethods := make([]string, len(app.Image.Steps))
	for i, s := range app.Image.Steps {
		gotMethods[i] = s.Method
	}
	if !reflect.DeepEqual(gotMethods, wantMethods) {
		t.Errorf("image.Steps methods = %v, want %v (steps from every statement, in order)", gotMethods, wantMethods)
	}

	wantPip := map[string]bool{"torch": true, "vllm": true}
	for _, p := range app.Image.Pip {
		delete(wantPip, p)
	}
	if len(wantPip) != 0 {
		t.Errorf("image pip missing: %v (got %v) — statement 1's pip_install must survive", wantPip, app.Image.Pip)
	}

	// This is exactly the case the pre-fix code mishandled: no "image chain not
	// rooted at a known base constructor" leak should fire, since the base DID
	// resolve (in statement 1) — it must not be lost by the later reassignments.
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "image chain not rooted at a known base constructor") {
			t.Errorf("unexpected base_unresolved leak: %+v (base was resolved in statement 1 and should have been preserved across reassignments)", l)
		}
	}
}

// TestStringifyArgsPreservesPositionForLocalCopyMethods (calque#180) proves
// an unresolved add_local_file() SOURCE arg becomes a placeholder in its
// ORIGINAL position rather than being dropped — dropping it would shift the
// real destination path into position 0, where internal/image.localCopyArgs
// reads it as the SOURCE, producing a mangled COPY line (found live against
// AI-Almanac's forecasts_app.py, whose add_local_file(FORECAST_MODELS_YAML,
// "/almanac/forecast_models.yaml") has a non-literal Path-object source).
func TestStringifyArgsPreservesPositionForLocalCopyMethods(t *testing.T) {
	rep := &leak.Report{}
	args := []any{
		map[string]any{"__unparsed__": "str(workflow_file_path)"},
		"/root/workflow.json",
	}
	got := stringifyArgs(args, "add_local_file", "s.py", rep)
	want := []string{unresolvedArgPlaceholder, "/root/workflow.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stringifyArgs(add_local_file) = %#v, want %#v (unresolved source must NOT be dropped — that would shift the real destination into position 0)", got, want)
	}
	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "non-literal arg") {
			found = true
		}
	}
	if !found {
		t.Error("expected a non-literal-arg leak for the unresolved source")
	}
}

// TestStringifyArgsStillDropsUnresolvedForIndependentArgMethods (calque#180)
// proves the fix is scoped narrowly: pip_install/apt_install/run_commands
// etc. — whose args are each independent, not a paired (src, dst) — keep
// their EXISTING drop-on-unresolved behavior unchanged. A regression here
// would mean an unrelated method started emitting <<unresolved>> into a
// real shell command list.
func TestStringifyArgsStillDropsUnresolvedForIndependentArgMethods(t *testing.T) {
	rep := &leak.Report{}
	args := []any{"torch", map[string]any{"__unparsed__": "some_var"}, "vllm"}
	got := stringifyArgs(args, "pip_install", "s.py", rep)
	want := []string{"torch", "vllm"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stringifyArgs(pip_install) = %#v, want %#v (unrelated methods must keep dropping unresolved args, unchanged)", got, want)
	}
}
