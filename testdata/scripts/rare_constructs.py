"""rare_constructs.py — calque#91 fixture: constructs that were either fully
invisible (Dict/Queue/NetworkFileSystem/App.include/.deploy) or silently
MISCATEGORIZED as an unrelated construct (CloudBucketMount used inline as a
volumes= value used to be indistinguishable from an ordinary Volume mount).

Dict/Queue and App.include/.deploy stay unmodeled — this fixture proves each
is a distinct, greppable leak (`where` in helper_leaks) instead of silence or
a false classification, per calque#91's "tag before you have real usage
evidence" plan. modal.NetworkFileSystem is DIFFERENT as of Workstream B: its
from_name(...) binding is now structurally tracked (mirroring Volume's own
zero-leak-on-just-the-binding posture) — see network_file_system.py for the
positive (real EFS mount) case. shared_fs below is bound but never used as a
network_file_systems= mount value, so it emits no leak at all here, same as
an unused Volume var wouldn't.
"""

import modal

app = modal.App("rare-constructs")

# modal.Dict.from_name(...) — before this fix, the assignment vanished entirely
# (no visit_Assign branch matched it), so nothing ever referenced `counters`
# showed up on the wire at all.
counters = modal.Dict.from_name("counters", create_if_missing=True)

# modal.Queue.from_name(...) — same invisibility as Dict.
work_queue = modal.Queue.from_name("work-queue", create_if_missing=True)

# modal.NetworkFileSystem.from_name(...) — structurally tracked (calque#91
# Workstream B), not leaked merely for existing; never used as a mount here.
shared_fs = modal.NetworkFileSystem.from_name("shared-fs")

# modal.CloudBucketMount(...) used INLINE as a volumes= value (the real Modal
# idiom — CloudBucketMount is constructed directly in the volumes= dict, not
# assigned to a variable first) — before this fix, this was silently treated
# as an ordinary Volume mount (no leak at all), since _volumes_map's non-Name
# fallback just unparsed it with no distinguishing tag.
@app.function(volumes={"/data": modal.CloudBucketMount("my-bucket", secret=None)})
def use_bucket_mount(x):
    return x


@app.local_entrypoint()
def main():
    counters.put("hits", 1)
    work_queue.put("job")
    app.include(modal.App("other"))
