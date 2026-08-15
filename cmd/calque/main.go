// Command calque is the spike CLI (spec §12).
//
// Usage:
//
//	calque analyze <script.py> [<script.py> ...]        # static passes over a corpus
//	calque run [--n N] [--region R] [--dry-run] <script.py>   # parse -> warm exec -> cost model
//
// `run --dry-run` exercises every stage end-to-end WITHOUT launching a billable
// instance: it drives the warm worker locally on a synthetic sample and emits a
// cost verdict with its inputs honestly flagged measured|proxy. Dropping --dry-run
// (a real launch) is gated pending explicit authorization.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spore-host/calque/internal/gate"
	"github.com/spore-host/calque/internal/gpu"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
	"github.com/spore-host/calque/internal/plan"
	"github.com/spore-host/calque/internal/target"
)

// bedrockRegion defines "the catalog" for the gate's live fetch (spike default).
const bedrockRegion = "us-east-1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "analyze":
		if len(os.Args) < 3 {
			usage()
			os.Exit(2)
		}
		if err := analyze(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "run":
		if err := runCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "smoke":
		if err := smokeCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "real":
		if err := realCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "ramp":
		if err := rampCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "pool":
		if err := poolCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "spawn-run":
		if err := spawnRunCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "session":
		if err := sessionCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "version":
		printVersion()
	default:
		usage()
		os.Exit(2)
	}
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	n := fs.Int("n", 100000, "item count the verdict locates the user against")
	region := fs.String("region", "us-west-2", "AWS region for acquisition/pricing")
	dryRun := fs.Bool("dry-run", true, "exercise every stage without launching a billable instance (default true; real launch is gated)")
	rates := fs.String("rates", "config/rates.json", "path to the dated rate table")
	entrypoint := fs.String("entrypoint", "", "which @app.local_entrypoint() to select when the script has more than one (mimics `modal run file.py::entrypoint`, calque#90)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: calque run [--n N] [--region R] [--dry-run] [--entrypoint NAME] <script.py>")
	}
	return run(runOpts{script: fs.Arg(0), n: *n, region: *region, dryRun: *dryRun, ratesFP: *rates, entrypoint: *entrypoint})
}

// smokeCmd runs the acquire-only smoke test — the FIRST billable action. Gated
// behind an explicit --i-understand-this-spends-money flag so it can never fire
// by accident.
func smokeCmd(args []string) error {
	o, confirm, err := parseSmokeArgs(args)
	if err != nil {
		return err
	}
	if !confirm {
		return fmt.Errorf("refusing to launch: pass --i-understand-this-spends-money (this acquires a billable g7e)")
	}
	return smoke(o)
}

// parseSmokeArgs parses `calque smoke`'s flags into a smokeOpts (plus the
// separate --i-understand-this-spends-money confirmation, checked by the
// caller) without launching anything — split out from smokeCmd so flag
// wiring (in particular --spot/--spot-max-price, calque#94) is unit-testable
// on its own.
func parseSmokeArgs(args []string) (smokeOpts, bool, error) {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	bucket := fs.String("bucket", "", "S3 bucket for artifacts/results (required)")
	region := fs.String("region", "us-west-2", "AWS region")
	runID := fs.String("run-id", "", "unique run id (required; e.g. smoke-YYYYMMDD-HHMM)")
	ttl := fs.String("ttl", "30m", "instance TTL hard cap (spawn reaps at this)")
	deadlineMin := fs.Int("deadline-min", 20, "give up acquiring/waiting after N minutes")
	instance := fs.String("instance", "", "override instance type (capacity fallback, e.g. g6.2xlarge); empty => g7e.2xlarge")
	ami := fs.String("ami", "", "pin the AMI (spawn's GPU auto-select is broken); empty => let spawn choose")
	spot := fs.Bool("spot", false, "acquire on the Spot market (different capacity pool than on-demand; interruptible; K is then a SPOT rate)")
	spotMaxPrice := fs.String("spot-max-price", "", "spot bid cap in $/hr (empty => on-demand price)")
	confirm := fs.Bool("i-understand-this-spends-money", false, "required: launches a billable g7e")
	if err := fs.Parse(args); err != nil {
		return smokeOpts{}, false, err
	}
	if *bucket == "" || *runID == "" {
		return smokeOpts{}, false, fmt.Errorf("usage: calque smoke --bucket B --run-id ID [--region R] [--ttl 30m] [--spot] --i-understand-this-spends-money")
	}
	return smokeOpts{
		bucket: *bucket, region: *region, runID: *runID, ttl: *ttl,
		deadline: time.Duration(*deadlineMin) * time.Minute, instance: *instance, ami: *ami,
		spot: *spot, spotMaxPrice: *spotMaxPrice,
	}, *confirm, nil
}

