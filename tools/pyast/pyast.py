#!/usr/bin/env python3
"""calque pyast helper (spec §13).

Emit the *decorator* AST of a Modal script as JSON for the Go control plane.

Contract:
  - Decorators are CONFIGURATION -> parsed, understood, emitted structurally.
  - Function/method BODIES are PAYLOAD -> extracted VERBATIM as strings, never
    interpreted. They ship to the worker and run under Python exactly as on Modal.

We are not writing a Python parser in Go for the spike (tree-sitter-python is the
v2 answer). This walks only what the IR (§14) and the static passes (§7 gpu guard,
§11 Bedrock gate) need, and refuses to guess about anything else.

Usage:  python pyast.py <script.py>   # JSON to stdout
Stdlib only (ast, json) — no third-party deps, so `uv run` needs no network.
"""

from __future__ import annotations

import ast
import json
import sys
from typing import Any


def _const_str(node: ast.AST) -> str | None:
    """A string constant, or None. We do not evaluate expressions."""
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    return None


def _spawn_arg_str(node: ast.AST) -> str | None:
    """Best-effort string form of a .spawn() call arg (calque#112, discovered
    via live-AWS verification: _const_str's string-only scope — correct for
    from_name's needs, which .spawn() originally reused as-is — silently
    dropped numeric args to None, e.g. worker.spawn(5) captured no arg at
    all, indistinguishable from worker.spawn(some_variable)). Accepts str/
    int/float/bool Constant nodes and stringifies them; None-valued
    Constants (a literal `None` argument) and everything else (variables,
    expressions) stay None — same "we do not evaluate expressions" contract
    _const_str already documents, just widened to numeric/bool literals
    the wire's Optional[str] shape can represent losslessly via str()."""
    if isinstance(node, ast.Constant) and node.value is not None and isinstance(node.value, (str, int, float, bool)):
        return str(node.value)
    return None


def _literal(node: ast.AST) -> Any:
    """Best-effort literal for a decorator kwarg. Non-literals become a tagged
    marker so the Go side can log a leak instead of silently dropping meaning."""
    try:
        return ast.literal_eval(node)
    except (ValueError, SyntaxError):
        return {"__unparsed__": ast.unparse(node)}


def _attr_chain(node: ast.AST) -> list[str]:
    """Flatten a dotted name like `modal.App` / `app.function` into ['modal','App']."""
    parts: list[str] = []
    while isinstance(node, ast.Attribute):
        parts.append(node.attr)
        node = node.value
    if isinstance(node, ast.Name):
        parts.append(node.id)
    parts.reverse()
    return parts


def _decorator_name(node: ast.AST) -> str:
    """Dotted name of a decorator, ignoring any call args. `@app.function(...)` -> 'app.function'."""
    target = node.func if isinstance(node, ast.Call) else node
    return ".".join(_attr_chain(target))


def _volumes_map(node: ast.AST, leaks: list[dict[str, Any]] | None = None) -> dict[str, str] | None:
    """Extract `volumes={"/mount": vol_handle}` as {mount_path: volume_var_name}.

    The keys are string literals (mount paths); the values are Volume *variables*
    (handles), so `literal_eval` on the whole dict fails. We resolve it structurally
    to match IR §14 `Volumes map[string]string // mount path -> volume name`.
    Returns None if this isn't a dict we can map.

    calque#91: a value that's a direct `CloudBucketMount(...)`/`NetworkFileSystem(...)`
    call (not a Volume.from_name()-derived variable) is a DIFFERENT, unmodeled
    construct — before this check existed it fell into the generic `ast.unparse(v)`
    branch below and was silently treated as an ordinary Volume mount, with no
    leak distinguishing it at all. When leaks is supplied, tag it there instead.
    """
    if not isinstance(node, ast.Dict):
        return None
    out: dict[str, str] = {}
    for k, v in zip(node.keys, node.values):
        key = _const_str(k) if k is not None else None
        if key is None:
            continue
        if isinstance(v, ast.Name):
            out[key] = v.id
        else:
            if leaks is not None and isinstance(v, ast.Call):
                construct = _unsupported_construct_call(_attr_chain(v.func))
                if construct is not None:
                    leaks.append(
                        {"where": construct, "detail": f"{construct}(...) used as a volumes= value, recognized but not modeled — NOT an ordinary Volume mount (calque#91)", "lineno": getattr(v, "lineno", 0)}
                    )
            out[key] = ast.unparse(v)  # e.g. Volume.from_name(...) inline
    return out


# calque#91: rare Modal constructs with zero implementation and, until now, no
# distinguishable leak signal either — they either vanished entirely
# (Dict/Queue/NetworkFileSystem's own visit_Assign branch didn't exist) or were
# silently MISCATEGORIZED as an unrelated construct (CloudBucketMount(...) used
# as a volumes={} value looked exactly like an ordinary Volume mount, since
# _volumes_map's non-Name fallback just unparsed it with no tag at all). This
# does NOT attempt to model any of them — it only makes their presence
# distinguishable (a named "where" in the leak report) so a real script using
# one is a clean grep hit instead of silence or a false Volume classification.
_FROM_NAME_CONSTRUCTS = frozenset({"Dict", "Queue", "NetworkFileSystem"})
_CALL_CONSTRUCTS = frozenset({"CloudBucketMount"})


def _unsupported_construct_from_name(dotted: list[str]) -> str | None:
    """dotted is an attribute chain ending in `.from_name`, e.g.
    ['modal','Dict','from_name']. Returns "modal.Dict" if the owner (second-
    to-last element) is one of Dict/Queue/NetworkFileSystem, else None."""
    if len(dotted) < 2 or dotted[-1] != "from_name":
        return None
    owner = dotted[-2]
    return "modal." + owner if owner in _FROM_NAME_CONSTRUCTS else None


def _unsupported_construct_call(dotted: list[str]) -> str | None:
    """dotted is a call's own attribute chain (no trailing method), e.g.
    ['modal','CloudBucketMount']. Returns "modal.CloudBucketMount" if the
    LEAF name is a known direct-constructor unsupported construct, else
    None."""
    if not dotted:
        return None
    leaf = dotted[-1]
    return "modal." + leaf if leaf in _CALL_CONSTRUCTS else None


