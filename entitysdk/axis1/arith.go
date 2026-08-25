package axis1

import (
	"fmt"
	"math"

	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/ext/compute"
)

// Arithmetic, comparison, cast, and truthiness — re-derived from the Stage-1
// reference (ext/compute/eval_arith.go @ core-go 769a888), which exports none
// of it.
//
// This file is the drift risk. Life and Snake exercise none of the corners
// here — no NumericCast, no wraparound, no float, no unsigned — so the probe
// suites cannot catch a mistake in it. The differential harness is what covers
// this file, and it is why the harness is property-based rather than a handful
// of examples (see COMPUTE-AXIS1-ORACLE-GAPS-2026-07-16.md §4.1).
//
// The rules, per EXTENSION-COMPUTE §2.2 (v3.16/v3.18 integer model — WASM/LLVM
// style: integer types are widths, signedness is a property of OPERATIONS, not
// of values):
//
//	Rule 1:  float promotion — either operand float ⇒ both promoted; result float.
//	Rule 4:  mod is integer-only; a float operand → type_mismatch.
//	Rule 5:  integer div/mod by 0 → division_by_zero; float div by 0 → IEEE 754.
//	Rule 6:  non-numeric operand → type_mismatch.
//	Rule 8:  add/sub/mul are sign-agnostic 64-bit two's-complement (one wrap at
//	         2^64). No int/uint decision, no mixed case.
//	Rule 9:  div/mod/compare use SIGNED interpretation by default. Unsigned is
//	         requested by a numeric-cast → primitive/uint on the operand
//	         immediately before the op.
//	Rule 10: integer results encode by their signed two's-complement
//	         interpretation — return int64, so the CBOR encoder writes a
//	         bit-63-set value as major type 1.
//	Rule 11: numeric-cast is EAGER — consumed by the immediately-following op,
//	         and it does NOT flow through a let. Detected from the operand
//	         entity's type, which is why the decoder hoists it (decode.go).
//
// Rule 9's "div returns a float when the quotient is not exact" is the POC's
// F-D1 footgun: a non-exact div yields a float that then poisons a downstream
// mod ("Modulo requires integer operands"). Integer floor-div lowers as
// div(sub(a, mod(a,b)), b).

// applyArithmetic mirrors ext/compute/eval_arith.go::applyArithmeticWithIntent.
func applyArithmetic(op string, left, right interface{}, unsignedHint bool) (interface{}, error) {
	_, lFloat := left.(float64)
	_, rFloat := right.(float64)
	lBits, lInt := bits64(left)
	rBits, rInt := bits64(right)

	// Rule 6.
	if !lFloat && !lInt {
		return nil, newError(compute.ErrTypeMismatch,
			fmt.Sprintf("Arithmetic requires numeric operands, got %T", left))
	}
	if !rFloat && !rInt {
		return nil, newError(compute.ErrTypeMismatch,
			fmt.Sprintf("Arithmetic requires numeric operands, got %T", right))
	}

	// Rule 1 + Rule 4: float promotion applies to add/sub/mul/div, NOT mod.
	if lFloat || rFloat {
		if op == "mod" {
			return nil, newError(compute.ErrTypeMismatch, "Modulo requires integer operands")
		}
		lf, _ := toFloat64(left)
		rf, _ := toFloat64(right)
		switch op {
		case "add":
			return lf + rf, nil
		case "sub":
			return lf - rf, nil
		case "mul":
			return lf * rf, nil
		case "div":
			return lf / rf, nil // Rule 5: IEEE 754 special values, no error.
		default:
			return nil, newError(compute.ErrInvalidExpression, "Unknown arithmetic op: "+op)
		}
	}

	// Rule 8: sign-agnostic 64-bit two's-complement. Go's uint64 arithmetic
	// wraps natively, so the wraparound is free — the discipline is simply
	// never to coerce uint64 → int64 before operating.
	switch op {
	case "add":
		return signedResult(lBits + rBits), nil
	case "sub":
		return signedResult(lBits - rBits), nil
	case "mul":
		return signedResult(lBits * rBits), nil

	// Rule 9: div/mod are signed by default; the cast hint flips them unsigned.
	case "div":
		if rBits == 0 {
			return nil, newError(compute.ErrDivisionByZero, "Division by zero")
		}
		if unsignedHint {
			if lBits%rBits == 0 {
				return unsignedResult(lBits / rBits), nil
			}
			return float64(lBits) / float64(rBits), nil
		}
		l, r := int64(lBits), int64(rBits)
		if l%r == 0 {
			return signedResult(uint64(l / r)), nil
		}
		// F-D1: non-exact quotient ⇒ FLOAT, which will poison a downstream mod.
		return float64(l) / float64(r), nil
	case "mod":
		if rBits == 0 {
			return nil, newError(compute.ErrDivisionByZero, "Division by zero")
		}
		if unsignedHint {
			return unsignedResult(lBits % rBits), nil
		}
		l, r := int64(lBits), int64(rBits)
		// Truncated mod — Go's % already matches the sign-of-dividend rule.
		return signedResult(uint64(l % r)), nil
	default:
		return nil, newError(compute.ErrInvalidExpression, "Unknown arithmetic op: "+op)
	}
}

