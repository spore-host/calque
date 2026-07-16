// Command warmd is the on-instance entrypoint baked into the worker image (spec
// §6). The container runs `warmd run --manifest s3://.../manifest.json`. It:
//
//  1. reads the manifest (warm-unit bodies + item payloads + output prefix) from S3,
//  2. starts the occupancy sampler sidecar (occupancy.py) for the tach hook (§8),
//  3. drives the warm supervisor: @enter once, drain items, each result -> S3
//     keyed by index (crash-restart + re-drive handled by the supervisor),
//  4. writes a run summary (enter seconds, per-item series, occupancy) to S3.
//
// This is distinct from the spore.host `spored` systemd daemon (which owns the
// whole instance lifecycle and runs THIS under it). See worker/warm-runner docs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	calexec "github.com/spore-host/calque/internal/exec"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// Manifest is the work order the control plane writes to S3; warmd reads it.
type Manifest struct {
	EnterBody    string                   `json:"enter_body"`
	MethodBody   string                   `json:"method_body"`
	MethodArg    string                   `json:"method_arg"`
	Items        []warm.Item              `json:"items"`
	Bucket       string                   `json:"bucket"`
	ResultPrefix string                   `json:"result_prefix"`
	SummaryKey   string                   `json:"summary_key"`
	PythonBin    string                   `json:"python_bin"`            // interpreter in the image
	RunnerPath   string                   `json:"runner_path"`           // path to runner.py in the image
	Occupancy    string                   `json:"occupancy_path"`        // path to occupancy.py in the image
	VolumeSync   []calexec.VolumeSyncSpec `json:"volume_sync,omitempty"` // staged (aws s3 sync) before @enter (§3/§15)
}

// Summary is what warmd writes back so the control plane's measure step can fold
// the ground truth into the cost model.
type Summary struct {
	EnterSeconds float64              `json:"enter_seconds"`
	EnterCount   int                  `json:"enter_count"`
	PerItemSecs  []float64            `json:"per_item_secs"`
	Failed       []int                `json:"failed"`
	Occupancy    calexec.OccupancyRaw `json:"occupancy"`
	StartedUnix  int64                `json:"started_unix"`
	EndedUnix    int64                `json:"ended_unix"`
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: warmd run --manifest s3://bucket/key")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	manifestURI := fs.String("manifest", "", "s3://bucket/key of the work manifest")
	_ = fs.Parse(os.Args[2:])
	if *manifestURI == "" {
		fmt.Fprintln(os.Stderr, "error: --manifest required")
		os.Exit(2)
	}
	if err := runOnInstance(context.Background(), *manifestURI); err != nil {
		fmt.Fprintln(os.Stderr, "warmd error:", err)
		os.Exit(1)
	}
}

func runOnInstance(ctx context.Context, manifestURI string) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	s3c := s3.NewFromConfig(cfg)

	bucket, key, err := parseS3URI(manifestURI)
	if err != nil {
		return err
	}
	var man Manifest
	if err := getJSON(ctx, s3c, bucket, key, &man); err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}

	// Stage Volume weights BEFORE @enter (§3/§15): aws s3 sync each volume's S3
	// prefix to its mount path. sync is delta-only, so a warm cache is a near-noop
	// (weight-cache reuse). Fail loudly — @enter will fault if weights are missing.
	for _, v := range man.VolumeSync {
		fmt.Fprintf(os.Stderr, "staging volume %s -> %s\n", v.URI, v.MountPath)
		if out, serr := exec.CommandContext(ctx, "aws", "s3", "sync", "--no-progress", v.URI, v.MountPath).CombinedOutput(); serr != nil {
			return fmt.Errorf("volume sync %s -> %s: %w (%s)", v.URI, v.MountPath, serr, out)
		}
	}

	// Start the occupancy sampler sidecar (tach hook, §8). Best-effort: if it
	// can't start, we still run and report occupancy as unmeasured.
	occPath := man.Occupancy
	occOut := "/tmp/calque-occ.jsonl"
	sampler := exec.CommandContext(ctx, pyOr(man.PythonBin), occPath, "sample", "--interval", "1.0", "--out", occOut)
	var occSummaryBuf strings.Builder
	sampler.Stdout = &occSummaryBuf
	occStarted := sampler.Start() == nil

	started := time.Now()
	sink := &calexec.S3Sink{Client: s3c, Bucket: man.Bucket, Prefix: man.ResultPrefix}
	sup := &warm.Supervisor{
		Python: pyOr(man.PythonBin),
		Script: man.RunnerPath,
		Sink:   sink,
		Leak:   stderrLeaker{},
		Config: warm.Config{EnterBody: man.EnterBody, MethodBody: man.MethodBody, MethodArg: man.MethodArg},
	}
	failed, runErr := sup.Run(ctx, man.Items)
	ended := time.Now()

	// Stop the sampler and capture its JSON summary (SIGTERM -> it prints + exits).
	var occ calexec.OccupancyRaw
	if occStarted {
		_ = sampler.Process.Signal(syscall.SIGTERM)
		_ = sampler.Wait()
		_ = json.Unmarshal([]byte(strings.TrimSpace(occSummaryBuf.String())), &occ)
	}

	summary := Summary{
		EnterSeconds: sup.EnterSeconds, EnterCount: sup.EnterCount,
		PerItemSecs: sink.Seconds(), Failed: failed, Occupancy: occ,
		StartedUnix: started.Unix(), EndedUnix: ended.Unix(),
	}
	if err := putJSON(ctx, s3c, man.Bucket, man.SummaryKey, summary); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return runErr
}

// --- small S3/URI helpers ---

func parseS3URI(uri string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	if !ok {
		return "", "", fmt.Errorf("not an s3:// uri: %q", uri)
	}
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", fmt.Errorf("s3 uri missing key: %q", uri)
	}
	return rest[:i], rest[i+1:], nil
}

func getJSON(ctx context.Context, c *s3.Client, bucket, key string, v any) error {
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return err
	}
	defer out.Body.Close()
	return json.NewDecoder(out.Body).Decode(v)
}

func putJSON(ctx context.Context, c *s3.Client, bucket, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(string(b))})
	return err
}

func pyOr(p string) string {
	if p != "" {
		return p
	}
	return "python3"
}

type stderrLeaker struct{}

func (stderrLeaker) Leak(kind, detail string) {
	fmt.Fprintf(os.Stderr, "LEAK[%s] %s\n", kind, detail)
}
