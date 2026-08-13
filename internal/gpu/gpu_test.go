package gpu

import (
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

func TestParseSpec(t *testing.T) {
	cases := []struct {
		raw   string
		card  string
		count int
	}{
		{"H100", "H100", 1},
		{"A100:8", "A100", 8},
		{"H100:1", "H100", 1},
		{"", "", 0},
		{"  L4  ", "L4", 1},
		{"H100:foo", "H100", 1}, // malformed count -> treated as single
	}
	for _, c := range cases {
		got := ParseSpec(c.raw)
		if got.Card != c.card || got.Count != c.count {
			t.Errorf("ParseSpec(%q) = {card:%q count:%d}, want {card:%q count:%d}",
				c.raw, got.Card, got.Count, c.card, c.count)
		}
	}
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		body      string
		clustered bool
		want      Disposition
	}{
		{"single card plain inference", "H100", "return self.llm.generate(prompt)", false, CleanSwap},
		{"no gpu", "", "return x * 2", false, NoGPU},
		{"multi-gpu count", "H100:8", "whatever", false, FlagMulti},
		{"explicit single is clean", "A100:1", "self.model(x)", false, CleanSwap},

		// Coupling signals in the body must FLAG even when count == 1. These are
		// the silent-downgrade cases §7 exists to prevent.
		{"torchrun", "A100", "os.system('torchrun --nproc 4 train.py')", false, FlagCouple},
		{"torch.distributed", "H100", "import torch.distributed as dist", false, FlagCouple},
		{"init_process_group", "A100", "dist.init_process_group(backend='nccl')", false, FlagCouple},
		{"nccl backend", "A100", "backend='nccl'", false, FlagCouple},
		{"deepspeed", "H100", "engine = deepspeed.initialize(model)", false, FlagCouple},

		// Regression: coupling token embedded inside a larger identifier. The
		// original \b-anchored regex missed this and silently clean-swapped a
		// tensor-parallel model. Must FLAG.
		{"tensor_parallel inside identifier", "A100", "self.model = build_tensor_parallel_model(world_size=4)", false, FlagCouple},

		// calque#152: @modal.experimental.clustered(...) requests multi-node
		// execution — invisible to BOTH spec.Count (parsed from the gpu=
		// STRING alone) and couplingSignal (a body-text regex), since it's a
		// decorator-level construct neither check ever inspects. A literal
		// single-card gpu= with no body-text coupling signal at all must
		// still FLAG when clustered is true — this is the exact silent-
		// downgrade case that was confirmed as a real bug (calque#152).
		{"clustered single-GPU-per-node still FLAGs", "A100", "return train()", true, FlagCouple},
		{"clustered with no gpu= at all still FLAGs", "", "return train()", true, FlagCouple},
		{"clustered with multi-gpu count still FLAGs (not FlagMulti)", "H100:8", "return train()", true, FlagCouple},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got, reason := evaluate(c.raw, c.body, c.clustered)
			if got != c.want {
				t.Errorf("evaluate(%q, %q, clustered=%v) = %s (%s), want %s", c.raw, c.body, c.clustered, got, reason, c.want)
			}
		})
	}
}

// TestGuardBiasesTowardFlagging documents the asymmetry as an executable claim:
// a plausible coupling token should never be dismissed as a clean swap.
func TestGuardBiasesTowardFlagging(t *testing.T) {
	// Uppercase, hyphenated, and spaced variants all count as coupling.
	for _, body := range []string{
		"TENSOR-PARALLEL", "tensor parallel", "NVLink bridge", "using FSDP wrapper",
	} {
		if _, disp, _ := evaluate("A100", body, false); disp != FlagCouple {
			t.Errorf("body %q should FlagCouple, got %s", body, disp)
		}
	}
}

// TestRewriteApp_ClusteredPlainFunctionFlagsCoupleDespiteLiteralSingleGPU is
// the regression test for calque#152: a real-world script (avataRL's
// modal_train.py) stacks @modal.experimental.clustered(...) under
// @app.function(gpu="A100") — a literal, single-card gpu= with no body-text
// coupling signal at all. Before this fix, ir.Function had no IsClustered
// field at all, so RewriteApp's eval() had no way to see the decorator and
// reported CleanSwap — a real multi-node workload silently passed as legal,
// exactly the class of bug §7's own coupledRe comment says is unacceptable.
func TestRewriteApp_ClusteredPlainFunctionFlagsCoupleDespiteLiteralSingleGPU(t *testing.T) {
	app := ir.App{
		Script: "clustered_repro.py",
		Functions: []ir.Function{
			{Name: "train_multi_node", GPU: "A100", Body: "return train()", IsClustered: true},
		},
	}
	rep := &leak.Report{}
	log := RewriteApp(app, rep)
	if len(log.Subs) != 1 {
		t.Fatalf("Subs = %+v, want exactly 1", log.Subs)
	}
	sub := log.Subs[0]
	if sub.Disposition != FlagCouple {
		t.Errorf("Disposition = %s, want %s (a clustered function must never CleanSwap, regardless of its own literal single-GPU gpu=)", sub.Disposition, FlagCouple)
	}
	if sub.Substituted != "" {
		t.Errorf("Substituted = %q, want empty — a FlagCouple site must NOT be substituted", sub.Substituted)
	}
}

// TestRewriteApp_ClusteredClsMethodFlagsWholeClass proves a @cls whose card
// is shared across @enter + methods (c.GPU) also FLAGs when ANY of its
// methods carries @modal.experimental.clustered(...) — the class's single
// gpu= site is coupled by construction if any method requests multi-node.
func TestRewriteApp_ClusteredClsMethodFlagsWholeClass(t *testing.T) {
	app := ir.App{
		Script: "clustered_cls_repro.py",
		Classes: []ir.Class{
			{
				Name: "Trainer", GPU: "H100", EnterBody: "self.ok = True",
				Methods: []ir.Function{
					{Name: "train", Body: "return train()", IsClustered: true},
				},
			},
		},
	}
	rep := &leak.Report{}
	log := RewriteApp(app, rep)
	if len(log.Subs) != 1 || log.Subs[0].Disposition != FlagCouple {
		t.Fatalf("Subs = %+v, want exactly 1 FlagCouple", log.Subs)
	}
}
