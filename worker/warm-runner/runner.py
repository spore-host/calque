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
import sys
import time
import traceback
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

    def enter(self) -> float:
        if self.entered:
            # Enter must run exactly once. A second enter is a supervisor bug; we
            # refuse rather than silently reload (which would destroy the economics).
            raise RuntimeError("enter called twice; model would reload (see spec §6)")
        t0 = time.perf_counter()
        self._exec_body(self.enter_body, extra_locals={})
        self.entered = True
        return time.perf_counter() - t0

    def item(self, payload: Any) -> tuple[Any, float]:
        if not self.entered:
            raise RuntimeError("item before enter; warm state not loaded")
        t0 = time.perf_counter()
        # The @method body refers to its argument by name (e.g. `prompt`) and
        # returns a value. We bind the arg, exec the body, and capture the return.
        local_ns: dict[str, Any] = {self.method_arg: payload}
        result = self._exec_body(self.method_body, extra_locals=local_ns, capture_return=True)
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


class _Namespace:
    """A permissive attribute bag standing in for the @cls `self`."""

    pass


def _emit(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()  # flush every line so spored sees results as they land


def main(argv: list[str] | None = None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    # Config (enter/method bodies + arg name) arrives as a JSON file path or via a
    # first "config" line, so the harness is model-agnostic. We accept a config line.
    reader = io.TextIOWrapper(sys.stdin.buffer, encoding="utf-8")

    runner: Runner | None = None
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
                _emit({"kind": "configured"})
            elif kind == "enter":
                if runner is None:
                    raise RuntimeError("enter before config")
                secs = runner.enter()
                _emit({"kind": "ready", "enter_seconds": secs})
            elif kind == "item":
                if runner is None:
                    raise RuntimeError("item before config")
                result, secs = runner.item(msg.get("payload"))
                _emit({"kind": "result", "index": msg.get("index"), "seconds": secs, "result": result})
            elif kind == "shutdown":
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
