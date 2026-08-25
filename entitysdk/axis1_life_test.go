package entitysdk_test

// AXIS-1 × LIFE — D3 widened from lowerings to ENGINES (handoff §5, second
// acceptance bullet; exploration §13.8 item 2).
//
// TestExpLifeD3_LoweringsConverge is the track's de-facto compiler oracle: two
// different lowerings of the same program (arith vs. table) converge on
// identical materialized state hashes every generation. That is compiler
// correctness in miniature — equivalence at the materialized boundary is what
// licenses substituting one form for another (POC §5.3).
//
// This file widens the same claim across the engine axis. Four pairings —
// S1-arith, S1-table, Axis1-arith, Axis1-table — must ALL agree, generation by
// generation. Two lowerings × two engines is the full cross product: if any
// cell of it disagrees, some "equivalent" rewrite isn't.
//
// Why this matters beyond a smoke test: the differential sweep
// (axis1_equivalence_test.go) generates small graphs. Life is a real program
// with a real tick loop, a tree read on the impure frontier, a map over 144+
// elements with a capturing closure, and a construct at the boundary written
// back every generation. It is the closest thing available to the workload the
// cost sweep measures.

import (
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/ext/compute"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/entitysdk/axis1"
)

// axis1Life drives a lifeProgram's tick loop on the Axis-1 engine instead of
// through system/compute:eval.
//
// It has to bypass the executor: the protocol path is hardwired to Stage-1, so
// running Life "through the peer" on Axis-1 is not possible without registering
// a replacement compute handler. Everything else about the tick is the
// descriptor-faithful loop from the probe — eval the step, materialize, write
// the grid back to the state path — so the only difference from
// lifeProgram.tick() is which engine evaluates.
type axis1Life struct {
	ap     *entitysdk.AppPeer
	eng    *axis1.Engine
	ctx    *compute.EvalContext
	p      *lifeProgram
	stepEn entity.Entity
}

func newAxis1Life(t testing.TB, ap *entitysdk.AppPeer, eng *axis1.Engine, p *lifeProgram) *axis1Life {
	t.Helper()
	h, ok := ap.TestLocationIndex().Get(ap.PeerID() + "/" + p.stepPath)
	if !ok {
		// The probe builds at a bare path; the index stores it canonicalized.
		h, ok = ap.TestLocationIndex().Get(p.stepPath)
		if !ok {
			t.Fatalf("step entity not found at %s", p.stepPath)
		}
	}
	ent, ok := ap.TestContentStore().Get(h)
	if !ok {
		t.Fatalf("step entity %s not in content store", h)
	}
	return &axis1Life{ap: ap, eng: eng, ctx: ap.TestEvalContext(), p: p, stepEn: ent}
}

// tick runs one generation on Axis-1 and writes the materialized grid back.
func (a *axis1Life) tick() (*lifeGrid, hash.Hash, error) {
	v, err := a.eng.EvaluateMaterialized(a.stepEn, nil,
		compute.NewBudget(compute.DefaultMaxOps, compute.DefaultMaxDepth), a.ctx)
	if err != nil {
		return nil, hash.Hash{}, err
	}
	ent, ok := v.(entity.Entity)
	if !ok {
		return nil, hash.Hash{}, errNotEntity(v)
	}
	var g lifeGrid
	if err := ecf.Decode(ent.Data, &g); err != nil {
		return nil, hash.Hash{}, err
	}
	h, err := a.ap.PutEntity(a.p.statePath, ent)
	if err != nil {
		return nil, hash.Hash{}, err
	}
	return &g, h, nil
}

func errNotEntity(v interface{}) error {
	return &compute.ComputeError{
		Code:    compute.ErrTypeMismatch,
		Message: "axis1 life tick: step did not materialize to an entity",
	}
}

