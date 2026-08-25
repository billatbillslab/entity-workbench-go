package axis1

import (
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/compute"
)

// ERROR-AS-VALUE — the half of the semantics this engine did not have.
//
// §1.5 makes a compute/error an ordinary VALUE, so at every point where a
// sub-expression's result is used there are two representations that must be
// treated identically (§2.4 provenance independence):
//
//   - MINTED — the evaluator raised it, so it arrives as a Go error.
//   - VALUE-FORM — a compute/error ENTITY arrived as an ordinary result: read
//     out of the tree, threaded in as a root binding, or (now) taken out of a
//     collection where a previous primitive CONTAINED it.
//
// Stage-1 keys every decision on the position, never on which representation
// showed up. There are exactly three position kinds, and this file is the
// transcription of all three from ext/compute:
//
//	CONSUMED  — the result is READ (an arithmetic operand, an if condition, a
//	            filter predicate, an index, a collection). Both representations
//	            SHORT-CIRCUIT. Chokepoint: evaluator.operand, mirroring
//	            ext/compute/eval.go::evalOperand.
//	CONTAINED — the result is PLACED somewhere without being read (map's output
//	            element, fold's accumulator and initial). Both representations
//	            become a value in that slot — the §1.5 NaN model — EXCEPT a
//	            short-circuit limit code. Chokepoint: containClosureResult.
//	BOUNDARY  — materialization, where a contained error element reduces to
//	            code-only. See materialize().
//
// Before this file, axis1 handled only the minted arm and only by propagating
// it, which is the CONSUMED rule applied everywhere. That is why
// `length(map(arr, λe. e + oob_index))` answered index_out_of_range here and 4
// under the reference: the map element is a CONTAINED position and the error
// belonged in the array, not in the return value. Cases 9, 28 and 79 of the
// differential sweep were three instances of that one shape.

// errorFromValue recognizes the VALUE-FORM arm. Mirrors
// ext/compute/errors.go::computeErrorFromValue.
//
// A malformed compute/error is itself an error rather than a value, and is
// surfaced loudly — same as the reference, for the same reason: embedding a
// broken error as a value hides the decode failure inside a well-formed result.
func errorFromValue(v interface{}) (*compute.ComputeError, bool) {
	ent, ok := v.(entity.Entity)
	if !ok || ent.Type != types.TypeComputeError {
		return nil, false
	}
	d, err := types.ComputeErrorDataFromEntity(ent)
	if err != nil {
		return newError(compute.ErrInvalidExpression,
			"malformed compute/error value: "+err.Error()), true
	}
	ce := &compute.ComputeError{Code: d.Code, Message: d.Message, At: d.At}
	if d.Expression != nil {
		ce.Expression = *d.Expression
	}
	return ce, true
}

// isShortCircuitLimitCode reports whether a code short-circuits even in a
// CONTAINED position. Mirrors ext/compute/builtins.go::isShortCircuitLimitCode,
// including its reasoning, which is worth keeping because the obvious rule
// ("limit codes short-circuit") is the wrong one:
//
// the discriminator is whether the counter is RESTORED ON UNWIND. `operations`
// is not (§8.1) and the §7.3 cascade counter is not (§8.2), so containing them
// would make element i's outcome a function of elements 1…i−1 — two conformant
// peers could exhaust at a different element and produce different boundary
// bytes for one program. `depth` IS restored (§5.1), so depth_exceeded is
// element-local and CONTAINS like any ordinary error (§8.3) — no carve-out.
func isShortCircuitLimitCode(code string) bool {
	switch code {
	case compute.ErrBudgetExhausted, compute.ErrCascadeLimit:
		return true
	}
	return false
}

// containClosureResult is the ONE rule for a CONTAINED closure-result position,
// covering both arms symmetrically. Mirrors
// ext/compute/builtins.go::containClosureResult (and containOrPropagate, folded
// in — axis1 has no call site that needs the minted-only form).
//
// Callers pass the (value, err) pair an invocation returned:
//
//	contained, perr := containClosureResult(e.invokeClosure(...))
//	if perr != nil { return nil, perr }   // limit-code short-circuit, or infra
//	out = append(out, contained)          // error contained, or an ordinary value
//
// The minted arm converts to the error's ENTITY form so that a minted error and
// a value-form error produce identical bytes at the boundary (§2.4). This must
// stay the single implementation shared by map and fold: it determines boundary
// hashes, and the charter's rule is that a hash-determining concept implemented
// twice is implemented wrong.
func containClosureResult(v interface{}, err error) (interface{}, error) {
	if err != nil {
		ce, ok := err.(*compute.ComputeError)
		if !ok || isShortCircuitLimitCode(ce.Code) {
			return nil, err
		}
		errEnt, eerr := ce.ToEntity()
		if eerr != nil {
			return nil, eerr
		}
		return errEnt, nil
	}
	if ce, isErr := errorFromValue(v); isErr && isShortCircuitLimitCode(ce.Code) {
		return nil, ce
	}
	return v, nil
}
