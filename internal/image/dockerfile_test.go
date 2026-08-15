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
// TestRegistryRef (calque#176) proves the extraction cmd/calque/realrun.go
// needs to decide whether a --script real run can pull the picked unit's
// OWN resolved image, instead of resolveBase's Dockerfile-FROM-line logic
// (which additionally leaks/falls back — not what a pull decision needs).
func TestRegistryRef(t *testing.T) {
	cases := []struct {
		name    string
		img     ir.Image
		wantRef string
		wantOK  bool
	}{
		{
			name: "from_registry resolves",
			img: ir.Image{
				Base:  "from_registry",
				Steps: []ir.ImageStep{{Method: "from_registry", Args: []string{"us-central1-docker.pkg.dev/ai-almanac/almanac/romp:latest"}}},
			},
			wantRef: "us-central1-docker.pkg.dev/ai-almanac/almanac/romp:latest",
			wantOK:  true,
		},
		{
			name: "from_aws_ecr resolves",
			img: ir.Image{
				Base:  "from_aws_ecr",
				Steps: []ir.ImageStep{{Method: "from_aws_ecr", Args: []string{"123456789012.dkr.ecr.us-west-2.amazonaws.com/myrepo:latest"}}},
			},
			wantRef: "123456789012.dkr.ecr.us-west-2.amazonaws.com/myrepo:latest",
			wantOK:  true,
		},
		{
			name:   "debian_slim has no pullable ref",
			img:    ir.Image{Base: "debian_slim", Steps: []ir.ImageStep{{Method: "debian_slim"}}},
			wantOK: false,
		},
		{
			name:   "unresolved never returns a ref",
			img:    ir.Image{Unresolved: true},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, ok := RegistryRef(c.img)
			if ok != c.wantOK {
				t.Errorf("RegistryRef() ok = %v, want %v", ok, c.wantOK)
			}
			if ref != c.wantRef {
				t.Errorf("RegistryRef() ref = %q, want %q", ref, c.wantRef)
			}
		})
	}
}

// TestNeedsBuild (calque#177) proves the three-way split realrun.go relies
// on to pick pull-only (#176) vs. build-on-instance (#177) vs. host-mode
// (#79): a bare pullable ref needs no build, a pullable ref with layered
// steps on top DOES need a build (pulling the base alone would silently
// drop those steps), a from-scratch chain (no pullable base at all) needs
// a build, and an unresolved/empty image needs neither (the caller's
// existing host-mode fallback already covers that case).
func TestNeedsBuild(t *testing.T) {
	cases := []struct {
		name string
		img  ir.Image
		want bool
	}{
		{
			name: "bare from_registry, no layered steps: pull-only, no build",
			img: ir.Image{
				Base:  "from_registry",
				Steps: []ir.ImageStep{{Method: "from_registry", Args: []string{"nvcr.io/nvidia/pytorch:25.12-py3"}}},
			},
			want: false,
		},
		{
			name: "from_registry with pip_install layered on top: needs build",
			img: ir.Image{
				Base: "from_registry",
				Steps: []ir.ImageStep{
					{Method: "from_registry", Args: []string{"nvcr.io/nvidia/pytorch:25.12-py3"}},
					{Method: "pip_install", Args: []string{"earth2studio"}},
				},
			},
			want: true,
		},
		{
			name: "from-scratch debian_slim chain, no pullable base: needs build",
			img: ir.Image{
				Base: "debian_slim",
				Steps: []ir.ImageStep{
					{Method: "debian_slim"},
					{Method: "apt_install", Args: []string{"git"}},
				},
			},
			want: true,
		},
		{
			name: "unresolved: no build (host-mode fallback handles it)",
			img:  ir.Image{Unresolved: true},
			want: false,
		},
		{
			name: "empty/zero-value image: no build",
			img:  ir.Image{},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NeedsBuild(c.img); got != c.want {
				t.Errorf("NeedsBuild() = %v, want %v", got, c.want)
			}
		})
	}
}

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

// TestMicromambaBase (calque#84): Image.micromamba() is recognized as a base at
// the AST layer but was falling through to "unknown image base" and silently
// defaulting to CUDA. Must resolve to a real micromamba base + a leak noting
// kwargs (python_version=) aren't captured by the parser.
func TestMicromambaBase(t *testing.T) {
	rep := &leak.Report{}
	img := ir.Image{
		Base:  "micromamba",
		Steps: []ir.ImageStep{{Method: "micromamba"}},
	}
	df, err := Render(Spec{Image: img}, "s.py", rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(df, "nvidia/cuda") {
		t.Errorf("micromamba() must NOT silently default to the CUDA base\n%s", df)
	}
	if !strings.Contains(df, "FROM mambaorg/micromamba") {
		t.Errorf("micromamba() should resolve to a real micromamba base\n%s", df)
	}
	if rep.Len() == 0 {
		t.Error("micromamba() should leak that kwargs aren't captured by the parser")
	}
}

// TestFromDockerfileBase (calque#84): Image.from_dockerfile(path) is recognized as
// a base at the AST layer but was falling through to "unknown image base" and
// silently defaulting to CUDA, with no mention of the actual local Dockerfile path.
func TestFromDockerfileBase(t *testing.T) {
	rep := &leak.Report{}
	img := ir.Image{
		Base:  "from_dockerfile",
		Steps: []ir.ImageStep{{Method: "from_dockerfile", Args: []string{"./Dockerfile.gpu"}}},
	}
	df, err := Render(Spec{Image: img}, "s.py", rep)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Len() == 0 {
		t.Fatal("from_dockerfile(...) should leak that the local Dockerfile isn't staged")
	}
	found := false
	for _, l := range rep.Leaks {
		if strings.Contains(l.Detail, "Dockerfile.gpu") {
			found = true
		}
	}
	if !found {
		t.Errorf("leak should name the specific unstaged path; leaks=%+v (df=%s)", rep.Leaks, df)
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
