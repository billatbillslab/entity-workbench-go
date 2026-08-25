package entitysdk_test

// SHARDING — the budget-cliff prototype (POC §2 option 1).
//
// Life at 32x32/arith cannot tick AT ALL: one generation exceeds the 100k
// per-eval op cap and returns budget_exhausted. The Axis-1 rung does not help —
// TestAxis1BudgetCliff measured the cliff in the identical place on both
// engines, because op accounting is a pure function of the LOGICAL GRAPH WALK
// and Axis-1 walks the same graph. CaptureScope/LoadScope were costing time,
// not ops. So no execution-strategy rung moves the cliff, and sharding is not
// merely the preferred option (POC §2 listed it first of three) — it is the
// only one of the three that can work.
//
// The idea: split the step's map into k contiguous index ranges, eval each
// separately so EACH GETS A FRESH BUDGET, and let the host stitch the k partial
// cell arrays into one grid and write it once. k evals + 1 put per generation
// instead of 1 eval + 1 put.
//
// What this changes about the program, precisely: nothing in the per-cell rule.
// A shard is the SAME expression over a smaller index set — the lowering is
// untouched, only the collection literal differs. That is what makes sharding a
// runtime concern rather than a program rewrite, and it is why it composes with
// the descriptor rather than fighting it.
//
// What it costs, and the honest part: each shard independently does its own
// lookup/tree of the state and binds the whole cells array, because every cell
// needs neighbors that may live in another strip. So a k-shard tick reads the
// grid k times. Sharding trades tree reads for budget headroom; §4 measures the
// trade rather than assuming it is cheap.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/entitysdk/axis1"
)

// buildLifeShardArith builds a step that computes cells [i0,i1) only, returning
// a BARE ARRAY rather than a grid entity.
//
// The per-cell rule is byte-for-byte the arith lowering from
// buildLifeStepArith (exp_compute_life_test.go) — same neighbor arithmetic,
// same F-D1 floor-div workaround, same sorted-let dependency order. It is
// duplicated rather than factored out of the probe because
// exp_compute_life_test.go is the frozen record carrying the POC's oracle and
// replay proofs; a later experiment should not shift it.
//
// The difference from the unsharded step is exactly two things: the collection
// literal is the shard's index range, and there is no Construct — a shard
// produces a FRAGMENT, and only the host knows what the whole is. Fragments
// have no meaningful content hash, which is the first real consequence (§5).
func buildLifeShardArith(ap *entitysdk.AppPeer, w, h int, statePath string, i0, i1 int) *entitysdk.Builder {
	c := ap.Compute()
	W, H := uint64(w), uint64(h)

	neighbor := func(dx, dy int) *entitysdk.Builder {
		nx := c.Arithmetic("mod",
			c.Arithmetic("add", c.LookupScope("x"), c.Literal(uint64(w+dx))),
			c.Literal(W))
		ny := c.Arithmetic("mod",
			c.Arithmetic("add", c.LookupScope("y"), c.Literal(uint64(h+dy))),
			c.Literal(H))
		ni := c.Arithmetic("add", c.Arithmetic("mul", ny, c.Literal(W)), nx)
		return c.Index(c.LookupScope("cells"), ni)
	}
	terms := make([]*entitysdk.Builder, 0, 8)
	for _, d := range lifeOffsets {
		terms = append(terms, neighbor(d[0], d[1]))
	}

	perCell := c.Let(map[string]*entitysdk.Builder{
		"x": c.Arithmetic("mod", c.LookupScope("i"), c.Literal(W)),
		"y": c.Arithmetic("div",
			c.Arithmetic("sub", c.LookupScope("i"), c.LookupScope("x")),
			c.Literal(W)),
	}, c.Let(map[string]*entitysdk.Builder{
		"count": lifeAddTree(c, terms),
		"alive": c.Compare("eq",
			c.Index(c.LookupScope("cells"), c.LookupScope("i")),
			c.Literal(uint64(1))),
	}, lifeRule(c)))

	indices := make([]uint64, 0, i1-i0)
	for i := i0; i < i1; i++ {
		indices = append(indices, uint64(i))
	}

	// Each shard reads the FULL grid: a cell's neighbors may live in another
	// strip, so a shard cannot be given only its own rows. This is the k-times
	// read the header warns about.
	return c.Let(map[string]*entitysdk.Builder{
		"g": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"cells": c.Field(c.LookupScope("g"), "cells"),
	}, c.BuiltinsCall("map", map[string]*entitysdk.Builder{
		"collection": c.Literal(indices),
		"fn":         c.Lambda([]string{"i"}, perCell),
	})))
}

// shardRig is a sharded Life program: k shard steps over one state path.
type shardRig struct {
	ap        *entitysdk.AppPeer
	eng       *axis1.Engine
	w, h, k   int
	statePath string
	shards    []string
}

