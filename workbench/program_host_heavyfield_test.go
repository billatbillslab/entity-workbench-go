package workbench

// TIER: integration (TESTING-STRATEGY) — real peer, store, evaluator.
//
// THE COMPUTE-BOUND EXPERIMENT (arch §5.3 first bullet). The 64×64 Life probe
// measured a flat ~1.8× parallel speedup and blamed the fixed per-shard grid
// read (Life is op-cheap). This isolates the variable: the heavy-field program
// (program_heavyfield.go) has a tunable per-cell op cost (`iters`), same sharded
// host path otherwise. Two things to pin:
//
//   - CORRECTNESS: the sharded result matches an independent Go kernel oracle
//     (heavyFieldNextGen), and k=1 (one shard = unsharded-equivalent) exhausts the
//     budget while k>1 clears it — so we know sharding is actually load-bearing,
//     not decorative.
//   - THE SCALING LAW: fix grid + k, sweep ops/cell, measure serial vs parallel.
//     Prediction: as ops/cell rises the tick becomes compute-bound and the
//     parallel speedup climbs toward k×, which would prove Life's 1.8× was the
//     store reads, not a ceiling on entity-compute parallelism.

import (
	"fmt"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"

	"entity-workbench-go/entitysdk"
)

// fieldCellsAt reads the live field grid's cells (row-major). The field grid
// shares Life's {width,height,cells} shape, so lifeWireState decodes it.
func fieldCellsAt(t *testing.T, h *Host) []uint64 {
	t.Helper()
	ent, ok, err := h.ap.Get(h.desc.StatePath)
	if err != nil || !ok {
		t.Fatalf("read field state: ok=%v err=%v", ok, err)
	}
	var s lifeWireState
	if err := ecf.Decode(ent.Data, &s); err != nil {
		t.Fatalf("decode field state: %v", err)
	}
	return s.Cells
}

// mountHeavyField authors + mounts a heavy field, returning the host. Fails the
// test on author/mount error (a fault at tick time is the caller's to check).
func mountHeavyField(t testing.TB, w, h, k, iters int, seed uint64, parallel bool) *Host {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	root := fmt.Sprintf("app/field-%dx%d-k%d-i%d", w, h, k, iters)
	descPath, err := AuthorHeavyFieldSharded(ap, root, seed, w, h, k, iters)
	if err != nil {
		t.Fatalf("AuthorHeavyFieldSharded: %v", err)
	}
	host, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(host.Close)
	host.parallelShards = parallel
	return host
}

// TestHeavyField_MatchesOracleAndNeedsSharding pins correctness AND that sharding
// is load-bearing: at 16×16 with a heavy kernel, k=1 (one shard over the whole
// grid = unsharded-equivalent) busts the budget, while k=4 ticks and matches the
// Go oracle generation by generation, with the field actually evolving.
func TestHeavyField_MatchesOracleAndNeedsSharding(t *testing.T) {
	const seed = uint64(12345)
	const w, h, iters = 16, 16, 120
	const gens = 3

	// k=1 must exhaust the budget — else the heavy kernel is not heavy enough for
	// this to be a sharding test at all.
	one := mountHeavyField(t, w, h, 1, iters, seed, true)
	if one.tickOnce() {
		t.Fatalf("k=1 (unsharded-equivalent) ticked at %dx%d iters=%d — kernel not past the "+
			"budget cliff, so sharding is not load-bearing here", w, h, iters)
	}

	// k=4 must tick and match the oracle.
	host := mountHeavyField(t, w, h, 4, iters, seed, true)
	cur := fieldSeedState(w, h, seed)
	distinct := map[uint64]bool{}
	for g := 1; g <= gens; g++ {
		if !host.tickOnce() {
			t.Fatalf("k=4 faulted at gen %d: %s", g, host.Render().Err)
		}
		want := heavyFieldNextGen(cur, iters)
		got := fieldCellsAt(t, host)
		if len(got) != len(want) {
			t.Fatalf("gen %d: %d cells, want %d", g, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("gen %d cell %d: sharded %d != Go kernel oracle %d — the sharded heavy "+
					"field diverged from its reference", g, i, got[i], want[i])
			}
		}
		distinct[got[0]] = true
		cur = want
	}
	// Anti-vacuity: the field must actually be changing, or oracle agreement on a
	// fixed point proves nothing.
	if len(distinct) < 2 {
		t.Fatalf("VACUOUS: field not evolving across %d gens (cell0 constant)", gens)
	}
}

// TestHeavyField_ParallelEqualsSerial pins determinism for the compute-bound
// program: parallel and serial shard evaluation produce identical cells, so the
// speedup numbers below compare two runs that compute the same thing.
func TestHeavyField_ParallelEqualsSerial(t *testing.T) {
	const seed = uint64(777)
	const w, h, k, iters = 32, 32, 8, 40

	par := mountHeavyField(t, w, h, k, iters, seed, true)
	ser := mountHeavyField(t, w, h, k, iters, seed, false)
	for g := 1; g <= 3; g++ {
		if !par.tickOnce() {
			t.Fatalf("parallel faulted gen %d: %s", g, par.Render().Err)
		}
		if !ser.tickOnce() {
			t.Fatalf("serial faulted gen %d: %s", g, ser.Render().Err)
		}
		hp := stateHashAt(t, par.ap, par.desc.StatePath)
		hs := stateHashAt(t, ser.ap, ser.desc.StatePath)
		if hp != hs {
			t.Fatalf("gen %d: heavy-field parallel hash %s != serial %s — nondeterministic", g, hp, hs)
		}
	}
}

// BenchmarkHeavyFieldScaling is the experiment. Fix 32×32 / k=8; sweep ops/cell
// (iters); measure serial vs parallel ms/tick. Run WITHOUT -race:
//
//	make go ARGS="test ./workbench -run XXXNONE -bench BenchmarkHeavyFieldScaling -benchtime 20x -count=1"
//
// Read the ratio serial/parallel per iters: if it rises with iters, the parallel
// region is growing against the fixed serial (read + stitch) cost — the
// compute-bound regime Life never reached.
func BenchmarkHeavyFieldScaling(b *testing.B) {
	const w, h, k = 32, 32, 8
	seed := uint64(12345)
	for _, iters := range []int{10, 30, 60} {
		for _, parallel := range []bool{false, true} {
			mode := "serial"
			if parallel {
				mode = "parallel"
			}
			b.Run(fmt.Sprintf("iters%d/%s", iters, mode), func(b *testing.B) {
				host := mountHeavyField(b, w, h, k, iters, seed, parallel)
				if !host.tickOnce() {
					b.Fatalf("warm tick faulted (iters=%d): %s", iters, host.Render().Err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if !host.tickOnce() {
						b.Fatalf("tick faulted: %s", host.Render().Err)
					}
				}
			})
		}
	}
}
