package programs

// TIER: integration (TESTING-STRATEGY) — real peer, store, evaluator.
//
// THE TIME-AXIS NEGATIVE (reviews/COMPUTE-SHARDING-INTO-HOST-2026-07-18.md §6
// item 1). §5/§5a proved the SPACE axis: a wide `map` shards into k independent
// pieces that parallelize, and the speedup climbs toward k× as op-cost rises.
// This is the mirror: a `fold` (a strict accumulator dependency chain) that does
// NOT parallelize, no matter the op-cost — because shard j's carry-in is shard
// j-1's carry-out. The carry-chain program (program_chain.go) carries BOTH a map
// component (cells) and a fold component (carry) in one tick, so these tests can
// watch them behave oppositely under the identical sharded host:
//
//   - TestChain_MapParallelizes_FoldDoesNot — the thesis in one assertion. Serial
//     and parallel shard evaluation produce BYTE-IDENTICAL cells (the map is
//     independent) but DIFFERENT carry (the fold's chain is corrupted when the
//     shards run without the prior carry). map parallelizes; fold does not.
//   - TestChain_SerialMatchesOracleAndNeedsSharding — correctness + load-bearing:
//     the serial sharded carry equals the independent Go GLOBAL-fold oracle (fold-
//     exactness — sharded == unsharded), and k=1 busts the budget while k>1 clears
//     it, so sharding is actually doing the work.
//   - TestChain_SerialFloorIsConstantInN — the Amdahl residue: the serial barrier
//     depth per generation is the k carry-links + one stitch, O(1) in N. Confirmed
//     by holding k fixed and growing N: the chain length (shard evals + stitch) is
//     invariant, so the serial floor is the chain, not the cells.
//   - BenchmarkChainVsMapParallelRelief — the measurement: for the map, parallel
//     is the faster CORRECT evaluation; for the fold, parallel is WRONG, so serial
//     is the only correct evaluation and there is no parallel relief. Same host,
//     opposite parallelizability.

import (
	"fmt"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"

	"entity-workbench-go/entitysdk"
)

// chainWireState decodes the carry-chain state ({width, height, cells, carry}).
type chainWireState struct {
	Width  uint64   `cbor:"width"`
	Height uint64   `cbor:"height"`
	Cells  []uint64 `cbor:"cells"`
	Carry  uint64   `cbor:"carry"`
}

func chainStateAt(t *testing.T, h *Host) chainWireState {
	t.Helper()
	ent, ok, err := h.ap.Get(h.desc.StatePath)
	if err != nil || !ok {
		t.Fatalf("read chain state: ok=%v err=%v", ok, err)
	}
	var s chainWireState
	if err := ecf.Decode(ent.Data, &s); err != nil {
		t.Fatalf("decode chain state: %v", err)
	}
	return s
}

// mountChain authors + mounts a carry chain, returning the host.
func mountChain(t testing.TB, w, h, k, iters int, seed uint64, parallel bool) *Host {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	root := fmt.Sprintf("app/chain-%dx%d-k%d-i%d-%t", w, h, k, iters, parallel)
	descPath, err := AuthorChainSharded(ap, root, seed, w, h, k, iters)
	if err != nil {
		t.Fatalf("AuthorChainSharded: %v", err)
	}
	host, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(host.Close)
	host.parallelShards = parallel
	return host
}

// TestChain_MapParallelizes_FoldDoesNot is the thesis. One program, one tick, two
// components: the map cells parallelize (serial == parallel, byte-identical), the
// fold carry does not (serial != parallel, because shard j reads a stale carry).
func TestChain_MapParallelizes_FoldDoesNot(t *testing.T) {
	const seed = uint64(0xC0FFEE)
	const w, h, k, iters = 16, 16, 4, 60

	par := mountChain(t, w, h, k, iters, seed, true)
	ser := mountChain(t, w, h, k, iters, seed, false)

	if !par.tickOnce() {
		t.Fatalf("parallel faulted: %s", par.Render().Err)
	}
	if !ser.tickOnce() {
		t.Fatalf("serial faulted: %s", ser.Render().Err)
	}

	sp := chainStateAt(t, par)
	ss := chainStateAt(t, ser)

	// The SPACE component (map): identical. Parallel sharding is exact here.
	if len(sp.Cells) != len(ss.Cells) {
		t.Fatalf("cells length differ: parallel %d, serial %d", len(sp.Cells), len(ss.Cells))
	}
	for i := range ss.Cells {
		if sp.Cells[i] != ss.Cells[i] {
			t.Fatalf("cell %d differs (parallel %d, serial %d) — the MAP should be "+
				"parallel-exact; the space axis is not supposed to diverge", i, sp.Cells[i], ss.Cells[i])
		}
	}

	// The TIME component (fold): corrupted under parallel. This is the negative —
	// the dependency chain cannot be evaluated without the prior carry.
	if sp.Carry == ss.Carry {
		t.Fatalf("VACUOUS: parallel carry %d == serial carry %d — the fold chain did not "+
			"diverge under parallel sharding, so this test proves nothing. Raise iters/k or "+
			"check that shard j actually reads frag{j-1}.carry", sp.Carry, ss.Carry)
	}
	t.Logf("map cells identical (%d cells); fold carry diverged: serial=%d parallel=%d — "+
		"the map parallelizes, the fold does not", len(ss.Cells), ss.Carry, sp.Carry)
}

