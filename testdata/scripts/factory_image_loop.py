"""factory_image_loop.py — calque#179 fixture: a module-level for-loop over
a dict-comprehension of images, registering N differently-imaged functions
from decorated defs that appear ONCE in source but are re-executed by real
Python once per (key, value) pair.

Mirrors the idiom in AI-Almanac's forecasts_app.py (github.com/AI-Almanac/
ai-almanac) — its `_inference_image(extras)` factory takes one argument,
built from a dict comprehension over `_INFERENCE_EXTRAS`, then consumed by
a `for _env, _image in INFERENCE_IMAGES.items():` loop whose body defines
TWO @app.function-decorated defs per iteration
(`run_forecast_inference`/`warm_model_weights`) — never copied verbatim,
per CONTRIBUTING.md's no-vendoring policy; this is a synthetic, structurally
equivalent stand-in with different names/packages:

    def _inference_image(extras):
        spec = ",".join(extras)
        return modal.Image.from_registry(...).run_commands(f"...[{spec}]...")

    _INFERENCE_EXTRAS = {"base": [...], "aifs2": [...]}
    INFERENCE_IMAGES = {env: _inference_image(extras) for env, extras in _INFERENCE_EXTRAS.items()}

    for _env, _image in INFERENCE_IMAGES.items():
        @app.function(name=f"run_forecast_inference_{_env}", image=_image, gpu="A100-80GB")
        def run_forecast_inference(job_id): ...

        @app.function(name=f"warm_model_weights_{_env}", image=_image, gpu="A100-80GB")
        def warm_model_weights(): ...

Before calque#179, NEITHER function resolved its real per-env image at all
— pyast's walker only produces ONE ir.Function per AST FunctionDef, had no
for-loop modeling, no dict-comprehension image tracking, no parameterized-
factory support, and never resolved f-strings to literals. calque#179 closes
all four gaps together: `do_inference_alpha`/`do_warm_alpha` and
`do_inference_beta`/`do_warm_beta` below must each resolve their OWN real
image, with the correct per-env `extras` value folded into the pip spec.
"""

import modal

app = modal.App("factory-image-loop")


def _worker_image(extras: list[str]) -> modal.Image:
    spec = ",".join(extras)
    return (
        modal.Image.debian_slim()
        .pip_install("uv")
        .run_commands(f"uv pip install 'examplepkg[{spec}]'")
    )


_WORKER_EXTRAS = {
    "alpha": ["a1", "a2"],
    "beta": ["b1"],
}
WORKER_IMAGES = {env: _worker_image(extras) for env, extras in _WORKER_EXTRAS.items()}

for _env, _image in WORKER_IMAGES.items():

    @app.function(name=f"do_inference_{_env}", image=_image, gpu="A100-80GB")
    def do_inference(x: int) -> int:
        return x

    @app.function(name=f"do_warm_{_env}", image=_image, gpu="A100-80GB")
    def do_warm() -> None:
        return None
