// Package parse turns a Modal script into the six-primitive IR (spec §13) by
// shelling out to the pyast helper (tools/pyast) and transcribing its JSON.
//
// We do NOT write a Python parser in Go for the spike; tree-sitter-python is the
// v2 answer. We are not testing the parser — we're testing whether the mapping
// carries. Anything the helper couldn't model, or that this loader can't map
// cleanly, becomes a structured leak (§10) rather than a silent drop.
package parse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// ---- JSON contract emitted by tools/pyast/pyast.py ----

type pyOut struct {
	Script      string              `json:"script"`
	AppName     string              `json:"app_name"`
	Images      map[string]pyImage  `json:"images"`
	Volumes     map[string]pyVolume `json:"volumes"`
	Functions   []pyFunc            `json:"functions"`
	Classes     []pyClass           `json:"classes"`
	Entrypoint  *pyFunc             `json:"entrypoint"`
	MapCalls    []pyMapCall         `json:"map_calls"`
	HelperLeaks []map[string]any    `json:"helper_leaks"`
	Error       string              `json:"error"`
}

type pyImage struct {
	Base           string      `json:"base"`
	Steps          []pyImgStep `json:"steps"`
	BaseUnresolved bool        `json:"base_unresolved"`
}

type pyImgStep struct {
	Method string `json:"method"`
	Args   []any  `json:"args"`
}

type pyVolume struct {
	FromName string `json:"from_name"`
	Lineno   int    `json:"lineno"`
}

type pyFunc struct {
	Name       string        `json:"name"`
	Lineno     int           `json:"lineno"`
	Args       []string      `json:"args"`
	Decorators []pyDecorator `json:"decorators"`
	Body       string        `json:"body"`
}

type pyDecorator struct {
	Name   string                     `json:"name"`
	Kwargs map[string]json.RawMessage `json:"kwargs"`
	Lineno int                        `json:"lineno"`
}

type pyClass struct {
	Name      string                     `json:"name"`
	Lineno    int                        `json:"lineno"`
	ClsKwargs map[string]json.RawMessage `json:"cls_kwargs"`
	Enter     *pyFunc                    `json:"enter"`
	Methods   []pyFunc                   `json:"methods"`
}

type pyMapCall struct {
	Target string `json:"target"`
	Lineno int    `json:"lineno"`
}

// Parse runs the helper on scriptPath and returns the IR plus any leaks emitted
// during transcription. runner/runnerArgs is how we invoke the helper (so callers
// and tests can point at `uv run ...`); see DefaultRunner.
func Parse(ctx context.Context, scriptPath string, rep *leak.Report, runner string, runnerArgs ...string) (ir.App, error) {
	args := append(append([]string{}, runnerArgs...), scriptPath)
	cmd := exec.CommandContext(ctx, runner, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return ir.App{}, fmt.Errorf("pyast helper failed: %w (stderr: %s)", err, stderr.String())
	}

	var out pyOut
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return ir.App{}, fmt.Errorf("pyast JSON decode: %w", err)
	}
	if out.Error != "" {
		return ir.App{}, fmt.Errorf("pyast reported error: %s", out.Error)
	}

	return build(out, rep), nil
}

// DefaultRunner is how the CLI invokes the helper: uv, from the pyast project.
// Kept here so the invocation lives in one place.
func DefaultRunner(pyastDir string) (string, []string) {
	return "uv", []string{"run", "--project", pyastDir, "python", pyastDir + "/pyast.py"}
}

func build(out pyOut, rep *leak.Report) ir.App {
	script := out.Script

	// Surface helper-level leaks first (the helper saw something it couldn't model).
	for _, hl := range out.HelperLeaks {
		where, _ := hl["where"].(string)
		detail, _ := hl["detail"].(string)
		line := asInt(hl["lineno"])
		rep.Addf(leak.PrimImage, leak.KindUnhandledCase, script, line,
			"pyast helper flagged %q: %s", where, detail)
	}

	app := ir.App{
		Name:    out.AppName,
		Script:  script,
		Volumes: map[string]string{},
	}
	for name, v := range out.Volumes {
		app.Volumes[name] = v.FromName
	}

	// Resolve the app image. Modal scripts commonly define exactly one image var;
	// if there are several we take the first deterministically and leak the ambiguity.
	app.Image = resolveImage(out, script, rep)

	// Which callables have .map() invoked on them? (spec §13: "where .map() is called")
	mapped := map[string]bool{}
	for _, mc := range out.MapCalls {
		mapped[mc.Target] = true
	}

	for _, f := range out.Functions {
		app.Functions = append(app.Functions, buildFn(f, script, rep, mapped))
	}
	for _, c := range out.Classes {
		app.Classes = append(app.Classes, buildClass(c, script, rep, mapped))
	}
	if out.Entrypoint != nil {
		fn := buildFn(*out.Entrypoint, script, rep, mapped)
		app.Entrypoint = &fn
	}
	return app
}

func resolveImage(out pyOut, script string, rep *leak.Report) ir.Image {
	if len(out.Images) == 0 {
		return ir.Image{}
	}
	// Deterministic pick: prefer a var literally named "image", else the
	// lexicographically first, and leak if we had to choose among several.
	var chosenName string
	if _, ok := out.Images["image"]; ok {
		chosenName = "image"
	} else {
		for name := range out.Images {
			if chosenName == "" || name < chosenName {
				chosenName = name
			}
		}
	}
	if len(out.Images) > 1 {
		rep.Addf(leak.PrimImage, leak.KindUnhandledCase, script, 0,
			"multiple image definitions (%d); spike uses %q. Per-function image selection is deferred.",
			len(out.Images), chosenName)
	}
	pi := out.Images[chosenName]
	img := ir.Image{Base: pi.Base, Unresolved: pi.BaseUnresolved}
	if pi.BaseUnresolved {
		rep.Add(leak.PrimImage, leak.KindSemanticGap, script, 0,
			"image chain not rooted at a known base constructor; Dockerfile base cannot be resolved")
	}
	for _, s := range pi.Steps {
		strArgs := stringifyArgs(s.Args, s.Method, script, rep)
		img.Steps = append(img.Steps, ir.ImageStep{Method: s.Method, Args: strArgs})
		if s.Method == "pip_install" || s.Method == "uv_pip_install" {
			img.Pip = append(img.Pip, strArgs...)
		}
	}
	return img
}