def _iterable_literal(node: ast.Call) -> dict[str, Any] | None:
    """Best-effort static resolution of a `.map()`/`.starmap()` call's iterable
    argument (calque#136): node is the `.map(...)`/`.starmap(...)` Call itself;
    the iterable is its first positional arg (`node.args[0]`).

    Two shapes are statically resolvable without executing any script code:
      - A literal list/tuple/string (or any other literal `ast.literal_eval`
        accepts): `[1,2,3]`, `[(1,2),(3,4)]`, `"abc"`. Returns
        {"kind": "literal", "values": [...]}, coercing a non-list/tuple/str
        result (e.g. a bare literal) to a single-element list, and a str to
        its list of characters (str is iterable the same way in real .map()).
      - `range(N)` / `range(start, stop)` / `range(start, stop, step)`, when
        range's OWN args are all literal ints: we replay `list(range(*ints))`
        using ints we already safely extracted via literal_eval — this is
        NOT executing arbitrary script code, just the 1-3 ints themselves.
        Returns {"kind": "range", "values": [...]}.

    Everything else (a variable reference, a comprehension, a function call
    result other than range, unpacking, etc.) returns None so the caller
    omits the "iterable" key entirely — never a null placeholder — and the Go
    side falls back to its synthesized placeholder.
    """
    if not node.args:
        return None
    arg = node.args[0]

    try:
        val = ast.literal_eval(arg)
    except (ValueError, SyntaxError):
        val = None
    else:
        if isinstance(val, (list, tuple, str)):
            values = list(val)
        else:
            values = [val]
        return {"kind": "literal", "values": values}

    # range(...) whose own args are all literal ints: replay it structurally.
    if isinstance(arg, ast.Call) and isinstance(arg.func, ast.Name) and arg.func.id == "range":
        try:
            ints = [ast.literal_eval(a) for a in arg.args]
        except (ValueError, SyntaxError):
            return None
        if ints and all(isinstance(i, int) and not isinstance(i, bool) for i in ints):
            return {"kind": "range", "values": list(range(*ints))}

    return None


def _schedule_marker(node: ast.AST) -> dict[str, Any] | None:
    """Recognize `schedule=modal.Cron(...)` / `schedule=modal.Period(...)` object
    forms (calque#91). Without this, the Call node falls through to `_literal`'s
    generic `{"__unparsed__": ...}` marker, which the Go side cannot distinguish
    from any other unparseable schedule= value.

    Returns {"__schedule__": "Cron"|"Period", "args": [...], "kwargs": {...}} with
    each positional/keyword arg individually literal-eval'd (never the whole Call
    node — a Cron/Period call itself is never itself a literal). Returns None if
    this isn't a Cron/Period call, so the caller falls through to the generic path.
    """
    if not isinstance(node, ast.Call):
        return None
    name = _attr_chain(node.func)[-1:]
    if name not in (["Cron"], ["Period"]):
        return None
    return {
        "__schedule__": name[0],
        "args": [_literal(a) for a in node.args],
        "kwargs": {kw.arg: _literal(kw.value) for kw in node.keywords if kw.arg is not None},
    }


def _decorator_kwargs(node: ast.AST, leaks: list[dict[str, Any]] | None = None) -> dict[str, Any]:
    """kwargs of a decorator call. `gpu=`, `timeout=`, `image=`, `volumes=` etc.
    leaks, when supplied, receives calque#91's unsupported-construct tags found
    while walking volumes= (see _volumes_map)."""
    if not isinstance(node, ast.Call):
        return {}
    out: dict[str, Any] = {}
    for kw in node.keywords:
        if kw.arg is None:  # **kwargs splat — record as a leak signal
            out["__splat__"] = ast.unparse(kw.value)
            continue
        if kw.arg == "volumes":
            vm = _volumes_map(kw.value, leaks)
            out["volumes"] = vm if vm is not None else _literal(kw.value)
            continue
        if kw.arg == "image" and isinstance(kw.value, ast.Name):
            # image=image_var — record the referenced var name so IR can resolve it.
            out["image"] = {"__ref__": kw.value.id}
            continue
        if kw.arg == "schedule":
            sched = _schedule_marker(kw.value)
            if sched is not None:
                out["schedule"] = sched
                continue
        out[kw.arg] = _literal(kw.value)
    return out


def _body_source(src: str, node: ast.FunctionDef) -> str:
    """Verbatim source of a function BODY (statements only, dedented). This is the
    payload shipped to the worker. We never interpret it."""
    if not node.body:
        return ""
    start = node.body[0].lineno
    end = node.body[-1].end_lineno
    lines = src.splitlines()[start - 1 : end]
    # Dedent by the body's own indentation so it can be re-embedded/exec'd cleanly.
    indent = len(lines[0]) - len(lines[0].lstrip()) if lines else 0
    return "\n".join(ln[indent:] if len(ln) >= indent else ln for ln in lines)


def _arg_names(node: ast.FunctionDef) -> list[str]:
    a = node.args
    names = [p.arg for p in (a.posonlyargs + a.args)]
    if a.vararg:
        names.append("*" + a.vararg.arg)
    names += [p.arg for p in a.kwonlyargs]
    if a.kwarg:
        names.append("**" + a.kwarg.arg)
    return names


def _local_calls(node: ast.FunctionDef) -> list[str]:
    """.local() call targets referenced ANYWHERE inside node's own body
    (calque#92) — a nested-function-scoped scan, unlike visit_Call's
    module-wide walk, so each function/method carries exactly the sibling
    names IT ITSELF references. Same target-extraction as visit_Call's
    "local" branch (the trailing attribute before .local is the callee)."""
    targets: set[str] = set()
    for n in ast.walk(node):
        if (
            isinstance(n, ast.Call)
            and isinstance(n.func, ast.Attribute)
            and n.func.attr == "local"
        ):
            targets.add(".".join(_attr_chain(n.func)[:-1]))
    return sorted(targets)


# ---- calque#139: free-variable references (bare names, no .local() suffix) ----
#
# Real Modal code overwhelmingly references module-level helpers/constants via a
# plain, undecorated name — not `.local(...)` — because they're ordinary Python
# globals, never registered as an @app.function at all. _local_calls above only
# ever sees the explicit `.local()` idiom; this is its free-name counterpart,
# needing REAL scope tracking (params/locals/loop-vars/comprehension-vars/nested
# defs and lambdas all shadow an outer name of the same spelling) rather than a
# naive regex/flat scan.


def _arg_id_set(args: ast.arguments) -> set[str]:
    """Bare parameter names bound by a function/lambda signature (no */**
    markers, unlike _arg_names — those markers would break scope membership
    tests, since the body refers to the vararg by its bare name)."""
    names = {p.arg for p in (args.posonlyargs + args.args + args.kwonlyargs)}
    if args.vararg:
        names.add(args.vararg.arg)
    if args.kwarg:
        names.add(args.kwarg.arg)
    return names


def _comp_target_names(target: ast.AST) -> list[str]:
    """Flatten a (possibly nested/starred) `for` or comprehension target into
    the bare names it binds — `for a, (b, *c) in ...` binds a/b/c."""
    out: list[str] = []

    def rec(t: ast.AST) -> None:
        if isinstance(t, ast.Name):
            out.append(t.id)
        elif isinstance(t, (ast.Tuple, ast.List)):
            for e in t.elts:
                rec(e)
        elif isinstance(t, ast.Starred):
            rec(t.value)

    rec(target)
    return out