// realCmd runs a REAL GPU inference run against actual acquired capacity.
// Gated behind --i-understand-this-spends-money (launches a billable GPU
// instance).
func realCmd(args []string) error {
	opts, shards, pool, confirm, err := parseRealArgs(args)
	if err != nil {
		return err
	}
	if !confirm {
		return fmt.Errorf("refusing to launch: pass --i-understand-this-spends-money")
	}
	if shards > opts.n {
		return fmt.Errorf("--shards %d exceeds --n %d (a shard would be empty)", shards, opts.n)
	}
	if pool {
		if shards > 1 {
			return fmt.Errorf("--pool and --shards>1 are mutually exclusive: a pool submission is one claim against one already-provisioned pool, not a fleet acquisition")
		}
		return realRunViaPool(opts)
	}
	// shards>1 fans out across a fleet (§15); shards<=1 is the single-instance path.
	return fleetRun(opts, shards)
}

// parseRealArgs parses `calque real`'s flags into a realOpts (plus shards,
// --pool, and the separate --i-understand-this-spends-money confirmation,
// checked by the caller) without launching anything — split out from realCmd
// so flag wiring (in particular --spot/--spot-max-price, calque#94) is
// unit-testable on its own.
// pipFlag implements flag.Value for a repeatable `--pip PACKAGE` flag —
// Go's flag package has no built-in repeatable/slice flag type, so this
// is the standard "collect into a slice, one flag.Var call per
// occurrence" pattern, mirroring secretsFlag's map-shaped sibling below.
type pipFlag struct{ packages *[]string }

func (p pipFlag) String() string {
	if p.packages == nil {
		return ""
	}
	return strings.Join(*p.packages, ",")
}

func (p pipFlag) Set(pkg string) error {
	*p.packages = append(*p.packages, pkg)
	return nil
}

// stageFileFlag implements flag.Value for a repeatable
// `--stage-file URL=PATH` flag, mirroring secretsFlag's map-shaped
// pattern.
type stageFileFlag map[string]string

func (s stageFileFlag) String() string {
	return fmt.Sprintf("%d staged file(s)", len(s))
}

func (s stageFileFlag) Set(kv string) error {
	url, path, ok := strings.Cut(kv, "=")
	if !ok || url == "" || path == "" {
		return fmt.Errorf("--stage-file must be URL=PATH, got %q", kv)
	}
	s[url] = path
	return nil
}

// intKeyedFlag implements flag.Value for a repeatable `--flag IDX=VALUE`
// flag keyed by a tuple position (int), used by --arg-file/--arg-json — the
// generic positional-args mechanism for a picked unit whose real signature
// mixes a bytes positional arg with non-bytes ones (e.g. AI-Almanac's
// run_benchmark_local(job_id: str, config: dict, input_bundle: bytes,
// runtime_env: dict | None)), where --item-file's single-whole-payload-is-
// bytes design doesn't apply. VALUE's own interpretation (a file path vs. a
// raw JSON literal) is up to the specific flag using this type, not this
// type itself.
type intKeyedFlag map[int]string

func (f intKeyedFlag) String() string {
	return fmt.Sprintf("%d indexed value(s)", len(f))
}

func (f intKeyedFlag) Set(kv string) error {
	idxStr, value, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("must be IDX=VALUE, got %q", kv)
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		return fmt.Errorf("IDX must be an integer, got %q: %w", idxStr, err)
	}
	if idx < 0 {
		return fmt.Errorf("IDX must be >= 0, got %d", idx)
	}
	if _, exists := f[idx]; exists {
		return fmt.Errorf("index %d given more than once", idx)
	}
	f[idx] = value
	return nil
}

// secretsFlag implements flag.Value for a repeatable `--secret NAME=VALUE`
// flag — Go's flag package has no built-in repeatable/map flag type, so
// this is the standard "collect into a map, one flag.Var call per
// occurrence" pattern.
type secretsFlag map[string]string

func (s secretsFlag) String() string {
	return fmt.Sprintf("%d secret(s)", len(s))
}

func (s secretsFlag) Set(kv string) error {
	name, value, ok := strings.Cut(kv, "=")
	if !ok || name == "" {
		return fmt.Errorf("--secret must be NAME=VALUE, got %q", kv)
	}
	s[name] = value
	return nil
}

