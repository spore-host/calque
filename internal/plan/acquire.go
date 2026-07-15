package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/smithy-go"

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
	PublicIP         string
	State            string
	RequestedAt      time.Time // when we started trying to acquire
	AcquiredAt       time.Time // when a live instance landed
}

// TimeToAcquire is the wall-clock spent sniping capacity — free ground truth the
// real brain will need (§5). Logged per (card, region).
func (a Acquired) TimeToAcquire() time.Duration { return a.AcquiredAt.Sub(a.RequestedAt) }

// Launcher is the one-shot acquire+bring-up primitive. This is the seam confirmed
// in spawn#351: spawn OWNS RunInstances (there is no pre-acquired-instance
// bring-up). The real implementation wraps spawn.launcher.Provision; a fake drives
// tests offline. Returns a *LaunchOutcome or an error whose code we classify.
type Launcher interface {
	Provision(ctx context.Context, instanceType, region string) (LaunchOutcome, error)
}

// LaunchOutcome mirrors the fields calque needs from spawn's *aws.LaunchResult.
type LaunchOutcome struct {
	InstanceID       string
	Region           string
	AvailabilityZone string
	PublicIP         string
	State            string
}

// Progress receives status updates for the live "waiting for capacity…" line
// (§5: lagotto/acquisition exposes no push channel, so the Acquirer emits its own).
type Progress func(attempt int, code string, waited time.Duration)

