// Package ir holds the six-primitive intermediate representation (spec §14):
// a struct-shaped transcription of what a Modal script's decorators said.
//
// Nothing clever lives here. Decorators are CONFIGURATION and are transcribed;
// function/method BODIES are PAYLOAD and are carried verbatim as strings, never
// interpreted — they ship to the worker and run under Python exactly as on Modal.
package ir

// App is a whole Modal application, transcribed from its decorators.
type App struct {
	Name    string            // modal.App("name")
	Image   Image             // resolved image for the app (see parse.resolveImage)
	Volumes map[string]string // module-level Volume var name -> Modal volume name (from_name)
	// CommittedVolumes is the set of module-level Volume var names (keys of
	// Volumes above) that the script calls .commit() on somewhere — a
	// real-AWS run (calque real/fleetrun) syncs THESE volumes back to S3
	// after @method drains (§E), mirroring Modal's own persistence
	// semantics; a volume never .commit()'d stays download-only (synced
	// before @enter, never written back — matches Modal, where an
	// uncommitted local write is never visible to another container).
	CommittedVolumes map[string]bool
	// DefaultVolumes/DefaultSecrets are App(volumes=..., secrets=...)'s own
	// kwargs (calque#168) — a Function/Class declaring neither inherits
	// from here, the same fallback-if-own-is-empty shape buildClass already
	// uses for a method inheriting its class's gpu=/volumes=. Before this,
	// App-level volumes=/secrets= were silently dropped with NO leak at
	// all — worse than image=, which at least surfaced a generic leak.
	DefaultVolumes map[string]string
	DefaultSecrets []string
	Functions      []Function // @app.function
	Classes        []Class    // @app.cls
	Entrypoints    []Function // @app.local_entrypoint (may be more than one)
	Script         string     // source path, for leak attribution (§10)
	// EntrypointInvokes attributes invoke-kind evidence to the SPECIFIC
	// @app.local_entrypoint() whose body contains the call site (calque#98):
	// entrypoint name -> invoked callable's leaf name -> best InvokeKind seen
	// at a call site nested inside that entrypoint's own body. A callable
	// only appears under an entrypoint here if a recognized call site
	// (.map/.starmap/.for_each/.remote/.spawn) inside that entrypoint's body
	// targets it — this is what lets pickWarmUnit (cmd/calque/run.go) ask
	// "what does entrypoint X specifically invoke?" instead of only ever
	// seeing the whole-script-flat union every Function/Class's own
	// Invoke/IsMap field already carries (that whole-script field is
	// unchanged and still the right source of truth for 0-or-1-entrypoint
	// scripts, where there is no ambiguity to resolve). Nil/missing for a
	// script with no entrypoints, or an entrypoint with no recognized call
	// sites of its own.
	EntrypointInvokes map[string]map[string]InvokeKind
	// ModuleConsts is every module-level `NAME = <literal-or-expression>`
	// assignment, keyed by name (calque#139) — real Modal code commonly
	// reads a bare module-level constant from inside an @enter/@method body
	// (e.g. `self.prefix = GREETING`) with no .local() in sight, since it's
	// never registered as an @app.function to begin with. collectLocalExtras
	// (cmd/calque/run.go) resolves a Function's/Class's FreeRefs/
	// EnterFreeRefs against this map (and ModuleFuncs below) the same way it
	// already resolves LocalCalls against FindFunction.
	ModuleConsts map[string]ModuleConst
	// ModuleFuncs carries every module-level function/method, keyed by name
	// (calque#139) — INCLUDING a plain, undecorated helper like `_format`
	// that is never an @app.function and so never appears in Functions.
	// Real Modal scripts overwhelmingly reference such helpers via a bare
	// call, not `.local()`; FreeRefs/EnterFreeRefs may name one of these
	// instead of (or in addition to) an entry in Functions.
	ModuleFuncs map[string]ModuleFunc
	// ModuleImports is the verbatim source of every module-level `import X`
	// / `from X import Y` statement, keyed by EACH name it binds (calque#146)
	// — a bare reference to an imported name (e.g. `Path(...)` after `from
	// pathlib import Path`, or `modal.Volume.from_name(...)` after `import
	// modal`) inside an @enter/@method body was previously an unconditional
	// NameError on execution: calque#139 shipped bare-referenced functions/
	// constants but explicitly did NOT resolve imports. A plain top-level
	// import THIS script does itself is unambiguous (unlike a re-exported
	// name from another module, which stays a leak, not a false positive)
	// and is shippable the same way a module-level constant already is.
	// collectLocalExtras (cmd/calque/run.go) resolves a Function's/Class's
	// FreeRefs/EnterFreeRefs against this map too, alongside ModuleFuncs/
	// ModuleConsts.
	ModuleImports map[string]string
	// ModuleClasses is every PLAIN (non-`@app.cls`) module-level class,
	// keyed by name (calque#147) — an ordinary helper class (e.g. a
	// log-tee context manager) a picked unit's body bare-instantiates,
	// e.g. `_LogTee(sys.stdout, log_buffer)`. Distinct from Classes above
	// (those are Modal's own `@app.cls` execution units, already modeled
	// structurally) — this is the FOURTH shippable free-reference target,
	// alongside ModuleFuncs/ModuleConsts/ModuleImports.
	ModuleClasses map[string]ModuleClass
}

