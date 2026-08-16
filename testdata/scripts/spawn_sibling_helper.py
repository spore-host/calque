"""spawn_sibling_helper.py — calque#198 fixture: a .spawn()'d callable
whose body delegates to a private module-level sibling helper, mirroring
AI-Almanac's forecasts_app.py's EXACT real shape:

    def run_season_forecast_bundle(job_id, model_id, config, season_params):
        return _season_bundle_impl(job_id, model_id, config, season_params)

A thin @app.function wrapper delegating to a plain (non-decorated) helper
is an entirely ordinary Python pattern — keeping the decorated surface
small while the real logic lives in a testable, undecorated function.
Before calque#198, calque spawn-run shipped a spawned callable's
MethodBody 100% verbatim with NO sibling-function resolution at all
(unlike calque real/fleetrun, which already resolve this via
collectLocalExtras) — a real run against forecasts_app.py failed with
`name '_season_bundle_impl' is not defined`.
"""

import modal

app = modal.App("spawn-sibling-helper")


def _bundle_impl(job_id, tag):
    # The real logic lives here, never decorated — exactly _season_bundle_impl's
    # own shape in forecasts_app.py.
    return {"job_id": job_id, "tag": tag}


@app.function()
def run_bundle(job_id, tag):
    return _bundle_impl(job_id, tag)


@app.function()
def caller(job_id):
    call = run_bundle.spawn(job_id, "season-2024")
    return call.get()


@app.local_entrypoint()
def main():
    caller.remote("job-1")