// TestAxis1Life_D3CrossEngine is the acceptance criterion: all four
// lowering×engine pairings agree on identical state hashes every generation.
func TestAxis1Life_D3CrossEngine(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	const w, h = 12, 12
	seed := lifeRandCells(w, h, 42)
	eng := axis1.NewEngine()

	// Four independent program instances so each engine/lowering pairing owns
	// its own state path and cannot accidentally read another's writes — the
	// four are meant to converge, not to share.
	s1Arith := lifeSetup(t, ap, "app/life/x-s1-arith", w, h, seed, buildLifeStepArith)
	s1Table := lifeSetup(t, ap, "app/life/x-s1-table", w, h, seed, buildLifeStepTable)
	a1ArithP := lifeSetup(t, ap, "app/life/x-a1-arith", w, h, seed, buildLifeStepArith)
	a1TableP := lifeSetup(t, ap, "app/life/x-a1-table", w, h, seed, buildLifeStepTable)
	a1Arith := newAxis1Life(t, ap, eng, a1ArithP)
	a1Table := newAxis1Life(t, ap, eng, a1TableP)

	cur := seed
	for gen := 1; gen <= 5; gen++ {
		want := lifeNext(w, h, cur)

		// F-D3's durable lesson: a degenerate seed once let D3 compare two dead
		// grids and pass. The assertion, not the seed, is the fix — so check the
		// grid is alive on every generation before trusting any agreement below.
		if lifePop(want) == 0 {
			t.Fatalf("gen %d: oracle grid is dead — the seed is degenerate and "+
				"every convergence claim below would be vacuous", gen)
		}

		gs1a, hs1a, err := s1Arith.tick()
		if err != nil {
			t.Fatalf("gen %d s1/arith: %v", gen, err)
		}
		gs1t, hs1t, err := s1Table.tick()
		if err != nil {
			t.Fatalf("gen %d s1/table: %v", gen, err)
		}
		ga1a, ha1a, err := a1Arith.tick()
		if err != nil {
			t.Fatalf("gen %d axis1/arith: %v", gen, err)
		}
		ga1t, ha1t, err := a1Table.tick()
		if err != nil {
			t.Fatalf("gen %d axis1/table: %v", gen, err)
		}

		// Every pairing must match the independent Go oracle...
		for name, g := range map[string]*lifeGrid{
			"s1/arith": gs1a, "s1/table": gs1t, "axis1/arith": ga1a, "axis1/table": ga1t,
		} {
			if !cellsEq(g.Cells, want) {
				t.Fatalf("gen %d: %s diverged from the Life oracle", gen, name)
			}
		}

		// ...and, the actual §9.1 claim, agree at the materialized boundary.
		// Hash equality is the licence to substitute one for another; cell
		// equality above would not be enough (two engines could agree on the
		// grid and still encode it differently).
		for name, got := range map[string]hash.Hash{
			"s1/table": hs1t, "axis1/arith": ha1a, "axis1/table": ha1t,
		} {
			if !hashEq(hs1a, got) {
				t.Fatalf("gen %d: %s state hash %s != s1/arith %s — "+
					"the materialized boundary diverged across the %s axis",
					gen, name, got, hs1a, axisOf(name))
			}
		}
		cur = want
	}

	st := eng.Stats()
	if st.Fallbacks != 0 {
		t.Fatalf("axis1 Life hit %d Stage-1 fallbacks — the run compared "+
			"Stage-1 against itself, so cross-engine agreement proves nothing", st.Fallbacks)
	}
	if st.Closures == 0 {
		t.Fatal("axis1 Life built no closures — the map/live-frame path never ran")
	}
	// Two unique step IRs (arith, table), each decoded once across 5 generations.
	if st.Decodes != 2 {
		t.Fatalf("expected 2 root decodes (arith + table) across 10 ticks, got %d — "+
			"decode-once is not holding on the real workload", st.Decodes)
	}
	t.Logf("D3 holds across 4 pairings x 5 generations; axis1 stats: %+v", st)
}

// axisOf labels which axis a divergence crossed, so the failure says whether
// the lowering or the engine is implicated.
func axisOf(name string) string {
	switch name {
	case "s1/table":
		return "lowering"
	case "axis1/arith":
		return "engine"
	default:
		return "engine+lowering"
	}
}
