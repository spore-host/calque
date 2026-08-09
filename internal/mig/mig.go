// Package mig implements calque#107's fixed-layout MIG slice provisioning:
// choosing a MIG profile ONCE at instance boot/prep time (mirroring
// cmd/calque/session.go's own "prep once" phase), then emitting the shell
// commands to enable MIG mode and carve the chosen layout — no live
// reconfiguration while slices may be in use (explicitly deferred, matching
// this issue's own scope).
//
// Profile data is sourced directly from LIVE hardware verification
// (calque#104, docs/gpu-sharing-support-matrix.md's "Live profiles"
// column) — nvidia-smi mig -lgip output on the actual AWS-deployed Server
// Edition cards — not vendor datasheets, which is exactly the gap that
// caused the earlier g7-MIG ambiguity in the first place.
package mig

import (
	"fmt"
	"strings"
)

// Profile is one MIG GPU-instance profile a card supports, as reported by
// `nvidia-smi mig -lgip`.
type Profile struct {
	// Name is nvidia-smi's own profile name, e.g. "1g.24gb" — passed verbatim
	// to `nvidia-smi mig -cgi <Name> -C` at provisioning time.
	Name string
	// MaxInstances is how many slices of this profile a single card can host
	// simultaneously (nvidia-smi mig -lgip's "Instances" column, the
	// denominator — e.g. "4/4" means MaxInstances=4).
	MaxInstances int
	// MemoryGiB is the profile's memory allocation per slice, as reported by
	// mig -lgip's Memory column.
	MemoryGiB float64
}

// profilesByFamily is the static MIG profile catalog, keyed by instance
// family, populated from live nvidia-smi mig -lgip output (calque#104). Only
// the BASE profile variants are listed (no +gfx/+me suffix variants,
// which mig -lgip also reports) — the base profile is the one calque
// provisions; the variants are alternate feature sets of the same
// memory/instance-count shape and are out of scope for a fixed, dumb layout
// pick (spec §1's "deliberately dumb" discipline, mirrored from
// plan.PickSmallest).
var profilesByFamily = map[string][]Profile{
	"g7": {
		{Name: "1g.16gb", MaxInstances: 2, MemoryGiB: 15.66},
		{Name: "2g.32gb", MaxInstances: 1, MemoryGiB: 31.41},
	},
	"g7e": {
		{Name: "1g.24gb", MaxInstances: 4, MemoryGiB: 23.62},
		{Name: "2g.48gb", MaxInstances: 2, MemoryGiB: 47.38},
		{Name: "4g.96gb", MaxInstances: 1, MemoryGiB: 95.00},
	},
}

// ProfilesFor returns the known MIG profiles for an instance family (e.g.
// "g7e" from "g7e.2xlarge" — callers pass the family, matching
// target.SharingModeFor's own convention). ok is false for a family with no
// MIG profiles cataloged (either it's not MIG-capable at all, per
// target.SharingModeFor, or the catalog hasn't been extended for it yet) —
// callers must not silently default to an empty layout.
func ProfilesFor(family string) ([]Profile, bool) {
	p, ok := profilesByFamily[family]
	return p, ok
}

// ErrNoProfiles means the family has no cataloged MIG profiles at all.
type ErrNoProfiles struct{ Family string }

func (e *ErrNoProfiles) Error() string {
	return fmt.Sprintf("mig: no MIG profiles cataloged for family %q", e.Family)
}

// ErrNoProfileFits means the family has cataloged MIG profiles, but none of
// them offers at least MinMemoryGiB of per-slice memory — i.e. every slice
// this card can be carved into is too small for the caller's workload.
type ErrNoProfileFits struct {
	Family       string
	MinMemoryGiB float64
}

func (e *ErrNoProfileFits) Error() string {
	return fmt.Sprintf("mig: no MIG profile for family %q offers >= %.2f GiB per slice", e.Family, e.MinMemoryGiB)
}

