# Security Policy

calque is an experimental spike, but it holds real capability: it reads AWS
credentials and can **acquire billable GPU compute**. We take reports about that
surface seriously.

## Reporting a vulnerability

**Do not open a public issue for a security report** — reporting a spend or
credential flaw in the open exposes it before it's fixed.

Preferred channel is GitHub's private vulnerability reporting:

> Repository → **Security** tab → **Report a vulnerability**
> (https://github.com/spore-host/calque/security/advisories/new)

If that option isn't visible (the repo has to have it enabled), instead open a
public issue containing **only** the words "security report — request private
channel" and no details; a maintainer will open a private thread to receive the
specifics.

Either way, please be ready to share: what you found, how to reproduce it, the
affected version/commit, and the impact you observed. We'll acknowledge receipt and
keep you updated on the fix and disclosure timing.

## Scope — what matters most here

Because calque can spend money and touch cloud infrastructure, these are the
highest-severity areas:

- **Unintended spend.** Any path that acquires an AWS resource (EC2, etc.) without
  the explicit `--i-understand-this-spends-money` gate, or that could be triggered
  accidentally. The dry-run-default posture and the spend gate are the safety model;
  a way around either is a security issue, not just a bug.
- **Credential exposure.** Anything that logs, transmits, or persists AWS
  credentials, session tokens, or account-identifying data (AMIs, ARNs, bucket
  names) where they don't belong.
- **Code/command execution.** calque ships user Python function bodies to a worker
  verbatim and shells out (docker, aws, ssm). Report injection or
  privilege-escalation paths in that plumbing.
- **Supply chain.** The Go module graph and the `tools/pyast` Python deps
  (`uv.lock`).

## Out of scope

- The stub recommender returning a constant card — that's a documented, intentional
  fake behind the seam (§4), not a vulnerability.
- Cost estimates being wrong — file a normal issue.
- Reports requiring an attacker who already has your AWS credentials or repo write
  access.

## Handling secrets in contributions

Never commit credentials, `.env` files, account-tied AMIs, or measured run
artifacts. `.gitignore` already excludes `.env`, `/runs/`, and `*.local.json`; if
you add a new secret-bearing artifact type, extend it.
