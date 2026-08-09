#!/usr/bin/env python3
"""calque warm Python runner (spec §6).

The riskiest plumbing in the spike: a long-lived Python process that loads the
model ONCE (the @enter body) and then processes many items against it, so the
load cost amortizes. The naive "one item = one process" mapping silently reloads
the model per item and destroys the economics — this process exists precisely to
NOT do that.

Protocol (decision #7): newline-framed JSON over stdio, serial.
  stdin  <- one JSON object per line, each a request
  stdout -> one JSON object per line, each a response (same order — serial)

Request kinds:
  {"kind":"enter"}                              run the @enter body ONCE
  {"kind":"item","index":<int>,"payload":<any>} process one item; index echoed back
  {"kind":"shutdown"}                           flush and exit cleanly

Response kinds:
  {"kind":"ready","enter_seconds":<float>}          @enter completed (once)
  {"kind":"result","index":<int>,"seconds":<float>,"result":<any>}
  {"kind":"error","index":<int|null>,"error":<str>,"traceback":<str>}
  {"kind":"bye"}                                    clean shutdown ack

The @enter and @method bodies are the VERBATIM Modal payload (extracted as
strings by the parser, §13). They are exec'd here, unchanged, exactly as they ran
on Modal — the control plane never interpreted them. This file is the thin harness
that gives them a warm process and a socket.

warmd (our Go supervisor) owns this process's lifecycle: it starts us, sends
enter once, drains items, writes each result to S3 keyed by index, and on a crash
restarts us and re-drives unfinished items. We just have to be a well-behaved
serial worker and fail loudly (structured error) rather than silently.

(Note: "warmd" is calque's supervisor, distinct from the spore.host "spored"
systemd daemon that owns the whole instance's lifecycle and runs warmd under it.)
"""

from __future__ import annotations

import io
import json
import os
import sys
import threading
import time
import traceback
from concurrent.futures import ThreadPoolExecutor
from typing import Any


