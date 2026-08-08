"""concurrent_class.py — calque#82 fixture: @modal.concurrent stacked on @app.cls.

@modal.concurrent(max_inputs=, target_inputs=) is a SEPARATE decorator from
@app.cls, not one of @app.cls's own kwargs — real scripts stack both on the
same class. pyast's visit_ClassDef only read the @app.cls decorator's own
kwargs; a @modal.concurrent decorator on the same class was invisible.
max_inputs/target_inputs must merge into the same cls_kwargs map so they
reach the dedicated "behind the seam" autoscaling leak, not fall through
unrecognized.
"""

import modal

app = modal.App("concurrent-class")


@app.cls(gpu="A100")
@modal.concurrent(max_inputs=8, target_inputs=4)
class Batcher:
    @modal.enter()
    def enter(self):
        self.model = "loaded"

    @modal.method()
    def generate(self, prompt):
        return prompt


@app.local_entrypoint()
def main():
    Batcher().generate.remote("hi")
