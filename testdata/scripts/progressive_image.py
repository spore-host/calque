"""progressive_image.py — calque#140 fixture: an image built across MULTIPLE
statements that reassign the same variable name, rather than one expression.

Found auditing `caru-ini/modal-comfyui` (a real production ComfyUI-on-Modal
deployment, saved as testdata/real-world/modal_comfyui.py): its image is built
by an initial `image = modal.Image.debian_slim()....` chain, then extended by
LATER, separate `image = image.<step>(...)` statements — including one nested
inside an `if` (a runtime-conditional append). Before calque#140's fix,
`_walk_image_chain` correctly resolved the FIRST statement (real base + steps)
but every later `image = image.step(...)` statement's chain-walk climbed to a
bare `Name` (not a base-constructor `Call`), so `base_unresolved` was true and
`visit_Assign` OVERWROTE the whole record — discarding the real base and every
earlier step. The fix merges: a reassignment whose root climbs to an
ALREADY-KNOWN image variable now chain-extends that variable's prior record
(keeping its base, appending its own steps) instead of replacing it.

Three reassignments here (one unconditional, one nested inside an `if`, one
more unconditional after it) exercise that the merge holds across more than
one extension AND regardless of AST nesting depth — `generic_visit` already
walks into `if` bodies, so the merge logic (scoped to visit_Assign, not to
module-level-only assigns) fires for the nested one too.
"""

import pathlib

import modal

app = modal.App("progressive-image")

workflow_file_path = pathlib.Path("/tmp/workflow.json")

# Statement 1: fully resolved in ONE expression — real base + two steps.
image = modal.Image.debian_slim().pip_install("torch", "vllm").apt_install("git")

# Statement 2: separate statement, same var name — chain roots at the bare
# Name `image`, not a base constructor. Must chain-extend statement 1's
# record, not overwrite it.
image = image.env({"HF_HUB_ENABLE_HF_TRANSFER": "1"})

# Statement 3: a conditional reassignment — same merge must apply even though
# this Assign is nested inside an `if`, not at module top level.
if workflow_file_path.exists():
    image = image.add_local_file(str(workflow_file_path), "/root/workflow.json")

# Statement 4: another unconditional reassignment after the conditional one —
# proves the merge chain holds across more than two statements.
image = image.run_commands("echo done")


@app.function(image=image, gpu="H100")
def f(x):
    return x
