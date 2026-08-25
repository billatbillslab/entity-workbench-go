package axis1

import (
	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/compute"
)

// This file is the boundary contract (exploration §13.5) — the ONE place where
// bit-identity with Stage-1 is mandatory and not this engine's call.
//
// The interior is free: live frames, resolved nodes, closures held by pointer.
// The moment a value crosses into the entity system it must be
// canonical-CBOR bit-identical and same-content-hash as what Stage-1 produces
// for the same logical entity. Everything below is transcribed from
// ext/compute/eval_construct.go::materialize with that as the only goal.

// constructedValue is the in-flight form of a compute/construct result — the
// axis1 counterpart of ext/compute/eval_construct.go::constructedValue (v3.19c
// Part A M3: typed in-flight, never a sniffable wire shape).
//
// Field values are held as TYPED Go values: entity.Entity for entity-kind,
// anything else for value-kind. Navigation (field/index/length) reads them off
// this structure directly — no kind tag in any wire data, no shape sniffing.
//
// It must be a distinct type from Stage-1's because Stage-1's is unexported.
// That has one real consequence, handled by lowerForStage1 below: an axis1
// constructedValue cannot be handed to Stage-1's evaluator as-is.
type constructedValue struct {
	entityType string
	fields     map[string]interface{}
}

// materialize converts an in-flight compute value into its bare wire form
// (v3.19c M1 + M3). Mirrors ext/compute/eval_construct.go::materialize.
//
// A *constructedValue becomes an entity.Entity whose data follows V7 §1.4:
// entity-kind fields become bare 33-byte system/hash content refs, value-kind
// fields inline as their raw value. Recursive — a constructed entity nested as
// a field value is materialized (and stored) first, then referenced by hash in
// its parent.
//
// Inputs that aren't *constructedValue pass through unchanged (entity.Entity is
// already materialized; primitives, arrays, maps are bare).
//
// Like the reference, this does NOT recurse into Go maps: construct fields live
// on *constructedValue (recursed here) and no current op produces a naked
// map[string]interface{} carrying constructed values across a boundary. If one
// ever does (compute/record, compute/merge), both this and the reference must
// grow map recursion together.
func materialize(v interface{}, cs store.ContentStore) (interface{}, error) {
	switch t := v.(type) {
	case *constructedValue:
		dataMap := make(map[string]interface{}, len(t.fields))
		for k, fv := range t.fields {
			mv, err := materialize(fv, cs)
			if err != nil {
				return nil, err
			}
			// Per M1: an entity-typed materialized field becomes a bare
			// system/hash ref (V7 §1.4); hash.Hash's MarshalCBOR emits the
			// 33-byte bytestring.
			if ent, ok := mv.(entity.Entity); ok {
				dataMap[k] = ent.ContentHash
			} else {
				dataMap[k] = mv
			}
		}
		raw, err := ecf.Encode(dataMap)
		if err != nil {
			return nil, err
		}
		ent, err := entity.NewEntity(t.entityType, raw)
		if err != nil {
			return nil, err
		}
		if cs != nil {
			if _, err := cs.Put(ent); err != nil {
				return nil, err
			}
		}
		return ent, nil
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			me, err := materialize(e, cs)
			if err != nil {
				return nil, err
			}
			if ent, ok := me.(entity.Entity); ok {
				out[i] = ent.ContentHash
			} else {
				out[i] = me
			}
		}
		return out, nil
	case *closure:
		// A live closure crossing the boundary must become the artifacts
		// Stage-1 would have built all along: a compute/scope entity for the
		// captured environment and a compute/closure entity referencing it.
		//
		// This is the §13.4 thesis stated precisely — the CaptureScope round-trip
		// is not WRONG, it is just an interpretation artifact that is only
		// REQUIRED here, at the crossing. Interior map/fold never reaches this
		// line, which is exactly why deleting it inside the loop is legal.
		return materializeClosure(t, cs)
	default:
		return v, nil
	}
}

// materializeClosure builds the compute/closure + compute/scope entities for a
// live closure that is crossing the boundary. Mirrors what
// ext/compute/eval.go::evalLambda does eagerly on every lambda evaluation.
//
// The body hash is not recoverable from a decoded node, so a live closure keeps
// its source hashes (lambdaNode.bodyHash / paramNames) for exactly this path.
func materializeClosure(cl *closure, cs store.ContentStore) (interface{}, error) {
	if cs == nil {
		return nil, newError(compute.ErrInvalidExpression,
			"axis1: cannot materialize a closure without a content store")
	}
	// Rebuild the captured environment as a Stage-1 scope and capture it the
	// way the reference does, so the scope entity is byte-identical.
	flat := cl.env.flatten(nil)
	lowered := make(map[string]interface{}, len(flat))
	for name, v := range flat {
		lv, err := lowerForStage1(v, cs)
		if err != nil {
			return nil, err
		}
		lowered[name] = lv
	}
	envEnt, err := compute.CaptureScope(toScope(lowered), cs)
	if err != nil {
		return nil, err
	}
	closureData := types.ComputeClosureData{
		Params: cl.node.params,
		Body:   cl.node.bodyHash,
	}
	if !envEnt.ContentHash.IsZero() {
		closureData.Env = &envEnt.ContentHash
	}
	ent, err := closureData.ToEntity()
	if err != nil {
		return nil, err
	}
	if _, err := cs.Put(ent); err != nil {
		return nil, err
	}
	return ent, nil
}

// lowerForStage1 converts an axis1 interior value into something the Stage-1
// evaluator understands, for the fallback seam and for closure capture.
//
// Stage-1 cannot see an axis1 *constructedValue (its own is a different,
// unexported type) and would misroute it through evalField's default branch,
// silently returning "Field access requires an entity or record value" or
// worse, navigating the wrong shape. So it is materialized first.
//
// That is not a semantic change smuggled in at the seam: Stage-1's own
// CaptureScope does exactly this (buildScopeBinding materializes a
// *constructedValue before binding it — ext/compute/scope.go), so any value
// crossing into a captured scope is already materialized under Stage-1 too.
// The seam matches the reference's behavior at the same kind of crossing.
func lowerForStage1(v interface{}, cs store.ContentStore) (interface{}, error) {
	switch v.(type) {
	case *constructedValue, *closure, []interface{}:
		return materialize(v, cs)
	default:
		return v, nil
	}
}

// Materialize is the public boundary crossing: turn an engine result into the
// bare entity form callers write to the tree.
//
// Callers use it exactly where Stage-1's handler does — the eval-return
// boundary — so a materialized state' entity has the same content hash under
// either engine. That equality IS the conformance surface (§13.5), and it is
// what the differential harness asserts.
func Materialize(v interface{}, cs store.ContentStore) (interface{}, error) {
	return materialize(v, cs)
}
