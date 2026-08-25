package entitysdk_test

// Phase H.3 battle-test scenarios.
//
// End-to-end realistic compute use cases that exercise the S1 builder
// + H.2 lowering toolkit + S5 unwrap helper under conditions a
// workbench app would actually hit. Each scenario tests a recognizable
// shape of work and surfaces friction documented inline as comments
// (FRICTION/FOUND) for the H.3 battle-test memo to arch.
//
// Findings discovered while building these:
//
//   - **F11 — BuiltinsCall arg-name inconsistency.** filter uses
//     "predicate"; fold/map use "fn". Cross-builtin spec asymmetry
//     forces authors to remember per-builtin convention. A
//     LowerFilter/LowerMap helper would hide it.
//
//   - **F9 secondary manifestation — closure-captured params.** A
//     filter/map predicate that closes over a scope-typed value (the
//     common case: `params.threshold` in a filter predicate) fails
//     for the same reason as F9's headline Field-chain case. The
//     captured params loses its entity.Entity type on the CBOR scope
//     round-trip (scope.go::CaptureScope+LoadScope encode bindings
//     as map[string]interface{}), so the lambda body's
//     Field(captured-params, ...) hits evalField's bare-map rejection.
//     **Impact: parametric filter/map predicates are unreachable
//     pre-F9-fix.** Hardcoded predicates work; closures over scope
//     values don't.
//
//   - **F9 also blocks compute→compute composition** (Scenario C).
//     Outer compute calling inner compute needs Field("value") on
//     the compute/result envelope, then Field("...") on the inner
//     record — a two-level Field chain.

import (
	"context"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// respData returns resp.Data or nil if resp is nil. Test helper.
func respData(r *entitysdk.Response) []byte {
	if r == nil {
		return nil
	}
	return r.Data
}

// --- Scenario A.1: filter + sum (literal threshold — works) -----------

// TestScenarioA1_FilterAndSum_LiteralThreshold — filter+fold composes
// when the predicate uses an inlined literal (no closure capture of
// scope-typed values). The toolkit's BuiltinsCall("filter") feeds
// LowerFold via the collection-arg threading.
func TestScenarioA1_FilterAndSum_LiteralThreshold(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/scenarios/filter-sum-literal"
	c := ap.Compute()
	predicate := c.Lambda([]string{"elem"},
		c.Compare("gt", c.LookupScope("elem"), c.Literal(uint64(5))))
	filtered := c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
		"collection": c.Field(c.LookupScope("params"), "numbers"),
		// Post-v3.19 (F11 closed): all three collection builtins use "fn".
		"fn": predicate,
	})
	expr := entitysdk.LowerFold(c, filtered, c.Literal(uint64(0)),
		func(acc, elem *entitysdk.Builder) *entitysdk.Builder {
			return c.Arithmetic("add", acc, elem)
		})
	h, err := ap.RegisterComputeHandler(context.Background(), entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    "filter-sum-literal",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, expr)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	params, _ := entitysdk.PrimitiveAny(map[string]interface{}{
		"numbers": []interface{}{uint64(1), uint64(3), uint64(7), uint64(2), uint64(10), uint64(5), uint64(8)},
	})
	resp, err := ap.Executor().ExecuteWithParams(pattern, "compute", params)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d (type=%s)", resp.Status, resp.Type)
	}
	var got interface{}
	if err := ecf.Decode(resp.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !numEq(got, 25) {
		t.Fatalf("expected 25 (sum of {7,10,8}), got %v (%T)", got, got)
	}
	t.Logf("PASS Scenario A.1 (v3.19 F11): filter(>5) | fold(+) = 25 — all three collection builtins now use \"fn\" as the lambda arg key")
}

// --- Scenario A.2: filter + sum (parametric threshold — F9-blocked) ---