def _bound_names_in_block(stmts: list[ast.stmt]) -> set[str]:
    """Every name bound directly within this block's OWN statements — Python
    hoists a function-local's "local-ness" across the WHOLE function body
    regardless of textual order (unlike e.g. JS `let`), so this is a
    non-recursive-into-nested-scopes scan, not a sequential one: assignment
    targets (incl. tuple/list/starred unpacking), augmented/annotated assign,
    `for`/`with`/`except` bound names, walrus (`:=`) targets, imports, and a
    nested def/class's own NAME (the `def foo():` statement binds `foo` in
    THIS scope even though foo's body is a separate one). Does NOT descend
    into a nested function/lambda/comprehension's own body — those get their
    own call from _FreeRefFinder when it recurses into them."""
    bound: set[str] = set()

    def add_target(t: ast.AST) -> None:
        if isinstance(t, ast.Name):
            bound.add(t.id)
        elif isinstance(t, (ast.Tuple, ast.List)):
            for e in t.elts:
                add_target(e)
        elif isinstance(t, ast.Starred):
            add_target(t.value)

    class _Walker(ast.NodeVisitor):
        def visit_FunctionDef(self, n: ast.FunctionDef) -> None:
            bound.add(n.name)  # binds the name here; body is a separate scope

        visit_AsyncFunctionDef = visit_FunctionDef

        def visit_ClassDef(self, n: ast.ClassDef) -> None:
            bound.add(n.name)

        def visit_Lambda(self, n: ast.Lambda) -> None:
            return  # separate scope, no name bound in THIS one

        def visit_ListComp(self, n: ast.AST) -> None:
            return  # separate scope (Python 3: comprehensions don't leak vars)

        visit_SetComp = visit_ListComp
        visit_DictComp = visit_ListComp
        visit_GeneratorExp = visit_ListComp

        def visit_Assign(self, n: ast.Assign) -> None:
            for t in n.targets:
                add_target(t)
            self.generic_visit(n)

        def visit_AugAssign(self, n: ast.AugAssign) -> None:
            add_target(n.target)
            self.generic_visit(n)

        def visit_AnnAssign(self, n: ast.AnnAssign) -> None:
            add_target(n.target)
            self.generic_visit(n)

        def visit_NamedExpr(self, n: ast.NamedExpr) -> None:  # walrus :=
            add_target(n.target)
            self.generic_visit(n)

        def visit_For(self, n: ast.For) -> None:
            add_target(n.target)
            self.generic_visit(n)

        visit_AsyncFor = visit_For

        def visit_With(self, n: ast.With) -> None:
            for item in n.items:
                if item.optional_vars is not None:
                    add_target(item.optional_vars)
            self.generic_visit(n)

        visit_AsyncWith = visit_With

        def visit_ExceptHandler(self, n: ast.ExceptHandler) -> None:
            if n.name:
                bound.add(n.name)
            self.generic_visit(n)

        def visit_Import(self, n: ast.Import) -> None:
            for alias in n.names:
                bound.add((alias.asname or alias.name).split(".")[0])

        def visit_ImportFrom(self, n: ast.ImportFrom) -> None:
            for alias in n.names:
                bound.add(alias.asname or alias.name)

    w = _Walker()
    for s in stmts:
        w.visit(s)
    return bound


class _FreeRefFinder(ast.NodeVisitor):
    """Scope-aware free-`Name`-Load collector (calque#139). Maintains a stack
    of bound-name sets (innermost last) and records every `ast.Name` Load
    that isn't bound in ANY enclosing scope. A nested def/lambda/comprehension
    pushes its OWN scope (params + its own hoisted locals) before descending,
    so it correctly SHADOWS an outer name of the same spelling rather than
    conflating the two — real Python scoping, not a flat/naive scan.
    """

    def __init__(self, top_params: set[str], top_body: list[ast.stmt]) -> None:
        self.scopes: list[set[str]] = [top_params | _bound_names_in_block(top_body)]
        self.free: set[str] = set()

    def _bound(self, name: str) -> bool:
        return any(name in s for s in self.scopes)

    def visit_Name(self, node: ast.Name) -> None:
        if isinstance(node.ctx, ast.Load) and not self._bound(node.id):
            self.free.add(node.id)

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        # The def's own NAME is already bound in the CURRENT scope (folded
        # into that scope's _bound_names_in_block); only its BODY gets a
        # fresh one.
        params = _arg_id_set(node.args)
        self.scopes.append(params | _bound_names_in_block(node.body))
        for stmt in node.body:
            self.visit(stmt)
        self.scopes.pop()

    visit_AsyncFunctionDef = visit_FunctionDef

    def visit_Lambda(self, node: ast.Lambda) -> None:
        self.scopes.append(_arg_id_set(node.args))
        self.visit(node.body)
        self.scopes.pop()

    def _visit_comprehension(self, node: ast.AST, exprs: list[ast.AST]) -> None:
        scope: set[str] = set()
        for gen in node.generators:  # type: ignore[attr-defined]
            for n in _comp_target_names(gen.target):
                scope.add(n)
        self.scopes.append(scope)
        for e in exprs:
            self.visit(e)
        for gen in node.generators:  # type: ignore[attr-defined]
            self.visit(gen.iter)
            for cond in gen.ifs:
                self.visit(cond)
        self.scopes.pop()

    def visit_ListComp(self, node: ast.ListComp) -> None:
        self._visit_comprehension(node, [node.elt])

    def visit_SetComp(self, node: ast.SetComp) -> None:
        self._visit_comprehension(node, [node.elt])

    def visit_GeneratorExp(self, node: ast.GeneratorExp) -> None:
        self._visit_comprehension(node, [node.elt])

    def visit_DictComp(self, node: ast.DictComp) -> None:
        self._visit_comprehension(node, [node.key, node.value])

    def visit_ClassDef(self, node: ast.ClassDef) -> None:
        # The name is already bound (folded into the enclosing scope's
        # hoisted set); a nested class's own body is a rare shape this pass
        # doesn't attempt to resolve free names inside.
        return


def _arg_names_raw(args: ast.arguments) -> list[str]:
    """Bare parameter names in signature order (no */** markers, unlike
    _arg_names) — used to seed _FreeRefFinder's top scope."""
    names = [p.arg for p in (args.posonlyargs + args.args + args.kwonlyargs)]
    if args.vararg:
        names.append(args.vararg.arg)
    if args.kwarg:
        names.append(args.kwarg.arg)
    return names


def _free_refs(node: ast.FunctionDef, module_names: frozenset[str]) -> list[str]:
    """Free-variable references inside node's own body (calque#139): every
    `ast.Name` Load that resolves to neither a parameter nor a local (assigned
    anywhere in node's own scope, including loop/comprehension/nested-def
    shadowing) AND matches the name of a real module-level `FunctionDef`/
    simple `Assign` (module_names) in the SAME script. This is the non-
    `.local()`-suffixed counterpart to _local_calls — a plain call like
    `_format(name)` or a bare read like `GREETING` never goes through
    `.local`, so _local_calls' Call-node scan never sees it."""
    finder = _FreeRefFinder(set(_arg_names_raw(node.args)), node.body)
    for stmt in node.body:
        finder.visit(stmt)
    return sorted(n for n in finder.free if n in module_names)


