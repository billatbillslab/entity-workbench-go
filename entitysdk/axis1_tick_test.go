package entitysdk_test

// AXIS-1 WHOLE-TICK — closing the gap the engine benchmarks could not.
//
// The engine benchmarks (axis1_cost_test.go) evaluate the step directly and
// report a 69-304x eval ratio. That number is honest but it is eval-only, and
// the results doc §6 flags the reason it cannot be quoted as a tick speedup:
// at Stage-1, eval was ~97% of the tick, so once eval gets ~70x faster the
// REMAINDER (dispatch, wrap, decode, put) stops being noise and becomes the
// tick. Amdahl, in one sentence.
//
// This file measures the actual thing. An Axis-1-backed eval handler is
// registered at a non-system pattern in the SAME peer as the stock
// system/compute handler, so one test can drive the SAME program through
// either engine over a byte-identical protocol path — executor → dispatch →
// handler → eval → materialize → wrap → decode → put. The only difference
// downstream of ExecuteOnResource is which evaluator runs.
//
// Two deliverables:
//   - TestAxis1Tick_HandlerEquivalence: the handler produces identical state
//     hashes to system/compute, generation by generation. Without this the
//     benchmark below would be timing an unvalidated path.
//   - BenchmarkAxis1Tick: gens/s, the quantity POC §2 actually reported, so the
//     report can state a tick speedup instead of extrapolating one.

import (
	"fmt"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/entitysdk/axis1"

	"github.com/fxamacker/cbor/v2"
)

// axis1Pattern is where the Axis-1 eval handler lives. Non-system on purpose:
// it sits BESIDE system/compute rather than replacing it, so both engines are
// reachable from one peer and the comparison needs no second process.
const axis1Pattern = "app/axis1/compute"

// tickRig is one peer carrying both engines' handlers plus a built Life program.
type tickRig struct {
	ap  *entitysdk.AppPeer
	eng *axis1.Engine
	p   *lifeProgram
	w   int
}

func newTickRig(t testing.TB, w, h int, build func(*entitysdk.AppPeer, int, int, string) *entitysdk.Builder) *tickRig {
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
	p := lifeSetup(t, ap, fmt.Sprintf("app/life/tick-%dx%d", w, h), w, h,
		lifeRandCells(w, h, 42), build)
	return &tickRig{ap: ap, eng: eng, p: p, w: w}
}

// tickVia runs one whole descriptor-faithful tick through the given handler
// pattern: eval the step, decode the materialized grid, write it back.
//
// This is lifeProgram.tick() verbatim except for the handler pattern — kept as
// a copy rather than refactoring the probe, because exp_compute_life_test.go is
// the frozen record carrying the POC's oracle and replay proofs (POC §3.5) and
// should not shift under a later experiment.
func (r *tickRig) tickVia(pattern string) (*lifeGrid, hash.Hash, error) {
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return nil, hash.Hash{}, err
	}
	resp, err := r.ap.Executor().ExecuteOnResource(pattern, "eval", req,
		&types.ResourceTarget{Targets: []string{r.p.stepPath}})
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
		return nil, hash.Hash{}, fmt.Errorf("expected %s, got type=%s", lifeGridType, resp.Type)
	}
	var g lifeGrid
	if err := ecf.Decode(resp.Data, &g); err != nil {
		return nil, hash.Hash{}, fmt.Errorf("decode grid: %w", err)
	}
	ent, err := entity.NewEntity(lifeGridType, cbor.RawMessage(resp.Data))
	if err != nil {
		return nil, hash.Hash{}, err
	}
	h, err := r.ap.PutEntity(r.p.statePath, ent)
	if err != nil {
		return nil, hash.Hash{}, fmt.Errorf("put state: %w", err)
	}
	return &g, h, nil
}

