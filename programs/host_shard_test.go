package programs

// TIER: integration (TESTING-STRATEGY) — a real AppPeer, real store, real
// compute evaluator. Naming the tier is the discipline.
//
// These tests carry arch's next-build item 1 gate (HANDOFF-2026-07-18-…-rulings
// §5.1): the host-managed static-k sharding floor, lifted out of
// entitysdk/axis1_shard_test.go and into a program the generic host mounts. The
// gate arch named is "parallel state hash == serial == unsharded, every
// generation." Two tests carry it:
//
//   - TestMountShard_EqualsUnsharded — at 16×16 (a size the unsharded step also
//     fits), the sharded mount's state hash equals the unsharded mount's, tick
//     for tick, for BOTH parallel and serial shard evaluation. That is the whole
//     "parallel == serial == unsharded" bar, at the mounted-host layer.
//   - TestMountShard_ClearsBudgetCliff — at 32×32 (past the arith cliff), the
//     unsharded step returns budget_exhausted and cannot tick, while the sharded
//     host ticks and the grid evolves. That is the point of sharding: a program
//     that cannot mount unsharded, mounting through the floor.
//
// ANTI-VACUITY runs through both: a Life that froze or went extinct would make
// hash equality free (this track has been burned by exactly that green), so each
// asserts the grid actually evolves before it trusts any equality.

import (
	"context"
	"testing"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// tickHashes mounts the descriptor at descPath, ticks it for n generations, and
// returns the state-entity content hash after each tick. parallel selects the
// shard-eval mode (ignored for an unsharded program).
func tickHashes(t *testing.T, ap *entitysdk.AppPeer, descPath string, n int, parallel bool) []hash.Hash {
	t.Helper()
	h, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(h.Close)
	h.parallelShards = parallel

	hashes := make([]hash.Hash, 0, n)
	for i := 0; i < n; i++ {
		if !h.tickOnce() {
			t.Fatalf("mounted program faulted at tick %d: %s", i, h.Render().Err)
		}
		hashes = append(hashes, stateHashAt(t, ap, h.Descriptor().StatePath))
	}
	return hashes
}

// assertEvolving is the anti-vacuity guard: a sharded/unsharded equality proves
// nothing if the grid is a fixed point. Require the run to visit enough distinct
// states that "identical" is a real measurement.
func assertEvolving(t *testing.T, hashes []hash.Hash) {
	t.Helper()
	distinct := map[string]bool{}
	for _, hh := range hashes {
		distinct[hh.String()] = true
	}
	if len(distinct) < len(hashes)/2 {
		t.Fatalf("VACUOUS: only %d distinct states across %d ticks — the grid is not evolving, "+
			"so hash equality proves nothing", len(distinct), len(hashes))
	}
}

// TestMountShard_EqualsUnsharded is the core gate: the sharded floor is the SAME
// program as the unsharded one, wired differently. Same seed, same size, ticked
// in lockstep — the state hashes must match every generation, for both parallel
// and serial shard evaluation. If a byte diverged, sharding would be a different
// program wearing the same name (and the whole floor would be void).
func TestMountShard_EqualsUnsharded(t *testing.T) {
	const seed = uint64(12345)
	const ticks = 8
	const w, h, k = lifeWidth, lifeHeight, 4 // 16×16 — a size the unsharded step fits

	// Unsharded reference (AuthorLife is 16×16, same seed function).
	apU := newTestPeer(t)
	descU, err := AuthorLife(apU, LifeRoot, seed)
	if err != nil {
		t.Fatalf("AuthorLife: %v", err)
	}
	want := tickHashes(t, apU, descU, ticks, false)
	assertEvolving(t, want)

	// Sharded, parallel and serial — each must match the unsharded reference.
	for _, parallel := range []bool{true, false} {
		mode := "serial"
		if parallel {
			mode = "parallel"
		}
		apS := newTestPeer(t)
		descS, err := AuthorLifeSharded(apS, "app/life-shard", seed, w, h, k)
		if err != nil {
			t.Fatalf("%s: AuthorLifeSharded: %v", mode, err)
		}
		got := tickHashes(t, apS, descS, ticks, parallel)
		for i := range want {
			if want[i] != got[i] {
				t.Fatalf("%s gen %d: sharded state hash %s != unsharded %s — the shard floor "+
					"changed WHAT the program computes, not just how it is wired",
					mode, i, got[i], want[i])
			}
		}
		t.Logf("%s: sharded == unsharded at the boundary, k=%d, %d generations", mode, k, ticks)
	}
}

// TestMountShard_ClearsBudgetCliff is the point of the whole floor: a grid that
// CANNOT tick unsharded (budget_exhausted at 32×32/arith) ticks correctly when
// sharded — through the generic host, not a test rig.
func TestMountShard_ClearsBudgetCliff(t *testing.T) {
	const seed = uint64(12345)
	const ticks = 4
	const w, h, k = lifeShardWidth, lifeShardHeight, lifeShardK // 32×32, k=4

	// First establish the cliff is real at this size — otherwise the sharded
	// success below proves nothing. Build the unsharded 32×32 step and eval it.
	apC := newTestPeer(t)
	statePath := "app/life-cliff/state"
	stepPath := "app/life-cliff/step"
	s0 := lifeSeedState(w, h, seed)
	ent, err := lifeStateEntity(s0)
	if err != nil {
		t.Fatalf("cliff state₀: %v", err)
	}
	if _, err := apC.PutEntity(statePath, ent); err != nil {
		t.Fatalf("cliff put state₀: %v", err)
	}
	if _, err := buildLifeStepExpr(apC, w, h, statePath).Build(context.Background(), stepPath); err != nil {
		t.Fatalf("cliff build step: %v", err)
	}
	if ticked := unshardedTicks(t, apC, stepPath); ticked {
		t.Fatalf("32×32 arith ticked unsharded — the budget cliff this floor exists to clear "+
			"is not there at %dx%d, so the sharded success proves nothing", w, h)
	}

	// Now mount and tick the SHARDED 32×32 program through the host. It must tick,
	// and the grid must evolve (anti-vacuity: a frozen board would tick trivially).
	apS := newTestPeer(t)
	descS, err := AuthorLifeSharded(apS, "app/life-shard-32", seed, w, h, k)
	if err != nil {
		t.Fatalf("AuthorLifeSharded: %v", err)
	}
	got := tickHashes(t, apS, descS, ticks, true)
	assertEvolving(t, got)
	t.Logf("32×32 arith ticks correctly SHARDED through the host at k=%d, %d generations "+
		"(unsharded: budget_exhausted)", k, ticks)
}

// unshardedTicks evals the expression at stepPath once and reports whether it
// produced a value (true) or failed the budget/eval (false). Mirrors
// axis1_shard_test's cliff check at the mounted-host layer's evaluator.
func unshardedTicks(t *testing.T, ap *entitysdk.AppPeer, stepPath string) bool {
	t.Helper()
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{stepPath}})
	if err != nil {
		return false
	}
	if resp.Status != 200 || resp.Type == types.TypeComputeError {
		return false
	}
	return true
}

