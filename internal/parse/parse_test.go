package parse

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

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
