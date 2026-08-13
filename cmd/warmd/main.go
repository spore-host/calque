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
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	calexec "github.com/spore-host/calque/internal/exec"
	calpool "github.com/spore-host/calque/internal/pool"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// Manifest is the work order the control plane writes to S3; warmd reads it.
type Manifest struct {
	EnterBody  string `json:"enter_body"`
	MethodBody string `json:"method_body"`
	MethodArg  string `json:"method_arg"`
	// MethodArgs/Starmap/Extras/ExtraConsts mirror warm.Config's fields of
	// the same name (calque#93/#139), threaded through so a real-AWS run
	// (calque#79 Part 1) can drive an arbitrary parsed script's picked warm
	// unit — not just the hardcoded vLLM reference body — the same way
	// dryRunWarm already does locally.
	MethodArgs  []string          `json:"method_args,omitempty"`
	Starmap     bool              `json:"starmap,omitempty"`
	Extras      []warm.ExtraFunc  `json:"extras,omitempty"`
	ExtraConsts []warm.ExtraConst `json:"extra_consts,omitempty"`
	// ExtraImports mirrors warm.Config's field of the same name (calque#146)
	// — module-level import statements a picked unit's body bare-references.
	ExtraImports []warm.ExtraImport `json:"extra_imports,omitempty"`
	// ExtraClasses mirrors warm.Config's field of the same name (calque#147)
	// — plain (non-@app.cls) module-level classes a picked unit's body
	// bare-instantiates.
	ExtraClasses []warm.ExtraClass `json:"extra_classes,omitempty"`
	// Secrets mirrors warm.Config's field of the same name — name/value
	// pairs injected into the runner's environment before @enter runs.
	Secrets map[string]string `json:"secrets,omitempty"`
	// PayloadIsBase64Bytes mirrors warm.Config's field of the same name
	// (calque real --item-file PATH).
	PayloadIsBase64Bytes bool `json:"payload_is_base64_bytes,omitempty"`
	// Base64ArgIndices mirrors warm.Config's field of the same name
	// (calque real --arg-file IDX=PATH) — the multi-arg sibling of
	// PayloadIsBase64Bytes.
	Base64ArgIndices []int                    `json:"base64_arg_indices,omitempty"`
	Items            []warm.Item              `json:"items"`
	Bucket           string                   `json:"bucket"`
	ResultPrefix     string                   `json:"result_prefix"`
	SummaryKey       string                   `json:"summary_key"`
	PythonBin        string                   `json:"python_bin"`              // interpreter in the image
	RunnerPath       string                   `json:"runner_path"`             // path to runner.py in the image
	Occupancy        string                   `json:"occupancy_path"`          // path to occupancy.py in the image
	VolumeSync       []calexec.VolumeSyncSpec `json:"volume_sync,omitempty"`   // staged (aws s3 sync) before @enter (§3/§15)
	VolumeCommit     []calexec.VolumeSyncSpec `json:"volume_commit,omitempty"` // written back (mount -> S3) after @method drains (§E)
	Concurrency      int                      `json:"concurrency,omitempty"`   // items in flight at once; 0/1 => serial (raises GPU occupancy)
	BatchSize        int                      `json:"batch_size,omitempty"`    // items per micro-batch (one .generate(list) call); 0/1 => per-item
}

