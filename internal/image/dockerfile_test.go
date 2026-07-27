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

// TestUnhandledVerbLeaks: a verb the renderer STILL doesn't model must be
// commented AND leaked (never silently dropped).
func TestUnhandledVerbLeaks(t *testing.T) {
	rep := &leak.Report{}
	img := ir.Image{
		Base:  "debian_slim",
		Steps: []ir.ImageStep{{Method: "debian_slim"}, {Method: "some_future_verb", Args: []string{"x"}}},
	}
	df, err := Render(Spec{Image: img}, "s.py", rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(df, "# UNHANDLED .some_future_verb") {
		t.Error("unhandled verb should be commented in the Dockerfile")
	}
	if rep.Len() == 0 {
		t.Error("unhandled verb should emit a leak, not be silently dropped")
	}
}

// TestNewlyHandledVerbs (A1): verbs that previously fell to the UNHANDLED default
// now render real Dockerfile lines and do NOT emit an UNHANDLED comment.
func TestNewlyHandledVerbs(t *testing.T) {
	rep := &leak.Report{}
	img := ir.Image{
		Base: "debian_slim",
		Steps: []ir.ImageStep{
			{Method: "debian_slim"},
			{Method: "pip_install_from_requirements", Args: []string{"requirements.txt"}},
			{Method: "poetry_install_from_file", Args: []string{"pyproject.toml"}},
			{Method: "dockerfile_commands", Args: []string{"RUN echo hi", "ENV FOO=bar"}},
			{Method: "entrypoint", Args: []string{"python", "-m", "server"}},
		},
	}
	df, err := Render(Spec{Image: img}, "s.py", rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"COPY requirements.txt /tmp/requirements.txt",
		"pip3 install --no-cache-dir -r /tmp/requirements.txt",
		"poetry install --no-root",
		"RUN echo hi",
		"ENV FOO=bar",
		`ENTRYPOINT ["python", "-m", "server"]`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile missing %q\n---\n%s", want, df)
		}
	}
	if strings.Contains(df, "# UNHANDLED") {
		t.Errorf("newly-handled verbs should not be UNHANDLED\n%s", df)
	}
}

// TestFromAwsEcrBase (A2): from_aws_ecr resolves to the named ECR image as the
// FROM base and notes the pull-permission requirement.
func TestFromAwsEcrBase(t *testing.T) {
	rep := &leak.Report{}
	ref := "123456789012.dkr.ecr.us-west-2.amazonaws.com/myrepo:latest"
	img := ir.Image{
		Base:  "from_aws_ecr",
		Steps: []ir.ImageStep{{Method: "from_aws_ecr", Args: []string{ref}}},
	}
	df, err := Render(Spec{Image: img}, "s.py", rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(df, "FROM "+ref) {
		t.Errorf("from_aws_ecr should resolve to the ECR image as base\n%s", df)
	}
	if rep.Len() == 0 {
		t.Error("from_aws_ecr should note the ECR pull-permission requirement")
	}
}

// TestLocalCopyStagingLeaks (A3): add_local_* emits a COPY AND a semantic-gap leak
// (the local path must be staged into the build context, which calque doesn't do).
func TestLocalCopyStagingLeaks(t *testing.T) {
	rep := &leak.Report{}
	img := ir.Image{
		Base:  "debian_slim",
		Steps: []ir.ImageStep{{Method: "debian_slim"}, {Method: "add_local_dir", Args: []string{"./data", "/data"}}},
	}
	df, err := Render(Spec{Image: img}, "s.py", rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(df, "COPY ./data /data") {
		t.Errorf("add_local_dir should emit a COPY\n%s", df)
	}
	if rep.Len() == 0 {
		t.Error("add_local_dir must leak the build-context staging requirement")
	}
}
