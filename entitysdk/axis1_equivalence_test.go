package entitysdk_test

// AXIS-1 EQUIVALENCE HARNESS — the admission gate for an alternate execution
// engine (exploration §13.8, handoff §5).
//
// This is a first-class deliverable, not a test afterthought: it is the
// reusable, empirical definition of "an alternate engine is conformant," and
// it is what lets every future rung of the compile ladder be trusted.
//
//	An alternate engine is conformant iff, for the same IR, it produces
//	materialized-boundary-equivalent results to the Stage-1 reference.
//
// Stage-1 (entity-core-go ext/compute) is the reference BY CONSTRUCTION: any
// divergence is an Axis-1 bug, never a Stage-1 bug. That asymmetry is what
// makes the oracle usable — there is nothing to adjudicate.
//
// WHY PROPERTY-BASED RATHER THAN VECTOR-BASED
//
// Handoff §5 asks for "the existing compute conformance vectors" run through
// both engines. Those do not exist — no portable compute corpus exists anywhere
// in the cohort, and compute's only tests are per-impl and in-package (see
// docs/architecture/reviews/COMPUTE-AXIS1-ORACLE-GAPS-2026-07-16.md §1). So the
// substitution is a generator.
//
// It is stronger than a fixed corpus for THIS job. Axis-1 re-derives ~1,200
// lines of Stage-1 semantics (arithmetic, comparison, casts, canonical
// ordering) because none of it is exported, and reimplementation drift is the
// risk. Life and Snake exercise none of those corners — no NumericCast, no
// wraparound, no float, no unsigned — so the probe suites are blind exactly
// where the risk is. A generator reaches them, cannot go vacuous the way a
// hand-authored example can, and every failure is a minimizable reproducer.
//
// It is NOT a substitute for the cohort-level thing a corpus does: pinning
// go/rust/py agreement. That gap is arch's and is routed.
//
// WHAT "EQUIVALENT" MEANS HERE (§13.5 — the boundary contract)
//
// Only the MATERIALIZED BOUNDARY is compared. The interior is deliberately
// free: Axis-1 holds closures as pointers and environments as live frames,
// where Stage-1 builds content-addressed compute/scope entities. So a Stage-1
// run leaves entities in the content store that an Axis-1 run does not, and
// comparing store CONTENTS would fail for exactly the reason the rung exists.
// Comparing boundary hashes is the contract.
//
// Both sides are reduced through compute.CaptureScope — Stage-1's OWN
// materialization path (buildScopeBinding materializes a constructed value,
// stores it, and yields its content hash). Using the reference's code for the
// final reduction means the boundary hash is authoritative rather than
// something this file re-derives and could get wrong in the same direction as
// the engine under test.

import (
	"context"
	hexpkg "encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/compute"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/entitysdk/axis1"
)

// --- The boundary reduction ---

// boundary reduces an engine result to a comparable string describing the
// materialized boundary form.
//
// Routing through compute.CaptureScope is deliberate (see the file header): it
// is Stage-1's own materialization, reached with exported API, so the answer for
// an in-flight constructed value is the reference's answer rather than this
// file's opinion of it.
//
// Value-kind bindings are compared by their CANONICAL CBOR BYTES, not by Go
// formatting. That is the only comparison that catches Rule 10: int64(5) and
// uint64(5) format identically but encode as different CBOR major types, and
// telling them apart is the whole point of the signed/unsigned model.
func boundary(v interface{}, cs store.ContentStore) (string, error) {
	s := compute.NewScope()
	s.Set("r", v)
	ent, err := compute.CaptureScope(s, cs)
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	var d types.ComputeScopeData
	if err := ecf.Decode(ent.Data, &d); err != nil {
		return "", fmt.Errorf("decode scope: %w", err)
	}
	b, ok := d.Bindings["r"]
	if !ok {
		return "", fmt.Errorf("binding r missing")
	}
	switch b.Kind {
	case types.ScopeBindingKindEntity:
		return "entity:" + b.EntityHash.String(), nil
	case types.ScopeBindingKindValue:
		raw, err := ecf.Encode(b.Value)
		if err != nil {
			return "", fmt.Errorf("encode value: %w", err)
		}
		return fmt.Sprintf("value:%T:%s", b.Value, hexpkg.EncodeToString(raw)), nil
	default:
		return "", fmt.Errorf("unknown binding kind %q", b.Kind)
	}
}

