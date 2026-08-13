"""cross_app.py — calque#87 fixture: Function.from_name/Cls.from_name cross-app
invocation.

modal.Function.from_name(app_name, obj_name) / modal.Cls.from_name(...) look up
an ALREADY-DEPLOYED separate app by name — a real, structurally important
pattern for external consumers of a Modal deployment (confirmed on AI-Almanac's
forecasts_app.py: modal.Function.from_name("almanac-blending",
"score_live_forecast_bundle").remote(...)). calque has no notion of a
separately-deployed app to call into, so this must be recognized and leaked,
not silently produce an unexplained target-less invoke entry.

`caller` also uses Volume.from_name/Secret.from_name — both share the same
method name as Function.from_name/Cls.from_name but are unrelated, already-
correctly-handled constructs. They must NOT be misclassified as cross-app
invocation (an earlier version of this fix did exactly that against a real
script before being narrowed to Function/Cls specifically).
"""

import modal

app = modal.App("cross-app")

weights = modal.Volume.from_name("weights-cache")
api_key = modal.Secret.from_name("api-key")


@app.function(volumes={"/weights": weights}, secrets=[api_key])
def caller(x):
    remote_fn = modal.Function.from_name("other-app", "remote_worker")
    remote_cls = modal.Cls.from_name("other-app", "RemoteBatcher")
    remote_fn.remote(x)
    return remote_cls().generate.remote(x)


@app.local_entrypoint()
def main():
    list(caller.map(range(3)))
