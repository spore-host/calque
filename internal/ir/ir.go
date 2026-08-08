// Package ir holds the six-primitive intermediate representation (spec §14):
// a struct-shaped transcription of what a Modal script's decorators said.
//
// Nothing clever lives here. Decorators are CONFIGURATION and are transcribed;
// function/method BODIES are PAYLOAD and are carried verbatim as strings, never
// interpreted — they ship to the worker and run under Python exactly as on Modal.
package ir

// App is a whole Modal application, transcribed from its decorators.
type App struct {
	Name        string            // modal.App("name")
	Image       Image             // resolved image for the app (see parse.resolveImage)
	Volumes     map[string]string // module-level Volume var name -> Modal volume name (from_name)
	Functions   []Function        // @app.function
	Classes     []Class           // @app.cls
	Entrypoints []Function        // @app.local_entrypoint (may be more than one)
	Script      string            // source path, for leak attribution (§10)
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
	Schedule string   // schedule= (e.g. "0 * * * *"); recorded, not honored
	Region   string   // region= placement hint; recorded, not honored
}

// Function is an @app.function (or, when embedded in a Class, an @method).
type Function struct {
	Name      string
	GPU       string            // raw from source, e.g. "H100" or "A100:8" — guarded/rewritten in §7
	Volumes   map[string]string // mount path -> Modal volume name (from_name)
	Timeout   int               // seconds; 0 if unset
	Config    Config            // portable decorator config (cpu/memory/retries/secrets/schedule/region)
	IsMap     bool              // is this callable's .map() invoked anywhere in the script?
	Invoke    InvokeKind        // how the callable is invoked (map/starmap/for_each/remote); §C
	EntryKind EntryKind         // execution shape: batch (default) or serve (§F)
	Body      string            // verbatim payload, shipped to the worker
	ItemArg   string            // first non-self parameter name — the per-item arg the warm runner binds
	Line      int               // source line of the def, for leak attribution
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
// (block-and-wait) idioms are all in the spike's scope; async (.spawn/.map.aio)
// is deferred (M10/S2) and never appears here.
type InvokeKind string

const (
	InvokeNone    InvokeKind = ""         // not invoked via a recognized idiom
	InvokeMap     InvokeKind = "map"      // .map(iterable) — items -> results
	InvokeStarmap InvokeKind = "starmap"  // .starmap(iterable_of_tuples) — tuple-splat args
	InvokeForEach InvokeKind = "for_each" // .for_each(iterable) — side effects, no result collect
	InvokeRemote  InvokeKind = "remote"   // .remote(args) — single blocking call
)

// Class is an @app.cls: a warm, stateful unit whose @enter body runs once per
// container and whose @method bodies process items against the loaded state.
type Class struct {
	Name      string
	GPU       string
	Volumes   map[string]string
	Timeout   int
	Config    Config     // portable decorator config (§B)
	EnterBody string     // @modal.enter body — runs ONCE in the warm runner (§6)
	HasExit   bool       // @modal.exit() present (calque#86); teardown is not reproduced
	Methods   []Function // @modal.method bodies
	Line      int
}

// GPUSpec is the parsed form of a raw gpu= string: card plus requested count.
// "H100" -> {Card:"H100", Count:1}; "A100:8" -> {Card:"A100", Count:8}.
// Parsing lives in the gpu package (§7); this type is shared so plan/cost can read it.
type GPUSpec struct {
	Raw   string
	Card  string
	Count int
}
