package axis1

import (
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
)

// node is a decoded expression: a resolved in-memory form of one compute
// expression entity, with child pointers already resolved (not hashes to
// re-fetch) and literals already parsed. Decoded once per unique IR hash and
// walked every eval — this is the whole point of the engine (exploration §13.3).
//
// A resolved node walker, not a bytecode VM: it removes the dominant cost with
// far less surface and keeps a 1:1 map to the IR that makes the equivalence
// oracle trivial to reason about. A linear bytecode form is a later increment
// on this same rung (§13.3) — deliberately not built here.
type node interface {
	// isNode is a marker. Evaluation dispatches on the concrete type in
	// eval.go rather than through a method, so the walker's switch mirrors
	// evaluateInner's switch (ext/compute/eval.go) line for line — the
	// oracle argument is "these two switches agree", and that is much easier
	// to check when both are switches.
	isNode()
}

// --- Leaves ---

// litNode is a pre-parsed literal. Stage-1 ref: evalLiteral, ext/compute/eval.go.
// The CBOR decode that Stage-1 pays on every eval happens once, here, at decode.
type litNode struct{ value interface{} }

// slotNode is a lexically-resolved variable reference: an array index into the
// frame stack, never a string-map lookup and never an entity reload. This is
// the LoadScope kill (exploration §13.4 — the load-bearing part).
//
// depth is the number of parent hops from the current frame; index is the slot
// within that frame. Stage-1 ref: evalLookupScope, ext/compute/eval.go, which
// does a map lookup against a Scope that was itself rebuilt per element.
type slotNode struct {
	depth int
	index int
	name  string // retained for error messages only — never used to resolve
}

// rootNode is a free variable: a lookup/scope whose name is not bound by any
// enclosing let/lambda in the expression, so it must resolve against the root
// scope supplied at Evaluate time. Not knowable at decode time, so it stays a
// name lookup — but the root scope is tiny (the caller's initial bindings) and
// off the hot path, unlike Stage-1 where EVERY lookup is a map hit.
type rootNode struct{ name string }

// --- Impure frontier (§13.5: lookup/tree is the impure boundary host-call) ---

// treeNode is compute/lookup/tree. Stage-1 ref: evalLookupTree, ext/compute/eval.go.
// Path canonicalization, capability checking, and dep registration all happen at
// eval time (they depend on ctx, not on the graph), so only the decode is saved.
type treeNode struct {
	path     string
	relative bool
}

// hashNode is compute/lookup/hash. Stage-1 ref: evalLookupHash, ext/compute/eval.go.
// The target cannot be resolved at decode time: resolution runs through
// validate_compute_resolvable, which depends on the eval context's access tier.
type hashNode struct {
	target hash.Hash
}

// --- Interior ---

// arithNode is compute/arithmetic.
//
// unsignedHint is computed at DECODE time by inspecting whether either operand
// node is literally a numeric-cast(primitive/uint). That is exactly Rule 11's
// semantics — the cast is eager, consumed by the immediately-following op, and
// does NOT flow through a let — because Stage-1 detects it by inspecting the
// operand ENTITY's type (castIntent, ext/compute/eval_arith.go), which is a
// static property of the graph. Hoisting it to decode is free and cannot drift.
type arithNode struct {
	op           string
	left, right  node
	unsignedHint bool
}

// cmpNode is compute/compare. Same Rule 11 decode-time hint as arithNode.
type cmpNode struct {
	op           string
	left, right  node
	unsignedHint bool
}

// logicNode is compute/logic. right is nil for "not".
//
// NOTE: Stage-1 (evalLogic, ext/compute/eval_arith.go) evaluates BOTH operands
// before applying and/or — it does not short-circuit. Preserved deliberately:
// short-circuiting would be an observable semantic change (a poisoned operand
// that Stage-1 evaluates would go unevaluated) and §13.5 forbids "optimizing"
// evaluation order. Lazy evaluation lives in `if`, not here.
type logicNode struct {
	op          string
	left, right node
}

// ifNode is compute/if. elseN is nil when the IR omits it (Stage-1 yields nil).
// Lazy: only the taken branch is evaluated (§13.5 — must not become eager).
type ifNode struct {
	cond, then, elseN node
}

// letBinding is one binding of a letNode, in IR order.
type letBinding struct {
	name  string
	value node
}

// letNode is compute/let. Bindings are EAGER and evaluated in IR order, each
// visible to those after it (Stage-1: evalLet copies the scope and Sets each
// binding in sequence). The S1 builder normalizes bindings to sorted name
// order, so sequential visibility follows alphabetical order — the F-E-series
// footgun. That ordering is inherited from the IR here, never re-sorted.
type letNode struct {
	bindings []letBinding
	body     node
	nslots   int
}