def _free_refs_in_class(node: ast.ClassDef, module_names: frozenset[str]) -> list[str]:
    """_free_refs' sibling for a PLAIN (non-`@app.cls`) module-level class
    body (calque#147) — mirrors _free_refs' shape exactly, but seeded with
    the class's OWN body as top_body (no params: a class has no argument
    list of its own the way a function does) instead of a function's args.
    Each method inside still gets its OWN pushed scope via
    _FreeRefFinder.visit_FunctionDef (params + hoisted locals), so a bare
    module-level reference inside `__init__`/any method is still found
    correctly — this only differs from _free_refs in what seeds the
    OUTERMOST scope."""
    finder = _FreeRefFinder(set(), node.body)
    for stmt in node.body:
        finder.visit(stmt)
    return sorted(n for n in finder.free if n in module_names)


def _free_refs_in_expr(node: ast.expr, module_names: frozenset[str]) -> list[str]:
    """_free_refs' sibling for a bare EXPRESSION rather than a function body
    (calque#146.2) — a module-level constant's own RHS (e.g. `forecast_volume
    = modal.Volume.from_name(...)`) can itself reference an import or another
    constant, but has no function scope of its own to seed _FreeRefFinder
    with (no params, no hoisted locals). Empty top scope: every Name Load in
    the expression is automatically "free" unless it's a comprehension's own
    loop var (_FreeRefFinder already handles that shadowing regardless of
    what seeded the top scope)."""
    finder = _FreeRefFinder(set(), [])
    finder.visit(node)
    return sorted(n for n in finder.free if n in module_names)


_CLASS_DECO_SUFFIXES = ("cls",)  # matches Collector.visit_ClassDef's own "endswith('cls')" test


def _is_app_cls(node: ast.ClassDef) -> bool:
    """Mirrors Collector.visit_ClassDef's own decorator test exactly (module-
    scope helper so _module_bindings doesn't need a Collector instance) — a
    class decorated `@app.cls`/`@modal.cls` is Modal's own execution unit,
    already modeled structurally elsewhere; only a PLAIN, undecorated helper
    class (never `@app.cls`) is a calque#147 shipping candidate."""
    return any(_decorator_name(d).endswith(_CLASS_DECO_SUFFIXES) for d in node.decorator_list)


def _module_bindings(
    tree: ast.Module,
) -> tuple[dict[str, ast.AST], dict[str, ast.Assign], dict[str, ast.stmt], dict[str, ast.ClassDef]]:
    """Top-level-only scan (tree.body, NOT ast.walk) for calque#139/#146/
    #147's FOUR shippable free-reference targets: every module-level `def`/
    `async def` (decorated or not — a bare helper like `_format` is never
    decorated), every module-level SIMPLE assignment (`NAME = <expr>`,
    single ast.Name target only — `A = B = 5` and tuple-unpacking assigns
    are deliberately excluded, matching the issue's "simple Assign target"
    scope), every module-level `import X` / `from X import Y` statement
    (calque#146 — a bare, non-`.local()` reference to an imported name, e.g.
    `Path(...)` after `from pathlib import Path`, was previously a hard
    NameError: unlike a re-exported name from ANOTHER module (deliberately
    still unresolved — genuinely ambiguous without executing that module),
    a name this script imports itself is unambiguous and shippable the same
    way a module-level constant already is), and every PLAIN (non-`@app.cls`)
    module-level class (calque#147 — an ordinary helper class like a log-tee
    context manager, never Modal's own `@app.cls` execution unit, which is
    already modeled structurally by Collector and must NOT be double-
    collected here). `imports` maps EACH bound name from a statement to that
    SAME statement node — `from datetime import UTC, datetime` binds two
    names, both pointing at the one import statement, so shipping resolves
    per-name but the source text is identical either way. Scanning only
    tree.body (not the whole tree) is what makes this "module-level": the
    same node types nested inside a function/class body are a different
    binding entirely and must not be captured here."""
    funcs: dict[str, ast.AST] = {}
    consts: dict[str, ast.Assign] = {}
    imports: dict[str, ast.stmt] = {}
    classes: dict[str, ast.ClassDef] = {}
    for stmt in tree.body:
        if isinstance(stmt, (ast.FunctionDef, ast.AsyncFunctionDef)):
            funcs[stmt.name] = stmt
        elif isinstance(stmt, ast.Assign) and len(stmt.targets) == 1 and isinstance(stmt.targets[0], ast.Name):
            consts[stmt.targets[0].id] = stmt
        elif isinstance(stmt, ast.Import):
            for alias in stmt.names:
                imports[(alias.asname or alias.name).split(".")[0]] = stmt
        elif isinstance(stmt, ast.ImportFrom):
            # `from __future__ import annotations` etc. bind names too, but
            # re-execing a __future__ import at runtime (outside module
            # scope) raises SyntaxError — excluded, matching CPython's own
            # restriction that __future__ imports must be the first
            # statement in a module.
            if stmt.module == "__future__":
                continue
            for alias in stmt.names:
                imports[alias.asname or alias.name] = stmt
        elif isinstance(stmt, ast.ClassDef) and not _is_app_cls(stmt):
            classes[stmt.name] = stmt
    return funcs, consts, imports, classes


def _stmt_source(src: str, node: ast.stmt) -> str:
    """Verbatim source text of a whole top-level statement (calque#139's
    `NAME = <expr>` assignments, calque#146's `import X`/`from X import Y`
    statements) — analogous to _body_source but for a statement rather than
    a function body. Ships the exact source line(s) so the runner can exec
    it into globals unchanged; same "ship the payload verbatim, never
    interpret it" trust model this file already applies to every
    @enter/@method body. Named generically (not `_assign_source`) since
    calque#146 reuses this for import statements too — the slicing logic is
    identical regardless of statement kind."""
    lines = src.splitlines()[node.lineno - 1 : node.end_lineno]
    return "\n".join(lines)


# Serve decorators (§F): long-lived request-driven entrypoints. Detected so the Go
# side can gate/leak them (build is deferred — §16 success is batch+K), never
# silently treated as batch. Matched by the trailing decorator name.
_SERVE_DECOS = frozenset({"web_endpoint", "fastapi_endpoint", "asgi_app", "wsgi_app", "web_server"})


def _entry_kind(decos: list[str]) -> str:
    """Classify a function's execution shape from its decorators (§F). 'serve' for a
    long-lived request-driven entrypoint, else '' (batch/plain)."""
    for d in decos:
        leaf = d.rsplit(".", 1)[-1]
        if leaf in _SERVE_DECOS:
            return "serve"
    return ""


