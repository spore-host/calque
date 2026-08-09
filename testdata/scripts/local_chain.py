"""local_chain.py — calque#92 fixture: .local()-chained plain @app.functions.

Mirrors the real, blocking shape found on AI-Almanac's blending_app.py
(calque#79): `run_blend` calls `build_lat_lon_intermediates_bundle.local(...)`
then `train_blending_model_bundle.local(...)` — in-container pipeline chaining
between plain @app.functions, no new container per call.

`run_blend` is the picked warm unit (it's .map()'d). It .local()-calls
`stage_one`, which itself .local()-calls `stage_two` — proving the transitive
closure resolves two hops deep, not just the immediate call site. `run_blend`
also .local()-calls `Batcher().score`, a @cls METHOD — this must NOT be
shipped (no warm @enter state outside the picked unit) and must surface as an
honest leak rather than a silent NameError.
"""

import modal

app = modal.App("local-chain")


@app.function()
def stage_two(z):
    return str(z) + "-stage_two"


@app.function()
def stage_one(y):
    return stage_two.local(y) + "-stage_one"


@app.cls()
class Batcher:
    # No @modal.enter() — this class is here only so `score` exists as a real
    # @cls method for collectLocalExtras to skip+leak; it must never itself be
    # picked as the warm unit (pickWarmUnit skips any @cls with no @enter).
    @modal.method()
    def score(self, x):
        return x


@app.function()
def run_blend(x):
    a = stage_one.local(x)
    if x is None:
        # Never reached with a real payload — exists so the .local() call site
        # is present for the PARSER to find (proving the "not shipped, honest
        # leak" path for a @cls-method target) without crashing every item at
        # runtime, since score is deliberately never shipped.
        a = Batcher().score.local(a)
    return a


@app.local_entrypoint()
def main():
    list(run_blend.map(range(3)))