// Acquirer snipes a single resolved target: it calls Provision and, on a capacity
// failure, retries with backoff until it lands or the deadline passes — the
// block-and-wait posture (§5). This is the lean path the spore.host owner blessed
// in lagotto#73 (Provision + ClassifyFailure), avoiding lagotto's DynamoDB
// dependency. When watcher.Snipe ships, it swaps in behind this same interface.
type Acquirer struct {
	Launcher     Launcher
	Report       *leak.Report
	OnProgress   Progress
	PollInterval time.Duration // backoff between capacity retries (default 15s)
	Deadline     time.Duration // give up after this (default 30m); 0 => default
	// now is injectable so tests don't sleep in real time.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// Acquire blocks until the target lands or the deadline passes. It fills the
// Target's Region on success (§4: acquisition fills Region).
func (a *Acquirer) Acquire(ctx context.Context, t *target.Target, region string) (Acquired, error) {
	now := a.now
	if now == nil {
		now = time.Now
	}
	sleep := a.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	poll := a.PollInterval
	if poll == 0 {
		poll = 15 * time.Second
	}
	deadline := a.Deadline
	if deadline == 0 {
		deadline = 30 * time.Minute
	}

	start := now()
	giveUp := start.Add(deadline)
	attempt := 0
	unknownStreak := 0
	const maxUnknown = 3 // tolerate a few transient/unknown blips, then fail fast
	for {
		attempt++
		out, err := a.Launcher.Provision(ctx, t.Instance, region)
		if err == nil {
			acq := Acquired{
				InstanceID: out.InstanceID, Region: out.Region, AvailabilityZone: out.AvailabilityZone,
				PublicIP: out.PublicIP, State: out.State, RequestedAt: start, AcquiredAt: now(),
			}
			if acq.Region == "" {
				acq.Region = region // spawn#351: LaunchResult has no Region; carry ours
			}
			t.Region = acq.Region
			// Free ground truth: time-to-acquire per (card, region) (§5).
			if a.Report != nil && attempt > 1 {
				a.Report.Addf(leak.PrimAcquire, leak.KindIntegrationEdge, t.Card, 0,
					"acquired %s in %s after %d attempts / %s waiting for capacity",
					t.Instance, acq.Region, attempt, acq.TimeToAcquire().Round(time.Second))
			}
			return acq, nil
		}

		kind, code := classify(err)
		if kind == failureTerminal {
			return Acquired{}, fmt.Errorf("acquire %s/%s: terminal failure %q: %w", t.Instance, region, code, err)
		}
		if kind == failureUnknown {
			unknownStreak++
			if unknownStreak > maxUnknown {
				return Acquired{}, fmt.Errorf("acquire %s/%s: %d consecutive unrecognized errors (last %q); failing fast rather than looping on a likely misconfig: %w",
					t.Instance, region, unknownStreak, code, err)
			}
		} else {
			unknownStreak = 0 // a real capacity signal resets the unknown counter
		}
		// capacity (or a bounded unknown): wait and retry, unless the deadline has
		// passed. This is what lagotto's warm pool hides on Modal.
		waited := now().Sub(start)
		if a.OnProgress != nil {
			a.OnProgress(attempt, code, waited)
		}
		if now().After(giveUp) {
			if a.Report != nil {
				a.Report.Addf(leak.PrimAcquire, leak.KindIntegrationEdge, t.Card, 0,
					"gave up acquiring %s in %s after %s (%d attempts); last code %q",
					t.Instance, region, deadline, attempt, code)
			}
			return Acquired{}, fmt.Errorf("acquire %s/%s: no capacity after %s (%d attempts)", t.Instance, region, deadline, attempt)
		}
		if err := sleep(ctx, poll); err != nil {
			return Acquired{}, err
		}
	}
}

// --- failure classification (mirrors lagotto watcher.ClassifyFailure) ---
//
// We MIRROR lagotto's taxonomy rather than import pkg/watcher, which would drag
// in DynamoDB/S3/SageMaker/SSM transitively (the poller's deps) for ~30 lines of
// well-defined AWS error codes. The owner blessed keying retry on this in
// lagotto#73. Source of truth: lagotto/pkg/watcher/failure.go — keep in sync.

type failureKind int

const (
	failureNone failureKind = iota
	failureCapacity
	failureTerminal
	failureUnknown // unrecognized code: retry, but only a bounded number of times
)

var capacityCodes = map[string]bool{
	"InsufficientInstanceCapacity":         true,
	"InsufficientHostCapacity":             true,
	"InsufficientReservedInstanceCapacity": true,
	"InsufficientCapacity":                 true,
	"Server.InsufficientInstanceCapacity":  true,
	"SpotMaxPriceTooLow":                   true,
}

var terminalCodes = map[string]bool{
	"InstanceLimitExceeded":        true,
	"VcpuLimitExceeded":            true,
	"MaxSpotInstanceCountExceeded": true,
	"InvalidAMIID.NotFound":        true,
	"InvalidAMIID.Malformed":       true,
	"UnauthorizedOperation":        true,
	"AuthFailure":                  true,
	"InvalidParameterValue":        true,
	"InvalidParameterCombination":  true,
	"InvalidSubnetID.NotFound":     true,
	"InvalidGroup.NotFound":        true,
	"Unsupported":                  true,
	// Config/setup errors surfaced by spawn's pre-launch steps (AMI resolution via
	// SSM, IAM). These will NEVER resolve by waiting — retrying them just masks a
	// misconfiguration as a capacity wait (observed: spawn's GPU AL2023 SSM param
	// doesn't exist, yielding ParameterNotFound on every g6/g7 attempt).
	"ParameterNotFound":     true,
	"AccessDenied":          true,
	"AccessDeniedException": true,
	"ValidationError":       true,
	"ValidationException":   true,
}

// classify returns the failure kind and the AWS error code (for status/logging).
// Unknown and non-AWS errors default to capacity (retryable): a transient blip
// shouldn't permanently abort, and the deadline bounds the loop regardless.
func classify(err error) (failureKind, string) {
	if err == nil {
		return failureNone, ""
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		switch {
		case capacityCodes[code]:
			return failureCapacity, code
		case terminalCodes[code]:
			return failureTerminal, code
		case strings.Contains(code, "InsufficientInstanceCapacity"),
			strings.Contains(code, "InsufficientCapacity"):
			return failureCapacity, code
		default:
			// A recognized AWS error we haven't classified: retry, but bounded — an
			// unknown code looping for the whole deadline masks a real misconfig as a
			// capacity wait (the ParameterNotFound lesson).
			return failureUnknown, code
		}
	}
	return failureUnknown, "" // non-AWS error (e.g. network): retry, bounded
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
