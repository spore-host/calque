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
	"strings"

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
	Entrypoints  []pyFunc            `json:"entrypoints"`
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
	// LocalCalls are .local()-called sibling targets referenced ANYWHERE inside
	// this function/method's own body (calque#92) — dotted call targets, leaf-
	// resolved the same way every other invoke target is (see leafName).
	LocalCalls []string `json:"local_calls"`
	Body       string   `json:"body"`
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
	Exit      *pyFunc                    `json:"exit"` // @modal.exit(); calque#86
	Methods   []pyFunc                   `json:"methods"`
}

type pyMapCall struct {
	Target string `json:"target"`
	Lineno int    `json:"lineno"`
	// Entrypoint is the @app.local_entrypoint() this call site's own body was
	// found nested inside, or "" if it wasn't inside one (calque#98: lets
	// invocationKinds attribute evidence to a SPECIFIC entrypoint instead of
	// folding every call site into one whole-script-flat union).
	Entrypoint string `json:"entrypoint"`
	// Iterable is the .map() call's real iterable argument, when pyast could
	// statically resolve it (calque#136) — nil when it's a variable,
	// comprehension, or non-range function call result.
	Iterable *pyIterable `json:"iterable,omitempty"`
}

// pyIterable is a `.map()`/`.starmap()` call's iterable argument, captured at
// parse time when it's statically resolvable (calque#136; see pyast.py's
// _iterable_literal). Kind is "literal" (a literal list/tuple/str argument)
// or "range" (a range(...) call whose own args were all literal ints); Values
// is the resolved item list either way, heterogeneous JSON decoded lazily by
// the caller (a starmap's items are themselves lists/tuples, a map's items
// are usually scalars).
type pyIterable struct {
	Kind   string            `json:"kind"`
	Values []json.RawMessage `json:"values"`
}

// pyInvokeCall is a recognized invocation idiom call site (§C): kind is one of
// map/starmap/for_each/remote (synchronous, in scope) or spawn/map.aio (async,
// deferred + leaked). Target is the dotted callable reference.
type pyInvokeCall struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Lineno int    `json:"lineno"`
	// Args carries from_name(app_name, obj_name)'s string literals (calque#87),
	// best-effort — a nil entry means that positional arg wasn't a plain string.
	// Empty for every other invoke kind.
	Args []*string `json:"args"`
	// Entrypoint mirrors pyMapCall.Entrypoint (calque#98) — the enclosing
	// @app.local_entrypoint()'s name, or "" if this call site isn't nested
	// inside one.
	Entrypoint string `json:"entrypoint"`
	// Iterable mirrors pyMapCall.Iterable (calque#136) — populated for "map"
	// and "starmap" kinds when pyast could statically resolve the call's
	// iterable argument; nil otherwise (and for every other Kind).
	Iterable *pyIterable `json:"iterable,omitempty"`
}

// Parse runs the helper on scriptPath and returns the IR plus any leaks emitted
// during transcription. runner/runnerArgs is how we invoke the helper (so callers
// and tests can point at `uv run ...`); see DefaultRunner.
func Parse(ctx context.Context, scriptPath string, rep *leak.Report, runner string, runnerArgs ...string) (ir.App, error) {
	out, err := runHelper(ctx, scriptPath, runner, runnerArgs...)
	if err != nil {
		return ir.App{}, err
	}
	return build(out, rep), nil
}

// SpawnCallSite is one .spawn() call site's target callable + best-effort
// string args (calque#112: the CLI-wiring glue a real spawnRun driver needs
// — pyInvokeCall.Args is captured at parse time per calque#88's own plan
// ("for the same future driver's eventual use — not consumed by anything
// yet") but wasn't exposed anywhere outside this package until now). Kept
// as its own exported type here, rather than exporting pyInvokeCall itself,
// so callers depend on a minimal stable shape instead of this package's
// full wire-format struct.
type SpawnCallSite struct {
	Target string
	Args   []*string
}