// TestScenarioA2_FilterAndSum_ParametricThreshold — same shape as
// A.1 but with the threshold supplied via params, accessed via
// closure-captured `Field(scope.params, "threshold")` inside the
// predicate body.
//
// **F9 manifestation B — residual gap.** Core-go's v3.19 N.5 fix
// (`evalField` accepts bare maps) closes A and C. For B (this test),
// the closure works, but `CaptureScope`+`LoadScope` round-trip
// (`scope.go:53/75`) encodes the captured `params` entity.Entity as
// its envelope: post-round-trip, `bindings["params"]` is
// `map[type:..., data:{numbers, threshold}, content_hash:...]`.
// `Field("threshold")` on this map looks for "threshold" in the
// envelope's top-level keys (type/data/content_hash), not in the
// nested data map. Result: `compute/error code=not_found`,
// `message="Field not found: threshold"`.
//
// **Workaround:** see Scenario A.3 — bind the threshold via Let
// before the lambda, so the closure captures the unwrapped primitive
// value rather than the entity envelope.
//
// **What core-go would need to fully close B:** either
// `evalLookupScope` unwraps entity-envelope-shaped values to their
// `.data` on lookup, or `CaptureScope` unwraps entity.Entity
// bindings to their decoded data before encoding. Filed in friction
// log as F9-B-residual.
func TestScenarioA2_FilterAndSum_ParametricThreshold(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/scenarios/filter-sum-parametric"
	c := ap.Compute()
	threshold := c.Field(c.LookupScope("params"), "threshold")
	predicate := c.Lambda([]string{"elem"},
		c.Compare("gt", c.LookupScope("elem"), threshold))
	filtered := c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
		"collection": c.Field(c.LookupScope("params"), "numbers"),
		"fn":         predicate,
	})
	expr := entitysdk.LowerFold(c, filtered, c.Literal(uint64(0)),
		func(acc, elem *entitysdk.Builder) *entitysdk.Builder {
			return c.Arithmetic("add", acc, elem)
		})
	h, err := ap.RegisterComputeHandler(context.Background(), entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    "filter-sum-parametric",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, expr)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	params, _ := entitysdk.PrimitiveAny(map[string]interface{}{
		"numbers":   []interface{}{uint64(1), uint64(3), uint64(7), uint64(2), uint64(10), uint64(5), uint64(8)},
		"threshold": uint64(5),
	})
	resp, err := ap.Executor().ExecuteWithParams(pattern, "compute", params)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d (type=%s)", resp.Status, resp.Type)
	}

	// Today (v3.19 + F9-B-residual): expect compute/error not_found.
	// When F9-B-residual closes upstream this flips to the success
	// path — assertions at the bottom catch either outcome.
	switch resp.Type {
	case types.TypeComputeError:
		var ed types.ComputeErrorData
		mustDecode(t, resp.Data, &ed)
		if ed.Code != "not_found" {
			t.Fatalf("expected not_found (envelope-shaped lookup), got code=%q msg=%q", ed.Code, ed.Message)
		}
		t.Logf("F9-B-RESIDUAL (documented): closure-captured params lookup returns compute/error{code=%q, message=%q} because CaptureScope round-trips entity.Entity as envelope shape. Use Let-binding workaround (see Scenario A.3).", ed.Code, ed.Message)
	default:
		var got interface{}
		if err := ecf.Decode(resp.Data, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !numEq(got, 25) {
			t.Fatalf("expected 25, got %v (%T)", got, got)
		}
		t.Logf("F9-B-RESIDUAL RESOLVED: closure-captured Field navigation works end-to-end; parametric-threshold filter+sum = 25")
	}
}

// --- Scenario A.3: parametric threshold via Let-binding (workaround) --

