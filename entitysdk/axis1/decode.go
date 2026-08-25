package axis1

import (
	"sort"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/compute"
)

// decoder walks an expression graph once, resolving child hashes to pointers,
// pre-parsing literals, and turning every lexically-bound lookup/scope into a
// (depth, index) slot coordinate.
//
// Everything here is paid once per unique IR hash (engine.go caches on it) and
// amortized across every tick and every program instance sharing that hash.
// Stage-1 pays all of it on every node of every eval — the ~54% the POC measured.
type decoder struct {
	ctx *compute.EvalContext
}

// decodeRoot decodes an expression entity into a resolved node tree.
func (d *decoder) decodeRoot(ent entity.Entity) (node, error) {
	return d.decode(ent, nil)
}

// resolveHash mirrors Stage-1's resolve + validate_compute_resolvable layering
// (ext/compute/eval.go::resolve). Decode-time resolution is safe for expression
// children because the access tier that governs it is fixed for the eval — but
// see hashNode: compute/lookup/hash targets are NOT resolved here, because the
// spec routes them through validate_compute_resolvable at eval time and the
// result can legitimately be a non-expression value.
func (d *decoder) resolveHash(h hash.Hash, label string) (entity.Entity, error) {
	if d.ctx.Included != nil {
		if ent, ok := d.ctx.Included[h]; ok {
			return ent, nil
		}
	}
	if d.ctx.ContentStore != nil {
		if ent, ok := d.ctx.ContentStore.Get(h); ok {
			return ent, nil
		}
	}
	return entity.Entity{}, newError(compute.ErrNotFound,
		"Cannot resolve hash for "+label+": "+h.String())
}

// decodeChild resolves a hash and decodes the entity behind it.
func (d *decoder) decodeChild(h hash.Hash, lex *compileLevel, label string) (node, error) {
	ent, err := d.resolveHash(h, label)
	if err != nil {
		return nil, err
	}
	return d.decode(ent, lex)
}

