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
	Script  string `json:"script"`
	AppName string `json:"app_name"`
	// AppKwargs holds App(volumes=..., secrets=...)'s own kwargs (calque#168)
	// — a function/class declaring neither inherits from here. image= is
	// deliberately absent: real per-function image RESOLUTION (not just an
	// app-level fallback) is a separately-tracked gap, see resolveImage.
	AppKwargs map[string]json.RawMessage `json:"app_kwargs"`
	Images    map[string]pyImage         `json:"images"`
	Volumes   map[string]pyVolume        `json:"volumes"`
	// NetworkFileSystems is every module-level modal.NetworkFileSystem.
	// from_name(...) var, keyed by var name (calque#91 Workstream B) —
	// mirrors Volumes' own wire shape exactly.
	NetworkFileSystems map[string]pyVolume `json:"network_file_systems"`
	Functions          []pyFunc            `json:"functions"`
	Classes            []pyClass           `json:"classes"`
	Entrypoints        []pyFunc            `json:"entrypoints"`
	MapCalls           []pyMapCall         `json:"map_calls"`
	InvokeCalls        []pyInvokeCall      `json:"invoke_calls"`
	VolumeWrites       []pyVolumeWrite     `json:"volume_writes"`
	HelperLeaks        []map[string]any    `json:"helper_leaks"`
	// ModuleConsts is every module-level `NAME = <literal-or-expression>`
	// assignment, keyed by name (calque#139) — the shippable half of
	// free-variable resolution besides Functions itself (a FreeRefs name may
	// resolve to either this map or a plain @app.function in Functions;
	// collectLocalExtras tries both, same as it already does for LocalCalls'
	// @cls-method-vs-plain-function distinction). Each entry also carries its
	// OWN free_refs (calque#146.2): a constant's RHS can itself reference an
	// import or another constant (e.g. `forecast_volume =
	// modal.Volume.from_name(...)` needs `import modal` shipped too) —
	// without this, collectLocalExtras' transitive walk stops AT a shipped
	// constant instead of continuing THROUGH it.
	ModuleConsts map[string]pyModuleConst `json:"module_consts"`
	// ModuleFuncs is EVERY module-level function (calque#139), decorated or
	// not — crucially including a plain, undecorated helper that never
	// becomes an @app.function and so never appears in Functions above. The
	// OTHER shippable free-reference target besides ModuleConsts.
	ModuleFuncs map[string]pyModuleFunc `json:"module_funcs"`
	// ModuleImports is the verbatim source of every module-level `import X`
	// / `from X import Y` statement, keyed by each name it binds (calque#146)
	// — the THIRD shippable free-reference target, alongside ModuleConsts/
	// ModuleFuncs. A re-exported name from ANOTHER module stays unresolved
	// (a leak, not shipped) — this only covers an import THIS script does
	// itself, which is unambiguous.
	ModuleImports map[string]string `json:"module_imports"`
	// ModuleClasses is every PLAIN (non-`@app.cls`) module-level class,
	// keyed by name (calque#147) — the FOURTH shippable free-reference
	// target, alongside ModuleConsts/ModuleFuncs/ModuleImports.
	ModuleClasses map[string]pyModuleConst `json:"module_classes"`
	Error         string                   `json:"error"`
}

// pyModuleFunc is one module-level function's shippable shape (calque#139):
// mirrors the subset of pyFunc a plain helper needs (no decorators/entry_kind
// — a bare helper has none of those by definition).
type pyModuleFunc struct {
	Args       []string `json:"args"`
	Body       string   `json:"body"`
	LocalCalls []string `json:"local_calls"`
	FreeRefs   []string `json:"free_refs"`
}

