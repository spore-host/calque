"""factory_image.py — calque#76/#175 fixture: image built via a factory function.

Found auditing AI-Almanac's real Modal scripts (forecasts_app.py): a common
pattern the pyast AST walker's visit_Assign does not see through — an Image
chain built inside a function and assigned via the function's return value,
rather than a direct `x = modal.Image....` chain at module scope.

calque#175 taught the walker to inline a TRIVIAL factory (no control flow,
one unconditional return) — see factory_image_trivial.py for that now-
resolving case. `_build_gpu_image` here is deliberately NOT trivial: it
branches on an argument, so it must STILL leak rather than silently
resolve or silently inherit `render_image`. This preserves calque#76's
original regression coverage now that the trivial case is handled
separately.

`render` uses a directly-resolvable image (no leak expected); `gpu_work`
uses the branching factory-built one (must leak, and must NOT silently
inherit render_image).
"""

import modal

app = modal.App("factory-image")


def _build_gpu_image(use_cuda=True):
    if use_cuda:
        return modal.Image.debian_slim().pip_install("torch")
    return modal.Image.debian_slim().pip_install("torch-cpu")


render_image = modal.Image.debian_slim().pip_install("numpy")
_gpu_image = _build_gpu_image()


@app.function(image=_gpu_image, gpu="A100")
def gpu_work(x):
    return x


@app.function(image=render_image)
def render(x):
    return x
