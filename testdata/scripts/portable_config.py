"""portable_config.py — M6 fixture: portable decorator config + sync invocation idioms.

Not one of the three prescribed scripts (§15). It exercises the parser's B/C
pass-through: the portable kwargs (cpu/memory/retries/region) that should land in
ir.Config, an autoscaling kwarg (keep_warm) that must be recognized-and-leaked as
behind-the-seam (§4/§1, M10-S1), and the synchronous invocation idioms
(.starmap / .for_each / .remote) that should be classified beyond plain .map.

`bin_pack` additionally exercises cpu=(request, limit) — the same
request/limit tuple convention Modal also allows for memory= (calque#77).

Two @app.local_entrypoint()s (`main`, `secondary`) exercise a script with
more than one local entrypoint — a real pattern (calque#78) that must not
silently collapse to just one.

`combine`'s body calling `bin_pack.local(...)` exercises .local() call-site
recognition (calque#81) — inline in-container invocation, not a separate
warm unit.
"""

import modal

app = modal.App("portable-config")

image = modal.Image.debian_slim().pip_install("numpy")


@app.function(cpu=4, memory=8192, retries=3, region="us-west-2", keep_warm=1, image=image)
def transform(x):
    return x * 2


@app.function(cpu=2)
def combine(a, b):
    return a + b + bin_pack.local(a)


@app.function(cpu=(0.25, 1))
def bin_pack(x):
    return x


@app.local_entrypoint()
def main():
    # .map — items -> results (already supported)
    list(transform.map(range(100)))
    # .starmap — tuple-splat args
    list(combine.starmap([(1, 2), (3, 4)]))
    # .for_each — side effects, no result collection
    transform.for_each(range(10))
    # .remote — a single blocking call
    combine.remote(5, 6)


@app.local_entrypoint()
def secondary():
    bin_pack.remote(1)
