package axis1

import (
	"fmt"
	"strings"

	"go.entitychurch.org/entity-core-go/core/capability"
	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/compute"
)

// newError builds a *compute.ComputeError — deliberately the SAME error type
// Stage-1 raises, not a parallel one. ComputeError and the §9.1 code constants
// are exported, so reusing them keeps error codes/messages directly comparable
// in the differential harness and keeps callers' *entitysdk.Error predicates
// working unchanged across engines.
func newError(code, message string) *compute.ComputeError {
	return &compute.ComputeError{Code: code, Message: message}
}

// closure is a live closure: a decoded lambda plus the frame it captured.
//
// This is the F-D2 fix in one struct. Stage-1's closure is a compute/closure
// ENTITY whose env is a content-addressed compute/scope entity, so invoking it
// per element re-decodes the closure and LoadScope-reloads the whole captured
// environment every time (ext/compute/builtins.go::invokeClosure) — O(N²) for a
// closure capturing an N-item collection. Here the body is already decoded and
// the environment is a pointer.
type closure struct {
	node *lambdaNode
	env  *frame
}

// tailNode is the trampoline continuation, mirroring Stage-1's tailCall
// (ext/compute/eval.go). root is carried because a lookup/tree that yields an
// expression rebinds the dynamic scope (see evalInner).
type tailNode struct {
	n    node
	fr   *frame
	root map[string]interface{}
}

type evaluator struct {
	eng    *Engine
	budget *compute.Budget
	ctx    *compute.EvalContext

	// local is merged into the engine's Stats once, at the end of Evaluate —
	// never locked here, because this is the hot path.
	local Stats
}

// eval mirrors ext/compute/eval.go::Evaluate exactly, including the trampoline
// and the budget/depth accounting.
//
// The metering is deliberately identical (§13.5): one operation per trampoline
// iteration, depth decremented on entry and restored on every exit path. Any
// movement of the 100k-op cliff must therefore be explained by fewer
// materialized artifacts, not by silent metering drift — so this loop is a
// transcription, not a re-design.
func (e *evaluator) eval(n node, fr *frame, root map[string]interface{}) (interface{}, error) {
	if e.budget.Depth <= 0 {
		return nil, newError(compute.ErrDepthExceeded, "Maximum evaluation depth exceeded")
	}
	e.budget.Depth--

	for {
		e.budget.Operations--
		if e.budget.Operations <= 0 {
			e.budget.Depth++
			return nil, newError(compute.ErrBudgetExhausted, "Computation budget exhausted")
		}

		result, err := e.evalInner(n, fr, root)
		if err != nil {
			e.budget.Depth++
			return nil, err
		}
		if tc, ok := result.(tailNode); ok {
			n, fr, root = tc.n, tc.fr, tc.root
			continue
		}

		e.budget.Depth++
		return result, nil
	}
}

// evalInner dispatches one node. The switch mirrors Stage-1's evaluateInner
// case for case; keep them aligned.
func (e *evaluator) evalInner(n node, fr *frame, root map[string]interface{}) (interface{}, error) {
	switch t := n.(type) {
	case litNode:
		return t.value, nil

	case slotNode:
		// The hot path: array index + pointer hops. No map, no hash, no reload.
		return fr.lookup(t.depth, t.index), nil

	case rootNode:
		if v, ok := root[t.name]; ok {
			return v, nil
		}
		return nil, newError(compute.ErrNotFound, "No scope binding: "+t.name)

	case treeNode:
		return e.evalTree(t, fr, root)

	case hashNode:
		return e.evalHash(t, fr, root)

	case arithNode:
		left, err := e.eval(t.left, fr, root)
		if err != nil {
			return nil, err
		}
		right, err := e.eval(t.right, fr, root)
		if err != nil {
			return nil, err
		}
		return applyArithmetic(t.op, left, right, t.unsignedHint)

	case cmpNode:
		left, err := e.eval(t.left, fr, root)
		if err != nil {
			return nil, err
		}
		right, err := e.eval(t.right, fr, root)
		if err != nil {
			return nil, err
		}
		return applyCompare(t.op, left, right, t.unsignedHint)

	case logicNode:
		return e.evalLogic(t, fr, root)

	case ifNode:
		return e.evalIf(t, fr, root)

	case letNode:
		return e.evalLet(t, fr, root)

	case lambdaNode:
		// The live-closure move (§13.4): capture the frame by POINTER. No
		// CaptureScope, no compute/scope entity, no store write, no hash.
		// Stage-1 builds and stores one here on every evaluation of the lambda.
		e.local.Closures++
		return &closure{node: &t, env: fr}, nil

	case fieldNode:
		return e.evalField(t, fr, root)

	case constructNode:
		return e.evalConstruct(t, fr, root)

	case indexNode:
		return e.evalIndex(t, fr, root)

	case lengthNode:
		arr, err := e.evalArray(t.array, fr, root, msgLengthArr)
		if err != nil {
			return nil, err
		}
		return int64(len(arr)), nil

	case castNode:
		val, err := e.eval(t.value, fr, root)
		if err != nil {
			return nil, err
		}
		return applyNumericCast(val, t.toType)

	case mapNode:
		return e.evalMap(t, fr, root)

	case filterNode:
		return e.evalFilter(t, fr, root)

	case foldNode:
		return e.evalFold(t, fr, root)

	case applyNode:
		return e.evalApply(t, fr, root)

	case fallbackNode:
		e.local.Fallbacks++
		return e.evalFallback(t, fr, root)

	default:
		return nil, newError(compute.ErrUnknownType,
			fmt.Sprintf("axis1: unhandled node %T", n))
	}
}

