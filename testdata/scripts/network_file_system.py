"""network_file_system.py — calque#91 Workstream B fixture: a real
modal.NetworkFileSystem.from_name(...) used as a network_file_systems= value,
the actual Modal idiom (network_file_systems= is Modal's own SEPARATE
decorator kwarg — never nested inside volumes= the way CloudBucketMount is).
The NetworkFileSystem is always assigned to a variable first (unlike
CloudBucketMount's inline-constructor idiom), then referenced by name in the
decorator kwarg — the same shape an ordinary Volume mount already uses.

This fixture's from_name string is a plain literal, so it must resolve to a
real (bring-your-own) EFS mount — ir.Function.NetworkFileSystems — not a
leak; the actual EFS filesystem discovery/mounting happens at real-run time
(internal/plan/efs.go), not at parse time.
"""

import modal

app = modal.App("network-file-system-fixture")

shared_fs = modal.NetworkFileSystem.from_name("shared-fs")


@app.function(network_file_systems={"/shared": shared_fs})
def use_shared_fs(x):
    return x
