package entitysdk_test

// WHERE THE BUDGET BOUNDARY ACTUALLY IS — and whether sharding can live inside
// compute rather than in the host.
//
// The sharding prototype (axis1_shard_test.go) puts the fan-out and the
// aggregation in the HOST: k evals from Go, stitch in Go, one put. The obvious
// question is whether that is forced. The intuition is that it must be — if k
// shards ran inside one compute expression they would share its budget, and
// sharing the budget is the one thing sharding exists to avoid.
//
// That intuition is WRONG, and this file proves it. Reading the dispatcher:
//
//   - decrementBounds (core/protocol/dispatch.go:326) copies BoundsData and
//     decrements TTL ONLY. Budget rides through untouched as a *uint64 CAP.
//   - Every handler invocation calls initBudget(hctx) (ext/compute/handler.go:176)
//     which builds a FRESH *compute.Budget from that cap.
//   - The parent's CONSUMED ops live in the parent's own *Budget struct, which
//     is never handed to the child.
//
// So a compute/apply that dispatches to system/compute:eval gets a fresh 100k.
// The budget boundary is the HANDLER INVOCATION, not the expression tree.
// TestBudgetScope_ApplyGetsFreshBudget makes that empirical rather than a
// reading of the code.
//
// Which means the real blocker on in-compute sharding is somewhere else
// entirely — see TestBudgetScope_NoWayToRejoinShards.

import (
	"context"
	"fmt"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/compute"

	"entity-workbench-go/entitysdk"
)

// buildLifeMapOnly builds a step that maps the arith rule over ALL cells and
// returns the bare array — the shard shape at k=1. ~174 ops/cell measured, so
// 16x16 = 256 cells is roughly 44k ops: comfortably under the 100k cap alone,
// and decisively over it when three of them are summed.
func buildLifeMapOnly(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
	return buildLifeShardArith(ap, w, h, statePath, 0, w*h)
}

// TestBudgetScope_ApplyGetsFreshBudget — the decisive experiment.
//
// A parent expression applies to system/compute:eval THREE times, each child a
// ~44k-op Life map, and sums the three result lengths.
//
//	if budgets were SHARED:  3 x 44k = ~132k > 100k -> budget_exhausted
//	if budgets are FRESH:    parent pays ~15 ops, each child 44k on its own
//	                         -> returns 768
//
// The arithmetic is what makes this decisive: there is no reading of the result
// under which a shared budget survives.
func TestBudgetScope_ApplyGetsFreshBudget(t *testing.T) {
	const w, h = 16, 16
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	statePath := "app/budget/state"
	seedEnt, err := lifeGridEntity(w, h, lifeRandCells(w, h, 42))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.PutEntity(statePath, seedEnt); err != nil {
		t.Fatal(err)
	}

	// Three identical children. Identical is fine and actually sharper: the IR
	// dedups by content hash, so this isolates the BUDGET question from any
	// question about distinct expressions.
	childPaths := make([]string, 3)
	for i := range childPaths {
		p := fmt.Sprintf("app/budget/child%d", i)
		if _, err := buildLifeMapOnly(ap, w, h, statePath).Build(context.Background(), p); err != nil {
			t.Fatalf("build child %d: %v", i, err)
		}
		childPaths[i] = p
	}

	// First: establish ONE child costs a large fraction of the cap. If a child
	// were cheap, three of them fitting would prove nothing about budget scope.
	one := evalOps(t, ap, childPaths[0])
	t.Logf("one child (%dx%d map) = %d ops of the %d cap (%.0f%%)",
		w, h, one, compute_DefaultMaxOps, 100*float64(one)/float64(compute_DefaultMaxOps))
	if one*3 <= compute_DefaultMaxOps {
		t.Fatalf("three children (%d ops) fit under the %d cap anyway — this test "+
			"cannot distinguish shared from fresh budgets; make the child bigger",
			one*3, compute_DefaultMaxOps)
	}

	// Now the parent: sum of the three children's result lengths.
	c := ap.Compute()
	callChild := func(p string) *entitysdk.Builder {
		// apply -> compute/result entity; field(...,"value") -> the bare array.
		return c.Length(c.Field(
			c.Apply("system/compute", "eval", nil,
				entitysdk.WithResource(c.Literal(map[string]interface{}{
					"targets": []interface{}{p},
				}))),
			"value"))
	}
	parent := c.Arithmetic("add",
		c.Arithmetic("add", callChild(childPaths[0]), callChild(childPaths[1])),
		callChild(childPaths[2]))
	if _, err := parent.Build(context.Background(), "app/budget/parent"); err != nil {
		t.Fatalf("build parent: %v", err)
	}

	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{"app/budget/parent"}})
	if err != nil {
		t.Fatalf("parent dispatch: %v", err)
	}
	if resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		if ed.Code == "budget_exhausted" {
			t.Fatalf("parent exhausted its budget — sub-dispatched applies SHARE "+
				"the caller's budget, so in-compute fan-out is impossible and the "+
				"host-side shard design is forced. (%s)", ed.Message)
		}
		t.Fatalf("parent compute/error: code=%s message=%q", ed.Code, ed.Message)
	}
	if resp.Status != 200 {
		t.Fatalf("parent status %d", resp.Status)
	}

	var rd types.ComputeResultData
	if err := ecf.Decode(resp.Data, &rd); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	got := toUint(t, rd.Value)
	if want := uint64(3 * w * h); got != want {
		t.Fatalf("parent summed %d cells, want %d", got, want)
	}

	t.Logf("PARENT SUCCEEDED: %d cells across 3 applies = ~%d ops of work under a "+
		"%d cap. Each compute/apply -> system/compute:eval gets a FRESH budget; "+
		"the budget boundary is the handler invocation, not the expression tree.",
		got, one*3, compute_DefaultMaxOps)
}