// evalTree mirrors ext/compute/eval.go::evalLookupTree — the impure frontier.
// Path canonicalization, the capability ceiling check, and dep registration are
// transcribed rather than reused (they are unexported), so this function is the
// one most worth re-diffing when Stage-1's tree lookup changes.
func (e *evaluator) evalTree(t treeNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	ctx := e.ctx
	path := t.path
	if t.relative && ctx.SubgraphRoot != "" {
		path = store.CleanPath(ctx.SubgraphRoot + "/" + path)
	} else if !strings.HasPrefix(path, "/") && ctx.LocalPeerID != "" {
		path = store.QualifyPath(ctx.LocalPeerID, path)
	}

	if !ctx.Capability.ContentHash.IsZero() {
		capData, err := types.CapabilityTokenDataFromEntity(ctx.Capability)
		if err != nil {
			return nil, newError(compute.ErrPermissionDenied,
				"cannot decode evaluation capability: "+err.Error())
		}
		granterPeerID, gerr := capability.ResolveGranterPeerID(
			capData.Granter, ctx.ContentStore, crypto.PeerID(ctx.LocalPeerID))
		if gerr != nil {
			return nil, newError(compute.ErrPermissionDenied,
				"granter unresolvable: "+gerr.Error())
		}
		if !capability.CheckPathPermission("get", path, capData, "system/tree",
			crypto.PeerID(ctx.LocalPeerID), granterPeerID) {
			return nil, newError(compute.ErrPermissionDenied,
				"capability does not cover tree read: "+path)
		}
	}
	if ctx.RegisterDep != nil {
		ctx.RegisterDep(path)
	}
	if ctx.LocationIndex == nil {
		return nil, newError(compute.ErrNotFound, "No entity at path: "+path)
	}
	h, ok := ctx.LocationIndex.Get(path)
	if !ok {
		return nil, newError(compute.ErrNotFound, "No entity at path: "+path)
	}
	treeEnt, ok := ctx.ContentStore.Get(h)
	if !ok {
		return nil, newError(compute.ErrNotFound, "No entity at path: "+path)
	}
	if compute.IsComputeExpression(treeEnt) {
		return e.tailIntoDynamic(treeEnt, fr, root)
	}
	return treeEnt, nil
}

// evalHash mirrors ext/compute/eval.go::evalLookupHash.
func (e *evaluator) evalHash(t hashNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	target, ok := e.resolveChecked(t.target)
	if !ok {
		return nil, newError(compute.ErrNotFound,
			fmt.Sprintf("Cannot resolve hash for hash lookup: %s", t.target))
	}
	if compute.IsComputeExpression(target) {
		return e.tailIntoDynamic(target, fr, root)
	}
	return target, nil
}

