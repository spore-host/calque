package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	warm "github.com/spore-host/calque/worker/warm-runner"
)

// Manifest mirrors cmd/warmd.Manifest (the work order warmd reads from S3). Kept
// here too so the control plane can WRITE it without importing package main.
type Manifest struct {
	EnterBody  string `json:"enter_body"`
	MethodBody string `json:"method_body"`
	MethodArg  string `json:"method_arg"`
	// MethodArgs/Starmap/Extras/ExtraConsts mirror warm.Config's fields of the
	// same name (calque#93/#139) — carried here so a real-AWS run (calque#79
	// Part 1) can ship an arbitrary parsed script's picked warm unit (with
	// its .starmap() tuple-splat args and sibling function/constant
	// closure) through the S3 manifest, the same way dryRunWarm already
	// does locally. Omitted (zero value) reproduces every pre-#79 manifest
	// byte-for-byte, since none of calque real/ramp/fleetrun ever set them
	// before this change.
	MethodArgs   []string          `json:"method_args,omitempty"`
	Starmap      bool              `json:"starmap,omitempty"`
	Extras       []warm.ExtraFunc  `json:"extras,omitempty"`
	ExtraConsts  []warm.ExtraConst `json:"extra_consts,omitempty"`
	Items        []warm.Item       `json:"items"`
	Bucket       string            `json:"bucket"`
	ResultPrefix string            `json:"result_prefix"`
	SummaryKey   string            `json:"summary_key"`
	PythonBin    string            `json:"python_bin"`
	RunnerPath   string            `json:"runner_path"`
	Occupancy    string            `json:"occupancy_path"`
	VolumeSync   []VolumeSyncSpec  `json:"volume_sync,omitempty"`   // staged before @enter (§3/§15)
	VolumeCommit []VolumeSyncSpec  `json:"volume_commit,omitempty"` // written back after @method drains (§E)
	Concurrency  int               `json:"concurrency,omitempty"`   // items in flight; 0/1 => serial (occupancy knob)
	BatchSize    int               `json:"batch_size,omitempty"`    // items per micro-batch (one generate(list) call)
}

// VolumeSyncSpec tells warmd to `aws s3 sync <URI> <MountPath>` before @enter, so
// the payload finds its Volume weights at the mount path. Delta-sync => a warm
// cache is a near-noop (weight-cache reuse, §15).
type VolumeSyncSpec struct {
	URI       string `json:"uri"`        // s3://bucket/volumes/<name>
	MountPath string `json:"mount_path"` // e.g. /weights
}

// RunLayout is the S3 key layout for one run, derived from a runID.
type RunLayout struct {
	Bucket       string
	ArtifactPfx  string // warmd + *.py live here
	ManifestKey  string
	ResultPrefix string
	SummaryKey   string
	LogKey       string // bootstrap log, uploaded on instance exit (observability)
	Concurrency  int    // items warmd keeps in flight; 0/1 => serial (occupancy knob)
	BatchSize    int    // items per micro-batch (real vLLM occupancy lever); 0/1 => per-item
}

func NewLayout(bucket, runID string) RunLayout {
	base := "runs/" + runID
	return RunLayout{
		Bucket:       bucket,
		ArtifactPfx:  base + "/artifacts",
		ManifestKey:  base + "/manifest.json",
		ResultPrefix: base + "/results",
		SummaryKey:   base + "/summary.json",
		LogKey:       base + "/bootstrap.log",
	}
}

// UploadArtifacts puts the warmd binary + python scripts under the artifact prefix.
func UploadArtifacts(ctx context.Context, c *s3.Client, l RunLayout, warmdBin, runnerPy, occupancyPy string) error {
	files := map[string]string{
		"warmd":        warmdBin,
		"runner.py":    runnerPy,
		"occupancy.py": occupancyPy,
	}
	for name, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		_, err = c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(l.Bucket),
			Key:    aws.String(l.ArtifactPfx + "/" + name),
			Body:   f,
		})
		f.Close()
		if err != nil {
			return fmt.Errorf("put %s: %w", name, err)
		}
	}
	return nil
}