func parseRealArgs(args []string) (opts realOpts, shards int, pool bool, confirm bool, err error) {
	fs := flag.NewFlagSet("real", flag.ExitOnError)
	bucket := fs.String("bucket", "", "S3 bucket (required)")
	region := fs.String("region", "us-east-1", "AWS region")
	runID := fs.String("run-id", "", "unique run id (required)")
	instance := fs.String("instance", "", "GPU instance type; empty => \"g6.2xlarge\", UNLESS --allow-card-swap substituted a different card, in which case an instance for that card is resolved automatically (calque#178)")
	ami := fs.String("ami", "", "pin the AMI; empty => spawn auto-selects a GPU-capable AMI (verified working on g6/g6e/g7/g7e, calque#75)")
	model := fs.String("model", "Qwen/Qwen2.5-1.5B-Instruct", "HF model repo id (must NOT be on Bedrock)")
	n := fs.Int("n", 1, "number of prompts (N=1 validates inference; N~100 for amortized K)")
	shardsFlag := fs.Int("shards", 1, "fan out .map across N single-node instances acquired in parallel (§15 fleet; 1 => single instance)")
	ttl := fs.String("ttl", "40m", "hard cap on EACH SHARD'S WHOLE RUNTIME (acquire+bootstrap+work), not just acquisition — spawn reaps the instance at this even mid-work (calque#141); for a fleet run set this generously above your expected per-shard work time, not just the acquire wait")
	deadlineMin := fs.Int("deadline-min", 40, "give up ONLY the acquire/wait-for-capacity phase after N minutes; unrelated to --ttl, which bounds the instance's total lifetime afterward (calque#141)")
	rates := fs.String("rates", "config/rates.json", "rate table path")
	poolFlag := fs.Bool("pool", false, "submit to the existing model pool (via `calque pool create --model M`) instead of self-acquiring a dedicated instance (calque#103)")
	spot := fs.Bool("spot", false, "acquire on the Spot market (different capacity pool than on-demand; interruptible; K is then a SPOT rate)")
	spotMaxPrice := fs.String("spot-max-price", "", "spot bid cap in $/hr (empty => on-demand price)")
	script := fs.String("script", "", "optional Modal script to parse for its REAL .map()/.starmap() iterable (calque#136); empty => today's synthesized-prompt items, unchanged")
	entrypoint := fs.String("entrypoint", "", "which @app.local_entrypoint() to select when --script has more than one (mimics `modal run file.py::entrypoint`, calque#90); required if --script has 2+ entrypoints")
	function := fs.String("function", "", "drive this specific @app.function/@cls method by NAME instead of pickWarmUnit's automatic entrypoint/`.map()`-preference scan — for a callable not reachable through any @app.local_entrypoint() at all (e.g. a sibling function only a different entrypoint invokes); takes priority over --entrypoint's own selection")
	secrets := secretsFlag{}
	fs.Var(secrets, "secret", "NAME=VALUE, repeatable — injected into the runner's environment before @enter runs (generic counterpart to Modal's secrets=[...], which was previously only recorded, never injected)")
	itemFile := fs.String("item-file", "", "path to a file whose raw bytes become the SINGLE real item driven through the picked unit's body (e.g. for a `def f(input_bundle: bytes)` signature) — mutually exclusive with --n's synthesized/literal items and with --arg-file/--arg-json")
	argFiles := intKeyedFlag{}
	fs.Var(argFiles, "arg-file", "IDX=PATH, repeatable — the picked unit's real signature is a tuple of positional args (like .starmap()); position IDX's arg becomes PATH's raw bytes, base64-decoded back to bytes on the runner side. For a signature mixing bytes with non-bytes args (e.g. `def f(job_id: str, config: dict, bundle: bytes)`), where --item-file's single-whole-payload-is-bytes design doesn't apply. Combine with --arg-json for the non-bytes positions; every position must be covered by exactly one of --arg-file/--arg-json.")
	argJSON := intKeyedFlag{}
	fs.Var(argJSON, "arg-json", "IDX=JSON, repeatable — position IDX's arg is this literal JSON value (e.g. a string, number, or object), unmarshaled and passed through unchanged. Sibling of --arg-file for the non-bytes positions of the same tuple.")
	var pipPackages []string
	fs.Var(pipFlag{&pipPackages}, "pip", "third-party Python package to install via uv on the instance before running a --script-picked unit's REAL body (calque#148), repeatable — needed when the script's own pip_install(...) chain wasn't statically resolvable (e.g. built via a factory function)")
	pythonVersion := fs.String("python-version", "", "Python version for uv to install on the instance (calque#148), e.g. 3.11 — only meaningful alongside --pip; empty lets uv pick its own default")
	stageFiles := stageFileFlag{}
	fs.Var(stageFiles, "stage-file", "URL=PATH, repeatable — downloads URL to the absolute PATH on the instance (parent dirs created) before warmd runs; for a script body that shells out to a hardcoded absolute path its original Docker image would have placed there")
	allowCardSwap := fs.Bool("allow-card-swap", false, "opt into target.CardSwapFor's curated substitution table (calque#178) for a CleanSwap gpu= site whose asked-for card has a VERIFIED cheaper alternative (e.g. A100-80GB, which AWS has no single-GPU instance for at all) — false (the default) always carries the script's own asked-for card through unchanged")
	confirmFlag := fs.Bool("i-understand-this-spends-money", false, "required: launches a billable GPU instance")
	if err := fs.Parse(args); err != nil {
		return realOpts{}, 0, false, false, err
	}
	if *bucket == "" || *runID == "" {
		return realOpts{}, 0, false, false, fmt.Errorf("usage: calque real --bucket B --run-id ID [--ami AMI] [--instance g6.2xlarge] [--model ...] [--n 1] [--shards 1] [--pool] [--spot] [--script FILE.py] [--entrypoint NAME] [--function NAME] [--secret NAME=VALUE] [--item-file PATH] [--arg-file IDX=PATH] [--arg-json IDX=JSON] [--pip PACKAGE] [--python-version X.Y] [--stage-file URL=PATH] [--allow-card-swap] --i-understand-this-spends-money")
	}
	opts = realOpts{
		bucket: *bucket, region: *region, runID: *runID, instance: *instance, ami: *ami,
		model: *model, n: *n, ttl: *ttl, deadline: time.Duration(*deadlineMin) * time.Minute, ratesFP: *rates,
		spot: *spot, spotMaxPrice: *spotMaxPrice, script: *script, entrypoint: *entrypoint, function: *function,
		pipPackages: pipPackages, pythonVersion: *pythonVersion,
		secrets: secrets, itemFile: *itemFile, argFiles: argFiles, argJSON: argJSON, stageFiles: stageFiles,
		allowCardSwap: *allowCardSwap,
	}
	return opts, *shardsFlag, *poolFlag, *confirmFlag, nil
}