def _describe_fn(
    src: str,
    node: ast.FunctionDef,
    leaks: list[dict[str, Any]] | None = None,
    module_names: frozenset[str] = frozenset(),
) -> dict[str, Any]:
    decos = []
    for d in node.decorator_list:
        decos.append(
            {
                "name": _decorator_name(d),
                "kwargs": _decorator_kwargs(d, leaks),
                "lineno": getattr(d, "lineno", node.lineno),
            }
        )
    return {
        "name": node.name,
        "lineno": node.lineno,
        "args": _arg_names(node),
        "decorators": decos,
        "entry_kind": _entry_kind([d["name"] for d in decos]),
        # calque#152: @modal.experimental.clustered(...) requests MULTI-NODE
        # execution — a decorator-level construct invisible to both the §7
        # guard's spec.Count (parsed from the gpu= STRING alone, e.g.
        # "H100:8") and its couplingSignal (a body-text regex): neither
        # check ever inspects the function's OWN decorator list. Matched by
        # trailing name, same pattern as _SERVE_DECOS/_entry_kind above.
        "is_clustered": any(d["name"].rsplit(".", 1)[-1] == "clustered" for d in decos),
        "local_calls": _local_calls(node),
        # calque#139: bare (non-.local()-suffixed) references to a module-level
        # helper function or constant, found ANYWHERE inside this function/
        # method's own body — real scope-tracked, see _free_refs/_FreeRefFinder.
        # module_names is the set of names _free_refs is allowed to resolve
        # against (every module-level def/simple-assign in the script);
        # defaults to empty so a caller that doesn't pass it (none currently
        # do, but keeps this function safely callable standalone) gets [].
        "free_refs": _free_refs(node, module_names),
        # PAYLOAD — verbatim, never interpreted:
        "body": _body_source(src, node),
    }


# Image chain vocabulary (spec §13). Base constructors terminate a chain; build
# steps are the DSL verbs. A call chain is only an Image if it contains at least one
# of these — this is what stops `modal.App(...)` / `Volume.from_name(...)` from being
# mis-parsed as images.
_IMAGE_BASES = frozenset(
    {"debian_slim", "from_registry", "from_dockerfile", "from_aws_ecr", "micromamba"}
)
_IMAGE_STEPS = frozenset(
    {
        "pip_install", "uv_pip_install", "poetry_install_from_file", "pip_install_from_requirements",
        "apt_install", "run_commands", "run_function", "env", "workdir", "entrypoint",
        "add_local_dir", "add_local_file", "add_local_python_source",
        "copy_local_dir", "copy_local_file", "dockerfile_commands",
    }
)


def _walk_image_chain(node: ast.AST) -> dict[str, Any] | None:
    """Flatten a `modal.Image.debian_slim().pip_install(...).uv_pip_install(...)` chain.

    Returns {base, steps:[{method, args}], base_unresolved: bool, root_name: str|None}
    or None if this isn't an Image chain. A chain counts as an image only if it
    contains a known base constructor or at least one known build step —
    otherwise `App(...)` / `Volume.from_name(...)` would be misread as images.
    We resolve the chain structurally; we never execute it.

    calque#140: `root_name` is the bare variable name the chain's root climbed
    to when it did NOT bottom out at a real base-constructor call (i.e. only
    populated when `base_unresolved` is True) — e.g. for `image.env(...)`,
    root_name is "image". This lets the caller (`visit_Assign`) distinguish
    "this reassignment's RHS is itself rooted at a PRE-EXISTING image
    variable, so its steps should chain-extend that variable's already-
    recorded steps" from "this chain's root is some other/unknown name, so
    base truly cannot be resolved" — without this, EVERY `x = x.step(...)`
    reassignment (a natural pattern for progressively/conditionally built
    images across multiple statements) silently overwrote and discarded
    whatever base+steps the earlier statement(s) had already resolved.
    """
    steps: list[dict[str, Any]] = []
    cur = node
    base = None
    saw_image_verb = False
    while isinstance(cur, ast.Call) and isinstance(cur.func, ast.Attribute):
        method = cur.func.attr
        args: list[Any] = []
        for a in cur.args:
            if isinstance(a, (ast.List, ast.Tuple)):
                args.extend(_literal(e) for e in a.elts)
            else:
                # pip_install("torch", "vllm") — varargs of package strings
                args.append(_literal(a))
        if method in _IMAGE_BASES:
            base = method
            saw_image_verb = True
            steps.append({"method": method, "args": args})
            break  # base constructor terminates the chain
        if method in _IMAGE_STEPS:
            saw_image_verb = True
        steps.append({"method": method, "args": args})
        cur = cur.func.value
    if not saw_image_verb:
        return None  # not an image chain (e.g. App(), Volume.from_name())
    steps.reverse()
    # base_unresolved: image-like chain rooted at a variable, not a known constructor.
    # Recorded as a helper_leak by the caller so the Go side can log it (§10).
    root_name = cur.id if base is None and isinstance(cur, ast.Name) else None
    return {"base": base, "steps": steps, "base_unresolved": base is None, "root_name": root_name}