// bits64 extracts the 64-bit pattern from an integer value. int64 and uint64
// share a memory layout, so reinterpretation is bit-identical.
func bits64(v interface{}) (uint64, bool) {
	switch n := v.(type) {
	case int64:
		return uint64(n), true
	case uint64:
		return n, true
	}
	return 0, false
}

// signedResult returns int64 per Rule 10: arithmetic results encode by their
// signed two's-complement interpretation, which is what gives cross-impl wire
// agreement (Go's CBOR encoder writes a bit-63-set int64 as major type 1).
func signedResult(bits uint64) int64 { return int64(bits) }

// unsignedResult returns uint64 — only when the op was unsigned via cast
// intent. Per A.5 a result meant as unsigned in [2^63, 2^64) encodes as CBOR
// major type 0.
func unsignedResult(bits uint64) uint64 { return bits }

// applyCompare mirrors ext/compute/eval_arith.go::applyCompareWithIntent.
// Equality is sign-independent; ordering honors the unsigned hint (Rule 9).
func applyCompare(op string, left, right interface{}, unsignedHint bool) (interface{}, error) {
	switch op {
	case "eq":
		return compareEqual(left, right), nil
	case "neq":
		return !compareEqual(left, right), nil
	case "lt", "gt", "lte", "gte":
		return compareOrdered(op, left, right, unsignedHint)
	default:
		return nil, newError(compute.ErrInvalidExpression, "Unknown compare op: "+op)
	}
}

// compareEqual mirrors ext/compute/eval_arith.go::compareEqual.
func compareEqual(a, b interface{}) bool {
	if !sameTypeClass(a, b) {
		return false
	}
	// Entities compare by content hash.
	if ae, ok := a.(entity.Entity); ok {
		if be, ok := b.(entity.Entity); ok {
			return ae.ContentHash == be.ContentHash
		}
		return false
	}
	// Numeric: promote to float for cross-type comparison.
	af, aOk := toFloat64(a)
	bf, bOk := toFloat64(b)
	if aOk && bOk {
		return af == bf
	}
	// Same primitive type: Stage-1 compares formatted values. Transcribed
	// verbatim — %v on two same-class primitives is total here, and diverging
	// (e.g. using ==) would change behavior for types whose formatting collides.
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// sameTypeClass mirrors ext/compute/eval_arith.go::sameTypeClass (v3.6 A2):
// numeric values are comparable across int/float; everything else must match
// its concrete type.
func sameTypeClass(a, b interface{}) bool {
	_, aNum := toFloat64(a)
	_, bNum := toFloat64(b)
	if aNum && bNum {
		return true
	}
	switch a.(type) {
	case string:
		_, ok := b.(string)
		return ok
	case bool:
		_, ok := b.(bool)
		return ok
	case entity.Entity:
		_, ok := b.(entity.Entity)
		return ok
	}
	return false
}

// compareOrdered mirrors ext/compute/eval_arith.go::compareOrdered.
func compareOrdered(op string, left, right interface{}, unsignedHint bool) (bool, error) {
	lb, lIntBits := bits64(left)
	rb, rIntBits := bits64(right)
	_, lFloat := left.(float64)
	_, rFloat := right.(float64)

	// Integer-integer: signed by default (Rule 9), unsigned via eager cast.
	if lIntBits && rIntBits && !lFloat && !rFloat {
		if unsignedHint {
			switch op {
			case "lt":
				return lb < rb, nil
			case "gt":
				return lb > rb, nil
			case "lte":
				return lb <= rb, nil
			case "gte":
				return lb >= rb, nil
			}
		} else {
			l, r := int64(lb), int64(rb)
			switch op {
			case "lt":
				return l < r, nil
			case "gt":
				return l > r, nil
			case "lte":
				return l <= r, nil
			case "gte":
				return l >= r, nil
			}
		}
	}

	// Float promotion.
	lf, lok := toFloat64(left)
	rf, rok := toFloat64(right)
	if lok && rok {
		switch op {
		case "lt":
			return lf < rf, nil
		case "gt":
			return lf > rf, nil
		case "lte":
			return lf <= rf, nil
		case "gte":
			return lf >= rf, nil
		}
	}

	// String comparison.
	ls, lsOk := left.(string)
	rs, rsOk := right.(string)
	if lsOk && rsOk {
		switch op {
		case "lt":
			return ls < rs, nil
		case "gt":
			return ls > rs, nil
		case "lte":
			return ls <= rs, nil
		case "gte":
			return ls >= rs, nil
		}
	}
	return false, newError(compute.ErrTypeMismatch,
		fmt.Sprintf("Cannot compare %T and %T with %s", left, right, op))
}

// applyNumericCast mirrors ext/compute/eval_arith.go::evalNumericCast (§2.2):
// intra-numeric conversion among primitive/{int,uint,float}. int↔uint
// reinterpret bits at 64-bit width; int/uint→float is native (defined-lossy
// above 2^53); float→int/uint truncates toward zero, with NaN/±Inf/out-of-range
// → cast_out_of_range.
func applyNumericCast(val interface{}, toType string) (interface{}, error) {
	switch toType {
	case "primitive/int":
		switch v := val.(type) {
		case int64:
			return v, nil
		case uint64:
			return int64(v), nil
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, newError(compute.ErrCastOutOfRange,
					"cannot cast NaN or ±Inf to primitive/int")
			}
			t := math.Trunc(v)
			if t < math.MinInt64 || t >= float64(math.MaxInt64)+1 {
				return nil, newError(compute.ErrCastOutOfRange,
					fmt.Sprintf("float %v out of range for primitive/int", v))
			}
			return int64(t), nil
		default:
			return nil, newError(compute.ErrTypeMismatch,
				fmt.Sprintf("compute/numeric-cast value must be numeric, got %T", val))
		}
	case "primitive/uint":
		switch v := val.(type) {
		case int64:
			return uint64(v), nil
		case uint64:
			return v, nil
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, newError(compute.ErrCastOutOfRange,
					"cannot cast NaN or ±Inf to primitive/uint")
			}
			t := math.Trunc(v)
			if t < 0 || t >= float64(math.MaxUint64)+1 {
				return nil, newError(compute.ErrCastOutOfRange,
					fmt.Sprintf("float %v out of range for primitive/uint", v))
			}
			return uint64(t), nil
		default:
			return nil, newError(compute.ErrTypeMismatch,
				fmt.Sprintf("compute/numeric-cast value must be numeric, got %T", val))
		}
	case "primitive/float":
		switch v := val.(type) {
		case int64:
			return float64(v), nil
		case uint64:
			return float64(v), nil
		case float64:
			return v, nil
		default:
			return nil, newError(compute.ErrTypeMismatch,
				fmt.Sprintf("compute/numeric-cast value must be numeric, got %T", val))
		}
	default:
		return nil, newError(compute.ErrTypeMismatch,
			"compute/numeric-cast to_type must be primitive/int, primitive/uint, or primitive/float; got "+toType)
	}
}