// WriteManifest builds and uploads the work manifest for a run. Optional
// volumeSync entries are staged (aws s3 sync) by warmd before @enter (§3/§15).
func WriteManifest(ctx context.Context, c *s3.Client, l RunLayout, enterBody, methodBody, methodArg, workerDir string, items []warm.Item, volumeSync ...VolumeSyncSpec) error {
	return WriteManifestFull(ctx, c, l, enterBody, methodBody, methodArg, workerDir, items, volumeSync, nil)
}

// WriteManifestFull is WriteManifest plus volume COMMIT specs (§E): volumes warmd
// syncs BACK to S3 after @method drains (Modal's volume.commit()). Kept as a
// separate entry point so the common no-commit callers stay terse.
func WriteManifestFull(ctx context.Context, c *s3.Client, l RunLayout, enterBody, methodBody, methodArg, workerDir string, items []warm.Item, volumeSync, volumeCommit []VolumeSyncSpec) error {
	return writeManifest(ctx, c, l, ManifestBody{EnterBody: enterBody, MethodBody: methodBody, MethodArg: methodArg}, workerDir, items, volumeSync, volumeCommit)
}

// ManifestBody carries a warm unit's actual executable content — everything
// warm.Config needs beyond items/volumes/concurrency knobs. Exists so a
// caller with a real parsed script's picked unit (calque#79 Part 1: plain-
// @app.function/`.starmap()`/`.local()`-chained real-AWS execution, not
// just --dry-run) can ship its FULL shape — tuple-splat args, sibling
// function/constant closure — through WriteManifestBody, instead of
// WriteManifest's EnterBody/MethodBody/MethodArg-only signature, which
// predates calque#93/#139 and has no way to carry them.
type ManifestBody struct {
	EnterBody   string
	MethodBody  string
	MethodArg   string
	MethodArgs  []string         // calque#93: full .starmap() positional arg list
	Starmap     bool             // calque#93: splat MethodArgs instead of binding MethodArg alone
	Extras      []warm.ExtraFunc // calque#92/#139: sibling functions the body references
	ExtraConsts []warm.ExtraConst
}

// WriteManifestBody is WriteManifestFull with a full ManifestBody instead of
// just EnterBody/MethodBody/MethodArg — the real-AWS-execution counterpart
// to dryRunWarm's local warm.Config construction (cmd/calque/run.go).
func WriteManifestBody(ctx context.Context, c *s3.Client, l RunLayout, body ManifestBody, workerDir string, items []warm.Item, volumeSync, volumeCommit []VolumeSyncSpec) error {
	return writeManifest(ctx, c, l, body, workerDir, items, volumeSync, volumeCommit)
}

func writeManifest(ctx context.Context, c *s3.Client, l RunLayout, body ManifestBody, workerDir string, items []warm.Item, volumeSync, volumeCommit []VolumeSyncSpec) error {
	man := Manifest{
		EnterBody: body.EnterBody, MethodBody: body.MethodBody, MethodArg: body.MethodArg,
		MethodArgs: body.MethodArgs, Starmap: body.Starmap, Extras: body.Extras, ExtraConsts: body.ExtraConsts,
		Items: items, Bucket: l.Bucket, ResultPrefix: l.ResultPrefix, SummaryKey: l.SummaryKey,
		PythonBin: "python3", RunnerPath: workerDir + "/runner.py", Occupancy: workerDir + "/occupancy.py",
		VolumeSync: volumeSync, VolumeCommit: volumeCommit, Concurrency: l.Concurrency, BatchSize: l.BatchSize,
	}
	buf, err := json.Marshal(man)
	if err != nil {
		return err
	}
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(l.Bucket), Key: aws.String(l.ManifestKey), Body: bytes.NewReader(buf),
	})
	return err
}

// ErrBootstrapFailed means the instance's bootstrap script exited (uploading its
// log) WITHOUT producing a summary — a fast-failure signal so we don't dead-wait
// the full deadline for a summary that will never come. BootstrapLog carries the
// uploaded log for post-mortem.
type ErrBootstrapFailed struct{ BootstrapLog string }

func (e *ErrBootstrapFailed) Error() string {
	return "bootstrap exited without writing a summary (see bootstrap log)"
}