class Runner:
    """Holds warm state between the one @enter and the many @method calls.

    The Modal @cls instance is emulated by a namespace object `self_ns` that the
    bodies read/write via `self.` — we rewrite `self` to a module-level handle so
    the verbatim body (which says `self.llm = ...`) mutates state that survives
    across items. See _exec_body.
    """

    def __init__(
        self,
        enter_body: str,
        method_body: str,
        method_arg: str,
        extras: list[dict] | None = None,
        method_args: list[str] | None = None,
        starmap: bool = False,
    ) -> None:
        self.enter_body = enter_body
        self.method_body = method_body
        self.method_arg = method_arg  # the @method's item parameter name, e.g. "prompt"
        # method_args (calque#93) is the FULL non-self/cls parameter list, e.g.
        # ["a","b"] for combine(self,a,b) — only meaningful when starmap is True.
        # Falls back to [method_arg] so _compile_method has something to bind
        # even if a caller sets starmap without also setting method_args.
        self.method_args = method_args or ([method_arg] if method_arg else [])
        # starmap (calque#93): this unit is .starmap()'d — bind EVERY name in
        # method_args (tuple-splat), not just the first. False (the default)
        # is the original single-arg bind path, byte-for-byte unchanged.
        self.starmap = starmap
        self.extras = extras or []  # .local()-referenced sibling functions (calque#92)
        self.entered = False
        # The warm namespace: a stand-in for the @cls instance `self`. Bodies see
        # it as `self`; whatever @enter assigns (self.llm = ...) lives here and is
        # visible to every subsequent @method call. This IS the load-once state.
        self.state = _Namespace()
        # Globals shared by enter and method bodies (imports done in @enter persist).
        self.globals: dict[str, Any] = {"__name__": "__calque_worker__"}
        # Compiled @method function, built once in enter() (see _compile_method).
        self._method_fn = None

    def enter(self) -> float:
        if self.entered:
            # Enter must run exactly once. A second enter is a supervisor bug; we
            # refuse rather than silently reload (which would destroy the economics).
            raise RuntimeError("enter called twice; model would reload (see spec §6)")
        t0 = time.perf_counter()
        # Extras go into self.globals FIRST — before @enter itself runs — so an
        # @enter body that .local()-calls a sibling (calque#92) can resolve it too,
        # not just @method.
        self._compile_extras()
        self._exec_body(self.enter_body, extra_locals={})
        # Compile the @method body once, AFTER @enter (so any imports @enter added to
        # self.globals are in scope for the method). Reused for every item, incl.
        # concurrent calls.
        self._compile_method()
        self.entered = True
        return time.perf_counter() - t0

    def item(self, payload: Any) -> tuple[Any, float]:
        if not self.entered:
            raise RuntimeError("item before enter; warm state not loaded")
        t0 = time.perf_counter()
        # Call the compiled @method function (built once in enter()). Under
        # concurrency (C>1) many threads call this at once — each call binds only
        # its own args and shares `self.state`, exactly as concurrent @method calls
        # would on a Modal container. We do NOT hold a lock across the body: the
        # whole point is to let inference overlap (the library releases the GIL).
        # Bodies that mutate shared self.state under concurrency race the same way
        # they would on Modal — that's the honest behavior, not something we serialize.
        fn = self._method_fn
        if fn is None:  # method body compiled lazily at first enter
            raise RuntimeError("method not compiled; enter did not run")
        # calque#93: a .starmap()'d unit's payload is a tuple/list of the
        # callable's real positional args (e.g. [1, 2] for combine(1, 2)) —
        # splat it so every one of __calque_method__'s params binds, matching
        # Modal's own tuple-unpack semantics. Every other invocation kind
        # (map/for_each/remote) is unaffected: starmap defaults False, so this
        # branch is simply never taken and fn(self.state, payload) runs exactly
        # as before.
        if self.starmap and isinstance(payload, (list, tuple)):
            result = fn(self.state, *payload)
        else:
            result = fn(self.state, payload)
        return result, time.perf_counter() - t0

    def batch(self, payloads: list) -> Any:
        """Call the compiled method function ONCE with the whole payload LIST bound
        to the method arg. In batch mode the @method body is batch-shaped — it takes
        the list (e.g. `prompts`) and returns a list — so vLLM batches the whole
        group in a single .generate(list) call. Same self.state as the serial path.
        """
        if not self.entered:
            raise RuntimeError("batch before enter; warm state not loaded")
        fn = self._method_fn
        if fn is None:
            raise RuntimeError("method not compiled; enter did not run")
        return fn(self.state, payloads)

    def _exec_body(self, body: str, extra_locals: dict, capture_return: bool = False) -> Any:
        """Exec a verbatim Modal body with `self` bound to the warm namespace.

        Modal method bodies contain bare `return` statements; `exec` can't run a
        top-level return, so we wrap the body in a function def and call it. The
        wrapper takes `self` and any method args, so the body is textually unchanged
        inside. Imports and assignments to `self.` persist across calls via
        self.globals / self.state.
        """
        arg_names = ["self", *extra_locals.keys()]
        indented = "\n".join("    " + ln for ln in body.splitlines()) or "    pass"
        src = f"def __calque_fn__({', '.join(arg_names)}):\n{indented}\n"
        code = compile(src, "<calque-body>", "exec")
        exec(code, self.globals)  # defines __calque_fn__ in the shared globals
        fn = self.globals["__calque_fn__"]
        ret = fn(self.state, *extra_locals.values())
        return ret if capture_return else None

    def _compile_method(self) -> None:
        """Compile the @method body ONCE into a reusable function (thread-safe to
        call concurrently). The old path re-exec'd the body per item, defining
        __calque_fn__ in shared globals each time — a data race under concurrency
        (two threads clobber the same name). Compiling once, into a distinct name,
        removes that race; the returned function closes over nothing mutable beyond
        the shared self.state, which is intentional (see item()).
        """
        body = self.method_body
        indented = "\n".join("    " + ln for ln in body.splitlines()) or "    pass"
        # calque#93: a .starmap()'d unit's real signature takes MULTIPLE
        # positional args (e.g. combine(self, a, b)) — bind every name in
        # method_args, not just the first, so item()'s *payload splat has
        # somewhere to land. Every other invocation kind keeps the original
        # single-param signature, unchanged.
        if self.starmap and self.method_args:
            params = ", ".join(self.method_args)
        else:
            params = self.method_arg
        src = f"def __calque_method__(self, {params}):\n{indented}\n"
        code = compile(src, "<calque-method>", "exec")
        exec(code, self.globals)
        self._method_fn = self.globals["__calque_method__"]

    def _compile_extras(self) -> None:
        """Compile every .local()-referenced sibling function (calque#92) into
        self.globals, so the CALLING body's verbatim `helper(x)` or
        `helper.local(x)` text resolves unmodified — bodies stay payload, never
        rewritten. Unlike @enter/@method, an extra's own real Modal signature has
        NO `self` (it's a plain @app.function, not a @cls method) — its args are
        used exactly as captured.

        All extras land in self.globals before any of them (or @enter/@method)
        actually runs, and Python resolves bare names at CALL time — so extras
        can reference each other, including self-reference and cycles, with no
        compile-order requirement here (calque#92's Go-side transitive-closure
        walk already bounds WHICH extras are shipped; this just binds them).
        """
        for i, extra in enumerate(self.extras):
            name = extra["name"]
            args = extra.get("args") or []
            body = extra.get("body") or ""
            indented = "\n".join("    " + ln for ln in body.splitlines()) or "    pass"
            fn_name = f"__calque_extra_{i}__"
            src = f"def {fn_name}({', '.join(args)}):\n{indented}\n"
            code = compile(src, f"<calque-extra:{name}>", "exec")
            exec(code, self.globals)
            self.globals[name] = _LocalCallable(self.globals[fn_name])


