"""cls_serve.py — pure warm-serve economics (spec §15, script 2).

A single @cls GPU serve with an @enter load. Stresses warm-pool economics:
load-once amortization as a real number. Secondary proof after the K vehicle —
it isolates "what does the @enter cost buy us per item" without fan-out or
multi-instance acquisition in the way.
"""

import modal

app = modal.App("cls-serve")

image = modal.Image.debian_slim().pip_install(
    "torch==2.4.1", "transformers==4.45.2", "accelerate"
)


@app.cls(gpu="A100", image=image, timeout=600)
class Embedder:
    @modal.enter()
    def load(self):
        # One-time model load; reused across every request the warm container sees.
        import torch
        from transformers import AutoModel, AutoTokenizer

        self.device = "cuda" if torch.cuda.is_available() else "cpu"
        self.tok = AutoTokenizer.from_pretrained("BAAI/bge-large-en-v1.5")
        self.model = AutoModel.from_pretrained("BAAI/bge-large-en-v1.5").to(self.device)
        self.model.eval()

    @modal.method()
    def embed(self, text: str) -> list:
        import torch

        with torch.no_grad():
            batch = self.tok(text, return_tensors="pt", truncation=True).to(self.device)
            out = self.model(**batch)
            return out.last_hidden_state.mean(dim=1).squeeze().tolist()


@app.local_entrypoint()
def main(n: int = 1000):
    texts = [f"passage number {i} about scientific computing" for i in range(n)]
    vectors = list(Embedder().embed.map(texts))
    print(f"embedded {len(vectors)} passages")
