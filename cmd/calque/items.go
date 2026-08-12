package main

import (
	"context"
	"strings"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
	"github.com/spore-host/calque/internal/target"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// realOrSyntheticItems returns the warm.Item batch for a run: n items built
// from unit.method.Items (the real .map()/.starmap() iterable extracted at
// parse time, calque#136) when there are at least n of them, else calque's
// existing synthesized placeholder — the default/safe path for scripts whose
// iterable wasn't statically resolvable (a variable, comprehension, or
// non-range function call), or whose real data is shorter than what --n asks
// for. synth is the caller's OWN pre-existing per-index placeholder closure,
// so a script that falls back behaves byte-identically to before this
// function existed.
func realOrSyntheticItems(unit warmUnit, n int, synth func(i int) any, rep *leak.Report) []warm.Item {
	if len(unit.method.Items) >= n {
		items := make([]warm.Item, n)
		for i := 0; i < n; i++ {
			items[i] = warm.Item{Index: i, Payload: unit.method.Items[i]}
		}
		return items
	}
	if len(unit.method.Items) > 0 {
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, unit.method.Name, unit.method.Line,
			"real iterable data has %d items but --n requested %d; using synthesized placeholder for all %d items (calque#136)",
			len(unit.method.Items), n, n)
	} else {
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, unit.method.Name, unit.method.Line,
			"real iterable data wasn't statically extractable (not a literal list/tuple or range()); using synthesized placeholder (calque#136)")
	}
	items := make([]warm.Item, n)
	for i := 0; i < n; i++ {
		items[i] = warm.Item{Index: i, Payload: synth(i)}
	}
	return items
}

// manifestBodyForUnit builds the calexec.ManifestBody a real-AWS run
// (calque real/ramp/fleetrun, calque#79 Part 1) should ship for unit,
// instead of always driving the hardcoded vLLM realEnterBody/realMethodBody
// regardless of what --script actually parsed. ok is false when unit is the
// zero warmUnit{} (--script unset, or its parse failed) — callers fall back
// to their existing hardcoded body unchanged, exactly today's pre-#79
// behavior.
//
// Mirrors dryRunWarm's (run.go) extras/starmap logic, MINUS its GPU-body
// substitution: dry-run has no GPU, so it swaps in a CPU stand-in and leaks
// the substitution; a real-AWS run legitimately runs unit's OWN body on
// real hardware — there is nothing to substitute. checkInvokeSupport
// should already have been called by the time this runs (mirroring run.go's
// own pipeline order); this function does not re-check .starmap()
// refusal — see run.go's checkInvokeSupport for that.
func manifestBodyForUnit(app ir.App, unit warmUnit, rep *leak.Report) (calexec.ManifestBody, bool) {
	if unit.method.Name == "" {
		return calexec.ManifestBody{}, false // zero warmUnit{} — no script parsed, or its parse failed
	}
	arg := unit.method.ItemArg
	if arg == "" {
		arg = "item"
	}
	isStarmap := unit.method.Invoke == ir.InvokeStarmap
	methodArgs := nonSelfArgs(unit.method.Args)
	extras, extraConsts := collectLocalExtras(app, unit, rep)
	if len(extras) > 0 {
		names := make([]string, len(extras))
		for i, e := range extras {
			names[i] = e.Name
		}
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, app.Script, unit.method.Line,
			"shipped %d sibling function(s): %s", len(extras), strings.Join(names, ", "))
	}
	if len(extraConsts) > 0 {
		names := make([]string, len(extraConsts))
		for i, e := range extraConsts {
			names[i] = e.Name
		}
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, app.Script, unit.method.Line,
			"shipped %d module-level constant(s) referenced via a bare name (calque#139): %s", len(extraConsts), strings.Join(names, ", "))
	}
	return calexec.ManifestBody{
		EnterBody: unit.class.EnterBody, MethodBody: unit.method.Body, MethodArg: arg,
		MethodArgs: methodArgs, Starmap: isStarmap, Extras: extras, ExtraConsts: extraConsts,
	}, true
}

// warmUnitForScript parses scriptPath (when non-empty) and returns its
// pickWarmUnit result, for the real-AWS execution paths (calque real/ramp/
// fleetrun) that don't otherwise parse ANY Modal script — they drive a fixed,
// hardcoded vLLM reference body against a bare `--model` HF repo id, with no
// ir.App/warmUnit in their pipeline at all (see CHANGELOG.md's own "Known
// gaps" note). scriptPath is a NEW, purely opt-in flag on those commands
// (calque#136): passing it is the only way for realOrSyntheticItems to find
// real Items on this path, and leaving it unset ("") reproduces today's
// behavior byte-for-byte — returns a zero warmUnit with ok=false, which
// realOrSyntheticItems treats exactly like "iterable wasn't statically
// resolvable" (nil Items), i.e. always falls back to the caller's synth
// closure. A parse error is reported via rep rather than failing the run —
// this is a best-effort enrichment of the item batch, not a new hard
// dependency for these commands.
// The returned ir.App is the zero value when ok is false (--script unset,
// or its parse failed) — manifestBodyForUnit (calque#79 Part 1) needs the
// parsed app alongside the picked unit to resolve collectLocalExtras'
// .local()/free-ref closure, which warmUnitForScript's callers previously
// had no reason to keep around since only items.go's realOrSyntheticItems
// consumed the unit itself.
func warmUnitForScript(ctx context.Context, scriptPath string, rep *leak.Report) (ir.App, warmUnit, bool) {
	if scriptPath == "" {
		return ir.App{}, warmUnit{}, false
	}
	runner, runnerArgs := parse.DefaultRunner(pyastDir())
	app, err := parse.Parse(ctx, scriptPath, rep, runner, runnerArgs...)
	if err != nil {
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, scriptPath, 0,
			"--script %q could not be parsed (%v); using synthesized placeholder items (calque#136)", scriptPath, err)
		return ir.App{}, warmUnit{}, false
	}
	unit, ok := pickWarmUnit(app, "")
	return app, unit, ok
}

// recommendedTarget builds the Target these real-AWS commands (real/ramp/
// fleetrun/spawn-run/smoke/gpuprobe) launch against, by calling
// target.StubRecommender.Recommend on unit.method so the Target carries the
// card the script actually asked for (calque#134) instead of always
// hardcoding target.DefaultCard regardless of what any parsed script
// requested. When no script was parsed, unit is the zero warmUnit{}, whose
// method.GPU is "" exactly like ir.Function{} — Recommend's own DefaultCard
// fallback applies identically either way, so no separate branch is needed
// here. instance is always the caller's own pinned --instance — these
// commands never call plan.FillTarget to derive it from the card, so
// Instance is set here directly regardless of which card Recommend picked.
func recommendedTarget(unit warmUnit, instance string) *target.Target {
	tgt := target.StubRecommender{}.Recommend(ir.App{}, unit.method)
	tgt.Instance = instance
	return &tgt
}