// PickLayout chooses ONE fixed layout for the whole card, at boot/prep time
// only (calque#107's own scope — no live reconfiguration).
//
// When minMemoryGiB is 0, it's "dumb" by design, mirroring
// plan.PickSmallest's own "deliberately dumb" philosophy (spec §1): it picks
// the profile with the MOST instances (maximizing the number of concurrent
// tenants a card can serve), breaking ties by the SMALLEST per-slice memory
// (the finer-grained split, since more/smaller slices is the more
// conservative multi-tenant default).
//
// When minMemoryGiB is > 0, it instead picks the SMALLEST profile (by
// MemoryGiB) that offers at least minMemoryGiB per slice — a memory-aware
// override for workloads that need a memory floor the "most tenants wins"
// default can't guarantee (e.g. a 30-40GB workload can't fit a 24GB slice
// just because the scheduler maximized slice count). If no profile in the
// family satisfies that floor, it returns *ErrNoProfileFits.
//
// Returns the profile plus the resulting slice count (== profile.MaxInstances,
// named separately for callers that want it without re-reading the struct).
func PickLayout(family string, minMemoryGiB float64) (Profile, int, error) {
	profiles, ok := ProfilesFor(family)
	if !ok || len(profiles) == 0 {
		return Profile{}, 0, &ErrNoProfiles{Family: family}
	}

	if minMemoryGiB > 0 {
		var best Profile
		found := false
		for _, p := range profiles {
			if p.MemoryGiB < minMemoryGiB {
				continue
			}
			if !found || p.MemoryGiB < best.MemoryGiB {
				best = p
				found = true
			}
		}
		if !found {
			return Profile{}, 0, &ErrNoProfileFits{Family: family, MinMemoryGiB: minMemoryGiB}
		}
		return best, best.MaxInstances, nil
	}

	best := profiles[0]
	for _, p := range profiles[1:] {
		switch {
		case p.MaxInstances > best.MaxInstances:
			best = p
		case p.MaxInstances == best.MaxInstances && p.MemoryGiB < best.MemoryGiB:
			best = p
		}
	}
	return best, best.MaxInstances, nil
}

// ProvisionScript emits the ONE-TIME boot/prep shell commands that enable
// MIG mode and carve `count` GPU-instances of the given profile — mirroring
// cmd/calque/session.go's own SessionPrep.PrepCommand shape: a bash script
// meant to run once via SSM/user-data before any workload starts, exiting 0
// on success. Enabling MIG mode requires a GPU reset (confirmed live,
// docs/gpu-sharing-support-matrix.md), so this script assumes it runs on a
// freshly-booted instance with no live workloads — the exact precondition
// this issue's "no live reconfiguration" scope exists to guarantee.
//
// gpuID is the target GPU's PCI bus ID (e.g. "0000:2F:00.0", from
// `nvidia-smi -L`/`nvidia-smi --query-gpu=pci.bus_id`) — nvidia-smi's MIG
// subcommands require naming the specific GPU on a multi-GPU instance, and
// even calque's single-GPU instances (g7.2xlarge, g7e.2xlarge) have exactly
// one, so this is never ambiguous in calque's current instance-size scope.
func ProvisionScript(gpuID string, profile Profile, count int) string {
	lines := []string{
		"#!/bin/bash",
		"set -euxo pipefail",
		fmt.Sprintf("sudo nvidia-smi -i %s -mig 1", gpuID),
		fmt.Sprintf("sudo nvidia-smi mig -i %s -cgi %s -C", gpuID, strings.Repeat(profile.Name+",", count-1)+profile.Name),
		"echo MIG_PROVISION_DONE",
	}
	return strings.Join(lines, "\n")
}

// SliceUUIDsScript emits the shell command to list the just-provisioned MIG
// GPU-instances' UUIDs (one per line) — the identifiers a tenancy.Registry
// binds users to (tenancy.Slice.ID) and warmd's own process launch passes
// via CUDA_VISIBLE_DEVICES. Separate from ProvisionScript because the
// UUIDs aren't known until AFTER `-cgi` runs (nvidia-smi assigns them), so
// a caller runs ProvisionScript, then this, then parses the output into
// tenancy.Slice values.
func SliceUUIDsScript(gpuID string) string {
	return fmt.Sprintf("nvidia-smi -i %s -L | grep -oE 'MIG-[0-9a-f-]+'", gpuID)
}
