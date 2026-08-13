# guide

Task-oriented HOW-TO documentation — start here if you just want to run
something. This is a different genre from [`../README.md`](../README.md)'s
design-doc index one level up: those files each answer a specific design
*question* (why does calque do X, not Y); these answer "how do I actually
do X."

- [`getting-started.md`](getting-started.md) — clone → build → analyze →
  dry-run → first billable smoke test → first real AWS run, in order.
  Start here if you haven't run anything yet.
- [`which-verb.md`](which-verb.md) — `real` vs `ramp` vs `pool` vs
  `spawn-run` vs `session` vs `--shards`: which command for which
  workload shape, and `real`'s own flag sub-choices once you're driving
  your own script instead of the reference vLLM body.
- [`cli-reference.md`](cli-reference.md) — every flag, for every command
  and subcommand, sourced directly from the CLI's own flag definitions.
- [`troubleshooting.md`](troubleshooting.md) — real problems already hit
  and fixed, as symptom → cause → fix, so the next person gets a
  documented answer instead of re-deriving the cause from a bootstrap log.

For "does calque support the specific Modal construct my script uses" or
"what does calque deliberately not do," see the design-doc index at
[`../README.md`](../README.md) instead — that's a different, complementary
set of questions this `guide/` tree doesn't try to answer.
