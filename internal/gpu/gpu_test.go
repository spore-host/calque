package gpu

import "testing"

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
		name string
		raw  string
		body string
		want Disposition
	}{
		{"single card plain inference", "H100", "return self.llm.generate(prompt)", CleanSwap},
		{"no gpu", "", "return x * 2", NoGPU},
		{"multi-gpu count", "H100:8", "whatever", FlagMulti},
		{"explicit single is clean", "A100:1", "self.model(x)", CleanSwap},

		// Coupling signals in the body must FLAG even when count == 1. These are
		// the silent-downgrade cases §7 exists to prevent.
		{"torchrun", "A100", "os.system('torchrun --nproc 4 train.py')", FlagCouple},
		{"torch.distributed", "H100", "import torch.distributed as dist", FlagCouple},
		{"init_process_group", "A100", "dist.init_process_group(backend='nccl')", FlagCouple},
		{"nccl backend", "A100", "backend='nccl'", FlagCouple},
		{"deepspeed", "H100", "engine = deepspeed.initialize(model)", FlagCouple},

		// Regression: coupling token embedded inside a larger identifier. The
		// original \b-anchored regex missed this and silently clean-swapped a
		// tensor-parallel model. Must FLAG.
		{"tensor_parallel inside identifier", "A100", "self.model = build_tensor_parallel_model(world_size=4)", FlagCouple},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got, reason := evaluate(c.raw, c.body)
			if got != c.want {
				t.Errorf("evaluate(%q, %q) = %s (%s), want %s", c.raw, c.body, got, reason, c.want)
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
		if _, disp, _ := evaluate("A100", body); disp != FlagCouple {
			t.Errorf("body %q should FlagCouple, got %s", body, disp)
		}
	}
}
