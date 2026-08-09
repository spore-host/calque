package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spore-host/calque/internal/cost"
	"github.com/spore-host/calque/internal/gpu"
	"github.com/spore-host/calque/internal/image"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/measure"
	"github.com/spore-host/calque/internal/parse"
	"github.com/spore-host/calque/internal/plan"
	"github.com/spore-host/calque/internal/target"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// runOpts controls a `calque run` invocation.
type runOpts struct {
	script     string
	n          int
	region     string
	dryRun     bool // exercise every stage WITHOUT launching a billable instance
	ratesFP    string
	entrypoint string // calque#90: --entrypoint <name>; "" => auto-select if unambiguous
}

// run wires the full pipeline (spec §3). In --dry-run it stops short of the one
// billable action (instance acquisition) and instead drives the warm worker
// LOCALLY over a small synthetic sample, so the crossover K is produced end-to-end
// with its inputs honestly flagged measured|proxy. This is the "build up to
// launch, then pause" boundary made runnable.
func run(o runOpts) error {
	ctx := context.Background()
	rep := &leak.Report{}
	runner, runnerArgs := parse.DefaultRunner(pyastDir())

	// 1. parse -> IR
	app, err := parse.Parse(ctx, o.script, rep, runner, runnerArgs...)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	fmt.Printf("parsed %q: %d classes, %d functions\n", app.Name, len(app.Classes), len(app.Functions))

	// calque#90: mimics `modal run file.py::entrypoint` — validates --entrypoint
	// against app.Entrypoints (or requires it when the choice is ambiguous) and
	// reports which one is in play. calque#98: epName is now also threaded into
	// pickWarmUnit below, which restricts its scan to invoke-kind evidence
	// attributed to epName's own body once the script has 2+ entrypoints — see
	// entrypointScoped/app.EntrypointInvokes.
	epName, err := resolveEntrypoint(app, o.entrypoint)
	if err != nil {
		return err
	}
	if epName != "" {
		fmt.Printf("entrypoint: %s (selected)\n", epName)
	}

	// pick the mapped warm unit (a @cls with @enter whose method is .map'd),
	// scoped to epName's own body when the script has 2+ entrypoints (calque#98).
	unit, ok := pickWarmUnit(app, epName)
	if !ok {
		// F2: a serve-shaped app (long-lived, request-driven) has no batch .map warm
		// unit. Don't crash — emit a semantic-gap leak explaining the deferred shape,
		// and (below) still let the Bedrock gate route a served model away.
		if serveApp(app) {
			rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, app.Script, 0,
				"serve shape: long-lived request-driven entrypoint(s), not batch .map — execution shape deferred (§F/§16; the spike measures batch+K). A served Bedrock model still routes away below.")
			if printOffersAndStop(bedrockOffers(ctx, app, rep)) {
				fmt.Println("\n--- leak report (§10) ---")
				rep.Summary(os.Stdout)
				return nil
			}
			fmt.Println("serve entrypoint(s) detected; batch execution not applicable — see leak report.")
			fmt.Println("\n--- leak report (§10) ---")
			rep.Summary(os.Stdout)
			return nil
		}
		return fmt.Errorf("no mapped @cls+@enter warm unit found in %s (spike targets map_batch shape)", o.script)
	}
	fmt.Printf("warm unit: class %q, method %q, gpu asked-for %q\n", unit.class.Name, unit.method.Name, unit.class.GPU)
	if unit.plainFunction {
		// calque#80: a plain @app.function has no @enter — no once-per-container
		// load to amortize across items. Any K computed against it measures
		// something different than a warm @cls+@enter unit's reuse economics;
		// say so rather than silently reporting a K that reads the same as one.
		if len(unit.method.Volumes) > 0 {
			// calque#124: this function has no @enter, but it DOES mount a
			// Volume — VolumeSync's delta-only `aws s3 sync` (internal/plan/volume.go)
			// still avoids re-downloading cached weights across separate runs, even
			// though there's no per-item in-memory reuse within this one run. Say
			// both things, rather than let the @enter-less leak read as "no caching
			// at all here."
			rep.Addf(leak.PrimEnter, leak.KindSemanticGap, app.Script, unit.method.Line,
				"%s: plain @app.function, no @cls+@enter — no per-item warm-reuse economics within this run; K here measures a different thing than a @cls+@enter warm unit's K. It DOES mount a Volume, though: VolumeSync's delta sync still avoids re-downloading cached weights across separate runs, even without @enter's in-memory reuse", unit.method.Name)
		} else {
			rep.Addf(leak.PrimEnter, leak.KindSemanticGap, app.Script, unit.method.Line,
				"%s: plain @app.function, no @cls+@enter — no warm-reuse economics to amortize across items; K here measures a different thing than a @cls+@enter warm unit's K", unit.method.Name)
		}
	}
	// calque#83: the warm runner only binds ONE positional arg per item (§6
	// protocol) and always collects+returns a result. .starmap/.for_each are
	// classified at the IR layer but were silently run as if .map'd — check
	// explicitly rather than let the mismatch surface as a mystery failure.
	if err := checkInvokeSupport(app.Script, unit.method, rep); err != nil {
		return err
	}

	// 2a. route-away gate (§11, G3): before recommend/acquire, check whether this
	// model is already an exact Bedrock API call. If so, renting a GPU is the wrong
	// answer — emit the offer and stop. This is the credibility short-circuit; it
	// runs on the runnable path, not just analyze.
	if printOffersAndStop(bedrockOffers(ctx, app, rep)) {
		fmt.Println("\n--- leak report (§10) ---")
		rep.Summary(os.Stdout)
		return nil
	}

	// 2. gpu guard (§7): the swap must be legal or we refuse.
	glog := gpu.RewriteApp(app, rep)
	askedCard := gpu.ParseSpec(unit.class.GPU).Card
	if !swapLegal(glog, unit.class.Name) {
		return fmt.Errorf("gpu= swap for %q is FLAGGED (multi-GPU or coupled); out of single-node scope — see leak report", unit.class.Name)
	}

	// 3. recommend (STUB seam, §4): card -> Target, no logic.
	tgt := target.StubRecommender{}.Recommend(app, unit.method)

	// 4. plan: truffle card -> instance (offline), guard the .* fallback.
	if err := plan.FillTarget(&tgt, plan.NewTruffleResolver(rep)); err != nil {
		return fmt.Errorf("plan/resolve: %w", err)
	}
	fmt.Printf("recommend+resolve: card=%q -> instance=%q\n", tgt.Card, tgt.Instance)

	// B3: honor the portable config we can, record+leak what belongs behind the
	// seam. cpu=/memory= are SIZING decisions — the real right-sizing brain lives
	// behind the seam (§4), so we keep the pick dumb and only NOTE the request
	// rather than acting on it. retries= is a genuine reliability knob and is wired
	// into the warm supervisor's re-drive cap below.
	cfg := unit.class.Config
	if cfg.CPU != 0 || cfg.MemoryMB != 0 {
		rep.Addf(leak.PrimEntrypoint, leak.KindSemanticGap, app.Script, unit.class.Line,
			"%s: cpu=%.0f memory=%dMB requested — recorded, but instance sizing is behind the seam (§4); the pick stays dumb (%s)",
			unit.class.Name, cfg.CPU, cfg.MemoryMB, tgt.Instance)
	}

	// 4b. image: .image DSL -> Dockerfile -> digest (build/push deferred to real run).
	df, err := image.Render(image.Spec{Image: app.Image, WorkerDir: "/opt/calque"}, app.Script, rep)
	if err != nil {
		return fmt.Errorf("image: %w", err)
	}
	fmt.Printf("image: Dockerfile rendered, digest=%s (tag for ECR cache)\n", image.Digest(df))

	// 5-7. exec + measure: dry-run drives the warm worker locally; real run acquires.
	var m measure.Measurement
	m.CardAskedFor = askedCard
	m.InstanceUsed = tgt.Instance

	if o.dryRun {
		fmt.Println("\n[DRY-RUN] not launching a billable instance; driving warm worker locally on a synthetic sample")
		if err := dryRunWarm(ctx, app, unit, o.n, &m, rep); err != nil {
			return fmt.Errorf("dry-run warm: %w", err)
		}
	} else {
		return fmt.Errorf("real run (acquire+spawn) is gated: launch not yet authorized in this build path")
	}

	// 8. cost + crossover K (§9)
	rates, err := cost.LoadRates(o.ratesFP)
	if err != nil {
		return fmt.Errorf("rates: %w", err)
	}
	occ, occMeasured := m.OccupancyFraction()
	_, awsMeasured, _ := rates.AWSOnDemandPerSecond(tgt.Instance)
	model := &cost.Model{Rates: rates, M: cost.Measured{
		CardAskedFor: m.CardAskedFor, InstanceUsed: m.InstanceUsed,
		SecPerItem: m.PerItem.MeanSecs, Occupancy: occ, SampleItems: m.PerItem.Count,
		AWSRateMeasured: awsMeasured, AcquireSeconds: m.AcquireWaitSeconds, EnterSeconds: m.EnterSeconds,
	}}
	fmt.Println("\n--- crossover K (§9) ---")
	verdict, err := model.Verdict(o.n)
	switch {
	case err == cost.ErrNoComputeMeasured:
		fmt.Println("K is UNDEFINED: per-item compute is ~0 (trivial stand-in). Run on a real instance for a meaningful K.")
	case err != nil:
		return fmt.Errorf("cost: %w", err)
	default:
		fmt.Print(verdict)
	}
	if o.dryRun {
		fmt.Println("\n*** DRY-RUN K IS NOT DEFENSIBLE ***")
		fmt.Println("Per-item seconds and occupancy are SYNTHETIC (stand-in body, no GPU). A K that")
		fmt.Println("survives a hostile read requires the real payload on an acquired RTX PRO 6000 (§16.1).")
	} else if !occMeasured {
		fmt.Println("NOTE: occupancy was NOT measured — K's occupancy input is a proxy.")
	}

	fmt.Println("\n--- leak report (§10) ---")
	rep.Summary(os.Stdout)
	return nil
}

