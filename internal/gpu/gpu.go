// Package gpu implements the gpu= rewrite rule and its guard (spec §7).
//
// Modal scripts say gpu="H100", gpu="A100", etc. We rewrite to RTX PRO 6000
// (96GB) — memory is "same-ish" vs 80GB A100/H100, so the model still fits. BUT
// the swap is only legal if the original job was memory-bound or single-card. We
// FLAG (never silently substitute) jobs that genuinely needed the big card's
// bandwidth/interconnect:
//
//	>1 GPU (e.g. "H100:8")                       -> FLAG multi-GPU, out of single-node scope
//	torchrun / NVLink / tensor-parallel in body  -> FLAG coupled, out of scope
//	else                                         -> substitute -> RTX PRO 6000, log substitution
//
// The ratio (clean-swaps : flags) across a corpus is itself a finding (§16.4):
// most Modal inference is B=1 request-response and lands swap-legal by construction.
package gpu

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/target"
)

// Disposition is the outcome of evaluating one gpu= site.
type Disposition string

const (
	CleanSwap  Disposition = "clean_swap"  // legal substitution -> RTX PRO 6000
	FlagMulti  Disposition = "flag_multi"  // >1 GPU requested; out of single-node scope
	FlagCouple Disposition = "flag_couple" // torchrun/NVLink/tensor-parallel; coupled workload
	NoGPU      Disposition = "no_gpu"      // callable declared no gpu=
)

// Substitution is one structured log entry (spec §7: "every clean swap and every flag").
type Substitution struct {
	Script      string      `json:"script"`
	Owner       string      `json:"owner"` // function or class name
	Line        int         `json:"line"`
	Requested   ir.GPUSpec  `json:"requested"` // parsed from raw gpu=
	Disposition Disposition `json:"disposition"`
	Substituted string      `json:"substituted"` // the card we swapped to, or "" if flagged
	Reason      string      `json:"reason"`
}

// Log accumulates substitutions across a run. Counts feed the corpus census.
type Log struct {
	Subs []Substitution `json:"substitutions"`
}

// Counts summarizes the log: the clean-swaps : flags ratio (spec §7/§16.4).
type Counts struct {
	CleanSwaps int `json:"clean_swaps"`
	FlagMulti  int `json:"flag_multi"`
	FlagCouple int `json:"flag_couple"`
	NoGPU      int `json:"no_gpu"`
}

func (l *Log) Counts() Counts {
	var c Counts
	for _, s := range l.Subs {
		switch s.Disposition {
		case CleanSwap:
			c.CleanSwaps++
		case FlagMulti:
			c.FlagMulti++
		case FlagCouple:
			c.FlagCouple++
		case NoGPU:
			c.NoGPU++
		}
	}
	return c
}

// ParseSpec parses a raw gpu= string into card + count. "H100" -> {H100,1};
// "A100:8" -> {A100,8}; "" -> {"",0}. Count defaults to 1 when a card is present
// without an explicit ":n".
func ParseSpec(raw string) ir.GPUSpec {
	raw = strings.TrimSpace(raw)
	spec := ir.GPUSpec{Raw: raw}
	if raw == "" {
		return spec
	}
	if i := strings.LastIndex(raw, ":"); i >= 0 {
		spec.Card = strings.TrimSpace(raw[:i])
		if n, err := strconv.Atoi(strings.TrimSpace(raw[i+1:])); err == nil {
			spec.Count = n
			return spec
		}
		// "H100:foo" — malformed count; treat as single but keep raw for the leak.
		spec.Count = 1
		return spec
	}
	spec.Card = raw
	spec.Count = 1
	return spec
}

// coupledRe matches multi-GPU coupling signals in a function body (spec §7).
//
// Deliberately NOT word-boundary-anchored: coupling tokens routinely appear
// inside larger identifiers (e.g. `build_tensor_parallel_model`, `nccl_backend`),
// and the risk is asymmetric — a false FLAG only tells the user "we can't carry
// this" (safe), while a missed signal SILENTLY DOWNGRADES a coupled job onto one
// card (the exact failure §7 exists to prevent). So we substring-match and accept
// occasional over-flagging; §7 explicitly forbids silently substituting across
// coupling.
var coupledRe = regexp.MustCompile(`(?i)(torchrun|nvlink|tensor[_\s-]?parallel|nccl|deepspeed|megatron|torch\.distributed|init_process_group|fullyshardeddataparallel|distributeddataparallel|\bfsdp\b|\bddp\b)`)

// couplingSignal returns the first coupling token found in body, or "".
func couplingSignal(body string) string {
	if m := coupledRe.FindString(body); m != "" {
		return m
	}
	return ""
}

// evaluate decides the disposition for one gpu= site given its raw spec and the
// concatenated body text that would run on the card.
func evaluate(raw, body string) (ir.GPUSpec, Disposition, string) {
	spec := ParseSpec(raw)
	if spec.Card == "" {
		return spec, NoGPU, "no gpu= declared"
	}
	if spec.Count > 1 {
		return spec, FlagMulti, "requests >1 GPU (" + raw + "); multi-GPU is out of single-node scope"
	}
	if sig := couplingSignal(body); sig != "" {
		return spec, FlagCouple, "body shows coupling signal " + strconv.Quote(sig) + "; coupled/tensor-parallel is out of scope"
	}
	return spec, CleanSwap, "single-card, no coupling signal; memory-bound B=1 substitution is legal"
}

// RewriteApp evaluates every gpu= site in the app, appends to the substitution
// log, emits leaks for flags, and returns the log. It does NOT mutate the IR's
// raw GPU strings — downstream reads target.DefaultCard via the seam for clean
// swaps; the raw value is preserved for cost's rate-asymmetry (R_m uses the card
// the script ASKED for, §9). Flagged sites are left for the caller to refuse.
func RewriteApp(app ir.App, rep *leak.Report) *Log {
	log := &Log{}
	eval := func(owner, raw, body string, line int) {
		spec, disp, reason := evaluate(raw, body)
		sub := Substitution{
			Script: app.Script, Owner: owner, Line: line,
			Requested: spec, Disposition: disp, Reason: reason,
		}
		switch disp {
		case CleanSwap:
			sub.Substituted = target.DefaultCard
		case FlagMulti:
			rep.Addf(leak.PrimGPU, leak.KindSemanticGap, app.Script, line,
				"%s: %s — NOT substituted", owner, reason)
		case FlagCouple:
			rep.Addf(leak.PrimGPU, leak.KindSemanticGap, app.Script, line,
				"%s: %s — NOT substituted", owner, reason)
		}
		log.Subs = append(log.Subs, sub)
	}

	for _, f := range app.Functions {
		eval(f.Name, f.GPU, f.Body, f.Line)
	}
	for _, c := range app.Classes {
		// A class's card runs the @enter body plus each @method body; scan them together
		// so a coupling signal anywhere in the warm unit trips the guard.
		var b strings.Builder
		b.WriteString(c.EnterBody)
		b.WriteByte('\n')
		for _, m := range c.Methods {
			b.WriteString(m.Body)
			b.WriteByte('\n')
		}
		eval(c.Name, c.GPU, b.String(), c.Line)
	}
	return log
}