// TestScenarioA3_FilterAndSum_ParametricViaLet — same intent as A.2
// (filter by a parameter from params) but uses Let to bind the
// threshold *before* the lambda. The closure captures the Let-
// binding's primitive value (uint64), so the envelope-shape issue
// from A.2 (F9-B-residual) doesn't apply.
//
// This is the working pattern for parametric closures today. When
// F9-B-residual closes upstream, A.2's natural pattern works too;
// until then, this is what frontends should use.
func TestScenarioA3_FilterAndSum_ParametricViaLet(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/scenarios/filter-sum-via-let"
	c := ap.Compute()
	// Let binds threshold to the primitive uint64 (not the params
	// entity) so the closure captures the unwrapped value.
	predicate := c.Lambda([]string{"elem"},
		c.Compare("gt", c.LookupScope("elem"), c.LookupScope("threshold")))
	filtered := c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
		"collection": c.Field(c.LookupScope("params"), "numbers"),
		"fn":         predicate,
	})
	sum := entitysdk.LowerFold(c, filtered, c.Literal(uint64(0)),
		func(acc, elem *entitysdk.Builder) *entitysdk.Builder {
			return c.Arithmetic("add", acc, elem)
		})
	expr := c.Let(map[string]*entitysdk.Builder{
		"threshold": c.Field(c.LookupScope("params"), "threshold"),
	}, sum)

	h, err := ap.RegisterComputeHandler(context.Background(), entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    "filter-sum-via-let",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, expr)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	params, _ := entitysdk.PrimitiveAny(map[string]interface{}{
		"numbers":   []interface{}{uint64(1), uint64(3), uint64(7), uint64(2), uint64(10), uint64(5), uint64(8)},
		"threshold": uint64(5),
	})
	resp, err := ap.Executor().ExecuteWithParams(pattern, "compute", params)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d (type=%s)", resp.Status, resp.Type)
	}
	var got interface{}
	if err := ecf.Decode(resp.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !numEq(got, 25) {
		t.Fatalf("expected 25 (sum of {7,10,8}), got %v (%T)", got, got)
	}
	t.Logf("PASS Scenario A.3 (workaround for F9-B-residual): parametric filter via Let-binding = 25. The Let-binding captures the unwrapped uint64 threshold so the closure doesn't hit the entity-envelope round-trip issue.")
}

// --- Scenario B: aggregation with typed record output -----------------

// TestScenarioB_AverageAggregation — given params.values (list of
// uints), return {count, sum, average} as a primitive/any record.
// Tests LowerRecord composing with LowerFold + LowerArithmetic.
//
// Real-use motivation: "summary statistics over a list" is the
// second-most-common aggregation pattern (after raw sum). A
// frontend rendering a chart or status badge needs structured
// output, not a single number.
func TestScenarioB_AverageAggregation(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/scenarios/average"
	c := ap.Compute()
	values := c.Field(c.LookupScope("params"), "values")
	sum := entitysdk.LowerFold(c, values, c.Literal(uint64(0)),
		func(acc, elem *entitysdk.Builder) *entitysdk.Builder {
			return c.Arithmetic("add", acc, elem)
		})
	count := c.Length(values)
	// average = sum / count (signed div is default; both operands are
	// uint64-typed at the source but signed div works correctly here
	// since both are small positive numbers).
	average := entitysdk.LowerArithmetic(c, entitysdk.SignedIntent, "div", sum, count)
	expr := entitysdk.LowerRecord(c, "primitive/any", map[string]interface{}{
		"count":   count,
		"sum":     sum,
		"average": average,
	})

	h, err := ap.RegisterComputeHandler(context.Background(), entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    "average",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, expr)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	params, _ := entitysdk.PrimitiveAny(map[string]interface{}{
		"values": []interface{}{uint64(10), uint64(20), uint64(30), uint64(40)},
	})
	resp, err := ap.Executor().ExecuteWithParams(pattern, "compute", params)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d (type=%s)", resp.Status, resp.Type)
	}

	rec, err := entitysdk.UnwrapComputeResultAsMap(resp)
	if err != nil {
		t.Fatalf("UnwrapComputeResultAsMap: %v", err)
	}
	if !numEq(rec["count"], 4) {
		t.Fatalf("count: expected 4, got %v (%T)", rec["count"], rec["count"])
	}
	if !numEq(rec["sum"], 100) {
		t.Fatalf("sum: expected 100, got %v (%T)", rec["sum"], rec["sum"])
	}
	if !numEq(rec["average"], 25) {
		t.Fatalf("average: expected 25, got %v (%T)", rec["average"], rec["average"])
	}
	t.Logf("PASS Scenario B: {count:4, sum:100, average:25} returned as a typed primitive/any record (via UnwrapComputeResultAsMap)")
	t.Logf("OBSERVATION: LowerRecord+LowerFold+LowerArithmetic compose cleanly. The `sum` builder is referenced twice (once for the record field, once via LowerArithmetic for average) and the IR de-duplicates at content-hash level — the same fold sub-expression is reachable under both Construct fields without re-evaluation.")
}

