package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
	"github.com/spore-host/calque/internal/plan"
	"github.com/spore-host/calque/internal/target"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// itemFromFile reads path's raw bytes and wraps them as the SINGLE item a
// real-AWS run should drive (calque real --item-file PATH) — for a picked
// unit whose real signature takes `bytes` (e.g. a netCDF/tarball bundle),
// not a string/dict literal a script's own .map()/.starmap() call already
// provides statically (realOrSyntheticItems' existing sources). encoding/
// json auto-base64-encodes a []byte value, so Payload just holds the raw
// bytes — no manual encoding needed on this side; the runner decodes the
// resulting base64 STRING back to bytes before calling the shipped body
// (see runner.py's item(), gated on Config.PayloadIsBase64Bytes so every
// other invocation kind is untouched).
func itemFromFile(path string) ([]warm.Item, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --item-file %q: %w", path, err)
	}
	return []warm.Item{{Index: 0, Payload: data}}, nil
}

// itemFromArgs builds the SINGLE item a real-AWS run should drive from
// argFiles (IDX -> file path, raw bytes) and argJSON (IDX -> literal JSON
// value), for a picked unit whose real signature mixes a bytes positional
// arg with non-bytes ones (calque real --arg-file/--arg-json — the
// positional-args sibling of itemFromFile, needed when the whole payload
// can't be treated as a single bytes value the way --item-file's design
// requires; e.g. run_benchmark_local(job_id: str, config: dict, bundle:
// bytes, runtime_env: dict | None)). Every index from 0 through the
// highest given must be covered by exactly one of the two maps — a gap or a
// double-cover is a caller error, not a best-effort fallback, since a
// missing/duplicated positional arg would silently mis-bind the shipped
// body's real signature. base64Indices reports which resulting tuple
// positions hold file bytes (encoding/json auto-base64-encodes each
// position's own []byte value, same mechanism as itemFromFile) — the
// caller ships this as Config.Base64ArgIndices so the runner decodes only
// those positions back to bytes (runner.py's item(), gated on
// self.base64_arg_indices).
func itemFromArgs(argFiles, argJSON map[int]string) ([]warm.Item, []int, error) {
	n := 0
	for idx := range argFiles {
		if idx+1 > n {
			n = idx + 1
		}
	}
	for idx := range argJSON {
		if idx+1 > n {
			n = idx + 1
		}
	}
	tuple := make([]any, n)
	base64Indices := make([]int, 0, len(argFiles))
	for idx := 0; idx < n; idx++ {
		filePath, fromFile := argFiles[idx]
		jsonLit, fromJSON := argJSON[idx]
		switch {
		case fromFile && fromJSON:
			return nil, nil, fmt.Errorf("arg index %d given via both --arg-file and --arg-json", idx)
		case fromFile:
			data, err := os.ReadFile(filePath)
			if err != nil {
				return nil, nil, fmt.Errorf("read --arg-file %d=%q: %w", idx, filePath, err)
			}
			tuple[idx] = data
			base64Indices = append(base64Indices, idx)
		case fromJSON:
			var v any
			if err := json.Unmarshal([]byte(jsonLit), &v); err != nil {
				return nil, nil, fmt.Errorf("--arg-json %d=%q is not valid JSON: %w", idx, jsonLit, err)
			}
			tuple[idx] = v
		default:
			return nil, nil, fmt.Errorf("arg index %d has no --arg-file or --arg-json (positions must be contiguous from 0, each covered exactly once)", idx)
		}
	}
	sort.Ints(base64Indices)
	return []warm.Item{{Index: 0, Payload: tuple}}, base64Indices, nil
}

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
	extras, extraConsts, extraImports, extraClasses := collectLocalExtras(app, unit, rep)
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
	if len(extraImports) > 0 {
		names := make([]string, len(extraImports))
		for i, e := range extraImports {
			names[i] = e.Name
		}
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, app.Script, unit.method.Line,
			"shipped %d module-level import(s) referenced via a bare name (calque#146): %s", len(extraImports), strings.Join(names, ", "))
	}
	if len(extraClasses) > 0 {
		names := make([]string, len(extraClasses))
		for i, e := range extraClasses {
			names[i] = e.Name
		}
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, app.Script, unit.method.Line,
			"shipped %d module-level class(es) referenced via a bare name (calque#147): %s", len(extraClasses), strings.Join(names, ", "))
	}
	return calexec.ManifestBody{
		EnterBody: unit.class.EnterBody, MethodBody: unit.method.Body, MethodArg: arg,
		MethodArgs: methodArgs, Starmap: isStarmap, Extras: extras, ExtraConsts: extraConsts,
		ExtraImports: extraImports, ExtraClasses: extraClasses,
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
func warmUnitForScript(ctx context.Context, scriptPath, entrypoint string, rep *leak.Report) (ir.App, warmUnit, bool) {
	return warmUnitForScriptFn(ctx, scriptPath, entrypoint, "", rep)
}

// warmUnitForScriptFn is warmUnitForScript plus function (calque real
// --function NAME): when non-empty, selects that specific @app.function/
// @cls method by name (pickWarmUnitByName) instead of running pickWarmUnit's
// automatic entrypoint/`.map()`-preference scan at all — entrypoint is still
// resolved/validated (a bad --entrypoint should still error), but function
// takes priority over its selection once resolution succeeds. Needed when
// the target callable isn't reachable through any @app.local_entrypoint()
// at all (e.g. AI-Almanac's app.py: its only entrypoint invokes the sibling
// run_benchmark, never run_benchmark_local, so pickWarmUnit's scan would
// silently pick run_benchmark instead). "" (the default) reproduces
// warmUnitForScript's behavior byte-for-byte.
func warmUnitForScriptFn(ctx context.Context, scriptPath, entrypoint, function string, rep *leak.Report) (ir.App, warmUnit, bool) {
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
	// --function selects a specific callable by name directly — this is
	// deliberately checked BEFORE entrypoint resolution, since the target
	// callable may not be reachable through any @app.local_entrypoint() at
	// all (the exact AI-Almanac app.py shape this exists for): requiring a
	// valid/unambiguous --entrypoint first would wrongly refuse a
	// perfectly resolvable --function request on a multi-entrypoint script.
	if function != "" {
		unit, ok := pickWarmUnitByName(app, function)
		if !ok {
			rep.Addf(leak.PrimMap, leak.KindSemanticGap, scriptPath, 0,
				"--function %q not found as any @app.function or @cls method; using synthesized placeholder items (calque#136)", function)
			return ir.App{}, warmUnit{}, false
		}
		return app, unit, true
	}
	// calque#79/#90: real/ramp/fleetrun previously had no way to pick an
	// entrypoint at all — a multi-entrypoint script (e.g. AI-Almanac's
	// blending_app.py, 7 entrypoints) silently defaulted to whichever
	// pickWarmUnit("") happened to find first, with no error and no way
	// to override it, unlike run --dry-run's own --entrypoint flag
	// (resolveEntrypoint). entrypoint="" preserves prior behavior
	// exactly for the single/no-entrypoint case.
	epName, eerr := resolveEntrypoint(app, entrypoint)
	if eerr != nil {
		rep.Addf(leak.PrimMap, leak.KindSemanticGap, scriptPath, 0,
			"--script %q: %v; using synthesized placeholder items (calque#136)", scriptPath, eerr)
		return ir.App{}, warmUnit{}, false
	}
	unit, ok := pickWarmUnit(app, epName)
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
// here.
//
// instance is the caller's own EXPLICIT --instance ("" means the operator
// didn't pass one). fallbackInstance is what instance defaults to when
// unset AND no swap applied — the subcommand's own pre-#178 hardcoded
// default (e.g. "g6.2xlarge" for real), preserved so the no-swap case (the
// overwhelming majority) stays byte-for-byte unchanged. allowSwap threads
// through --allow-card-swap (calque#178): when true and the asked-for card
// has a verified target.CardSwapFor entry, the swapped card becomes
// tgt.Card, and — ONLY when instance is also unset — a real instance for
// that NEW card is resolved via plan.FillTarget instead of the old
// hardcoded fallback, since the old fallback was sized for the OLD card. An
// explicit --instance always wins over both the swap and FillTarget.
func recommendedTarget(unit warmUnit, instance, fallbackInstance string, allowSwap bool, rep *leak.Report) *target.Target {
	tgt := target.StubRecommender{}.Recommend(ir.App{}, unit.method)
	swapped := false
	if allowSwap {
		if to, ok := target.CardSwapFor(tgt.Card); ok {
			tgt.Card = to
			swapped = true
		}
	}
	if instance != "" {
		tgt.Instance = instance
		return &tgt
	}
	if !swapped {
		tgt.Instance = fallbackInstance
		return &tgt
	}
	if err := plan.FillTarget(&tgt, plan.NewTruffleResolver(rep), rep); err != nil {
		// Matches FillTarget's own no-silent-fallback contract
		// (internal/plan/truffle.go) — surface the failure rather than
		// guessing an instance for the swapped card.
		rep.Addf(leak.PrimAcquire, leak.KindIntegrationEdge, "real", 0,
			"--allow-card-swap: could not resolve an instance for swapped card %q: %v", tgt.Card, err)
	}
	return &tgt
}
