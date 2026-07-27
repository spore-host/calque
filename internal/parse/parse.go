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
	Script       string              `json:"script"`
	AppName      string              `json:"app_name"`
	Images       map[string]pyImage  `json:"images"`
	Volumes      map[string]pyVolume `json:"volumes"`
	Functions    []pyFunc            `json:"functions"`
	Classes      []pyClass           `json:"classes"`
	Entrypoint   *pyFunc             `json:"entrypoint"`
	MapCalls     []pyMapCall         `json:"map_calls"`
	InvokeCalls  []pyInvokeCall      `json:"invoke_calls"`
	VolumeWrites []pyVolumeWrite     `json:"volume_writes"`
	HelperLeaks  []map[string]any    `json:"helper_leaks"`
	Error        string              `json:"error"`
}

// pyVolumeWrite is a volume.commit()/reload() call site (§E). Target is the var the
// method was called on; the Go side correlates it against known Volume vars.
type pyVolumeWrite struct {
	Target string `json:"target"`
	Kind   string `json:"kind"` // "commit" | "reload"
	Lineno int    `json:"lineno"`
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
	EntryKind  string        `json:"entry_kind"` // "serve" for a serve entrypoint, else "" (§F)
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

// pyInvokeCall is a recognized invocation idiom call site (§C): kind is one of
// map/starmap/for_each/remote (synchronous, in scope) or spawn/map.aio (async,
// deferred + leaked). Target is the dotted callable reference.
type pyInvokeCall struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`
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

	// How is each callable invoked? (spec §13: "where .map() is called", §C: the
	// other sync idioms .starmap/.for_each/.remote). The target of a call like
	// `Chat().generate.map(...)` is the trailing attribute ("generate"); we key by
	// that leaf name to match the callable's Name.
	invokes := invocationKinds(out, script, rep)

	for _, f := range out.Functions {
		app.Functions = append(app.Functions, buildFn(f, script, rep, invokes))
	}
	for _, c := range out.Classes {
		app.Classes = append(app.Classes, buildClass(c, script, rep, invokes))
	}
	if out.Entrypoint != nil {
		fn := buildFn(*out.Entrypoint, script, rep, invokes)
		app.Entrypoint = &fn
	}

	// E3: volume.commit()/reload() call sites. A volume that's WRITTEN (not just
	// read) has persistence semantics; a MID-RUN commit (inside a method body,
	// re-reading between items) is a semantic gap the spike can't reproduce — leak
	// it rather than silently persist only at end-of-run.
	knownVol := map[string]bool{}
	for name := range out.Volumes {
		knownVol[name] = true
	}
	for _, vw := range out.VolumeWrites {
		if !knownVol[vw.Target] {
			continue // some other object's .commit()/.reload(), not a Volume
		}
		if vw.Kind == "reload" {
			rep.Addf(leak.PrimVolume, leak.KindSemanticGap, script, vw.Lineno,
				"volume %q .reload() — mid-run re-read of a mutated volume is not reproduced; the spike syncs once before @enter and commits once after drain", vw.Target)
		} else {
			// commit(): honored as an END-OF-RUN write-back (E1/E2). Note it so the
			// caller wires CommitCommands for this volume.
			rep.Addf(leak.PrimVolume, leak.KindSemanticGap, script, vw.Lineno,
				"volume %q .commit() — persisted as an END-OF-RUN write-back (§E); a mid-run commit visible to concurrent readers is not reproduced", vw.Target)
		}
	}
	return app
}

// invocationKinds resolves each callable's invocation idiom from the helper's call
// sites. Precedence when a callable is invoked multiple ways: map > starmap >
// for_each > remote (the batch idioms dominate the spike's execution model). Async
// idioms (.spawn/.map.aio) are recognized by the helper and leaked as deferred
// (§C/M10-S2), never mapped to an executable kind here.
func invocationKinds(out pyOut, script string, rep *leak.Report) map[string]ir.InvokeKind {
	rank := map[ir.InvokeKind]int{
		ir.InvokeMap: 4, ir.InvokeStarmap: 3, ir.InvokeForEach: 2, ir.InvokeRemote: 1,
	}
	invokes := map[string]ir.InvokeKind{}
	consider := func(target string, kind ir.InvokeKind) {
		leaf := leafName(target)
		if rank[kind] > rank[invokes[leaf]] {
			invokes[leaf] = kind
		}
	}
	// Back-compat: MapCalls is the pre-existing .map() channel.
	for _, mc := range out.MapCalls {
		consider(mc.Target, ir.InvokeMap)
	}
	// §C: the other synchronous idioms, plus async ones flagged for a deferred leak.
	for _, ic := range out.InvokeCalls {
		switch ic.Kind {
		case "map":
			consider(ic.Target, ir.InvokeMap)
		case "starmap":
			consider(ic.Target, ir.InvokeStarmap)
		case "for_each":
			consider(ic.Target, ir.InvokeForEach)
		case "remote":
			consider(ic.Target, ir.InvokeRemote)
		case "spawn", "map.aio":
			// S2: async result futures / detach — deferred per §18; block-and-wait only.
			rep.Addf(leak.PrimMap, leak.KindSemanticGap, script, ic.Lineno,
				"%s.%s(...): async result futures / detach — deferred per §18; the spike is block-and-wait only", leafName(ic.Target), ic.Kind)
		}
	}
	return invokes
}

// leafName returns the trailing attribute of a dotted call target: the callable's
// own name. "Chat().generate" -> "generate"; "f" -> "f".
func leafName(target string) string {
	if i := lastDot(target); i >= 0 {
		return target[i+1:]
	}
	return target
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// mergeConfig folds a decorator's config into the accumulated one; later
// non-zero values win (a callable rarely sets the same field twice).
func mergeConfig(dst, src ir.Config) ir.Config {
	if src.CPU != 0 {
		dst.CPU = src.CPU
	}
	if src.MemoryMB != 0 {
		dst.MemoryMB = src.MemoryMB
	}
	if src.Retries != 0 {
		dst.Retries = src.Retries
	}
	if len(src.Secrets) > 0 {
		dst.Secrets = src.Secrets
	}
	if src.Schedule != "" {
		dst.Schedule = src.Schedule
	}
	if src.Region != "" {
		dst.Region = src.Region
	}
	return dst
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

func buildFn(f pyFunc, script string, rep *leak.Report, invokes map[string]ir.InvokeKind) ir.Function {
	fn := ir.Function{
		Name:      f.Name,
		Body:      f.Body,
		Line:      f.Lineno,
		Invoke:    invokes[f.Name],
		EntryKind: ir.EntryKind(f.EntryKind), // "serve" or "" (§F)
		ItemArg:   firstItemArg(f.Args),
	}
	fn.IsMap = fn.Invoke == ir.InvokeMap // back-compat: IsMap tracks the .map() idiom
	// The function-config decorator is the one named "*.function" (or "*.method"
	// for class methods); enter/method markers carry no gpu/volumes.
	for _, d := range f.Decorators {
		gpu, vols, timeout, cfg := readConfigKwargs(d.Kwargs, leak.PrimGPU, f.Name, script, d.Lineno, rep)
		if gpu != "" {
			fn.GPU = gpu
		}
		if vols != nil {
			fn.Volumes = vols
		}
		if timeout != 0 {
			fn.Timeout = timeout
		}
		fn.Config = mergeConfig(fn.Config, cfg)
	}
	return fn
}

func buildClass(c pyClass, script string, rep *leak.Report, invokes map[string]ir.InvokeKind) ir.Class {
	cls := ir.Class{Name: c.Name, Line: c.Lineno}
	gpu, vols, timeout, cfg := readConfigKwargs(c.ClsKwargs, leak.PrimGPU, c.Name, script, c.Lineno, rep)
	cls.GPU, cls.Volumes, cls.Timeout, cls.Config = gpu, vols, timeout, cfg
	if c.Enter != nil {
		cls.EnterBody = c.Enter.Body
	} else {
		// A @cls with no @enter still runs, but the warm-load-once economics (§6)
		// don't apply — worth noting since it changes the amortization story.
		rep.Addf(leak.PrimEnter, leak.KindUnhandledCase, script, c.Lineno,
			"@cls %q has no @enter; no warm load-once body to run", c.Name)
	}
	for _, m := range c.Methods {
		method := buildFn(m, script, rep, invokes)
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

// autoscalingKwargs are Modal's warm-pool / concurrency knobs. They belong to the
// real scheduling brain behind the seam (§4), NOT the spike (§1) — so we RECOGNIZE
// them (a specific deferred-leak, never a generic "unmodeled arg") and move on
// (M10/S1). Recognizing them keeps the census honest without pretending to honor.
var autoscalingKwargs = map[string]bool{
	"concurrency_limit": true, "allow_concurrent_inputs": true,
	"min_containers": true, "max_containers": true, "keep_warm": true,
	// older Modal spellings seen in the wild:
	"container_idle_timeout": true, "concurrent_inputs": true,
}

// readConfigKwargs pulls gpu/volumes/timeout + the portable Config kwargs
// (cpu/memory/retries/secrets/schedule/region, §B) out of a decorator's kwargs.
// Autoscaling kwargs are recognized and leaked as deferred (§4/§1, M10/S1);
// anything else it can't model becomes a generic leak (§10).
func readConfigKwargs(kwargs map[string]json.RawMessage, _ leak.Primitive, owner, script string, line int, rep *leak.Report) (gpu string, vols map[string]string, timeout int, cfg ir.Config) {
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
		case "cpu":
			// cpu= may be an int or float (cores). Accept either.
			if f, ok := decodeFloat(raw); ok {
				cfg.CPU = f
			}
		case "memory":
			// memory= is MB (int) in Modal; a [request, limit] list also occurs — take
			// the request (first) element and leak the limit we don't model.
			if n, ok := decodeInt(raw); ok {
				cfg.MemoryMB = n
			} else if lst, ok := decodeIntList(raw); ok && len(lst) > 0 {
				cfg.MemoryMB = lst[0]
				if len(lst) > 1 {
					rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
						"%s: memory=[request,limit] — using request %dMB; the limit is not enforced", owner, lst[0])
				}
			}
		case "retries":
			// retries= may be an int or a modal.Retries(...) object; take the int form.
			if n, ok := decodeInt(raw); ok {
				cfg.Retries = n
			} else {
				rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
					"%s: retries= is not a plain int (%s); re-drive cap uses the default", owner, string(raw))
			}
		case "secrets":
			// Recorded, not honored: secret injection is behind the seam. Leak so a
			// payload that NEEDS a secret fails visibly rather than mysteriously.
			cfg.Secrets = decodeStringListBestEffort(raw)
			rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
				"%s: secrets=%s recorded but NOT injected in the spike; a payload needing them will fail", owner, string(raw))
		case "schedule":
			if s, ok := decodeString(raw); ok {
				cfg.Schedule = s
			} else {
				cfg.Schedule = string(raw)
			}
			rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
				"%s: schedule= recorded but NOT honored (no scheduler in the spike)", owner)
		case "region":
			if s, ok := decodeString(raw); ok {
				cfg.Region = s
			}
			rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
				"%s: region= placement hint recorded but NOT honored (acquisition sweeps offered AZs)", owner)
		case "image":
			// image=var reference; recorded structurally, resolution is at app level.
		case "__splat__":
			rep.Addf(leak.PrimEntrypoint, leak.KindUnsupportedArg, script, line,
				"%s: decorator uses **kwargs splat; args not statically visible", owner)
		default:
			if autoscalingKwargs[k] {
				// S1: recognized-but-deferred. Autoscaling/warm-pool config is the real
				// brain's job behind the seam (§4), not ported in the spike (§1).
				rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
					"%s: autoscaling/warm-pool config %q=%s — belongs to the real brain behind the seam (§4), not ported in the spike (§1)", owner, k, string(raw))
				continue
			}
			// A decorator arg the parser doesn't model (spec §10: "any decorator arg the parser doesn't handle").
			rep.Addf(leak.PrimEntrypoint, leak.KindUnsupportedArg, script, line,
				"%s: unmodeled decorator arg %q=%s", owner, k, string(raw))
		}
	}
	return gpu, vols, timeout, cfg
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

func decodeFloat(raw json.RawMessage) (float64, bool) {
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	return 0, false
}

func decodeIntList(raw json.RawMessage) ([]int, bool) {
	var xs []int
	if err := json.Unmarshal(raw, &xs); err == nil {
		return xs, true
	}
	return nil, false
}

// decodeStringListBestEffort accepts a JSON string, a []string, or falls back to
// the raw text — secret refs come through in several shapes (a name, a list, or a
// modal.Secret.from_name(...) unparsed marker). We only need a record for the leak.
func decodeStringListBestEffort(raw json.RawMessage) []string {
	if s, ok := decodeString(raw); ok {
		return []string{s}
	}
	var xs []string
	if err := json.Unmarshal(raw, &xs); err == nil {
		return xs
	}
	return []string{string(raw)}
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

// firstItemArg returns the first non-self/cls parameter — the per-item argument
// the warm runner binds each work item to (e.g. "prompt" in generate(self, prompt)).
func firstItemArg(args []string) string {
	for _, a := range args {
		if a == "self" || a == "cls" {
			continue
		}
		return a
	}
	return "item"
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