func newShardRig(t testing.TB, w, h, k int, seed []uint64) *shardRig {
	t.Helper()
	eng := axis1.NewEngine()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		Handlers: []entitysdk.HandlerRegistration{
			{Pattern: axis1Pattern, Handler: axis1.NewHandler(eng, axis1Pattern)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	root := fmt.Sprintf("app/life/shard-%dx%d-k%d", w, h, k)
	r := &shardRig{ap: ap, eng: eng, w: w, h: h, k: k, statePath: root + "/state"}

	seedEnt, err := lifeGridEntity(w, h, seed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.PutEntity(r.statePath, seedEnt); err != nil {
		t.Fatal(err)
	}

	// Contiguous index ranges. Row-aligned when k divides h, but the split is
	// by cell index and does not need to be — the rule is per-cell and index
	// arithmetic recovers (x,y), so shards have no geometric requirement.
	n := w * h
	for j := 0; j < k; j++ {
		i0 := j * n / k
		i1 := (j + 1) * n / k
		path := fmt.Sprintf("%s/shard%d", root, j)
		if _, err := buildLifeShardArith(ap, w, h, r.statePath, i0, i1).
			Build(context.Background(), path); err != nil {
			t.Fatalf("build shard %d: %v", j, err)
		}
		r.shards = append(r.shards, path)
	}
	return r
}

// tick runs one sharded generation: k evals, host-side stitch, one put.
func (r *shardRig) tick(pattern string) ([]uint64, hash.Hash, error) {
	cells := make([]uint64, 0, r.w*r.h)
	for j, path := range r.shards {
		part, err := r.evalShard(pattern, path)
		if err != nil {
			return nil, hash.Hash{}, fmt.Errorf("shard %d: %w", j, err)
		}
		cells = append(cells, part...)
	}
	if len(cells) != r.w*r.h {
		return nil, hash.Hash{}, fmt.Errorf("stitched %d cells, want %d", len(cells), r.w*r.h)
	}
	ent, err := lifeGridEntity(r.w, r.h, cells)
	if err != nil {
		return nil, hash.Hash{}, err
	}
	h, err := r.ap.PutEntity(r.statePath, ent)
	if err != nil {
		return nil, hash.Hash{}, err
	}
	return cells, h, nil
}

// evalShard evaluates one shard and decodes its partial cell array.
//
// A shard returns a bare array, not an entity, so the handler wraps it in a
// compute/result carrying the expression hash (wrapResult) — unlike the
// unsharded step, whose Construct produces a grid entity that passes through
// bare. The runtime therefore has to know it is talking to a fragment.
func (r *shardRig) evalShard(pattern, path string) ([]uint64, error) {
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	resp, err := r.ap.Executor().ExecuteOnResource(pattern, "eval", req,
		&types.ResourceTarget{Targets: []string{path}})
	if err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("status %d (type=%s)", resp.Status, resp.Type)
	}
	if resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		return nil, fmt.Errorf("compute/error code=%s message=%q", ed.Code, ed.Message)
	}
	var rd types.ComputeResultData
	if err := ecf.Decode(resp.Data, &rd); err != nil {
		return nil, fmt.Errorf("decode compute/result: %w", err)
	}
	arr, ok := rd.Value.([]interface{})
	if !ok {
		return nil, fmt.Errorf("shard value is %T, want array", rd.Value)
	}
	out := make([]uint64, 0, len(arr))
	for _, v := range arr {
		switch n := v.(type) {
		case uint64:
			out = append(out, n)
		case int64:
			out = append(out, uint64(n))
		default:
			return nil, fmt.Errorf("cell is %T, want integer", v)
		}
	}
	return out, nil
}

// TestShard_ClearsTheBudgetCliff is the point of the whole prototype: a grid
// that CANNOT tick unsharded on either engine ticks correctly when sharded.
func TestShard_ClearsTheBudgetCliff(t *testing.T) {
	const w, h = 32, 32
	seed := lifeRandCells(w, h, 42)

	// First, establish the cliff is real at this size on BOTH engines —
	// otherwise the sharded success below proves nothing.
	unsharded := newTickRig(t, w, h, buildLifeStepArith)
	if _, _, err := unsharded.tickVia("system/compute"); err == nil {
		t.Fatal("32x32 arith ticked unsharded on system/compute — the cliff this " +
			"prototype exists to clear is not there, so the test is meaningless")
	} else {
		t.Logf("unsharded 32x32 arith, system/compute: %v", err)
	}
	if _, _, err := unsharded.tickVia(axis1Pattern); err == nil {
		t.Fatal("32x32 arith ticked unsharded on axis1 — the cliff is not there")
	} else {
		t.Logf("unsharded 32x32 arith, axis1:           %v", err)
	}

	// Now shard it. k=4 gives each shard 256 cells, comfortably under the cap.
	for _, pattern := range []string{"system/compute", axis1Pattern} {
		r := newShardRig(t, w, h, 4, seed)
		cur := seed
		for gen := 1; gen <= 3; gen++ {
			want := lifeNext(w, h, cur)
			if lifePop(want) == 0 {
				t.Fatalf("gen %d: oracle grid is dead — agreement would be vacuous", gen)
			}
			got, _, err := r.tick(pattern)
			if err != nil {
				t.Fatalf("%s gen %d sharded tick: %v", pattern, gen, err)
			}
			if !cellsEq(got, want) {
				t.Fatalf("%s gen %d: sharded result diverged from the Life oracle", pattern, gen)
			}
			cur = want
		}
		t.Logf("%s: 32x32 arith ticks correctly at k=4, 3 generations", pattern)
	}
}

// TestShard_EqualsUnsharded pins the claim sharding actually rests on: a
// sharded program is the SAME program.
//
// At a size where both forms fit the budget, the sharded tick must produce a
// state entity with the SAME CONTENT HASH as the unsharded one, generation by
// generation — and at every k. Hash equality (not cell equality) is the bar,
// because the whole licence to substitute one form for another is equivalence
// at the materialized boundary (§9.1). If a k=1 and a k=7 tick disagreed by a
// byte, sharding would be a different program wearing the same name.
func TestShard_EqualsUnsharded(t *testing.T) {
	const w, h = 16, 16
	seed := lifeRandCells(w, h, 42)

	base := newTickRig(t, w, h, buildLifeStepArith)

	// k=7 does not divide 256, deliberately: it forces ragged, non-row-aligned
	// shards and would catch an off-by-one in the index split that even k values
	// hide.
	ks := []int{1, 2, 4, 7, 16}
	rigs := make(map[int]*shardRig, len(ks))
	for _, k := range ks {
		rigs[k] = newShardRig(t, w, h, k, seed)
	}

	cur := seed
	for gen := 1; gen <= 4; gen++ {
		want := lifeNext(w, h, cur)
		if lifePop(want) == 0 {
			t.Fatalf("gen %d: oracle grid is dead", gen)
		}
		_, wantHash, err := base.tickVia("system/compute")
		if err != nil {
			t.Fatalf("gen %d unsharded: %v", gen, err)
		}
		for _, k := range ks {
			got, h, err := rigs[k].tick(axis1Pattern)
			if err != nil {
				t.Fatalf("gen %d k=%d: %v", gen, k, err)
			}
			if !cellsEq(got, want) {
				t.Fatalf("gen %d k=%d: diverged from the Life oracle", gen, k)
			}
			if !hashEq(h, wantHash) {
				t.Fatalf("gen %d k=%d: state hash %s != unsharded %s — sharding "+
					"is not boundary-equivalent, so it is a different program", gen, k, h, wantHash)
			}
		}
		cur = want
	}
	t.Logf("sharded == unsharded at the materialized boundary for k=%v, 4 generations", ks)
}

// BenchmarkShardTick measures what sharding costs when you do not need it —
// the k-times grid re-read, priced.
//
//	make go ARGS="test ./entitysdk -run XXXNONE -bench BenchmarkShardTick -benchtime 20x -count=1"
func BenchmarkShardTick(b *testing.B) {
	const w, h = 16, 16
	seed := lifeRandCells(w, h, 42)

	for _, k := range []int{1, 2, 4, 8, 16} {
		b.Run(fmt.Sprintf("axis1/k=%d", k), func(b *testing.B) {
			r := newShardRig(b, w, h, k, seed)
			if _, _, err := r.tick(axis1Pattern); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := r.tick(axis1Pattern); err != nil {
					b.Fatalf("tick: %v", err)
				}
			}
		})
	}
}