// rampCmd runs the acquire-once / hold / run-many ramp — the efficient way
// to run the ramp: pay the (hard, slow) g7e acquisition once, hold the instance,
// run every rung on it via SSM. Gated behind --i-understand-this-spends-money.
func rampCmd(args []string) error {
	fs := flag.NewFlagSet("ramp", flag.ExitOnError)
	bucket := fs.String("bucket", "", "S3 bucket (required)")
	region := fs.String("region", "us-east-1", "AWS region")
	runID := fs.String("run-id", "", "unique session id (required)")
	instance := fs.String("instance", "g7e.2xlarge", "GPU instance type to hold")
	ami := fs.String("ami", "", "pin the AMI; empty => spawn auto-selects a GPU-capable AMI (verified working on g6/g6e/g7/g7e, calque#75)")
	model := fs.String("model", "Qwen/Qwen2.5-1.5B-Instruct", "HF model repo id (must NOT be on Bedrock)")
	rungsCSV := fs.String("rungs", "1,100,1000", "comma-separated N-ramp to run on the held instance")
	ttl := fs.String("ttl", "3h", "instance TTL hard cap (held across the whole ramp)")
	acquireMin := fs.Int("acquire-deadline-min", 180, "patient acquisition window in minutes ($0 until it lands)")
	rates := fs.String("rates", "config/rates.json", "rate table path")
	spot := fs.Bool("spot", false, "acquire on the Spot market (different capacity pool than on-demand; interruptible; K is then a SPOT rate)")
	spotMaxPrice := fs.String("spot-max-price", "", "spot bid cap in $/hr (empty => on-demand price)")
	fallbackRegionsCSV := fs.String("fallback-regions", "", "comma-separated regions to try (in order) if --region has no capacity (calque#95); empty => single-region only")
	prepMin := fs.Int("prep-timeout-min", 30, "minutes to wait for the one-time docker image pull before giving up")
	concurrency := fs.Int("concurrency", 1, "items in flight per rung for THREAD-SAFE bodies (guarded off for vLLM-offline; use --batch-size there)")
	batchSize := fs.Int("batch-size", 1, "items per micro-batch: one vLLM .generate(list) call fills the GPU — the real occupancy lever (1 = per-item)")
	script := fs.String("script", "", "optional Modal script to parse for its REAL .map()/.starmap() iterable (calque#136); empty => today's synthesized-prompt items, unchanged")
	confirm := fs.Bool("i-understand-this-spends-money", false, "required: launches a billable GPU instance held for hours")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bucket == "" || *runID == "" {
		return fmt.Errorf("usage: calque ramp --bucket B --run-id ID [--ami AMI] [--instance g7e.2xlarge] [--rungs 1,100,1000] [--spot] [--script FILE.py] --i-understand-this-spends-money")
	}
	if !*confirm {
		return fmt.Errorf("refusing to launch: pass --i-understand-this-spends-money (holds a billable GPU for up to the TTL)")
	}
	rungs, err := parseRungs(*rungsCSV)
	if err != nil {
		return err
	}
	return runRamp(rampOpts{
		bucket: *bucket, region: *region, runID: *runID, instance: *instance, ami: *ami,
		model: *model, rungs: rungs, ttl: *ttl,
		acquireDeadline: time.Duration(*acquireMin) * time.Minute, ratesFP: *rates,
		spot: *spot, spotMaxPrice: *spotMaxPrice,
		prepTimeout: time.Duration(*prepMin) * time.Minute,
		concurrency: *concurrency, batchSize: *batchSize,
		fallbackRegions: splitComma(*fallbackRegionsCSV),
		script:          *script,
	})
}