// ModuleFunc is one module-level function's shippable shape (calque#139):
// its own parameter names and verbatim body, plus its OWN LocalCalls/
// FreeRefs (a bare helper can itself reference a sibling helper/constant),
// so collectLocalExtras' transitive-closure walk can enqueue through it
// exactly as it already does through a plain @app.function's LocalCalls.
type ModuleFunc struct {
	Args       []string
	Body       string
	LocalCalls []string
	FreeRefs   []string
}

// ModuleConst is one module-level constant's shippable shape (calque#146.2):
// its verbatim source plus its OWN FreeRefs — a constant's RHS can itself
// reference an import or another constant (e.g. `forecast_volume =
// modal.Volume.from_name(...)` needs `import modal` shipped too), so
// collectLocalExtras' transitive-closure walk must be able to enqueue
// THROUGH a shipped constant, not just stop at it.
type ModuleConst struct {
	Source   string
	FreeRefs []string
	// UnshippableConstruct is non-empty (e.g. "modal.Dict") when this
	// constant's RHS is a live-Modal-control-plane construct
	// (calque#151) — Dict/Queue/NetworkFileSystem.from_name(...). A bare
	// reference resolving here must be refused, not shipped verbatim: the
	// runner has no live Modal credentials, so exec'ing it crashes with a
	// confusing SDK auth error instead of an honest leak.
	UnshippableConstruct string
}

// ModuleClass is one PLAIN (non-`@app.cls`) module-level class's shippable
// shape (calque#147): its verbatim source (whole class body, methods
// included) plus its OWN FreeRefs — mirrors ModuleConst's exact shape,
// since the exec mechanics (verbatim source, exec'd into shared globals)
// are identical for a class statement and a constant assignment.
type ModuleClass struct {
	Source   string
	FreeRefs []string
}

// FindFunction looks up a plain @app.function by name (calque#88: correlating
// a .spawn() call site's target string back to its definition). Linear scan,
// matching every other app.Functions consumer's style — no name index exists
// or is warranted at typical script sizes.
func (a App) FindFunction(name string) (Function, bool) {
	for _, f := range a.Functions {
		if f.Name == name {
			return f, true
		}
	}
	return Function{}, false
}

// FindClass looks up an @app.cls by name (calque#88), same rationale as
// FindFunction.
func (a App) FindClass(name string) (Class, bool) {
	for _, c := range a.Classes {
		if c.Name == name {
			return c, true
		}
	}
	return Class{}, false
}

// Image is the .image DSL chain (§13). Base+Pip cover the common case called out
// in §14; Steps carries the full ordered chain losslessly for the Dockerfile
// generator (§image) so nothing has to be re-parsed downstream.
type Image struct {
	Base       string      // "debian_slim" | "from_registry" | ... ("" if unresolved)
	Pip        []string    // flattened pip_install + uv_pip_install packages
	Steps      []ImageStep // full DSL chain, in call order
	Unresolved bool        // image rooted at a variable we could not resolve to a base
}

// ImageStep is one verb in the .image chain, e.g. .pip_install("vllm","torch").
type ImageStep struct {
	Method string   // "debian_slim" | "pip_install" | "uv_pip_install" | "apt_install" | "run_commands" | ...
	Args   []string // string args (packages/commands); non-literal args are dropped with a leak
}