// TestShard_BudgetHeadroom maps how far sharding actually reaches: for each
// grid size, the smallest k that ticks.
//
// This is the number the descriptor needs. POC §3 asked for "a budget story"
// in the tick contract — a per-program op-cost class, or a shard hint. A shard
// hint is only writable if the runtime can work out k, so this measures whether
// k is predictable (it should scale with cells/shard) or has to be discovered.
func TestShard_BudgetHeadroom(t *testing.T) {
	if testing.Short() {
		t.Skip("headroom sweep is slow")
	}
	t.Log("smallest k that ticks (arith, axis1), and cells per shard:")
	for _, n := range []int{16, 24, 32, 48, 64} {
		seed := lifeRandCells(n, n, 42)
		found := 0
		for _, k := range []int{1, 2, 4, 8, 16, 32, 64, 128} {
			r := newShardRig(t, n, n, k, seed)
			if _, _, err := r.tick(axis1Pattern); err == nil {
				found = k
				break
			}
		}
		if found == 0 {
			t.Logf("  %2dx%-2d  (%5d cells)  no k up to 128 ticks", n, n, n*n)
			continue
		}
		t.Logf("  %2dx%-2d  (%5d cells)  k=%-3d  -> %d cells/shard",
			n, n, n*n, found, n*n/found)
	}
}

