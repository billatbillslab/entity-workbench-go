package entitysdk_test

// AXIS-1 COST SWEEP — the deliverable number (handoff §4.3 / §7).
//
// POC §2 measured the Stage-1 Life profile: ~87% inside compute.Evaluate, of
// which ~54% is per-node cbor.Unmarshal, plus LoadScope at 67% of CPU in the
// F-D2 (table/24x24) case. Boundary hashing and the actual arithmetic are noise.
// So the question this file answers is §7 Q1: HOW MUCH OF THAT ~75% DOES ONE
// RUNG REMOVE — which sizes the distance to Doom and decides whether per-program
// compilation (Axis 2) is worth opening next.
//
// PERF DISCIPLINE (AGENTS.md): numbers are ONLY honest without -race.
// modernc.org/sqlite under -race is ~17x slower; these use an in-memory store,
// but the rule stands and the default sweep runs with -race, so:
//
//	make go ARGS="test ./entitysdk -run XXXNONE -bench BenchmarkAxis1 -benchtime 20x -count=1"
//
// Add -cpuprofile to re-take the profile split against POC §2's method.
//
// WHAT IS AND IS NOT COMPARABLE TO POC §2
//
// POC §2's gens/s are WHOLE descriptor-faithful ticks through the executor
// (system/compute:eval → dispatch → handler → Evaluate → decode → put). These
// benchmarks evaluate the step DIRECTLY on each engine. That is deliberate — the
// protocol path is hardwired to Stage-1, so an engine comparison cannot go
// through it — but it means the absolute numbers here are eval-only and are NOT
// the same quantity as POC §2's gens/s. The Stage-1 arm is measured the same way
// as the Axis-1 arm, so the RATIO is the honest deliverable; the absolute
// figures are not directly comparable to the POC table.

import (
	"fmt"
	"testing"

	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/ext/compute"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/entitysdk/axis1"
)

// costRig holds one built Life step ready to evaluate on either engine.
type costRig struct {
	ap   *entitysdk.AppPeer
	ctx  *compute.EvalContext
	step entity.Entity
}

func newCostRig(b testing.TB, w, h int, build func(*entitysdk.AppPeer, int, int, string) *entitysdk.Builder) *costRig {
	b.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		b.Fatal(err)
	}
	p := lifeSetup(b, ap, fmt.Sprintf("app/life/cost-%dx%d", w, h), w, h, lifeRandCells(w, h, 42), build)
	hs, ok := ap.TestLocationIndex().Get(p.statePath)
	_ = hs
	_ = ok
	sh, ok := ap.TestLocationIndex().Get(p.stepPath)
	if !ok {
		b.Fatalf("step not indexed at %s", p.stepPath)
	}
	ent, ok := ap.TestContentStore().Get(sh)
	if !ok {
		b.Fatalf("step entity missing")
	}
	return &costRig{ap: ap, ctx: ap.TestEvalContext(), step: ent}
}

func (r *costRig) budget() *compute.Budget {
	return compute.NewBudget(compute.DefaultMaxOps, compute.DefaultMaxDepth)
}

