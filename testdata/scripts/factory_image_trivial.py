"""factory_image_trivial.py — calque#175 fixture: a TRIVIAL (unconditional,
no-branching) image factory, the shape calque#76's original fix left
unresolved and calque#175 now inlines.

Mirrors the real idiom in AI-Almanac's blending_app.py (github.com/
AI-Almanac/ai-almanac) — its `_image()` factory reads two env vars into
locals, then returns one unconditional `modal.Image....` chain, with no
`if`/`for`/`while`/`try` anywhere in its body:

    def _image() -> modal.Image:
        repo_url = os.environ.get("ALMANAC_BLENDING_REPO_URL", DEFAULT_REPO_URL)
        repo_ref = os.environ.get("ALMANAC_BLENDING_REPO_REF", DEFAULT_REPO_REF)
        return (
            modal.Image.debian_slim(python_version="3.11")
            .apt_install("build-essential", "git", "libgeos-dev")
            ...
        )

Before calque#175, `blending_image = _image()` never entered pyast's
`self.images` at all (the walker only matches a direct attribute-chain
call, not a call to a bare Name) — every function relying on it silently
fell back to the app-wide default image with a leak naming the mismatch.
calque#175 widened the walker to look inside a zero-arg, undecorated,
control-flow-free factory's single Return and resolve it directly.

`worker` below must resolve `factory_image` with ZERO leak — contrast with
factory_image.py's `gpu_work`, whose factory branches and must still leak.
"""

import os

import modal

DEFAULT_REPO_URL = "https://github.com/example/repo"
DEFAULT_REPO_REF = "main"
CLONE_ROOT = "/opt/build"

app = modal.App("factory-image-trivial")


def _image():
    repo_url = os.environ.get("EXAMPLE_REPO_URL", DEFAULT_REPO_URL)
    repo_ref = os.environ.get("EXAMPLE_REPO_REF", DEFAULT_REPO_REF)
    return (
        modal.Image.debian_slim(python_version="3.11")
        .apt_install("build-essential", "git")
        .pip_install("uv")
        .run_commands(f"git clone --depth 1 {repo_url} {CLONE_ROOT}")
        .pip_install("google-cloud-storage")
    )


factory_image = _image()


@app.function(image=factory_image)
def worker(x):
    return x