class _Namespace:
    """A permissive attribute bag standing in for the @cls `self`."""

    pass


class _LocalCallable:
    """Wraps a compiled .local()-referenced sibling function (calque#92) so a
    verbatim Modal call site works UNCHANGED whether written as `helper(x)`
    (a bare call — same process either way in real Modal) or `helper.local(x)`
    (explicit same-process call) — real Python functions have no `.local`
    attribute, only Modal's SDK-wrapped callables do, so this stands in for
    that wrapper.
    """

    def __init__(self, fn) -> None:
        self._fn = fn
        self.local = fn  # `.local(...)` call sites bind straight to the same fn

    def __call__(self, *args, **kwargs):
        return self._fn(*args, **kwargs)


# The protocol channel. warmd frames responses as newline-JSON on our stdout —
# but the verbatim payload (vLLM, transformers, tqdm, ...) ALSO prints to stdout
# ("INFO ... Initializing an LLM engine"), which corrupts the frame: warmd sees
# 'I' and fails to decode JSON, restart-loops, and never loads the model (observed
# on a real g6 run — spec §6's "the socket protocol draws blood" edge). Fix: grab
# the real stdout fd ONCE as the private protocol channel, then point sys.stdout at
# stderr so any library print goes to stderr and can never pollute the protocol.
_PROTO = os.fdopen(os.dup(sys.stdout.fileno()), "w", encoding="utf-8")
sys.stdout.flush()
os.dup2(sys.stderr.fileno(), sys.stdout.fileno())  # library stdout -> stderr


# Under concurrency (C>1) multiple worker threads emit results, so the write+flush
# to the single protocol channel must be atomic or newline frames interleave and
# corrupt the JSON stream. The lock is uncontended in the serial (C=1) path.
_EMIT_LOCK = threading.Lock()


def _emit(obj: dict) -> None:
    line = json.dumps(obj) + "\n"
    with _EMIT_LOCK:
        _PROTO.write(line)
        _PROTO.flush()  # flush every line so warmd sees responses as they land


def _process_item(runner: Runner, index: Any, payload: Any) -> None:
    """Run one item and emit its result/error. Safe to call from a pool thread —
    _emit is locked, and runner.item() shares only self.state (see item()). The
    per-item try/except keeps a bad payload from killing the worker thread: it
    becomes a structured 'error' (a partial failure), exactly as in the serial path.
    """
    try:
        result, secs = runner.item(payload)
        _emit({"kind": "result", "index": index, "seconds": secs, "result": result})
    except Exception as e:
        _emit({"kind": "error", "index": index, "error": str(e), "traceback": traceback.format_exc()})


