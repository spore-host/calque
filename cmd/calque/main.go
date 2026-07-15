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

func usage() {
	fmt.Fprintln(os.Stderr, "usage: calque analyze <script.py> [...]   |   calque run [--n N] [--region R] [--dry-run] <script.py>")
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
	var gateResults []gate.Result

	for _, s := range scripts {
		app, err := parse.Parse(ctx, s, rep, runner, runnerArgs...)
		if err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}

		// Gate first: is this work that should route AWAY from calque?
		if cat != nil {
			grs, err := gate.Evaluate(ctx, app, cat, rep)
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
				fmt.Printf("  %s: EXACT %s + inference -> SUGGEST BEDROCK (%s), don't rent a GPU\n",
					short(r.Script), r.ModelRef, r.MatchID)
			case r.Tier == gate.TierNear:
				fmt.Printf("  %s: NEAR %s ~ %s [differs: %v] — offer, no quality claim\n",
					short(r.Script), r.ModelRef, r.MatchID, r.DiffAxes)
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