// --- Scenario C: two-stage compute pipeline (F9-blocked) --------------

// TestScenarioC_TwoStageComputePipeline — outer compute calls a
// registered inner compute handler via Apply, consumes the
// SA-4-wrapped result via Field("value") + Field("sum").
// Post-v3.19 N.5: compute→compute composition works — outer's
// two-level Field chain through the compute/result envelope
// composes per the navigation guarantee.
func TestScenarioC_TwoStageComputePipeline(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	// Inner handler: {sum, count} aggregation.
	innerPattern := "app/scenarios/two-stage/inner"
	c := ap.Compute()
	innerValues := c.Field(c.LookupScope("params"), "numbers")
	innerSum := entitysdk.LowerFold(c, innerValues, c.Literal(uint64(0)),
		func(acc, elem *entitysdk.Builder) *entitysdk.Builder {
			return c.Arithmetic("add", acc, elem)
		})
	innerCount := c.Length(innerValues)
	innerExpr := entitysdk.LowerRecord(c, "primitive/any", map[string]interface{}{
		"sum":   innerSum,
		"count": innerCount,
	})
	hInner, err := ap.RegisterComputeHandler(context.Background(), entitysdk.HandlerSpec{
		Pattern: innerPattern,
		Name:    "two-stage-inner",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, innerExpr)
	if err != nil {
		t.Fatalf("register inner: %v", err)
	}
	t.Cleanup(func() { _ = hInner.Close() })

	// Outer handler: call inner, extract sum, add 1000.
	outerPattern := "app/scenarios/two-stage/outer"
	c2 := ap.Compute()
	innerCall := c2.Apply(innerPattern, "compute", map[string]*entitysdk.Builder{
		"numbers": c2.Field(c2.LookupScope("params"), "numbers"),
	})
	// SA-4 wraps the inner record as compute/result {value, expression}.
	// To use the inner value, peel the envelope: Field("value") then
	// Field("sum"). This is the Field-chain F9 blocks.
	resultValue := c2.Field(innerCall, "value")
	innerSumField := c2.Field(resultValue, "sum")
	outerExpr := c2.Arithmetic("add", innerSumField, c2.Literal(uint64(1000)))

	hOuter, err := ap.RegisterComputeHandler(context.Background(), entitysdk.HandlerSpec{
		Pattern: outerPattern,
		Name:    "two-stage-outer",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, outerExpr)
	if err != nil {
		t.Fatalf("register outer: %v", err)
	}
	t.Cleanup(func() { _ = hOuter.Close() })

	params, _ := entitysdk.PrimitiveAny(map[string]interface{}{
		"numbers": []interface{}{uint64(10), uint64(20), uint64(30)},
	})
	resp, err := ap.Executor().ExecuteWithParams(outerPattern, "compute", params)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d (type=%s)", resp.Status, resp.Type)
	}
	var got interface{}
	if err := ecf.Decode(resp.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !numEq(got, 1060) {
		t.Fatalf("expected 1060 (inner sum 60 + 1000), got %v (%T)", got, got)
	}
	t.Logf("PASS Scenario C (v3.19 N.5): two-stage compute pipeline = 1060; compute→compute composition works")
}

// --- Scenario D: aggregate over a collection of records ---------------

// TestScenarioD_FilesStatsAggregate — the panel-level battle-test
// shape. Given params.files (list of {path, size} records, the
// shape produced by mounting/listing a localfiles prefix), return
// {count, total_bytes} as a primitive/any record.
//
// What's new versus Scenarios A/B/C:
//   - A.1/A.2/A.3/B/C all pass *bare uint64s* in their collections;
//     the fold step does `acc + elem` where `elem` IS the number.
//   - This scenario passes *records* (per-element {path, size, ...}),
//     and the fold step does `acc + Field(elem, "size")` — navigating
//     INTO each collection element. This is the v3.19c N.5 nav-composes
//     guarantee exercised end-to-end: Field accepts a record/map value
//     (not just entity.Entity) when the value is a lambda-bound scope
//     element. Pre-v3.19, this would have been an F9 blocker.
//
// Real-use motivation: any workbench aggregate over typed entities
// (FileData, doc/markdown-file, etc.) needs this shape — collection
// of records in, structured summary out. If this works, the toolkit
// covers real workbench aggregate use cases; if it doesn't, there's
// an N.5 follow-on to file.
func TestScenarioD_FilesStatsAggregate(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/scenarios/files-stats"
	c := ap.Compute()
	files := c.Field(c.LookupScope("params"), "files")

	count := c.Length(files)
	totalBytes := entitysdk.LowerFold(c, files, c.Literal(uint64(0)),
		func(acc, elem *entitysdk.Builder) *entitysdk.Builder {
			return c.Arithmetic("add", acc, c.Field(elem, "size"))
		})

	expr := entitysdk.LowerRecord(c, "primitive/any", map[string]interface{}{
		"count":       count,
		"total_bytes": totalBytes,
	})

	h, err := ap.RegisterComputeHandler(context.Background(), entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    "files-stats",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, expr)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	params, _ := entitysdk.PrimitiveAny(map[string]interface{}{
		"files": []interface{}{
			map[string]interface{}{"path": "a.md", "size": uint64(100)},
			map[string]interface{}{"path": "b.md", "size": uint64(250)},
			map[string]interface{}{"path": "c.md", "size": uint64(750)},
		},
	})
	resp, err := ap.Executor().ExecuteWithParams(pattern, "compute", params)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d (type=%s, data=%s)", resp.Status, resp.Type, string(respData(resp)))
	}

	rec, err := entitysdk.UnwrapComputeResultAsMap(resp)
	if err != nil {
		t.Fatalf("UnwrapComputeResultAsMap: %v", err)
	}
	if !numEq(rec["count"], 3) {
		t.Fatalf("count: expected 3, got %v (%T)", rec["count"], rec["count"])
	}
	if !numEq(rec["total_bytes"], 1100) {
		t.Fatalf("total_bytes: expected 1100, got %v (%T)", rec["total_bytes"], rec["total_bytes"])
	}
	t.Logf("PASS Scenario D (v3.19c N.5): files-stats aggregate over records = {count:3, total_bytes:1100}")
	t.Logf("OBSERVATION: Field(elem, \"size\") inside LowerFold's step navigates into per-element records cleanly. This is the canonical 'aggregate over typed entities' shape; it composes from the same three primitives (LowerFold, LowerRecord, Field).")
}

// --- Scenario E: filter+map over records (Field inside lambda body) ---

// TestScenarioE_FilterAndMapOverRecords closes the toolkit coverage gap
// that Scenario D opened for LowerFold. Existing end-to-end tests for
// LowerFilter (TestLowerFilter_LiteralPredicate) and LowerMap
// (TestLowerMap_DoubleEachElement) use *bare-element* collections —
// the lambda receives a uint64 and returns one. Scenario D showed
// LowerFold works on record collections with Field-per-elem. This
// test confirms the same composability for Filter and Map: the
// predicate / transform navigates into record fields via Field(elem, ...).
//
// Real-use motivation: "find large files" is filter-by-field; "extract
// paths of matching files" is filter then map. Both compose by
// navigating into the record element inside the lambda body. Together
// they cover the canonical "do something useful per-element" pattern
// that any workbench feature aggregating over typed entities will hit.
func TestScenarioE_FilterAndMapOverRecords(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/scenarios/big-files"
	c := ap.Compute()
	files := c.Field(c.LookupScope("params"), "files")

	// Filter: keep records where elem.size > 200.
	bigFiles := entitysdk.LowerFilter(c, files, func(elem *entitysdk.Builder) *entitysdk.Builder {
		return c.Compare("gt", c.Field(elem, "size"), c.Literal(uint64(200)))
	})

	// Map: extract elem.path from each surviving record.
	bigFilePaths := entitysdk.LowerMap(c, bigFiles, func(elem *entitysdk.Builder) *entitysdk.Builder {
		return c.Field(elem, "path")
	})

	h, err := ap.RegisterComputeHandler(context.Background(), entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    "big-files",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, bigFilePaths)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	params, _ := entitysdk.PrimitiveAny(map[string]interface{}{
		"files": []interface{}{
			map[string]interface{}{"path": "a.md", "size": uint64(100)}, // skip
			map[string]interface{}{"path": "b.md", "size": uint64(250)}, // keep
			map[string]interface{}{"path": "c.md", "size": uint64(150)}, // skip
			map[string]interface{}{"path": "d.md", "size": uint64(750)}, // keep
			map[string]interface{}{"path": "e.md", "size": uint64(200)}, // skip (gt, not ge)
			map[string]interface{}{"path": "f.md", "size": uint64(201)}, // keep
		},
	})
	resp, err := ap.Executor().ExecuteWithParams(pattern, "compute", params)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d (type=%s, data=%s)", resp.Status, resp.Type, string(respData(resp)))
	}

	got, err := entitysdk.UnwrapComputeResultAsList(resp)
	if err != nil {
		t.Fatalf("UnwrapComputeResultAsList: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 big-files, got %d: %v", len(got), got)
	}
	wantPaths := map[string]bool{"b.md": true, "d.md": true, "f.md": true}
	for i, p := range got {
		s, ok := p.(string)
		if !ok {
			t.Errorf("got[%d] not a string: %v (%T)", i, p, p)
			continue
		}
		if !wantPaths[s] {
			t.Errorf("unexpected path in result: %q", s)
		}
		delete(wantPaths, s)
	}
	if len(wantPaths) != 0 {
		t.Errorf("missing expected paths: %v", wantPaths)
	}
	t.Logf("PASS Scenario E: filter-by-size>200 + map-to-path over records returned %v", got)
	t.Logf("OBSERVATION: LowerFilter predicate and LowerMap transform both navigate into per-element records via Field(elem, ...) cleanly. Combined with Scenario D's LowerFold coverage, all three single/two-param collection-iteration toolkit ops are confirmed against the canonical 'aggregate over typed entities' shape.")
}