def _process_batch(runner: Runner, indices: list, payloads: list) -> None:
    """Run the method body ONCE over a LIST of payloads and emit one batch_result
    with a per-item outcome for each index. This is the micro-batch path: the body
    receives the whole list (e.g. self.llm.generate(prompts, ...)) so vLLM batches
    internally — the real GPU-occupancy lever. Contract: the body returns a LIST
    aligned 1:1 with payloads. A whole-batch exception fails every item in the
    batch (structured, not a crash); a length mismatch fails each item with a clear
    error. Per-item wall-clock is the batch time / count (overlapping by nature).
    """
    n = len(payloads)
    try:
        t0 = time.perf_counter()
        results = runner.batch(payloads)
        secs = (time.perf_counter() - t0) / max(n, 1)
        if not isinstance(results, (list, tuple)) or len(results) != n:
            raise ValueError(
                f"batch body must return a list of {n} results aligned to inputs, "
                f"got {type(results).__name__} of len {len(results) if hasattr(results, '__len__') else 'n/a'}"
            )
        items = [{"index": idx, "result": r, "seconds": secs} for idx, r in zip(indices, results)]
        _emit({"kind": "batch_result", "results": items})
    except Exception as e:
        # Whole-batch failure: report each item as failed (partial failure), so the
        # supervisor records them without treating it as a runner crash.
        tb = traceback.format_exc()
        items = [{"index": idx, "error": str(e), "traceback": tb} for idx in indices]
        _emit({"kind": "batch_result", "results": items})


def main(argv: list[str] | None = None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    # Config (enter/method bodies + arg name) arrives as a JSON file path or via a
    # first "config" line, so the harness is model-agnostic. We accept a config line.
    reader = io.TextIOWrapper(sys.stdin.buffer, encoding="utf-8")

    runner: Runner | None = None
    pool: ThreadPoolExecutor | None = None  # non-None only when concurrency > 1
    for line in reader:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError as e:
            _emit({"kind": "error", "index": None, "error": f"bad json: {e}", "traceback": ""})
            continue

        kind = msg.get("kind")
        try:
            if kind == "config":
                runner = Runner(
                    enter_body=msg.get("enter_body", ""),
                    method_body=msg.get("method_body", ""),
                    method_arg=msg.get("method_arg", "item"),
                    extras=msg.get("extras") or [],
                    method_args=msg.get("method_args") or None,
                    starmap=bool(msg.get("starmap", False)),
                )
                # concurrency=C>1 => items run in a C-wide thread pool so inference
                # overlaps (vLLM batches in-flight requests). Absent/1 => the serial
                # path below, byte-for-byte the original behavior.
                conc = int(msg.get("concurrency", 1) or 1)
                if conc > 1:
                    pool = ThreadPoolExecutor(max_workers=conc)
                _emit({"kind": "configured"})
            elif kind == "enter":
                if runner is None:
                    raise RuntimeError("enter before config")
                secs = runner.enter()
                _emit({"kind": "ready", "enter_seconds": secs})
            elif kind == "item":
                if runner is None:
                    raise RuntimeError("item before config")
                if pool is not None:
                    # Concurrent: submit and move on. The result/error is emitted by
                    # the worker thread when it finishes — OUT OF ORDER, keyed by
                    # index (warmd collects by index). Backpressure: warmd caps items
                    # in flight at C (it won't send the C+1th until one lands).
                    pool.submit(_process_item, runner, msg.get("index"), msg.get("payload"))
                else:
                    _process_item(runner, msg.get("index"), msg.get("payload"))
            elif kind == "batch":
                # Micro-batch: the method body is called ONCE with the LIST of B
                # payloads (batch-shaped body — how vLLM actually batches: a single
                # .generate([p1..pB]) fills the GPU). It returns a LIST of B results,
                # aligned to indices. A whole-batch failure marks every item failed;
                # a body that returns the wrong count is a structured error per item.
                if runner is None:
                    raise RuntimeError("batch before config")
                _process_batch(runner, msg.get("indices") or [], msg.get("payloads") or [])
            elif kind == "shutdown":
                # Drain in-flight work before acking, so no result is lost on exit.
                if pool is not None:
                    pool.shutdown(wait=True)
                    pool = None
                _emit({"kind": "bye"})
                return 0
            else:
                _emit({"kind": "error", "index": msg.get("index"), "error": f"unknown kind {kind!r}", "traceback": ""})
        except Exception as e:  # fail loudly, structured — spored decides retry
            _emit(
                {
                    "kind": "error",
                    "index": msg.get("index"),
                    "error": str(e),
                    "traceback": traceback.format_exc(),
                }
            )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