// ErrInstanceStale means the instance's spawn:last-heartbeat tag (spawn#497,
// spored v0.100.0+) hasn't advanced for longer than the caller's staleAfter
// window — the instance is hung or gone, not just slow. LastHeartbeat is the
// most recent heartbeat timestamp observed (zero if never seen at all).
type ErrInstanceStale struct {
	InstanceID    string
	LastHeartbeat time.Time
}

func (e *ErrInstanceStale) Error() string {
	if e.LastHeartbeat.IsZero() {
		return fmt.Sprintf("instance %s: spawn:last-heartbeat never observed", e.InstanceID)
	}
	return fmt.Sprintf("instance %s: spawn:last-heartbeat stale since %s", e.InstanceID, e.LastHeartbeat.Format(time.RFC3339))
}

// WaitForSummary polls S3 for the run summary warmd writes on completion. It ALSO
// watches for the bootstrap log: that uploads only on the bootstrap's EXIT
// (success or failure), so if the log appears but no summary follows within a
// short grace window, the bootstrap died — we fail fast with ErrBootstrapFailed
// (carrying the log) instead of waiting out the whole timeout. Returns the summary
// bytes on success.
func WaitForSummary(ctx context.Context, c *s3.Client, l RunLayout, timeout, poll time.Duration, onWait func(elapsed time.Duration)) ([]byte, error) {
	return waitForSummary(ctx, c, l, timeout, poll, onWait, nil)
}

// heartbeatGetter is the slice of *ec2.Client this file depends on to read
// spawn:last-heartbeat (spawn#497) — kept as an interface so tests can inject
// a fake instead of requiring live AWS credentials, mirroring
// internal/plan/quota.go's quotaGetter/capsGetter seam pattern.
type heartbeatGetter interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// WaitForSummaryLiveness is WaitForSummary plus a liveness check (spawn#497):
// spored stamps instanceID's spawn:last-heartbeat EC2 tag every monitor tick
// (throttled to once/minute) REGARDLESS of any completion-webhook config —
// an always-on signal, present on any spawn v0.100.0+ instance. Alongside
// polling for the summary/bootstrap-log artifacts, this also checks that
// tag; if it goes stale for longer than staleAfter, the wait fails fast with
// ErrInstanceStale instead of dead-waiting the full timeout on an instance
// that's actually hung or gone — the exact ambiguity ("still legitimately
// running" vs. "stuck") that made calque#141/#142/#143's re-verification
// runs need a guessed wall-clock deadline in the first place.
//
// A missing/unparsable heartbeat tag (an older spawn, or the instance hasn't
// ticked yet) is NOT treated as staleness — the liveness check simply
// reports "not stale" for that tick, falling back to today's timeout-only
// behavior. This is purely additive: a caller who can't supply an
// instanceID/EC2 client still gets WaitForSummary's original behavior.
func WaitForSummaryLiveness(ctx context.Context, c *s3.Client, ec2c heartbeatGetter, instanceID string, l RunLayout, timeout, poll, staleAfter time.Duration, onWait func(elapsed time.Duration)) ([]byte, error) {
	var lastSeen time.Time
	check := func() error {
		if hb, ok := instanceHeartbeat(ctx, ec2c, instanceID); ok && hb.After(lastSeen) {
			lastSeen = hb
		}
		if heartbeatStale(time.Now(), lastSeen, staleAfter) {
			return &ErrInstanceStale{InstanceID: instanceID, LastHeartbeat: lastSeen}
		}
		return nil
	}
	return waitForSummary(ctx, c, l, timeout, poll, onWait, check)
}

// ErrFleetStale means EVERY worker instance in instanceIDs has gone stale
// on spawn:last-heartbeat (calque#145 slice 3) — the fleet-wide counterpart
// to ErrInstanceStale, for a shard whose claim is served by ANY of a pool
// of workers rather than one dedicated instance. LastHeartbeat is the MOST
// RECENT heartbeat observed across all of them (zero if none was ever
// seen at all).
type ErrFleetStale struct {
	InstanceIDs   []string
	LastHeartbeat time.Time
}

