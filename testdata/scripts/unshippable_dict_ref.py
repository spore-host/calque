"""unshippable_dict_ref.py — calque#151 fixture: a bare reference to a
module-level modal.Dict.from_name(...) constant.

Mirrors the real shape found in RomeroLab/alphafast's InferenceWorker
(calque#150's torture-test pass): a module-level `data_dict =
modal.Dict.from_name(...)` assignment, referenced BARE (no `.local()`,
just an ordinary Python global read) from inside a @method body.

`data_dict` resolves via calque#139's free-variable pass (pyast's
_free_refs) the same way any other module-level constant would — but it
must NOT be shipped verbatim into the runner's globals the way an ordinary
literal constant is: the runner has no live Modal control-plane
credentials, so exec'ing `modal.Dict.from_name(...)` for real crashes with
a confusing SDK auth error ("Token missing...") instead of an honest leak.
Before calque#151's fix, this shipped anyway; after the fix, calque
refuses to ship it and leaks a clear explanation instead, and the item
fails with a plain NameError (data_dict was never defined) rather than a
confusing Modal SDK crash.
"""

import modal

app = modal.App("unshippable-dict-ref")

data_dict = modal.Dict.from_name("test-dict", create_if_missing=True)


@app.cls()
class Worker:
    @modal.enter()
    def load(self):
        self.ok = True

    @modal.method()
    def use_dict(self, key):
        # bare reference, no .local() anywhere — data_dict is a module-level
        # constant whose RHS is a live-Modal-control-plane construct.
        return data_dict.get(key, "missing")


@app.local_entrypoint()
def main():
    list(Worker().use_dict.map(["a", "b", "c"]))
