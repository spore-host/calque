"""portable_config.py — M6 fixture: portable decorator config + sync invocation idioms.

Not one of the three prescribed scripts (§15). It exercises the parser's B/C
pass-through: the portable kwargs (cpu/memory/retries/region) that should land in
ir.Config, an autoscaling kwarg (keep_warm) that must be recognized-and-leaked as
behind-the-seam (§4/§1, M10-S1), and the synchronous invocation idioms
(.starmap / .for_each / .remote) that should be classified beyond plain .map.
"""

import modal

app = modal.App("portable-config")

image = modal.Image.debian_slim().pip_install("numpy")


@app.function(cpu=4, memory=8192, retries=3, region="us-west-2", keep_warm=1, image=image)
def transform(x):
    return x * 2


@app.function(cpu=2)
def combine(a, b):
    return a + b


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