// Config carries the PORTABLE decorator kwargs beyond gpu/volumes/timeout (§B):
// resource sizing + reliability + placement config that Modal expresses on
// @function/@cls and that has a defensible AWS analog. Autoscaling/warm-pool
// kwargs (concurrency_limit, keep_warm, min/max_containers) are deliberately NOT
// here — they belong behind the seam (§4) and are recognized+leaked (M10/S1).
type Config struct {
	CPU      float64  // cpu= cores requested (0 if unset)
	MemoryMB int      // memory= in MB (0 if unset)
	Retries  int      // retries= (re-drive cap; 0 if unset)
	Secrets  []string // secret names referenced (recorded; not honored in the spike)
	// schedule= (e.g. "0 * * * *"); recorded, not honored. Also recognizes the
	// modal.Cron(cron_string, timezone=)/modal.Period(days=,hours=,minutes=,
	// seconds=) object forms (calque#91): Cron's cron string lands here verbatim
	// (timezone= is discarded, matching the bare-string form); Period's kwargs
	// are additive and get normalized to a single "<n>d<n>h<n>m<n>s" string,
	// omitting any zero/absent unit (e.g. days=1,hours=6 -> "1d6h").
	Schedule string
	Region   string // region= placement hint; recorded, not honored
	Cloud    string // cloud= ("aws"/"gcp"/"oci"/"auto"); recorded, not honored (calque#91)
}

// CloudBucketMount is one modal.CloudBucketMount(...) used inline as a
// volumes= value (calque#91) — mounts the USER'S OWN S3 bucket directly
// via mountpoint-s3, not calque's own --bucket staging area the way an
// ordinary Volume does.
type CloudBucketMount struct {
	BucketName string
	KeyPrefix  string
	ReadOnly   bool
}

// Function is an @app.function (or, when embedded in a Class, an @method).
type Function struct {
	Name string
	// Image is THIS callable's own resolved image (calque#174) — its own
	// image=<var> kwarg resolved against the script's known image chains,
	// falling back to App.Image only when this callable declares no
	// image= of its own. Before #174, every callable in a script shared
	// ONE globally-picked image (App.Image) regardless of which image=
	// var it actually referenced — a function with its OWN explicit
	// image= could silently get a DIFFERENT function's image.
	Image   Image
	GPU     string            // raw from source, e.g. "H100" or "A100:8" — guarded/rewritten in §7
	Volumes map[string]string // mount path -> Modal volume name (from_name)
	// CloudBucketMounts is every modal.CloudBucketMount(...) used INLINE as a
	// volumes= value on this callable (calque#91 Workstream A) — mounts the
	// USER'S OWN S3 bucket directly via mountpoint-s3, not calque's own
	// --bucket staging area the way an ordinary Volume mount is. Keyed by
	// mount path, disjoint from Volumes above (a given mount path is either
	// an ordinary Volume or a CloudBucketMount, never both).
	CloudBucketMounts map[string]CloudBucketMount
	Timeout           int        // seconds; 0 if unset
	Config            Config     // portable decorator config (cpu/memory/retries/secrets/schedule/region)
	IsMap             bool       // is this callable's .map() invoked anywhere in the script?
	Invoke            InvokeKind // how the callable is invoked (map/starmap/for_each/remote); §C
	EntryKind         EntryKind  // execution shape: batch (default) or serve (§F)
	Body              string     // verbatim payload, shipped to the worker
	Args              []string   // verbatim parameter names, incl. self/cls (calque#92: needed to reconstruct a .local()-referenced sibling's call signature)
	ItemArg           string     // first non-self parameter name — the per-item arg the warm runner binds
	// LocalCalls are the leaf names of sibling callables THIS function's own
	// body references via .local() (calque#92) — a property of the body, not
	// of how this function itself is invoked (distinct from Invoke/IsMap).
	LocalCalls []string
	// FreeRefs are bare (non-.local()-suffixed) references to a module-level
	// helper function or constant THIS function's own body reads/calls
	// (calque#139) — the .local()-free counterpart to LocalCalls; resolved
	// against App.ModuleFuncs/App.ModuleConsts by collectLocalExtras
	// (cmd/calque/run.go), the same shipping mechanism LocalCalls already
	// uses.
	FreeRefs []string
	Line     int // source line of the def, for leak attribution
	// Items is the real .map()/.starmap() iterable this callable is invoked
	// against, extracted at parse time from a literal list/tuple/str or a
	// range(N) call site (calque#136) — nil if the script's iterable wasn't
	// statically resolvable (a variable, comprehension, or non-range function
	// call result). See cmd/calque's realOrSyntheticItems, which falls back to
	// synthesized placeholders when this is nil.
	Items []any
	// IsClustered is true when this callable carries an
	// @modal.experimental.clustered(...) decorator (calque#152) — a
	// decorator-level multi-node request that neither the §7 GPU guard's
	// gpu= string parsing nor its body-text coupling regex can see on their
	// own; RewriteApp (internal/gpu) reads this to force FlagCouple
	// regardless of what the gpu= spec alone would conclude.
	IsClustered bool
}

