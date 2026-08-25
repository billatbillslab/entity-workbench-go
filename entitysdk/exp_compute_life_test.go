package entitysdk_test

// EXPERIMENT-LIFE (Exp-D) — Conway's Game of Life as a hostable compute
// program: the first end-to-end probe of the runtime interface contract
// and the Stage-1 cost model.
//
// Arch context (entity-system-architecture):
//   docs/research/explorations/EXPLORATION-COMPUTE-PROGRAM-RUNTIME-CONTRACT.md
//     §5  tick must be host-driven (this file's tick loop is that contract)
//     §6  the Life sketch this implements; the cost-model deliverable
//     §9.1 determinism pinned at the materialized boundary (D3 validates)
//   docs/proposals/PROPOSAL-APP-CONVENTION-COMPUTE-PROGRAM.md (DRAFT descriptor)
//
// What this file implements, descriptor-faithfully:
//   - program  = state at a tree path ("app/life/.../state") + a pure step
//     expression (grid → grid') built with the S1 builder, stored in the tree.
//   - runtime  = the host tick loop: system/compute:eval on the step path →
//     decode the materialized grid entity → PutEntity back to the state path.
//     (No reactive install on state — the §5 cascade rule.)
//   - output   = the state path itself is the snapshot output port; the ASCII
//     renderer in D2 is the thinnest possible display driver.
//
// Two step lowerings are built for the SAME program (a real "compute
// options" comparison):
//   - arith: per-neighbor toroidal wrap via modular arithmetic — the
//     exploration §6.1 shape, unrolled (no fold; ~150 evals/cell est.).
//   - table: a static per-cell neighbor-index table carried as literal
//     data — the §7.2 "blockmap" pattern (static content-addressed
//     index; ~70 evals/cell est.).
//
// Tests:
//   D1 blinker oscillates (period 2) + constructed state hash == hand-built
//      hash (v3.19c construct-materializes-bare, checked at the boundary).
//   D2 glider travels, oracle-checked per generation, ASCII frames logged.
//   D3 both lowerings converge on identical state hashes every generation —
//      the §9.1 claim (equivalence at the materialized boundary) live.
//   D4 budget cliff: the spec-recommended eval budget (compute.DefaultMaxOps
//      = 100k, core-go initBudget can only lower it) caps one generation's
//      grid size per lowering. Empirical map logged as cost-model data.
//
// Benchmarks (perf numbers are ONLY honest without -race; see AGENTS.md):
//   make go ARGS="test ./entitysdk -run XXXNONE -bench BenchmarkExpLifeGen -benchtime 10x"
//   add -cpuprofile for the hash-vs-decode-vs-eval cost split.
//
// MEASURED RESULTS — 2026-07-15, i5-11400, in-memory store, no -race
// (machine-specific; re-run for current numbers):
//
//   throughput (whole descriptor-faithful tick: eval + decode + put):
//     arith  8x8 133 gens/s · 16x16 36 · 24x24 14    (~8.5k cells/s, flat)
//     table  8x8 206 gens/s · 16x16 30 · 24x24  7 · 32x32 2.3  (superlinear decay)
//   budget cliff (DefaultMaxOps=100k, not raisable): arith ≤24x24; table ≤32x32.
//   cost split (arith/16x16 profile): ~87% inside compute.Evaluate, of which
//     ~54% cbor.Unmarshal (per-node expression decode), ~12% resolve,
//     ~9% content-store Get; boundary hashing + real arithmetic ≈ noise.
//     Empirically confirms GUIDE-CORE §6 / EXPLORATION §9.1: the
//     compiler-removable interpretation artifact is nearly all of the cost;
//     the always-kept materialized boundary is nearly free. (A Stage-1.5
//     decoded-node cache alone would cut >50% before any compilation.)
//
// Two findings routed from this probe (for core-go / the lowering toolkit):
//   F-D1 (lowering rule): compute `div` is TRUE division — non-exact
//        quotients return float (ext/compute/eval_arith.go Rule 9), which
//        then fails `mod` ("Modulo requires integer operands"). Integer
//        floor-div lowers as div(sub(a, mod(a,b)), b). The EXPLORATION §6.1
//        sketch assumed integer div; the lowering toolkit must encode this.
//   F-D2 (Stage-1 evaluator perf, core-go): map/filter/fold call
//        invokeClosure per element, and invokeClosure re-decodes the closure
//        entity AND LoadScope-loads the full captured environment per element
//        (ext/compute/builtins.go::invokeClosure). A captured O(N) collection
//        therefore makes iteration O(N^2) per tick — LoadScope measured at
//        67% of CPU for table/24x24. Caching the decoded env per builtin
//        call would restore linearity; until then, big static tables are
//        cheaper re-derived arithmetically per element than captured
//        (inverting the §7.2 static-index intuition at Stage 1).

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

