// Package leak is the structured leak report (spec §10) — a primary, first-class
// deliverable of the spike. Every place the Modal shape does not carry gets an
// EMITTED record, not a code comment. A clean run that surfaces three ugly edges
// taught us more than a suspiciously clean run that surfaced none.
package leak

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Primitive is which Modal primitive leaked. Values per spec §10.
type Primitive string

const (
	PrimMap        Primitive = "map"
	PrimEnter      Primitive = "enter"
	PrimImage      Primitive = "image"
	PrimVolume     Primitive = "volume"
	PrimGPU        Primitive = "gpu"
	PrimEntrypoint Primitive = "entrypoint"
	PrimAcquire    Primitive = "acquire"
)

// Kind is the shape of the leak. Values per spec §10.
type Kind string

const (
	KindUnsupportedArg  Kind = "unsupported_arg"  // a decorator arg the parser doesn't model
	KindSemanticGap     Kind = "semantic_gap"     // Modal does X, we structurally can't
	KindUnhandledCase   Kind = "unhandled_case"   // a case we hit but didn't handle
	KindIntegrationEdge Kind = "integration_edge" // friction at a spore.host / AWS boundary
)

// Leak is one emitted record. Matches spec §10 exactly, plus a Line for precision.
type Leak struct {
	Primitive Primitive `json:"primitive"`
	Kind      Kind      `json:"kind"`
	Detail    string    `json:"detail"` // what Modal does; what we did/didn't do
	Script    string    `json:"script"` // which test script
	Line      int       `json:"line,omitempty"`
}

// Report accumulates leaks over a run. The report is the finding whether or not
// the brain ever gets built (§10): it's simultaneously the engineering map and a
// market census of how much Modal usage is even AWS-mappable.
type Report struct {
	Leaks []Leak `json:"leaks"`
}

// Add appends a leak. Convenience for the common call shape.
func (r *Report) Add(p Primitive, k Kind, script string, line int, detail string) {
	r.Leaks = append(r.Leaks, Leak{Primitive: p, Kind: k, Detail: detail, Script: script, Line: line})
}

// Addf is Add with a printf-style detail.
func (r *Report) Addf(p Primitive, k Kind, script string, line int, format string, args ...any) {
	r.Add(p, k, script, line, fmt.Sprintf(format, args...))
}

// Len reports how many leaks were emitted.
func (r *Report) Len() int { return len(r.Leaks) }

// JSON writes the report as indented JSON — the machine-readable deliverable.
func (r *Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Summary writes a human-readable digest grouped by primitive. Deterministic
// ordering so a skeptic diffing two runs sees a stable report.
func (r *Report) Summary(w io.Writer) {
	if len(r.Leaks) == 0 {
		fmt.Fprintln(w, "LEAKS: none emitted (note: a suspiciously clean run is itself worth a second look)")
		return
	}
	byPrim := map[Primitive][]Leak{}
	for _, l := range r.Leaks {
		byPrim[l.Primitive] = append(byPrim[l.Primitive], l)
	}
	prims := make([]string, 0, len(byPrim))
	for p := range byPrim {
		prims = append(prims, string(p))
	}
	sort.Strings(prims)

	fmt.Fprintf(w, "LEAKS: %d emitted across %d primitives\n", len(r.Leaks), len(prims))
	for _, p := range prims {
		ls := byPrim[Primitive(p)]
		fmt.Fprintf(w, "  %s (%d):\n", p, len(ls))
		for _, l := range ls {
			loc := l.Script
			if l.Line > 0 {
				loc = fmt.Sprintf("%s:%d", l.Script, l.Line)
			}
			fmt.Fprintf(w, "    - [%s] %s (%s)\n", l.Kind, strings.TrimSpace(l.Detail), loc)
		}
	}
}