// decode turns one entity into a node. The switch mirrors Stage-1's
// evaluateInner (ext/compute/eval.go) case for case — that parallel is the
// oracle argument, so keep the order and the coverage aligned when either moves.
func (d *decoder) decode(ent entity.Entity, lex *compileLevel) (node, error) {
	switch ent.Type {
	case types.TypeComputeLiteral:
		var v types.ComputeLiteralData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		return litNode{value: v.Value}, nil

	case types.TypeComputeLookupScope:
		var v types.ComputeLookupScopeData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		if depth, index, ok := lex.resolve(v.Name); ok {
			return slotNode{depth: depth, index: index, name: v.Name}, nil
		}
		// Free variable: resolves against the root scope at eval time.
		return rootNode{name: v.Name}, nil

	case types.TypeComputeLookupTree:
		var v types.ComputeLookupTreeData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		return treeNode{path: v.Path, relative: v.Relative}, nil

	case types.TypeComputeLookupHash:
		var v types.ComputeLookupHashData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		return hashNode{target: v.Hash}, nil

	case types.TypeComputeArithmetic:
		var v types.ComputeArithmeticData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		left, err := d.decodeChild(v.Left, lex, "arithmetic left")
		if err != nil {
			return nil, err
		}
		right, err := d.decodeChild(v.Right, lex, "arithmetic right")
		if err != nil {
			return nil, err
		}
		hint, err := d.castIntent(v.Left, v.Right)
		if err != nil {
			return nil, err
		}
		return arithNode{op: v.Op, left: left, right: right, unsignedHint: hint}, nil

	case types.TypeComputeCompare:
		var v types.ComputeCompareData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		left, err := d.decodeChild(v.Left, lex, "compare left")
		if err != nil {
			return nil, err
		}
		right, err := d.decodeChild(v.Right, lex, "compare right")
		if err != nil {
			return nil, err
		}
		hint, err := d.castIntent(v.Left, v.Right)
		if err != nil {
			return nil, err
		}
		return cmpNode{op: v.Op, left: left, right: right, unsignedHint: hint}, nil

	case types.TypeComputeLogic:
		var v types.ComputeLogicData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		left, err := d.decodeChild(v.Left, lex, "logic left")
		if err != nil {
			return nil, err
		}
		n := logicNode{op: v.Op, left: left}
		// "not" uses left only; Stage-1 returns before touching right.
		if v.Op != "not" && v.Right != nil {
			right, err := d.decodeChild(*v.Right, lex, "logic right")
			if err != nil {
				return nil, err
			}
			n.right = right
		}
		return n, nil

	case types.TypeComputeIf:
		var v types.ComputeIfData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		cond, err := d.decodeChild(v.Condition, lex, "if condition")
		if err != nil {
			return nil, err
		}
		then, err := d.decodeChild(v.Then, lex, "if then")
		if err != nil {
			return nil, err
		}
		n := ifNode{cond: cond, then: then}
		if v.Else != nil {
			elseN, err := d.decodeChild(*v.Else, lex, "if else")
			if err != nil {
				return nil, err
			}
			n.elseN = elseN
		}
		return n, nil

	case types.TypeComputeLet:
		return d.decodeLet(ent, lex)

	case types.TypeComputeLambda:
		return d.decodeLambda(ent, lex)

	case types.TypeComputeField:
		var v types.ComputeFieldData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		target, err := d.decodeChild(v.Entity, lex, "field target")
		if err != nil {
			return nil, err
		}
		return fieldNode{name: v.Name, target: target}, nil

	case types.TypeComputeConstruct:
		return d.decodeConstruct(ent, lex)

	case types.TypeComputeIndex:
		var v types.ComputeIndexData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		arr, err := d.decodeChild(v.Array, lex, "index array")
		if err != nil {
			return nil, err
		}
		idx, err := d.decodeChild(v.Index, lex, "index value")
		if err != nil {
			return nil, err
		}
		return indexNode{array: arr, index: idx}, nil

	case types.TypeComputeLength:
		var v types.ComputeLengthData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		arr, err := d.decodeChild(v.Array, lex, "length array")
		if err != nil {
			return nil, err
		}
		return lengthNode{array: arr}, nil

	case types.TypeComputeNumericCast:
		var v types.ComputeNumericCastData
		if err := ecf.Decode(ent.Data, &v); err != nil {
			return nil, err
		}
		val, err := d.decodeChild(v.Value, lex, "numeric-cast value")
		if err != nil {
			return nil, err
		}
		return castNode{value: val, toType: v.ToType}, nil

	case types.TypeComputeApply:
		return d.decodeApply(ent, lex)

	default:
		// Value types (closure/scope/result/error) and anything unknown go to
		// Stage-1, which returns value types as-is (SA-1) and raises
		// unknown_type for the rest. No reason to duplicate either behavior.
		return fallbackNode{ent: ent}, nil
	}
}

// castIntent hoists Rule 11 to decode time. Stage-1's castIntent
// (ext/compute/eval_arith.go) inspects the resolved operand ENTITY's type — a
// static property of the graph — so computing it once at decode is exact, not
// an approximation. A cast indirected behind a let or a lookup is NOT
// recognized, which is precisely the spec's "doesn't flow through let".
func (d *decoder) castIntent(left, right hash.Hash) (bool, error) {
	for _, h := range []hash.Hash{left, right} {
		ent, err := d.resolveHash(h, "cast intent")
		if err != nil {
			return false, err
		}
		if ent.Type != types.TypeComputeNumericCast {
			continue
		}
		var v types.ComputeNumericCastData
		// Stage-1 swallows a decode error here and returns false; match it.
		if ecf.Decode(ent.Data, &v) != nil {
			continue
		}
		if v.ToType == "primitive/uint" {
			return true, nil
		}
	}
	return false, nil
}

