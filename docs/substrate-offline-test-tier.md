# Design note: Substrate as calque's offline AWS test tier (calque#114)

**Status:** shipped, with one documented scope reduction from the original
issue text. `github.com/scttfrdmn/substrate/emulator` (Scott's event-sourced
AWS emulator) is now calque's middle test tier: hand-written fakes (today's
`plan_test.go` `fakeResolver`/`scriptedLauncher` pattern, unchanged) at one
end, real-`AWS_PROFILE=aws` spend at the other, and Substrate in between —
real request/response wire round-trips, no billing.

## What shipped

`internal/plan/substrate_test.go`:

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

Both tests run with `PricePerHour` pinned on the launcher (skips spawn's own
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