// axisOutcome is the full comparable result of one eval on one engine: either an
// error (code + message) or a materialized boundary form.
//
// Errors are compared as rigorously as values. Error-as-value propagation is
// part of the contract (§13.5) — a compute/error poisons its consumers and can
// end up materialized in the tree, so an engine that raises the right code with
// the wrong message, or the right error for the wrong reason, is not conformant.
type axisOutcome struct {
	err  string
	form string
}

func (o axisOutcome) String() string {
	if o.err != "" {
		return "error(" + o.err + ")"
	}
	return o.form
}

func errString(err error) string {
	if ce, ok := err.(*compute.ComputeError); ok {
		return ce.Code + ": " + ce.Message
	}
	return "non-compute-error: " + err.Error()
}

// runStage1 evaluates on the reference engine.
func runStage1(ent entity.Entity, root map[string]interface{}, ctx *compute.EvalContext, ops int) axisOutcome {
	s := compute.NewScope()
	for k, v := range root {
		s.Set(k, v)
	}
	v, err := compute.Evaluate(ent, s, compute.NewBudget(ops, compute.DefaultMaxDepth), ctx)
	if err != nil {
		return axisOutcome{err: errString(err)}
	}
	form, ferr := boundary(v, ctx.ContentStore)
	if ferr != nil {
		return axisOutcome{err: "boundary: " + ferr.Error()}
	}
	return axisOutcome{form: form}
}

// runAxis1 evaluates on the engine under test.
//
// The Axis-1 result is materialized before the shared reduction because its
// in-flight constructed value is a distinct (necessarily — Stage-1's is
// unexported) type that CaptureScope cannot recognize. Materialize first, then
// both sides reduce through identical reference code.
func runAxis1(eng *axis1.Engine, ent entity.Entity, root map[string]interface{}, ctx *compute.EvalContext, ops int) axisOutcome {
	v, err := eng.Evaluate(ent, root, compute.NewBudget(ops, compute.DefaultMaxDepth), ctx)
	if err != nil {
		return axisOutcome{err: errString(err)}
	}
	mv, err := axis1.Materialize(v, ctx.ContentStore)
	if err != nil {
		return axisOutcome{err: "materialize: " + err.Error()}
	}
	form, ferr := boundary(mv, ctx.ContentStore)
	if ferr != nil {
		return axisOutcome{err: "boundary: " + ferr.Error()}
	}
	return axisOutcome{form: form}
}

// --- The generator ---

// exprGen builds random well-formed expression graphs over a small typed
// grammar. Typed generation (int / bool / array) keeps most programs meaningful
// rather than degenerating into a type_mismatch corpus — but the grammar
// deliberately still emits div/mod (division_by_zero, and F-D1's float-poisoning
// non-exact quotient), index (index_out_of_range), and casts (cast_out_of_range),
// so error paths are covered on purpose rather than by accident.
type exprGen struct {
	c   *entitysdk.ComputeBuilder
	rnd *rand.Rand
}

func (g *exprGen) intExpr(depth int) *entitysdk.Builder {
	return g.intExprOpt(depth, true)
}