// resolveEntrypoint validates --entrypoint against app.Entrypoints, mimicking
// modal run file.py::entrypoint's selection (calque#90). Returns the selected
// name ("" if the script has none at all — nothing to select, not an error).
// Errors if a requested name doesn't exist, or if the choice is ambiguous
// (2+ entrypoints, none specified) — matching Modal's own "pick one" posture
// rather than silently running whichever pickWarmUnit happens to find.
func resolveEntrypoint(app ir.App, requested string) (string, error) {
	if len(app.Entrypoints) == 0 {
		if requested != "" {
			return "", fmt.Errorf("--entrypoint %q requested but %s has no @app.local_entrypoint()", requested, app.Script)
		}
		return "", nil
	}
	if requested != "" {
		for _, ep := range app.Entrypoints {
			if ep.Name == requested {
				return requested, nil
			}
		}
		names := make([]string, len(app.Entrypoints))
		for i, ep := range app.Entrypoints {
			names[i] = ep.Name
		}
		return "", fmt.Errorf("--entrypoint %q not found in %s; available: %s", requested, app.Script, strings.Join(names, ", "))
	}
	if len(app.Entrypoints) == 1 {
		return app.Entrypoints[0].Name, nil
	}
	names := make([]string, len(app.Entrypoints))
	for i, ep := range app.Entrypoints {
		names[i] = ep.Name
	}
	return "", fmt.Errorf("%s has multiple entrypoints (%s); pass --entrypoint to select one", app.Script, strings.Join(names, ", "))
}

