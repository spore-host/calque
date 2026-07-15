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
	EnterBody    string      `json:"enter_body"`
	MethodBody   string      `json:"method_body"`
	MethodArg    string      `json:"method_arg"`
	Items        []warm.Item `json:"items"`
	Bucket       string      `json:"bucket"`
	ResultPrefix string      `json:"result_prefix"`
	SummaryKey   string      `json:"summary_key"`
	PythonBin    string      `json:"python_bin"`
	RunnerPath   string      `json:"runner_path"`
	Occupancy    string      `json:"occupancy_path"`
}

// RunLayout is the S3 key layout for one run, derived from a runID.
type RunLayout struct {
	Bucket       string
	ArtifactPfx  string // warmd + *.py live here
	ManifestKey  string
	ResultPrefix string
	SummaryKey   string
}

func NewLayout(bucket, runID string) RunLayout {
	base := "runs/" + runID
	return RunLayout{
		Bucket:       bucket,
		ArtifactPfx:  base + "/artifacts",
		ManifestKey:  base + "/manifest.json",
		ResultPrefix: base + "/results",
		SummaryKey:   base + "/summary.json",
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

// WriteManifest builds and uploads the work manifest for a run.
func WriteManifest(ctx context.Context, c *s3.Client, l RunLayout, enterBody, methodBody, methodArg, workerDir string, items []warm.Item) error {
	man := Manifest{
		EnterBody: enterBody, MethodBody: methodBody, MethodArg: methodArg,
		Items: items, Bucket: l.Bucket, ResultPrefix: l.ResultPrefix, SummaryKey: l.SummaryKey,
		PythonBin: "python3", RunnerPath: workerDir + "/runner.py", Occupancy: workerDir + "/occupancy.py",
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

// WaitForSummary polls S3 for the run summary warmd writes on completion, up to
// timeout. Returns the parsed summary bytes (caller decodes to its Summary type).
func WaitForSummary(ctx context.Context, c *s3.Client, l RunLayout, timeout, poll time.Duration, onWait func(elapsed time.Duration)) ([]byte, error) {
	// Note: deadline is enforced by ctx if the caller sets one; we also self-bound.
	deadlineHit := time.NewTimer(timeout)
	defer deadlineHit.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	startedTicks := 0
	for {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(l.Bucket), Key: aws.String(l.SummaryKey)})
		if err == nil {
			buf, rerr := io.ReadAll(out.Body)
			out.Body.Close()
			if rerr != nil {
				return nil, rerr
			}
			return buf, nil
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