// pyModuleConst is one module-level constant's shippable shape (calque#146.2):
// its verbatim source plus its OWN free_refs — a constant's RHS can itself
// reference an import or another constant.
type pyModuleConst struct {
	Source   string   `json:"source"`
	FreeRefs []string `json:"free_refs"`
	// UnshippableConstruct is non-empty (e.g. "modal.Dict") when this
	// constant's RHS is a live-Modal-control-plane construct
	// (calque#151) — Dict/Queue/NetworkFileSystem.from_name(...). A bare
	// reference resolving here must NOT be shipped verbatim the way an
	// ordinary literal constant is: the runner has no live Modal
	// credentials, so exec'ing it crashes with a confusing SDK auth error
	// instead of an honest leak. "" for every ordinary constant.
	UnshippableConstruct string `json:"unshippable_construct"`
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
	// FreeRefs are bare (non-.local()-suffixed) references to a module-level
	// helper function or constant, found ANYWHERE inside this function/
	// method's own body (calque#139) — real Modal code overwhelmingly
	// references its own module globals this way, never via .local(), since
	// the helper was never registered as an @app.function to begin with. Each
	// name here is already resolved (by pyast's _free_refs, real scope
	// tracking — params/locals/loop-vars/comprehension-vars/nested defs all
	// shadow) to a genuine module-level def or simple assignment in the SAME
	// script; already bare (no dotted chain, unlike LocalCalls), since a
	// free-variable reference is never a call-site attribute chain.
	FreeRefs []string `json:"free_refs"`
	// IsClustered mirrors pyast's is_clustered (calque#152): true when any of
	// this function's decorators has trailing name "clustered" (i.e.
	// @modal.experimental.clustered(...)) — a decorator-level multi-node
	// request invisible to the §7 GPU guard's gpu= string parsing and
	// body-text coupling regex alike.
	IsClustered bool   `json:"is_clustered"`
	Body        string `json:"body"`
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
	// calque#91 Workstream B: module-level NetworkFileSystem.from_name(...)
	// vars, keyed by var name — mirrors app.Volumes' own population exactly.
	if len(out.NetworkFileSystems) > 0 {
		app.NetworkFileSystems = make(map[string]string, len(out.NetworkFileSystems))
		for name, v := range out.NetworkFileSystems {
			app.NetworkFileSystems[name] = v.FromName
		}
	}

	// calque#139: module-level constants + module-level functions (incl.
	// plain, undecorated helpers absent from out.Functions), the two
	// resolution targets collectLocalExtras (cmd/calque/run.go) consults for
	// each callable's FreeRefs/EnterFreeRefs alongside FindFunction.
	if len(out.ModuleConsts) > 0 {
		app.ModuleConsts = make(map[string]ir.ModuleConst, len(out.ModuleConsts))
		for name, mc := range out.ModuleConsts {
			app.ModuleConsts[name] = ir.ModuleConst{Source: mc.Source, FreeRefs: mc.FreeRefs, UnshippableConstruct: mc.UnshippableConstruct}
		}
	}
	if len(out.ModuleFuncs) > 0 {
		app.ModuleFuncs = make(map[string]ir.ModuleFunc, len(out.ModuleFuncs))
		for name, mf := range out.ModuleFuncs {
			localCalls := make([]string, 0, len(mf.LocalCalls))
			for _, lc := range mf.LocalCalls {
				localCalls = append(localCalls, leafName(lc))
			}
			app.ModuleFuncs[name] = ir.ModuleFunc{
				Args:       mf.Args,
				Body:       mf.Body,
				LocalCalls: localCalls,
				FreeRefs:   mf.FreeRefs,
			}
		}
	}
	// calque#146: module-level imports — the THIRD resolution target
	// collectLocalExtras consults, alongside ModuleConsts/ModuleFuncs.
	if len(out.ModuleImports) > 0 {
		app.ModuleImports = out.ModuleImports
	}
	// calque#147: plain (non-@app.cls) module-level classes — the FOURTH
	// resolution target.
	if len(out.ModuleClasses) > 0 {
		app.ModuleClasses = make(map[string]ir.ModuleClass, len(out.ModuleClasses))
		for name, mc := range out.ModuleClasses {
			app.ModuleClasses[name] = ir.ModuleClass{Source: mc.Source, FreeRefs: mc.FreeRefs}
		}
	}

	// Resolve the app-wide DEFAULT image — the fallback any callable with no
	// image= of its own inherits. Modal scripts commonly define exactly one
	// image var; if there are several, resolveImage takes the first
	// deterministically. calque#174: this is no longer what every callable
	// actually RUNS with — see resolveCallableImage below, applied per
	// function/class.
	app.Image = resolveImage(out, script, rep)

	// calque#76: a function/class's image=<var> kwarg may reference a name the AST
	// walker never resolved to an Image chain (e.g. built via a factory function
	// rather than a direct `x = modal.Image....` assignment). resolveImage() above
	// silently picks whatever DID resolve in that case — loudly flag every such
	// dangling reference so the pick isn't mistaken for the function's real image.
	flagUnresolvedImageRefs(out, script, rep)

	// calque#168: App(volumes=..., secrets=...)'s own kwargs — a Function/Class
	// declaring neither inherits from here (applied per-callable in buildFn/
	// buildClass below). Previously silently dropped with NO leak at all.
	app.DefaultVolumes, app.DefaultSecrets = resolveAppDefaults(out, script, rep)

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
		fn := buildFn(f, script, rep, invokes, items, app.NetworkFileSystems)
		applyAppDefaults(&fn.Volumes, &fn.Config.Secrets, app.DefaultVolumes, app.DefaultSecrets)
		fn.Image = resolveCallableImage(f.Decorators, out, app.Image, script, rep)
		app.Functions = append(app.Functions, fn)
	}
	for _, c := range out.Classes {
		app.Classes = append(app.Classes, buildClass(c, script, rep, invokes, items, app.DefaultVolumes, app.DefaultSecrets, out, app.Image, app.NetworkFileSystems))
	}
	for _, ep := range out.Entrypoints {
		// @app.local_entrypoint() runs LOCALLY, not in a container — App-level
		// volumes=/secrets=/image= inheritance doesn't apply here (nothing to
		// mount/inject/build for).
		app.Entrypoints = append(app.Entrypoints, buildFn(ep, script, rep, invokes, items, app.NetworkFileSystems))
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
			if app.CommittedVolumes == nil {
				app.CommittedVolumes = map[string]bool{}
			}
			app.CommittedVolumes[vw.Target] = true
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

// resolveAppDefaults decodes App(volumes=..., secrets=...)'s own kwargs
// (calque#168) — a Function/Class declaring neither inherits these via
// applyAppDefaults below. Before this, App-level volumes=/secrets= were
// silently dropped with NO leak at all (worse than App(image=), which at
// least surfaced a generic leak) — confirmed via a live repro:
// `modal.App("t", secrets=[...], volumes={...})` with a plain
// `@app.function()` declaring neither produced ZERO leaks.
func resolveAppDefaults(out pyOut, script string, rep *leak.Report) (volumes map[string]string, secrets []string) {
	if raw, ok := out.AppKwargs["volumes"]; ok {
		if m, ok := decodeStringMap(raw); ok {
			volumes = m
		} else {
			rep.Addf(leak.PrimVolume, leak.KindUnsupportedArg, script, 0,
				"App(volumes=...): not a {str:str} map (%s)", string(raw))
		}
	}
	if raw, ok := out.AppKwargs["secrets"]; ok {
		secrets = decodeStringListBestEffort(raw)
		rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, script, 0,
			"App(secrets=%s): app-level default secrets recorded but NOT injected in the spike, same as a function's own secrets= — a payload needing them will fail unless every function/class that inherits this default also gets a matching --secret NAME=VALUE", string(raw))
	}
	return volumes, secrets
}

