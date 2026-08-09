"""dunder_lifecycle.py — calque#138 fixture: pre-1.0 Modal API class lifecycle.

Before @modal.enter()/@modal.exit() existed, Modal's class-based lifecycle
was expressed via Python's own context-manager dunders — __enter__(self) /
__exit__(self, exc_type, exc_value, traceback) — defined directly on an
@app.cls(...)-decorated class, no decorator at all. This is Modal's
ORIGINAL API (confirmed against a real, still-online script, see
testdata/real-world/modal_sqlcoder.py), not a hypothetical.

Before the fix, pyast's visit_ClassDef only recognized @modal.enter()/
@modal.exit() by decorator name — a bare __enter__/__exit__ had no decorator
at all and fell into the same untagged "plain method" bucket as a real
@modal.method(). `generate` proves __enter__/__exit__ are excluded from the
callable method list (the only remaining method is the real one); enter/exit
are populated from the dunders with no decorator present.
"""

import modal

app = modal.App("dunder-lifecycle")


@app.cls(gpu="A100")
class Batcher:
    def __enter__(self):
        self.model = "loaded"

    def __exit__(self, exc_type, exc_value, traceback):
        print("shutting down")

    @modal.method()
    def generate(self, prompt):
        return prompt


@app.local_entrypoint()
def main():
    list(Batcher().generate.map(["a", "b", "c"]))
