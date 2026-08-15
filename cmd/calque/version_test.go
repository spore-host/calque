package main

import (
	"testing"
)

// TestVersionDefaultsToDev proves the -ldflags-injected variables have a
// harmless, honest fallback for `go build` without -ldflags (a source
// checkout, or a CI job that isn't goreleaser) -- "dev"/"unknown", never
// an empty string a release-detection script might misread as "unset".
func TestVersionDefaultsToDev(t *testing.T) {
	if Version == "" {
		t.Error("Version defaults to empty string, want a non-empty placeholder like \"dev\"")
	}
	if GitCommit == "" {
		t.Error("GitCommit defaults to empty string, want a non-empty placeholder like \"unknown\"")
	}
	if BuildDate == "" {
		t.Error("BuildDate defaults to empty string, want a non-empty placeholder like \"unknown\"")
	}
}
