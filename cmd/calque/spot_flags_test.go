package main

import (
	"testing"

	"github.com/spore-host/calque/internal/plan"
)

// TestParseSmokeArgs_SpotFlags verifies calque#94's --spot/--spot-max-price
// flags parse into smokeOpts correctly (both when set and when left at their
// defaults), following ramp.go's established --spot/--spot-max-price CLI
// pattern.
func TestParseSmokeArgs_SpotFlags(t *testing.T) {
	o, confirm, err := parseSmokeArgs([]string{
		"--bucket", "b", "--run-id", "r",
		"--spot", "--spot-max-price", "1.23",
		"--i-understand-this-spends-money",
	})
	if err != nil {
		t.Fatalf("parseSmokeArgs: %v", err)
	}
	if !confirm {
		t.Errorf("expected confirm=true")
	}
	if !o.spot {
		t.Errorf("expected o.spot=true")
	}
	if o.spotMaxPrice != "1.23" {
		t.Errorf("expected o.spotMaxPrice=%q, got %q", "1.23", o.spotMaxPrice)
	}
}

func TestParseSmokeArgs_SpotDefaultsOff(t *testing.T) {
	o, _, err := parseSmokeArgs([]string{"--bucket", "b", "--run-id", "r"})
	if err != nil {
		t.Fatalf("parseSmokeArgs: %v", err)
	}
	if o.spot {
		t.Errorf("expected o.spot=false by default")
	}
	if o.spotMaxPrice != "" {
		t.Errorf("expected o.spotMaxPrice=\"\" by default, got %q", o.spotMaxPrice)
	}
}

// TestParseRealArgs_SpotFlags mirrors TestParseSmokeArgs_SpotFlags for
// `calque real` (whose realOpts is shared with fleetRun — calque real
// --shards>1 IS the fleet path, so this single realOpts covers both
// realrun.go and fleetrun.go's flag wiring).
func TestParseRealArgs_SpotFlags(t *testing.T) {
	opts, shards, pool, confirm, err := parseRealArgs([]string{
		"--bucket", "b", "--run-id", "r", "--n", "10", "--shards", "3",
		"--spot", "--spot-max-price", "0.50",
		"--i-understand-this-spends-money",
	})
	if err != nil {
		t.Fatalf("parseRealArgs: %v", err)
	}
	if !confirm {
		t.Errorf("expected confirm=true")
	}
	if pool {
		t.Errorf("expected pool=false")
	}
	if shards != 3 {
		t.Errorf("expected shards=3, got %d", shards)
	}
	if !opts.spot {
		t.Errorf("expected opts.spot=true")
	}
	if opts.spotMaxPrice != "0.50" {
		t.Errorf("expected opts.spotMaxPrice=%q, got %q", "0.50", opts.spotMaxPrice)
	}
}

func TestParseRealArgs_SpotDefaultsOff(t *testing.T) {
	opts, _, _, _, err := parseRealArgs([]string{"--bucket", "b", "--run-id", "r"})
	if err != nil {
		t.Fatalf("parseRealArgs: %v", err)
	}
	if opts.spot {
		t.Errorf("expected opts.spot=false by default")
	}
	if opts.spotMaxPrice != "" {
		t.Errorf("expected opts.spotMaxPrice=\"\" by default, got %q", opts.spotMaxPrice)
	}
}

// TestSpawnLauncherBuild_CarriesSpotFields verifies that the same
// plan.SpawnLauncher{Spot: ..., SpotMaxPrice: ...} construction realrun.go,
// smoke.go, and fleetrun.go each now use (mirroring ramp.go's established
// pattern) produces a spawnaws.LaunchConfig with those fields set — the
// mechanical plumbing this issue's 3 CLI files all depend on, proven once
// against plan.SpawnLauncher directly rather than duplicated per-file.
func TestSpawnLauncherBuild_CarriesSpotFields(t *testing.T) {
	cfg := plan.SpawnLauncher{Spot: true, SpotMaxPrice: "0.75"}.Build()
	if !cfg.Spot {
		t.Errorf("expected built LaunchConfig.Spot=true")
	}
	if cfg.SpotMaxPrice != "0.75" {
		t.Errorf("expected built LaunchConfig.SpotMaxPrice=%q, got %q", "0.75", cfg.SpotMaxPrice)
	}
}

func TestSpawnLauncherBuild_SpotDefaultsOff(t *testing.T) {
	cfg := plan.SpawnLauncher{}.Build()
	if cfg.Spot {
		t.Errorf("expected built LaunchConfig.Spot=false by default")
	}
	if cfg.SpotMaxPrice != "" {
		t.Errorf("expected built LaunchConfig.SpotMaxPrice=\"\" by default, got %q", cfg.SpotMaxPrice)
	}
}
