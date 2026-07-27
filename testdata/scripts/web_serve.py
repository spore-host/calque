"""web_serve.py — M9 fixture: a SERVE-shaped app (§F).

A long-lived, request-driven entrypoint — Modal's @web_endpoint / @asgi_app. This
is a fundamentally different execution shape from the batch .map the spike measures
(§16 success is batch+K), so calque should DETECT it, gate it (a served Bedrock
model still routes away), and LEAK the deferred shape — never crash, never treat it
as batch. The server itself is out of spike scope (M9/F3 design note).
"""

import modal

app = modal.App("web-serve")

image = modal.Image.debian_slim().pip_install("fastapi", "vllm")


@app.function(gpu="H100", image=image)
@modal.web_endpoint(method="POST")
def generate(prompt: str):
    from vllm import LLM

    llm = LLM(model="meta-llama/Meta-Llama-3-8B-Instruct")
    return llm.generate(prompt)


@app.function(image=image)
@modal.asgi_app()
def api():
    from fastapi import FastAPI

    web = FastAPI()
    return web
