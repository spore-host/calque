"""plain_function.py — calque#79/#80 fixture: plain @app.function, no @cls.

Mirrors a real, common shape (per the Pass 3 frequency survey in
docs/modal-compatibility-matrix.md: plain @app.function is ~2x as prevalent
as @app.cls in modal-labs/modal-examples, and earth-mover/forecast-datacube-
demo — a real forecasting pipeline — uses ONLY this shape: no @cls, no
@enter, no .map()). Before calque#80, pickWarmUnit refused any script
without a @cls+@enter warm unit; this fixture proves the fallback works.

`transform` is .map()'d in the entrypoint — the closest analog to the
class-based warm-unit shape. `greet` is never invoked via any recognized
idiom, exercising the plain "first function" fallback.
"""

import modal

app = modal.App("plain-function")


@app.function()
def transform(x):
    return x * 2


@app.function()
def greet(name):
    return f"hello {name}"


@app.local_entrypoint()
def main():
    list(transform.map(range(10)))
    greet.remote("world")
