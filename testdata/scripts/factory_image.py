"""factory_image.py — calque#76 fixture: image built via a factory function.

Found auditing AI-Almanac's real Modal scripts (forecasts_app.py): a common
pattern the pyast AST walker's visit_Assign does not see through — an Image
chain built inside a function and assigned via the function's return value,
rather than a direct `x = modal.Image....` chain at module scope. The
function's image=<var> reference then never resolves, and without a guard
resolveImage() would silently substitute a DIFFERENT image with no leak.

`render` uses a directly-resolvable image (no leak expected); `gpu_work`
uses the factory-built one (must leak, and must NOT silently inherit
render_image).
"""

import modal

app = modal.App("factory-image")


def _build_gpu_image():
    return modal.Image.debian_slim().pip_install("torch")


render_image = modal.Image.debian_slim().pip_install("numpy")
_gpu_image = _build_gpu_image()


@app.function(image=_gpu_image, gpu="A100")
def gpu_work(x):
    return x


@app.function(image=render_image)
def render(x):
    return x