// BenchmarkAxis1LifeEval is the three-arm comparison.
//
//	stage1        — the reference: decode per node visit, CaptureScope/LoadScope per element.
//	axis1-nocache — resolved form + live frames, but re-decoded every eval.
//	axis1         — the real rung: decode once ever, live frames.
//
// stage1 → axis1-nocache isolates what the resolved form + live frames buy
// WITHIN one eval; axis1-nocache → axis1 isolates what the §13.7 cache buys
// ACROSS ticks. See Engine.noCache for why that split is approximate rather than
// exact (uncached still decodes once per eval, not once per visit).
func BenchmarkAxis1LifeEval(b *testing.B) {
	sizes := []int{8, 16, 24}
	lowerings := map[string]func(*entitysdk.AppPeer, int, int, string) *entitysdk.Builder{
		"arith": buildLifeStepArith,
		"table": buildLifeStepTable,
	}

	for _, n := range sizes {
		for lname, build := range lowerings {
			rig := newCostRig(b, n, n, build)

			b.Run(fmt.Sprintf("stage1/%s/%dx%d", lname, n, n), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if _, err := compute.Evaluate(rig.step, compute.NewScope(), rig.budget(), rig.ctx); err != nil {
						b.Fatalf("stage1 eval: %v", err)
					}
				}
			})

			b.Run(fmt.Sprintf("axis1-nocache/%s/%dx%d", lname, n, n), func(b *testing.B) {
				eng := axis1.NewEngineNoCache()
				for i := 0; i < b.N; i++ {
					if _, err := eng.Evaluate(rig.step, nil, rig.budget(), rig.ctx); err != nil {
						b.Fatalf("axis1-nocache eval: %v", err)
					}
				}
				assertNoFallback(b, eng)
			})

			b.Run(fmt.Sprintf("axis1/%s/%dx%d", lname, n, n), func(b *testing.B) {
				eng := axis1.NewEngine()
				// Warm the decode cache outside the timed loop: the rung's claim
				// is about steady-state ticks, and the first decode is a
				// one-time cost amortized over every tick and every instance
				// sharing the step hash.
				if _, err := eng.Evaluate(rig.step, nil, rig.budget(), rig.ctx); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := eng.Evaluate(rig.step, nil, rig.budget(), rig.ctx); err != nil {
						b.Fatalf("axis1 eval: %v", err)
					}
				}
				b.StopTimer()
				assertNoFallback(b, eng)
			})
		}
	}
}

// assertNoFallback guards the measurement itself: a fallback would route the
// work to Stage-1, and the benchmark would report Stage-1's time under Axis-1's
// name — a number that looks like a result and is an artifact.
func assertNoFallback(b *testing.B, eng *axis1.Engine) {
	if st := eng.Stats(); st.Fallbacks != 0 {
		b.Fatalf("%d Stage-1 fallbacks fired — this measurement is Stage-1 "+
			"wearing Axis-1's name, not a result", st.Fallbacks)
	}
}

// TestAxis1BudgetCliff answers §7 Q2: does the budget cliff move?
//
// POC §2 measured the Stage-1 cliff at DefaultMaxOps=100k: arith fits <=24x24,
// table <=32x32, both budget_exhausted above. Axis-1 keeps the op accounting
// identical on purpose (§13.5), so the cliff should move only if FEWER
// MATERIALIZED ARTIFACTS are produced per eval — never because metering drifted.
// This maps it empirically for both engines so the report can say which.
//
// Not a benchmark: it asserts nothing about time, only about where each engine
// stops being able to tick at all.
func TestAxis1BudgetCliff(t *testing.T) {
	if testing.Short() {
		t.Skip("budget cliff sweep is slow")
	}
	type row struct {
		lowering string
		size     int
		s1, a1   string
	}
	var rows []row

	for _, lname := range []string{"arith", "table"} {
		build := buildLifeStepArith
		if lname == "table" {
			build = buildLifeStepTable
		}
		for _, n := range []int{8, 16, 24, 32} {
			rig := newCostRig(t, n, n, build)

			s1 := "ok"
			if _, err := compute.Evaluate(rig.step, compute.NewScope(), rig.budget(), rig.ctx); err != nil {
				s1 = codeOf(err)
			}
			a1 := "ok"
			eng := axis1.NewEngine()
			if _, err := eng.Evaluate(rig.step, nil, rig.budget(), rig.ctx); err != nil {
				a1 = codeOf(err)
			}
			rows = append(rows, row{lname, n, s1, a1})
		}
	}

	t.Log("budget cliff at DefaultMaxOps=100k (lowering/size: stage1 -> axis1):")
	moved := 0
	for _, r := range rows {
		marker := ""
		if r.s1 != r.a1 {
			marker = "   <-- CLIFF MOVED"
			moved++
		}
		t.Logf("  %-5s %2dx%-2d  %-18s %-18s%s", r.lowering, r.size, r.size, r.s1, r.a1, marker)
	}
	t.Logf("cliff differences: %d", moved)
}

func codeOf(err error) string {
	if ce, ok := err.(*compute.ComputeError); ok {
		return ce.Code
	}
	return err.Error()
}