// SpawnCallSites re-runs the same pyast helper invocation as Parse and
// returns every .spawn() call site found in scriptPath — a sibling entry
// point, not an addition to Parse's own return shape, so every existing
// Parse caller (cmd/calque/run.go, cmd/calque/main.go's analyze) is
// unaffected. A caller that needs BOTH the ir.App and its spawn call sites
// currently pays for two helper invocations; this is acceptable for a
// design-time convenience function driving a real-AWS-verification-gated
// path (calque#112), not a hot loop.
func SpawnCallSites(ctx context.Context, scriptPath string, runner string, runnerArgs ...string) ([]SpawnCallSite, error) {
	out, err := runHelper(ctx, scriptPath, runner, runnerArgs...)
	if err != nil {
		return nil, err
	}
	var sites []SpawnCallSite
	for _, ic := range out.InvokeCalls {
		if ic.Kind != "spawn" {
			continue
		}
		sites = append(sites, SpawnCallSite{Target: leafName(ic.Target), Args: ic.Args})
	}
	return sites, nil
}

// runHelper invokes the pyast helper and decodes its JSON output — the
// shared subprocess-invocation logic behind both Parse and SpawnCallSites.
func runHelper(ctx context.Context, scriptPath string, runner string, runnerArgs ...string) (pyOut, error) {
	args := append(append([]string{}, runnerArgs...), scriptPath)
	cmd := exec.CommandContext(ctx, runner, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return pyOut{}, fmt.Errorf("pyast helper failed: %w (stderr: %s)", err, stderr.String())
	}

	var out pyOut
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return pyOut{}, fmt.Errorf("pyast JSON decode: %w", err)
	}
	if out.Error != "" {
		return pyOut{}, fmt.Errorf("pyast reported error: %s", out.Error)
	}
	return out, nil
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

	// calque#76: a function/class's image=<var> kwarg may reference a name the AST
	// walker never resolved to an Image chain (e.g. built via a factory function
	// rather than a direct `x = modal.Image....` assignment). resolveImage() above
	// silently picks whatever DID resolve in that case — loudly flag every such
	// dangling reference so the pick isn't mistaken for the function's real image.
	flagUnresolvedImageRefs(out, script, rep)

	// How is each callable invoked? (spec §13: "where .map() is called", §C: the
	// other sync idioms .starmap/.for_each/.remote). The target of a call like
	// `Chat().generate.map(...)` is the trailing attribute ("generate"); we key by
	// that leaf name to match the callable's Name.
	invokes, epInvokes := invocationKinds(out, script, rep)
	app.EntrypointInvokes = epInvokes

	// calque#136: leaf callable name -> real .map()/.starmap() iterable, when
	// pyast statically resolved it. Threaded into buildFn below so ir.Function.
	// Items carries the real item batch instead of always being nil.
	items := mapItems(out)

	for _, f := range out.Functions {
		app.Functions = append(app.Functions, buildFn(f, script, rep, invokes, items))
	}
	for _, c := range out.Classes {
		app.Classes = append(app.Classes, buildClass(c, script, rep, invokes, items))
	}
	for _, ep := range out.Entrypoints {
		app.Entrypoints = append(app.Entrypoints, buildFn(ep, script, rep, invokes, items))
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

// mapItems collects the real .map()/.starmap() iterable pyast statically
// resolved for each leaf callable name (calque#136), decoding pyIterable.
// Values from json.RawMessage to any. When a callable has more than one
// call site with a resolved iterable (rare — the same callable .map()'d at
// two different sites), the FIRST one found wins deterministically, mirroring
// resolveImage's own "first resolved wins, no ambiguity logic beyond that"
// posture; a callable with ANY unresolvable call site alongside a resolvable
// one still keeps the resolvable one, since that's the best real data
// available. A callable with only unresolvable call sites has no entry here,
// so buildFn leaves ir.Function.Items nil.
func mapItems(out pyOut) map[string][]any {
	items := map[string][]any{}
	add := func(target string, it *pyIterable) {
		if it == nil {
			return
		}
		leaf := leafName(target)
		if _, ok := items[leaf]; ok {
			return // first resolved wins
		}
		vals := make([]any, len(it.Values))
		for i, raw := range it.Values {
			var v any
			if err := json.Unmarshal(raw, &v); err == nil {
				vals[i] = v
			}
		}
		items[leaf] = vals
	}
	for _, mc := range out.MapCalls {
		add(mc.Target, mc.Iterable)
	}
	for _, ic := range out.InvokeCalls {
		if ic.Kind == "map" || ic.Kind == "starmap" {
			add(ic.Target, ic.Iterable)
		}
	}
	return items
}

// invocationKinds resolves each callable's invocation idiom from the helper's call
// sites. Precedence when a callable is invoked multiple ways: map > starmap >
// for_each > remote (the batch idioms dominate the spike's execution model). Async
// idioms (.spawn/.map.aio) are recognized by the helper and leaked as deferred
// (§C/M10-S2), never mapped to an executable kind here.
//
// It returns two views of the same call-site evidence (calque#98):
//   - invokes: the pre-existing whole-script-flat map (leaf callable name ->
//     best InvokeKind seen ANYWHERE in the script). Unchanged behavior — this
//     is still what buildFn/buildClass use to populate every Function.Invoke,
//     so a 0-or-1-entrypoint script (no ambiguity to resolve) sees exactly the
//     same result as before this change.
//   - epInvokes: the SAME evidence, but partitioned by which
//     @app.local_entrypoint()'s body the call site was nested inside (""
//     for a call site outside any entrypoint, e.g. module level). This lets a
//     caller (cmd/calque/run.go's pickWarmUnit) ask "what does entrypoint X
//     specifically invoke?" once a script has 2+ entrypoints and --entrypoint
//     disambiguates which one is in play.
func invocationKinds(out pyOut, script string, rep *leak.Report) (map[string]ir.InvokeKind, map[string]map[string]ir.InvokeKind) {
	// InvokeSpawn ranks lowest (calque#88): a callable that's BOTH .map()'d
	// somewhere and .spawn()'d elsewhere should still resolve to InvokeMap for
	// warm-unit selection — .spawn() classification exists so a future fan-out
	// driver can find spawned callables, not to compete with the idioms calque
	// actually executes.
	rank := map[ir.InvokeKind]int{
		ir.InvokeMap: 5, ir.InvokeStarmap: 4, ir.InvokeForEach: 3, ir.InvokeRemote: 2, ir.InvokeSpawn: 1,
	}
	invokes := map[string]ir.InvokeKind{}
	epInvokes := map[string]map[string]ir.InvokeKind{}
	consider := func(entrypoint, target string, kind ir.InvokeKind) {
		leaf := leafName(target)
		if rank[kind] > rank[invokes[leaf]] {
			invokes[leaf] = kind
		}
		if entrypoint == "" {
			return
		}
		m := epInvokes[entrypoint]
		if m == nil {
			m = map[string]ir.InvokeKind{}
			epInvokes[entrypoint] = m
		}
		if rank[kind] > rank[m[leaf]] {
			m[leaf] = kind
		}
	}
	// Back-compat: MapCalls is the pre-existing .map() channel.
	for _, mc := range out.MapCalls {
		consider(mc.Entrypoint, mc.Target, ir.InvokeMap)
	}
	// §C: the other synchronous idioms, plus async ones flagged for a deferred leak.
	for _, ic := range out.InvokeCalls {
		switch ic.Kind {
		case "map":
			consider(ic.Entrypoint, ic.Target, ir.InvokeMap)
		case "starmap":
			consider(ic.Entrypoint, ic.Target, ir.InvokeStarmap)
		case "for_each":
			consider(ic.Entrypoint, ic.Target, ir.InvokeForEach)
		case "remote":
			consider(ic.Entrypoint, ic.Target, ir.InvokeRemote)
		case "map.aio":
			// S2: async result futures / detach — deferred per §18; block-and-wait only.
			rep.Addf(leak.PrimMap, leak.KindSemanticGap, script, ic.Lineno,
				"%s.%s(...): async result futures / detach — deferred per §18; the spike is block-and-wait only", leafName(ic.Target), ic.Kind)
		case "spawn":
			// calque#88: .spawn() is now CLASSIFIED (ir.InvokeSpawn) so a future
			// block-and-wait fan-out driver can find every spawned callable — but
			// still not EXECUTED (§18 keeps calque block-and-wait-only). The leak
			// reflects that shift: "we know what this is" rather than "deferred,
			// unclassified."
			consider(ic.Entrypoint, ic.Target, ir.InvokeSpawn)
			rep.Addf(leak.PrimMap, leak.KindSemanticGap, script, ic.Lineno,
				"%s.spawn(...): classified but not executed — block-and-wait fan-out over distinct spawned callables is deferred per §18 (calque#97 tracks the driver)", leafName(ic.Target))
		case "local":
			// calque#81: .local() runs the callee inline in the caller's own
			// container — no new invocation, no serialization boundary. calque
			// ships only the ONE function/method picked as the warm unit,
			// verbatim; any OTHER function referenced via .local() is not in
			// scope in the worker process and will NameError at runtime if the
			// body it appears in is ever the one shipped. Leak loudly rather
			// than let that surface as a mysterious crash.
			rep.Addf(leak.PrimMap, leak.KindSemanticGap, script, ic.Lineno,
				"%s.local(...): runs inline in the caller's own container, not a separate warm unit — calque ships only the picked warm unit's body verbatim, so %s is NOT in scope here and this call will NameError unless %s is also inlined manually", leafName(ic.Target), leafName(ic.Target), leafName(ic.Target))
		case "from_name":
			// calque#87: Function.from_name(app, fn)/Cls.from_name(app, cls) look up
			// an ALREADY-DEPLOYED separate app by name — cross-app invocation, an
			// execution boundary calque doesn't own (calque parses+runs ONE script;
			// it has no notion of a separately-deployed app to call into). Recognize
			// and leak distinctly, naming the looked-up app/object when the args are
			// plain string literals, rather than let whatever's chained after
			// from_name(...) (.remote()/.spawn()/etc.) record a target-less,
			// unexplained invoke entry.
			rep.Addf(leak.PrimMap, leak.KindSemanticGap, script, ic.Lineno,
				"%s.from_name(%s): cross-app invocation of an already-deployed separate app — calque has no notion of a separately-deployed app to call into; not reproduced", ic.Target, formatFromNameArgs(ic.Args))
		}
	}
	return invokes, epInvokes
}

// formatFromNameArgs renders Function.from_name/Cls.from_name's positional
// args for a leak message, falling back to "?" for any non-string-literal arg
// (calque#87) rather than guessing at its value.
func formatFromNameArgs(args []*string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if a == nil {
			parts[i] = "?"
			continue
		}
		parts[i] = strconv.Quote(*a)
	}
	return strings.Join(parts, ", ")
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
	if src.Cloud != "" {
		dst.Cloud = src.Cloud
	}
	return dst
}

