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
	// reports which one is in play. Does NOT yet change pickWarmUnit's selection
	// below: calque has no call-site-to-entrypoint attribution (a .map()/.remote()
	// call site isn't tracked as belonging to one entrypoint's body vs. another's),
	// so --entrypoint can't steer WHICH callable runs yet — see calque#90's
	// follow-up for that deeper fix. This pass only closes the "silently ran
	// whichever pickWarmUnit found, no way to even ask for a different one" gap.
	epName, err := resolveEntrypoint(app, o.entrypoint)
	if err != nil {
		return err
	}
	if epName != "" {
		fmt.Printf("entrypoint: %s (selected)\n", epName)
	}

	// pick the mapped warm unit (a @cls with @enter whose method is .map'd)
	unit, ok := pickWarmUnit(app)
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
		rep.Addf(leak.PrimEnter, leak.KindSemanticGap, app.Script, unit.method.Line,
			"%s: plain @app.function, no @cls+@enter — no warm-reuse economics to amortize across items; K here measures a different thing than a @cls+@enter warm unit's K", unit.method.Name)
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
		if err := dryRunWarm(ctx, unit, o.n, &m, rep); err != nil {
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

func pickWarmUnit(app ir.App) (warmUnit, bool) {
	for _, c := range app.Classes {
		if c.EnterBody == "" {
			continue
		}
		for _, mth := range c.Methods {
			if mth.IsMap {
				return warmUnit{class: c, method: mth}, true
			}
		}
		// fall back to the first method if none is explicitly .map'd
		if len(c.Methods) > 0 {
			return warmUnit{class: c, method: c.Methods[0]}, true
		}
	}
	// No @cls+@enter unit — prefer a plain function that's .map'd (closest
	// analog to the class-based shape: many items through one warm process),
	// else fall back to the first plain function (single-call replay, §G).
	for _, f := range app.Functions {
		if f.IsMap {
			return warmUnit{class: syntheticClass(f), method: f, plainFunction: true}, true
		}
	}
	if len(app.Functions) > 0 {
		f := app.Functions[0]
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

// checkInvokeSupport reports whether the warm runner's single-arg,
// always-collects-a-result protocol (§6) can faithfully drive fn's invocation
// idiom (calque#83). .starmap needs tuple-splat (multiple positional args per
// item) — running it as .map would bind only the first and crash every item
// (confirmed via a live repro: a raw NameError for the unbound second+ arg,
// with nothing pointing at the real cause) — so this returns a hard error
// naming the mismatch instead. .for_each shares .map's single-arg signature
// and runs correctly; the only difference is Modal discards the return value
// where calque collects+reports it — a leak, not a refusal, since nothing
// crashes and the mismatch is honest but minor.
func checkInvokeSupport(script string, fn ir.Function, rep *leak.Report) error {
	switch fn.Invoke {
	case ir.InvokeStarmap:
		return fmt.Errorf("%s is .starmap()'d (tuple-splat args); the warm runner only binds one positional arg per item (§6) and would crash every item — not yet supported, see leak report", fn.Name)
	case ir.InvokeForEach:
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, script, fn.Line,
			"%s is .for_each()'d (side-effects only, no result collection in real Modal); calque collects+reports a result per item anyway — harmless but not a faithful .for_each", fn.Name)
	}
	return nil
}

// dryRunWarm drives the real warmd supervisor + runner.py LOCALLY over a small
// synthetic sample, so we exercise the warm-once path and collect real per-item
// wall-clock without any AWS. Occupancy stays unmeasured (no GPU) -> proxy flag.
func dryRunWarm(ctx context.Context, unit warmUnit, n int, m *measure.Measurement, rep *leak.Report) error {
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
	sink := warm.NewMemSink()
	sup := &warm.Supervisor{
		Python: pythonBin(),
		Script: runnerScriptPath(),
		Sink:   sink,
		Leak:   leakAdapter{rep},
		Config: warm.Config{EnterBody: enterBody, MethodBody: methodBody, MethodArg: arg},
		// B3: retries= (portable config) caps the warm supervisor's crash re-drive.
		// 0 leaves warmd's sane default.
		MaxRestarts: unit.class.Config.Retries,
	}
	items := make([]warm.Item, sample)
	for i := range items {
		items[i] = warm.Item{Index: i, Payload: fmt.Sprintf("dry-run-item-%d", i)}
	}
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
