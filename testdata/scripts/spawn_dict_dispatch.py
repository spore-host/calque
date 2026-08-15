"""spawn_dict_dispatch.py — calque#189 fixture: .spawn() on a dict-of-
functions Subscript selected by a runtime key, e.g. AI-Almanac's
forecasts_app.py's `SEASON_BUNDLE_FNS[_model_env(model_id)].spawn(...)`.

Before calque#189, `_attr_chain` couldn't see through the Subscript node
between the dict-lookup and `.spawn`, so the call site's target resolved to
an empty string — invisible to any driver keyed by target name. Now
`_callable_dicts` tracks every function ever assigned into the dict via
`NAME[<key>] = some_function` (regardless of what <key> is — it's a loop
variable here, never a literal, so the actual runtime-selected callable
still isn't resolvable), and a `.spawn()` on a Subscript of a known such
dict lists every candidate instead of going silently empty.
"""

import modal

app = modal.App("spawn-dict-dispatch")

REGISTRY = {}


for _env in ("a", "b"):

    @app.function(name=f"bundle_{_env}")
    def bundle(job_id, config):
        # Returns BOTH args (not just job_id) so a real end-to-end test can
        # prove config bound correctly too — calque#191 found that only
        # MethodArg (the FIRST non-self/cls name) was ever populated for a
        # spawned callable, silently leaving every arg past it undefined.
        return {"job_id": job_id, "config": config}

    REGISTRY[_env] = bundle


@app.function()
def dispatcher(job_id, env, config):
    # The real callable is chosen by a RUNTIME key (env) — not statically
    # resolvable, matching forecasts_app.py's _model_env(model_id) exactly.
    call = REGISTRY[env].spawn(job_id, config)
    return call.get()


@app.local_entrypoint()
def main():
    dispatcher.remote("job-1", "a", {})