// decodeLet builds one lexical level holding all bindings, raising `visible` as
// it goes so binding i resolves against bindings 0..i-1 only — Stage-1's
// copy-and-Set-in-sequence order (evalLet, ext/compute/eval.go).
//
// Binding order is taken from the IR verbatim and never re-sorted. The S1
// builder already normalized it to sorted name order, which is why sequential
// visibility follows the alphabet (the F-E-series footgun); re-sorting here
// would silently change which bindings see which.
func (d *decoder) decodeLet(ent entity.Entity, lex *compileLevel) (node, error) {
	var v types.ComputeLetData
	if err := ecf.Decode(ent.Data, &v); err != nil {
		return nil, err
	}

	level := &compileLevel{
		names:   make([]string, len(v.Bindings)),
		visible: 0,
		parent:  lex,
	}
	for i, b := range v.Bindings {
		level.names[i] = b.Name
	}

	bindings := make([]letBinding, len(v.Bindings))
	for i, b := range v.Bindings {
		// visible is still i here: binding i cannot see itself or any later one.
		val, err := d.decodeChild(b.Value, level, "let binding "+b.Name)
		if err != nil {
			return nil, err
		}
		bindings[i] = letBinding{name: b.Name, value: val}
		level.visible = i + 1
	}

	level.visible = len(v.Bindings)
	body, err := d.decodeChild(v.Body, level, "let body")
	if err != nil {
		return nil, err
	}
	return letNode{bindings: bindings, body: body, nslots: len(v.Bindings)}, nil
}

// decodeLambda builds a lexical level for the params. The body resolves against
// params + everything enclosing, which at eval becomes the live captured frame
// rather than a CaptureScope'd entity (§13.4).
func (d *decoder) decodeLambda(ent entity.Entity, lex *compileLevel) (node, error) {
	var v types.ComputeLambdaData
	if err := ecf.Decode(ent.Data, &v); err != nil {
		return nil, err
	}
	level := &compileLevel{
		names:   append([]string(nil), v.Params...),
		visible: len(v.Params),
		parent:  lex,
	}
	body, err := d.decodeChild(v.Body, level, "lambda body")
	if err != nil {
		return nil, err
	}
	return lambdaNode{
		params:   append([]string(nil), v.Params...),
		body:     body,
		bodyHash: v.Body,
		nslots:   len(v.Params),
	}, nil
}

// decodeConstruct resolves field evaluation order at decode time.
//
// Order is observable — a field expression can hit the impure frontier — so it
// is part of the contract. Stage-1 iterates canonicalSorted(d.Fields)
// (ext/compute/eval.go), i.e. ECF canonical map-key order: by encoded byte
// length, then lexicographically. Reproduced here exactly; see canonicalOrder.
func (d *decoder) decodeConstruct(ent entity.Entity, lex *compileLevel) (node, error) {
	var v types.ComputeConstructData
	if err := ecf.Decode(ent.Data, &v); err != nil {
		return nil, err
	}
	names := canonicalOrder(v.Fields)
	vals := make([]node, len(names))
	for i, name := range names {
		val, err := d.decodeChild(v.Fields[name], lex, "construct field "+name)
		if err != nil {
			return nil, err
		}
		vals[i] = val
	}
	return constructNode{entityType: v.EntityType, fieldNames: names, fieldVals: vals}, nil
}

// canonicalOrder reproduces ext/compute/eval.go::canonicalSorted — ECF canonical
// map key order: sort by encoded byte length, then lexicographically.
func canonicalOrder(m map[string]hash.Hash) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		li, lj := len(names[i]), len(names[j])
		if li != lj {
			return li < lj
		}
		return names[i] < names[j]
	})
	return names
}