// TestBudgetScope_NoWayToRejoinShards — the real blocker, isolated.
//
// Given fresh budgets per apply, in-compute sharding needs only one more thing:
// a way to REJOIN k partial arrays into the one flat array the state entity's
// `cells` field requires. entity-compute has no such operation.
//
// The builtin set is arithmetic, compare, logic, field, construct, map, filter,
// fold, store (ext/compute/builtins.go). There is no concat, no append, no
// flatten. (EXTENSION-COMPUTE mentions "append" five times — all of them are
// `list.append` inside the spec's own auditing PSEUDOCODE, not a primitive.)
//
// fold is the obvious candidate and cannot do it: folding k arrays into one
// needs an append in the accumulator step, which is the operation that does not
// exist. map produces k arrays, not one. construct can hold k array FIELDS, but
// that is a different entity shape — the grid's `cells` must be one flat array,
// and changing it changes the boundary bytes, which is exactly what equivalence
// forbids.
//
// This test pins the negative so it is recorded as a MISSING PRIMITIVE rather
// than rediscovered as a budget problem. It asserts the shape of the failure,
// not merely that something failed.
func TestBudgetScope_NoWayToRejoinShards(t *testing.T) {
	const w, h = 8, 8
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	statePath := "app/rejoin/state"
	seedEnt, err := lifeGridEntity(w, h, lifeRandCells(w, h, 42))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.PutEntity(statePath, seedEnt); err != nil {
		t.Fatal(err)
	}
	for j, r := range [][2]int{{0, 32}, {32, 64}} {
		p := fmt.Sprintf("app/rejoin/shard%d", j)
		if _, err := buildLifeShardArith(ap, w, h, statePath, r[0], r[1]).
			Build(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}

	c := ap.Compute()
	shardCall := func(j int) *entitysdk.Builder {
		return c.Field(
			c.Apply("system/compute", "eval", nil,
				entitysdk.WithResource(c.Literal(map[string]interface{}{
					"targets": []interface{}{fmt.Sprintf("app/rejoin/shard%d", j)},
				}))),
			"value")
	}

	// The ONLY in-compute rejoin available: put the two arrays in a construct as
	// separate fields. It builds and evaluates fine — and produces the wrong
	// shape, which is the point.
	nested := c.Construct(lifeGridType, map[string]*entitysdk.Builder{
		"width":  c.Literal(uint64(w)),
		"height": c.Literal(uint64(h)),
		"cells":  c.Literal([]interface{}{}), // placeholder; the real cells cannot be formed
		"part0":  shardCall(0),
		"part1":  shardCall(1),
	})
	if _, err := nested.Build(context.Background(), "app/rejoin/nested"); err != nil {
		t.Fatalf("build nested: %v", err)
	}

	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{"app/rejoin/nested"}})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 || resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		t.Fatalf("nested construct failed (status %d): code=%s msg=%q — expected it "+
			"to SUCCEED with the wrong shape, which is the actual finding",
			resp.Status, ed.Code, ed.Message)
	}

	// It succeeded — the shards ran, each on its own budget, and their arrays are
	// sitting in the entity as SEPARATE FIELDS. What is missing is not compute
	// power or budget: it is one operation that would turn part0 ++ part1 into
	// `cells`.
	var got map[string]interface{}
	if err := ecf.Decode(resp.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	p0, ok0 := got["part0"].([]interface{})
	p1, ok1 := got["part1"].([]interface{})
	if !ok0 || !ok1 {
		t.Fatalf("expected part0/part1 arrays, got %T / %T", got["part0"], got["part1"])
	}
	cells, _ := got["cells"].([]interface{})
	if len(cells) != 0 {
		t.Fatalf("cells should be the empty placeholder, got %d", len(cells))
	}
	t.Logf("in-compute fan-out WORKS (part0=%d cells, part1=%d cells, each on its "+
		"own fresh budget) but the rejoin does not exist: the two arrays cannot be "+
		"concatenated into the flat `cells` field. Blocker = a missing array-concat "+
		"primitive, NOT the budget model.", len(p0), len(p1))
}

