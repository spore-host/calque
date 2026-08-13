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

The commands that acquire billable AWS hardware (`smoke`, `real`, `ramp`,
`pool create`/`scale`, `spawn-run`) are gated behind an explicit
`--i-understand-this-spends-money` flag and default to a dry-run posture.
`calque session` is NOT in this set — it never acquires or terminates an
EC2 instance itself, only checks a slice in/out on one someone else
already acquired (see `docs/tenancy-vs-session.md`); its risk category is
different (no hardware isolation under `--backend mps`), gated behind its
own `--i-understand-shared-gpu-has-no-isolation` flag instead. Preserve
this distinction: any new path that can acquire AWS resources must be
opt-in and impossible to trigger by accident, gated behind the flag whose
risk category actually matches. Never commit credentials, AMIs tied to an
account, or measured run artifacts.

## Before tagging a release

No version tag should move without checking the authoritative user-facing
docs against the CLI. Before tagging: diff the current CLI's flag surface
(`docs/guide/cli-reference.md`) against README/`docs/porting-modal-to-aws.md`/
`docs/modal-compatibility-matrix.md`'s claims, and confirm no doc still
describes pre-this-release behavior as current. See the two status-banner
forms below — a doc marked "Authoritative current behavior" is exactly the
kind that should be checked here.

## Doc status banners

Every doc in `docs/` should open with one of two status lines, so a reader
(and a release-checklist reviewer) can tell at a glance whether it
describes CURRENT behavior or a DESIGN DECISION that may already be
superseded by shipped code:

```
**Status:** Authoritative current behavior. Verified through: vX.Y.Z.
```
or
```
**Status:** Historical design decision. Implemented by: vX.Y.Z.
Current user documentation: `docs/guide/...` or wherever the shipped
behavior is actually described.
```

A design-record doc (the second form) is allowed to describe intent that
was never built, or that's since been superseded — that's fine, as long
as the banner makes it unambiguous this ISN'T a live status page. Several
docs in this repo were found describing already-shipped features as
"still unbuilt" (or vice versa) simply because nobody updated the status
line after the code landed — the banner exists so that class of drift is
easy to spot and fix in one glance, not a full re-read.

## Questions

Open an issue with the `meta:question` label — surfacing an integration question is
better than guessing (§17).