// lambdaNode is compute/lambda. Evaluating it produces a live closure over the
// current frame — no CaptureScope, no content-addressed compute/scope entity.
// Per §13.4, a captured scope entity is only semantically required when a
// closure actually crosses the boundary, which for a pure map/fold never happens.
//
// bodyHash is the IR hash of the body, retained solely so a closure that DOES
// cross the boundary can be materialized into the compute/closure entity
// Stage-1 would have built (materialize.go::materializeClosure). It is not
// reachable from the decoded body node, and it is never touched on the
// interior path.
type lambdaNode struct {
	params   []string
	body     node
	bodyHash hash.Hash
	nslots   int
}

// fieldNode is compute/field. Stage-1 ref: evalField, ext/compute/eval_construct.go.
type fieldNode struct {
	name   string
	target node
}

// constructNode is compute/construct. Fields are held in canonical map-key
// order resolved at decode time (Stage-1 ref: canonicalSorted, ext/compute/eval.go)
// so eval never re-sorts. Evaluation order is observable — a field expression
// can hit the impure frontier — so the order is part of the contract, not an
// implementation detail.
type constructNode struct {
	entityType string
	fieldNames []string // canonical order, parallel to fieldVals
	fieldVals  []node
}

// indexNode is compute/index. Stage-1 ref: evalIndex, ext/compute/eval_construct.go.
type indexNode struct {
	array, index node
}

// lengthNode is compute/length. Stage-1 ref: evalLength, ext/compute/eval_construct.go.
type lengthNode struct {
	array node
}

// castNode is compute/numeric-cast. Stage-1 ref: evalNumericCast,
// ext/compute/eval_arith.go.
type castNode struct {
	value  node
	toType string
}

// --- Builtins (§3.5): the collection ops that carry the F-D2 cost ---

// mapNode, filterNode, foldNode are compute/apply against
// system/compute/builtins/{map,filter,fold}. Stage-1 ref: builtinMap /
// builtinFilter / builtinFold, ext/compute/builtins.go.
//
// These are where the O(N²) lives at Stage 1: invokeClosure re-decodes the
// closure entity AND LoadScope-loads its full captured environment per element
// (ext/compute/builtins.go::invokeClosure), so a closure capturing an N-item
// collection makes iteration O(N²) per call. Here fn is a decoded lambdaNode
// invoked against a fresh element slot in a shared frame — linear, and the
// general form of the F-D2 point-fix routed to core-go.
type mapNode struct {
	coll, fn node
}

type filterNode struct {
	coll, fn node
}

type foldNode struct {
	coll, fn, initial node
}

// --- The Stage-1 fallback seam ---

// fallbackNode carries an expression this engine does not implement (compute/apply
// in dispatch mode: capability checks, handler dispatch, builtins/store). Eval
// materializes the live frame into a Stage-1 *compute.Scope and calls the
// exported compute.Evaluate on the original entity.
//
// This makes the engine correct-by-construction and incrementally buildable:
// implement the hot nodes, fall back for the rest, and let the oracle say when
// a fallback is wrong. It also keeps the capability/dispatch/install machinery
// on the Stage-1 path, where it belongs during an exploration.
//
// Placement is not arbitrary. The seam sits at compute/apply, which is already
// a boundary where Stage-1 itself materializes (an apply arg on a hash-typed
// field is a materialized boundary entity per §13.5) — so materializing at the
// seam matches what Stage-1 does there rather than introducing a new
// materialization point.
//
// It does not contaminate the measurement: every node type Life and Snake use
// is in the fast set, so no fallback fires on the hot path (see the gaps doc §4.2).
type fallbackNode struct {
	ent entity.Entity
}

func (litNode) isNode()       {}
func (slotNode) isNode()      {}
func (rootNode) isNode()      {}
func (treeNode) isNode()      {}
func (hashNode) isNode()      {}
func (arithNode) isNode()     {}
func (cmpNode) isNode()       {}
func (logicNode) isNode()     {}
func (ifNode) isNode()        {}
func (letNode) isNode()       {}
func (lambdaNode) isNode()    {}
func (fieldNode) isNode()     {}
func (constructNode) isNode() {}
func (indexNode) isNode()     {}
func (lengthNode) isNode()    {}
func (castNode) isNode()      {}
func (mapNode) isNode()       {}
func (filterNode) isNode()    {}
func (foldNode) isNode()      {}
func (fallbackNode) isNode()  {}
