"""map_iterables.py — calque#136 fixture: every statically-resolvable (and one
UNRESOLVABLE) `.map()`/`.starmap()` iterable shape.

Four call sites, one warm unit each, so each Function's `ir.Function.Items`
can be asserted independently:

  - `lit_map`     .map([1, 2, 3])                literal list -> Items=[1,2,3]
  - `lit_starmap` .starmap([(1, 2), (3, 4)])      literal tuple list -> Items=[[1,2],[3,4]]
  - `lit_range`   .map(range(5))                  range(5) -> Items=[0,1,2,3,4]
  - `lit_unresolvable` .map(some_variable)        a variable ref -> Items=nil (fallback)

Real Modal code would ordinarily read these into one script; kept as four
separate @app.function + @app.local_entrypoint pairs so pyast's call-site
attribution and the Go-side Items population are each independently testable
without one entrypoint's call site shadowing another's.
"""

import modal

app = modal.App("map-iterables")

some_variable = [1, 2, 3]


@app.function()
def lit_map(x: int) -> int:
    return x


@app.local_entrypoint()
def run_lit_map():
    list(lit_map.map([1, 2, 3]))


@app.function()
def lit_starmap(a: int, b: int) -> int:
    return a + b


@app.local_entrypoint()
def run_lit_starmap():
    list(lit_starmap.starmap([(1, 2), (3, 4)]))


@app.function()
def lit_range(x: int) -> int:
    return x


@app.local_entrypoint()
def run_lit_range():
    list(lit_range.map(range(5)))


@app.function()
def lit_unresolvable(x: int) -> int:
    return x


@app.local_entrypoint()
def run_lit_unresolvable():
    list(lit_unresolvable.map(some_variable))
