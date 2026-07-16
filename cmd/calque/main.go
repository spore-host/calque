// Command calque is the spike CLI (spec §12).
//
// Usage:
//
//	calque analyze <script.py> [<script.py> ...]        # static passes over a corpus
//	calque run [--n N] [--region R] [--dry-run] <script.py>   # full pipeline -> crossover K
//
// `run --dry-run` exercises every stage end-to-end WITHOUT launching a billable
// instance: it drives the warm worker locally on a synthetic sample and emits a
// crossover K with its inputs honestly flagged measured|proxy. Dropping --dry-run
// (a real launch) is gated pending explicit authorization.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spore-host/calque/internal/gate"
	"github.com/spore-host/calque/internal/gpu"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
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
	case "session":
		if err := sessionCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: calque run [--n N] [--region R] [--dry-run] <script.py>")
	}
	return run(runOpts{script: fs.Arg(0), n: *n, region: *region, dryRun: *dryRun, ratesFP: *rates})
}

// smokeCmd runs the acquire-only smoke test — the FIRST billable action. Gated
// behind an explicit --i-understand-this-spends-money flag so it can never fire
// by accident.
func smokeCmd(args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	bucket := fs.String("bucket", "", "S3 bucket for artifacts/results (required)")
	region := fs.String("region", "us-west-2", "AWS region")
	runID := fs.String("run-id", "", "unique run id (required; e.g. smoke-YYYYMMDD-HHMM)")
	ttl := fs.String("ttl", "30m", "instance TTL hard cap (spawn reaps at this)")
	deadlineMin := fs.Int("deadline-min", 20, "give up acquiring/waiting after N minutes")
	instance := fs.String("instance", "", "override instance type (capacity fallback, e.g. g6.2xlarge); empty => g7e.2xlarge")
	ami := fs.String("ami", "", "pin the AMI (spawn's GPU auto-select is broken); empty => let spawn choose")
	confirm := fs.Bool("i-understand-this-spends-money", false, "required: launches a billable g7e")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bucket == "" || *runID == "" {
		return fmt.Errorf("usage: calque smoke --bucket B --run-id ID [--region R] [--ttl 30m] --i-understand-this-spends-money")
	}
	if !*confirm {
		return fmt.Errorf("refusing to launch: pass --i-understand-this-spends-money (this acquires a billable g7e)")
	}
	return smoke(smokeOpts{
		bucket: *bucket, region: *region, runID: *runID, ttl: *ttl,
		deadline: time.Duration(*deadlineMin) * time.Minute, instance: *instance, ami: *ami,
	})
}

// realCmd runs a REAL GPU inference run — the measured-K vehicle. Gated behind
// --i-understand-this-spends-money (launches a billable GPU instance).
func realCmd(args []string) error {
	fs := flag.NewFlagSet("real", flag.ExitOnError)
	bucket := fs.String("bucket", "", "S3 bucket (required)")
	region := fs.String("region", "us-east-1", "AWS region")
	runID := fs.String("run-id", "", "unique run id (required)")
	instance := fs.String("instance", "g6.2xlarge", "GPU instance type")
	ami := fs.String("ami", "", "pinned AMI (required for GPU; spawn auto-select is broken)")
	model := fs.String("model", "Qwen/Qwen2.5-1.5B-Instruct", "HF model repo id (must NOT be on Bedrock)")
	n := fs.Int("n", 1, "number of prompts (N=1 validates inference; N~100 for amortized K)")
	ttl := fs.String("ttl", "40m", "instance TTL hard cap")
	deadlineMin := fs.Int("deadline-min", 40, "give up acquiring/waiting after N minutes")
	rates := fs.String("rates", "config/rates.json", "rate table path")
	confirm := fs.Bool("i-understand-this-spends-money", false, "required: launches a billable GPU instance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bucket == "" || *runID == "" || *ami == "" {
		return fmt.Errorf("usage: calque real --bucket B --run-id ID --ami AMI [--instance g6.2xlarge] [--model ...] [--n 1] --i-understand-this-spends-money")
	}
	if !*confirm {
		return fmt.Errorf("refusing to launch: pass --i-understand-this-spends-money")
	}
	return realRun(realOpts{
		bucket: *bucket, region: *region, runID: *runID, instance: *instance, ami: *ami,
		model: *model, n: *n, ttl: *ttl, deadline: time.Duration(*deadlineMin) * time.Minute, ratesFP: *rates,
	})
}