class Collector(ast.NodeVisitor):
    def __init__(self, src: str, module_names: frozenset[str] = frozenset()) -> None:
        self.src = src
        # calque#139: names of every module-level def/simple-assign in the
        # script — the resolution universe _free_refs is allowed to match
        # against when walking each function/method body (see _describe_fn).
        self._module_names = module_names
        self.app_name: str | None = None
        self.functions: list[dict[str, Any]] = []
        self.classes: list[dict[str, Any]] = []
        self.entrypoints: list[dict[str, Any]] = []
        self.images: dict[str, Any] = {}       # varname -> image chain
        self.volumes: dict[str, Any] = {}       # varname -> {from_name: str, lineno}
        self.map_calls: list[dict[str, Any]] = []  # every `.map(` occurrence
        self.invoke_calls: list[dict[str, Any]] = []  # §C: starmap/for_each/remote/spawn/map.aio
        self.volume_writes: list[dict[str, Any]] = []  # §E: volume.commit()/reload() call sites
        self.leaks: list[dict[str, Any]] = []      # helper-level "I saw this but can't model it"
        # calque#98: names of @app.local_entrypoint()s we are CURRENTLY walking
        # into, innermost last. A call site visited while this is non-empty is
        # attributed to _current_entrypoint below — this is what lets the Go
        # side ask "what does entrypoint X specifically invoke?" instead of
        # only ever seeing a whole-script-flat union of every call site.
        self._entrypoint_stack: list[str] = []

    @property
    def _current_entrypoint(self) -> str:
        """The @app.local_entrypoint() whose body we're currently inside, or ""
        if we're not inside one (module level, or inside a plain @app.function/
        @cls method body — those are never recursed into for call sites; see
        visit_ClassDef, and visit_FunctionDef only pushes for entrypoints)."""
        return self._entrypoint_stack[-1] if self._entrypoint_stack else ""

    # ---- module-level assignments: App(), Image chains, Volume.from_name() ----
    def visit_Assign(self, node: ast.Assign) -> None:
        val = node.value
        # app = modal.App("name")
        if isinstance(val, ast.Call) and _attr_chain(val.func)[-1:] == ["App"]:
            if val.args:
                self.app_name = _const_str(val.args[0]) or self.app_name
            kw = _decorator_kwargs(val, self.leaks)
            if "image" in kw:  # App(image=...) — note it
                self.leaks.append(
                    {"where": "App(image=)", "detail": "app-level default image", "lineno": node.lineno}
                )
        # image = modal.Image.debian_slim().pip_install(...)
        chain = _walk_image_chain(val) if isinstance(val, ast.Call) else None
        if chain is not None:
            for t in node.targets:
                if isinstance(t, ast.Name):
                    # calque#140: a real-world image is frequently built across
                    # MULTIPLE statements that reassign the same variable name,
                    # e.g. `image = image.env(...).run_function(...)` following
                    # an earlier `image = modal.Image.debian_slim()...` — this
                    # new chain's own root climbs to a bare Name (base=None,
                    # root_name="image") rather than a real base constructor.
                    # If that root name is itself an ALREADY-KNOWN image var,
                    # this is a chain-extension of it, not a fresh unrelated
                    # image — merge by keeping the prior record's resolved
                    # base and appending this statement's own steps onto the
                    # end of its steps list, rather than overwriting the whole
                    # record with a base-less one and losing every earlier
                    # layer. The prior record is looked up by root_name (not
                    # necessarily t.id — in the common `image = image.env(...)`
                    # shape they're the same name, but this also correctly
                    # handles a rename-while-chaining like
                    # `new_image = image.env(...)`). A root name that is NOT
                    # already a known image var (a genuinely unresolved/
                    # external reference) stays base_unresolved, unchanged
                    # from today.
                    prior = self.images.get(chain["root_name"]) if chain["root_name"] is not None else None
                    if chain["base_unresolved"] and prior is not None:
                        merged = {
                            "base": prior["base"],
                            "steps": prior["steps"] + chain["steps"],
                            "base_unresolved": prior["base_unresolved"],
                        }
                        self.images[t.id] = merged
                    else:
                        # Drop the internal root_name key from the public record —
                        # it's plumbing for this merge decision, not part of the
                        # wire contract the Go side reads.
                        self.images[t.id] = {
                            "base": chain["base"],
                            "steps": chain["steps"],
                            "base_unresolved": chain["base_unresolved"],
                        }
        # vol = modal.Volume.from_name("weights")
        if isinstance(val, ast.Call) and _attr_chain(val.func)[-2:] == ["Volume", "from_name"]:
            name = _const_str(val.args[0]) if val.args else None
            for t in node.targets:
                if isinstance(t, ast.Name):
                    self.volumes[t.id] = {"from_name": name, "lineno": node.lineno}
        # calque#91: d = modal.Dict.from_name(...) / q = modal.Queue.from_name(...) /
        # nfs = modal.NetworkFileSystem.from_name(...) — none of these are Volumes,
        # but before this branch existed they simply vanished (no visit_Assign
        # branch matched, so the whole statement fell through generic_visit unseen).
        if isinstance(val, ast.Call):
            construct = _unsupported_construct_from_name(_attr_chain(val.func))
            if construct is not None:
                self.leaks.append(
                    {"where": construct, "detail": f"{construct}.from_name(...) recognized but not modeled (calque#91)", "lineno": node.lineno}
                )
        self.generic_visit(node)

    # ---- record invocation idioms (spec §13 map; §C starmap/for_each/remote;
    # async spawn/.map.aio recognized so the census stays honest, leaked as
    # deferred on the Go side) ----
    # .local() (calque#81) is intentionally NOT in _SYNC_IDIOMS: unlike
    # map/starmap/for_each/remote, it never becomes an ir.InvokeKind for the
    # TARGET callable (a .local()-called function is not itself a warm unit) —
    # it is a property of the CALL SITE inside whichever body already shipped,
    # so it gets its own branch below rather than routing through `consider()`.
    _SYNC_IDIOMS = frozenset({"map", "starmap", "for_each", "remote"})

    def visit_Call(self, node: ast.Call) -> None:
        if isinstance(node.func, ast.Attribute):
            attr = node.func.attr
            # `.map.aio(...)` / `.starmap.aio(...)`: async variant — func is X.<idiom>.aio,
            # so the idiom is the PENULTIMATE attribute and this is an async future.
            if attr == "aio" and isinstance(node.func.value, ast.Attribute) and node.func.value.attr in ("map", "starmap"):
                target = ".".join(_attr_chain(node.func.value)[:-1])
                self.invoke_calls.append({"target": target, "kind": "map.aio", "lineno": node.lineno, "entrypoint": self._current_entrypoint})
            elif attr == "map":
                target = ".".join(_attr_chain(node.func)[:-1])
                mc = {"target": target, "lineno": node.lineno, "entrypoint": self._current_entrypoint}
                ic = {"target": target, "kind": "map", "lineno": node.lineno, "entrypoint": self._current_entrypoint}
                # calque#136: capture the real .map() iterable when it's statically
                # resolvable (a literal list/tuple/str or a range() of literal ints),
                # so run/real/ramp/fleetrun can drive the actual item batch instead of
                # a synthesized placeholder. Omitted entirely (no "iterable": null)
                # when unresolved, matching every other optional marker in this file.
                iterable = _iterable_literal(node)
                if iterable is not None:
                    mc["iterable"] = iterable
                    ic["iterable"] = iterable
                # keep the legacy map_calls channel AND the unified invoke_calls one
                self.map_calls.append(mc)
                self.invoke_calls.append(ic)
            elif attr in self._SYNC_IDIOMS:
                ic = {"target": ".".join(_attr_chain(node.func)[:-1]), "kind": attr, "lineno": node.lineno, "entrypoint": self._current_entrypoint}
                if attr == "starmap":
                    # calque#136: same iterable capture as .map(), for the tuple-splat
                    # shape's own real data (e.g. combine.starmap([(1,2),(3,4)])).
                    iterable = _iterable_literal(node)
                    if iterable is not None:
                        ic["iterable"] = iterable
                self.invoke_calls.append(ic)
            elif attr == "spawn":
                # calque#88: .spawn(args) fires an async call — deferred per §18
                # (still leaked, block-and-wait only), but the TARGET is now also
                # classified (ir.InvokeSpawn) so a future fan-out driver can find
                # every spawned callable. Args captured best-effort via
                # _spawn_arg_str (calque#112: widened beyond from_name's
                # string-only _const_str, since .spawn()'s args are the actual
                # per-call PAYLOAD a real driver sends, not just an app-name
                # string — a numeric literal like worker.spawn(5) must not
                # silently collapse to the same None a variable reference gets).
                self.invoke_calls.append(
                    {
                        "target": ".".join(_attr_chain(node.func)[:-1]),
                        "kind": "spawn",
                        "lineno": node.lineno,
                        "args": [_spawn_arg_str(a) for a in node.args],
                        "entrypoint": self._current_entrypoint,
                    }
                )
            elif attr == "local":
                # calque#81: .local() runs the callee in the CALLER's own process —
                # no new container, no serialization boundary. Recorded so the Go
                # side can leak that the callee's body isn't shipped (calque ships
                # only the picked warm unit's body verbatim; a sibling function
                # referenced via .local() is not in scope and will NameError).
                self.invoke_calls.append(
                    {"target": ".".join(_attr_chain(node.func)[:-1]), "kind": "local", "lineno": node.lineno, "entrypoint": self._current_entrypoint}
                )
            elif attr in ("commit", "reload"):
                # §E: volume.commit()/reload() call sites. We record the target var
                # + kind; the Go side correlates `target` against known Volume vars
                # (so a random obj.commit() isn't mistaken for a volume write) and
                # leaks a MID-RUN mutation as a semantic gap.
                self.volume_writes.append(
                    {"target": ".".join(_attr_chain(node.func)[:-1]), "kind": attr, "lineno": node.lineno}
                )
            elif attr in ("include", "deploy"):
                # calque#91: App.include(...)/.deploy(...) — multi-app composition
                # and deploy-strategy lifecycle calque has no concept of at all
                # (always-ephemeral execution model). Deliberately NOT tagging
                # bare `.run()` here: unlike include/deploy, "run" is far too
                # generic a method name across unrelated objects (subprocess,
                # asyncio, arbitrary classes) to tag reliably without false
                # positives drowning out the real signal.
                self.leaks.append(
                    {"where": f"App.{attr}", "detail": f"App.{attr}(...) recognized but not modeled — no deployed-vs-ephemeral app concept (calque#91)", "lineno": node.lineno}
                )
            elif attr == "from_name" and _attr_chain(node.func)[-2:-1] in (["Function"], ["Cls"]):
                # calque#87: Function.from_name(app, fn)/Cls.from_name(app, cls) look
                # up an ALREADY-DEPLOYED separate app by name — cross-app invocation,
                # a fundamentally different execution boundary than anything calque
                # owns (calque parses+runs ONE script). Detected independently of
                # whatever's chained after it (.remote()/.spawn()/etc, which would
                # otherwise record a target-less "remote"/"spawn" invoke_calls entry
                # here — see the leading owner chain being empty in that case).
                # Guarded to Function/Cls specifically: Volume.from_name(...) and
                # Secret.from_name(...) are unrelated, already-handled constructs
                # that happen to share the same method name.
                base = _attr_chain(node.func)[-2]
                args = [_const_str(a) for a in node.args]
                self.invoke_calls.append(
                    {"target": base, "kind": "from_name", "lineno": node.lineno, "args": args, "entrypoint": self._current_entrypoint}
                )
        self.generic_visit(node)

    def _decos(self, node: ast.FunctionDef) -> list[str]:
        return [_decorator_name(d) for d in node.decorator_list]

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        decos = self._decos(node)
        leaves = {d.rsplit(".", 1)[-1] for d in decos}
        is_entrypoint = any(d.endswith("local_entrypoint") for d in decos)
        if is_entrypoint:
            self.entrypoints.append(_describe_fn(self.src, node, self.leaks, self._module_names))
        elif any(d.endswith("function") for d in decos) or (leaves & _SERVE_DECOS):
            # A serve entrypoint (§F) may carry @app.function too, or only the serve
            # decorator — collect it either way so its entry_kind reaches the IR.
            self.functions.append(_describe_fn(self.src, node, self.leaks, self._module_names))
        # methods handled inside ClassDef; free functions with neither decorator are ignored
        if is_entrypoint:
            # calque#98: push this entrypoint's name so every call site visited
            # while walking its body (below, via generic_visit) is attributed to
            # it — see _current_entrypoint / visit_Call's invoke_calls/map_calls
            # entries. Popped in a finally so a malformed body can't leave the
            # stack (and therefore every subsequent call site's attribution)
            # corrupted for the rest of the walk.
            self._entrypoint_stack.append(node.name)
            try:
                self.generic_visit(node)
            finally:
                self._entrypoint_stack.pop()
        else:
            self.generic_visit(node)

    def visit_AsyncFunctionDef(self, node) -> None:  # treat async defs the same
        self.visit_FunctionDef(node)  # type: ignore[arg-type]

    def visit_ClassDef(self, node: ast.ClassDef) -> None:
        decos = [_decorator_name(d) for d in node.decorator_list]
        if not any(d.endswith("cls") for d in decos):
            self.generic_visit(node)
            return
        cls_kwargs: dict[str, Any] = {}
        for d in node.decorator_list:
            name = _decorator_name(d)
            if name.endswith("cls"):
                # decorator order in source is not significant here, so merge
                # rather than overwrite — a separate @modal.concurrent(...) may
                # have already contributed kwargs below (or will, if it comes
                # later in decorator_list).
                cls_kwargs.update(_decorator_kwargs(d, self.leaks))
            elif name.endswith("concurrent"):
                # calque#82: @modal.concurrent(max_inputs=, target_inputs=) is a
                # SEPARATE decorator stacked on @app.cls, not a cls_kwargs entry —
                # fold its kwargs in so the Go side's autoscaling-leak path sees
                # them (the decorator itself carries no other calque-visible
                # config, so merging is safe: no real collision with cls_kwargs).
                cls_kwargs.update(_decorator_kwargs(d, self.leaks))
        methods: list[dict[str, Any]] = []
        enter: dict[str, Any] | None = None
        exit_: dict[str, Any] | None = None
        for item in node.body:
            if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)):
                mdecos = [_decorator_name(dd) for dd in item.decorator_list]
                desc = _describe_fn(self.src, item, self.leaks, self._module_names)  # type: ignore[arg-type]
                if any(dd.endswith("enter") for dd in mdecos):
                    enter = desc
                elif any(dd.endswith("exit") for dd in mdecos):
                    # calque#86: @modal.exit() runs ONCE at container shutdown —
                    # the documented pair to @enter. Previously fell into the
                    # generic "plain method" bucket below and would have been
                    # invoked on EVERY item if ever picked as a per-item method.
                    exit_ = desc
                elif any(dd.endswith("method") for dd in mdecos):
                    methods.append(desc)
                elif item.name == "__enter__":
                    # calque#138: pre-1.0 Modal API — before @modal.enter()
                    # existed, the class-lifecycle load-once hook was spelled
                    # as the bare context-manager dunder __enter__(self), no
                    # decorator at all. Recognize it as the load-once body
                    # (unless a real @modal.enter()-decorated method on this
                    # class already claimed that role — decorator-based
                    # recognition wins). Either way, exclude it from the
                    # generic methods list below: same calque#86 rationale —
                    # a load-once/teardown hook must never be eligible for
                    # pickWarmUnit's per-item "fall back to first method".
                    if enter is None:
                        enter = desc
                elif item.name == "__exit__":
                    # calque#138: legacy pair to __enter__ above —
                    # __exit__(self, exc_type, exc_value, traceback) is the
                    # pre-1.0 shutdown hook, predating @modal.exit(). Same
                    # precedence and methods-exclusion rules as __enter__.
                    if exit_ is None:
                        exit_ = desc
                else:
                    methods.append(desc)  # plain method inside @cls — keep, label by decos
        self.classes.append(
            {
                "name": node.name,
                "lineno": node.lineno,
                "cls_kwargs": cls_kwargs,
                "enter": enter,
                "exit": exit_,
                "methods": methods,
            }
        )
        # do not generic_visit into the class again (methods already handled)


