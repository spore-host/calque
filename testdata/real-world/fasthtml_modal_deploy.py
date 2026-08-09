# Fetched during calque#79 corpus-expansion pass.
# Origin: arihanv/fasthtml-modal, file deploy.py
# https://github.com/arihanv/fasthtml-modal/blob/main/deploy.py
#
# Verbatim real-world source (only this header block added). Deliberately
# tiny -- a minimal real-world "deploy my existing web app on Modal" script:
# a plain @app.function wrapping a THIRD-PARTY (non-Modal) `fasthtml_app`
# object via @asgi_app, imported from a sibling module (`from app import
# fasthtml_app`) that itself has ZERO modal imports at all. Exercises the
# smallest possible serve-shaped script and the "import something built
# elsewhere, just re-expose it" idiom -- a real and probably common on-ramp
# pattern for adopters porting an existing web app to Modal, distinct from
# every other serve-shaped script in this corpus (which all define their web
# app inline).

from modal import App, Image, asgi_app
from app import fasthtml_app

image = Image.debian_slim(python_version="3.11").pip_install("python-fasthtml")

app = App("fasthtml-modal-template")


@app.function(image=image)
@asgi_app()
def get():
    return fasthtml_app