// toFloat64 mirrors ext/compute/eval_arith.go::toFloat64.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// truthy mirrors ext/compute/eval.go::truthy (§4.5).
func truthy(value interface{}) bool {
	if value == nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case int64:
		return v != 0
	case uint64:
		return v != 0
	case float64:
		return v != 0
	case string:
		return v != ""
	case []interface{}:
		return len(v) > 0
	}
	return true
}

// asIndex mirrors ext/compute/eval_construct.go::asIndex. It returns three
// facts, not two, because "is this an integer?" and "does it fit int64?" are
// distinct questions with distinct error codes (F-2 / R3 §9.1):
//
//   - isInt   — an int64 or a uint64 is a well-formed integer index argument;
//     anything else (a float, a string, …) is a type_mismatch.
//   - inInt64 — whether the magnitude fits int64. A uint64 above MaxInt64 does
//     NOT fit, but is STILL a valid integer index — it is just
//     necessarily out of range (no array approaches 2⁶³ elements), so
//     it is index_out_of_range, not a type error.
//
// The earlier two-value asInt64Index collapsed these, answering type_mismatch
// for a uint64 above MaxInt64 where the ruling (Rust ruled correct over Go's
// old type_mismatch) says index_out_of_range. This is that fix, transcribed.
func asIndex(v interface{}) (idx int64, isInt, inInt64 bool) {
	switch n := v.(type) {
	case int64:
		return n, true, true
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), true, true
		}
		return 0, true, false
	}
	return 0, false, false
}

// indexMagnitude mirrors ext/compute/eval_construct.go::indexMagnitude: render a
// uint64 index that overflowed int64 by its true unsigned value, so the
// out-of-range message reports the magnitude the caller actually passed.
func indexMagnitude(v interface{}, idx int64, inInt64 bool) string {
	if !inInt64 {
		if u, ok := v.(uint64); ok {
			return fmt.Sprintf("%d", u)
		}
	}
	return fmt.Sprintf("%d", idx)
}

// toStringMap mirrors ext/compute/handler.go::toStringMap.
func toStringMap(v interface{}) map[string]interface{} {
	switch m := v.(type) {
	case map[string]interface{}:
		return m
	case map[interface{}]interface{}:
		result := make(map[string]interface{}, len(m))
		for k, val := range m {
			if ks, ok := k.(string); ok {
				result[ks] = val
			}
		}
		return result
	}
	return nil
}