func parseRungs(csv string) ([]int, error) {
	var out []int
	for _, part := range splitComma(csv) {
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err != nil || n <= 0 {
			return nil, fmt.Errorf("bad rung %q", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no rungs")
	}
	return out, nil
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
		} else if r != ' ' {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  calque analyze <script.py> [...]")
	fmt.Fprintln(os.Stderr, "  calque run [--n N] [--region R] [--dry-run] <script.py>")
	fmt.Fprintln(os.Stderr, "  calque smoke --bucket B --run-id ID [--region R] [--ttl 30m] --i-understand-this-spends-money")
	fmt.Fprintln(os.Stderr, "  calque real --bucket B --run-id ID [--ami AMI] [--instance g6.2xlarge] [--model ...] [--n 1] [--shards 1] [--pool] --i-understand-this-spends-money")
	fmt.Fprintln(os.Stderr, "  calque ramp --bucket B --run-id ID [--ami AMI] [--instance g7e.2xlarge] [--rungs 1,100,1000] [--fallback-regions us-west-2,eu-central-1] --i-understand-this-spends-money")
	fmt.Fprintln(os.Stderr, "  calque pool create --model M --instance-type T --manifest-bucket B --results-bucket B --runner-path P [--workers N] --i-understand-this-spends-money")
	fmt.Fprintln(os.Stderr, "  calque spawn-run --bucket B --run-id ID --ami AMI [--instance m7i.large] <script.py> --i-understand-this-spends-money")
	fmt.Fprintln(os.Stderr, "  calque session <checkout|checkin|status|list> ... (institutional MIG/MPS slice check-out/check-in, docs/tenancy-vs-session.md)")
	fmt.Fprintln(os.Stderr, "  calque version")
}

// pyastDir locates the helper relative to the repo. We resolve it from this
// binary's module layout; override with CALQUE_PYAST_DIR for out-of-tree runs.
func pyastDir() string {
	if d := os.Getenv("CALQUE_PYAST_DIR"); d != "" {
		return d
	}
	return "tools/pyast"
}

func analyze(scripts []string) error {
	ctx := context.Background()
	runner, runnerArgs := parse.DefaultRunner(pyastDir())

	rep := &leak.Report{}
	corpus := gpu.Counts{}
	stub := target.StubRecommender{}

	// The Bedrock gate runs BEFORE recommend (§11). Fetch the live catalog once,
	// up front, and share it across the corpus. If the catalog is unreachable we
	// degrade to the gpu/leak passes rather than failing the whole analysis.
	var cat gate.Catalog
	if lc, err := gate.NewLiveCatalog(ctx, bedrockRegion); err != nil {
		fmt.Fprintf(os.Stderr, "warn: Bedrock catalog unavailable (%v); skipping gate\n", err)
	} else {
		cat = lc
	}

	// Authoritative HF->Bedrock mapping (hf-bedrock-map v1 reverse-lookup API):
	// curated + AWS-EULA-verified, preferred over the signature heuristic.
	// Best-effort — the gate degrades to signature-only if it's unreachable.
	var hfMap *gate.HFBedrockClient
	if hm, err := gate.NewHFBedrockClient(ctx, ""); err != nil {
		fmt.Fprintf(os.Stderr, "warn: hf-bedrock-map unavailable (%v); gate falls back to signature heuristic\n", err)
	} else {
		hfMap = hm
		fmt.Printf("hf-bedrock-map: API %s reachable (data generated %s)\n", hm.Version, hm.GeneratedAt)
	}
	var gateResults []gate.Result

	for _, s := range scripts {
		app, err := parse.Parse(ctx, s, rep, runner, runnerArgs...)
		if err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}

		// Gate first: is this work that should route AWAY from calque?
		if cat != nil {
			// Pass a true-nil interface when the client is nil, so the gate's
			// hfMap != nil guard works (a typed-nil pointer in an interface is
			// non-nil and would panic on Lookup).
			var hf gate.HFLookup
			if hfMap != nil {
				hf = hfMap
			}
			grs, err := gate.EvaluateWith(ctx, app, cat, hf, rep)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warn: gate failed for %s: %v\n", s, err)
			} else {
				gateResults = append(gateResults, grs...)
			}
		}

		log := gpu.RewriteApp(app, rep, false) // corpus analyze: informational only, not a real launch (calque#178)
		c := log.Counts()
		corpus.CleanSwaps += c.CleanSwaps
		corpus.FlagMulti += c.FlagMulti
		corpus.FlagCouple += c.FlagCouple
		corpus.NoGPU += c.NoGPU

		fmt.Printf("=== %s (app %q) ===\n", filepath.Base(s), app.Name)
		// calque#174: this is the app-wide DEFAULT image (the fallback a
		// callable with no image= of its own inherits) — not necessarily
		// what every function/class actually builds with. A function/class
		// with its OWN image= kwarg resolves independently; see each
		// gpu[fn] line's owner if you need to check a specific callable.
		fmt.Printf("  functions=%d classes=%d entrypoints=%d image.default.base=%q pip=%v\n",
			len(app.Functions), len(app.Classes), len(app.Entrypoints), app.Image.Base, app.Image.Pip)
		for _, sub := range log.Subs {
			// Every clean swap resolves its instance via the seam, never inlined.
			line := ""
			if sub.Substituted != "" {
				t := stub.Recommend(app, ir.Function{Name: sub.Owner})
				line = " -> " + t.Card
			}
			fmt.Printf("  gpu[%s]: %s requested=%q%s (%s)\n",
				sub.Owner, sub.Disposition, sub.Requested.Raw, line, sub.Reason)
		}
		// Volume -> S3 prefix mapping (§3): named Volumes become stable S3 prefixes,
		// synced to the mount path before @enter. Stable-by-name => cache reuse (§15).
		for _, m := range plan.ResolveVolumes(app, rep) {
			fmt.Printf("  volume: %q -> %s (mount %s, delta-sync => warm-cache reuse)\n",
				m.Name, m.S3Prefix, m.MountPath)
		}
	}

	if cat != nil {
		gate.Sort(gateResults)
		fmt.Println("\n--- Bedrock eligibility gate (§11) ---")
		for _, r := range gateResults {
			// The offer surface (G2/G4) is the single source of truth for a
			// route-away; render it when present, else explain why there's none.
			if o := r.Offer(); o != nil {
				fmt.Printf("  %s: %s", short(r.Script), o.Render())
				continue
			}
			switch r.ModelRef {
			case "":
				fmt.Printf("  %s: identity hidden (no repo id; %s shape) — cannot claim Bedrock match\n",
					short(r.Script), r.Shape)
			default:
				fmt.Printf("  %s: %s (%s shape) — no catalog match; legitimately calque's job\n",
					short(r.Script), r.ModelRef, r.Shape)
			}
		}
		fmt.Println("\n--- Bedrock census (§11/§16.4) ---")
		cb, _ := json.MarshalIndent(gate.Summarize(gateResults), "", "  ")
		fmt.Println(string(cb))
	}

	fmt.Println("\n--- corpus census (gpu guard, §7/§16.4) ---")
	b, _ := json.MarshalIndent(corpus, "", "  ")
	fmt.Println(string(b))

	fmt.Println("\n--- leak report (§10) ---")
	rep.Summary(os.Stdout)
	return nil
}

func short(p string) string { return filepath.Base(p) }
