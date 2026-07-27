"""volume_commit.py — M8 fixture: a volume that is WRITTEN back (§E).

Exercises volume.commit() (end-of-run write-back, honored) and volume.reload()
(mid-run re-read, a semantic gap the spike leaks). Distinct from volume_cache.py,
which only reads.
"""

import modal

app = modal.App("volume-commit")

image = modal.Image.debian_slim().pip_install("numpy")
cache = modal.Volume.from_name("compute-cache", create_if_missing=True)


@app.cls(gpu="L4", image=image, volumes={"/cache": cache})
class Compute:
    @modal.enter()
    def load(self):
        self.n = 0

    @modal.method()
    def step(self, x):
        with open("/cache/out.txt", "a") as f:
            f.write(str(x))
        self.n += 1
        return x


@app.local_entrypoint()
def main():
    list(Compute().step.map(range(100)))
    # write the mutated volume back so a later run sees it
    cache.commit()
    # a mid-run re-read of a mutated volume — the deferred semantic
    cache.reload()
