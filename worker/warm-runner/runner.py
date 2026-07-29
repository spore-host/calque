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

    def __init__(self, enter_body: str, method_body: str, method_arg: str) -> None:
        self.enter_body = enter_body
        self.method_body = method_body
        self.method_arg = method_arg  # the @method's item parameter name, e.g. "prompt"
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
        result = fn(self.state, payload)
        return result, time.perf_counter() - t0

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
        src = f"def __calque_method__(self, {self.method_arg}):\n{indented}\n"
        code = compile(src, "<calque-method>", "exec")
        exec(code, self.globals)
        self._method_fn = self.globals["__calque_method__"]


class _Namespace:
    """A permissive attribute bag standing in for the @cls `self`."""

    pass


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
