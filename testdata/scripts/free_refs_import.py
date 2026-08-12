"""free_refs_import.py — calque#146 fixture: a bare (non-.local()) reference
to a module-level IMPORT, the gap calque#139's own fix explicitly left open
("deliberately does NOT attempt to resolve imports"). Mirrors free_refs.py's
exact shape but for `import X` / `from X import Y` instead of a plain
function/constant.

`Path` (from `from pathlib import Path`, a plain top-level import) is read
BARE inside `load`'s @enter body — no `.local()`, no re-export, just an
ordinary Python module using its own top-level import. Before calque#146
this NameError'd unconditionally, even though calque#139 already shipped
bare-referenced functions/constants. `modal` itself hits the exact same gap
(this script's own `import modal` is never referenced from inside a body,
but every real AI-Almanac script's picked unit DOES bare-reference `modal`
via `modal.Volume.from_name(...)` or similar at module scope, reached
transitively through the picked unit's body).
"""

import modal
from pathlib import Path

app = modal.App("free-refs-import")


@app.cls()
class Worker:
    @modal.enter()
    def load(self):
        self.base = Path("/tmp")  # bare import reference, no .local()

    @modal.method()
    def run(self, name):
        return str(self.base / name)


@app.local_entrypoint()
def main():
    list(Worker().run.map(["a", "b", "c"]))
