package image

import (
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

func mapBatchImage() ir.Image {
	return ir.Image{
		Base: "debian_slim",
		Pip:  []string{"vllm==0.6.3", "transformers==4.45.2", "huggingface_hub"},
		Steps: []ir.ImageStep{
			{Method: "debian_slim"},
			{Method: "pip_install", Args: []string{"vllm==0.6.3", "transformers==4.45.2"}},
			{Method: "uv_pip_install", Args: []string{"huggingface_hub"}},
		},
	}
}

func TestRenderBasic(t *testing.T) {
	rep := &leak.Report{}
	df, err := Render(Spec{Image: mapBatchImage()}, "map_batch.py", rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04",
		"vllm==0.6.3",
		"transformers==4.45.2",
		"huggingface_hub",
		"COPY runner.py",
		"COPY warmd",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile missing %q\n---\n%s", want, df)
		}
	}
	if rep.Len() != 0 {
		t.Errorf("clean image should not leak, got %+v", rep.Leaks)
	}
}

// TestDigestStable is the cache-hit property (§10): the SAME image chain must
// produce the SAME digest, so a rebuild-on-no-change is a cache hit, not a rebuild.
func TestDigestStable(t *testing.T) {
	rep := &leak.Report{}
	df1, _ := Render(Spec{Image: mapBatchImage()}, "s.py", rep)
	df2, _ := Render(Spec{Image: mapBatchImage()}, "s.py", rep)
	if Digest(df1) != Digest(df2) {
		t.Errorf("identical image chains produced different digests: %s vs %s", Digest(df1), Digest(df2))
	}
	// A different pip set must produce a different digest.
	other := mapBatchImage()
	other.Steps[1].Args = []string{"vllm==0.7.0"}
	df3, _ := Render(Spec{Image: other}, "s.py", rep)
	if Digest(df1) == Digest(df3) {
		t.Error("different image chains produced the same digest (cache would wrongly hit)")
	}
}

// TestPipOrderInvariant: package order shouldn't change the digest (so trivially
// reordered pip lists still cache-hit), but versions should.
func TestPipOrderInvariant(t *testing.T) {
	rep := &leak.Report{}
	a := mapBatchImage()
	b := mapBatchImage()
	b.Steps[1].Args = []string{"transformers==4.45.2", "vllm==0.6.3"} // swapped
	dfa, _ := Render(Spec{Image: a}, "s.py", rep)
	dfb, _ := Render(Spec{Image: b}, "s.py", rep)
	if Digest(dfa) != Digest(dfb) {
		t.Error("reordered pip packages changed the digest; cache would miss spuriously")
	}
}

func TestUnhandledVerbLeaks(t *testing.T) {
	rep := &leak.Report{}
	img := ir.Image{
		Base:  "debian_slim",
		Steps: []ir.ImageStep{{Method: "debian_slim"}, {Method: "add_local_dir", Args: []string{"./data", "/data"}}},
	}
	df, err := Render(Spec{Image: img}, "s.py", rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(df, "# UNHANDLED .add_local_dir") {
		t.Error("unhandled verb should be commented in the Dockerfile")
	}
	if rep.Len() == 0 {
		t.Error("unhandled verb should emit a leak, not be silently dropped")
	}
}