// --- Go-side oracle -----------------------------------------------------

// lifeNext is the reference B3/S23 step on a toroidal w×h grid.
func lifeNext(w, h int, cells []uint64) []uint64 {
	next := make([]uint64, len(cells))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			n := 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					nx := (x + dx + w) % w
					ny := (y + dy + h) % h
					if cells[ny*w+nx] == 1 {
						n++
					}
				}
			}
			i := y*w + x
			if n == 3 || (n == 2 && cells[i] == 1) {
				next[i] = 1
			}
		}
	}
	return next
}

// lifeASCII renders a grid for t.Logf — the thinnest display driver.
func lifeASCII(w, h int, cells []uint64) string {
	var sb strings.Builder
	sb.WriteByte('\n')
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if cells[y*w+x] == 1 {
				sb.WriteString("█")
			} else {
				sb.WriteString("·")
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// lifeRandCells produces a deterministic pseudo-random seed grid (LCG —
// the same trick the Snake sketch uses in-IR, here on the Go side).
//
// The density test reads the LCG's HIGH bits. This is load-bearing: a
// power-of-two-modulus LCG has period 2^k in its low k bits, so the
// original `s%8` cycled with period 8 in the cell index — a lattice on
// every grid size, and identical rows (vertical stripes) on any width
// divisible by 8. D3's 12x12 grid went extinct at gen 4 as a result,
// making the last two of its five generations a comparison of two empty
// grids. Same fix as wb.lifeSeedState (workbench/program_life.go).
func lifeRandCells(w, h int, seed uint64) []uint64 {
	cells := make([]uint64, w*h)
	s := seed
	for i := range cells {
		s = (s*1103515245 + 12345) % 2147483648
		if (s>>16)%8 < 3 { // ~37% density
			cells[i] = 1
		}
	}
	return cells
}

// --- grid entity (the state at the boundary) ----------------------------

const lifeGridType = "app/life/grid"

// lifeGridEntity hand-builds the state entity — the same bare shape the
// step's construct must materialize to (v3.19c: construct == hand-built).
func lifeGridEntity(w, h int, cells []uint64) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{
		"width":  uint64(w),
		"height": uint64(h),
		"cells":  cells,
	})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(lifeGridType, cbor.RawMessage(raw))
}

type lifeGrid struct {
	Width  uint64   `cbor:"width"`
	Height uint64   `cbor:"height"`
	Cells  []uint64 `cbor:"cells"`
}

// --- the two step lowerings ----------------------------------------------

// lifeRule wraps a neighbor-count expression with the B3/S23 rule. The
// caller supplies `count`/`alive` via Let so the shared reads are bound
// once (the interpreter re-evaluates every reference otherwise — DAG
// sharing does not save evals at Stage 1).
func lifeRule(c *entitysdk.ComputeBuilder) *entitysdk.Builder {
	return c.If(
		c.Logic("or",
			c.Compare("eq", c.LookupScope("count"), c.Literal(uint64(3))),
			c.Logic("and",
				c.Compare("eq", c.LookupScope("count"), c.Literal(uint64(2))),
				c.LookupScope("alive"))),
		c.Literal(uint64(1)),
		c.Literal(uint64(0)))
}

// lifeAddTree folds 8 neighbor reads into a balanced add tree.
func lifeAddTree(c *entitysdk.ComputeBuilder, terms []*entitysdk.Builder) *entitysdk.Builder {
	for len(terms) > 1 {
		next := make([]*entitysdk.Builder, 0, (len(terms)+1)/2)
		for i := 0; i < len(terms); i += 2 {
			if i+1 < len(terms) {
				next = append(next, c.Arithmetic("add", terms[i], terms[i+1]))
			} else {
				next = append(next, terms[i])
			}
		}
		terms = next
	}
	return terms[0]
}

