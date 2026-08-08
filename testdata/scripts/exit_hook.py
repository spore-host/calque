"""exit_hook.py — calque#86 fixture: @modal.exit(), the documented pair to @enter.

Before the fix, pyast's visit_ClassDef had no `.endswith("exit")` check —
a @modal.exit()-decorated method fell into the same untagged "plain method"
bucket as an ordinary method. If it were ever the ONLY method on the class
(as here), pickWarmUnit's "fall back to first method" would pick it as the
per-item warm unit method, running teardown logic on every item instead of
once at shutdown.

`generate` proves @modal.exit() is excluded from the callable method list
(picked correctly over `cleanup` even though `cleanup` appears first in
source order); teardown itself is leaked as not reproduced, never silently
dropped.
"""

import modal

app = modal.App("exit-hook")


@app.cls(gpu="A100")
class Batcher:
    @modal.enter()
    def enter(self):
        self.model = "loaded"

    @modal.exit()
    def cleanup(self):
        print("shutting down")

    @modal.method()
    def generate(self, prompt):
        return prompt


@app.local_entrypoint()
def main():
    list(Batcher().generate.map(["a", "b", "c"]))
