"""free_refs_class.py — calque#147 fixture: a bare (non-.local()) reference
to a PLAIN (non-@app.cls) module-level CLASS, the gap left open after
calque#139 (functions/constants) and calque#146 (imports). Mirrors
free_refs.py's exact shape but for an ordinary helper class instead of a
function/constant/import.

`_Adder` is a plain module-level class — never decorated `@app.cls`, just an
ordinary Python helper — instantiated BARE inside `run`'s @method body, no
`.local()` anywhere. Before calque#147 this NameError'd unconditionally,
even though calque#139/#146 already shipped bare-referenced functions/
constants/imports. Mirrors the real motivating case: AI-Almanac's app.py
defines `_LogTee`, a plain log-tee context-manager class, instantiated bare
inside its picked warm unit's body.
"""

import modal

app = modal.App("free-refs-class")


class _Adder:
    def __init__(self, base):
        self.base = base

    def add(self, x):
        return self.base + x


@app.cls()
class Worker:
    @modal.enter()
    def load(self):
        self.adder = _Adder(10)  # bare class instantiation, no .local()

    @modal.method()
    def run(self, x):
        return self.adder.add(x)


@app.local_entrypoint()
def main():
    list(Worker().run.map([1, 2, 3]))
