package plan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/smithy-go"

	"github.com/spore-host/lagotto/pkg/snipe"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/target"
)

// Acquired is a live instance handle returned by acquisition. It carries what
// exec/measure/collect need: the instance id, where it landed, and the acquire
// timestamps that anchor the AWS "rectangle" (§8).
type Acquired struct {
	InstanceID       string
	Region           string
	AvailabilityZone string
	RequestedAt      time.Time // when we started trying to acquire
	AcquiredAt       time.Time // when a live instance landed
}

// TimeToAcquire is the wall-clock spent sniping capacity — free ground truth the
// real brain will need (§5). Logged per (card, region).
func (a Acquired) TimeToAcquire() time.Duration { return a.AcquiredAt.Sub(a.RequestedAt) }

// Placement is one (AZ, subnet) the Acquirer will try in its sweep. Passing the
// subnet explicitly avoids the InvalidInput that occurs when an AZ has no default
// subnet (observed: us-west-2d for g7e). Kept as calque's own type (rather than a
// direct alias of snipe.Placement) so callers building calque's Placement slice
// don't need to import lagotto/pkg/snipe themselves — Acquire converts internally.
type Placement struct {
	AZ     string
	Subnet string
}

// Progress receives status updates for the live "waiting for capacity…" line.
// detail is the FULL verbatim error message from AWS (not just the code) — so a
// changed error (capacity opening in an AZ, or a non-capacity failure the code
// classifier would otherwise swallow) is visible in the log.
type Progress func(attempt int, code, detail string, waited time.Duration)

// Acquirer snipes a single resolved target — the block-and-wait posture (§5).
//
// This is a thin adapter over lagotto/pkg/snipe.Snipe (shipped in v0.52.0,
// lagotto#106; region-pinned client resolution fixed in v0.52.1, lagotto#111)
// rather than a hand-rolled retry/backoff/AZ-sweep/classify loop — that loop,
// and the local capacityCodes/terminalCodes table it used to carry, are gone
// (calque#75/lagotto#105/lagotto#106's own audit named this the largest
// deletion opportunity in this package, ~180 lines). Snipe owns retry/backoff/
// AZ-sweep/placement-fallback/deadline/classify entirely; Acquirer's own job is
// now just: build the spawnaws.LaunchConfig calque's callers already assemble
// via LaunchConfig, adapt Placements/Progress to Snipe's shapes, and translate
// snipe.Result back into calque's own Acquired (which every downstream cost-
// model/measure consumer already depends on — kept stable so none of them
// needed to change).
//
// Test coverage: this package's own retry/backoff/AZ-sweep/deadline/classify
// tests were deleted alongside the loop they tested — lagotto/pkg/snipe's own
// test suite (16 tests: TestSnipe_AcquiresAfterCapacityRetries,
// TestSnipe_SubnetPerPlacement, TestSnipe_TerminalStopsImmediately,
// TestSnipe_UnknownFailureCappedThenGivesUp, TestSnipe_DeadlineReached, etc.)
// covers the equivalent behavior directly against Snipe's real implementation,
// confirmed by inspection before this migration — this package no longer needs
// its own copy of those guarantees, the same trust boundary calque already
// extends to spawn.launcher.Provision itself.
//
// REGRESSION (calque#106/docs/substrate-offline-test-tier.md): this migration
// also deleted internal/plan/substrate_test.go's Substrate-backed offline
// tests for Acquire's request-building/response-parsing path — Snipe builds
// its own *spawnaws.Client internally with no way to point it at a custom
// endpoint (spawnaws.NewClientFromConfig, which Substrate needs, is
// unreachable from Snipe). Filed upstream: lagotto#113. Until that lands,
// Acquire has NO offline test tier at all beyond errorCode's own pure-logic
// test — only real AWS_PROFILE=aws runs exercise the actual RunInstances
// round-trip.
type Acquirer struct {
	Report     *leak.Report
	OnProgress Progress
	// PollInterval is the initial backoff between capacity-failed rounds (it
	// doubles each round up to a 5-minute cap, per snipe.DefaultMaxInterval).
	// 0 => snipe.DefaultRetryInterval (30s).
	PollInterval time.Duration
	Deadline     time.Duration // give up after this (default 30m); 0 => default
	// Placements to sweep within the region before backing off, in preference
	// order. Empty => a single attempt letting EC2 choose.
	Placements []Placement
	// LaunchConfig is the base launch config for this acquire — every caller
	// already builds one of these (formerly wrapped in the now-deleted
	// SpawnLauncher). InstanceType/Region/Spot/AvailabilityZone/SubnetID are
	// overridden per attempt by Acquire/Snipe; every other field (AMI, TTL,
	// OnComplete, Username, JobArrayCommand, IMDSv2HopLimit, RootVolumeGiB,
	// PricePerHour, SpotMaxPrice, ...) passes through as given.
	LaunchConfig spawnaws.LaunchConfig
}

// Acquire blocks until the target lands or the deadline passes. It fills the
// Target's Region on success (§4: acquisition fills Region).
//
// This is a thin single-region wrapper over [Acquirer.AcquireMultiRegion]
// (calque#95): it hands the multi-region path a one-element region list using
// a.Placements as that region's sweep, so it produces exactly the same
// snipe.Target/snipe.Options (zero Fallbacks) as before the #95 migration —
// existing single-region callers are unaffected.
func (a *Acquirer) Acquire(ctx context.Context, t *target.Target, region string) (Acquired, error) {
	return a.AcquireMultiRegion(ctx, t, []string{region}, map[string][]Placement{region: a.Placements})
}