func (e *ErrFleetStale) Error() string {
	if e.LastHeartbeat.IsZero() {
		return fmt.Sprintf("fleet: all %d worker instance(s) never observed a spawn:last-heartbeat", len(e.InstanceIDs))
	}
	return fmt.Sprintf("fleet: all %d worker instance(s) stale since %s", len(e.InstanceIDs), e.LastHeartbeat.Format(time.RFC3339))
}

// WaitForSummaryLivenessAny is WaitForSummaryLiveness's fleet-wide sibling
// (calque#145 slice 3): instead of one instanceID pinned to this shard, it
// takes every worker instance currently believed live for the RUN (a pool
// of workers any of which might serve this shard's claim), and fails fast
// ONLY when every one of them is stale — a single dead worker among
// survivors is NOT fleet death; SQS's own visibility-timeout redelivery
// already recovers that case by routing the claim to a survivor
// (internal/pool.Worker's claim loop, unmodified by this). This function's
// entire job is detecting the case that recovery mechanism can't handle:
// every worker gone, so no survivor will ever claim the redelivered
// message and the shard would otherwise dead-wait the full timeout.
//
// instanceIDs is a snapshot taken ONCE by the caller before submitting
// shard claims (calque#145 slice 3 deliberately does not re-discover
// workers mid-wait — fleet size is fixed at ProvisionFleetWorkers time,
// there is no scale-up-mid-run mechanism to make a mid-wait refresh
// meaningful). An empty instanceIDs (e.g. discovery itself failed) falls
// back to WaitForSummary's plain timeout-only behavior rather than
// treating "no IDs to check" as "all IDs stale."
func WaitForSummaryLivenessAny(ctx context.Context, c *s3.Client, ec2c heartbeatGetter, instanceIDs []string, l RunLayout, timeout, poll, staleAfter time.Duration, onWait func(elapsed time.Duration)) ([]byte, error) {
	if len(instanceIDs) == 0 {
		return waitForSummary(ctx, c, l, timeout, poll, onWait, nil)
	}
	// lastSeen must persist ACROSS ticks via this closure, exactly like
	// WaitForSummaryLiveness's single lastSeen variable — a value
	// recomputed fresh each tick would make every tick look like "never
	// seen," and heartbeatStale treats a zero-value lastSeen as NOT stale,
	// so that bug would silently manifest as "this check never fires,"
	// not a crash. See orchestrate_test.go's persistence test.
	lastSeen := make(map[string]time.Time, len(instanceIDs))
	check := func() error {
		seen := instanceHeartbeats(ctx, ec2c, instanceIDs)
		allStale := true
		var newest time.Time
		for _, id := range instanceIDs {
			if hb, ok := seen[id]; ok && hb.After(lastSeen[id]) {
				lastSeen[id] = hb
			}
			if !heartbeatStale(time.Now(), lastSeen[id], staleAfter) {
				allStale = false
			}
			if lastSeen[id].After(newest) {
				newest = lastSeen[id]
			}
		}
		if allStale {
			return &ErrFleetStale{InstanceIDs: append([]string{}, instanceIDs...), LastHeartbeat: newest}
		}
		return nil
	}
	return waitForSummary(ctx, c, l, timeout, poll, onWait, check)
}

// heartbeatStale is WaitForSummaryLiveness's staleness decision, extracted
// as a pure function (now/lastSeen injected rather than time.Now()/a
// closure-captured var) so it's unit-testable without a real clock. A
// lastSeen that's still its zero value (no heartbeat ever observed) is NOT
// staleness on its own — an older spawn, or one that hasn't ticked yet,
// must not fail a run that's otherwise progressing fine; staleness only
// fires once a REAL heartbeat has been seen and then stopped advancing.
func heartbeatStale(now, lastSeen time.Time, staleAfter time.Duration) bool {
	if lastSeen.IsZero() {
		return false
	}
	return now.Sub(lastSeen) > staleAfter
}

// instanceHeartbeat returns instanceID's spawn:last-heartbeat tag value
// (spawn#497), parsed as RFC3339, or (zero time, false) if the tag is
// absent, unparsable, or the DescribeInstances call itself fails — any of
// which just means "no liveness signal available," not "instance is dead."
func instanceHeartbeat(ctx context.Context, c heartbeatGetter, instanceID string) (time.Time, bool) {
	out, ok := instanceHeartbeats(ctx, c, []string{instanceID})[instanceID]
	return out, ok
}

