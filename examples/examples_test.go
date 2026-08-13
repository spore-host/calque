// Package examples holds copies of the canonical fixtures from testdata/scripts,
// promoted here so they're discoverable as a documented learning path (see
// README.md). testdata/scripts stays the single source of truth — the parser
// tests and the spike spec reference those paths — so these are COPIES, and this
// test is the guard that keeps them byte-identical. If it fails, re-copy the
// testdata original over the examples/ file (never edit the copy in isolation).
package examples

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// examplesAreVerbatimCopies is the map of each example file to its canonical
// source under testdata/scripts. Add a row when you promote another fixture.
var copies = map[string]string{
	"map_batch_inference.py": "../testdata/scripts/map_batch_inference.py",
	"bedrock_eligible.py":    "../testdata/scripts/bedrock_eligible.py",
	"multi_gpu_train.py":     "../testdata/scripts/multi_gpu_train.py",
	"volume_cache.py":        "../testdata/scripts/volume_cache.py",
	"cross_app.py":           "../testdata/scripts/cross_app.py",
}

func TestExamplesMatchCanonicalFixtures(t *testing.T) {
	for example, source := range copies {
		example, source := example, source
		t.Run(example, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Clean(source))
			if err != nil {
				t.Fatalf("read canonical source %s: %v", source, err)
			}
			got, err := os.ReadFile(filepath.Clean(example))
			if err != nil {
				t.Fatalf("read example %s: %v", example, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s has drifted from %s — they must be byte-identical.\n"+
					"Re-copy: cp %s %s", example, source, source, example)
			}
		})
	}
}