// warmUnit is the callable calque actually drives through the warm supervisor:
// either a @cls with @enter and a .map'd @method (the spike's original target
// shape), or — calque#79/#80 — a plain @app.function with no @cls at all, which
// real-world Modal code uses roughly 2x as often (Pass 3 frequency survey,
// docs/modal-compatibility-matrix.md §B/§G). A plain function has no once-per-
// container load, so `class` is a synthesized zero-value ir.Class (EnterBody ""
// — Runner.enter() treats an empty body as a no-op) wrapping the function as its
// sole "method". plainFunction records this so callers can leak the missing
// warm-reuse economics rather than silently reporting a K that means something
// different than it does for a real warm unit.
type warmUnit struct {
	class         ir.Class
	method        ir.Function
	plainFunction bool
}

// entrypointScoped reports whether pickWarmUnit should restrict its scan to
// invoke-kind evidence attributed to epName specifically (calque#98), rather
// than fall back to the pre-existing whole-script scan. Scoping only kicks in
// when there's a genuine choice to disambiguate: a script with 0 or 1
// entrypoints has no ambiguity — a single entrypoint's own body already IS
// the whole script's relevant call-site evidence, and a script with none has
// no body to scope to at all — so both must keep running the original,
// unscoped scan exactly as before this change (the regression guard for
// today's common case).
func entrypointScoped(app ir.App, epName string) bool {
	return epName != "" && len(app.Entrypoints) >= 2
}