// instanceHeartbeats is instanceHeartbeat's batched sibling (calque#145
// slice 3): ONE DescribeInstances call for every id in instanceIDs, instead
// of N separate calls — WaitForSummaryLivenessAny checks every fleet worker
// on every poll tick, and issuing N individual API calls per tick (rather
// than one call carrying N ids, which EC2's DescribeInstances already
// supports natively) would multiply needlessly with fleet size. Missing/
// unparsable entries are simply absent from the returned map (same "no
// signal available" contract as instanceHeartbeat's (zero, false)).
func instanceHeartbeats(ctx context.Context, c heartbeatGetter, instanceIDs []string) map[string]time.Time {
	out := make(map[string]time.Time, len(instanceIDs))
	if len(instanceIDs) == 0 {
		return out
	}
	resp, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: instanceIDs})
	if err != nil {
		return out
	}
	for _, res := range resp.Reservations {
		for _, inst := range res.Instances {
			if inst.InstanceId == nil {
				continue
			}
			for _, t := range inst.Tags {
				if t.Key != nil && *t.Key == "spawn:last-heartbeat" && t.Value != nil {
					if ts, perr := time.Parse(time.RFC3339, *t.Value); perr == nil {
						out[*inst.InstanceId] = ts
					}
					break
				}
			}
		}
	}
	return out
}

// waitForSummary is WaitForSummary/WaitForSummaryLiveness's shared core.
// liveness is nil for the plain (no-heartbeat) case; when non-nil, it's
// called once per tick and any error it returns ends the wait immediately —
// the same fail-fast treatment as ErrBootstrapFailed.
func waitForSummary(ctx context.Context, c *s3.Client, l RunLayout, timeout, poll time.Duration, onWait func(elapsed time.Duration), liveness func() error) ([]byte, error) {
	deadlineHit := time.NewTimer(timeout)
	defer deadlineHit.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	startedTicks := 0
	logSeenAt := -1 // tick index when the bootstrap log first appeared; -1 = not yet
	// Grace ticks: after the log appears, how long to keep polling for a summary
	// before declaring failure (warmd writes the summary just before the trap
	// uploads the log, so a healthy run has both within a tick or two).
	const graceTicks = 2

	for {
		if buf, ok := tryGet(ctx, c, l.Bucket, l.SummaryKey); ok {
			return buf, nil // summary landed -> success
		}
		// If the bootstrap log exists but the summary doesn't, the script exited
		// without success. Give a short grace, then fail fast with the log.
		if logSeenAt < 0 {
			if _, ok := tryGet(ctx, c, l.Bucket, l.LogKey); ok {
				logSeenAt = startedTicks
			}
		} else if startedTicks-logSeenAt >= graceTicks {
			logBuf, _ := tryGet(ctx, c, l.Bucket, l.LogKey)
			return nil, &ErrBootstrapFailed{BootstrapLog: string(logBuf)}
		}
		if liveness != nil {
			if lerr := liveness(); lerr != nil {
				return nil, lerr
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadlineHit.C:
			return nil, fmt.Errorf("summary %s did not appear within %s", l.SummaryKey, timeout)
		case <-ticker.C:
			startedTicks++
			if onWait != nil {
				onWait(time.Duration(startedTicks) * poll)
			}
		}
	}
}

// TryGetSummary fetches an S3 object, returning (body, true) on 200 or
// (nil, false) if it isn't there yet / on any error. Exported for the session
// runner to poll for prep logs, rung summaries, and test logs.
func TryGetSummary(ctx context.Context, c *s3.Client, bucket, key string) ([]byte, bool) {
	return tryGet(ctx, c, bucket, key)
}

// tryGet fetches an S3 object, returning (body, true) on 200 or (nil, false) on
// any error (including 404 — the object isn't there yet).
func tryGet(ctx context.Context, c *s3.Client, bucket, key string) ([]byte, bool) {
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, false
	}
	defer out.Body.Close()
	buf, rerr := io.ReadAll(out.Body)
	if rerr != nil {
		return nil, false
	}
	return buf, true
}