// intExprOpt generates an integer-valued expression. allowCast=false forbids a
// bare numeric-cast at the ROOT of the generated expression.
//
// That is not a generator convenience — it is the S1 builder enforcing Rule 11
// at build time: `compute.Let` rejects a NumericCast binding value outright
// ("the cast effect does not flow through let; inline the cast at the use site
// instead"). The toolkit already encodes the F-E-series lesson as a guard, so
// the generator has to respect it or it cannot build at all. Casts still appear
// freely under arithmetic/compare operands, which is the position where Rule 11
// says the hint is actually consumed — i.e. exactly the position the decoder
// hoists (decode.go::castIntent), which is what needs testing.
func (g *exprGen) intExprOpt(depth int, allowCast bool) *entitysdk.Builder {
	c := g.c
	if depth <= 0 {
		switch g.rnd.Intn(4) {
		case 0:
			return c.Literal(int64(g.rnd.Intn(21) - 10))
		case 1:
			return c.Literal(uint64(g.rnd.Intn(10)))
		case 2:
			return c.LookupScope("n")
		default:
			return c.LookupScope("m")
		}
	}
	n := 8
	if !allowCast {
		n = 7
	}
	switch g.rnd.Intn(n) {
	case 0, 1, 2:
		ops := []string{"add", "sub", "mul", "div", "mod"}
		return c.Arithmetic(ops[g.rnd.Intn(len(ops))], g.intExpr(depth-1), g.intExpr(depth-1))
	case 3:
		return c.If(g.boolExpr(depth-1), g.intExpr(depth-1), g.intExpr(depth-1))
	case 4:
		// Names chosen to sort in dependency order — the S1 builder normalizes
		// let bindings to sorted name order, so "a" is visible to "b" but not
		// vice versa (the F-E-series footgun, exercised here on purpose).
		return c.Let(map[string]*entitysdk.Builder{
			"a": g.intExprOpt(depth-1, false),
			"b": c.Arithmetic("add", c.LookupScope("a"), g.intExpr(depth-1)),
		}, c.Arithmetic("mul", c.LookupScope("a"), c.LookupScope("b")))
	case 5:
		return c.Index(g.arrExpr(depth-1), g.intExpr(depth-1))
	case 6:
		return c.Length(g.arrExpr(depth - 1))
	default:
		// Rule 11: an eager cast at the operand position flips div/mod/compare
		// unsigned. The cast is generated directly under the op so the hint is
		// actually consumed — that is the semantic the decoder hoists.
		t := []string{"primitive/int", "primitive/uint", "primitive/float"}[g.rnd.Intn(3)]
		return c.NumericCast(g.intExpr(depth-1), t)
	}
}

func (g *exprGen) boolExpr(depth int) *entitysdk.Builder {
	c := g.c
	if depth <= 0 {
		return c.Compare([]string{"eq", "neq", "lt", "gt", "lte", "gte"}[g.rnd.Intn(6)],
			g.intExpr(0), g.intExpr(0))
	}
	switch g.rnd.Intn(3) {
	case 0:
		return c.Compare([]string{"eq", "neq", "lt", "gt", "lte", "gte"}[g.rnd.Intn(6)],
			g.intExpr(depth-1), g.intExpr(depth-1))
	case 1:
		return c.Logic([]string{"and", "or"}[g.rnd.Intn(2)], g.boolExpr(depth-1), g.boolExpr(depth-1))
	default:
		return c.Logic("not", g.boolExpr(depth-1), nil)
	}
}

func (g *exprGen) arrExpr(depth int) *entitysdk.Builder {
	c := g.c
	if depth <= 0 {
		n := g.rnd.Intn(4)
		vals := make([]interface{}, n)
		for i := range vals {
			vals[i] = int64(g.rnd.Intn(10))
		}
		return c.Literal(vals)
	}
	switch g.rnd.Intn(3) {
	case 0:
		// map with a closure capturing an enclosing binding — the F-D2 shape,
		// and the case where Axis-1's live frames diverge most from Stage-1's
		// CaptureScope/LoadScope round-trip.
		return c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": g.arrExpr(depth - 1),
			"fn": c.Lambda([]string{"e"},
				c.Arithmetic("add", c.LookupScope("e"), g.intExpr(depth-1))),
		})
	case 1:
		return c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
			"collection": g.arrExpr(depth - 1),
			"fn":         c.Lambda([]string{"e"}, c.Compare("gt", c.LookupScope("e"), g.intExpr(0))),
		})
	default:
		return c.LookupScope("arr")
	}
}

// rootBindings are the free variables every generated expression may reference.
// Both engines receive the same values; Stage-1 as a *compute.Scope built from
// this map, Axis-1 as the map itself.
func rootBindings() map[string]interface{} {
	return map[string]interface{}{
		"n":   int64(7),
		"m":   int64(-3),
		"arr": []interface{}{int64(1), int64(2), int64(3), int64(4)},
	}
}