// decodeApply splits compute/apply into the fast collection builtins and
// everything else.
//
// map/filter/fold are where the F-D2 O(N²) lives, so they get resolved nodes.
// The rest — handler dispatch, builtins/store, the capability machinery, and
// the inline aliases — goes to the Stage-1 seam. The inline aliases
// (builtins/arithmetic, /compare, /field, /construct, /logic) could be decoded
// to their inline nodes, but they are not on any measured hot path and Stage-1
// implements them by rebuilding the inline entity anyway (builtinViaInline,
// ext/compute/builtins.go), so a fallback is both cheaper to trust and
// semantically identical.
func (d *decoder) decodeApply(ent entity.Entity, lex *compileLevel) (node, error) {
	var v types.ComputeApplyData
	if err := ecf.Decode(ent.Data, &v); err != nil {
		return nil, err
	}

	// Mirror Stage-1's dispatch shape (ext/compute/eval_apply.go::evalApply): a
	// Path is builtin/handler dispatch; an empty Path with an Fn is a CLOSURE
	// application; neither is the error. Take the closure branch first, because a
	// closure application carries no Operation (builder.go::applyClosure) and
	// would otherwise be swallowed by the Operation!="eval" fallback below.
	if v.Path == "" {
		if !v.Fn.IsZero() {
			return d.decodeApplyClosure(ent, v, lex)
		}
		// "compute/apply requires path or fn" — let Stage-1 raise it verbatim.
		return fallbackNode{ent: ent}, nil
	}

	// Path != "": builtin or handler dispatch. Operation MUST be "eval" for
	// builtins (§9.2); anything else is Stage-1's to reject, with its exact message.
	if v.Operation != "eval" {
		return fallbackNode{ent: ent}, nil
	}

	switch v.Path {
	case compute.BuiltinMap, compute.BuiltinFilter:
		coll, fn, err := d.decodeCollArgs(v, lex)
		if err != nil {
			return nil, err
		}
		if coll == nil || fn == nil {
			return fallbackNode{ent: ent}, nil
		}
		if v.Path == compute.BuiltinMap {
			return mapNode{coll: coll, fn: fn}, nil
		}
		return filterNode{coll: coll, fn: fn}, nil

	case compute.BuiltinFold:
		coll, fn, err := d.decodeCollArgs(v, lex)
		if err != nil {
			return nil, err
		}
		initialHash, ok := v.Args["initial"]
		if coll == nil || fn == nil || !ok {
			return fallbackNode{ent: ent}, nil
		}
		initial, err := d.decodeChild(initialHash, lex, "initial")
		if err != nil {
			return nil, err
		}
		return foldNode{coll: coll, fn: fn, initial: initial}, nil

	default:
		return fallbackNode{ent: ent}, nil
	}
}

// decodeApplyClosure decodes a closure application — compute/apply with an Fn
// hash and named Args, no Path (builder.go::applyClosure). The fn expression is
// decoded in the caller's lexical scope (it resolves to a live closure at eval);
// the args are decoded here too but stay keyed by param NAME, since the slot
// order is the closure's, bound at eval when fn is known.
//
// The fn is usually a lookup/tree (the fixpoint self-reference) or an inline
// lambda; both are native. A fn hash pointing at anything else is decoded like
// any child — if it fails to resolve to a live closure at eval, evalApply routes
// the whole application to Stage-1 from ent.
func (d *decoder) decodeApplyClosure(ent entity.Entity, v types.ComputeApplyData, lex *compileLevel) (node, error) {
	fn, err := d.decodeChild(v.Fn, lex, "apply fn")
	if err != nil {
		return nil, err
	}
	args := make(map[string]node, len(v.Args))
	for name, h := range v.Args {
		an, err := d.decodeChild(h, lex, "apply arg "+name)
		if err != nil {
			return nil, err
		}
		args[name] = an
	}
	return applyNode{fn: fn, args: args, ent: ent}, nil
}

// decodeCollArgs decodes the shared collection/fn args of map/filter/fold.
// A missing arg yields (nil, nil, nil) so the caller falls back and lets
// Stage-1 produce the canonical error message rather than duplicating it.
func (d *decoder) decodeCollArgs(v types.ComputeApplyData, lex *compileLevel) (coll, fn node, err error) {
	collHash, ok := v.Args["collection"]
	if !ok {
		return nil, nil, nil
	}
	fnHash, ok := v.Args["fn"]
	if !ok {
		return nil, nil, nil
	}
	coll, err = d.decodeChild(collHash, lex, "collection")
	if err != nil {
		return nil, nil, err
	}
	fn, err = d.decodeChild(fnHash, lex, "fn")
	if err != nil {
		return nil, nil, err
	}
	return coll, fn, nil
}
