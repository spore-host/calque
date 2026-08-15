"""network_file_system_create_if_missing.py — calque#91 Workstream B: proves
create_if_missing=True on modal.NetworkFileSystem.from_name(...) leaks
distinctly (calque never auto-creates an EFS filesystem; bring-your-own
only), while the mount itself still resolves normally.
"""

import modal

app = modal.App("nfs-create-if-missing-fixture")

shared_fs = modal.NetworkFileSystem.from_name("shared-fs", create_if_missing=True)


@app.function(network_file_systems={"/shared": shared_fs})
def use_shared_fs(x):
    return x