var lifeOffsets = [8][2]int{
	{-1, -1}, {0, -1}, {1, -1},
	{-1, 0}, {1, 0},
	{-1, 1}, {0, 1}, {1, 1},
}

// buildLifeStepArith lowers the step with per-neighbor modular
// arithmetic (EXPLORATION §6.1, unrolled). Width/height are known at
// build time (the expression is per-grid-size, like the indices array
// the exploration's sketches all carry), so the wrap adds fold to
// non-negative uint arithmetic: nx = mod(x + (w+dx), w).
func buildLifeStepArith(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
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

	// Lowering rule (found live): compute `div` is TRUE division — a
	// non-exact quotient returns a float (eval_arith.go Rule 9), which
	// then poisons `mod` ("Modulo requires integer operands"). Integer
	// floor-div lowers as div(sub(i, mod(i,w)), w) — exact by
	// construction. `let` is sequential (let*, sorted binding order),
	// so "y" may reference "x".
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

	return lifeWrapStep(c, w, h, statePath, perCell, nil)
}

// buildLifeStepTable lowers the step with a static neighbor-index table
// (the §7.2 blockmap pattern: a precomputed spatial index carried as one
// content-addressed literal, navigated with index — pure, no per-node
// entities). Cuts per-cell op count roughly in half vs. arith — which
// buys one budget rung (32x32 fits) — but at Stage 1 it LOSES on wall
// time beyond ~8x8: the closure captures the O(N) table, and F-D2 (see
// header) makes every element invocation re-decode it. Ops-cheaper and
// wall-slower at once — a live example of the §9.1 point that interior
// cost is an interpretation artifact, not a property of the program.
func buildLifeStepTable(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
	c := ap.Compute()

	table := make([][]uint64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			nbrs := make([]uint64, 0, 8)
			for _, d := range lifeOffsets {
				nx := (x + d[0] + w) % w
				ny := (y + d[1] + h) % h
				nbrs = append(nbrs, uint64(ny*w+nx))
			}
			table[y*w+x] = nbrs
		}
	}

	terms := make([]*entitysdk.Builder, 0, 8)
	for k := 0; k < 8; k++ {
		terms = append(terms, c.Index(c.LookupScope("cells"),
			c.Index(c.LookupScope("nbrs"), c.Literal(uint64(k)))))
	}

	perCell := c.Let(map[string]*entitysdk.Builder{
		"nbrs": c.Index(c.LookupScope("nbr_table"), c.LookupScope("i")),
	}, c.Let(map[string]*entitysdk.Builder{
		"count": lifeAddTree(c, terms),
		"alive": c.Compare("eq",
			c.Index(c.LookupScope("cells"), c.LookupScope("i")),
			c.Literal(uint64(1))),
	}, lifeRule(c)))

	extra := map[string]*entitysdk.Builder{"nbr_table": c.Literal(table)}
	return lifeWrapStep(c, w, h, statePath, perCell, extra)
}

// lifeWrapStep shares the outer shape of both lowerings:
//
//	let g = lookup/tree(statePath)                 ; the impure edge
//	in let cells = field(g, "cells") [, extras…]   ; bound BEFORE the
//	   in construct(app/life/grid,                 ;   lambda so the closure
//	        {width, height,                        ;   captures plain values
//	         cells: map(indices, λi. perCell)})    ;   (Scenario A.3 pattern)
func lifeWrapStep(c *entitysdk.ComputeBuilder, w, h int, statePath string,
	perCell *entitysdk.Builder, extraBindings map[string]*entitysdk.Builder) *entitysdk.Builder {

	indices := make([]uint64, w*h)
	for i := range indices {
		indices[i] = uint64(i)
	}

	body := c.Construct(lifeGridType, map[string]*entitysdk.Builder{
		"width":  c.Literal(uint64(w)),
		"height": c.Literal(uint64(h)),
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         c.Lambda([]string{"i"}, perCell),
		}),
	})

	mid := map[string]*entitysdk.Builder{
		"cells": c.Field(c.LookupScope("g"), "cells"),
	}
	for k, v := range extraBindings {
		mid[k] = v
	}

	return c.Let(map[string]*entitysdk.Builder{
		"g": c.LookupTreeLocal(statePath),
	}, c.Let(mid, body))
}