// tailIntoDynamic handles an expression fetched at EVAL time (from the tree or
// by hash), which the graph could not have resolved at decode time.
//
// The subtlety worth pinning: Stage-1 tail-calls the fetched expression with
// the CURRENT scope (tailCall{entity: treeEnt, scope: scope}), so the fetched
// expression's free variables resolve dynamically against every enclosing let
// binding. A fetched expression has no lexical relationship to this graph, so
// it decodes with no lexical context and every lookup/scope in it becomes a
// rootNode — which means the root it evaluates against must be the flattened
// current scope, not the original root. Getting this wrong would silently
// resolve free variables against the wrong bindings, and no Life/Snake test
// would notice (neither program fetches an expression from the tree).
//
// The decode is cached by content hash like any other (engine.go), so an
// expression fetched from the tree every tick is still decoded only once.
func (e *evaluator) tailIntoDynamic(ent entity.Entity, fr *frame, root map[string]interface{}) (interface{}, error) {
	n, err := e.eng.decodeCached(ent, e.ctx)
	if err != nil {
		return nil, err
	}
	return tailNode{n: n, fr: nil, root: fr.flatten(root)}, nil
}

// resolveChecked mirrors ext/compute/eval.go::resolve + validateComputeResolvable
// (v3.6 D2 §4.2): the layered access model that stops the evaluator from being
// used as a content-store oracle.
func (e *evaluator) resolveChecked(h hash.Hash) (entity.Entity, bool) {
	ctx := e.ctx
	check := func(ent entity.Entity) (entity.Entity, bool) {
		if ctx.HasContentStoreAccess {
			return ent, true
		}
		if isComputeType(ent) {
			return ent, true
		}
		// Tier 2: sealed set from the installed subgraph (D5).
		if ctx.AuthorizedDataHashes != nil && ctx.AuthorizedDataHashes[h] {
			return ent, true
		}
		return entity.Entity{}, false
	}
	if ctx.Included != nil {
		if ent, ok := ctx.Included[h]; ok {
			return check(ent)
		}
	}
	if ctx.ContentStore != nil {
		if ent, ok := ctx.ContentStore.Get(h); ok {
			return check(ent)
		}
	}
	return entity.Entity{}, false
}

// isComputeType mirrors ext/compute/eval.go::isComputeType.
func isComputeType(ent entity.Entity) bool {
	switch ent.Type {
	case types.TypeComputeLiteral,
		types.TypeComputeLookupScope, types.TypeComputeLookupTree, types.TypeComputeLookupHash,
		types.TypeComputeApply, types.TypeComputeIf, types.TypeComputeLet, types.TypeComputeLambda,
		types.TypeComputeArithmetic, types.TypeComputeCompare, types.TypeComputeLogic,
		types.TypeComputeField, types.TypeComputeConstruct,
		types.TypeComputeIndex, types.TypeComputeLength, types.TypeComputeNumericCast,
		types.TypeComputeClosure, types.TypeComputeScope,
		types.TypeComputeResult, types.TypeComputeError,
		types.TypeComputeSubgraph,
		types.TypeComputeInstallRequest, types.TypeComputeInstallResult:
		return true
	}
	return false
}

// evalLogic mirrors ext/compute/eval_arith.go::evalLogic.
//
// Both operands are evaluated before and/or is applied — Stage-1 does not
// short-circuit, and neither does this. Adding short-circuiting would be an
// observable change (§13.5 forbids reordering evaluation): an operand that
// poisons the eval under Stage-1 would silently stop doing so.
func (e *evaluator) evalLogic(t logicNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	left, err := e.eval(t.left, fr, root)
	if err != nil {
		return nil, err
	}
	if t.op == "not" {
		return !truthy(left), nil
	}
	if t.right == nil {
		return nil, newError(compute.ErrInvalidExpression,
			"logic op "+t.op+" requires right operand")
	}
	right, err := e.eval(t.right, fr, root)
	if err != nil {
		return nil, err
	}
	switch t.op {
	case "and":
		return truthy(left) && truthy(right), nil
	case "or":
		return truthy(left) || truthy(right), nil
	default:
		return nil, newError(compute.ErrInvalidExpression, "Unknown logic op: "+t.op)
	}
}