// AcquireMultiRegion blocks until the target lands in the first of regions (in
// preference order) that has capacity, or the deadline passes. regions[0] is
// the primary region; any additional regions become lagotto/pkg/snipe's
// Options.Fallbacks (lagotto#76, shipped v0.50.0+) — calque#95's cross-region
// capacity search: Snipe sweeps the primary region's placements, then each
// fallback region's placements IN ORDER, all within one round, before backing
// off and retrying the whole thing. It fills the Target's Region on success
// (§4) to whichever candidate region actually landed.
//
// placementsByRegion supplies each candidate region's own (AZ, subnet) sweep
// list, keyed by region — a caller must resolve AZs/subnets per region (e.g.
// via calexec.AZsForInstance), since a placement in one region says nothing
// about another. A region absent from the map (or mapped to nil/empty) sweeps
// with a single EC2-chosen placement, same as Acquire's existing default.
func (a *Acquirer) AcquireMultiRegion(ctx context.Context, t *target.Target, regions []string, placementsByRegion map[string][]Placement) (Acquired, error) {
	if len(regions) == 0 {
		return Acquired{}, fmt.Errorf("acquire %s: at least one region is required", t.Instance)
	}

	deadline := a.Deadline
	if deadline == 0 {
		deadline = 30 * time.Minute
	}

	primary, fallbacks := buildSnipeTargets(t.Instance, regions, placementsByRegion, a.LaunchConfig)

	requestedAt := time.Now()
	// failedAttempts counts only Status reports carrying a LastErr — Snipe
	// also reports a pre-attempt Status (LastErr==nil) before each placement
	// try and a pre-backoff Status after a whole round fails; calque's own
	// Progress contract (and the leak line below) means "how many failed
	// attempts so far," so only count the ones that actually failed.
	failedAttempts := 0
	result, err := snipe.Snipe(ctx, primary, snipe.Options{
		Deadline:      requestedAt.Add(deadline),
		RetryInterval: a.PollInterval,
		Fallbacks:     fallbacks,
		Progress: func(s snipe.Status) {
			if s.LastErr == nil {
				return // pre-attempt report; nothing failed yet
			}
			failedAttempts++
			code, _ := errorCode(s.LastErr)
			if a.OnProgress != nil {
				a.OnProgress(failedAttempts, code, s.LastErr.Error(), time.Since(requestedAt))
			}
		},
	})
	if err != nil {
		if a.Report != nil {
			a.Report.Addf(leak.PrimAcquire, leak.KindIntegrationEdge, t.Card, 0,
				"gave up acquiring %s in %v after %s: %v", t.Instance, regions, deadline, err)
		}
		return Acquired{}, fmt.Errorf("acquire %s/%v: %w", t.Instance, regions, err)
	}

	acquiredAt := time.Now()
	acq := Acquired{
		InstanceID: result.InstanceID, Region: result.Region, AvailabilityZone: result.AvailabilityZone,
		RequestedAt: requestedAt, AcquiredAt: acquiredAt,
	}
	t.Region = acq.Region
	// Free ground truth: time-to-acquire per (card, region, AZ) (§5).
	if a.Report != nil && failedAttempts > 0 {
		a.Report.Addf(leak.PrimAcquire, leak.KindIntegrationEdge, t.Card, 0,
			"acquired %s in %s/%s after %s waiting for capacity",
			t.Instance, acq.Region, acq.AvailabilityZone, acq.TimeToAcquire().Round(time.Second))
	}
	return acq, nil
}

// buildSnipeTargets translates calque's own (regions, per-region Placements,
// LaunchConfig) shape into what lagotto/pkg/snipe.Snipe wants: a primary
// snipe.Target (regions[0]) plus one snipe.Target per remaining region, to be
// passed as snipe.Options.Fallbacks (lagotto#76) — calque#95's cross-region
// search. Each region gets its OWN Placements from placementsByRegion: an AZ
// in one region says nothing about another, so a caller resolving multiple
// candidate regions must look up each region's AZs/subnets separately (e.g.
// one calexec.AZsForInstance call per region).
func buildSnipeTargets(instanceType string, regions []string, placementsByRegion map[string][]Placement, launchCfg spawnaws.LaunchConfig) (primary snipe.Target, fallbacks []snipe.Target) {
	toSnipePlacements := func(region string) []snipe.Placement {
		places := placementsByRegion[region]
		out := make([]snipe.Placement, len(places))
		for i, p := range places {
			out[i] = snipe.Placement{AZ: p.AZ, Subnet: p.Subnet}
		}
		return out
	}
	buildTarget := func(region string) snipe.Target {
		return snipe.Target{
			InstanceType: instanceType,
			Region:       region,
			Placements:   toSnipePlacements(region),
			Spot:         launchCfg.Spot,
			LaunchConfig: launchCfg,
		}
	}

	primary = buildTarget(regions[0])
	fallbacks = make([]snipe.Target, 0, len(regions)-1)
	for _, region := range regions[1:] {
		fallbacks = append(fallbacks, buildTarget(region))
	}
	return primary, fallbacks
}

// errorCode extracts the AWS error code from err for logging, if it wraps one
// — mirrors the errors.As(err, &apiErr) pattern the old local classify() used,
// unwrapping through spawnaws.LaunchError to the underlying smithy.APIError.
func errorCode(err error) (string, bool) {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode(), true
	}
	return "", false
}
