# Changelog

All notable changes to **calque** are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
— read `0.x` as "the supported contract can still shift release to release,"
per [semver.org](https://semver.org/#spec-item-4).

## [Unreleased]

### Fixed

- A picked warm unit with 2+ non-self/cls positional args that ISN'T
  `.starmap()`'d (e.g. a `.spawn()`-invoked function like AI-Almanac's
  `forecasts_app.py`'s `run_forecast_inference(job_id, model_id, config)`)
  now refuses loudly instead of silently `NameError`-ing on every synthetic
  item (calque#187) — the warm runner only ever bound the FIRST positional
  arg outside the `.starmap()` splat path, leaving the rest undefined.
  Found while re-verifying calque#79's closing claim that AI-Almanac's
  three real scripts run end-to-end: `forecasts_app.py`'s and
  `blending_app.py`'s picked units both hit this unannounced. Does NOT
  affect `calque real --arg-file`/`--arg-json` (a real, caller-supplied
  positional tuple, e.g. `app.py`'s `run_benchmark_local` — calque#178's
  real-hardware verification) — that path explicitly bypasses the new
  arity guard, since it already supplies real per-position data.

## [0.6.0] - 2026-08-15

22 commits since v0.5.0 (folding in the changes originally staged under an
untagged `[0.5.1]` section below, which never got its own git tag — see that
section's own note). Minor bump: three real construct-mapping features
(on-instance Dockerfile build for a layered `--script` image, curated
GPU card-swap substitution, and closing calque#91's `modal.CloudBucketMount`
Workstream A / `modal.NetworkFileSystem` Workstream B into real S3/EFS
mounts), a new `calque ami bake/list/delete` opt-in AMI-prebake path, and a
quota-headroom-polling correctness fix are more than housekeeping.

Also fixes a real doc-drift gap caught by this release's own pre-tag
checklist (`CONTRIBUTING.md` "Before tagging a release"): `docs/guide/
getting-started.md`'s IAM policy JSON snippet said `RealRunPolicy` "grants
exactly" two S3 statements — stale since calque#176 (ECR pull permissions,
shipped before v0.5.0's own tag but never reflected here) and calque#91
Workstream A (per-script `CloudBucketMount` bucket grants, this release).
Fixed to describe the real, current statement set.

### Added

- Real `modal.NetworkFileSystem` → EFS mount support (calque#91 Workstream
  B), the second real-mapping workstream calque#91 was tracking (Workstream
  A, `modal.CloudBucketMount` → S3, this same release — see below). A real
  `NetworkFileSystem.from_name(name)` used as a
  `network_file_systems={mount: nfs}` value (a SEPARATE decorator kwarg
  from `volumes=`, never nested inside it) now resolves to a real EFS-over-
  NFS mount — bring-your-own only: calque never auto-creates an EFS
  filesystem (`create_if_missing=True` is a distinct leak, not a blocker),
  discovering the pre-provisioned filesystem via a `calque:nfs-name=<name>`
  tag convention. A `calque real`/`fleetrun` narrows its AZ sweep to only
  AZs with a live EFS mount target for every required mount (a hard error,
  not a leak, if that narrows to zero) and attaches a self-referential
  NFS/2049-ingress security group to the launched instance. IAM
  (`elasticfilesystem:ClientMount`/`ClientWrite`) is explicitly out of
  scope for this pass. `modal.Dict`/`Queue`/`App.include` remain
  deliberately leak-only, out of scope.
- Real `modal.CloudBucketMount` → S3 mount support (calque#91 Workstream
  A): a real `CloudBucketMount(bucket_name, key_prefix=, read_only=)` used
  INLINE as a `volumes=` value (the real Modal idiom — constructed
  directly in the dict, not assigned to a variable first) resolves to a
  real `mountpoint-s3` mount against the SCRIPT'S OWN S3 bucket, spliced
  into the bootstrap script before `@enter` runs. The instance role gains
  read/write/list access to that bucket specifically, separate from
  calque's own `--bucket` staging area. `secret=`/`bucket_endpoint_url=`/
  `requester_pays=`/`force_path_style=` are each leaked distinctly as
  unhonored — mounting is against AWS S3 with default settings only,
  authenticated via the instance's own IAM role.
- New `calque ami bake`/`ami list`/`ami delete` subcommands (calque#144)
  pre-bake a custom AMI with a docker image's layers already pulled, so
  `real`/`ramp`/`fleetrun`/`spawn-run`'s bootstrap no longer pays a fresh
  multi-GB Docker Hub pull on every single boot — the existing
  unconditional `docker pull` line becomes a fast manifest check against
  the AMI's own cache. Image-only: model weights stay fully free-form via
  `--model` and are never baked. Purely additive/opt-in — bake an AMI,
  then pass its id to any run command's existing `--ami` flag explicitly;
  no run command auto-selects a baked AMI.
- A `--script` real run whose picked unit's `.image` chain has steps
  LAYERED on a pullable `from_registry`/`from_aws_ecr` base, or no
  pullable base at all, now gets its Dockerfile built ON THE ACQUIRED
  INSTANCE (calque#177) instead of falling back to a hand-typed
  `--pip`/`--stage-file` substitute — no ECR round-trip, no ambient
  Docker requirement on the caller's machine, no second/throwaway
  instance. A bare, unlayered `from_registry`/`from_aws_ecr` reference
  (calque#176) still just `docker pull`s the exact ref, authenticating via
  `aws ecr get-login-password` for ECR hostnames.
- `calque real`'s new `--allow-card-swap` flag (calque#178) opts into a
  curated, real-hardware-verified table of GPU substitutions (e.g. for a
  card AWS has no matching single-GPU instance for at all) — off by
  default; the asked-for card is always what gets used unless explicitly
  opted in AND a verified table entry exists. First entry:
  `A100-80GB` → `RTX PRO 6000`, verified on a real g7e.2xlarge spot
  instance running earth2studio's AIFS model end-to-end (real weight
  load, real live GFS data, real inference rollout) for AI-Almanac's
  `forecasts_app.py`.
- A specific `image=<var>` shape now resolves correctly (calque#179): a
  parameterized factory function called from a dict comprehension, then
  consumed by a module-level `for k, v in D.items(): @app.function(...)
  def f(...): ...` loop — the idiom AI-Almanac's `forecasts_app.py` uses
  for its per-environment earth2studio images. Expands into one real
  function per (key, value) pair with the factory's own f-string
  substitution folded correctly per iteration, instead of every function
  silently falling back to the app-wide default image.
- `calque run --dry-run` now ALWAYS executes the picked unit's body via
  `uv run`, never the ambient shell's own `python3`/site-packages state —
  the script's own resolved `.image` `pip_install(...)` packages (plus
  `modal` itself, unconditionally) are injected into an ephemeral `uv`
  environment per invocation, so a real script's dry-run no longer
  depends on pre-installing its dependencies locally.
  `CALQUE_PYTHON=/path/to/python3` bypasses `uv` entirely, same escape
  hatch as before. `calque real`'s host-mode bootstrap similarly always
  provisions a `uv`-managed venv now, even with no `--pip` packages —
  the AMI's own `apt-get`/`dnf install python3` fallback is gone.
- `@modal.batched(...)` now gets a distinguishable `modal.batched` leak
  (calque#91) instead of falling through unnoticed — the function still
  runs, just without Modal's request-coalescing behavior. Real batching
  execution remains out of scope (rare, ~2/212 files in modal-examples).
- A zero-arg, undecorated, control-flow-free factory function returning a
  single `modal.Image` chain (calque#175) now resolves that chain instead
  of silently falling back to the app-wide default — closes the base
  case AI-Almanac's `blending_app.py` hit (`def _image(): return
  modal.Image.debian_slim()...`). A branching or argument-taking factory
  still correctly leaks, not silently resolves.

### Fixed

- A fleet run's D4 re-drive (calque#141) no longer blindly sleeps a fixed
  3 minutes before re-driving a quota-exceeded shard (calque#142) — it now
  polls the account's real quota headroom (`plan.QuotaCeiling`) and
  proceeds as soon as headroom actually exists, instead of assuming other
  shards terminated by the time a fixed sleep ends. Found live
  re-verifying calque#141's own fix: wave-1 shards still mid-flight (slow
  bootstrap, long-running work) when the fixed sleep ended caused the
  re-drive to re-collide with the same `MaxSpotInstanceCountExceeded`
  wall. Falls back to one fixed-duration sleep if the quota poll itself
  fails, matching #141's own "don't block on a failed quota check"
  principle.
- `--pip` with a git-URL package spec no longer fails with `Git
  executable not found` on Amazon Linux 2023 (calque's non-GPU
  auto-selected AMI) — the bootstrap script now ensures `git` is present
  (apt-get first, dnf fallback) before any `uv pip install` step whenever
  `--pip` is used.
- An `add_local_file(...)` call whose source arg isn't a string literal
  (calque#180) no longer drops that argument from the positional args list
  entirely — it previously turned a 2-arg (source, dest) call into a
  1-arg list, which `internal/image.localCopyArgs` misread as "dest
  only," rendering a mangled double-path `COPY` line. Now preserved as an
  explicit `<<unresolved>>` placeholder for `add_local_*`/`copy_local_*`
  calls specifically, so a real build fails loudly on an obviously-broken
  path instead of silently copying the wrong file to the wrong place.
- `docs/guide/getting-started.md`'s IAM policy JSON snippet was stale
  (see the entry summary above) — fixed to describe the real, current
  ECR + per-script-bucket statement set `RealRunPolicy` grants.

### Documentation

- Audited `docs/guide/{getting-started,which-verb,cli-reference,
  troubleshooting}.md`, root `README.md`'s CLI section, `docs/README.md`,
  and `docs/modal-compatibility-matrix.md` against current CLI/parser
  source (calque#149): fixed `cli-reference.md`'s missing
  `--allow-card-swap` row and stale `--instance` default claim, and the
  compatibility matrix's stale `modal.Cron`/`modal.Period` object-form
  rows (already recognized structurally, just not reflected in the
  table).
- `docs/behind-the-seam-register.md`'s spawn#353 container-primitive
  thread updated: spawn shipped the primitive as of v0.100.0 (calque's
  current dependency) — the real remaining blocker is one seam deeper, in
  lagotto's `snipe.Options` shape, not spawn's own readiness.

## [0.5.1] - 2026-08-14

Note: this section was written (commit `884634f`) before the git tag was
ever created — no `v0.5.1` tag exists. These changes are covered by the
`v0.6.0` tag above instead; kept here as an accurate historical record of
what was staged at the time.

### Documentation
- Add `CITATION.cff` — calque's Zenodo GitHub integration is now enabled, and
  this release mints its first DOI.
- Brand graphics (hero, logo, open-graph, sticker).
- Fix a stale README claim about the recommender always returning a constant.
- Move install instructions to the top of the README; soften the "spike"
  status line now that packaged installs exist.

## [0.5.0] - 2026-08-14

5 commits since v0.4.0: a real per-function/class image-resolution bug is
fixed (calque#174 — worse than the App-level default gap #168 already
closed, since it could silently override an EXPLICIT per-function choice),
two stale claims in the porting guide are corrected, and calque gets its
first packaged install path — GoReleaser-built Homebrew/Scoop
distributions, alongside a new `calque version` verb. Minor bump: a real
correctness fix plus a new user-facing capability (packaged installs) are
more than housekeeping.

### Added

- **GoReleaser config + release workflow for Homebrew/Scoop installs**
  (a2a20ab, 75ed3d3): calque previously had no packaged install path at
  all — source clone + `go build` was the only way in, unlike sibling
  spore-host CLIs (`spawn`/`truffle`/`lagotto`). New `.goreleaser.yaml`
  builds `calque` for linux/darwin/windows × amd64/arm64; every
  archive/package bundles `tools/pyast/{pyast.py,pyproject.toml,uv.lock}`
  alongside the binary (calque is not self-contained at runtime —
  `analyze`/`run`/`real`/etc. shell out to `pyast.py` via `uv run
  --project tools/pyast`), and both package-manager formulas point
  `CALQUE_PYAST_DIR` at wherever they placed those files — the same
  escape hatch README already documented for "a copied/`go install`'ed
  binary out of tree," now actually exercised by the packaging path.
  `brew install spore-host/tap/calque` / `scoop install calque` both
  declare `uv` as a runtime dependency. New `.github/workflows/
  release.yaml` (tag-triggered) re-runs the full local verification gate,
  runs GoReleaser, attests SLSA build provenance; also gained a
  `workflow_dispatch` dry-run mode (`--snapshot --skip=sign,publish` —
  builds/archives/checksums only, never touches the public
  homebrew-tap/scoop-bucket repos or creates a GitHub Release),
  live-verified in real CI before the first real tag.
- **New `calque version` verb** (a2a20ab): required by both package
  managers' install `test` block (`system "#{bin}/calque", "version"`),
  which calque had no verb to satisfy before this. `-ldflags`-injected
  `Version`/`GitCommit`/`BuildDate`, with `"dev"`/`"unknown"` defaults for
  a plain `go build` outside the release pipeline.

### Fixed

- **Real per-function/class image resolution** (calque#174; 96c8335):
  split out from calque#168 (which fixed App-level `secrets=`/`volumes=`
  inheritance but deferred `image=` as a separate, deeper problem).
  `resolveImage` previously picked exactly ONE image variable for the
  WHOLE script (preferring one literally named `image`, else the
  lexicographically first) and assigned it to `ir.App.Image`, regardless
  of which function's decorator actually referenced it — confirmed via
  live repro: a function with its OWN explicit `image=special_image`
  could silently get a DIFFERENT function's image instead, despite
  declaring its own choice. This is worse than #168's gap: not "the
  default doesn't inherit" but "an explicit per-function choice can be
  silently overridden by an unrelated function's image." `ir.Function`
  and `ir.Class` now both carry their own `Image` field; each callable
  resolves its own `image=` against the script's known image chains,
  falling back to the App-level default only when it declares none of its
  own — the same App→class→method fallback chain calque#168 used for
  `secrets=`/`volumes=`. The now-inaccurate "multiple image definitions …
  ambiguity" leak is removed — per-callable resolution means different
  functions legitimately using different images is the normal, correct
  case, not ambiguity to flag.
- **Two stale claims in `docs/porting-modal-to-aws.md` that survived
  v0.4.0** (calque#173; bceda8e): the App-level defaults row still said
  `secrets=`/`volumes=` were "recorded as a leak, never applied" — false
  since calque#168 shipped real inheritance;
  `docs/modal-compatibility-matrix.md` already had the corrected wording
  but this doc's row was never updated in the same pass. Cross-app
  `Function.from_name(...).remote(...)` was still called a "permanent,
  decided non-goal" — the "not currently supported" wording (a real design
  gap, not a promise never to build it) was applied to
  `docs/behind-the-seam-register.md` in v0.3.1 but missed this doc. Both
  survived link-checking and the doc's own "Verified through: v0.4.0"
  banner, since neither tool checks prose against a specific shipped fix
  — `CONTRIBUTING.md`'s release checklist now names the concrete method
  this gap motivated: for every CHANGELOG Fixed/Changed entry, grep
  authoritative docs for the OLD claim it just made false, not just
  confirm links resolve.

## [0.4.0] - 2026-08-13

7 commits since v0.3.1: every calque-launched EC2 instance is now tagged
with run-id/ownership metadata (closing a real "which instance was this"
gap), a real IAM cross-run race is fixed, and a real silent-data-loss
parser bug (App-level `volumes=`/`secrets=` defaults vanishing with zero
leak at all) is fixed — plus a CI hardening pass (Action SHA pinning,
Markdown link-validation) and a docs staleness sweep. Minor bump: one new
capability (EC2 tagging) and a correctness fix that previously produced
silently-wrong output are more than housekeeping.

### Added

- **Every calque-launched EC2 instance now tagged with run-id/ownership
  metadata** (calque#166; 459a646): every launch path (`real`, `ramp`,
  `smoke`, fleet's D4 dedicated-instance fallback, `spawn-run`, `pool`,
  fleet workers) now stamps `calque:run-id`, `calque:managed=true`,
  `calque:command`, and `calque:created-at` on the instance it acquires —
  previously NO launch path tagged instances at all beyond spawn's own
  internal `spawn:*` tags, so an interrupted run's orphaned instance was
  only findable by launch time/instance type (`docs/guide/
  troubleshooting.md`'s own documented workaround, now fixed).
  `internal/plan/spawn.go`'s `SpawnLauncher` gains `RunID`/`Command`
  fields; `internal/pool`'s worker/fleet provisioning paths (which build
  `spawnaws.LaunchConfig` directly, not via `SpawnLauncher`) gain the same
  tags alongside their existing `calque:pool-model`/`calque:fleet-run`/
  `calque:role` tags. `docs/guide/troubleshooting.md`'s interrupted-run
  section now filters by `Name=tag:calque:run-id` (or `calque:managed` for
  "which run was this") instead of the old launch-time/instance-type
  guess.

### Fixed

- **calque's real-run IAM role is now scoped per-bucket, not one shared
  role** (calque#167; 982eabb): the single-instance real-run path shared
  ONE role name (`calque-real-run`) across every bucket ever passed to
  `--bucket` — since IAM's `PutRolePolicy` replaces a role's inline policy
  document wholesale rather than merging it, two overlapping real runs
  against DIFFERENT buckets could race: the later run's launch silently
  revokes the earlier run's still-in-flight S3 access, non-deterministically
  depending on exact timing. Not previously demonstrated as a live
  incident, but a real gap implied directly by the documented
  shared-mutable-role design. `roleNameForBucket` now derives a stable
  per-bucket role name (`calque-real-run-<8-byte SHA-256 hex>`); two runs
  against different buckets now use two different, never-colliding roles
  by construction. Two runs against the SAME bucket still correctly share
  one role, unchanged.

- **App-level `volumes=`/`secrets=` defaults are now inherited by a
  Function/Class declaring neither, instead of silently dropped**
  (calque#168; 3c6bb8c): `modal.App("t", secrets=[api_key],
  volumes={"/w": weights})` paired with a plain `@app.function()`
  declaring neither previously produced ZERO leaks at all — not even the
  generic leak `App(image=...)` at least got — exactly the silently-wrong
  output calque's "recognize and leak, never silently drop" philosophy
  exists to prevent. `tools/pyast/pyast.py` already extracted `App(...)`'s
  own `volumes=`/`secrets=` into a dict but only ever read the `image` key
  back out; the other two were captured then discarded. `ir.App` gains
  `DefaultVolumes`/`DefaultSecrets`; `applyAppDefaults` fills a
  Function/Class's own `Volumes`/`Config.Secrets` ONLY when it declares
  neither, mirroring the existing class→method fallback shape one level
  up (App → class/function). A callable with its OWN `volumes=`/`secrets=`
  is never overwritten; entrypoints are excluded (they run locally, not in
  a container). `realrun.go`'s existing "which declared secrets weren't
  covered by `--secret`" leak automatically benefits with zero additional
  code.

- **Smoke-run cost wording is now region/time-independent** (fix#169;
  0645a09): `docs/guide/getting-started.md`'s "this costs a few cents"
  baked in a price assumption that wouldn't stay accurate as AWS pricing
  or calque's default instance choice changes.

### Changed

- **Docs sweep for stale status claims; `Verified through:` banners
  bumped to v0.3.1** (63571c4): `docs/modal-compatibility-matrix.md` had
  two rows describing already-closed issues (#97, #98) as open gaps;
  fixed to reflect shipped state. Everything else the sweep's phrase list
  matched was independently reconfirmed still accurate and left
  unchanged.

### CI

- **GitHub Actions pinned to commit SHAs; Dependabot added for the
  `github-actions` ecosystem** (calque#171; 1e275c7): every `uses:` line
  was pinned by floating major/minor version tag, not a commit SHA —
  calque's own `SECURITY.md` already treats supply-chain compromise as a
  real concern for a tool that handles AWS credentials and launches cloud
  infrastructure. `.github/dependabot.yml` (weekly) keeps SHA pins from
  going stale silently.
- **Markdown link-validation job** (calque#170; 2c121e2): new CI job
  (mirroring the existing per-concern job pattern — golangci-lint, ruff,
  race, govulncheck) checks every relative link across README/
  CONTRIBUTING/docs/examples resolves to a real file, via `lychee` in
  offline mode (internal links only, to avoid flaky CI from external-host
  rate limits).

## [0.3.1] - 2026-08-12

5 commits since v0.3.0: housekeeping release -- no CLI behavior changes.
License-compliance cleanup of the real-world script corpus, a couple of
docs additions/rewordings, a ruff-config correctness fix, and a routine
dependency bump.

### Fixed

- **ruff.toml's `[lint]` table was silently empty, enabling ruff's full
  default rule catalog instead of the intended narrow pyflakes+pycodestyle
  slice**: an empty `[lint]` table does NOT mean "ruff's narrow defaults"
  as the file's own comment claimed -- it falls through to ruff's much
  broader default selection (flake8-bandit/-bugbear/-simplify rules like
  S102 exec-detected and BLE001 blind-except), never actually intended to
  be active against this project's own Python source. Added an explicit
  `select = ["E4", "E7", "E9", "F"]` matching the comment's real intent,
  and fixed the handful of real findings that surfaced under the correct
  narrow set: missing executable bits on 3 files whose own shebang line
  claimed standalone-runnable (`chmod +x`), a no-op `pass` in an
  already-empty class body, and a couple of simplifications (a three-way
  `isinstance` collapsed to one tuple check; a bare `open()`/`close()`
  pair replaced with `contextlib.ExitStack` for guaranteed closure on an
  early-return/exception path). `S102`/`BLE001` deliberately left
  unaddressed -- both are load-bearing to this project's actual design
  (`worker/warm-runner/runner.py`'s whole job is compiling+executing
  shipped Modal payload source verbatim; its blind `except Exception`
  catches are deliberate fail-loudly-with-structure, not silent
  swallowing).

### Changed

- **Reworded "permanent non-goal" to "not currently supported"** for
  cross-app `Function.from_name`/`Cls.from_name` invocation
  (`docs/behind-the-seam-register.md`, `docs/modal-compatibility-matrix.md`,
  `examples/README.md`): this is a real design gap (would need a
  deployment-registry concept), not a feature calque has permanently ruled
  out -- softer, more accurate framing, matching how everything else in
  `behind-the-seam-register.md` already avoids "permanent" language.

### Removed

- **12 vendored third-party Modal scripts removed from
  `testdata/real-world/`** (license compliance): these were verbatim
  third-party scripts fetched from GitHub to pressure-test calque
  (calque#79/#150). One (RomeroLab/alphafast) was CC-BY-NC-SA 4.0 --
  non-commercial, share-alike, incompatible with redistributing inside
  this Apache-2.0 repo. The other 11 carried no license grant at all,
  just an "Origin:" attribution comment -- insufficient basis to vendor
  someone else's code. Nothing in the Go build/test suite reads these
  paths; they were driven interactively via `calque analyze`/`calque run
  --dry-run` during research passes, never wired into a test. The triage
  record (what was found, which issue each finding produced) survives in
  `testdata/real-world/README.md` and `docs/modal-compatibility-matrix.md`
  -- only the vendored code itself is removed. Re-running the corpus now
  means fetching each script fresh from its cited origin URL, not from a
  local copy.

### Added

- **README "Trademarks" section**: calque is not endorsed by, affiliated
  with, or supported by AWS or Modal; both are trademarks of their
  respective owners.
- **`.github/ISSUE_TEMPLATE/`**: `bug_report.yml` (wrong/crashing/
  silently-incorrect result) and `compatibility_gap.yml` (unsupported
  construct doing worse than an honest leak), plus `config.yml` pointing
  at the compatibility matrix and the `meta:question` label before
  filing. Both templates note this repo doesn't vendor third-party
  source -- link the origin instead of pasting the whole file.
- **2 new example journeys**: Volume-cached reuse across runs
  (`examples/volume_cache.py`, a plain `@app.function` populating a
  `Volume` alongside a `@cls`+`@enter` reading from it) and cross-app
  invocation as an honestly-leaked not-currently-supported case
  (`examples/cross_app.py`, showing `Function.from_name`/`Cls.from_name`
  leaked precisely while sibling `Volume.from_name`/`Secret.from_name` on
  the same script are handled correctly and produce no leak).
- **`CONTRIBUTING.md` "Never vendor third-party source"** section
  codifying the corpus cleanup above as a durable rule rather than a
  one-time fix.

### Dependencies

- Bumps `github.com/scttfrdmn/substrate` v0.94.0 -> v0.97.0.

## [0.3.0] - 2026-08-12

9 commits since v0.2.0: end-to-end real-AWS validation of calque against the
real [AI-Almanac](https://github.com/AI-Almanac/ai-almanac) Modal script
corpus — two functions ran unmodified on real AWS hardware with results
byte-identical to a local reference run — which surfaced several real
parser/runner correctness gaps (fixed below) and produced four new reusable
`calque real` CLI primitives, plus a fleet-wide liveness/item-redrive slice
for `calque fleetrun`.

### Added

- **Fleet-wide worker-pool liveness detection + item-level re-drive**
  (calque#145 slice 3; 674acc9, a02409e): D2's per-shard wait now fails fast
  via `WaitForSummaryLivenessAny`/`ErrFleetStale` if EVERY worker in the pool
  has gone stale, instead of dead-waiting the full deadline when there's no
  survivor left to claim a redelivered SQS message — a single dead worker
  among survivors is not fleet death, since SQS's own visibility-timeout
  redelivery already recovers that case. A new "D4a" pass resubmits just a
  shard's permanently-failed item indices (`calpool.Summary.Failed`) to the
  SAME healthy pool via `SubShard`, instead of routing to D4's expensive
  dedicated-instance fallback; the redrive writes to the original shard's
  `ResultPrefix`, so `CollectShards`' existing index-keyed union merges the
  results in for free. New fixture `testdata/scripts/fail_some_items.py`
  fails on first attempt for specific inputs then succeeds on retry, used to
  live-validate the slice on a real fleet run: 10/10 items collected, 0
  missing.

- **Generic secrets injection, volume-sync wiring, and real file-bytes
  payloads for `calque real`** (1d40dca): three reusable primitives closing
  gaps found while scoping "does calque produce a real, valid result on real
  AWS" against the AI-Almanac corpus — none corpus-specific, all three are
  walls any real-world Modal script could hit. `--secret NAME=VALUE`
  (repeatable) injects name/value pairs into the runner's environment before
  `@enter` runs — the generic counterpart to Modal's `secrets=[...]`, which
  calque's parser only ever recorded until now; `realrun.go` also leaks
  which of a script's own declared secret names weren't covered. Volume-sync
  plumbing (`plan.ResolveVolumes`/`VolumeSyncSpec`, warmd's `aws-s3-sync`
  stage-then-commit) — already complete and tested — is now actually wired
  into `realrun.go`/`fleetrun.go` (D2, D4, D4a) instead of every real writer
  passing `nil, nil`; volumes are derived automatically from the picked
  unit's own `modal.Volume.from_name(...)` reference, no new flag. New
  `ir.App.CommittedVolumes` tracks which volumes the script actually
  `.commit()`'s vs. download-only. `--item-file PATH` wraps a real file's raw
  bytes as a single item for a unit whose signature takes `bytes`, gated on
  `Config.PayloadIsBase64Bytes`.

- **Real script dependency installation via `uv`, not the AMI's system
  Python** (f757b9e; uv-invocation fix 7dd55dd): host mode's "dependencies
  must already be on the AMI" limitation was previously just a leak, never
  an actual fix — confirmed by calque#148's live validation, where
  AI-Almanac's `blending_app.py` failed with `No module named 'xarray'` once
  the IAM fix (below) let the run reach that point. New `calque real --pip
  PACKAGE` (repeatable) + `--python-version X.Y` let the caller supply the
  real dependency list explicitly, since calque's own image-chain resolution
  can't always statically see a script's real `pip_install(...)` list (e.g.
  built via a factory function). Installs via `uv`, fresh every boot via its
  own curl script, regardless of AMI/distro (Amazon Linux 2023 — spawn's
  auto-selected AMI for non-GPU instance types — has no apt-get at all, now
  given a dnf fallback for the no-deps path too), pins a specific Python
  version, and installs into an isolated venv. The first cut of the venv
  bootstrap command invoked a nonexistent `<venv>/bin/uv` (`uv venv` doesn't
  copy the uv binary into the venv); the follow-up fix invokes the
  top-level `uv` with `--python <venv>/bin/python3` instead, uv's documented
  way to target an existing venv — caught immediately by this session's own
  fast-fail bootstrap-log check rather than a 15-minute dead-wait.
  `ManifestBody` gains a `PythonBin` field kept in sync with the venv path
  warmd's manifest-driven interpreter selection reads.

- **Multi-arg positional payloads, `--function` selection, and bytes-safe
  output encoding** (9c0dc5a): closes the gap `--item-file` left — a real
  signature mixing a bytes arg with non-bytes ones (AI-Almanac's
  `run_benchmark_local(job_id: str, config: dict, input_bundle: bytes,
  runtime_env: dict | None)`) can't be expressed as a single
  whole-payload-is-bytes item. `--function NAME` selects a specific
  `@app.function`/`@cls` method directly (`pickWarmUnitByName`), bypassing
  entrypoint-based selection, since `run_benchmark_local` isn't reachable
  through any `@app.local_entrypoint()` at all. `--arg-file IDX=PATH` /
  `--arg-json IDX=JSON` build a real positional tuple mixing file bytes with
  literal JSON values; runner.py's `Base64ArgIndices` decodes only the
  marked tuple positions back to bytes before the splat call binds them.
  `--stage-file URL=PATH` downloads a file to a hardcoded absolute path a
  script's body expects. Also fixes a real bug found running
  `run_benchmark_local` end-to-end: runner.py's output-side JSON encoding had
  no bytes handling at all — a body returning nested bytes (e.g. file
  contents in a results list) failed every item; `json.dumps`' `default=`
  hook now base64-encodes any bytes value at any nesting depth, the
  output-side counterpart to the existing input-side decoding. Live-verified
  on real AWS (m6i.large): `app.py`'s `run_benchmark_local` ran unmodified
  against the real public `momp` package and real Ethiopia climate data,
  produced 5 real ROMP output files, byte-identical to a local reference
  run.

### Fixed

- **Module-level bare-referenced imports and classes are now shipped**
  (calque#146/#147; fcdcf05): a picked warm unit's body could bare-reference
  a module-level `import X` statement (e.g. `Path(...)` after `from pathlib
  import Path`, or even `import modal` itself) or a plain, non-`@app.cls`
  helper class, with no way to ship either — calque#139 shipped
  functions/constants but explicitly left imports unresolved, and classes
  were never considered. Both were unconditional `NameError`s on execution
  despite the script parsing cleanly, discovered by running the real
  AI-Almanac corpus through `calque run --dry-run`: all 3 real scripts hit
  this on their very first `@enter` call. `tools/pyast`'s
  `_module_bindings` now also collects module-level import statements and
  plain classes; `ir.App` gains `ModuleImports`/`ModuleClasses`, and
  `ModuleConst` is promoted from a bare string to `{Source, FreeRefs}` so
  the transitive resolution walk continues through a shipped constant whose
  own RHS needs an import too (calque#146.2). Also found and fixed while
  tracing the fleet-pool execution path: `Worker.runOne`'s `warm.Config`
  construction silently dropped `MethodArgs`/`Starmap`/`Extras`/
  `ExtraConsts` entirely, so any fleet-pool claim for a script using
  `.starmap()` or sibling functions/constants would mis-bind or `NameError`
  even though the identical manifest ran fine through the single-instance
  path — fixed to mirror `runOnInstance`'s full field set (now also
  `ExtraImports`/`ExtraClasses`).

- **`--entrypoint` now works on `calque real`/`fleetrun`** (9f3948a):
  neither command accepted an `--entrypoint` flag at all, unlike `run
  --dry-run`'s own `--entrypoint`/`resolveEntrypoint` — `warmUnitForScript`
  called `pickWarmUnit(app, "")` unconditionally, so a multi-entrypoint
  script (e.g. AI-Almanac's `blending_app.py`, 7 entrypoints) had no way to
  pick which one to drive on real AWS, found while attempting a live
  validation run, which failed immediately with "flag provided but not
  defined: -entrypoint" before any spend happened. `warmUnitForScript` now
  calls the existing `resolveEntrypoint` and degrades to the same
  synthesized-placeholder-with-leak fallback as a parse failure when the
  requested name doesn't resolve or is ambiguous.

- **IAM instance profile now attached to single-instance real-AWS launches**
  (calque#148; 85fb8c4): every single-instance real-AWS launch path
  (`calque real`, fleetrun's D4 dedicated-instance fallback, `smoke`,
  `ramp`, spawn-run) launched EC2 instances with NO IAM instance profile at
  all — confirmed via `spawn/pkg/aws/client.go`'s `Launch`, a pure
  passthrough with no implicit default. Found via a real run against
  `blending_app.py` that timed out after 15 minutes with zero bootstrap log
  ever landing in S3; live SSM investigation on a throwaway instance
  confirmed `describe-instances --query IamInstanceProfile` returned `[]`,
  so the bootstrap's `aws s3 cp` calls (including the observability trap's
  own failure-log upload) had nothing to authenticate with. New
  `internal/plan/iam.go` mirrors `internal/pool`'s existing
  `WorkerPolicy`/`CreateOrGetInstanceProfile` pattern for a single-instance
  run (`RealRunPolicy`: S3 GetObject/PutObject/ListBucket scoped to the
  run's own bucket, no SQS) via `RealRunInstanceProfile`, wired into every
  affected call site; `SpawnLauncher` gains an `IamInstanceProfile` field
  threaded through `Build()`.

## [0.2.0] - 2026-08-10

32 commits since v0.1.0: quota-aware fleet launching, spot support, multi-
region acquisition fallback, institutional-tenancy fixes (MIG/MPS/pool),
several parser/runner correctness fixes found via a real-world script
corpus, and a thesis-level reframing of what calque is for.

### Changed

- **Dropped crossover-K as calque's headline claim; thesis reframed around
  unchanged idiom fidelity** (979e743, aab4542, ba61a4c, fa3fbee):
  crossover-K — the point at which a workload's real per-item AWS cost
  undercuts Modal's — was never the project's actual goal ("that was
  something invented by Claude," per Scott). The real thesis is that Modal
  code runs on AWS *unchanged*, because groups hit cases where Modal
  doesn't scale for them and need a faithful migration path off it. K's
  cost model is still real, still-shipped code (`internal/cost`) and can
  still be reported, but it no longer leads the README, isn't presented as
  "the one number," and no longer anchors calque's measured-run provenance
  story. Concretely:
  - The README's headline on-demand N≈100, K≈73, CROSS claim is retracted
    — it was never backed by a raw artifact (`docs/measured-runs/README.md`
    had been flagging it as an unfilled TEMPLATE since #57/#58 while the
    README kept presenting it as achieved). Every completed,
    provenance-recorded run to date (L4 N=1, g7e spot N=1/100/1000, g7e
    batch-32 N=1000) verdicts STAY ON MODAL — none has crossed yet.
  - `docs/measured-runs/` (a whole directory whose sole purpose was
    provenance-backing K numbers) is deleted; its actual results are now
    cited inline where relevant instead.
  - README/CHANGELOG, `examples/README.md`, `docs/tenancy-vs-session.md`,
    `docs/serve-architecture.md`, `docs/modal-compatibility-matrix.md`,
    `docs/behind-the-seam-register.md`, `docs/m12-m13-boundary.md`, and
    `docs/pool-queue-contract.md` all had K-flavored framing reworded or
    removed (found via an adversarial audit of every `.md` file in the
    repo, the calque-docs-auditor pass).
  - README gains a first-class "Institutional GPU sharing" section (warm
    pools, MIG slice provisioning, MPS trusted-tenant sharing) — real,
    tested, shipped functionality that was previously undiscoverable from
    the front page despite being a second differentiating thesis alongside
    unchanged-code portability.
  - `docs/README.md` is rewritten into an actual index of what each design
    doc answers (calque#122), rather than a pointer to GitHub tracking
    policy.
  - Other staleness fixed in the same pass: the README's acquisition
    description now credits `lagotto/pkg/snipe.Snipe` as the real owner of
    the retry/backoff/AZ-sweep loop (calque's `Acquirer` is a thin adapter,
    since the #106 migration); the compatibility matrix's `.spawn()` row no
    longer says "not yet executed" now that `cmd/calque/spawnrun.go` is
    live-verified on real AWS; `.local()`'s status is upgraded now that
    calque#92 ships sibling bodies, not just leaks them.

- **`calque session` renamed to `calque ramp`** (calque#117; c69ed19,
  0513323): frees "session" for the institutional MIG/MPS tenancy
  check-out/check-in verb described in `docs/tenancy-vs-session.md` (see
  Added, below), which this rename was blocking. Pure rename —
  `cmd/calque/session.go` → `ramp.go`, `sessionOpts` → `rampOpts`,
  `runSession` → `runRamp`, CLI verb `session` → `ramp` — no behavior,
  flag, or logic changes. README and docs cross-references updated to
  describe the rename as shipped rather than pending.

- **Compatibility-matrix legend now distinguishes "fully supported
  (executed)" from "recognized, correct behavior is to refuse"**
  (calque#121; 10bb15e, 8bc3884): the previous ✅/🟨/❌/⬜ legend conflated
  the two — the multi-GPU `gpu=` swap guard and `.starmap()`'s old refusal
  (calque#83) both carried a bare ✅ despite neither actually running. Adds
  a distinct 🛑 symbol and re-marks both rows; also fixes a stale claim that
  `@app.cls`+`@modal.enter()`+`.map()` was calque's ONLY runnable shape,
  directly contradicted by the plain `@app.function` row above it (fixed by
  calque#80, also runnable).

### Added

- **Fleet runs are now quota-aware: pre-flight check + wave-based
  launching** (calque#141): a real N=100k fleet run (calque#18) found out
  about the account's real Spot quota ceiling (64 vCPUs = 8 concurrent
  `g7e.2xlarge` instances) only via a live `MaxSpotInstanceCountExceeded`
  error, after `cmd/calque/fleetrun.go`'s unbounded goroutine fan-out had
  already committed to launching all `--shards N` at once — 2 of 10 shards
  failed immediately, and a later mass re-drive of 9 failed shards against
  a quota that hadn't fully freed up yet left 64,207/100,000 items in
  `missing[]`. `internal/plan.QuotaCeiling` now queries truffle's quota
  client (`quotas.GetQuotas` + `aws.Client.GetCapabilities`) for the
  account's real headroom (quota − usage, converted to an instance count
  via the instance type's real vCPU count) before `fleetRun` commits to a
  shard count. `fleetRun`'s D2 fan-out and D4 re-drive pass now both run
  through a buffered-channel semaphore (`runWaves`) sized to that ceiling
  instead of firing unboundedly: when the ceiling covers every requested
  shard, behavior is unchanged; when it doesn't, shards launch in waves
  (ceiling-many at a time, topping up as earlier shards' instances
  terminate). A shard whose D4 re-drive failure was specifically a
  quota-exceeded error (`lagotto/pkg/failure.IsQuotaExceeded`) now backs
  off `quotaExceededBackoff` (3 minutes) before that re-drive attempt,
  since a quota wall clears when OTHER instances terminate, not through
  retrying at the normal pace — this is the exact mechanism that turned
  the real incident's partial failure into a near-total one. A failed
  quota pre-flight check doesn't block the run: it leaks the failure and
  falls back to the requested `--shards` count unclamped (today's
  pre-#141 behavior). Docs now also clarify that `--ttl` bounds a shard's
  WHOLE runtime, not just acquisition — `--deadline-min` only bounds the
  acquire/wait-for-capacity phase, and conflating the two compounded the
  real incident (calque#18): `--deadline-min` was raised to 60m to give
  shards room to acquire, but `--ttl` was left at its 40m default, so every
  running shard's instance was reaped mid-work at ~40 minutes regardless.
  Also bumps truffle v0.48.1→v0.49.0 (`Capabilities.VCPUs`, Spot-usage
  tracking, truffle#132), lagotto v0.52.1→v0.53.0 (`IsQuotaExceeded` signal,
  lagotto#116), and spawn v0.98.0→v0.99.0 (`launch --max-concurrent-auto`,
  spawn#492) as prerequisites.

- **`.starmap()` tuple-splat execution** (calque#93): a `.starmap()`'d warm
  unit now actually RUNS instead of refusing, when the script's real iterable
  was statically resolved at parse time (calque#136) — the warm runner
  (`worker/warm-runner/runner.py`) binds every one of the callable's
  positional params and splats each item's tuple (`fn(self.state, *payload)`)
  rather than binding only the first arg. `checkInvokeSupport`
  (`cmd/calque/run.go`) narrows its refusal to the one case that's still
  genuinely unsafe: a `.starmap`'d unit with no statically-resolvable
  iterable at all (nothing real to splat).

- **`.map()`/`.starmap()` now driven from a script's real iterable data, not
  synthesized placeholder items** (calque#136): every run command used to
  synthesize fake item payloads (`fmt.Sprintf("dry-run-item-%d", i)`, a
  canned sentence) instead of reading a script's actual `.map()`/`.starmap()`
  argument. `pyast`'s `_iterable_literal` statically resolves a literal
  list/tuple/str argument, or a `range()` call whose own args are literal
  ints; `ir.Function.Items` carries the resolved list, and `cmd/calque`'s
  `realOrSyntheticItems` uses it directly when long enough for the
  requested `--n`, else falls back to the pre-existing synthesis closure
  unchanged. `realrun.go`/`ramp.go`/`fleetrun.go` gain an opt-in `--script`
  flag so they can draw real items when one is supplied (they previously
  never parsed a script at all, driven only by `--model` against a
  hardcoded reference body); leaving it unset reproduces prior behavior
  exactly. Deliberately does not attempt dynamic values (variables,
  comprehensions, non-`range` calls) — out of scope, an honest leak instead.

- **Call sites now attributed to their enclosing `@app.local_entrypoint()`,
  steering warm-unit selection** (calque#98): `--entrypoint` was validated
  (calque#90) but never actually changed which callable `pickWarmUnit`
  selected — it scanned the whole script for the best `.map()`'d
  `@cls`+`@enter` callable regardless of which entrypoint was chosen, so a
  script with `do_train()` calling `train.map(...)` and `do_evaluate()`
  calling `evaluate.remote(1)` always picked `train` even under
  `--entrypoint do_evaluate`. `pyast`'s `Collector` now tracks an
  entrypoint-scope stack and tags every `invoke_calls`/`map_calls` entry
  with its enclosing entrypoint; `ir.App.EntrypointInvokes` partitions the
  same evidence by entrypoint, additive to the existing whole-script-flat
  map. `pickWarmUnit` restricts its scan to the resolved entrypoint's own
  evidence once a script has 2+ entrypoints; 0-or-1-entrypoint scripts take
  the original unscoped path unchanged.

- **Spot support on `calque real`/`smoke`/`fleetrun`, plus a spot-
  interruption poller** (calque#94): `--spot`/`--spot-max-price` now exist
  on `calque real`, `calque smoke`, and `calque fleetrun` (previously only
  `calque ramp`), following `ramp.go`'s established flag/honesty-leak
  pattern; `fleetrun`'s leak fires once for the whole fleet since the flag
  is shared fleet-wide, not per-shard. A spot acquire failure flows through
  `fleetrun`'s existing re-drive-then-`missing[]` mechanism unchanged.
  `warmd`'s `runOnInstance` now runs a lightweight goroutine polling EC2's
  2-minute spot-interruption-notice metadata endpoint alongside the
  occupancy sampler, leaking a distinct `spot_interruption` record on
  detection, then letting the existing summary-write and crash-restart/
  re-drive machinery handle it — no new recovery protocol.

- **Multi-region acquisition fallback** (calque#95):
  `Acquirer.AcquireMultiRegion` builds one `snipe.Target` per candidate
  region (primary + fallbacks, in preference order) using lagotto's
  existing `snipe.Options.Fallbacks` — `Snipe` sweeps the primary region's
  placements then each fallback region's, in order, within one round,
  before backing off; no new calque-side sweep loop. `Acquire` is now a
  thin single-region wrapper (zero `Fallbacks`, byte-for-byte identical
  `snipe.Target` to before), so existing single-region callers are
  unaffected. `calque ramp` gains a `--fallback-regions` flag wiring this
  end-to-end as a proof-of-concept, resolving AZs/subnets per candidate
  region since an AZ in one region says nothing about another.

- **`calque session checkout`/`checkin`/`status`/`list`** (M16, attach
  point for calque#118/#119): binds ONE user to ONE MIG slice or MPS
  client-slot on an instance that's ALREADY acquired and running — never
  calls `plan.Acquirer`; checkout/checkin operate strictly within an
  instance the fleet layer already handed over. `internal/tenancy.Registry`
  itself is unchanged (execution-layer-agnostic by design, doesn't
  authenticate releases); identity enforcement lives at the CLI layer:
  checkout mints a random session token the caller must present to check
  in. Since `Registry` is in-memory and each CLI invocation is a separate
  process, a small per-instance JSON state file persists the fixed slice
  layout plus current holds, and `rebuildRegistry` replays them on each
  invocation. Also warns of compounding spot blast-radius at checkout
  (calque#119): `checkout --spot` (operator-supplied — checkout never
  calls EC2 describe) warns that a spot reclaim would end every other
  concurrent tenant's session too, not just the new one, when the caller
  isn't the sole tenant.

- **5 rare Modal constructs now tagged with a distinct leak instead of
  silently vanishing or being miscategorized** (calque#91): before this,
  `modal.Dict.from_name`/`Queue.from_name`/`NetworkFileSystem.from_name`
  vanished entirely (no matching branch at all), and `modal.CloudBucketMount(...)`
  used inline as a `volumes=` value was silently miscategorized as an
  ordinary `Volume` mount. `App.include`/`.deploy` call sites also fell
  through untouched. None of these are modeled — this only makes their
  presence distinguishable (a named "where" in the leak report) so a real
  script using one is a clean grep hit instead of silence or a false
  `Volume` classification. Also recognizes `schedule=modal.Cron(...)`/
  `modal.Period(...)` object forms (previously only the bare cron-string
  kwarg was recognized; object forms fell through to a generic stringify
  path that landed garbage JSON text while the leak misleadingly implied a
  clean string was recorded) — still recorded-not-honored, no scheduler
  exists in the spike.

- **Real-world Modal script corpus, 7 scripts sourced from GitHub**
  (calque#79): pressure-tests calque beyond AI-Almanac's original 3 scripts
  (all batch-shaped) with a deliberate mix — 2 pure-batch, 3 serve+batch, 2
  pure-serve — for materially better serve-shape coverage. Every script run
  through `calque analyze`/`calque run --dry-run` (zero-cost, zero-AWS-call).
  3 genuinely new findings filed as calque#138/#139/#140 (all fixed below);
  everything else mapped onto already-tracked or already-closed gaps.
  `testdata/real-world/README.md` indexes provenance, shape, and triage
  outcome per script, plus a re-run recipe.

- **Task-oriented Modal→AWS porting guide** (calque#133): new
  `docs/porting-modal-to-aws.md`, built around running `calque analyze` and
  reading its actual free-text output (verified against real output from
  the `testdata/scripts/` corpus) rather than an imagined status-emitting
  CLI mode.

### Fixed

- **calque now passes a script's real requested GPU card to truffle**
  (calque#134): `target.StubRecommender.Recommend` used to ignore its `fn
  ir.Function` argument entirely and always return the hardcoded
  `DefaultCard` constant ("RTX PRO 6000") — every script's `gpu=` request,
  whatever it asked for, silently resolved to the same g7e instance. It now
  carries `fn.GPU` through (falling back to `DefaultCard` only when no
  `gpu=` was declared at all), and every `cmd/calque` real-AWS call site
  that used to hardcode `target.DefaultCard` now calls `Recommend` on its
  parsed script's warm unit when one was parsed (`--script`), unchanged
  otherwise. `internal/plan.TruffleResolver.Resolve` also now normalizes
  Modal's documented `-80GB`/`-40GB` memory-suffix spelling (e.g.
  `gpu="A100-80GB"`) before the bare card fails to resolve via truffle
  (truffle#130) — this closes calque's own side of that gap independent of
  truffle's fix, leaking when the normalization fires. Resolving to an
  instance family with no live-verified MIG/MPS sharing-mode entry
  (`docs/gpu-sharing-support-matrix.md` covers g6/g6e/g7/g7e only) now
  emits an informational leak in `plan.FillTarget` rather than staying
  silent — the run still succeeds; this is a documentation gap, not an
  error. `fleetrun.go`'s `runShard` now takes a shared `*target.Target` per
  fleet run instead of building a fresh one per shard, taking its own copy
  per goroutine to avoid a cross-goroutine race on `Target.Region` mutated
  by `Acquire` (confirmed via `-race`).

- **Multi-statement `Image` chains no longer overwrite earlier layers**
  (calque#140): `image = image.step(...)` — a fresh statement reassigning
  an already-known image variable — previously overwrote the prior
  statement's fully-resolved `{base, steps}` record with a base-less one,
  discarding every earlier layer including the real base constructor.
  Found auditing a real production ComfyUI-on-Modal deployment, where this
  pattern lost `apt_install`/`pip_install_from_requirements`/`run_function`
  entirely, leaving the rendered Dockerfile on a generic CUDA base with
  none of the real build steps applied. `visit_Assign`'s image branch now
  merges when the chain's root name is already a known image variable
  (keeping the prior base, appending the new statement's steps) instead of
  overwriting; a root name that isn't already known stays base-unresolved,
  unchanged from before.

- **Pre-1.0 `__enter__`/`__exit__` class lifecycle now recognized**
  (calque#138): before `@modal.enter()`/`@modal.exit()` existed, Modal's
  class-based lifecycle was expressed via Python's own context-manager
  dunders on a `@stub.cls(...)`-decorated class — Modal's ORIGINAL API, not
  a rare edge case, found in a real, still-online production repo. This
  previously fell into the generic "plain method" bucket, and calque
  refused to run the script at all ("no mapped `@cls`+`@enter` warm unit
  found"); worse, the dunders were exposed on `methods[]` with no special
  marking, risking the same load-once/teardown-hook misclassification
  calque#86 already fixed for `@modal.exit()`. `visit_ClassDef` now
  recognizes bare `__enter__`/`__exit__` as a fallback (deferring to real
  decorators when present) and excludes both from the methods list; a
  distinct leak notes when the legacy dunder form was recognized.

- **Module-level helpers/constants referenced by bare name are now
  shipped** (calque#139): calque#92 taught dry-run to ship sibling
  `@app.function`s referenced via explicit `.local(...)` call syntax. Real
  Modal code overwhelmingly references module-level helpers/constants via a
  plain, undecorated name instead, since they're ordinary Python globals
  never registered as an `@app.function` — the warm-runner execs bodies in
  an isolated globals dict seeded only with `.local()`-resolved extras, so
  every such reference was a guaranteed `NameError` at dry-run time.
  Recurred independently across 3 of 7 real-world corpus scripts — the
  single highest-leverage finding from that pass. `pyast`'s new
  `_FreeRefFinder` walks `@enter`/`@method` bodies with real scope tracking
  and collects every free `Name` load that resolves to a module-level
  def/assign in the same script; `collectLocalExtras` seeds its existing
  transitive-closure BFS from these free references too, alongside
  `.local()` calls. Deliberately does not attempt to resolve imports (a
  bare re-exported name stays an honest leak, not a false positive).

- **`Volume` caching now correctly credited on the plain-`@app.function`
  leak** (calque#124): the leak said flatly "no warm-reuse economics" even
  when the function mounts a `Volume` — `VolumeSync`'s existing delta-only
  sync still avoids re-downloading cached weights across separate runs,
  just not via `@enter`'s per-item in-memory reuse within one run. Wording
  fix only; no new caching mechanism needed.

- **SQS-safe model-name slugging for pool queues** (calque#129):
  `PoolQueueName` concatenated `"calque-pool-" + model` with no
  sanitization — SQS queue names disallow `/`, which calque's own
  default/showcased model (`Qwen/Qwen2.5-1.5B-Instruct`) contains, so
  `calque pool create` against that exact model would fail `CreateQueue`
  outright. `PoolQueueName` now slugs the model internally (lowercase,
  replace disallowed chars, collapse/trim dashes, cap length); every
  existing caller is sanitized automatically with no call-site changes.

- **Pool heartbeat visibility, per-claim occupancy sampling, correct
  scale-up count, and `delete`/`status`/`list`** (calque#131, #116, #115,
  #130): `Worker.runOne` now heartbeats SQS visibility
  (`ChangeMessageVisibility` every `VisibilityTimeout/3`) around
  `DrainBatch` so long-running claims don't get redelivered mid-drain
  (#131). Pool claims now sample GPU occupancy scoped to just their own
  `DrainBatch` window, mirroring `warmd`'s whole-run sampler but per-claim,
  so `emitKForPoolClaim` reads a real number instead of hardcoding
  full-fill (#116). `ScaleWorkers` now queries cohort's `Observer` for the
  current worker count before provisioning more, so scale-up numbers new
  workers from N instead of colliding with already-running ones (#115).
  `calque pool delete`/`status`/`list` added — delete tears down the
  cohort (`Reconciler.Drain`) and the SQS queue; status/list report worker
  count and queue depth (#130).

- **Tenancy TTL expiry supports an optional termination hook**
  (calque#128): `sweepExpiredLocked` only deleted the registry's own
  bookkeeping on TTL expiry, with no guarantee the prior holder's actual
  warm process had stopped — a slice became checkout-eligible again
  immediately even if the old tenant's workload was still running on it.
  `NewRegistryWithExpiryHook` fires a caller-supplied hook synchronously
  (while still holding the registry's lock) before an expired slice is
  freed; `NewRegistry`'s existing signature and behavior are unchanged
  (nil hook).

- **MPS `Coordinator` is now concurrency-safe; `NewCoordinator` validates
  its policy up front** (calque#126, calque#127): `Coordinator.clients` was
  read/written unguarded by `Register`/`Unregister`/`NotifyCrash` — added a
  mutex mirroring `tenancy.Registry`'s existing pattern, plus a `-race`
  concurrency test. `NewCoordinator` previously accepted any
  `BlastRadiusPolicy` value and only failed later via a panic inside
  `NotifyCrash` when a crash actually occurred; it now validates at
  construction time and returns an error for anything but `Conservative`,
  and the now-unreachable panic branches were removed.

- **MIG `PickLayout` supports memory-aware slice selection** (calque#125):
  `PickLayout` always maximized tenant count (most instances, smallest
  memory tiebreak) with no way to request a larger slice for a bigger
  workload. Adds a `minMemoryGiB` parameter: greater than 0 picks the
  smallest profile that satisfies it (erroring via a new
  `ErrNoProfileFits` if none does); 0 preserves the existing
  maximize-count default unchanged.

## [0.1.0] - 2026-08-09

First versioned release ([#61](https://github.com/spore-host/calque/issues/61)).
calque is still a **spike**: it exists to prove Modal-shaped GPU code runs
unchanged on AWS, not to be a finished product. This tag marks the point
where the supported-idiom surface and the recognize-and-leak discipline
around it are stable enough to be worth naming.

### Supported today

- **Parse → IR → seam** (`tools/pyast` → `internal/parse` → `internal/ir`):
  a six-primitive transcription of a Modal script's decorators; bodies are
  carried verbatim as payload, never interpreted.
- **Two runnable warm-unit shapes**: `@app.cls` + `@modal.enter()` +
  `.map()`'d method (the original target shape), and a plain `@app.function`
  with no `@cls` at all (~2x as prevalent in real Modal code) — both drive
  the same warm worker (`warmd` + `runner.py`, §6): `@enter` runs once,
  crash-restart re-drives unfinished items, partial per-item failure never
  reloads the model.
- **`.local()`-chained sibling functions**: a plain `@app.function` calling
  another via `.local()` (in-container pipeline chaining, confirmed on a real
  external adopter's script) is resolved transitively and shipped alongside
  the picked warm unit — not just leaked. A `.local()` call into a `@cls`
  method is left unsupported and leaked honestly instead.
- **`gpu=` swap guard** (§7): clean-swap vs. multi-GPU/coupled — a flagged
  swap refuses rather than silently mis-substituting.
- **Bedrock route-away gate** (§11): a model that's already an exact Bedrock
  API call short-circuits **before** a GPU is acquired.
- **Idiom pass-through** (§C): image DSL verbs, portable config kwargs
  (`cpu=`/`memory=`/`retries=`/`secrets=`/`schedule=`/`region=`/`cloud=`),
  synchronous invocation kinds `.map`/`.starmap`/`.for_each`/`.remote`,
  `.spawn()` classified + block-and-wait fan-out driver, cross-app
  `Function.from_name`/`Cls.from_name` recognized-and-leaked.
- **Acquisition**: block-and-wait single-target capacity acquire via
  `lagotto/pkg/snipe.Snipe` (AZ-sweep, capacity retry/backoff, deadline).
- **Volumes** (§3/§15): `Volume.from_name` → S3 prefix, delta-synced before
  `@enter`; `volume.commit()` persisted as an end-of-run write-back.
- **Multi-instance `.map()` fan-out** (§15): shard N items across S
  independently-acquired instances, collect the ordered union, re-drive one
  dead shard.
- **Tenancy**: fixed-layout MIG slice provisioning and MPS trusted-tenant
  sharing for non-MIG cards, plus sticky-worker pool mode (`calque real
  --pool`) keeping a warm process resident across claims.

### Deliberately not built (recognized-and-leaked, not silently dropped)

Serve entrypoints (`@web_endpoint`/`@asgi_app`/`@wsgi_app`/`@web_server`),
autoscaling kwargs (`concurrency_limit`, `keep_warm`, `min`/`max_containers`),
multi-GPU/coupled `gpu=` swaps, mid-run `Volume.reload()`/`.commit()`, and
everything else tracked in
[`docs/behind-the-seam-register.md`](docs/behind-the-seam-register.md) and
[`docs/modal-compatibility-matrix.md`](docs/modal-compatibility-matrix.md).
The discipline: if a script uses an idiom calque doesn't act on, it emits a
structured leak naming the gap — never a silent drop or a mysterious crash.

### Known gaps

- Real-AWS execution paths (`calque real`/`session`/`fleetrun`) still drive
  hardcoded reference bodies rather than an arbitrary parsed script's picked
  unit for some features (e.g. `.local()` sibling shipping is dry-run-only
  today, [#92](https://github.com/spore-host/calque/issues/92)).
- N=100k multi-instance rung not yet run at real scale
  ([#18](https://github.com/spore-host/calque/issues/18)).