// evalIf mirrors ext/compute/eval.go::evalIf — lazy: only the taken branch is
// evaluated, and it is tail-called so the metering matches.
func (e *evaluator) evalIf(t ifNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	cond, err := e.eval(t.cond, fr, root)
	if err != nil {
		return nil, err
	}
	if truthy(cond) {
		return tailNode{n: t.then, fr: fr, root: root}, nil
	}
	if t.elseN != nil {
		return tailNode{n: t.elseN, fr: fr, root: root}, nil
	}
	// Stage-1 returns (nil, nil) for a false `if` with no else branch.
	return nil, nil
}

// evalLet mirrors ext/compute/eval.go::evalLet — bindings are EAGER, evaluated
// in IR order, each visible to those after it.
//
// One frame, filled left to right. The decoder already fixed each binding's
// slot coordinates against exactly the bindings that precede it, so sequential
// visibility falls out of filling the slots in order rather than needing a
// scope copy per binding (Stage-1 copies the whole map once per let).
func (e *evaluator) evalLet(t letNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	e.local.Frames++
	inner := newFrame(nil, t.nslots, fr)
	for i, b := range t.bindings {
		v, err := e.eval(b.value, inner, root)
		if err != nil {
			return nil, err
		}
		inner.slots[i] = v
	}
	return tailNode{n: t.body, fr: inner, root: root}, nil
}

// evalField mirrors ext/compute/eval_construct.go::evalField — dispatch on the
// target's Go type (v3.19c Part A R3 / M3), never on a sniffed wire shape.
func (e *evaluator) evalField(t fieldNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	target, err := e.eval(t.target, fr, root)
	if err != nil {
		return nil, err
	}
	switch v := target.(type) {
	case *constructedValue:
		val, ok := v.fields[t.name]
		if !ok {
			return nil, newError(compute.ErrNotFound, "Field not found: "+t.name)
		}
		return val, nil
	case entity.Entity:
		var dataMap map[string]interface{}
		if err := ecf.Decode(v.Data, &dataMap); err != nil {
			return nil, newError(compute.ErrTypeMismatch,
				"Field access requires an entity with map data")
		}
		val, ok := dataMap[t.name]
		if !ok {
			return nil, newError(compute.ErrNotFound, "Field not found: "+t.name)
		}
		return val, nil
	default:
		dataMap := toStringMap(target)
		if dataMap == nil {
			return nil, newError(compute.ErrTypeMismatch,
				fmt.Sprintf("Field access requires an entity or record value, got: %T", target))
		}
		val, ok := dataMap[t.name]
		if !ok {
			return nil, newError(compute.ErrNotFound, "Field not found: "+t.name)
		}
		return val, nil
	}
}

// evalConstruct mirrors ext/compute/eval_construct.go::evalConstruct — produces
// the in-flight typed value, NOT a materialized entity. Field order was fixed
// at decode time to ECF canonical map-key order (the order Stage-1's
// canonicalSorted yields), which is observable because a field expression can
// hit the impure frontier.
func (e *evaluator) evalConstruct(t constructNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	fields := make(map[string]interface{}, len(t.fieldNames))
	for i, name := range t.fieldNames {
		v, err := e.eval(t.fieldVals[i], fr, root)
		if err != nil {
			return nil, err
		}
		fields[name] = v
	}
	return &constructedValue{entityType: t.entityType, fields: fields}, nil
}

// evalIndex mirrors ext/compute/eval_construct.go::evalIndex.
func (e *evaluator) evalIndex(t indexNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	arr, err := e.evalArray(t.array, fr, root, msgIndexArray)
	if err != nil {
		return nil, err
	}
	idxVal, err := e.eval(t.index, fr, root)
	if err != nil {
		return nil, err
	}
	idx, isInt, inInt64 := asIndex(idxVal)
	if !isInt {
		return nil, newError(compute.ErrTypeMismatch,
			fmt.Sprintf("compute/index requires an integer index, got %T", idxVal))
	}
	// F-2 (§9.1): an integer index whose magnitude overflows int64 is still a
	// well-formed index argument — it is necessarily ≥ len(arr), so it is out of
	// range, not a type error. Fold that into the bounds check exactly as Stage-1
	// does, so a uint64 above MaxInt64 answers index_out_of_range, not
	// type_mismatch (the divergence AE-5 caught: worked/record/index-out-of-range-uint).
	if !inInt64 || idx < 0 || idx >= int64(len(arr)) {
		return nil, newError(compute.ErrIndexOutOfRange,
			fmt.Sprintf("index %s out of range for array of length %d",
				indexMagnitude(idxVal, idx, inInt64), len(arr)))
	}
	return arr[idx], nil
}