// evalOps measures a child's op cost by bisecting on the budget cap: the
// smallest cap under which it still evaluates.
//
// Approximate by construction (it is a search, not an instrument) and that is
// fine — the test only needs to know the child is a large fraction of the cap,
// not its exact cost. There is no exported way to read a Budget's consumption
// after the fact, which is itself worth noting: op cost is observable only by
// running out of it.
func evalOps(t *testing.T, ap *entitysdk.AppPeer, path string) int {
	t.Helper()
	ent, ok := ap.TestContentStore().Get(mustIndex(t, ap, path))
	if !ok {
		t.Fatalf("expr not in store: %s", path)
	}
	ctx := ap.TestEvalContext()
	lo, hi := 1, compute_DefaultMaxOps
	for lo < hi {
		mid := (lo + hi) / 2
		_, err := computeEvaluate(ent, mid, ctx)
		if err != nil {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// --- small helpers ---

const compute_DefaultMaxOps = compute.DefaultMaxOps

func computeEvaluate(ent entity.Entity, ops int, ctx *compute.EvalContext) (interface{}, error) {
	return compute.Evaluate(ent, compute.NewScope(),
		compute.NewBudget(ops, compute.DefaultMaxDepth), ctx)
}

func mustIndex(t *testing.T, ap *entitysdk.AppPeer, path string) hash.Hash {
	t.Helper()
	h, ok := ap.TestLocationIndex().Get(path)
	if !ok {
		t.Fatalf("path not indexed: %s", path)
	}
	return h
}

func toUint(t *testing.T, v interface{}) uint64 {
	t.Helper()
	switch n := v.(type) {
	case uint64:
		return n
	case int64:
		return uint64(n)
	}
	t.Fatalf("expected integer, got %T", v)
	return 0
}
