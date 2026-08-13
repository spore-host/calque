# Design note: serve entrypoints on AWS (F3 — build deferred)

**Status:** Authoritative current behavior. Verified through: v0.3.1 —
serve is STILL not built; this decision record's own claim remains true.
Serve is DETECTED + gated/leaked in the spike (M9/F1,
F2), but the long-lived server is **not built** — the spike targets batch `.map()`
execution, and serve is a fundamentally different execution shape. This note
captures the architecture delta so the deferral is documented, not forgotten.

## What serve is

Modal's serve decorators — `@web_endpoint` / `@fastapi_endpoint`, `@asgi_app`,
`@wsgi_app`, `@web_server` — turn a function/class into a **long-lived,
request-driven** service: the container stays up, holds the loaded model warm, and
answers HTTP requests until scaled down. There is no fixed item set and no "done."

## Why it doesn't fit the spike's batch model

| Axis | Batch `.map` (what calque runs today) | Serve (deferred) |
|---|---|---|
| Work set | Fixed N items, known up front | Open-ended request stream |
| Lifecycle | Acquire → warm `@enter` → drain N → **terminate** | Acquire → warm → **stay up** → scale down on idle |
| Collection | S3, keyed by input index, ordered | Per-request response over the wire; no index collect |
| Termination | Deterministic (last item settles) | Autoscaling / idle-timeout driven |

A server has no fixed N — its economics are requests/sec against a latency SLO, and
however long you keep it up is an autoscaling decision (behind the seam, §4). The
batch warm-runner's whole shape (drain a manifest, terminate when done) simply
doesn't apply to an open-ended request stream.

## What a future build would touch (attach points)

- **Execution shape.** A new runner mode alongside warmd's batch drain: warm
  `@enter` once, then serve a port instead of draining a manifest. warmd's
  supervisor (`worker/warm-runner/warmd.go`) stays up rather than exiting after the
  last item settles.
- **Ingress.** A target group / load balancer in front of the acquired instance(s)
  — new territory; `internal/exec` currently only does one-shot bootstrap + S3
  collect.
- **No S3 index collect.** Results return per-request; `internal/exec/s3sink.go`'s
  index-keyed collect does not apply.
- **Autoscaling.** min/max containers, keep-warm, concurrency — exactly the
  behind-the-seam kwargs M10/S1 recognizes and leaks. The real scaling brain lives
  behind the Recommender seam (§4).

## What the spike DOES do today (F1/F2)

- `tools/pyast/pyast.py` detects the serve decorators and tags the function
  `entry_kind: "serve"`; `parse` carries it onto `ir.Function.EntryKind`.
- `cmd/calque/run.go` no longer hard-errors on a serve-shaped app — it emits a
  `semantic_gap` leak explaining the deferred shape.
- The Bedrock gate (§11, M5) still applies: a **served model that's on Bedrock
  routes away** just like a batch one — `run.go` runs the route-away gate on serve
  apps before giving up, so "call the API instead" holds regardless of shape.
