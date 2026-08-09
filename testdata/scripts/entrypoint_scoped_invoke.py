"""entrypoint_scoped_invoke.py — calque#98 fixture: two entrypoints, two
UNRELATED warm-unit candidates.

This is the exact repro shape from the issue: a script with `do_train()`
calling a `.map()`'d @cls+@enter unit (`Trainer.train_step`) and a SEPARATE
`do_evaluate()` calling a wholly different callable (`evaluate`) via
`.remote()`. Before calque#98, pickWarmUnit scanned the WHOLE script for the
best `.map()`'d @cls+@enter callable with zero awareness of which entrypoint
was selected — `--entrypoint do_evaluate` still picked `train_step` (the
script's only `.map()`'d callable), never `evaluate`.

`train_step` must be attributed ONLY to `do_train`; `evaluate` must be
attributed ONLY to `do_evaluate` — proving pyast's entrypoint-scope stack
(and internal/parse's per-entrypoint invoke-kind view) correctly partitions
call sites by which entrypoint's body they were found in, rather than
folding both into one whole-script-flat union.
"""

import modal

app = modal.App("entrypoint-scoped-invoke")


@app.cls(gpu="H100")
class Trainer:
    @modal.enter()
    def load(self):
        self.model = "loaded"

    @modal.method()
    def train_step(self, batch: int) -> int:
        return batch * 2


@app.function()
def evaluate(x: int) -> int:
    return x + 1


@app.local_entrypoint()
def do_train():
    list(Trainer().train_step.map(range(10)))


@app.local_entrypoint()
def do_evaluate():
    evaluate.remote(1)