// --- Scenario F: empty-collection boundary -----------------------------

// TestScenarioF_EmptyCollectionBoundaries — confirms empty-collection
// behavior across Fold / Filter / Map all combine cleanly. Filter
// + Map on an empty input should each return [], and Fold should
// return the initial. Lifted from the unit-level edge-case checks
// (TestLowerFold_EmptyCollection already covers fold) to confirm
// the multi-stage composition `LowerMap(LowerFilter(empty))` also
// degrades to [] without errors.
func TestScenarioF_EmptyCollectionBoundaries(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/scenarios/empty-pipeline"
	c := ap.Compute()
	files := c.Field(c.LookupScope("params"), "files")
	bigFiles := entitysdk.LowerFilter(c, files, func(elem *entitysdk.Builder) *entitysdk.Builder {
		return c.Compare("gt", c.Field(elem, "size"), c.Literal(uint64(200)))
	})
	bigFilePaths := entitysdk.LowerMap(c, bigFiles, func(elem *entitysdk.Builder) *entitysdk.Builder {
		return c.Field(elem, "path")
	})
	h, err := ap.RegisterComputeHandler(context.Background(), entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    "empty-pipeline",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, bigFilePaths)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	params, _ := entitysdk.PrimitiveAny(map[string]interface{}{
		"files": []interface{}{},
	})
	resp, err := ap.Executor().ExecuteWithParams(pattern, "compute", params)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d (type=%s)", resp.Status, resp.Type)
	}
	got, err := entitysdk.UnwrapComputeResultAsList(resp)
	if err != nil {
		t.Fatalf("UnwrapComputeResultAsList: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %v", got)
	}
	t.Logf("PASS Scenario F: LowerMap(LowerFilter(empty)) → [] without errors")
}
