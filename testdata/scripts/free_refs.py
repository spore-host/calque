"""free_refs.py — calque#139 fixture: bare (non-.local()) references to
module-level helpers/constants from inside @enter/@method bodies.

Mirrors the real, majority shape found across the calque#79 corpus pass
(ai_models_weather.py, mtp_gemma_serving.py, fasthtml_modal_deploy.py all hit
this independently): an ordinary Python module with helper functions and
constants at the top, referenced the same way any Python module references
its own globals — no `.local()` in sight, because the helper/constant was
never registered as a Modal `@app.function` at all.

`_format` is a plain, undecorated module-level helper — never an
@app.function — called BARE (no `.local()`) from `greet`'s @method body.
`GREETING` is a plain module-level string constant read BARE inside
`load`'s @enter body. Both must resolve via calque#139's free-variable pass
(pyast's _free_refs) and be shipped into the warm runner's globals
(collectLocalExtras -> Config.Extras/ExtraConsts), not silently NameError.

`stray` proves free-ref detection doesn't fire on an ordinary local variable
that merely SHARES a module-level constant's name inside a comprehension —
scope tracking must shadow it correctly, not just string-match.
"""

import modal

app = modal.App("free-refs")

GREETING = "hello"


def _format(name):
    return f"{GREETING}, {name}!"


image = modal.Image.debian_slim()


@app.cls(image=image)
class Greeter:
    @modal.enter()
    def load(self):
        self.prefix = GREETING  # bare constant read, no .local()

    @modal.method()
    def greet(self, name):
        # bare call, no .local() anywhere — _format is a plain helper, never
        # registered as an @app.function
        return _format(name)

    @modal.method()
    def stray(self, items):
        # GREETING here is a comprehension loop var, NOT the module constant —
        # must NOT be captured as a free ref (shadowing).
        return [GREETING for GREETING in items]


@app.local_entrypoint()
def main():
    list(Greeter().greet.map(["a", "b", "c"]))