def analyze(path: str) -> dict[str, Any]:
    with open(path, "r", encoding="utf-8") as f:
        src = f.read()
    tree = ast.parse(src, filename=path)
    # calque#139/#146: module-level def/simple-assign/import names, computed
    # ONCE up front so every function/method's _free_refs walk (inside
    # Collector, via _describe_fn) resolves against the same universe.
    module_func_nodes, module_const_nodes, module_import_nodes, module_class_nodes = _module_bindings(tree)
    module_names = (
        frozenset(module_func_nodes)
        | frozenset(module_const_nodes)
        | frozenset(module_import_nodes)
        | frozenset(module_class_nodes)
    )
    # calque#146.2: a module-level constant's OWN RHS can itself reference an
    # import or another constant (e.g. `forecast_volume =
    # modal.Volume.from_name(...)` needs `import modal` too) — module_consts
    # now carries free_refs alongside source, mirroring module_funcs' own
    # shape, so collectLocalExtras' transitive walk (cmd/calque/run.go) can
    # enqueue THROUGH a shipped constant, not just stop at it. Before this,
    # a constant that itself needed an import was shipped with no way to
    # discover that dependency, so the import never got enqueued and the
    # runner NameError'd on the constant's own exec.
    # calque#151: a module-level constant whose RHS is a live-Modal-control-
    # plane construct (modal.Dict/Queue/NetworkFileSystem.from_name(...))
    # must NOT be shipped the same way an ordinary literal constant is — the
    # runner has no live Modal credentials/connection, so exec'ing this
    # statement verbatim crashes with a confusing SDK auth error instead of
    # an honest leak. Tag it here (reusing calque#91's own
    # _unsupported_construct_from_name classifier) so the Go side
    # (collectLocalExtras) can refuse to ship it and emit a clear leak
    # instead.
    module_consts = {
        name: {
            "source": _stmt_source(src, node),
            "free_refs": _free_refs_in_expr(node.value, module_names),
            "unshippable_construct": (
                _unsupported_construct_from_name(_attr_chain(node.value.func))
                if isinstance(node.value, ast.Call)
                else None
            ),
        }
        for name, node in module_const_nodes.items()
    }
    # calque#146: verbatim source of every module-level import statement
    # (import X / from X import Y), keyed by EACH bound name — a bare,
    # non-.local() reference to an imported name (e.g. `Path(...)` after
    # `from pathlib import Path`) was previously an unconditional NameError
    # on execution, even though the script parsed fine; unlike a genuinely
    # re-exported name from ANOTHER module (still deliberately unresolved —
    # ambiguous without executing that module), a name THIS script imports
    # itself is unambiguous and shippable the same way a module-level
    # constant already is.
    module_imports = {name: _stmt_source(src, node) for name, node in module_import_nodes.items()}
    # module_funcs: EVERY module-level function, decorated or not — crucially
    # INCLUDING a plain, undecorated helper like `_format` that never becomes
    # an @app.function and so never lands in c.functions below. This is the
    # shippable shape a bare (non-.local()) call site resolves against; a
    # helper's own body may itself reference a sibling helper/constant, so it
    # carries its OWN local_calls/free_refs too (recursive resolution, mirrors
    # how a plain @app.function's LocalCalls already chains).
    module_funcs = {
        name: {
            "args": _arg_names(node),
            "body": _body_source(src, node),
            "local_calls": _local_calls(node),
            "free_refs": _free_refs(node, module_names),
        }
        for name, node in module_func_nodes.items()
    }
    # calque#147: verbatim source of every PLAIN (non-@app.cls) module-level
    # class, keyed by name — the FOURTH shippable free-reference target. A
    # bare reference like `_LogTee(sys.stdout, log_buffer)` inside a picked
    # unit's body was previously an unconditional NameError, the same shape
    # calque#139/#146 already fixed for functions/constants/imports. Ships
    # the class's WHOLE verbatim body (methods included) via _stmt_source,
    # which already handles a multi-line statement's source-line slicing
    # correctly regardless of statement kind.
    module_classes = {
        name: {
            "source": _stmt_source(src, node),
            "free_refs": _free_refs_in_class(node, module_names),
        }
        for name, node in module_class_nodes.items()
    }

    c = Collector(src, module_names)
    c.visit(tree)
    return {
        "script": path,
        "app_name": c.app_name,
        "images": c.images,
        "volumes": c.volumes,
        "functions": c.functions,
        "classes": c.classes,
        "entrypoints": c.entrypoints,
        "map_calls": c.map_calls,
        "invoke_calls": c.invoke_calls,
        "volume_writes": c.volume_writes,
        "helper_leaks": c.leaks,
        # calque#139/#146.2: verbatim source (+ the constant's OWN free_refs,
        # calque#146.2) of every module-level `NAME = <literal-or-expression>`
        # assignment, keyed by name — one of the three shippable free-
        # reference targets (see module_funcs/module_imports for the others).
        "module_consts": module_consts,
        # calque#139: every module-level function (decorated or not), keyed by
        # name — the OTHER shippable free-reference target. A plain,
        # undecorated helper (never an @app.function, so absent from
        # `functions` above) is exactly the shape the issue's `_format`
        # repro needs resolved.
        "module_funcs": module_funcs,
        # calque#146: verbatim source of every module-level import statement,
        # keyed by each name it binds — the THIRD shippable free-reference
        # target (see module_consts/module_funcs for the others).
        "module_imports": module_imports,
        # calque#147: verbatim source (+ free_refs) of every PLAIN (non-
        # @app.cls) module-level class — the FOURTH shippable free-reference
        # target.
        "module_classes": module_classes,
    }


def main(argv: list[str] | None = None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    if len(argv) != 1:
        print("usage: pyast.py <script.py>", file=sys.stderr)
        return 2
    try:
        out = analyze(argv[0])
    except (OSError, SyntaxError) as e:
        json.dump({"error": str(e), "script": argv[0]}, sys.stdout)
        return 1
    json.dump(out, sys.stdout, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
