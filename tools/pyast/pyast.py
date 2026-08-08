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


def _volumes_map(node: ast.AST) -> dict[str, str] | None:
    """Extract `volumes={"/mount": vol_handle}` as {mount_path: volume_var_name}.

    The keys are string literals (mount paths); the values are Volume *variables*
    (handles), so `literal_eval` on the whole dict fails. We resolve it structurally
    to match IR §14 `Volumes map[string]string // mount path -> volume name`.
    Returns None if this isn't a dict we can map.
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
            out[key] = ast.unparse(v)  # e.g. Volume.from_name(...) inline
    return out


def _decorator_kwargs(node: ast.AST) -> dict[str, Any]:
    """kwargs of a decorator call. `gpu=`, `timeout=`, `image=`, `volumes=` etc."""
    if not isinstance(node, ast.Call):
        return {}
    out: dict[str, Any] = {}
    for kw in node.keywords:
        if kw.arg is None:  # **kwargs splat — record as a leak signal
            out["__splat__"] = ast.unparse(kw.value)
            continue
        if kw.arg == "volumes":
            vm = _volumes_map(kw.value)
            out["volumes"] = vm if vm is not None else _literal(kw.value)
            continue
        if kw.arg == "image" and isinstance(kw.value, ast.Name):
            # image=image_var — record the referenced var name so IR can resolve it.
            out["image"] = {"__ref__": kw.value.id}
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


def _describe_fn(src: str, node: ast.FunctionDef) -> dict[str, Any]:
    decos = []
    for d in node.decorator_list:
        decos.append(
            {
                "name": _decorator_name(d),
                "kwargs": _decorator_kwargs(d),
                "lineno": getattr(d, "lineno", node.lineno),
            }
        )
    return {
        "name": node.name,
        "lineno": node.lineno,
        "args": _arg_names(node),
        "decorators": decos,
        "entry_kind": _entry_kind([d["name"] for d in decos]),
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

    Returns {base, steps:[{method, args}], base_unresolved: bool} or None if this
    isn't an Image chain. A chain counts as an image only if it contains a known base
    constructor or at least one known build step — otherwise `App(...)` / `Volume.from_name(...)`
    would be misread as images. We resolve the chain structurally; we never execute it.
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
    return {"base": base, "steps": steps, "base_unresolved": base is None}


class Collector(ast.NodeVisitor):
    def __init__(self, src: str) -> None:
        self.src = src
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

    # ---- module-level assignments: App(), Image chains, Volume.from_name() ----
    def visit_Assign(self, node: ast.Assign) -> None:
        val = node.value
        # app = modal.App("name")
        if isinstance(val, ast.Call) and _attr_chain(val.func)[-1:] == ["App"]:
            if val.args:
                self.app_name = _const_str(val.args[0]) or self.app_name
            kw = _decorator_kwargs(val)
            if "image" in kw:  # App(image=...) — note it
                self.leaks.append(
                    {"where": "App(image=)", "detail": "app-level default image", "lineno": node.lineno}
                )
        # image = modal.Image.debian_slim().pip_install(...)
        chain = _walk_image_chain(val) if isinstance(val, ast.Call) else None
        if chain is not None:
            for t in node.targets:
                if isinstance(t, ast.Name):
                    self.images[t.id] = chain
        # vol = modal.Volume.from_name("weights")
        if isinstance(val, ast.Call) and _attr_chain(val.func)[-2:] == ["Volume", "from_name"]:
            name = _const_str(val.args[0]) if val.args else None
            for t in node.targets:
                if isinstance(t, ast.Name):
                    self.volumes[t.id] = {"from_name": name, "lineno": node.lineno}
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
                self.invoke_calls.append({"target": target, "kind": "map.aio", "lineno": node.lineno})
            elif attr == "map":
                target = ".".join(_attr_chain(node.func)[:-1])
                # keep the legacy map_calls channel AND the unified invoke_calls one
                self.map_calls.append({"target": target, "lineno": node.lineno})
                self.invoke_calls.append({"target": target, "kind": "map", "lineno": node.lineno})
            elif attr in self._SYNC_IDIOMS or attr == "spawn":
                self.invoke_calls.append(
                    {"target": ".".join(_attr_chain(node.func)[:-1]), "kind": attr, "lineno": node.lineno}
                )
            elif attr == "local":
                # calque#81: .local() runs the callee in the CALLER's own process —
                # no new container, no serialization boundary. Recorded so the Go
                # side can leak that the callee's body isn't shipped (calque ships
                # only the picked warm unit's body verbatim; a sibling function
                # referenced via .local() is not in scope and will NameError).
                self.invoke_calls.append(
                    {"target": ".".join(_attr_chain(node.func)[:-1]), "kind": "local", "lineno": node.lineno}
                )
            elif attr in ("commit", "reload"):
                # §E: volume.commit()/reload() call sites. We record the target var
                # + kind; the Go side correlates `target` against known Volume vars
                # (so a random obj.commit() isn't mistaken for a volume write) and
                # leaks a MID-RUN mutation as a semantic gap.
                self.volume_writes.append(
                    {"target": ".".join(_attr_chain(node.func)[:-1]), "kind": attr, "lineno": node.lineno}
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
                    {"target": base, "kind": "from_name", "lineno": node.lineno, "args": args}
                )
        self.generic_visit(node)

    def _decos(self, node: ast.FunctionDef) -> list[str]:
        return [_decorator_name(d) for d in node.decorator_list]

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        decos = self._decos(node)
        leaves = {d.rsplit(".", 1)[-1] for d in decos}
        if any(d.endswith("local_entrypoint") for d in decos):
            self.entrypoints.append(_describe_fn(self.src, node))
        elif any(d.endswith("function") for d in decos) or (leaves & _SERVE_DECOS):
            # A serve entrypoint (§F) may carry @app.function too, or only the serve
            # decorator — collect it either way so its entry_kind reaches the IR.
            self.functions.append(_describe_fn(self.src, node))
        # methods handled inside ClassDef; free functions with neither decorator are ignored
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
                cls_kwargs.update(_decorator_kwargs(d))
            elif name.endswith("concurrent"):
                # calque#82: @modal.concurrent(max_inputs=, target_inputs=) is a
                # SEPARATE decorator stacked on @app.cls, not a cls_kwargs entry —
                # fold its kwargs in so the Go side's autoscaling-leak path sees
                # them (the decorator itself carries no other calque-visible
                # config, so merging is safe: no real collision with cls_kwargs).
                cls_kwargs.update(_decorator_kwargs(d))
        methods: list[dict[str, Any]] = []
        enter: dict[str, Any] | None = None
        exit_: dict[str, Any] | None = None
        for item in node.body:
            if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)):
                mdecos = [_decorator_name(dd) for dd in item.decorator_list]
                desc = _describe_fn(self.src, item)  # type: ignore[arg-type]
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
    c = Collector(src)
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
