"""spawn_fanout.py — calque#88 fixture: .spawn() over distinct callables.

Per the real-world frequency survey (docs/modal-compatibility-matrix.md),
.spawn()+.get() is a common "trigger and poll" pattern (~7% of files) —
confirmed as the exact idiom AI-Almanac's forecasts_app.py uses to fan out
N independent model-inference calls in parallel before collecting all of
them. calque#88 classifies each .spawn()'d callable (ir.InvokeSpawn) and
makes it findable via ir.App.FindFunction/FindClass, without executing the
fan-out itself (that's calque#97, a separate driver).

`worker_a`/`worker_b` are spawned from `caller`, which is itself .map()'d —
proving InvokeSpawn classification doesn't interfere with `caller` being
correctly selected as the warm unit (rank precedence: InvokeMap beats
InvokeSpawn).
"""

import modal

app = modal.App("spawn-fanout")


@app.function()
def worker_a(x):
    return x * 2


@app.function()
def worker_b(x):
    return x + 1


@app.function()
def caller(x):
    call_a = worker_a.spawn(x)
    call_b = worker_b.spawn(x)
    return call_a.get() + call_b.get()


@app.local_entrypoint()
def main():
    list(caller.map(range(3)))