// sessionCmd runs the acquire-once / hold / run-many session — the efficient way
// to run the ramp: pay the (hard, slow) g7e acquisition once, hold the instance,
// run every rung on it via SSM. Gated behind --i-understand-this-spends-money.
func sessionCmd(args []string) error {
	fs := flag.NewFlagSet("session", flag.ExitOnError)
	bucket := fs.String("bucket", "", "S3 bucket (required)")
	region := fs.String("region", "us-east-1", "AWS region")
	runID := fs.String("run-id", "", "unique session id (required)")
	instance := fs.String("instance", "g7e.2xlarge", "GPU instance type to hold")
	ami := fs.String("ami", "", "pinned AMI (required for GPU)")
	model := fs.String("model", "Qwen/Qwen2.5-1.5B-Instruct", "HF model repo id (must NOT be on Bedrock)")
	rungsCSV := fs.String("rungs", "1,100,1000", "comma-separated N-ramp to run on the held instance")
	ttl := fs.String("ttl", "3h", "instance TTL hard cap (held across the whole ramp)")
	acquireMin := fs.Int("acquire-deadline-min", 180, "patient acquisition window in minutes ($0 until it lands)")
	rates := fs.String("rates", "config/rates.json", "rate table path")
	confirm := fs.Bool("i-understand-this-spends-money", false, "required: launches a billable GPU instance held for hours")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bucket == "" || *runID == "" || *ami == "" {
		return fmt.Errorf("usage: calque session --bucket B --run-id ID --ami AMI [--instance g7e.2xlarge] [--rungs 1,100,1000] --i-understand-this-spends-money")
	}
	if !*confirm {
		return fmt.Errorf("refusing to launch: pass --i-understand-this-spends-money (holds a billable GPU for up to the TTL)")
	}
	rungs, err := parseRungs(*rungsCSV)
	if err != nil {
		return err
	}
	return runSession(sessionOpts{
		bucket: *bucket, region: *region, runID: *runID, instance: *instance, ami: *ami,
		model: *model, rungs: rungs, ttl: *ttl,
		acquireDeadline: time.Duration(*acquireMin) * time.Minute, ratesFP: *rates,
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

		log := gpu.RewriteApp(app, rep)
		c := log.Counts()
		corpus.CleanSwaps += c.CleanSwaps
		corpus.FlagMulti += c.FlagMulti
		corpus.FlagCouple += c.FlagCouple
		corpus.NoGPU += c.NoGPU

		fmt.Printf("=== %s (app %q) ===\n", filepath.Base(s), app.Name)
		fmt.Printf("  functions=%d classes=%d entrypoint=%v image.base=%q pip=%v\n",
			len(app.Functions), len(app.Classes), app.Entrypoint != nil, app.Image.Base, app.Image.Pip)
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
	}

	if cat != nil {
		gate.Sort(gateResults)
		fmt.Println("\n--- Bedrock eligibility gate (§11) ---")
		for _, r := range gateResults {
			switch {
			case r.ModelRef == "":
				fmt.Printf("  %s: identity hidden (no repo id; %s shape) — cannot claim Bedrock match\n",
					short(r.Script), r.Shape)
			case r.Eligible:
				fmt.Printf("  %s: EXACT %s + inference -> SUGGEST BEDROCK (%s) [%s], don't rent a GPU\n",
					short(r.Script), r.ModelRef, r.MatchID, r.Source)
				if r.Evidence != "" {
					fmt.Printf("        evidence: %s\n", r.Evidence)
				}
				if len(r.Regions) > 0 {
					fmt.Printf("        Bedrock regions: %v\n", r.Regions)
				}
			case r.Tier == gate.TierNear:
				fmt.Printf("  %s: NEAR %s ~ %s [%s; differs: %v] — offer, no quality claim\n",
					short(r.Script), r.ModelRef, r.MatchID, r.Source, r.DiffAxes)
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