// --- the host runtime (tick loop) ----------------------------------------

// lifeProgram is one seeded, built program instance: state at a tree
// path, step expression at a sibling path.
type lifeProgram struct {
	ap        *entitysdk.AppPeer
	w, h      int
	statePath string
	stepPath  string
}

func lifeSetup(t testing.TB, ap *entitysdk.AppPeer, root string, w, h int, seed []uint64,
	build func(*entitysdk.AppPeer, int, int, string) *entitysdk.Builder) *lifeProgram {
	t.Helper()
	p := &lifeProgram{ap: ap, w: w, h: h,
		statePath: root + "/state", stepPath: root + "/step"}

	seedEnt, err := lifeGridEntity(w, h, seed)
	if err != nil {
		t.Fatalf("seed grid entity: %v", err)
	}
	if _, err := ap.PutEntity(p.statePath, seedEnt); err != nil {
		t.Fatalf("put seed state: %v", err)
	}
	if _, err := build(ap, w, h, p.statePath).Build(context.Background(), p.stepPath); err != nil {
		t.Fatalf("build step: %v", err)
	}
	return p
}

// tick is the §5 host-clocked loop body: explicit eval of the step,
// write the materialized grid back to the state path. Returns the new
// grid and the state entity's content hash (the replay/lockstep handle).
func (p *lifeProgram) tick() (*lifeGrid, hash.Hash, error) {
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return nil, hash.Hash{}, err
	}
	resp, err := p.ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{p.stepPath}})
	if err != nil {
		return nil, hash.Hash{}, fmt.Errorf("eval dispatch: %w", err)
	}
	if resp.Status != 200 {
		return nil, hash.Hash{}, fmt.Errorf("eval status %d (type=%s)", resp.Status, resp.Type)
	}
	if resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		return nil, hash.Hash{}, fmt.Errorf("compute/error code=%s message=%q", ed.Code, ed.Message)
	}
	if resp.Type != lifeGridType {
		return nil, hash.Hash{}, fmt.Errorf("expected %s result, got type=%s", lifeGridType, resp.Type)
	}
	var g lifeGrid
	if err := ecf.Decode(resp.Data, &g); err != nil {
		return nil, hash.Hash{}, fmt.Errorf("decode grid: %w", err)
	}
	ent, err := entity.NewEntity(lifeGridType, cbor.RawMessage(resp.Data))
	if err != nil {
		return nil, hash.Hash{}, err
	}
	h, err := p.ap.PutEntity(p.statePath, ent)
	if err != nil {
		return nil, hash.Hash{}, fmt.Errorf("put state: %w", err)
	}
	return &g, h, nil
}

func hashEq(a, b hash.Hash) bool { return bytes.Equal(a.Digest[:], b.Digest[:]) }

// lifePop counts live cells — used to assert a probe grid is actually
// alive, not just self-consistently dead.
func lifePop(cells []uint64) int {
	n := 0
	for _, c := range cells {
		if c == 1 {
			n++
		}
	}
	return n
}

