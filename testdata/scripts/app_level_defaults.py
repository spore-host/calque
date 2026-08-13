"""app_level_defaults.py — calque#168 fixture: App(volumes=..., secrets=...)
app-level defaults inherited by a function/class declaring neither.

Before this fix, App-level volumes=/secrets= were silently dropped with NO
leak at all — worse than App(image=...), which at least surfaced a generic
leak. `plain_fn` declares neither volumes= nor secrets= of its own and must
inherit both from the App; `Scorer` (a @cls) does the same, and its method
must in turn inherit from the class (which inherited from the App) — the
same fallback-if-own-is-empty chain buildClass already used for gpu=/
volumes= one level down (class -> method), now extended one level up
(App -> class/function).

`overridden_fn` declares its OWN volumes= and must NOT be overwritten by
the App-level default — inheritance only fills in when a callable declares
NONE of its own.
"""

import modal

weights = modal.Volume.from_name("weights-cache")
own_volume = modal.Volume.from_name("own-cache")
api_key = modal.Secret.from_name("api-key")
app = modal.App("app-level-defaults", volumes={"/weights": weights}, secrets=[api_key])


@app.function()
def plain_fn(x):
    return x


@app.function(volumes={"/own": own_volume})
def overridden_fn(x):
    return x


@app.cls()
class Scorer:
    @modal.enter()
    def load(self):
        pass

    @modal.method()
    def score(self, x):
        return x


@app.local_entrypoint()
def main():
    plain_fn.remote(1)
    overridden_fn.remote(1)
    Scorer().score.remote(1)
