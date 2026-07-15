// Package ir holds the six-primitive intermediate representation (spec §14):
// a struct-shaped transcription of what a Modal script's decorators said.
//
// Nothing clever lives here. Decorators are CONFIGURATION and are transcribed;
// function/method BODIES are PAYLOAD and are carried verbatim as strings, never
// interpreted — they ship to the worker and run under Python exactly as on Modal.
package ir

// App is a whole Modal application, transcribed from its decorators.
type App struct {
	Name       string            // modal.App("name")
	Image      Image             // resolved image for the app (see parse.resolveImage)
	Volumes    map[string]string // module-level Volume var name -> Modal volume name (from_name)
	Functions  []Function        // @app.function
	Classes    []Class           // @app.cls
	Entrypoint *Function         // @app.local_entrypoint (nil if none)
	Script     string            // source path, for leak attribution (§10)
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

// Function is an @app.function (or, when embedded in a Class, an @method).
type Function struct {
	Name    string
	GPU     string            // raw from source, e.g. "H100" or "A100:8" — guarded/rewritten in §7
	Volumes map[string]string // mount path -> Modal volume name (from_name)
	Timeout int               // seconds; 0 if unset
	IsMap   bool              // is this callable's .map() invoked anywhere in the script?
	Body    string            // verbatim payload, shipped to the worker
	Line    int               // source line of the def, for leak attribution
}

// Class is an @app.cls: a warm, stateful unit whose @enter body runs once per
// container and whose @method bodies process items against the loaded state.
type Class struct {
	Name      string
	GPU       string
	Volumes   map[string]string
	Timeout   int
	EnterBody string     // @modal.enter body — runs ONCE in the warm runner (§6)
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