// pickWarmUnit selects the callable calque actually drives through the warm
// supervisor. epName is the --entrypoint selection already resolved by
// resolveEntrypoint (calque#90); when the script is entrypointScoped (2+
// entrypoints, one selected), the scan is restricted to invoke-kind evidence
// attributed to epName's OWN body (app.EntrypointInvokes, calque#98) — so
// `--entrypoint do_evaluate` picks the callable do_evaluate itself invokes,
// not whichever unrelated callable happens to be .map()'d somewhere else in
// the script. Otherwise (0 or 1 entrypoints) this is the original whole-
// script scan, unchanged.
func pickWarmUnit(app ir.App, epName string) (warmUnit, bool) {
	scoped := entrypointScoped(app, epName)
	var epEvidence map[string]ir.InvokeKind
	if scoped {
		epEvidence = app.EntrypointInvokes[epName]
	}

	for _, c := range app.Classes {
		if c.EnterBody == "" {
			continue
		}
		methods := c.Methods
		if scoped {
			methods = nil
			for _, mth := range c.Methods {
				if _, ok := epEvidence[mth.Name]; ok {
					methods = append(methods, mth)
				}
			}
			if len(methods) == 0 {
				// epName's own body never invokes any method on this class —
				// not a candidate for this entrypoint's warm unit at all.
				continue
			}
		}
		for _, mth := range methods {
			isMap := mth.IsMap
			if scoped {
				isMap = epEvidence[mth.Name] == ir.InvokeMap
			}
			if isMap {
				return warmUnit{class: c, method: mth}, true
			}
		}
		// fall back to the first (candidate) method if none is explicitly .map'd
		if len(methods) > 0 {
			return warmUnit{class: c, method: methods[0]}, true
		}
	}
	// No @cls+@enter unit — prefer a plain function that's .map'd (closest
	// analog to the class-based shape: many items through one warm process),
	// else fall back to the first plain function (single-call replay, §G).
	fns := app.Functions
	if scoped {
		fns = nil
		for _, f := range app.Functions {
			if _, ok := epEvidence[f.Name]; ok {
				fns = append(fns, f)
			}
		}
	}
	for _, f := range fns {
		isMap := f.IsMap
		if scoped {
			isMap = epEvidence[f.Name] == ir.InvokeMap
		}
		if isMap {
			return warmUnit{class: syntheticClass(f), method: f, plainFunction: true}, true
		}
	}
	if len(fns) > 0 {
		f := fns[0]
		return warmUnit{class: syntheticClass(f), method: f, plainFunction: true}, true
	}
	return warmUnit{}, false
}

// syntheticClass wraps a plain @app.function's config in a zero-value ir.Class
// so the rest of run.go (which reads unit.class.* throughout) doesn't need a
// separate code path for the plain-function case.
func syntheticClass(f ir.Function) ir.Class {
	return ir.Class{Name: f.Name, GPU: f.GPU, Config: f.Config, Line: f.Line}
}

