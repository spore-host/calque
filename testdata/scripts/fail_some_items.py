"""fail_some_items.py — calque#145 slice 3 live-validation fixture: a plain
@app.function that fails on the FIRST attempt for specific input values,
then succeeds on any subsequent attempt — so a real fleet run reliably
produces calpool.Summary.Failed entries (D2's initial pass) AND then
reliably clears them on D4a's item-level re-drive, without needing to
manually kill anything.

State is a local marker file on the worker's own disk — a D4a redrive
runs on the SAME pool (often the same warm worker, per WarmHit) as the
original claim, so a marker written during the first attempt is still
there for the retry to see. Deliberately account/bucket-agnostic (unlike
an S3 marker) so this fixture works against ANY --bucket. This is a
validation fixture only, not part of the shipped test suite's assertions.
"""

import modal

app = modal.App("fail-first-attempt")


@app.function()
def maybe_fail_once(x):
    if x % 5 == 0:
        import os
        marker = f"/tmp/calque145-slice3-redrive-validate-{x}.marker"
        if not os.path.exists(marker):
            open(marker, "w").close()
            raise ValueError(f"deliberate FIRST-attempt failure for item {x}")
    return x * 2


@app.local_entrypoint()
def main():
    list(maybe_fail_once.map(range(10)))
