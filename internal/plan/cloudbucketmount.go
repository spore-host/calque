package plan

import (
	"fmt"
	"sort"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// CloudBucketMount plumbing (calque#91 Workstream A).
//
// Modal's CloudBucketMount(bucket_name, ...), used inline as a volumes= value,
// mounts the USER'S OWN S3 bucket directly into the container via
// mountpoint-s3 — a fundamentally different shape from an ordinary
// Volume.from_name(...) mount (see volume.go): a Volume is calque's own
// staging area, synced through the run's --bucket; a CloudBucketMount is the
// script's OWN bucket, mounted live, with no calque-owned S3 prefix and no
// download/commit sync step. Real writes through mountpoint-s3 are already
// live against S3 — there is nothing to "commit" back the way a Volume needs.

// CloudBucketMountResolved is one resolved CloudBucketMount: the in-container
// mount path plus the real S3 bucket/prefix/read-only flag from the script's
// own CloudBucketMount(...) call.
type CloudBucketMountResolved struct {
	MountPath  string
	BucketName string
	KeyPrefix  string
	ReadOnly   bool
}

// ResolveCloudBucketMounts collects every modal.CloudBucketMount(...) mounted
// by the app's classes/functions, deduped by mount path — mirrors
// ResolveVolumes' exact collect/conflict/sort shape (see volume.go), applied
// to the sibling CloudBucketMounts map instead of Volumes. A mount path
// claimed by two DIFFERENT CloudBucketMounts (different bucket, prefix, or
// read-only flag) is a conflict we leak rather than guess through, same as
// ResolveVolumes.
func ResolveCloudBucketMounts(app ir.App, rep *leak.Report) []CloudBucketMountResolved {
	seen := map[string]ir.CloudBucketMount{} // mountPath -> resolved mount
	collect := func(owner string, mounts map[string]ir.CloudBucketMount, line int) {
		for mountPath, m := range mounts {
			if m.BucketName == "" {
				continue
			}
			if prev, ok := seen[mountPath]; ok && prev != m {
				rep.Addf(leak.PrimVolume, leak.KindUnhandledCase, app.Script, line,
					"%s: mount path %q maps to two different CloudBucketMounts (%+v vs %+v); mount overlap not modeled",
					owner, mountPath, prev, m)
				continue
			}
			seen[mountPath] = m
		}
	}
	for _, c := range app.Classes {
		collect(c.Name, c.CloudBucketMounts, c.Line)
	}
	for _, f := range app.Functions {
		collect(f.Name, f.CloudBucketMounts, f.Line)
	}

	// Deterministic order (stable plans / stable tests).
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]CloudBucketMountResolved, 0, len(paths))
	for _, p := range paths {
		m := seen[p]
		out = append(out, CloudBucketMountResolved{
			MountPath:  p,
			BucketName: m.BucketName,
			KeyPrefix:  m.KeyPrefix,
			ReadOnly:   m.ReadOnly,
		})
	}
	return out
}

// MountCommands returns the shell lines to mount each resolved
// CloudBucketMount via mountpoint-s3 (the AWS Labs FUSE driver Modal's own
// CloudBucketMount is itself built on) — idempotent install check matching
// this repo's existing `command -v X >/dev/null || (sudo apt-get update &&
// sudo apt-get install -y X)` idiom (internal/exec/bootstrap.go), then
// `mkdir -p` the mount path, then `mount-s3 <bucket> <mountpoint>` with
// `--read-only`/`--prefix <key_prefix>` when set. mountpoint-s3 ships as a
// .deb for Debian/Ubuntu-family AMIs (the DL AMI family this repo already
// targets); there is no apt package name for it (it's not in Debian/Ubuntu's
// own repos), so the fallback branch downloads AWS's published .deb directly
// instead of `apt-get install`.
func MountCommands(mounts []CloudBucketMountResolved) []string {
	var lines []string
	for _, m := range mounts {
		lines = append(lines,
			// mountpoint-s3 has no apt/dnf package; install AWS's own published
			// .deb the first time this runs (idempotent: skipped once mount-s3
			// is already on PATH).
			`command -v mount-s3 >/dev/null || (curl -LsSf -o /tmp/mount-s3.deb https://s3.amazonaws.com/mountpoint-s3-release/latest/x86_64/mount-s3.deb && sudo apt-get install -y /tmp/mount-s3.deb)`,
			fmt.Sprintf("mkdir -p %s", m.MountPath),
			mountCommand(m),
		)
	}
	return lines
}

// mountCommand renders one `mount-s3 <bucket> <mountpoint> [--prefix
// <key_prefix>] [--read-only]` invocation — mountpoint-s3's real CLI flags.
func mountCommand(m CloudBucketMountResolved) string {
	cmd := fmt.Sprintf("mount-s3 %s %s", m.BucketName, m.MountPath)
	if m.KeyPrefix != "" {
		cmd += fmt.Sprintf(" --prefix %s", m.KeyPrefix)
	}
	if m.ReadOnly {
		cmd += " --read-only"
	}
	return cmd
}