// collectLocalExtras resolves the transitive closure of sibling functions the
// picked warm unit's own body references via .local() (calque#92) — real
// idiomatic Modal code (confirmed on AI-Almanac's blending_app.py, calque#79)
// chains plain @app.functions together this way, and calque previously shipped
// only the picked unit's own body, guaranteeing a NameError.
//
// Only plain @app.function targets are resolved and shipped. A .local() call
// that resolves to a @cls METHOD is deliberately left unsupported (leaked, not
// shipped): that method would need its own warm @enter state, a materially
// bigger feature this pass doesn't attempt (see calque#92's own "approach 1 is
// more robust... revisit if a real adopter needs it" framing).
//
// visited is checked (and set) BEFORE enqueueing a name, not just before
// shipping — this is what bounds a self-referential (f calling f.local(...))
// or cyclic (a->b->a) chain to one visit per name rather than looping forever.
func collectLocalExtras(app ir.App, unit warmUnit, rep *leak.Report) []warm.ExtraFunc {
	visited := map[string]bool{}
	var queue []string
	enqueue := func(names []string) {
		for _, n := range names {
			if !visited[n] {
				visited[n] = true
				queue = append(queue, n)
			}
		}
	}
	enqueue(unit.method.LocalCalls)
	enqueue(unit.class.EnterLocalCalls)

	var extras []warm.ExtraFunc
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if fn, ok := app.FindFunction(name); ok {
			extras = append(extras, warm.ExtraFunc{Name: fn.Name, Args: fn.Args, Body: fn.Body})
			enqueue(fn.LocalCalls)
			continue
		}
		if resolvesToClassMethod(app, name) {
			rep.Addf(leak.PrimMap, leak.KindSemanticGap, app.Script, 0,
				"%s.local(...): resolves to a @cls method — not shipped (no warm @enter state outside the picked unit); will NameError", name)
		}
	}
	return extras
}