// Summary is what warmd writes back so the control plane's measure step can fold
// the ground truth into the cost model.
type Summary struct {
	EnterSeconds float64   `json:"enter_seconds"`
	EnterCount   int       `json:"enter_count"`
	PerItemSecs  []float64 `json:"per_item_secs"`
	Failed       []int     `json:"failed"`

	// Occupancy is the number K stands on. Since #71 it is the INFERENCE-WINDOW mean
	// whenever the sampler's timestamped stream allows computing one, with Scope
	// saying so; OccupancyWholeRun keeps the old load-contaminated mean alongside it
	// so the two are comparable and nothing is quietly replaced.
	Occupancy         calexec.OccupancyRaw `json:"occupancy"`
	OccupancyWholeRun calexec.OccupancyRaw `json:"occupancy_whole_run,omitempty"`

	// InferenceSpans are the windows during which items were processed (one per
	// drain; a crash-restart adds another). Committed to the summary so the windowing
	// is auditable after the fact, not just trusted.
	InferenceSpans   []warm.Span `json:"inference_spans,omitempty"`
	InferenceSeconds float64     `json:"inference_seconds,omitempty"`

	StartedUnix int64 `json:"started_unix"`
	EndedUnix   int64 `json:"ended_unix"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
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
	case "pool":
		fs := flag.NewFlagSet("pool", flag.ExitOnError)
		model := fs.String("model", "", "this pool's fixed model identity (calque#99 decision 2: single-model-per-pool)")
		region := fs.String("region", "", "AWS region the pool's SQS queue lives in")
		pythonBin := fs.String("python-bin", "python3", "interpreter for the warm runner")
		runnerPath := fs.String("runner-path", "", "path to runner.py in the image")
		idleTimeout := fs.Duration("idle-timeout", 30*time.Minute, "how long to keep the resident runner warm with an empty queue before closing it")
		pollWait := fs.Int("poll-wait-seconds", 20, "SQS long-poll wait per claim (0..20)")
		visibilityTimeout := fs.Int("visibility-timeout", 900, "MUST match the pool queue's own SQS VisibilityTimeout (set by `calque pool create`'s --i-understand-this-spends-money path, currently 900s) — used to size this worker's calque#131 heartbeat interval (visibilityTimeout/3), not to configure the queue itself")
		_ = fs.Parse(os.Args[2:])
		if *model == "" || *runnerPath == "" {
			fmt.Fprintln(os.Stderr, "error: --model and --runner-path required")
			os.Exit(2)
		}
		if err := runPoolWorker(context.Background(), poolWorkerArgs{
			model: *model, region: *region, pythonBin: *pythonBin, runnerPath: *runnerPath,
			idleTimeout: *idleTimeout, pollWaitSeconds: int32(*pollWait),
			visibilityTimeout: *visibilityTimeout,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "warmd pool error:", err)
			os.Exit(1)
		}
	case "fleet":
		fs := flag.NewFlagSet("fleet", flag.ExitOnError)
		runID := fs.String("run-id", "", "this fleet run's id (calque#145: the shard queue is scoped to the RUN, not a model, unlike `warmd pool`)")
		region := fs.String("region", "", "AWS region the run's SQS shard queue lives in")
		pythonBin := fs.String("python-bin", "python3", "interpreter for the warm runner")
		runnerPath := fs.String("runner-path", "", "path to runner.py in the image")
		idleTimeout := fs.Duration("idle-timeout", fleetWorkerDefaultIdleTimeout, "how long to keep the resident runner warm with an empty queue before closing it — short by default: a fleet submits every shard's claim upfront, so there is no future submission worth waiting on the way a pool's IdleTimeout does")
		pollWait := fs.Int("poll-wait-seconds", 20, "SQS long-poll wait per claim (0..20)")
		visibilityTimeout := fs.Int("visibility-timeout", 900, "MUST match the run queue's own SQS VisibilityTimeout (set by fleetRun's CreateRunQueue call) — used to size this worker's heartbeat interval, not to configure the queue itself")
		_ = fs.Parse(os.Args[2:])
		if *runID == "" || *runnerPath == "" {
			fmt.Fprintln(os.Stderr, "error: --run-id and --runner-path required")
			os.Exit(2)
		}
		if err := runFleetWorker(context.Background(), fleetWorkerArgs{
			runID: *runID, region: *region, pythonBin: *pythonBin, runnerPath: *runnerPath,
			idleTimeout: *idleTimeout, pollWaitSeconds: int32(*pollWait),
			visibilityTimeout: *visibilityTimeout,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "warmd fleet error:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: warmd run --manifest s3://bucket/key")
	fmt.Fprintln(os.Stderr, "       warmd pool --model <name> --region <region> [--idle-timeout 30m]")
	fmt.Fprintln(os.Stderr, "       warmd fleet --run-id <id> --region <region> --runner-path <path> [--idle-timeout 1m]")
}

// poolWorkerArgs bundles `warmd pool`'s flags.
type poolWorkerArgs struct {
	model, region, pythonBin, runnerPath string
	idleTimeout                          time.Duration
	pollWaitSeconds                      int32
	visibilityTimeout                    int
}

// runPoolWorker starts a calque#100 sticky pool worker: claims from the
// model's SQS queue, drains each claim's batch against a resident
// warm.Supervisor (warm ONCE across many claims), writes results+summary to
// S3, and acks — until idle past idleTimeout, then closes the resident runner
// and exits (the instance's own idle-reaper/spored lifecycle handles
// terminating the instance itself; this process just stops claiming).
func runPoolWorker(ctx context.Context, a poolWorkerArgs) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(a.region))
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}
	sqsClient := sqs.NewFromConfig(cfg)
	s3Client := s3.NewFromConfig(cfg)

	q, err := calpool.OpenPoolQueue(ctx, sqsClient, a.model)
	if err != nil {
		return fmt.Errorf("open pool queue for model %q: %w", a.model, err)
	}

	sup := &warm.Supervisor{Python: a.pythonBin, Script: a.runnerPath, Leak: stderrLeaker{}}
	w := &calpool.Worker{
		Queue:      q,
		Fetcher:    &calpool.S3Manifests{Client: s3Client},
		Results:    &calpool.S3Results{Client: s3Client},
		Supervisor: sup,
		Config: calpool.WorkerConfig{
			Model: a.model, PollWaitSeconds: a.pollWaitSeconds, IdleTimeout: a.idleTimeout,
			VisibilityTimeout: a.visibilityTimeout, Log: os.Stderr,
		},
	}
	served, err := w.Run(ctx)
	fmt.Fprintf(os.Stderr, "pool worker for model %q served %d claim(s)\n", a.model, served)
	return err
}

// fleetWorkerDefaultIdleTimeout is far shorter than warmd pool's 30m default
// (calque#145): a fleet run submits every shard's claim upfront (fleetRun's
// D2), so once a worker's queue goes empty there is no FUTURE submission
// worth waiting on the way a long-lived pool's IdleTimeout is sized for —
// the worker should self-exit promptly so fleetRun's own explicit teardown
// (DrainFleetWorkers) isn't racing a worker that's still needlessly polling.
const fleetWorkerDefaultIdleTimeout = 1 * time.Minute

// fleetWorkerArgs bundles `warmd fleet`'s flags.
type fleetWorkerArgs struct {
	runID, region, pythonBin, runnerPath string
	idleTimeout                          time.Duration
	pollWaitSeconds                      int32
	visibilityTimeout                    int
}

// runFleetWorker starts a calque#145 fleet worker: claims shard manifests
// from the RUN's SQS queue (calpool.OpenRunQueue — run-scoped, unlike
// warmd pool's model-scoped queue), drains each against a resident
// warm.Supervisor (warm ONCE across every shard this worker serves, not
// once per shard, the entire point of #145), writes results+summary to S3,
// and acks — until idle past idleTimeout, then closes the resident runner
// and exits. Otherwise identical to runPoolWorker; the only real
// difference is which queue it opens and what Config.Model is checked
// against: a fleet worker only ever knows its run id (not a model — it's
// booted with --run-id, no --model flag exists here), so Config.Model is
// set to a.runID, and fleetRun's submitter must set every ClaimRef.Model
// to that SAME run id — Worker.runOne's affinity check (ref.Model !=
// w.Config.Model) then still catches a misrouted claim exactly the way it
// does for pool mode, just keyed by run id instead of model name.
func runFleetWorker(ctx context.Context, a fleetWorkerArgs) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(a.region))
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}
	sqsClient := sqs.NewFromConfig(cfg)
	s3Client := s3.NewFromConfig(cfg)

	q, err := calpool.OpenRunQueue(ctx, sqsClient, a.runID)
	if err != nil {
		return fmt.Errorf("open run queue for run %q: %w", a.runID, err)
	}

	sup := &warm.Supervisor{Python: a.pythonBin, Script: a.runnerPath, Leak: stderrLeaker{}}
	w := &calpool.Worker{
		Queue:      q,
		Fetcher:    &calpool.S3Manifests{Client: s3Client},
		Results:    &calpool.S3Results{Client: s3Client},
		Supervisor: sup,
		Config: calpool.WorkerConfig{
			Model: a.runID, PollWaitSeconds: a.pollWaitSeconds, IdleTimeout: a.idleTimeout,
			VisibilityTimeout: a.visibilityTimeout, Log: os.Stderr,
		},
	}
	served, err := w.Run(ctx)
	fmt.Fprintf(os.Stderr, "fleet worker for run %q served %d claim(s)\n", a.runID, served)
	return err
}

func runOnInstance(ctx context.Context, manifestURI string) error {
	bucket, key, err := parseS3URI(manifestURI)
	if err != nil {
		return err
	}

	// Build the S3 client for the BUCKET's region, not AWS_REGION (the compute
	// region). warmd runs on the GPU instance, whose region is wherever capacity
	// was — which may differ from the artifact bucket's region (e.g. eu-central-1
	// spot compute + us-east-1 bucket). Using AWS_REGION here read the manifest
	// with the wrong endpoint -> 301 PermanentRedirect. The default AWS_REGION
	// seeds the bucket-region lookup. Mirrors the control-plane fix.
	hintRegion := os.Getenv("AWS_REGION")
	s3c, err := calexec.NewS3ClientForBucket(ctx, bucket, hintRegion)
	if err != nil {
		return fmt.Errorf("s3 client for bucket %q: %w", bucket, err)
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

	// Start the spot-interruption-notice poller (calque#94, gap 2): polls IMDS
	// for the EC2 2-minute spot interruption warning while the run is in
	// flight. This is intentionally NOT a new recovery protocol — on detecting
	// a notice it just leaks it loudly (a distinct kind from an ordinary crash)
	// and lets the existing flow continue to its normal summary-write path.
	// The existing crash-restart/re-drive machinery (warm.Supervisor here,
	// fleetrun.go's shard re-drive at the control-plane layer) already handles
	// a box disappearing out from under a run; this just makes the CAUSE
	// distinguishable in the log rather than looking like an ordinary crash.
	interruptCtx, stopInterruptPoll := context.WithCancel(ctx)
	defer stopInterruptPoll()
	go pollSpotInterruption(interruptCtx, stderrLeaker{})

	started := time.Now()
	sink := &calexec.S3Sink{Client: s3c, Bucket: man.Bucket, Prefix: man.ResultPrefix}
	sup := &warm.Supervisor{
		Python: pyOr(man.PythonBin),
		Script: man.RunnerPath,
		Sink:   sink,
		Leak:   stderrLeaker{},
		Config: warm.Config{
			EnterBody: man.EnterBody, MethodBody: man.MethodBody, MethodArg: man.MethodArg,
			MethodArgs: man.MethodArgs, Starmap: man.Starmap, Extras: man.Extras, ExtraConsts: man.ExtraConsts,
			ExtraImports: man.ExtraImports, ExtraClasses: man.ExtraClasses, Secrets: man.Secrets,
			PayloadIsBase64Bytes: man.PayloadIsBase64Bytes, Base64ArgIndices: man.Base64ArgIndices,
		},
		Concurrency: man.Concurrency,
		BatchSize:   man.BatchSize,
	}
	failed, runErr := sup.Run(ctx, man.Items)
	ended := time.Now()

	// Commit volumes BACK to S3 after @method drains (§E, volume.commit()). This is
	// the write-back half of the volume plumbing — runs before terminate so a
	// mutated volume persists. Best-effort + logged: a commit failure is surfaced,
	// not silently swallowed, but it does not fail the run's results.
	for _, v := range man.VolumeCommit {
		fmt.Fprintf(os.Stderr, "committing volume %s -> %s\n", v.MountPath, v.URI)
		if out, cerr := exec.CommandContext(ctx, "aws", "s3", "sync", "--no-progress", v.MountPath, v.URI).CombinedOutput(); cerr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: volume commit %s -> %s failed: %v (%s)\n", v.MountPath, v.URI, cerr, out)
		}
	}

	// Stop the sampler and capture its JSON summary (SIGTERM -> it prints + exits).
	var occ calexec.OccupancyRaw
	if occStarted {
		_ = sampler.Process.Signal(syscall.SIGTERM)
		_ = sampler.Wait()
		_ = json.Unmarshal([]byte(strings.TrimSpace(occSummaryBuf.String())), &occ)
	}

	// Recompute occupancy over the INFERENCE WINDOW only (#71). The sampler's own
	// mean spans its whole life, which includes the one-time @enter model load — so it
	// understates steady-state fill and, perversely, DROPS when inference gets faster
	// (batch-32: 24x faster, 2% reported). Re-averaging the timestamped samples inside
	// warmd's inference spans gives the number the cost model actually wants; the load
	// is still paid for, via enter_seconds, which K amortizes separately.
	//
	// If windowing isn't possible (sampler absent, no timestamps from an older image,
	// or zero samples landed in the window) we KEEP the whole-run mean and leave Scope
	// saying whole_run — an unavailable measurement must degrade to a labeled weaker
	// number, never to a fabricated one.
	wholeRun := occ
	if wholeRun.Scope == "" {
		wholeRun.Scope = calexec.ScopeWholeRun
	}
	spans := toExecSpans(sup.InferenceSpans)
	inferSecs := calexec.SpansSeconds(spans)
	if occStarted {
		raw, rerr := os.ReadFile(occOut)
		switch {
		case rerr != nil:
			fmt.Fprintf(os.Stderr, "LEAK[integration_edge] occupancy samples unreadable (%v); "+
				"occupancy stays WHOLE-RUN (includes the @enter load, understates fill)\n", rerr)
		default:
			samples := calexec.ParseOccSamples(raw)
			if windowed, ok := calexec.OccupancyInWindows(samples, spans, occ.IntervalS); ok {
				occ = windowed
			} else {
				fmt.Fprintf(os.Stderr, "LEAK[unhandled_case] no occupancy samples fell inside the inference "+
					"window (%d parsed, %d span(s), %.1fs); occupancy stays WHOLE-RUN and is load-contaminated\n",
					len(samples), len(spans), inferSecs)
				occ = wholeRun
			}
		}
	}

	summary := Summary{
		EnterSeconds: sup.EnterSeconds, EnterCount: sup.EnterCount,
		PerItemSecs: sink.Seconds(), Failed: failed,
		Occupancy: occ, OccupancyWholeRun: wholeRun,
		InferenceSpans: sup.InferenceSpans, InferenceSeconds: inferSecs,
		StartedUnix: started.Unix(), EndedUnix: ended.Unix(),
	}
	if err := putJSON(ctx, s3c, man.Bucket, man.SummaryKey, summary); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return runErr
}

// toExecSpans converts the supervisor's spans to the exec package's shape (the two
// are deliberately separate types so the worker supervisor doesn't depend on the
// control-plane cost path).
func toExecSpans(in []warm.Span) []calexec.Span {
	out := make([]calexec.Span, len(in))
	for i, s := range in {
		out[i] = calexec.Span{StartUnix: s.StartUnix, EndUnix: s.EndUnix}
	}
	return out
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

// spotInterruptionURL is EC2's instance-metadata endpoint for the 2-minute
// spot interruption warning (calque#94). It 404s until an interruption is
// actually pending. A package-level var so a test can point it at an
// httptest.Server standing in for IMDS.
var spotInterruptionURL = "http://169.254.169.254/latest/meta-data/spot/instance-action"

// spotInterruptionPollInterval is how often pollSpotInterruption checks IMDS.
// A package-level var so a test can shrink it instead of waiting ~5s per poll.
var spotInterruptionPollInterval = 5 * time.Second

// spotLeaker is the subset of warm.Leaker's shape pollSpotInterruption needs
// (stderrLeaker satisfies it; kept minimal so a test can supply a stub without
// depending on warm-runner).
type spotLeaker interface {
	Leak(kind, detail string)
}

// pollSpotInterruption polls the EC2 spot-interruption-notice metadata
// endpoint every spotInterruptionPollInterval until ctx is done. The endpoint
// returns 404 until an interruption is actually pending, so any non-200
// response is treated as "no interruption yet" — never an error, never a
// retry storm. On the first 200 response it leaks a clearly distinct
// "spot_interruption" record (so it reads as a warning, not an ordinary
// crash) and keeps polling until ctx is cancelled — a repeated warning is
// harmless, and this deliberately does NOT layer any new early-exit/flush
// logic on top (calque#94's scope note: don't over-design; the existing
// crash-restart/re-drive machinery already handles the box disappearing).
func pollSpotInterruption(ctx context.Context, leak spotLeaker) {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(spotInterruptionPollInterval)
	defer ticker.Stop()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, spotInterruptionURL, nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				continue // no signal, not an error — IMDS may be transiently unreachable
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				continue // 404 (the common case): no interruption pending
			}
			if !warned {
				warned = true
				leak.Leak("spot_interruption", "EC2 spot interruption notice received (instance-action metadata returned 200) — "+
					"this box will be reclaimed within ~2 minutes; the run continues to its normal summary-write path (calque#94)")
			}
		}
	}
}
