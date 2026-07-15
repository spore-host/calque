"""map_batch_inference.py — the K vehicle (spec §15, script 1).

.map() fan-out over prompts; a @cls + @enter loads vLLM once; a Volume caches
weights. Stresses: fan-out translation, ordering, partial failure, warm-drain,
multi-instance acquisition. This is the script the N-ramp runs on (N=1 -> ~100
-> 100k), because it exercises fan-out ordering + partial-failure + multi-instance
acquisition simultaneously and it is what yields the crossover K.

Modal-shaped and unchanged: calque's only mechanical edit is the gpu= rewrite.
"""

import modal

app = modal.App("map-batch-inference")

# Weights live in a Volume so they are cached across runs (image/volume separation).
weights = modal.Volume.from_name("llama-3-8b-weights")

# Image DSL: debian base + the inference stack. Bodies below are NEVER parsed by
# the control plane — they ship to the worker and run under Python as on Modal.
image = (
    modal.Image.debian_slim()
    .pip_install("vllm==0.6.3", "transformers==4.45.2")
    .uv_pip_install("huggingface_hub")
)


@app.cls(gpu="H100", image=image, volumes={"/weights": weights}, timeout=1200)
class Batcher:
    @modal.enter()
    def load(self):
        # Runs ONCE per container (warm). The load-once amortization is the whole
        # economic point — spored must run this exactly once, then reuse it (§6).
        from vllm import LLM, SamplingParams

        self.llm = LLM(model="/weights", dtype="bfloat16", gpu_memory_utilization=0.9)
        self.params = SamplingParams(temperature=0.7, max_tokens=256)

    @modal.method()
    def generate(self, prompt: str) -> str:
        # Processes one item against the warm model. B=1 request-response: the
        # swap-legal regime by construction (§7).
        out = self.llm.generate([prompt], self.params)
        return out[0].outputs[0].text


@app.local_entrypoint()
def main(n: int = 100_000):
    # A meaningless scale on Modal-the-prototype, load-bearing on AWS (§15).
    prompts = [f"Summarize document #{i} in one sentence." for i in range(n)]
    # .map() fan-out — ordering across n and partial failure (some items may die)
    # are the leaks that only bite at scale (§10).
    results = list(Batcher().generate.map(prompts))
    print(f"generated {len(results)} completions")