// TestAxis1Tick_HandlerEquivalence — the Axis-1 handler is substitutable for
// system/compute over the real protocol path.
//
// D3 cross-engine already proved the ENGINE agrees. This proves the HANDLER
// around it agrees too: the response entity, its type, and the resulting state
// hash are identical, generation by generation. That covers everything the
// handler re-derives from the unexported reference — wrapResult's bare-entity
// passthrough, the materialize-before-wrap ordering, and the F10 error-as-value
// status. A benchmark over an unvalidated path would just be fast nonsense.
func TestAxis1Tick_HandlerEquivalence(t *testing.T) {
	const w, h = 16, 16

	// Two rigs, same seed, same lowering — one ticked through each handler. They
	// must trace identical state-hash sequences.
	s1 := newTickRig(t, w, h, buildLifeStepArith)
	a1 := newTickRig(t, w, h, buildLifeStepArith)

	cur := lifeRandCells(w, h, 42)
	for gen := 1; gen <= 5; gen++ {
		want := lifeNext(w, h, cur)
		// F-D3's lesson: assert the grid is alive, or a dead board makes every
		// agreement below vacuous.
		if lifePop(want) == 0 {
			t.Fatalf("gen %d: oracle grid is dead — agreement would be vacuous", gen)
		}

		gs, hs, err := s1.tickVia("system/compute")
		if err != nil {
			t.Fatalf("gen %d system/compute: %v", gen, err)
		}
		ga, ha, err := a1.tickVia(axis1Pattern)
		if err != nil {
			t.Fatalf("gen %d %s: %v", gen, axis1Pattern, err)
		}
		if !cellsEq(gs.Cells, want) {
			t.Fatalf("gen %d: system/compute diverged from the Life oracle", gen)
		}
		if !cellsEq(ga.Cells, want) {
			t.Fatalf("gen %d: axis1 handler diverged from the Life oracle", gen)
		}
		if !hashEq(hs, ha) {
			t.Fatalf("gen %d: state hash %s (axis1) != %s (system/compute) — "+
				"the handler is not substitutable", gen, ha, hs)
		}
		cur = want
	}

	st := a1.eng.Stats()
	if st.Fallbacks != 0 {
		t.Fatalf("axis1 handler hit %d fallbacks — it ran Stage-1 and proved nothing", st.Fallbacks)
	}
	if st.Evals != 5 {
		t.Fatalf("expected 5 evals through the handler, got %d", st.Evals)
	}
	if st.Decodes != 1 {
		t.Fatalf("expected 1 root decode across 5 ticks, got %d — the cache is "+
			"not surviving the handler path", st.Decodes)
	}
	t.Logf("handler equivalence holds over 5 generations; axis1 stats: %+v", st)
}

// BenchmarkAxis1Tick is the POC §2 quantity: whole descriptor-faithful ticks
// through the protocol, per engine, directly comparable to the POC's gens/s.
//
//	make go ARGS="test ./entitysdk -run XXXNONE -bench BenchmarkAxis1Tick -benchtime 20x -count=1"
//
// Each iteration evals AND writes state back, so the grid advances and the work
// is the real per-generation work — not the same expression re-evaluated against
// a frozen board.
func BenchmarkAxis1Tick(b *testing.B) {
	for _, n := range []int{8, 16, 24} {
		for _, lname := range []string{"arith", "table"} {
			build := buildLifeStepArith
			if lname == "table" {
				build = buildLifeStepTable
			}

			b.Run(fmt.Sprintf("system-compute/%s/%dx%d", lname, n, n), func(b *testing.B) {
				rig := newTickRig(b, n, n, build)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := rig.tickVia("system/compute"); err != nil {
						b.Fatalf("tick: %v", err)
					}
				}
			})

			b.Run(fmt.Sprintf("axis1/%s/%dx%d", lname, n, n), func(b *testing.B) {
				rig := newTickRig(b, n, n, build)
				// Warm the decode cache outside the timed loop — the rung's claim
				// is about steady-state ticks.
				if _, _, err := rig.tickVia(axis1Pattern); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := rig.tickVia(axis1Pattern); err != nil {
						b.Fatalf("tick: %v", err)
					}
				}
				b.StopTimer()
				if st := rig.eng.Stats(); st.Fallbacks != 0 {
					b.Fatalf("%d fallbacks — measurement is Stage-1 in disguise", st.Fallbacks)
				}
			})
		}
	}
}