func cellsEq(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- D1: blinker + boundary determinism -----------------------------------

func TestExpLifeD1_BlinkerOscillates(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	const w, h = 5, 5
	seed := make([]uint64, w*h)
	seed[2*w+1], seed[2*w+2], seed[2*w+3] = 1, 1, 1 // horizontal blinker

	p := lifeSetup(t, ap, "app/life/d1", w, h, seed, buildLifeStepArith)
	t.Logf("gen 0:%s", lifeASCII(w, h, seed))

	cur := seed
	for gen := 1; gen <= 2; gen++ {
		want := lifeNext(w, h, cur)
		got, gotHash, err := p.tick()
		if err != nil {
			t.Fatalf("gen %d tick: %v", gen, err)
		}
		t.Logf("gen %d:%s", gen, lifeASCII(w, h, got.Cells))
		if !cellsEq(got.Cells, want) {
			t.Fatalf("gen %d: compute step diverged from oracle\ngot: %v\nwant: %v", gen, got.Cells, want)
		}

		// §9.1 boundary determinism: the constructed state entity must be
		// byte-identical to a hand-built one (v3.19c construct-materializes-
		// bare) — same materialized boundary ⇒ same content hash.
		wantEnt, err := lifeGridEntity(w, h, want)
		if err != nil {
			t.Fatal(err)
		}
		if !hashEq(gotHash, wantEnt.ContentHash) {
			t.Fatalf("gen %d: constructed state hash != hand-built hash (construct-materializes-bare violated?)\ngot:  %x\nwant: %x",
				gen, gotHash.Digest, wantEnt.ContentHash.Digest)
		}
		cur = want
	}
	if !cellsEq(cur, seed) {
		t.Fatalf("blinker should have period 2; gen 2 != gen 0")
	}
	t.Logf("PASS D1: blinker oscillates; constructed state hash == hand-built hash at every tick boundary")
}

// --- D2: glider, oracle-checked, ASCII frames ------------------------------

func TestExpLifeD2_GliderTravels(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	const w, h = 8, 8
	seed := make([]uint64, w*h)
	for _, xy := range [][2]int{{1, 0}, {2, 1}, {0, 2}, {1, 2}, {2, 2}} { // glider
		seed[xy[1]*w+xy[0]] = 1
	}

	p := lifeSetup(t, ap, "app/life/d2", w, h, seed, buildLifeStepArith)
	t.Logf("gen 0:%s", lifeASCII(w, h, seed))

	cur := seed
	const gens = 12
	for gen := 1; gen <= gens; gen++ {
		want := lifeNext(w, h, cur)
		got, _, err := p.tick()
		if err != nil {
			t.Fatalf("gen %d tick: %v", gen, err)
		}
		t.Logf("gen %d:%s", gen, lifeASCII(w, h, got.Cells))
		if !cellsEq(got.Cells, want) {
			t.Fatalf("gen %d: compute step diverged from oracle", gen)
		}
		cur = want
	}
	// A glider on a torus translates by (1,1) every 4 generations; 12 gens
	// = (3,3). Population stays 5 throughout (oracle equality already
	// guarantees this; the count is a cheap sanity backstop).
	pop := uint64(0)
	for _, v := range cur {
		pop += v
	}
	if pop != 5 {
		t.Fatalf("glider population should stay 5, got %d", pop)
	}
	t.Logf("PASS D2: glider matches oracle for %d generations", gens)
}

// --- D3: two lowerings, one materialized boundary --------------------------

func TestExpLifeD3_LoweringsConverge(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	const w, h = 12, 12
	seed := lifeRandCells(w, h, 42)

	pa := lifeSetup(t, ap, "app/life/d3-arith", w, h, seed, buildLifeStepArith)
	pt := lifeSetup(t, ap, "app/life/d3-table", w, h, seed, buildLifeStepTable)

	cur := seed
	for gen := 1; gen <= 5; gen++ {
		want := lifeNext(w, h, cur)
		ga, ha, err := pa.tick()
		if err != nil {
			t.Fatalf("gen %d arith tick: %v", gen, err)
		}
		gt, ht, err := pt.tick()
		if err != nil {
			t.Fatalf("gen %d table tick: %v", gen, err)
		}
		if !cellsEq(ga.Cells, want) || !cellsEq(gt.Cells, want) {
			t.Fatalf("gen %d: a lowering diverged from oracle", gen)
		}
		// §9.1: two different lowerings of the same step are equivalent
		// exactly when they produce the same materialized boundary
		// entities — i.e. identical state content hashes, every tick.
		if !hashEq(ha, ht) {
			t.Fatalf("gen %d: lowerings produced different state hashes (%x vs %x)",
				gen, ha.Digest, ht.Digest)
		}
		// The equivalence claim is only as strong as the grid it runs
		// on: two lowerings of a DEAD board agree trivially. The
		// original seed helper read the LCG's low bits and went extinct
		// at gen 4, so gens 4-5 compared two empty grids and proved
		// nothing while still passing. Assert the board is actually
		// alive, so this test can never silently go vacuous again.
		if n := lifePop(want); n == 0 {
			t.Fatalf("gen %d: the seed grid is extinct — this test is comparing "+
				"two empty grids and proving nothing (degenerate seed?)", gen)
		}
		cur = want
	}
	t.Logf("PASS D3: arith and table lowerings converge on identical state hashes for 5 generations, on a live grid (equivalence at the materialized boundary)")
}

// --- D4: the budget cliff (cost-model data) --------------------------------

// TestExpLifeD4_BudgetCliff maps where one generation stops fitting in a
// single eval's op budget. core-go pins eval to compute.DefaultMaxOps
// (100k; EXTENSION-COMPUTE §9.3 recommended limit) and initBudget can
// only lower it (ext/compute/handler.go::initBudget) — there is no
// raise surface. So grid size per eval is hard-capped per lowering.
// This is a real §8 runtime-contract finding: a tick whose step exceeds
// the budget cannot run at all — the descriptor/tick contract will need
// either a budget field, sharded evals, or the Stage-2/3 gradient.
func TestExpLifeD4_BudgetCliff(t *testing.T) {
	if testing.Short() {
		t.Skip("budget sweep is slow under -short")
	}
	variants := []struct {
		name  string
		build func(*entitysdk.AppPeer, int, int, string) *entitysdk.Builder
	}{
		{"arith", buildLifeStepArith},
		{"table", buildLifeStepTable},
	}
	sizes := []int{8, 12, 16, 24, 32, 40}

	for _, v := range variants {
		results := make([]string, 0, len(sizes))
		for _, n := range sizes {
			// Fresh peer per size: keeps the tree/store clean so cliff
			// placement isn't polluted by prior expression subgraphs.
			ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			p := lifeSetup(t, ap, fmt.Sprintf("app/life/d4-%s-%d", v.name, n),
				n, n, lifeRandCells(n, n, 7), v.build)
			_, _, err = p.tick()
			switch {
			case err == nil:
				results = append(results, fmt.Sprintf("%dx%d OK", n, n))
			case strings.Contains(err.Error(), "budget"):
				results = append(results, fmt.Sprintf("%dx%d BUDGET(%v)", n, n, err))
			default:
				t.Fatalf("%s %dx%d: unexpected failure (not budget): %v", v.name, n, n, err)
			}
			_ = ap.Close()
		}
		t.Logf("D4 %s cliff sweep (DefaultMaxOps=100k): %s", v.name, strings.Join(results, "; "))
	}
}

// --- benchmarks: the throughput deliverable --------------------------------

// BenchmarkExpLifeGen measures whole generations through the descriptor-
// faithful host loop (eval + decode + PutEntity): generations/sec is the
// inverse of ns/op; cells/s is reported as a custom metric. Sizes stay
// under each lowering's D4 budget cliff.
//
// Honest numbers require no -race:
//
//	make go ARGS="test ./entitysdk -run '^$' -bench BenchmarkExpLifeGen -benchtime 10x"
func BenchmarkExpLifeGen(b *testing.B) {
	cases := []struct {
		variant string
		build   func(*entitysdk.AppPeer, int, int, string) *entitysdk.Builder
		sizes   []int
	}{
		{"arith", buildLifeStepArith, []int{8, 16, 24}},
		{"table", buildLifeStepTable, []int{8, 16, 24, 32}},
	}
	for _, c := range cases {
		for _, n := range c.sizes {
			b.Run(fmt.Sprintf("%s/%dx%d", c.variant, n, n), func(b *testing.B) {
				ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = ap.Close() }()
				p := lifeSetup(b, ap, fmt.Sprintf("app/life/bench-%s-%d", c.variant, n),
					n, n, lifeRandCells(n, n, 7), c.build)

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := p.tick(); err != nil {
						b.Fatalf("tick %d: %v", i, err)
					}
				}
				b.StopTimer()
				secs := b.Elapsed().Seconds()
				if secs > 0 {
					b.ReportMetric(float64(n*n)*float64(b.N)/secs, "cells/s")
					b.ReportMetric(float64(b.N)/secs, "gens/s")
				}
			})
		}
	}
}