// tickParallel runs the k shard evals CONCURRENTLY, then stitches in index
// order. Order is restored by writing each shard's result into its own slot, so
// completion order cannot affect the output — determinism is preserved by
// construction, not by luck.
//
// The reason this is safe is a direct consequence of the Axis-1 rung, and it is
// the connection worth noticing: a shard eval on Axis-1 performs NO WRITES. It
// reads the grid via lookup/tree and returns a bare array — no Construct, and
// crucially no CaptureScope, because live frames never materialize the closure
// environment (§13.4). Under Stage-1 the same shard writes a compute/scope
// entity to the content store on EVERY closure invocation (256 per shard), so k
// parallel Stage-1 shards would contend on the store's write lock for work that
// is semantically pure.
//
// So live frames did not only make evaluation faster — by deleting the interior
// writes they made shard evaluation side-effect-free, which is what makes it
// trivially parallel. Speed and parallelizability came from the same change.
func (r *shardRig) tickParallel(pattern string) ([]uint64, hash.Hash, error) {
	parts := make([][]uint64, len(r.shards))
	errs := make([]error, len(r.shards))
	var wg sync.WaitGroup
	for j, path := range r.shards {
		wg.Add(1)
		go func(j int, path string) {
			defer wg.Done()
			parts[j], errs[j] = r.evalShard(pattern, path)
		}(j, path)
	}
	wg.Wait()
	for j, err := range errs {
		if err != nil {
			return nil, hash.Hash{}, fmt.Errorf("shard %d: %w", j, err)
		}
	}
	cells := make([]uint64, 0, r.w*r.h)
	for _, p := range parts {
		cells = append(cells, p...)
	}
	ent, err := lifeGridEntity(r.w, r.h, cells)
	if err != nil {
		return nil, hash.Hash{}, err
	}
	h, err := r.ap.PutEntity(r.statePath, ent)
	if err != nil {
		return nil, hash.Hash{}, err
	}
	return cells, h, nil
}

// TestShard_ParallelEqualsSerial — parallel shards produce the identical state
// hash, on both engines.
//
// Determinism is the whole product here (POC §1.4: replay, save-states,
// lockstep). Parallel evaluation that produced a different hash — or a
// different hash on different runs — would trade away the one property the
// design exists to guarantee. Run repeatedly so a race has chances to show.
func TestShard_ParallelEqualsSerial(t *testing.T) {
	const w, h = 24, 24
	seed := lifeRandCells(w, h, 42)

	for _, pattern := range []string{"system/compute", axis1Pattern} {
		serial := newShardRig(t, w, h, 6, seed)
		par := newShardRig(t, w, h, 6, seed)
		for gen := 1; gen <= 3; gen++ {
			_, hs, err := serial.tick(pattern)
			if err != nil {
				t.Fatalf("%s gen %d serial: %v", pattern, gen, err)
			}
			_, hp, err := par.tickParallel(pattern)
			if err != nil {
				t.Fatalf("%s gen %d parallel: %v", pattern, gen, err)
			}
			if !hashEq(hs, hp) {
				t.Fatalf("%s gen %d: parallel hash %s != serial %s — concurrent "+
					"shard evaluation is not deterministic, which trades away "+
					"replay/lockstep", pattern, gen, hp, hs)
			}
		}
		t.Logf("%s: parallel == serial at the materialized boundary, k=6, 3 generations", pattern)
	}
}

// BenchmarkShardParallel prices the parallel fan-out against the serial one.
//
//	make go ARGS="test ./entitysdk -run XXXNONE -bench BenchmarkShardParallel -benchtime 50x -count=1"
func BenchmarkShardParallel(b *testing.B) {
	const w, h = 32, 32
	seed := lifeRandCells(w, h, 42)
	for _, k := range []int{2, 4, 8} {
		for _, mode := range []string{"serial", "parallel"} {
			b.Run(fmt.Sprintf("axis1/%s/k=%d", mode, k), func(b *testing.B) {
				r := newShardRig(b, w, h, k, seed)
				if _, _, err := r.tick(axis1Pattern); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var err error
					if mode == "serial" {
						_, _, err = r.tick(axis1Pattern)
					} else {
						_, _, err = r.tickParallel(axis1Pattern)
					}
					if err != nil {
						b.Fatalf("tick: %v", err)
					}
				}
			})
		}
	}
}