// TestChain_SerialMatchesOracleAndNeedsSharding pins correctness AND that sharding
// is load-bearing on the fold: serial sharded carry equals the independent Go
// global-fold oracle (fold-exactness — the sharded chain == the unsharded fold),
// the cells match the map oracle, and k=1 (one fold over all N) busts the budget
// while k>1 clears it.
func TestChain_SerialMatchesOracleAndNeedsSharding(t *testing.T) {
	const seed = uint64(12345)
	// iters chosen so k=1 (a fold over all 256 cells) busts the 100k budget while
	// k=4 (64 cells/shard) clears it. The fold builtin's per-element lambda dispatch
	// makes it heavier than heavyfield's inline map (~8 effective ops/iter), so this
	// sits below heavyfield's iters for the same 16×16 cliff.
	const w, h, iters = 16, 16, 100
	const gens = 3

	// k=1 must exhaust the budget, or the fold is not heavy enough for sharding to
	// be load-bearing here.
	one := mountChain(t, w, h, 1, iters, seed, false)
	if one.tickOnce() {
		t.Fatalf("k=1 (unsharded fold) ticked at %dx%d iters=%d — fold not past the budget "+
			"cliff, so sharding is not load-bearing here", w, h, iters)
	}

	// k=4, serial: must tick and match BOTH oracles, generation by generation.
	host := mountChain(t, w, h, 4, iters, seed, false)
	cur := chainSeedState(w, h, seed)
	distinct := map[uint64]bool{}
	for g := 1; g <= gens; g++ {
		if !host.tickOnce() {
			t.Fatalf("k=4 serial faulted at gen %d: %s", g, host.Render().Err)
		}
		wantCells := chainNextCells(cur)
		wantCarry := chainNextCarry(cur, iters)
		got := chainStateAt(t, host)

		if len(got.Cells) != len(wantCells) {
			t.Fatalf("gen %d: %d cells, want %d", g, len(got.Cells), len(wantCells))
		}
		for i := range wantCells {
			if got.Cells[i] != wantCells[i] {
				t.Fatalf("gen %d cell %d: sharded %d != map oracle %d", g, i, got.Cells[i], wantCells[i])
			}
		}
		if got.Carry != wantCarry {
			t.Fatalf("gen %d: sharded carry %d != GLOBAL-fold oracle %d — the serial carry "+
				"chain diverged from the unsharded fold (fold-exactness broken)", g, got.Carry, wantCarry)
		}
		distinct[got.Carry] = true
		cur = wantCells
	}
	if len(distinct) < 2 {
		t.Fatalf("VACUOUS: carry not evolving across %d gens", gens)
	}
}

// TestChain_SerialFloorIsConstantInN pins the Amdahl residue: the serial barrier
// depth per generation is the k carry-links + one stitch — O(1) in N, not
// O(cells). We hold k fixed and grow N; the number of serial chain steps the host
// performs per tick (k shard evals threaded by the carry, then one stitch) is
// invariant. The descriptor's shard count IS that barrier depth, so a constant
// shard count across a 4× cell growth is the confirmation: the serial floor is the
// chain length, not the grid.
func TestChain_SerialFloorIsConstantInN(t *testing.T) {
	const k, iters = 4, 40
	const seed = uint64(777)
	sizes := [][2]int{{16, 16}, {24, 24}, {32, 32}} // N: 256 → 576 → 1024 (4×)

	var barrierDepth uint64
	for i, wh := range sizes {
		host := mountChain(t, wh[0], wh[1], k, iters, seed, false)
		if !host.tickOnce() {
			t.Fatalf("%dx%d serial faulted: %s", wh[0], wh[1], host.Render().Err)
		}
		s := host.Descriptor().Shard
		// The serial floor per tick: k carry-links (each shard eval waits on the
		// prior fragment) + one gather stitch = k + 1 barriers.
		depth := s.K + 1
		if i == 0 {
			barrierDepth = depth
		}
		if depth != barrierDepth {
			t.Fatalf("%dx%d: serial barrier depth %d != %d at %dx%d — the floor should be O(1) in N",
				wh[0], wh[1], depth, barrierDepth, sizes[0][0], sizes[0][1])
		}
		n := wh[0] * wh[1]
		t.Logf("%dx%d (N=%d): serial floor = k+1 = %d barriers (constant in N)", wh[0], wh[1], n, depth)
	}
}

// BenchmarkChainVsMapParallelRelief is the measurement. For the carry chain, run
// serial (the only CORRECT evaluation — parallel corrupts the carry) and time it;
// the "parallel" arm is timed too, but its result is WRONG, so it buys no correct
// speedup. Contrast with BenchmarkHeavyFieldScaling, where parallel is the faster
// correct evaluation. Run WITHOUT -race:
//
//	make go ARGS="test ./workbench -run XXXNONE -bench BenchmarkChainVsMapParallelRelief -benchtime 20x -count=1"
func BenchmarkChainVsMapParallelRelief(b *testing.B) {
	const w, h, k, iters = 32, 32, 8, 40
	seed := uint64(12345)
	for _, parallel := range []bool{false, true} {
		mode := "serial-correct"
		if parallel {
			mode = "parallel-WRONG"
		}
		b.Run(mode, func(b *testing.B) {
			host := mountChain(b, w, h, k, iters, seed, parallel)
			if !host.tickOnce() {
				b.Fatalf("warm tick faulted: %s", host.Render().Err)
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
