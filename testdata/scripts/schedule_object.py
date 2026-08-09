"""schedule_object.py — calque#91 fixture: modal.Cron(...)/modal.Period(...) OBJECT
forms for schedule=.

Before this fix, schedule=modal.Cron(...)/modal.Period(...) fell through pyast's
generic `_literal` fallback (a Call node ast.literal_eval can't touch), landing on
the wire as {"__unparsed__": "modal.Cron(...)"} — which decodeString then failed to
parse as a plain string, so ir.Config.Schedule ended up holding the stringified JSON
garbage itself, while the schedule=-specific leak still fired claiming a value was
"recorded" (misleadingly, since what got recorded was unusable).

`cron_job` exercises modal.Cron(cron_string, timezone=) — the cron string lands in
Config.Schedule verbatim; timezone= is discarded (the bare-string schedule= form
never carried timezone info either).

`period_job` exercises modal.Period(days=, hours=, ...) — Modal's own Period
combines any subset of days/hours/minutes/seconds ADDITIVELY (not preference-
ordered), so calque normalizes them into one "<n>d<n>h<n>m<n>s" string.

`bare_string_job` is the pre-existing bare-string schedule= case (a plain cron
string, no object form) — kept here as a regression guard so this fixture also
proves the object-form recognition didn't disturb the already-working path.
"""

import modal

app = modal.App("schedule-object")


@app.function(schedule=modal.Cron("0 * * * *", timezone="UTC"))
def cron_job():
    return "hourly"


@app.function(schedule=modal.Period(days=1, hours=6))
def period_job():
    return "every 1d6h"


@app.function(schedule="0 0 * * *")
def bare_string_job():
    return "daily"