// applyAppDefaults fills volumes/secrets from the App-level defaults ONLY
// when the callable (Function or Class) declares none of its own — the
// same fallback-if-own-is-empty shape buildClass already used for a
// method inheriting its class's gpu=/volumes=, extended one level up.
func applyAppDefaults(volumes *map[string]string, secrets *[]string, defaultVolumes map[string]string, defaultSecrets []string) {
	if *volumes == nil {
		*volumes = defaultVolumes
	}
	if len(*secrets) == 0 {
		*secrets = defaultSecrets
	}
}

// flagUnresolvedImageRefs walks every function/class-level decorator's image=
// kwarg and leaks loudly when it names a variable that never resolved to an
// Image chain in out.Images (calque#76). Without this, resolveCallableImage's
// fallback to the app-wide default (calque#174) — the best available
// substitute when a callable's OWN image= is unresolvable — would apply with
// no signal that the reference was dangling in the first place.
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
					"%s: image=%s did not resolve to a known Image chain (built via a factory function, or another pattern the AST walker doesn't see through); %s falls back to the app-wide default image, which may NOT be %s's real image", owner, ref, owner, owner)
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
	// lexicographically first. This is App.Image — the fallback a callable
	// with no image= of its own inherits (see resolveCallableImage); it is
	// NOT leaked as ambiguous here anymore (calque#174) — per-callable
	// resolution below means multiple DIFFERENT images across DIFFERENT
	// functions is the normal, correct case, not ambiguity to flag.
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
	return buildIRImage(out.Images[chosenName], script, rep)
}

