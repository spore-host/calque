"""bedrock_eligible.py — adversarial gate fixture: SHOULD route AWAY from calque.

Not one of the three prescribed scripts (§15). It exists to prove the §11 gate
can say YES — that the "Bedrock-eligible count" (the cheapest damning number in
the instrument) can actually be non-zero against the LIVE catalog. A gate that
only ever says "no match" hasn't been shown to detect eligibility at all.

This self-hosts meta-llama/Meta-Llama-3-8B-Instruct by its HuggingFace repo id
(identity is visible, not hidden behind a Volume) and uses it for plain B=1
request-response inference. Both ANDed checks should pass -> exact identity +
inference shape -> "don't rent a GPU, call the Bedrock API."

If Bedrock's catalog moves and this stops matching, that's a real signal the gate
should report — not something to paper over.
"""

import modal

app = modal.App("bedrock-eligible")

image = modal.Image.debian_slim().pip_install("vllm==0.6.3")


@app.cls(gpu="H100", image=image, timeout=600)
class Chat:
    @modal.enter()
    def load(self):
        from vllm import LLM, SamplingParams

        # Identity is VISIBLE here — a HuggingFace repo id, not a mount path.
        self.llm = LLM(model="meta-llama/Meta-Llama-3-8B-Instruct")
        self.params = SamplingParams(temperature=0.7, max_tokens=512)

    @modal.method()
    def chat(self, prompt: str) -> str:
        out = self.llm.generate([prompt], self.params)
        return out[0].outputs[0].text


@app.local_entrypoint()
def main(n: int = 50):
    prompts = [f"Answer question {i}." for i in range(n)]
    print(list(Chat().chat.map(prompts)))
