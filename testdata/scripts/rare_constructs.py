"""rare_constructs.py — calque#91 fixture: constructs that were either fully
invisible (Dict/Queue/NetworkFileSystem/App.include/.deploy) or silently
MISCATEGORIZED as an unrelated construct (CloudBucketMount used inline as a
volumes= value used to be indistinguishable from an ordinary Volume mount).

None of these are modeled — this fixture only proves each is now a distinct,
greppable leak (`where` in helper_leaks) instead of silence or a false
classification, per calque#91's "tag before you have real usage evidence" plan.
"""

import modal

app = modal.App("rare-constructs")

# modal.Dict.from_name(...) — before this fix, the assignment vanished entirely
# (no visit_Assign branch matched it), so nothing ever referenced `counters`
# showed up on the wire at all.
counters = modal.Dict.from_name("counters", create_if_missing=True)

# modal.Queue.from_name(...) — same invisibility as Dict.
work_queue = modal.Queue.from_name("work-queue", create_if_missing=True)

# modal.NetworkFileSystem.from_name(...) — same invisibility.
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