// evalArray evaluates a node and asserts an array result.
//
// format carries the caller's exact Stage-1 message (a single %T), because the
// three call sites word it differently — evalIndex and evalLength say
// "compute/{index,length} requires an array, got %T"
// (ext/compute/eval_construct.go) while the collection builtins say
// "collection must be an array, got %T" (ext/compute/builtins.go::resolveCollection).
// Error messages are compared by the differential harness, so the wording is
// part of the contract, not decoration.
func (e *evaluator) evalArray(n node, fr *frame, root map[string]interface{}, format string) ([]interface{}, error) {
	v, err := e.eval(n, fr, root)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, newError(compute.ErrTypeMismatch, fmt.Sprintf(format, v))
	}
	return arr, nil
}

const (
	msgCollection = "collection must be an array, got %T"
	msgIndexArray = "compute/index requires an array, got %T"
	msgLengthArr  = "compute/length requires an array, got %T"
)

// --- Collection builtins: where the F-D2 O(N²) is deleted ---

// resolveClosure evaluates the fn arg to a live closure. Mirrors
// ext/compute/builtins.go::resolveClosureArg, but the result is a decoded
// lambda + frame pointer rather than a compute/closure entity.
//
// A compute/closure ENTITY arriving here (built by Stage-1, or threaded in as a
// pre-computed value) cannot be invoked against live frames — its env is a
// content-addressed scope entity. That case routes to Stage-1 rather than being
// half-translated; it is off every measured path (a lambda written inline
// decodes to a lambdaNode) and correctness beats coverage at the seam.
func (e *evaluator) resolveClosure(n node, fr *frame, root map[string]interface{}) (*closure, error) {
	v, err := e.eval(n, fr, root)
	if err != nil {
		return nil, err
	}
	cl, ok := v.(*closure)
	if !ok {
		return nil, newError(compute.ErrTypeMismatch,
			fmt.Sprintf("fn must resolve to a closure, got %T", v))
	}
	return cl, nil
}

// invokeClosure runs a live closure against pre-evaluated args.
//
// Contrast ext/compute/builtins.go::invokeClosure, which per call re-decodes
// the closure entity AND LoadScope-loads its entire captured environment. Here:
// one frame allocation, N slot writes, no CBOR, no store access, no hashing.
// That difference is the whole F-D2 fix, generalized (§13.4).
func (e *evaluator) invokeClosure(cl *closure, args []interface{}, root map[string]interface{}) (interface{}, error) {
	if len(args) != len(cl.node.params) {
		return nil, newError(compute.ErrMissingArgument,
			fmt.Sprintf("closure expects %d args, got %d", len(cl.node.params), len(args)))
	}
	e.local.Frames++
	inner := newFrame(nil, cl.node.nslots, cl.env)
	copy(inner.slots, args)
	return e.eval(cl.node.body, inner, root)
}