// decodeImageRef decodes a decorator's image=<var> kwarg, emitted by pyast as
// {"__ref__": "<varname>"} for a plain name reference. Returns ("", false) for
// any other shape (e.g. an inline chain or non-literal), which readConfigKwargs
// already leaks separately via the "image" no-op case.
func decodeImageRef(raw json.RawMessage) (string, bool) {
	var ref struct {
		Ref string `json:"__ref__"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil || ref.Ref == "" {
		return "", false
	}
	return ref.Ref, true
}

// flagUnresolvedImageRefs walks every function/class-level decorator's image=
// kwarg and leaks loudly when it names a variable that never resolved to an
// Image chain in out.Images (calque#76). Without this, resolveImage()'s pick
// (whatever chain DID resolve, possibly for a wholly different function) is
// silently substituted with no signal that the reference was dangling.
func flagUnresolvedImageRefs(out pyOut, script string, rep *leak.Report) {
	check := func(owner string, decos []pyDecorator) {
		for _, d := range decos {
			raw, ok := d.Kwargs["image"]
			if !ok {
				continue
			}
			ref, ok := decodeImageRef(raw)
			if !ok {
				continue // inline chain or non-literal; not this check's concern
			}
			if _, resolved := out.Images[ref]; !resolved {
				rep.Addf(leak.PrimImage, leak.KindSemanticGap, script, d.Lineno,
					"%s: image=%s did not resolve to a known Image chain (built via a factory function, or another pattern the AST walker doesn't see through); the app image picked above may NOT be %s's real image", owner, ref, owner)
			}
		}
	}
	for _, f := range out.Functions {
		check(f.Name, f.Decorators)
	}
	for _, c := range out.Classes {
		check(c.Name, classDecorators(c))
		for _, m := range c.Methods {
			check(c.Name+"."+m.Name, m.Decorators)
		}
	}
}

// classDecorators synthesizes a pyDecorator carrying a @cls's kwargs, so
// flagUnresolvedImageRefs can treat class-level and function-level image=
// checks uniformly. cls_kwargs has no decorator name/lineno of its own in the
// wire contract, so the class's own line stands in.
func classDecorators(c pyClass) []pyDecorator {
	if len(c.ClsKwargs) == 0 {
		return nil
	}
	return []pyDecorator{{Kwargs: c.ClsKwargs, Lineno: c.Lineno}}
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

func buildFn(f pyFunc, script string, rep *leak.Report, invokes map[string]ir.InvokeKind, items map[string][]any) ir.Function {
	fn := ir.Function{
		Name:      f.Name,
		Body:      f.Body,
		Line:      f.Lineno,
		Invoke:    invokes[f.Name],
		EntryKind: ir.EntryKind(f.EntryKind), // "serve" or "" (§F)
		Args:      f.Args,
		ItemArg:   firstItemArg(f.Args),
		Items:     items[f.Name],
	}
	for _, lc := range f.LocalCalls {
		fn.LocalCalls = append(fn.LocalCalls, leafName(lc))
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

func buildClass(c pyClass, script string, rep *leak.Report, invokes map[string]ir.InvokeKind, items map[string][]any) ir.Class {
	cls := ir.Class{Name: c.Name, Line: c.Lineno}
	gpu, vols, timeout, cfg := readConfigKwargs(c.ClsKwargs, leak.PrimGPU, c.Name, script, c.Lineno, rep)
	cls.GPU, cls.Volumes, cls.Timeout, cls.Config = gpu, vols, timeout, cfg
	if c.Enter != nil {
		cls.EnterBody = c.Enter.Body
		for _, lc := range c.Enter.LocalCalls {
			cls.EnterLocalCalls = append(cls.EnterLocalCalls, leafName(lc))
		}
	} else {
		// A @cls with no @enter still runs, but the warm-load-once economics (§6)
		// don't apply — worth noting since it changes the amortization story.
		rep.Addf(leak.PrimEnter, leak.KindUnhandledCase, script, c.Lineno,
			"@cls %q has no @enter; no warm load-once body to run", c.Name)
	}
	if c.Exit != nil {
		// calque#86: @modal.exit() runs ONCE at container shutdown — the warm
		// supervisor has no shutdown-hook concept, so this is not reproduced.
		// Excluded from cls.Methods (unlike before the fix, where it fell into
		// the generic "plain method" bucket and could be invoked per-item).
		cls.HasExit = true
		rep.Addf(leak.PrimEnter, leak.KindSemanticGap, script, c.Exit.Lineno,
			"@cls %q has @modal.exit(); container-shutdown teardown is not reproduced by the warm supervisor", c.Name)
	}
	for _, m := range c.Methods {
		method := buildFn(m, script, rep, invokes, items)
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
	// current spellings (Modal 1.0, v0.73.76+; real scripts contain either era,
	// calque#82): scaledown_window replaces container_idle_timeout,
	// buffer_containers was promoted from _experimental_buffer_containers.
	"scaledown_window": true, "buffer_containers": true,
	// @modal.concurrent(max_inputs=, target_inputs=) replaced the
	// allow_concurrent_inputs=N kwarg as a SEPARATE decorator (v0.73.148); pyast
	// folds its kwargs into the same kwargs map (cls_kwargs, or a function's own
	// decorator kwargs) so they reach this set like any other autoscaling knob.
	"max_inputs": true, "target_inputs": true,
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
			} else if lst, ok := decodeStringList(raw); ok && len(lst) > 0 {
				// calque#85: gpu=["H100", "A100-40GB:2"] is Modal's fallback-list
				// syntax — try each type in list order until one has capacity.
				// calque has no live-availability probe at parse time, so it can't
				// reproduce the fallback itself; take the FIRST (highest-preference)
				// entry as the single gpu= value and leak the rest as unreproduced,
				// rather than hit the generic "not a plain string literal" message.
				gpu = lst[0]
				if len(lst) > 1 {
					rep.Addf(leak.PrimGPU, leak.KindSemanticGap, script, line,
						"%s: gpu=%v fallback-list — using first preference %q; the list's try-in-order-until-available semantic is not reproduced (no live availability probe at parse time)", owner, lst, lst[0])
				}
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
			// cpu= is cores (int or float) in Modal; a [request, limit] list also
			// occurs (mirrors memory=) — take the request (first) element and leak
			// the limit we don't model.
			if f, ok := decodeFloat(raw); ok {
				cfg.CPU = f
			} else if lst, ok := decodeFloatList(raw); ok && len(lst) > 0 {
				cfg.CPU = lst[0]
				if len(lst) > 1 {
					rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
						"%s: cpu=[request,limit] — using request %g cores; the limit is not enforced", owner, lst[0])
				}
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
			// calque#91: schedule=modal.Cron(...)/modal.Period(...) object forms —
			// recognized structurally (via the __schedule__ marker pyast emits) rather
			// than falling into the generic __unparsed__ string mangling below.
			suffix := ""
			if s, ok := decodeString(raw); ok {
				cfg.Schedule = s
			} else if sched, ok := decodeScheduleMarker(raw); ok {
				cfg.Schedule = sched
				suffix = " (recognized from modal.Cron/Period object form)"
			} else {
				cfg.Schedule = string(raw)
			}
			rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
				"%s: schedule= recorded but NOT honored (no scheduler in the spike)%s", owner, suffix)
		case "region":
			if s, ok := decodeString(raw); ok {
				cfg.Region = s
			}
			rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
				"%s: region= placement hint recorded but NOT honored (acquisition sweeps offered AZs)", owner)
		case "cloud":
			// calque#91: cloud= picks AWS/GCP/OCI/auto — directly relevant to
			// calque's own translation story (a script already saying
			// cloud="aws" is telling you something calque should probably read),
			// but calque only ever targets AWS regardless of this value.
			if s, ok := decodeString(raw); ok {
				cfg.Cloud = s
			}
			rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, line,
				"%s: cloud= recorded but NOT honored (calque always targets AWS)", owner)
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

func decodeFloatList(raw json.RawMessage) ([]float64, bool) {
	var xs []float64
	if err := json.Unmarshal(raw, &xs); err == nil {
		return xs, true
	}
	return nil, false
}

// decodeStringList decodes a strict []string — unlike decodeStringListBestEffort,
// it does NOT treat a bare string as a 1-element list (callers checking gpu=
// already try decodeString first; this is only reached for the list case).
func decodeStringList(raw json.RawMessage) ([]string, bool) {
	var xs []string
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

// decodeScheduleMarker decodes the {"__schedule__": "Cron"|"Period", "args": [...],
// "kwargs": {...}} shape pyast emits for schedule=modal.Cron(...)/modal.Period(...)
// object forms (calque#91; see _schedule_marker in tools/pyast/pyast.py). Returns
// the normalized ir.Config.Schedule string, or ("", false) if raw isn't this shape
// (the caller falls back to the pre-existing bare-string/stringify handling).
//
//   - Cron: the first positional arg (the cron string) becomes Schedule verbatim.
//     timezone= (and any other kwarg) is discarded — the bare-string schedule= form
//     never carried timezone info either, so this doesn't regress that posture.
//   - Period: days=/hours=/minutes=/seconds= are ADDITIVE (Modal itself combines any
//     subset, e.g. days=1, hours=6 means "every 1 day 6 hours"), so they're summed
//     into one normalized string of the form "<n>d<n>h<n>m<n>s", omitting any unit
//     that is zero/absent (see the doc comment on ir.Config.Schedule for the exact
//     format contract). All-zero/absent kwargs (a malformed script) yield "".
func decodeScheduleMarker(raw json.RawMessage) (string, bool) {
	var m struct {
		Kind   string         `json:"__schedule__"`
		Args   []any          `json:"args"`
		Kwargs map[string]any `json:"kwargs"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Kind == "" {
		return "", false
	}
	switch m.Kind {
	case "Cron":
		if len(m.Args) == 0 {
			return "", true
		}
		s, _ := m.Args[0].(string)
		return s, true
	case "Period":
		num := func(k string) int {
			v, ok := m.Kwargs[k]
			if !ok {
				return 0
			}
			f, ok := v.(float64) // JSON numbers decode to float64 in an `any`
			if !ok {
				return 0
			}
			return int(f)
		}
		days, hours, minutes, seconds := num("days"), num("hours"), num("minutes"), num("seconds")
		var b strings.Builder
		if days != 0 {
			fmt.Fprintf(&b, "%dd", days)
		}
		if hours != 0 {
			fmt.Fprintf(&b, "%dh", hours)
		}
		if minutes != 0 {
			fmt.Fprintf(&b, "%dm", minutes)
		}
		if seconds != 0 {
			fmt.Fprintf(&b, "%ds", seconds)
		}
		return b.String(), true
	default:
		return "", false
	}
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