// buildIRImage translates one resolved pyImage chain into ir.Image —
// factored out of resolveImage (calque#174) so resolveCallableImage below
// can build an ir.Image for a SPECIFIC callable's own image=<var>, not just
// the one app-wide pick.
func buildIRImage(pi pyImage, script string, rep *leak.Report) ir.Image {
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

// resolveCallableImage resolves ONE callable's own image=<var> kwarg against
// out.Images directly (calque#174) — the fix for resolveImage's pre-#174
// behavior of picking ONE image for the whole script regardless of who
// referenced it, which could silently hand a function a DIFFERENT
// function's image even when it declared its own explicit image=.
//
// decos is the callable's own decorator list (a function's Decorators, or
// classDecorators(c) for a class/method's cls_kwargs). Falls back to
// appImage (App.Image, or App.Image inherited via a class) when the
// callable declares no image= of its own —
// mirroring applyAppDefaults' fallback-if-own-is-empty shape for
// volumes=/secrets=. flagUnresolvedImageRefs (calque#76) already leaks
// loudly when a callable's own image=<var> never resolved to a chain in
// out.Images (e.g. built via a factory function) — this function silently
// falls back to appImage in that case too, matching the pre-#174 posture
// for an unresolvable reference (better to inherit SOMETHING than nothing,
// with the existing #76 leak already naming the problem).
func resolveCallableImage(decos []pyDecorator, out pyOut, appImage ir.Image, script string, rep *leak.Report) ir.Image {
	for _, d := range decos {
		raw, ok := d.Kwargs["image"]
		if !ok {
			continue
		}
		ref, ok := decodeImageRef(raw)
		if !ok {
			continue // inline chain or non-literal; not resolvable by name
		}
		if pi, resolved := out.Images[ref]; resolved {
			return buildIRImage(pi, script, rep)
		}
		break // dangling ref; #76's flagUnresolvedImageRefs already leaks this
	}
	return appImage
}

// nfsVarNames is app.NetworkFileSystems (var name -> from_name string) —
// threaded through buildFn/buildClass so a callable's own mount-path -> var
// name map (from readConfigKwargs' nfs return) can resolve through to the
// real Modal name, the same two-step indirection volumeSpecsForApp
// (cmd/calque/realrun.go) already reverses for Volumes.
func resolveNFSVarNames(mountToVar map[string]string, nfsVarNames map[string]string) map[string]ir.NetworkFileSystemMount {
	if len(mountToVar) == 0 {
		return nil
	}
	out := make(map[string]ir.NetworkFileSystemMount, len(mountToVar))
	for mountPath, varName := range mountToVar {
		out[mountPath] = ir.NetworkFileSystemMount{Name: nfsVarNames[varName]}
	}
	return out
}

func buildFn(f pyFunc, script string, rep *leak.Report, invokes map[string]ir.InvokeKind, items map[string][]any, nfsVarNames map[string]string) ir.Function {
	fn := ir.Function{
		Name:        f.Name,
		Body:        f.Body,
		Line:        f.Lineno,
		Invoke:      invokes[f.Name],
		EntryKind:   ir.EntryKind(f.EntryKind), // "serve" or "" (§F)
		Args:        f.Args,
		ItemArg:     firstItemArg(f.Args),
		Items:       items[f.Name],
		IsClustered: f.IsClustered,
	}
	for _, lc := range f.LocalCalls {
		fn.LocalCalls = append(fn.LocalCalls, leafName(lc))
	}
	// calque#139: FreeRefs are already bare names (never a dotted call-target
	// chain, unlike LocalCalls), so no leafName resolution is needed here.
	fn.FreeRefs = f.FreeRefs
	fn.IsMap = fn.Invoke == ir.InvokeMap // back-compat: IsMap tracks the .map() idiom
	// The function-config decorator is the one named "*.function" (or "*.method"
	// for class methods); enter/method markers carry no gpu/volumes.
	for _, d := range f.Decorators {
		gpu, vols, cbm, nfs, timeout, cfg := readConfigKwargs(d.Kwargs, leak.PrimGPU, f.Name, script, d.Lineno, rep)
		if gpu != "" {
			fn.GPU = gpu
		}
		if vols != nil {
			fn.Volumes = vols
		}
		if cbm != nil {
			fn.CloudBucketMounts = cbm
		}
		if nfs != nil {
			fn.NetworkFileSystems = resolveNFSVarNames(nfs, nfsVarNames)
		}
		if timeout != 0 {
			fn.Timeout = timeout
		}
		fn.Config = mergeConfig(fn.Config, cfg)
	}
	return fn
}

func buildClass(c pyClass, script string, rep *leak.Report, invokes map[string]ir.InvokeKind, items map[string][]any, defaultVolumes map[string]string, defaultSecrets []string, out pyOut, appImage ir.Image, nfsVarNames map[string]string) ir.Class {
	cls := ir.Class{Name: c.Name, Line: c.Lineno}
	gpu, vols, cbm, nfs, timeout, cfg := readConfigKwargs(c.ClsKwargs, leak.PrimGPU, c.Name, script, c.Lineno, rep)
	cls.GPU, cls.Volumes, cls.CloudBucketMounts, cls.Timeout, cls.Config = gpu, vols, cbm, timeout, cfg
	cls.NetworkFileSystems = resolveNFSVarNames(nfs, nfsVarNames)
	// calque#168: App-level volumes=/secrets= inherited if the CLASS itself
	// declares none — before a method's own class->method fallback below.
	applyAppDefaults(&cls.Volumes, &cls.Config.Secrets, defaultVolumes, defaultSecrets)
	// calque#174: the class's own image=<var> kwarg (cls_kwargs), falling
	// back to the App-wide default when the class declares none of its own.
	cls.Image = resolveCallableImage(classDecorators(c), out, appImage, script, rep)
	if c.Enter != nil {
		cls.EnterBody = c.Enter.Body
		for _, lc := range c.Enter.LocalCalls {
			cls.EnterLocalCalls = append(cls.EnterLocalCalls, leafName(lc))
		}
		cls.EnterFreeRefs = c.Enter.FreeRefs // calque#139: already bare names
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
		if c.Exit.Name == "__exit__" && len(c.Exit.Decorators) == 0 {
			// calque#138: recognized via the bare __exit__(self, exc_type,
			// exc_value, traceback) dunder — Modal's pre-@modal.exit() class-
			// lifecycle API. No decorator is present in the script at all, so
			// the @modal.exit() wording above would misdescribe what's on the
			// page; same non-reproduced posture either way.
			rep.Addf(leak.PrimEnter, leak.KindSemanticGap, script, c.Exit.Lineno,
				"@cls %q has legacy __exit__ (pre-@modal.exit() Modal API); container-shutdown teardown is not reproduced by the warm supervisor", c.Name)
		} else {
			rep.Addf(leak.PrimEnter, leak.KindSemanticGap, script, c.Exit.Lineno,
				"@cls %q has @modal.exit(); container-shutdown teardown is not reproduced by the warm supervisor", c.Name)
		}
	}
	for _, m := range c.Methods {
		method := buildFn(m, script, rep, invokes, items, nfsVarNames)
		// A class method inherits the class's gpu/volumes/secrets/image if it
		// declares none of its own (calque#168/#174 extend this existing
		// pattern one level up: the class itself may have already inherited
		// from the App). A method's own image= kwarg (rare — @modal.method
		// doesn't accept image= on real Modal, but resolveCallableImage
		// handles the "declares none" case identically either way) resolves
		// against the class's Image, so App->class->method chains correctly.
		if method.GPU == "" {
			method.GPU = cls.GPU
		}
		if method.Volumes == nil {
			method.Volumes = cls.Volumes
		}
		if method.CloudBucketMounts == nil {
			method.CloudBucketMounts = cls.CloudBucketMounts
		}
		if method.NetworkFileSystems == nil {
			method.NetworkFileSystems = cls.NetworkFileSystems
		}
		if len(method.Config.Secrets) == 0 {
			method.Config.Secrets = cls.Config.Secrets
		}
		method.Image = resolveCallableImage(m.Decorators, out, cls.Image, script, rep)
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
// anything else it can't model becomes a generic leak (§10). cbm is calque#91
// Workstream A's real CloudBucketMount->S3-mount resolution, alongside the
// pre-existing plain-Volume vols map — see decodeVolumesAndCloudBucketMounts.
// nfs is calque#91 Workstream B's mount-path -> NetworkFileSystem VAR NAME map
// (mirroring vols' own mount-path -> Volume var name shape exactly) — see
// decodeNetworkFileSystems; the caller (buildFn/buildClass) resolves the var
// name through app.NetworkFileSystems to the real from_name string.
func readConfigKwargs(kwargs map[string]json.RawMessage, _ leak.Primitive, owner, script string, line int, rep *leak.Report) (gpu string, vols map[string]string, cbm map[string]ir.CloudBucketMount, nfs map[string]string, timeout int, cfg ir.Config) {
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
			vols, cbm = decodeVolumesAndCloudBucketMounts(raw, owner, script, line, rep)
		case "network_file_systems":
			// calque#91 Workstream B: a SEPARATE decorator kwarg from volumes=
			// (never nested inside it, unlike CloudBucketMount) — see
			// decodeNetworkFileSystems.
			nfs = decodeNetworkFileSystems(raw, owner, script, line, rep)
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
	return gpu, vols, cbm, nfs, timeout, cfg
}

// decodeVolumesAndCloudBucketMounts decodes a volumes= kwarg's raw JSON into
// its two disjoint per-mount-path shapes (calque#91 Workstream A): the
// pre-existing plain-string {mount_path: volume_var_name} map (vols, an
// ordinary modal.Volume.from_name(...) mount) and the new
// {mount_path: {"__cloud_bucket_mount__": {...}}} shape pyast emits for a
// modal.CloudBucketMount(...) call used inline as a volumes= value (cbm, a
// real S3 mount). A raw value that's neither — including pyast's own
// "recognized but not modeled" __unparsed__ fallback for a CloudBucketMount
// whose bucket_name wasn't a string literal, or any OTHER unmodeled
// construct — is silently absent from both maps; that specific case already
// gets its own leak from pyast's helper_leaks (surfaced separately in
// build(), see the "pyast helper flagged" leak), so no second, redundant
// leak is emitted here. A volumes= value that isn't even a JSON object at
// all (e.g. a bare unparseable expression) leaks exactly like before this
// change.
func decodeVolumesAndCloudBucketMounts(raw json.RawMessage, owner, script string, line int, rep *leak.Report) (map[string]string, map[string]ir.CloudBucketMount) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		rep.Addf(leak.PrimVolume, leak.KindUnsupportedArg, script, line,
			"%s: volumes= not a {str:str} map (%s)", owner, string(raw))
		return nil, nil
	}
	var vols map[string]string
	var cbm map[string]ir.CloudBucketMount
	for mountPath, rawVal := range m {
		if s, ok := decodeString(rawVal); ok {
			if vols == nil {
				vols = map[string]string{}
			}
			vols[mountPath] = s
			continue
		}
		if mount, ok := decodeCloudBucketMount(rawVal); ok {
			if cbm == nil {
				cbm = map[string]ir.CloudBucketMount{}
			}
			cbm[mountPath] = mount
		}
		// Neither shape: pyast's own helper_leaks already named this (see doc
		// comment above) — no redundant leak here.
	}
	if vols == nil && cbm == nil {
		rep.Addf(leak.PrimVolume, leak.KindUnsupportedArg, script, line,
			"%s: volumes= not a {str:str} map (%s)", owner, string(raw))
	}
	return vols, cbm
}

// decodeCloudBucketMount decodes one volumes= dict value's
// {"__cloud_bucket_mount__": {"bucket_name": ..., "key_prefix": ...,
// "read_only": ...}} shape (calque#91 Workstream A; see pyast.py's
// _cloud_bucket_mount) into ir.CloudBucketMount. Returns (_, false) for any
// other shape, including a plain string (an ordinary Volume mount, handled
// by the caller instead) and pyast's {"__unparsed__": ...} fallback marker.
func decodeCloudBucketMount(raw json.RawMessage) (ir.CloudBucketMount, bool) {
	var wrapper struct {
		CBM *struct {
			BucketName string `json:"bucket_name"`
			KeyPrefix  string `json:"key_prefix"`
			ReadOnly   bool   `json:"read_only"`
		} `json:"__cloud_bucket_mount__"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || wrapper.CBM == nil || wrapper.CBM.BucketName == "" {
		return ir.CloudBucketMount{}, false
	}
	return ir.CloudBucketMount{
		BucketName: wrapper.CBM.BucketName,
		KeyPrefix:  wrapper.CBM.KeyPrefix,
		ReadOnly:   wrapper.CBM.ReadOnly,
	}, true
}

// decodeNetworkFileSystems decodes a network_file_systems= kwarg's raw JSON
// into the {mount_path: nfs_var_name} map (calque#91 Workstream B) — mirrors
// decodeVolumesAndCloudBucketMounts' plain-string vols half exactly, since
// pyast's _network_file_systems_map emits the identical
// {mount_path: var_name} shape _volumes_map already emits for an ordinary
// Volume mount (a NetworkFileSystem is always assigned to a variable first,
// never constructed inline the way CloudBucketMount is, so there is no
// second, distinguishable wire shape to decode here). A raw value that isn't
// even a JSON object at all leaks; an individual mount path whose value isn't
// a plain string (pyast's own ast.unparse(...) fallback for an unresolvable
// expression) is silently absent from the returned map — pyast's own
// helper_leaks already named that case when it recognized the construct (see
// _network_file_systems_map), so no second, redundant leak is emitted here.
func decodeNetworkFileSystems(raw json.RawMessage, owner, script string, line int, rep *leak.Report) map[string]string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		rep.Addf(leak.PrimVolume, leak.KindUnsupportedArg, script, line,
			"%s: network_file_systems= not a {str:str} map (%s)", owner, string(raw))
		return nil
	}
	var nfs map[string]string
	for mountPath, rawVal := range m {
		if s, ok := decodeString(rawVal); ok {
			if nfs == nil {
				nfs = map[string]string{}
			}
			nfs[mountPath] = s
		}
	}
	if nfs == nil {
		rep.Addf(leak.PrimVolume, leak.KindUnsupportedArg, script, line,
			"%s: network_file_systems= not a {str:str} map (%s)", owner, string(raw))
	}
	return nfs
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

// unresolvedArgPlaceholder fills an unresolved arg's POSITION in the output
// for a positionalArgMethods method (rather than omitting it), so
// localCopyArgs's (src, dst) positional read never misreads a LATER arg
// (e.g. the real destination path) as an earlier one (the source) just
// because an earlier one silently vanished (calque#180). Deliberately an
// obviously-invalid path fragment: if something downstream ever DOES render
// it verbatim (it shouldn't — every caller of stringifyArgs already leaks on
// this same non-literal arg via the __unparsed__/non-string branches below),
// a real `docker build` fails loudly on a nonsense path instead of silently
// copying the wrong file to the wrong place.
const unresolvedArgPlaceholder = "<<unresolved>>"

// positionalArgMethods are image-step verbs whose args have PAIRED positional
// meaning — add_local_*/copy_local_*'s (src, dst) — as opposed to
// pip_install/apt_install/run_commands/etc., whose args are each independent
// (dropping one unresolved package/command from an otherwise-fine list is
// harmless; there's no "position 2" that means something different once
// position 1 vanishes). Scoped narrowly to preserve every OTHER method's
// existing, already-tested "drop the unresolved one, keep the rest" behavior
// unchanged (calque#180) — only these methods get position-preserving
// placeholders instead.
var positionalArgMethods = map[string]bool{
	"add_local_dir": true, "add_local_file": true, "add_local_python_source": true,
	"copy_local_dir": true, "copy_local_file": true,
}

// stringifyArgs coerces image-step args to strings, leaking any non-string arg
// (e.g. an __unparsed__ marker dict) so it isn't silently dropped from the
// build. For a positionalArgMethods verb, every arg keeps its ORIGINAL
// POSITION in the output — an unresolved arg becomes unresolvedArgPlaceholder
// rather than being omitted, since omitting one shifts every later arg's
// index (calque#180: add_local_file's non-literal SOURCE arg being dropped
// entirely made its literal DESTINATION arg look like the source to
// localCopyArgs's positional (src, dst) read). Every other method keeps
// today's drop-on-unresolved behavior unchanged.
func stringifyArgs(args []any, method, script string, rep *leak.Report) []string {
	positional := positionalArgMethods[method]
	out := make([]string, 0, len(args))
	if positional {
		out = make([]string, len(args))
	}
	for i, a := range args {
		switch v := a.(type) {
		case string:
			if positional {
				out[i] = v
			} else {
				out = append(out, v)
			}
		case map[string]any:
			if positional {
				out[i] = unresolvedArgPlaceholder
			}
			if u, ok := v["__unparsed__"]; ok {
				rep.Addf(leak.PrimImage, leak.KindUnsupportedArg, script, 0,
					"image step .%s(...) has a non-literal arg: %v", method, u)
			}
		default:
			if positional {
				out[i] = unresolvedArgPlaceholder
			}
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
