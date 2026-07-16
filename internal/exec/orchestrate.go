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
	"github.com/aws/aws-sdk-go-v2/service/s3"

	warm "github.com/spore-host/calque/worker/warm-runner"
)

// Manifest mirrors cmd/warmd.Manifest (the work order warmd reads from S3). Kept
// here too so the control plane can WRITE it without importing package main.
type Manifest struct {
	EnterBody    string           `json:"enter_body"`
	MethodBody   string           `json:"method_body"`
	MethodArg    string           `json:"method_arg"`
	Items        []warm.Item      `json:"items"`
	Bucket       string           `json:"bucket"`
	ResultPrefix string           `json:"result_prefix"`
	SummaryKey   string           `json:"summary_key"`
	PythonBin    string           `json:"python_bin"`
	RunnerPath   string           `json:"runner_path"`
	Occupancy    string           `json:"occupancy_path"`
	VolumeSync   []VolumeSyncSpec `json:"volume_sync,omitempty"` // staged before @enter (§3/§15)
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
	man := Manifest{
		EnterBody: enterBody, MethodBody: methodBody, MethodArg: methodArg,
		Items: items, Bucket: l.Bucket, ResultPrefix: l.ResultPrefix, SummaryKey: l.SummaryKey,
		PythonBin: "python3", RunnerPath: workerDir + "/runner.py", Occupancy: workerDir + "/occupancy.py",
		VolumeSync: volumeSync,
	}
	body, err := json.Marshal(man)
	if err != nil {
		return err
	}
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(l.Bucket), Key: aws.String(l.ManifestKey), Body: bytes.NewReader(body),
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

// WaitForSummary polls S3 for the run summary warmd writes on completion. It ALSO
// watches for the bootstrap log: that uploads only on the bootstrap's EXIT
// (success or failure), so if the log appears but no summary follows within a
// short grace window, the bootstrap died — we fail fast with ErrBootstrapFailed
// (carrying the log) instead of waiting out the whole timeout. Returns the summary
// bytes on success.
func WaitForSummary(ctx context.Context, c *s3.Client, l RunLayout, timeout, poll time.Duration, onWait func(elapsed time.Duration)) ([]byte, error) {
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
