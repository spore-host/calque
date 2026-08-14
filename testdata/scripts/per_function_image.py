"""per_function_image.py — calque#174 fixture: two DIFFERENT resolvable
images, each referenced by its OWN function's explicit image= kwarg.

Before this fix, resolveImage() picked exactly ONE image variable for the
WHOLE script (preferring one literally named "image", else the
lexicographically first) and assigned it to ir.App.Image — regardless of
which function's decorator actually referenced it. `special_fn` here
explicitly declares image=special_image but would have silently gotten
plain_fn's default_image instead (or vice versa, depending on lexicographic
ordering) with no leak at all, since BOTH images resolve cleanly — this is
not the same failure mode as factory_image.py (calque#76), where the
reference itself is unresolvable.

`plain_fn` declares no image= of its own and must inherit the App-level
default (default_image, calque#168's inheritance mechanism extended to
image= by #174).
"""

import modal

default_image = modal.Image.debian_slim().pip_install("torch")
special_image = modal.Image.debian_slim().pip_install("numpy")
app = modal.App("per-function-image", image=default_image)


@app.function(image=special_image)
def special_fn(x):
    return x


@app.function()
def plain_fn(x):
    return x


@app.cls(image=special_image)
class SpecialCls:
    @modal.enter()
    def load(self):
        pass

    @modal.method()
    def run(self, x):
        return x


@app.cls()
class PlainCls:
    @modal.enter()
    def load(self):
        pass

    @modal.method()
    def run(self, x):
        return x


@app.local_entrypoint()
def main():
    special_fn.remote(1)
    plain_fn.remote(1)
    SpecialCls().run.remote(1)
    PlainCls().run.remote(1)
