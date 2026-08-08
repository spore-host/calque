package exec

import (
	"fmt"

	"github.com/spore-host/calque/internal/ir"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// SpawnCallable is one .spawn()-classified callable resolved back to its
// definition (calque#111): the glue #97/#110's design docs identified as
// missing — nothing in shard.go/orchestrate.go/fleetrun.go resolves a
// .spawn() call site's target name to the ACTUAL EnterBody/MethodBody/GPU/
// Config a driver needs to run it. Key mirrors NamedShard.Key (calque#110)
// so a driver can go straight from ResolveSpawnCallables' output to
// BuildSpawnManifests without a separate name-matching step.
type SpawnCallable struct {
	Key        string // the callable's name — also NamedShard.Key once sharded
	EnterBody  string // "" for a plain @app.function (calque#80's synthetic-class shape, no @enter)
	MethodBody string
	MethodArg  string
	GPU        string
	IsClass    bool // true if this callable is an @app.cls (EnterBody/MethodArg came from its .map'd/first method)
}

// ResolveSpawnCallables walks app's Functions and Classes, returning one
// SpawnCallable for every callable classified ir.InvokeSpawn anywhere in the
// app — a plain @app.function directly, or an @app.cls's method (the class'
// EnterBody + the SPECIFIC method's Body, matching how pickWarmUnit already
// treats a class: EnterBody runs once, the method drains items).
//
// This does NOT use ir.App.FindFunction/FindClass's single-name lookup in a
// loop over an EXTERNAL list of names — invokes (parse.go's own
// map[string]ir.InvokeKind) is already discarded by the time an ir.App
// exists, so the only way to find every spawned callable is to scan
// app.Functions/app.Classes directly and read each one's own Invoke field.
// FindFunction/FindClass remain useful for a driver that already has a
// call-site's target NAME (e.g. from a leak or a future call-graph pass) and
// wants that one callable's body — this function is the "give me ALL of
// them" counterpart calque#111 asks for.
func ResolveSpawnCallables(app ir.App) []SpawnCallable {
	var out []SpawnCallable
	for _, f := range app.Functions {
		if f.Invoke != ir.InvokeSpawn {
			continue
		}
		out = append(out, SpawnCallable{
			Key: f.Name, MethodBody: f.Body, MethodArg: f.ItemArg, GPU: f.GPU,
		})
	}
	for _, c := range app.Classes {
		for _, m := range c.Methods {
			if m.Invoke != ir.InvokeSpawn {
				continue
			}
			out = append(out, SpawnCallable{
				Key: m.Name, EnterBody: c.EnterBody, MethodBody: m.Body, MethodArg: m.ItemArg,
				GPU: c.GPU, IsClass: true,
			})
		}
	}
	return out
}

// SpawnCallSite is one .spawn() call site's arguments, keyed by the
// TARGET callable's name (matching SpawnCallable.Key) — the actual
// payload(s) a real fan-out driver sends, as opposed to
// ResolveSpawnCallables' callable-definition metadata. A callable spawned
// more than once (e.g. inside a loop) has multiple entries under the same
// key; each becomes its own warm.Item within that callable's NamedShard.
type SpawnCallSite struct {
	Target string
	Args   []*string
}

// BuildSpawnManifests turns ResolveSpawnCallables' output plus the actual
// call-site arguments into one NamedShard per SPAWNED CALLABLE (calque#110's
// NamedShard, keyed the same way), each with ITS OWN EnterBody/MethodBody —
// replacing fleetrun.go's shared realEnterBody/realMethodBody constants with
// per-callable bodies, per this issue's own "Do" section. layout derives
// each shard's S3 keys via ShardLayout, reusing that unchanged.
//
// A callable with no matching call sites in sites (defensive: parse.go's
// invocationKinds classifies from call sites, so this should not normally
// happen) is skipped with no shard emitted — nothing to run.
func BuildSpawnManifests(callables []SpawnCallable, sites []SpawnCallSite, base, artifactPfx string) []NamedShard {
	byTarget := map[string][]SpawnCallSite{}
	for _, s := range sites {
		byTarget[s.Target] = append(byTarget[s.Target], s)
	}

	out := make([]NamedShard, 0, len(callables))
	for _, c := range callables {
		callSites := byTarget[c.Key]
		if len(callSites) == 0 {
			continue
		}
		items := make([]warm.Item, len(callSites))
		for i, cs := range callSites {
			items[i] = warm.Item{Index: i, Payload: spawnArgsPayload(cs.Args)}
		}
		mk, rp, sk, lk := ShardLayout(base, artifactPfx, c.Key)
		out = append(out, NamedShard{
			Key: c.Key, Items: items,
			ManifestKey: mk, ResultPrefix: rp, SummaryKey: sk, LogKey: lk,
		})
	}
	return out
}

// spawnArgsPayload converts a call site's raw string args into the payload
// shape warmd's runner.py expects: a single value for one arg (matching the
// existing single-positional-arg protocol every other invocation idiom
// already uses, per §6), or a list for multiple args (best-effort — a
// .spawn()'d callable with more than one positional arg has no established
// binding convention yet in calque's protocol; this is the same single-arg
// limitation checkInvokeSupport already flags for .starmap, not a NEW gap
// this function introduces).
func spawnArgsPayload(args []*string) any {
	switch len(args) {
	case 0:
		return nil
	case 1:
		if args[0] == nil {
			return nil
		}
		return *args[0]
	default:
		vals := make([]string, len(args))
		for i, a := range args {
			if a != nil {
				vals[i] = *a
			}
		}
		return vals
	}
}

// ErrCallableNotFound is returned by ResolveOneSpawnCallable when a name
// resolves to neither a Function nor a Class in app — surfaced so a caller
// working from a call-site target string (rather than ResolveSpawnCallables'
// own already-classified list) gets an explicit error instead of a zero
// value that looks like "found but empty."
type ErrCallableNotFound struct{ Name string }

func (e *ErrCallableNotFound) Error() string {
	return fmt.Sprintf("exec: no Function or Class named %q in app", e.Name)
}

// ResolveOneSpawnCallable is ir.App.FindFunction/FindClass composed into the
// single SpawnCallable shape ResolveSpawnCallables' entries already have —
// for a caller that has ONE call-site target name (not the full
// already-classified list) and wants that callable's body. This is the
// direct FindFunction/FindClass use this issue's title names; kept as a
// separate entry point from ResolveSpawnCallables (which scans ALL
// callables at once) because the two have different callers: a full-app
// driver wants "every spawned callable" (ResolveSpawnCallables); a
// leak/call-graph-driven caller wants "resolve THIS one name."
func ResolveOneSpawnCallable(app ir.App, name string) (SpawnCallable, error) {
	if f, ok := app.FindFunction(name); ok {
		return SpawnCallable{Key: f.Name, MethodBody: f.Body, MethodArg: f.ItemArg, GPU: f.GPU}, nil
	}
	if c, ok := app.FindClass(name); ok {
		var mBody, mArg string
		for _, m := range c.Methods {
			if m.IsMap {
				mBody, mArg = m.Body, m.ItemArg
				break
			}
		}
		if mBody == "" && len(c.Methods) > 0 {
			mBody, mArg = c.Methods[0].Body, c.Methods[0].ItemArg
		}
		return SpawnCallable{Key: c.Name, EnterBody: c.EnterBody, MethodBody: mBody, MethodArg: mArg, GPU: c.GPU, IsClass: true}, nil
	}
	return SpawnCallable{}, &ErrCallableNotFound{Name: name}
}