// TestAxis1Equivalence_Differential is the sweep: N random graphs, both
// engines, identical outcomes required.
//
// A failure prints the seed and the case index. Both are deterministic, so
// `-run ...  -args` re-running with the same seed reproduces the exact graph —
// which is the property a fixed corpus cannot give you for free.
func TestAxis1Equivalence_Differential(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	ctx := ap.TestEvalContext()
	eng := axis1.NewEngine()
	root := rootBindings()

	const seed = 20260716
	const cases = 300
	rnd := rand.New(rand.NewSource(seed))
	g := &exprGen{c: ap.Compute(), rnd: rnd}

	var errs, vals int
	codes := map[string]int{}
	for i := 0; i < cases; i++ {
		// Wrap every case in a construct: it forces the result across the
		// materialized boundary (the only thing the contract binds), and it is
		// the shape every real program's step ends in.
		expr := g.c.Construct("app/axis1/probe", map[string]*entitysdk.Builder{
			"value": g.intExpr(3),
			"tag":   g.c.Literal(int64(i)),
		})
		h, err := expr.Build(context.Background(), fmt.Sprintf("app/axis1/diff/%d", i))
		if err != nil {
			t.Fatalf("case %d: build: %v", i, err)
		}
		ent, ok := ap.TestContentStore().Get(h)
		if !ok {
			t.Fatalf("case %d: root entity %s not in store", i, h)
		}

		s1 := runStage1(ent, root, ctx, compute.DefaultMaxOps)
		a1 := runAxis1(eng, ent, root, ctx, compute.DefaultMaxOps)

		if s1.String() != a1.String() {
			// t.Errorf, not Fatalf: AP15. A fail-fast sweep reports a LOWER BOUND
			// on the divergence count, and this file is where that was measured —
			// it reported ONE divergence for three days when there were three,
			// because cases 28 and 79 were never reached.
			t.Errorf("case %d DIVERGED (seed %d)\n  stage1: %s\n  axis1:  %s\n  ir:     %s",
				i, seed, s1, a1, h)
			continue
		}
		if s1.err != "" {
			errs++
			if idx := strings.Index(s1.err, ":"); idx > 0 {
				codes[s1.err[:idx]]++
			}
		} else {
			vals++
		}
	}

	// ── THE SWEEP IS FULLY GREEN. It was not, and the history is the useful part.
	//
	// Cases 9, 28 and 79 of 300 diverged: Stage-1 produced a value, Axis-1 raised
	// index_out_of_range. All three had the identical shape —
	//
	//	length( map(arr, λe. e + index(<short literal array>, <out-of-range idx>)) )
	//
	// — and the cause is now measured, not guessed: map's output element is a
	// CONTAINED position (§1.5 NaN model / v3.26 §3.5). The reference contains
	// each element's error AS A VALUE and length() answers 4. Axis-1 propagated
	// it, so the whole program answered index_out_of_range. Fixed by
	// axis1/contain.go + the CONSUMED/CONTAINED split at each call site.
	//
	// Two process notes worth more than the bug:
	//
	//  1. The count was wrong before it was diagnosed. This sweep used to
	//     `t.Fatalf` on the first divergence, so the tree reported ONE failing
	//     case for three days when there were three — cases 28 and 79 had never
	//     once been evaluated. AP15, one level down from where we had already
	//     fixed it. The `t.Errorf` above is the fix; do not turn it back.
	//
	//  2. A wrong probe is worse than no probe. We DID hold the right hypothesis
	//     ("contained-error semantics") and then retracted it on a measurement
	//     that could not test it: indexing a 2-element array at 4 both bare and
	//     under the sweep's Construct, which agreed byte-for-byte on both engines
	//     — because a BARE index is a consumed position, where both engines were
	//     already correct. The divergence only exists at a closure-result
	//     position inside a collection primitive. A probe that does not reproduce
	//     the shape refutes nothing, and reporting it as a refutation cost more
	//     than the original error did.
	//
	// Everything below guards against the sweep AGREEING VACUOUSLY — passing
	// while proving nothing. That is not a hypothetical failure mode in this
	// track: it is F-D3 (D3 compared two dead grids and passed) and it is the
	// cohort's ECF F30 (tag_reject vectors rejecting for the wrong reason —
	// keystone's own note calls it "a vacuous pass"). Three instances in one
	// track; this oracle does not get to be the fourth.
	st := eng.Stats()

	if vals < cases/4 {
		t.Fatalf("sweep was mostly errors (%d values / %d errors of %d) — "+
			"the generator is producing garbage and the agreement is vacuous",
			vals, errs, cases)
	}
	if errs == 0 {
		t.Fatalf("sweep produced no errors at all of %d cases — error-as-value "+
			"propagation is part of the contract and went untested", cases)
	}
	// The live-frame closure path (§13.4) is the whole F-D2 fix and the most
	// likely place for Axis-1 to diverge. If no closure was ever built, the
	// sweep agreed about everything EXCEPT the thing this rung changes.
	if st.Closures == 0 {
		t.Fatalf("sweep built no closures — map/filter never ran, so the " +
			"live-frame path (the entire point of the rung) went untested")
	}
	// A fallback silently routes to Stage-1, which would make Axis-1 agree with
	// Stage-1 by BEING Stage-1. On this grammar every node is in the fast set,
	// so any fallback means the decoder stopped recognizing something.
	if st.Fallbacks != 0 {
		t.Fatalf("sweep hit %d fallbacks — those cases compared Stage-1 against "+
			"itself and prove nothing about Axis-1", st.Fallbacks)
	}
	// At least three distinct error codes: one code repeated 143 times would
	// mean the error paths are covered on paper only.
	if len(codes) < 3 {
		t.Fatalf("only %d distinct error codes (%v) — the error surface is "+
			"barely covered", len(codes), codes)
	}

	t.Logf("differential sweep: %d cases, %d values / %d errors, engines agree\n"+
		"  error codes: %v\n  axis1 stats: %+v", cases, vals, errs, codes, st)
}