func buildFn(f pyFunc, script string, rep *leak.Report, mapped map[string]bool) ir.Function {
	fn := ir.Function{
		Name:  f.Name,
		Body:  f.Body,
		Line:  f.Lineno,
		IsMap: mapped[f.Name],
	}
	// The function-config decorator is the one named "*.function" (or "*.method"
	// for class methods); enter/method markers carry no gpu/volumes.
	for _, d := range f.Decorators {
		gpu, vols, timeout := readConfigKwargs(d.Kwargs, leak.PrimGPU, f.Name, script, d.Lineno, rep)
		if gpu != "" {
			fn.GPU = gpu
		}
		if vols != nil {
			fn.Volumes = vols
		}
		if timeout != 0 {
			fn.Timeout = timeout
		}
	}
	return fn
}

func buildClass(c pyClass, script string, rep *leak.Report, mapped map[string]bool) ir.Class {
	cls := ir.Class{Name: c.Name, Line: c.Lineno}
	gpu, vols, timeout := readConfigKwargs(c.ClsKwargs, leak.PrimGPU, c.Name, script, c.Lineno, rep)
	cls.GPU, cls.Volumes, cls.Timeout = gpu, vols, timeout
	if c.Enter != nil {
		cls.EnterBody = c.Enter.Body
	} else {
		// A @cls with no @enter still runs, but the warm-load-once economics (§6)
		// don't apply — worth noting since it changes the amortization story.
		rep.Addf(leak.PrimEnter, leak.KindUnhandledCase, script, c.Lineno,
			"@cls %q has no @enter; no warm load-once body to run", c.Name)
	}
	for _, m := range c.Methods {
		method := buildFn(m, script, rep, mapped)
		// A class method inherits the class's gpu/volumes if it declares none.
		if method.GPU == "" {
			method.GPU = cls.GPU
		}
		if method.Volumes == nil {
			method.Volumes = cls.Volumes
		}
		cls.Methods = append(cls.Methods, method)
	}
	return cls
}

// readConfigKwargs pulls gpu/volumes/timeout out of a decorator's kwargs and
// leaks any kwarg it can't model (splats, unparsed expressions, unknown args).
func readConfigKwargs(kwargs map[string]json.RawMessage, _ leak.Primitive, owner, script string, line int, rep *leak.Report) (gpu string, vols map[string]string, timeout int) {
	for k, raw := range kwargs {
		switch k {
		case "gpu":
			if s, ok := decodeString(raw); ok {
				gpu = s
			} else {
				rep.Addf(leak.PrimGPU, leak.KindUnsupportedArg, script, line,
					"%s: gpu= is not a plain string literal (%s); cannot apply rewrite rule", owner, string(raw))
			}
		case "timeout":
			if n, ok := decodeInt(raw); ok {
				timeout = n
			}
		case "volumes":
			if m, ok := decodeStringMap(raw); ok {
				vols = m
			} else {
				rep.Addf(leak.PrimVolume, leak.KindUnsupportedArg, script, line,
					"%s: volumes= not a {str:str} map (%s)", owner, string(raw))
			}
		case "image":
			// image=var reference; recorded structurally, resolution is at app level.
		case "__splat__":
			rep.Addf(leak.PrimEntrypoint, leak.KindUnsupportedArg, script, line,
				"%s: decorator uses **kwargs splat; args not statically visible", owner)
		default:
			// A decorator arg the parser doesn't model (spec §10: "any decorator arg the parser doesn't handle").
			rep.Addf(leak.PrimEntrypoint, leak.KindUnsupportedArg, script, line,
				"%s: unmodeled decorator arg %q=%s", owner, k, string(raw))
		}
	}
	return gpu, vols, timeout
}

// ---- small decode helpers (kwargs are heterogeneous JSON) ----

func decodeString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

func decodeInt(raw json.RawMessage) (int, bool) {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}
	return 0, false
}

// decodeStringMap accepts {"/mnt":"vol"} but rejects a map whose values aren't
// plain strings (e.g. the __unparsed__ marker the helper emits for non-literals).
func decodeStringMap(raw json.RawMessage) (map[string]string, bool) {
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err == nil {
		if _, bad := m["__unparsed__"]; bad {
			return nil, false
		}
		return m, true
	}
	return nil, false
}

// stringifyArgs coerces image-step args to strings, leaking any non-string arg
// (e.g. an __unparsed__ marker dict) so it isn't silently dropped from the build.
func stringifyArgs(args []any, method, script string, rep *leak.Report) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch v := a.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			if u, ok := v["__unparsed__"]; ok {
				rep.Addf(leak.PrimImage, leak.KindUnsupportedArg, script, 0,
					"image step .%s(...) has a non-literal arg: %v", method, u)
			}
		default:
			rep.Addf(leak.PrimImage, leak.KindUnsupportedArg, script, 0,
				"image step .%s(...) has a non-string arg: %v", method, v)
		}
	}
	return out
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}
