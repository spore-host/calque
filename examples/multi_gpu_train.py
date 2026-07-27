"""multi_gpu_train.py — adversarial fixture: MUST be flagged, never substituted.

Not one of the three prescribed scripts (§15). It exists to prove the §7 guard
can say NO: it trips both flag paths. A guard that only ever clean-swaps hasn't
been shown to guard anything. calque must refuse these, not silently downgrade
them onto a single RTX PRO 6000.

  - finetune:  gpu="H100:8"  -> FLAG multi-GPU (out of single-node scope)
  - shard:     gpu="A100"    -> FLAG coupled (torchrun / tensor-parallel in body)
"""

import modal

app = modal.App("multi-gpu-train")

image = modal.Image.debian_slim().pip_install("torch==2.4.1", "deepspeed")


@app.function(gpu="H100:8", image=image, timeout=3600)
def finetune(shard):
    # 8-way multi-GPU: needs interconnect a single card cannot provide.
    import torch.distributed as dist

    dist.init_process_group(backend="nccl")
    return train_step(shard)


@app.cls(gpu="A100", image=image)
class Sharded:
    @modal.enter()
    def load(self):
        # Single card requested, but the body couples across devices — the guard
        # must catch this via the body scan even though count == 1.
        import torch

        self.model = build_tensor_parallel_model(world_size=4)

    @modal.method()
    def step(self, batch):
        return self.model.forward(batch)


@app.local_entrypoint()
def main():
    list(finetune.map(range(8)))