// TestMountShard_RefusesUndrivenModes pins host admission: the floor drives
// static-k / host-managed / compute-gather only, and a descriptor declaring the
// range form or the continuation-managed model is refused at Mount with the
// reason — never a half-mount (the item-2 and dynamic-k boundaries, enforced).
func TestMountShard_RefusesUndrivenModes(t *testing.T) {
	load := func(t *testing.T, mutate func(*ProgramShard)) error {
		ap := newTestPeer(t)
		descPath, err := AuthorLifeSharded(ap, "app/life-shard-x", 12345, lifeWidth, lifeHeight, 4)
		if err != nil {
			t.Fatalf("AuthorLifeSharded: %v", err)
		}
		entv, ok, err := ap.Get(descPath)
		if err != nil || !ok {
			t.Fatalf("read descriptor: ok=%v err=%v", ok, err)
		}
		d, err := DecodeDescriptor(entv)
		if err != nil {
			t.Fatalf("DecodeDescriptor: %v", err)
		}
		mutate(d.Shard)
		bad, err := d.Entity()
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if _, err := ap.PutEntity(descPath, bad); err != nil {
			t.Fatalf("put: %v", err)
		}
		_, err = Mount(ap, descPath)
		return err
	}

	cases := []struct {
		name   string
		mutate func(*ProgramShard)
	}{
		{"range form", func(s *ProgramShard) { s.Form = ShardRange }},
		{"continuation-managed", func(s *ProgramShard) { s.Orchestration = OrchestrationContinuationManaged }},
		{"host stitch owner", func(s *ProgramShard) { s.StitchOwner = StitchOwnerHost }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := load(t, tc.mutate); err == nil {
				t.Fatalf("Mount accepted an undriven shard mode (%s) — admission is not enforced, "+
					"so the host would half-drive a mode it cannot", tc.name)
			}
		})
	}
}