// resolvesToClassMethod reports whether name matches any @cls method across
// app.Classes — a flat leaf-name scan, matching every other invoke-target
// lookup's convention (leafName-keyed, no per-class disambiguation).
func resolvesToClassMethod(app ir.App, name string) bool {
	for _, c := range app.Classes {
		for _, m := range c.Methods {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

// serveApp reports whether the app has any serve-shaped entrypoint (§F). Serve is
// detected and gated/leaked, but the long-lived server is not built in the spike.
func serveApp(app ir.App) bool {
	for _, f := range app.Functions {
		if f.EntryKind == ir.EntryServe {
			return true
		}
	}
	for _, ep := range app.Entrypoints {
		if ep.EntryKind == ir.EntryServe {
			return true
		}
	}
	return false
}

// swapLegal reports whether owner's gpu= site is safe to proceed with: a clean
// substitution, or no gpu= at all (a plain CPU function — legal, not a swap).
// Only FlagMulti/FlagCouple (an actually-flagged swap) refuses. calque#80: this
// used to implicitly require gpu.CleanSwap, which rejected every no-GPU plain
// function — invisible while every @cls fixture declared gpu=, surfaced once
// plain @app.functions (commonly GPU-free pipeline steps) became runnable.
func swapLegal(glog *gpu.Log, owner string) bool {
	for _, s := range glog.Subs {
		if s.Owner == owner {
			return s.Disposition == gpu.CleanSwap || s.Disposition == gpu.NoGPU
		}
	}
	return false
}

// checkInvokeSupport reports whether the warm runner's protocol (§6) can
// faithfully drive fn's invocation idiom. .for_each shares .map's single-arg
// signature and runs correctly; the only difference is Modal discards the
// return value where calque collects+reports it — a leak, not a refusal,
// since nothing crashes and the mismatch is honest but minor.
//
// .starmap needed tuple-splat (multiple positional args per item), which the
// runner now supports (calque#93, worker/warm-runner/runner.py's
// _compile_method/item()) whenever there's REAL tuple data to splat — a
// literal list-of-tuples or range() the parser statically resolved into
// fn.Items (calque#136). What .starmap still can't survive is the OTHER
// half of #83's original finding: no real tuple data at all, meaning the
// run would have to fall back to a synthesized SINGLE-value placeholder
// (dry-run-item-%d, a canned sentence, ...) — splatting a plain string
// crashes just as badly as never splatting did (confirmed by #83's original
// live repro). So the refusal narrows from "any .starmap'd unit" to "a
// .starmap'd unit with no statically-resolvable iterable" — the one case
// realOrSyntheticItems (items.go) can't make splat-safe on its own, because
// there is no real per-item shape to consult.
func checkInvokeSupport(script string, fn ir.Function, rep *leak.Report) error {
	switch fn.Invoke {
	case ir.InvokeStarmap:
		if len(fn.Items) == 0 {
			return fmt.Errorf("%s is .starmap()'d but its iterable wasn't statically resolvable (not a literal list of tuples or range()); the warm runner has no real tuple data to splat and refuses rather than crash on a synthesized single-value placeholder — see leak report", fn.Name)
		}
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, script, fn.Line,
			"%s is .starmap()'d; the warm runner splats each item's tuple across %s's positional args (calque#93) using the real iterable data extracted at parse time", fn.Name, fn.Name)
	case ir.InvokeForEach:
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, script, fn.Line,
			"%s is .for_each()'d (side-effects only, no result collection in real Modal); calque collects+reports a result per item anyway — harmless but not a faithful .for_each", fn.Name)
	}
	return nil
}

// dryRunWarm drives the real warmd supervisor + runner.py LOCALLY over a small
// synthetic sample, so we exercise the warm-once path and collect real per-item
// wall-clock without any AWS. Occupancy stays unmeasured (no GPU) -> proxy flag.
func dryRunWarm(ctx context.Context, app ir.App, unit warmUnit, n int, m *measure.Measurement, rep *leak.Report) error {
	sample := n
	if sample > 50 {
		sample = 50 // a dry-run measures per-item on a small sample; scale is modeled, not run
		rep.Addf(leak.PrimMap, leak.KindUnhandledCase, "dry-run", 0,
			"dry-run measured per-item on %d items; K at %d is modeled from that sample, not run at scale", sample, n)
	}
	arg := unit.method.ItemArg
	if arg == "" {
		arg = "item"
	}
	// calque#93: a .starmap()'d unit needs the FULL non-self/cls arg list (not
	// just the first) so the runner can bind every one of them, and a
	// tuple-shaped fallback synth closure so a too-short/unresolvable real
	// iterable still produces something splat-compatible instead of crashing
	// on a single-string payload. checkInvokeSupport already refused any
	// starmap unit with NO real Items at all — reaching here means there's
	// real tuple data, but realOrSyntheticItems can still fall back per-call
	// if `sample` asks for more items than the real data has.
	isStarmap := unit.method.Invoke == ir.InvokeStarmap
	methodArgs := nonSelfArgs(unit.method.Args)
	// The real @enter/@method bodies need a GPU + model weights we don't have
	// locally. A dry-run proves the PLUMBING (warm-once, framing, ordered collect,
	// per-item timing), not the model — so we substitute trivial CPU stand-in
	// bodies and LEAK the substitution, rather than crash on an import that only
	// resolves on the acquired instance.
	enterBody := unit.class.EnterBody
	methodBody := unit.method.Body
	if bodyNeedsGPU(enterBody) || bodyNeedsGPU(methodBody) {
		rep.Addf(leak.PrimEnter, leak.KindUnhandledCase, "dry-run", unit.class.Line,
			"dry-run substituted CPU stand-in bodies for %q (real @enter/@method need GPU+weights, only resolvable on the acquired instance)", unit.class.Name)
		enterBody = "import time\ntime.sleep(0.3)  # simulate a model load\nself.calls = 0"
		// Simulate a plausible per-item compute so the plumbing produces a
		// non-degenerate (but still SYNTHETIC) per-item second. The real number
		// only comes from the acquired-instance run; this K is plumbing proof only.
		methodBody = "import time\ntime.sleep(0.05)\nself.calls += 1\nreturn {'dry_run': True, 'n': self.calls}"
	}
	// calque#92: ship any sibling functions the picked unit's body reaches via
	// .local() — previously only leaked, now actually shipped so the dry-run
	// doesn't NameError on real in-container pipeline chaining (confirmed
	// blocking on AI-Almanac's blending_app.py, calque#79).
	extras := collectLocalExtras(app, unit, rep)
	if len(extras) > 0 {
		names := make([]string, len(extras))
		for i, e := range extras {
			names[i] = e.Name
		}
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, app.Script, unit.method.Line,
			"shipped %d sibling function(s) referenced via .local(): %s", len(extras), strings.Join(names, ", "))
	}

	sink := warm.NewMemSink()
	sup := &warm.Supervisor{
		Python: pythonBin(),
		Script: runnerScriptPath(),
		Sink:   sink,
		Leak:   leakAdapter{rep},
		Config: warm.Config{
			EnterBody: enterBody, MethodBody: methodBody, MethodArg: arg, Extras: extras,
			MethodArgs: methodArgs, Starmap: isStarmap,
		},
		// B3: retries= (portable config) caps the warm supervisor's crash re-drive.
		// 0 leaves warmd's sane default.
		MaxRestarts: unit.class.Config.Retries,
	}
	// calque#136/#93: drive the script's REAL .map()/.starmap() iterable when
	// pyast statically resolved one long enough for `sample` items, else fall
	// back to a synthesized placeholder. A .starmap()'d unit's fallback must
	// ALSO be tuple-shaped (one element per methodArgs entry) — a bare string
	// would crash item()'s *payload splat rather than silently mis-bind, so
	// the fallback closure differs by invocation kind (unchanged for
	// map/for_each/remote, which still get the original single string).
	items := realOrSyntheticItems(unit, sample, starmapAwareSynth(isStarmap, methodArgs), rep)
	start := time.Now()
	failed, err := sup.Run(ctx, items)
	if err != nil {
		return err
	}
	m.EnterSeconds = sup.EnterSeconds
	m.PerItem = measure.Aggregate(sink.Seconds())
	// In a dry-run there is no acquire wait or rectangle; occupancy is unmeasured.
	m.AcquiredAt = start
	m.TerminatedAt = time.Now()
	fmt.Printf("[DRY-RUN] warm unit ran %d items, %d failed; @enter x%d (%.3fs), mean %.4fs/item\n",
		sample, len(failed), sup.EnterCount, sup.EnterSeconds, m.PerItem.MeanSecs)
	return nil
}