// EntryKind is a callable's execution shape (§F). Serve entrypoints are long-lived
// and request-driven — a fundamentally different shape from the batch .map the
// spike measures; they are detected + gated/leaked, but the server is not built
// (§16 success is batch+K).
type EntryKind string

const (
	EntryBatch EntryKind = ""      // plain @function / @cls method — the batch shape
	EntryServe EntryKind = "serve" // @web_endpoint/@asgi_app/@wsgi_app/@web_server
)

// InvokeKind is how a callable is invoked (spec §C). Modal's synchronous
// (block-and-wait) idioms are all in the spike's scope. .spawn() (calque#88) is
// CLASSIFIED (so a future block-and-wait fan-out driver can find every spawned
// callable) but not yet EXECUTED — §18 keeps calque block-and-wait-only, so a
// spawn-classified callable is still never picked as calque's single warm unit
// on its own. .map.aio remains purely deferred and never appears here.
type InvokeKind string

const (
	InvokeNone    InvokeKind = ""         // not invoked via a recognized idiom
	InvokeMap     InvokeKind = "map"      // .map(iterable) — items -> results
	InvokeStarmap InvokeKind = "starmap"  // .starmap(iterable_of_tuples) — tuple-splat args
	InvokeForEach InvokeKind = "for_each" // .for_each(iterable) — side effects, no result collect
	InvokeRemote  InvokeKind = "remote"   // .remote(args) — single blocking call
	InvokeSpawn   InvokeKind = "spawn"    // .spawn(args) — classified only; not yet executed (calque#88, driver: #97)
)

// Class is an @app.cls: a warm, stateful unit whose @enter body runs once per
// container and whose @method bodies process items against the loaded state.
type Class struct {
	Name string
	// Image is THIS class's own resolved image (calque#174) — see
	// Function.Image's doc comment; the same App->class->method
	// resolution chain gpu=/volumes= already used is extended to image=.
	Image   Image
	GPU     string
	Volumes map[string]string
	// CloudBucketMounts mirrors Function.CloudBucketMounts (calque#91
	// Workstream A) — see its doc comment.
	CloudBucketMounts map[string]CloudBucketMount
	Timeout           int
	Config            Config     // portable decorator config (§B)
	EnterBody         string     // @modal.enter body — runs ONCE in the warm runner (§6)
	HasExit           bool       // @modal.exit() present (calque#86); teardown is not reproduced
	Methods           []Function // @modal.method bodies
	// EnterLocalCalls are sibling callables the @enter body itself references
	// via .local() (calque#92) — EnterBody is a bare string with no other Function
	// to carry this on.
	EnterLocalCalls []string
	// EnterFreeRefs mirrors EnterLocalCalls for calque#139's bare (non-
	// .local()) free-variable references — e.g. `self.prefix = GREETING`
	// inside @enter, with no .local() call in sight.
	EnterFreeRefs []string
	Line          int
}

// GPUSpec is the parsed form of a raw gpu= string: card plus requested count.
// "H100" -> {Card:"H100", Count:1}; "A100:8" -> {Card:"A100", Count:8}.
// Parsing lives in the gpu package (§7); this type is shared so plan/cost can read it.
type GPUSpec struct {
	Raw   string
	Card  string
	Count int
}
