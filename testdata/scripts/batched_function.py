"""batched_function.py — calque#91 fixture: @modal.batched(...) stacked on a
plain @app.function, Modal's automatic request-coalescing decorator.

Before this fixture existed, @modal.batched had ZERO recognition at all —
unlike its four from_name/CloudBucketMount siblings in rare_constructs.py,
it fell through completely unnoticed (no leak, no tag). This fixture only
proves the decorator is now a distinct, greppable leak (`where` ==
"modal.batched" in helper_leaks) — batching itself is NOT modeled; `process`
still runs, just without Modal's list-coalescing behavior.
"""

import modal

app = modal.App("batched-function")


@app.function()
@modal.batched(max_batch_size=4, wait_ms=100)
def process(items: list[int]) -> list[int]:
    return [i * 2 for i in items]