// nonSelfArgs filters "self"/"cls" out of a Function.Args list (calque#93),
// giving the runner the FULL ordered per-item parameter list a .starmap()'d
// method binds — e.g. ["a","b"] for combine(self, a, b). Mirrors
// internal/parse.firstItemArg's self/cls skip, but keeps every remaining name
// instead of just the first.
func nonSelfArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "self" || a == "cls" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// starmapAwareSynth returns the per-index fallback payload closure
// realOrSyntheticItems falls back to when the picked unit's real Items are
// nil or shorter than the requested sample (calque#93). A non-starmap unit
// keeps the pre-existing single-string placeholder, byte-identical to before
// this change. A starmap unit's fallback must ALSO be tuple-shaped — one
// synthesized element per methodArgs entry (e.g. (i, i) for two params) — so
// that even the "no real tuple data" fallback path stays splat-compatible:
// runner.py's item() splats *payload whenever starmap is set, and splatting a
// single string would crash (or silently iterate its characters) rather than
// bind cleanly. checkInvokeSupport already refuses a starmap unit with NO
// real Items at all, so this closure only ever fires for the narrower
// "some real Items, but --n/sample asked for more" case.
func starmapAwareSynth(isStarmap bool, methodArgs []string) func(i int) any {
	if !isStarmap || len(methodArgs) == 0 {
		return func(i int) any { return fmt.Sprintf("dry-run-item-%d", i) }
	}
	return func(i int) any {
		tuple := make([]any, len(methodArgs))
		for j := range methodArgs {
			tuple[j] = i
		}
		return tuple
	}
}

// bodyNeedsGPU is a heuristic: does this verbatim body import/use something that
// only resolves on a GPU instance (vllm, torch.cuda, a model load)? Used only to
// decide whether the LOCAL dry-run must substitute a stand-in body.
func bodyNeedsGPU(body string) bool {
	for _, sig := range []string{"vllm", "torch", "cuda", "transformers", "from_pretrained", "LLM(", "torchvision"} {
		if strings.Contains(body, sig) {
			return true
		}
	}
	return false
}

func pythonBin() string {
	if p := os.Getenv("CALQUE_PYTHON"); p != "" {
		return p
	}
	return "python3"
}

func runnerScriptPath() string {
	if p := os.Getenv("CALQUE_RUNNER"); p != "" {
		return p
	}
	p, _ := filepath.Abs("worker/warm-runner/runner.py")
	return p
}

// leakAdapter bridges warmd's Leaker to the leak.Report.
type leakAdapter struct{ rep *leak.Report }

func (l leakAdapter) Leak(kind, detail string) {
	l.rep.Add(leak.PrimEnter, leak.Kind(kind), "warm-runner", 0, detail)
}
