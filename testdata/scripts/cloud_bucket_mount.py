"""cloud_bucket_mount.py — calque#91 Workstream A fixture: a real
modal.CloudBucketMount(...) used inline as a volumes= value, the actual
Modal idiom (constructed directly in the volumes= dict, never assigned to a
variable first). Unlike testdata/scripts/rare_constructs.py's own
CloudBucketMount usage (which stays leak-only in THAT fixture, proving the
"recognized but not modeled" tag still exists for a genuinely unresolvable
case), this fixture's bucket_name/key_prefix/read_only are all plain string/
bool literals, so it must resolve to a REAL S3 mount via mountpoint-s3 —
ir.Function.CloudBucketMounts — not a leak.
"""

import modal

app = modal.App("cloud-bucket-mount-fixture")


@app.function(volumes={"/data": modal.CloudBucketMount("my-real-bucket", key_prefix="foo/", read_only=True)})
def use_bucket_mount(x):
    return x
