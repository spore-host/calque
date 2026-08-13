# Design note: Substrate as calque's offline AWS test tier (calque#114)

**Status:** Authoritative current behavior. Verified through: v0.3.0.
Shipped, then PARTIALLY REGRESSED by calque#106's later
`Acquirer` → `lagotto/pkg/snipe.Snipe` migration — see "Regression" below.
`github.com/scttfrdmn/substrate/emulator` (Scott's event-sourced AWS
emulator) was calque's middle test tier: hand-written fakes at one end,
real-`AWS_PROFILE=aws` spend at the other, Substrate in between — real
request/response wire round-trips, no billing. That middle tier for
`Acquirer` specifically no longer exists as of the `Snipe` migration; see
below for why and what's tracked to restore it.

## Regression: the Acquirer↔Substrate tests were deleted (calque#106 migration)

`internal/plan/substrate_test.go` (originally `TestAcquireAgainstSubstrate`/
`TestAcquireAgainstSubstrateInjectedFailure`, both described below in their
original, now-historical form) was DELETED when `Acquirer.Acquire` was
migrated to delegate to `lagotto/pkg/snipe.Snipe` (lagotto#106/#111,
calque commit history — the same migration that deleted calque's own
hand-rolled retry/backoff/AZ-sweep/classify loop and its 5 local tests,
per `internal/plan/plan_test.go`'s own updated comments).

The reason: `Snipe` builds its own `*spawnaws.Client` internally via
`spawnaws.NewClientWithRegion` (region-pinned as of lagotto#111), which
loads AWS config through the default credential chain with NO way to
point at a custom endpoint. The only constructor that supports a custom
endpoint, `spawnaws.NewClientFromConfig`, is unreachable from `Snipe` —
its internal `clientFor` is unexported and `Options`/`Target` have no
field for injecting a client or a custom `aws.Config`. Filed upstream:
[lagotto#113](https://github.com/spore-host/lagotto/issues/113).

Until that lands, `Acquirer`'s real request-building/response-parsing
code has NO offline test tier at all — only real `AWS_PROFILE=aws` runs
exercise it (as they already did for the retry/backoff logic before
`Snipe`, and as `fleetrun.go`'s own historical untested-without-spend
precedent already established for this exact code path). This is a real,
acknowledged loss of coverage, accepted as the cost of deleting ~180 lines
of hand-rolled retry logic in favor of a well-tested (16 tests) upstream
leaf — re-evaluate once lagotto#113 lands.

## What shipped (historical — describes the NOW-DELETED tests, kept for context)

`internal/plan/substrate_test.go` (deleted, see above):

- **`TestAcquireAgainstSubstrate`** — points the REAL `plan.SpawnLauncher`
  (wrapping spawn's real `*spawnaws.Client`/`launcher.Provision`) at a
  Substrate `emulator.StartTestServer(t)`, and confirms `Acquirer.Acquire`
  completes a genuine `RunInstances` round-trip: real request-building,
  real XML response parsing, a real instance ID/region come back. This is
  the exact gap the issue named — `SpawnLauncher`'s actual request-building
  code was, before this, NEVER exercised offline; only `Acquirer`'s
  retry/classify logic was, via `fakeLauncher`-shaped stand-ins.
- **`TestAcquireAgainstSubstrateInjectedFailure`** — seeds a Substrate fault
  rule so `RunInstances` fails on every attempt, and confirms the failure
  reaches `classify()` as a real `smithy.APIError` through the REAL
  `spawnaws.LaunchError` unwrap chain (not a hand-typed `apiErr` fake) and
  is handled by the bounded-retry-then-fail-fast path — never an infinite
  loop on an unclassifiable error.

Both tests ran with `PricePerHour` pinned on the launcher (skips spawn's own
live AWS Pricing API call — a real network dependency separate from EC2 that
would otherwise sneak into an "offline" test) and a pinned `AMI` (skips AMI
auto-detection's SSM round-trip, which Substrate would need separately
seeded).

## Scope reduction: capacity-code classification is not fully provable yet

The original issue asked for a test seeding `InsufficientInstanceCapacity`
and confirming `classify()`'s capacity branch fires. That's NOT what
`TestAcquireAgainstSubstrateInjectedFailure` proves today, and the test's
own comments say so explicitly — filed upstream as
[substrate#591](https://github.com/scttfrdmn/substrate/issues/591):

Substrate's EC2 plugin writes errors as `<ErrorResponse><Error><Code>...`
(confirmed via a raw HTTP POST, bypassing the SDK), but the AWS SDK v2 EC2
client's `ec2query` protocol deserializer
(`aws-sdk-go-v2/aws/protocol/ec2query/error_utils.go`) expects
`<Errors><Error><Code>...` — an extra wrapping `<Errors>` plural real EC2
responses have and Substrate's don't. The mismatched XPath means `Code`
decodes empty, and the SDK reports a generic `"UnknownError"` instead of the
injected (or any real) EC2 error code.

Consequence: `TestAcquireAgainstSubstrateInjectedFailure` proves the FAILURE
path works end-to-end (real error → real unwrap chain → bounded retry →
fail fast), but not the CODE-SPECIFIC classification `acquire.go`'s
`capacityCodes`/`terminalCodes` tables exist for — that still only has
offline coverage via the pre-existing hand-typed `apiErr` fakes in
`plan_test.go` (`TestClassify`, `TestAcquireRetriesThenLands`, etc.,
unchanged, still the correct tool for testing the CLASSIFICATION TABLE
itself). The test has a `TODO(substrate#591)` marking exactly what to
upgrade once that's fixed upstream.

## Scope reduction: `internal/gate/bedrock.go` is out of reach today

The original issue also asked to extend Substrate coverage to
`internal/gate/bedrock.go`'s `LiveCatalog.Models` (backed by
`ListFoundationModels`). Checked directly: that call is against the
`bedrock` control-plane service. Substrate's Bedrock support
(`emulator/bedrock_runtime_plugin.go`) is `bedrock-runtime` only —
`InvokeModel` and model-invocation-job operations, no
`ListFoundationModels`. These are two different AWS services sharing a
name prefix; the original issue's "Bedrock Runtime is in Substrate's
supported-service list" was true but didn't establish that the SPECIFIC
operation `bedrock.go` needs is covered. Not attempted here — `bedrock.go`
stays untestable-offline until Substrate adds the `bedrock` (non-Runtime)
service, or `bedrock.go` is refactored to use an operation Substrate does
support (neither is in scope for this issue).

## Real-AWS tier (unchanged)

`AWS_PROFILE=aws`-gated live-fire verification (used throughout M12-M14's
own work, e.g. calque#104's `gpuprobe`, calque#112's live `spawnRun`
verification) remains the tier for what Substrate's API-level observability
genuinely can't stand in for: actual instance boot, `warmd`/`runner.py`
execution, real driver/AMI behavior, and (per the two gaps above) EC2
error-code fidelity and Bedrock catalog fetches until Substrate's own gaps
close.
