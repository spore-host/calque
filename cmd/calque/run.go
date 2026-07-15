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
	script  string
	n       int
	region  string
	dryRun  bool // exercise every stage WITHOUT launching a billable instance
	ratesFP string
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

	// pick the mapped warm unit (a @cls with @enter whose method is .map'd)
	unit, ok := pickWarmUnit(app)
	if !ok {
		return fmt.Errorf("no mapped @cls+@enter warm unit found in %s (spike targets map_batch shape)", o.script)
	}
	fmt.Printf("warm unit: class %q, method %q, gpu asked-for %q\n", unit.class.Name, unit.method.Name, unit.class.GPU)

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

// warmUnit is a @cls with an @enter and a .map'd @method — the spike's target shape.
type warmUnit struct {
	class  ir.Class
	method ir.Function
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
	return warmUnit{}, false
}

func swapLegal(glog *gpu.Log, owner string) bool {
	for _, s := range glog.Subs {
		if s.Owner == owner {
			return s.Disposition == gpu.CleanSwap
		}
	}
	return false
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