// TestAxis1Equivalence_DecodeOnce asserts the premise of the whole rung: the
// step is decoded ONCE and reused across ticks.
//
// Without this, a passing equivalence sweep would prove correctness while the
// engine quietly re-decoded every eval — i.e. it would be conformant and
// pointless, and the cost numbers in the report would be measuring nothing.
func TestAxis1Equivalence_DecodeOnce(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	c := ap.Compute()
	expr := c.Construct("app/axis1/probe", map[string]*entitysdk.Builder{
		"value": c.Arithmetic("add", c.LookupScope("n"), c.Literal(int64(1))),
	})
	h, err := expr.Build(context.Background(), "app/axis1/decode-once")
	if err != nil {
		t.Fatal(err)
	}
	ent, _ := ap.TestContentStore().Get(h)

	ctx := ap.TestEvalContext()
	eng := axis1.NewEngine()
	root := rootBindings()

	for i := 0; i < 10; i++ {
		if _, err := eng.Evaluate(ent, root, compute.DefaultBudget(), ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}

	st := eng.Stats()
	if st.Evals != 10 {
		t.Fatalf("expected 10 evals, got %d", st.Evals)
	}
	// The graph has several nodes; each unique IR entity decodes once, and the
	// count must not grow with tick count.
	first := st.Decodes
	for i := 0; i < 10; i++ {
		if _, err := eng.Evaluate(ent, root, compute.DefaultBudget(), ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := eng.Stats().Decodes; got != first {
		t.Fatalf("decode count grew across ticks: %d → %d — the §13.7 cache "+
			"keyed by IR content hash is not working, so the rung is untested",
			first, got)
	}
	t.Logf("decode-once holds: %d decodes for 20 evals (%+v)", first, eng.Stats())
}

// TestAxis1Equivalence_ContainedErrorPositions is the REGRESSION GATE for what
// the seed-20260716 sweep found, written as vectors rather than left to a
// generator: the sweep reached this class by luck of the draw, and a grammar
// change could stop reaching it without anything else changing.
//
// Each vector names its POSITION, because the position is the whole rule.
// §1.5 makes a compute/error an ordinary value; what varies is whether a
// primitive READS the result (CONSUMED — short-circuit) or PLACES it
// (CONTAINED — it becomes a value in that slot). Getting the split wrong is
// invisible under any test whose closures cannot fail.
func TestAxis1Equivalence_ContainedErrorPositions(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	ctx := ap.TestEvalContext()
	root := rootBindings()
	c := ap.Compute()

	// oob is an index expression that always fails: [5,4][4].
	oob := func() *entitysdk.Builder {
		return c.Index(c.Literal([]interface{}{int64(5), int64(4)}), c.Literal(uint64(4)))
	}

	// failedMap is a map whose closure fails on EVERY element. Under the
	// contained rule its result is a well-formed 4-element array of errors — the
	// value that makes the read-back-out vectors below possible at all.
	failedMap := func() *entitysdk.Builder {
		return c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.LookupScope("arr"),
			"fn":         c.Lambda([]string{"e"}, c.Arithmetic("add", c.LookupScope("e"), oob())),
		})
	}
	// oobErr is what an out-of-range [5,4][4] raises, verbatim. Asserting the
	// exact string is deliberate: the harness compares error MESSAGES, so a
	// vector that lands on the right code for the wrong reason still fails.
	const oobErr = "error(index_out_of_range: index 4 out of range for array of length 2)"

	vectors := []struct {
		name string
		pos  string
		expr *entitysdk.Builder
		// want is the outcome BOTH engines must produce. Agreement alone is not
		// enough — two engines can agree on the wrong answer, and before the fix
		// several of these agreed on a propagated error or a type_mismatch. The
		// literal expectation is what makes each vector prove its position.
		want string
	}{
		{
			// The minimized shape of sweep cases 9, 28 and 79. Pre-fix: axis1
			// answered oobErr here while Stage-1 answered 4.
			name: "map-element-contains",
			pos:  "CONTAINED — map PLACES the closure result, so length sees 4 error VALUES",
			expr: c.Length(failedMap()),
			want: "value:uint64:04",
		},
		{
			name: "fold-accumulator-contains",
			pos:  "CONTAINED — a closure that ignores a failed accumulator RECOVERS",
			expr: c.BuiltinsCall("fold", map[string]*entitysdk.Builder{
				"collection": c.LookupScope("arr"),
				"initial":    c.Literal(int64(0)),
				"fn": c.Lambda([]string{"acc", "e"},
					// Every element fails against acc, but the LAST element (4)
					// takes the recovering branch and never reads acc — so the
					// fold returns 4, not an error. fold([],fn,E)=E's live cousin.
					c.If(c.Compare("gt", c.LookupScope("e"), c.Literal(int64(3))),
						c.LookupScope("e"),
						c.Arithmetic("add", c.LookupScope("acc"), oob()))),
			}),
			// uint64, not int64: the boundary reduction round-trips through
			// canonical CBOR, where a non-negative integer comes back unsigned.
			want: "value:uint64:04",
		},
		{
			// The MINTED arm of a consumed position. Axis-1 was already right
			// here — included so the pair is visible: the same failing predicate
			// short-circuits where map's element contains.
			name: "filter-predicate-consumes-minted",
			pos:  "CONSUMED — filter READS the predicate, so a failure short-circuits the filter",
			expr: c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
				"collection": c.LookupScope("arr"),
				"fn":         c.Lambda([]string{"e"}, c.Compare("gt", oob(), c.LookupScope("e"))),
			}),
			want: oobErr,
		},
		{
			// The VALUE-FORM arm of the same position, which axis1 did NOT have.
			// The predicate RETURNS a contained error rather than raising one
			// (index pulls element 0 straight out of failedMap). Pre-fix this
			// reached truthy(), whose default arm returns true, so every element
			// was KEPT on a failed predicate — a wrong ANSWER, not an error.
			name: "filter-predicate-consumes-value-form",
			pos:  "CONSUMED — a value-form error predicate short-circuits exactly as a minted one",
			expr: c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
				"collection": c.LookupScope("arr"),
				"fn":         c.Lambda([]string{"e"}, c.Index(failedMap(), c.Literal(uint64(0)))),
			}),
			want: oobErr,
		},
		{
			// A contained error read back OUT lands in an arithmetic operand — a
			// consumed position. Pre-fix, applyArithmetic received an
			// entity.Entity and reported type_mismatch: the right failure for
			// the wrong reason, which is the failure mode evaluator.operand exists
			// to prevent.
			name: "contained-error-read-back-out",
			pos:  "CONSUMED — an error taken back out of an array short-circuits AS that error",
			expr: c.Arithmetic("add", c.Literal(int64(1)),
				c.Index(failedMap(), c.Literal(uint64(0)))),
			want: oobErr,
		},
	}

	eng := axis1.NewEngine()
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			h, err := v.expr.Build(context.Background(), "app/axis1/contained/"+v.name)
			if err != nil {
				t.Fatal(err)
			}
			ent, ok := ap.TestContentStore().Get(h)
			if !ok {
				t.Fatalf("root entity %s not in store", h)
			}

			s1 := runStage1(ent, root, ctx, compute.DefaultMaxOps)
			a1 := runAxis1(eng, ent, root, ctx, compute.DefaultMaxOps)
			if s1.String() != a1.String() {
				t.Fatalf("%s\n  stage1: %s\n  axis1:  %s", v.pos, s1, a1)
			}
			if s1.String() != v.want {
				t.Fatalf("%s\n  both engines answered %s\n  want %s\n"+
					"(they agree, so this is the REFERENCE moving or the vector no longer "+
					"reaching its position — not an axis1 regression)", v.pos, s1, v.want)
			}
			t.Logf("%s → %s", v.pos, s1)
		})
	}

	// The BOUNDARY arm, asserted structurally rather than by hash: a contained
	// compute/error element materializes CODE-ONLY (v3.26 §3.5). Equality between
	// the engines would not catch this — both would have to be wrong the same
	// way, and they share the reduction — so this reads the bytes.
	//
	// Code-only is the load-bearing part: `message` is prose no spec pins, so a
	// contained message forks the enclosing array's bytes cross-impl. The tell is
	// that all four elements are the SAME hash despite four distinct element
	// values, which is only true if the diagnostics were dropped.
	t.Run("boundary-contained-error-is-code-only", func(t *testing.T) {
		h, err := failedMap().Build(context.Background(), "app/axis1/contained/boundary")
		if err != nil {
			t.Fatal(err)
		}
		ent, _ := ap.TestContentStore().Get(h)
		v, err := axis1.NewEngine().Evaluate(ent, root, compute.NewBudget(compute.DefaultMaxOps, compute.DefaultMaxDepth), ctx)
		if err != nil {
			t.Fatalf("map should have CONTAINED its element errors, not raised: %v", err)
		}
		mv, err := axis1.Materialize(v, ctx.ContentStore)
		if err != nil {
			t.Fatal(err)
		}
		arr, ok := mv.([]interface{})
		if !ok || len(arr) != 4 {
			t.Fatalf("want a 4-element materialized array, got %T %v", mv, mv)
		}
		for i, elt := range arr {
			eh, ok := elt.(hash.Hash)
			if !ok {
				t.Fatalf("element %d is %T, not a bare system/hash ref — a contained error "+
					"element must be referenced by hash like any entity-valued element", i, elt)
			}
			if eh != arr[0].(hash.Hash) {
				t.Fatalf("element %d hashes differently from element 0. Four elements failed "+
					"with the SAME code, so four identical hashes is the code-only property; "+
					"differing hashes mean message/at/expression leaked into the content", i)
			}
			ee, ok := ctx.ContentStore.Get(eh)
			if !ok {
				t.Fatalf("element %d: contained error entity %s was not stored", i, eh)
			}
			if ee.Type != types.TypeComputeError {
				t.Fatalf("element %d: want compute/error, got %s", i, ee.Type)
			}
			d, err := types.ComputeErrorDataFromEntity(ee)
			if err != nil {
				t.Fatal(err)
			}
			if d.Code != compute.ErrIndexOutOfRange {
				t.Fatalf("element %d: want code %q, got %q", i, compute.ErrIndexOutOfRange, d.Code)
			}
			if d.Message != "" || d.At != "" || d.Expression != nil {
				t.Fatalf("element %d materialized with diagnostics (message=%q at=%q expr=%v) — "+
					"that is ToEntity's in-flight form, not ToMaterializedEntity's code-only form, "+
					"and it forks the array's bytes across implementations",
					i, d.Message, d.At, d.Expression)
			}
		}
	})
}
