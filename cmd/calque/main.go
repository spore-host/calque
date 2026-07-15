// Command calque is the spike CLI. For now it wires the *static* spine that
// needs no live AWS: parse -> gpu rewrite/guard -> leak + substitution report.
// The live stages (gate/plan/exec/measure/cost) attach here as they land.
//
// Usage:
//
//	calque analyze <script.py> [<script.py> ...]   # static passes over a corpus
//	calque run <script.py>                          # full pipeline (TODO: live stages)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spore-host/calque/internal/gpu"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
	"github.com/spore-host/calque/internal/target"
)

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(2)
	}
	cmd, scripts := os.Args[1], os.Args[2:]
	switch cmd {
	case "analyze":
		if err := analyze(scripts); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "run":
		fmt.Fprintln(os.Stderr, "run: live stages (plan/exec/measure/cost) not wired yet; use `analyze` for the static spine")
		os.Exit(2)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: calque analyze <script.py> [...]   |   calque run <script.py>")
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
	var apps []ir.App
	stub := target.StubRecommender{}

	for _, s := range scripts {
		app, err := parse.Parse(ctx, s, rep, runner, runnerArgs...)
		if err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
		apps = append(apps, app)

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

	fmt.Println("\n--- corpus census (gpu guard, §7/§16.4) ---")
	b, _ := json.MarshalIndent(corpus, "", "  ")
	fmt.Println(string(b))

	fmt.Println("\n--- leak report (§10) ---")
	rep.Summary(os.Stdout)
	return nil
}
