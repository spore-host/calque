package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// Volume plumbing (spec §3 "weights: Volume -> S3 prefix", §15 volume_cache.py).
//
// Modal's Volume is a named, persistent filesystem mounted into the container
// (Volume.from_name("llama-weights"), mounted at /weights). On AWS the structure-
// preserving translation is: each named Volume maps to a stable S3 prefix, synced
// down to the declared mount path on the worker before @enter runs. The two
// semantics the spec calls out:
//   - weight-cache REUSE across runs: the S3 prefix persists, so a second run with
//     a warm cache is a no-op sync, not a re-download (a re-download on no change
//     is a leak, §10).
//   - image/volume SEPARATION: weights live in the Volume (S3), never baked into
//     the container image — so the image stays small and weights are shared.

// VolumeMount is one resolved Modal Volume: its name, the S3 prefix it maps to,
// and the in-container path it mounts at.
type VolumeMount struct {
	Name      string // Modal volume name, e.g. "llama-3-8b-weights"
	S3Prefix  string // stable S3 prefix, e.g. "volumes/llama-3-8b-weights/"
	MountPath string // in-container path, e.g. "/weights"
}

// URI is the full s3:// location of the volume's contents in the given bucket.
func (v VolumeMount) URI(bucket string) string {
	return fmt.Sprintf("s3://%s/%s", bucket, strings.TrimSuffix(v.S3Prefix, "/"))
}

// ResolveVolumes maps every Modal Volume mounted by the app's GPU units to a
// stable S3 prefix + mount path. The prefix is derived deterministically from the
// volume NAME (not the run), so it PERSISTS across runs — that persistence is what
// gives weight-cache reuse (§15). A volume mounted at multiple paths, or multiple
// volumes at one path, is a conflict we leak rather than guess through.
func ResolveVolumes(app ir.App, rep *leak.Report) []VolumeMount {
	// mount path -> volume name, collected across classes and functions (that's
	// where volumes= actually mounts, per the IR).
	seen := map[string]string{} // mountPath -> volumeName
	collect := func(owner string, vols map[string]string, line int) {
		for mountPath, volName := range vols {
			if volName == "" {
				continue
			}
			if prev, ok := seen[mountPath]; ok && prev != volName {
				rep.Addf(leak.PrimVolume, leak.KindUnhandledCase, app.Script, line,
					"%s: mount path %q maps to two volumes (%q vs %q); volume overlap not modeled",
					owner, mountPath, prev, volName)
				continue
			}
			seen[mountPath] = volName
		}
	}
	for _, c := range app.Classes {
		collect(c.Name, c.Volumes, c.Line)
	}
	for _, f := range app.Functions {
		collect(f.Name, f.Volumes, f.Line)
	}

	// Deterministic order (stable plans / stable tests).
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]VolumeMount, 0, len(paths))
	for _, p := range paths {
		name := seen[p]
		out = append(out, VolumeMount{
			Name:      name,
			S3Prefix:  "volumes/" + sanitize(name) + "/",
			MountPath: p,
		})
	}
	return out
}

// SyncCommands returns the shell lines to stage each volume from S3 to its mount
// path before @enter. It uses `aws s3 sync` (not cp): sync only transfers changed
// objects, so a warm cache is a near-no-op — that's the weight-cache reuse
// semantic (§15), and it makes a "rebuild/re-download on no change" observably
// cheap rather than a silent full re-fetch (§10). Weights are pulled from the
// Volume's S3 prefix, NEVER from the image — image/volume separation.
func SyncCommands(bucket string, mounts []VolumeMount) []string {
	var lines []string
	for _, m := range mounts {
		lines = append(lines,
			fmt.Sprintf("mkdir -p %s", m.MountPath),
			// --no-progress keeps the bootstrap log readable; sync is idempotent and
			// only moves deltas (warm-cache reuse).
			fmt.Sprintf("aws s3 sync --no-progress %s %s", m.URI(bucket), m.MountPath),
		)
	}
	return lines
}

// sanitize makes a volume name safe as an S3 key segment (Modal names are already
// tame, but be defensive).
func sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
