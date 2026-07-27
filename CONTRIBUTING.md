# Contributing to calque

calque is a spike (see [`README.md`](README.md) for what that means). Contributions
are welcome, but the bar is set by the project's core value: **intellectual
honesty**. A change that makes calque *look* more capable than it is measured to be
is worse than no change.

## The one principle: recognize and leak, never silently drop

calque translates Modal idioms onto AWS. When an idiom doesn't carry — multi-GPU,
tensor-parallel, async futures, a served endpoint, a mid-run volume commit — the
correct behavior is to **recognize it and emit a structured leak** (`internal/leak`),
not to silently ignore it or fake a substitution. An omission a user can see is a
decision; a silent gap is a bug. New parsing/execution work is reviewed against
this first.

## Dev setup

Prerequisites: **Go 1.26** (matches `go.mod`), **Python 3**, and
[`uv`](https://docs.astral.sh/uv/).

```
go build ./...                 # control plane
(cd tools/pyast && uv sync)    # Python AST helper deps
```

## Before you push

Run what CI runs (`.github/workflows/ci.yml`) — all of it must be green:

```
gofmt -l .        # must print nothing
go vet ./...
go build ./...
go test ./...     # with uv installed, the pyast contract tests run (not skip)
```

- **gofmt is a hard gate** — unformatted code fails CI.
- The `internal/parse` tests skip when `uv` is absent. With `uv` installed they
  exercise the Go↔Python contract; CI installs `uv` and treats a skip as a failure,
  so verify locally with `uv` present.
- If you change the pyast JSON contract (`tools/pyast/pyast.py` ↔ `internal/parse`),
  update **both** sides and the contract tests in the same change.

## Workflow

1. Branch off `main` (e.g. `feat/…`, `fix/…`, `docs/…`). Don't commit to `main`.
2. Keep commits focused; write a body explaining the *why*, not just the *what*.
3. Open a PR against `main`. PRs are landed with **rebase-and-merge** to keep
   history linear and preserve per-change commit messages — so a stack of commits
   should each stand on its own.
4. Reference the issue(s) the PR closes.

## Tracking lives on GitHub, not in the repo

Decisions, progress, and integration questions are tracked in **GitHub Issues /
milestones**, not in local files. Don't add `TODO.md`/`STATUS.md`-style tracking
files — file an issue instead. Runtime leak reports (`internal/leak`) are
*reproduced*, not committed.

## Anything that spends money

The billable commands (`real`, `session`, `smoke`) are gated behind an explicit
`--i-understand-this-spends-money` flag and default to a dry-run posture. Preserve
that: any new path that can acquire AWS resources must be opt-in and impossible to
trigger by accident. Never commit credentials, AMIs tied to an account, or measured
run artifacts.

## Questions

Open an issue with the `meta:question` label — surfacing an integration question is
better than guessing (§17).
