"""volume_cache.py — Volume weight reuse across runs (spec §15, script 3).

Stresses S3 weight-cache plumbing and image/volume separation: the model weights
live in a Volume that is populated once and reused, distinct from the container
image. Exercises the Volume -> S3 prefix mapping in the plan stage.

Also intentionally includes a plain @app.function (not a @cls) using the Volume,
to exercise the function path alongside the class path.
"""

import modal

app = modal.App("volume-cache")

# The weights Volume: written once (download_weights), read every run (score).
weights = modal.Volume.from_name("resnet-weights", create_if_missing=True)

image = modal.Image.debian_slim().pip_install("torch==2.4.1", "torchvision==0.19.1")


@app.function(image=image, volumes={"/models": weights}, timeout=900)
def download_weights():
    # One-time population of the Volume. On a warm cache this is a no-op — the
    # rebuild/repopulate-on-no-change case is a leak to watch (§10).
    import os
    import torchvision

    if not os.path.exists("/models/resnet50.pth"):
        m = torchvision.models.resnet50(weights="IMAGENET1K_V2")
        import torch

        torch.save(m.state_dict(), "/models/resnet50.pth")
    return "weights ready"


@app.cls(gpu="L4", image=image, volumes={"/models": weights}, timeout=600)
class Scorer:
    @modal.enter()
    def load(self):
        import torch
        import torchvision

        self.model = torchvision.models.resnet50()
        self.model.load_state_dict(torch.load("/models/resnet50.pth"))
        self.model.eval().cuda()

    @modal.method()
    def score(self, image_id: int) -> int:
        # Placeholder scoring; payload body is never parsed by the control plane.
        return image_id % 1000


@app.local_entrypoint()
def main(n: int = 500):
    download_weights.remote()
    scores = list(Scorer().score.map(range(n)))
    print(f"scored {len(scores)} images")
