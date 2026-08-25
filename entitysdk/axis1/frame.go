package axis1

import (
	"go.entitychurch.org/entity-core-go/ext/compute"
)

// --- Compile-time scope (decode) ---

// compileLevel is one lexical level during decode: a let's bindings or a
// lambda's params. It exists only to turn names into (depth, index) slot
// coordinates; nothing survives into eval except the names, kept for the
// boundary/fallback flatten path.
//
// visible implements let's sequential binding visibility. Stage-1's evalLet
// (ext/compute/eval.go) copies the scope and Sets each binding in turn, so
// binding i sees bindings 0..i-1 and NOT i..n-1. The decoder raises visible as
// it walks the bindings, reproducing exactly that window.
type compileLevel struct {
	names   []string
	visible int
	parent  *compileLevel
}

// resolve maps a name to a slot coordinate, or reports false if the name is
// free (not lexically bound — it must resolve against the root scope at eval).
//
// The backwards search within a level matters: a let with duplicate binding
// names produces two slots, and Stage-1's map would end up holding the LAST
// Set. Searching visible names in reverse reproduces last-wins, while still
// letting an earlier binding's value expression see the earlier slot.
func (l *compileLevel) resolve(name string) (depth, index int, ok bool) {
	d := 0
	for lv := l; lv != nil; lv = lv.parent {
		for i := lv.visible - 1; i >= 0; i-- {
			if lv.names[i] == name {
				return d, i, true
			}
		}
		d++
	}
	return 0, 0, false
}

// --- Runtime frames (eval) ---

// frame is the live interior environment: an array of value slots plus a parent
// pointer. This is the thing that replaces Stage-1's
// CaptureScope → hash → LoadScope round-trip (exploration §13.4).
//
// Per §9.1 applied to scope: a content-addressed compute/scope entity is only
// semantically required when a scope or closure value actually CROSSES the
// boundary. For a pure map/fold over interior data it never does — so the
// closure body is invoked per element against a fresh element slot in a shared
// frame, and no scope entity is ever built, hashed, stored, or reloaded.
type frame struct {
	level  *compileLevel
	slots  []interface{}
	parent *frame
}

func newFrame(level *compileLevel, n int, parent *frame) *frame {
	return &frame{level: level, slots: make([]interface{}, n), parent: parent}
}

// lookup reads a slot coordinate. This is the hot path: array index + pointer
// hops, no hashing, no map, no allocation, no entity reload.
func (f *frame) lookup(depth, index int) interface{} {
	fr := f
	for ; depth > 0; depth-- {
		fr = fr.parent
	}
	return fr.slots[index]
}

// flatten collapses the root scope plus the frame chain into the flat
// name→value map Stage-1 carries as its Scope. Outermost first so inner levels
// shadow outer ones, reproducing the copy-and-Set order of evalLet.
//
// Used at exactly three places where a live frame must become a dynamic scope:
//   - the fallback seam (fallbackNode),
//   - closure materialization, when a closure genuinely crosses the boundary, and
//   - a lookup/tree or lookup/hash that yields an expression, which Stage-1
//     evaluates against the CURRENT dynamic scope (see eval.go).
//
// It is never called on the interior hot path. If it shows up in a profile,
// something that should have stayed interior is crossing the boundary.
//
// The root scope is a plain map rather than a *compute.Scope because
// compute.Scope's bindings are unexported with no iteration API — it can be
// built and read by name, but never enumerated, so it cannot be a source here.
// Callers hold the map; the differential harness builds a *compute.Scope for
// Stage-1 and passes the same map here.
func (f *frame) flatten(root map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(root))
	// Root bindings sit outermost — any lexical binding shadows them.
	for name, v := range root {
		out[name] = v
	}
	var levels []*frame
	for fr := f; fr != nil; fr = fr.parent {
		levels = append(levels, fr)
	}
	for i := len(levels) - 1; i >= 0; i-- {
		fr := levels[i]
		if fr.level == nil {
			continue
		}
		for j, name := range fr.level.names {
			if j < len(fr.slots) {
				out[name] = fr.slots[j]
			}
		}
	}
	return out
}

// toScope builds the Stage-1 *compute.Scope the fallback seam hands to
// compute.Evaluate.
func toScope(bindings map[string]interface{}) *compute.Scope {
	s := compute.NewScope()
	for name, v := range bindings {
		s.Set(name, v)
	}
	return s
}