// evalApply evaluates a closure application (compute/apply with an fn, no Path)
// — mirrors ext/compute/eval_apply.go::evalApplyClosure.
//
// It returns a tailNode, NOT a nested e.eval, so the invocation joins the
// enclosing trampoline. That is the whole reason native apply is worth building:
// a tail-position self-call then iterates without growing depth, exactly as
// Stage-1's tailCall does. The recurse corpus vector runs 5 levels deep against a
// depth budget of 16 — it only reaches a value (rather than depth_exceeded) if
// each self-apply continues the same eval() loop instead of nesting a new one.
func (e *evaluator) evalApply(t applyNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	fnVal, err := e.eval(t.fn, fr, root)
	if err != nil {
		return nil, err
	}
	cl, ok := fnVal.(*closure)
	if !ok {
		// fn resolved to something other than a LIVE closure — a Stage-1 closure
		// ENTITY (env is a content-addressed scope, not invocable against live
		// frames), or a non-closure value (a type error). Both are Stage-1's to
		// settle from the original entity: it invokes a closure entity via
		// LoadScope, and raises "Apply target is not a closure" with the canonical
		// message otherwise. Same documented seam resolveClosure keeps for map/fold
		// fn args; count it as the fallback it is. (fn is re-evaluated inside
		// Stage-1 here — accepted because this branch is off the native path and
		// no corpus vector reaches it; the previous code fell the whole apply back
		// too.)
		e.local.Fallbacks++
		return e.evalFallback(fallbackNode{ent: t.ent}, fr, root)
	}
	// Bind args into a fresh invocation frame over the closure's captured env, in
	// the CLOSURE's param order. Each arg is evaluated against the CALLER's frame
	// (Stage-1 evaluates every arg in the caller's scope before Setting it into
	// the closure's newScope), so args cannot see each other or the params.
	e.local.Frames++
	inner := newFrame(nil, cl.node.nslots, cl.env)
	for i, p := range cl.node.params {
		an, ok := t.args[p]
		if !ok {
			return nil, newError(compute.ErrMissingArgument, "Missing argument: "+p)
		}
		av, err := e.eval(an, fr, root)
		if err != nil {
			return nil, err
		}
		inner.slots[i] = av
	}
	return tailNode{n: cl.node.body, fr: inner, root: root}, nil
}

// evalMap mirrors ext/compute/builtins.go::builtinMap.
func (e *evaluator) evalMap(t mapNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	arr, err := e.evalArray(t.coll, fr, root, msgCollection)
	if err != nil {
		return nil, err
	}
	cl, err := e.resolveClosure(t.fn, fr, root)
	if err != nil {
		return nil, err
	}
	out := make([]interface{}, 0, len(arr))
	for _, elt := range arr {
		v, err := e.invokeClosure(cl, []interface{}{elt}, root)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// evalFilter mirrors ext/compute/builtins.go::builtinFilter — retains elements
// whose predicate is truthy, in index order.
func (e *evaluator) evalFilter(t filterNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	arr, err := e.evalArray(t.coll, fr, root, msgCollection)
	if err != nil {
		return nil, err
	}
	cl, err := e.resolveClosure(t.fn, fr, root)
	if err != nil {
		return nil, err
	}
	out := make([]interface{}, 0, len(arr))
	for _, elt := range arr {
		v, err := e.invokeClosure(cl, []interface{}{elt}, root)
		if err != nil {
			return nil, err
		}
		if truthy(v) {
			out = append(out, elt)
		}
	}
	return out, nil
}

// evalFold mirrors ext/compute/builtins.go::builtinFold — threads initial
// through fn(acc, element) left to right.
func (e *evaluator) evalFold(t foldNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	arr, err := e.evalArray(t.coll, fr, root, msgCollection)
	if err != nil {
		return nil, err
	}
	cl, err := e.resolveClosure(t.fn, fr, root)
	if err != nil {
		return nil, err
	}
	acc, err := e.eval(t.initial, fr, root)
	if err != nil {
		return nil, err
	}
	for _, elt := range arr {
		acc, err = e.invokeClosure(cl, []interface{}{acc, elt}, root)
		if err != nil {
			return nil, err
		}
	}
	return acc, nil
}

// evalFallback is the Stage-1 seam: flatten the live frame into a dynamic
// scope and hand the original entity to the exported compute.Evaluate.
//
// Budget and depth are shared (the same *compute.Budget pointer), so metering
// stays continuous across the seam. Stage-1 will re-decode from here down —
// that is the point: this path is for expressions where correctness matters and
// speed does not (dispatch, capability checks, builtins/store).
//
// The frame flatten materializes in-flight constructed values the same way
// Stage-1's own CaptureScope does (buildScopeBinding materializes a
// *constructedValue before binding it), so the seam does not invent a
// materialization point Stage-1 lacks.
func (e *evaluator) evalFallback(t fallbackNode, fr *frame, root map[string]interface{}) (interface{}, error) {
	bindings := root
	if fr != nil {
		bindings = fr.flatten(root)
	}
	lowered := make(map[string]interface{}, len(bindings))
	for name, v := range bindings {
		lv, err := lowerForStage1(v, e.ctx.ContentStore)
		if err != nil {
			return nil, err
		}
		lowered[name] = lv
	}
	return compute.Evaluate(t.ent, toScope(lowered), e.budget, e.ctx)
}
