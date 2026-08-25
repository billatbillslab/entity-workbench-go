package programs

// TIER: integration (TESTING-STRATEGY) — real peer, store, evaluator.
//
// PUSHING THE LIMIT — how far does the host-managed sharding floor actually
// reach, and how does k scale with the grid? The arith single-eval budget cliff
// is ~24×24 (24×24 fits, 32×32 exhausts). This probe takes Life well past it —
// 32×32, 48×48, 64×64 — through the generic host, and pins two things:
//
//   - CORRECTNESS at sizes with no unsharded compute reference. 64×64 cannot tick
//     unsharded at all, so there is nothing to hash-compare against — we check the
//     sharded grid against an independent Go Life oracle (lifeNextGen) instead.
//     That is the anti-vacuity licence at scale: agreement with a second,
//     differently-written implementation, not with ourselves.
//   - THE FEEL: the smallest k that ticks at each size, and cells/shard. This is
//     the number arch's descriptor eventually needs (op_cost → k). It should scale
//     with cells/shard staying under the budget headroom, not with anything exotic.
//
// This is arch's §5.3 first bullet at a first scale: measure how sharding behaves
// as the map gets wide. A full compute-intensive wide-map program is the next
// step; 64×64 Life is the cheap probe that gets the shape.

import (
	"context"
	"fmt"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"

	"entity-workbench-go/entitysdk"
)

// lifeNextGen is an independent Go oracle for one Life generation on a torus
// (B3/S23), row-major i = y*w + x. Deliberately NOT the compute lowering — it is
// the second implementation the sharded result must agree with, so agreement is a
// real cross-check rather than a tautology.
func lifeNextGen(w, h int, cells []uint64) []uint64 {
	next := make([]uint64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			count := uint64(0)
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					nx := (x + dx + w) % w
					ny := (y + dy + h) % h
					count += cells[ny*w+nx]
				}
			}
			alive := cells[y*w+x] == 1
			if count == 3 || (count == 2 && alive) {
				next[y*w+x] = 1
			}
		}
	}
	return next
}

// unshardedFitsAt reports whether an unsharded arith Life step of size w×h ticks
// under the single-eval budget (true) or exhausts it (false) — the cliff probe.
func unshardedFitsAt(t *testing.T, w, h int, seed uint64) bool {
	t.Helper()
	ap := newTestPeer(t)
	statePath := fmt.Sprintf("app/life-fit-%dx%d/state", w, h)
	stepPath := fmt.Sprintf("app/life-fit-%dx%d/step", w, h)
	ent, err := lifeStateEntity(lifeSeedState(w, h, seed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.PutEntity(statePath, ent); err != nil {
		t.Fatal(err)
	}
	if _, err := buildLifeStepExpr(ap, w, h, statePath).Build(context.Background(), stepPath); err != nil {
		t.Fatal(err)
	}
	return unshardedTicks(t, ap, stepPath)
}

// mountShardedAt authors a sharded Life at (w,h,k), mounts it, and returns the
// host (parallel eval) — or an error path via t.Fatal on author/mount failure.
func mountShardedAt(t *testing.T, w, h, k int, seed uint64, parallel bool) *Host {
	t.Helper()
	ap := newTestPeer(t)
	root := fmt.Sprintf("app/life-scale-%dx%d-k%d", w, h, k)
	descPath, err := AuthorLifeSharded(ap, root, seed, w, h, k)
	if err != nil {
		t.Fatalf("AuthorLifeSharded %dx%d k=%d: %v", w, h, k, err)
	}
	host, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount %dx%d k=%d: %v", w, h, k, err)
	}
	t.Cleanup(host.Close)
	host.parallelShards = parallel
	return host
}

// gridCellsAt reads the live state grid's cells (row-major).
func gridCellsAt(t *testing.T, h *Host) []uint64 {
	t.Helper()
	ent, ok, err := h.ap.Get(h.desc.StatePath)
	if err != nil || !ok {
		t.Fatalf("read state: ok=%v err=%v", ok, err)
	}
	var s lifeWireState
	if err := ecf.Decode(ent.Data, &s); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return s.Cells
}

// TestMountShard_ScalesTo64 is the headline: Life at 32/48/64 — each PAST the
// unsharded cliff — ticks correctly through the generic host, checked against the
// independent Go oracle, and the smallest k that ticks is reported so we can see
// how k scales. 64×64 is 4096 cells: ~16× the 24×24 the single eval tops out at.
func TestMountShard_ScalesTo64(t *testing.T) {
	const seed = uint64(12345)
	const gens = 3
	kLadder := []int{2, 4, 8, 16, 32}

	t.Log("size     unsharded   smallest-k   cells/shard   (host-managed, arith)")
	for _, n := range []int{32, 48, 64} {
		w, h := n, n

		// The cliff must be real at this size, or "sharded works" proves nothing.
		if unshardedFitsAt(t, w, h, seed) {
			t.Fatalf("%dx%d ticks UNSHARDED — not past the cliff, so the sharded run is not a scale proof", w, h)
		}

		// Smallest k on the ladder that mounts and ticks one generation.
		foundK := 0
		for _, k := range kLadder {
			host := mountShardedAt(t, w, h, k, seed, true)
			if host.tickOnce() {
				foundK = k
				break
			}
			// A fault here means k shards still bust the budget — try a bigger k.
		}
		if foundK == 0 {
			t.Fatalf("%dx%d did not tick at any k in %v — sharding did not reach this size", w, h, kLadder)
		}
		t.Logf("%2dx%-2d   exhausted   k=%-8d   %d", w, h, foundK, w*h/foundK)

		// Correctness at foundK: gens generations, each matching the Go oracle,
		// with the grid actually evolving (anti-vacuity).
		host := mountShardedAt(t, w, h, foundK, seed, true)
		cur := lifeSeedState(w, h, seed).Cells
		prevPop := lifePopulation(cur)
		evolved := false
		for g := 1; g <= gens; g++ {
			if !host.tickOnce() {
				t.Fatalf("%dx%d k=%d: faulted at gen %d: %s", w, h, foundK, g, host.Render().Err)
			}
			want := lifeNextGen(w, h, cur)
			got := gridCellsAt(t, host)
			if !lifeCellsEqual(got, want) {
				t.Fatalf("%dx%d k=%d gen %d: sharded grid diverged from the Go Life oracle", w, h, foundK, g)
			}
			if pop := lifePopulation(got); pop == 0 {
				t.Fatalf("%dx%d k=%d gen %d: grid extinct — oracle agreement would be vacuous", w, h, foundK, g)
			} else if pop != prevPop {
				evolved = true
			}
			cur = want
			prevPop = lifePopulation(cur)
		}
		if !evolved {
			t.Fatalf("%dx%d k=%d: population never changed across %d gens — agreement is vacuous", w, h, foundK, gens)
		}
	}
}

// TestMountShard_ParallelEqualsSerialAt64 pins determinism at scale: at 64×64,
// parallel and serial shard evaluation produce the identical state hash, every
// generation. Concurrency that changed the bytes — or changed them run to run —
// would trade away the replay/lockstep the whole POC exists to guarantee.
func TestMountShard_ParallelEqualsSerialAt64(t *testing.T) {
	const seed = uint64(12345)
	const w, h, k = 64, 64, 8 // k=8 keeps each shard's 512 cells under the budget
	const gens = 3

	par := mountShardedAt(t, w, h, k, seed, true)
	ser := mountShardedAt(t, w, h, k, seed, false)
	for g := 1; g <= gens; g++ {
		if !par.tickOnce() {
			t.Fatalf("parallel faulted at gen %d: %s", g, par.Render().Err)
		}
		if !ser.tickOnce() {
			t.Fatalf("serial faulted at gen %d: %s", g, ser.Render().Err)
		}
		hp := stateHashAt(t, par.ap, par.desc.StatePath)
		hs := stateHashAt(t, ser.ap, ser.desc.StatePath)
		if hp != hs {
			t.Fatalf("64×64 gen %d: parallel hash %s != serial %s — concurrent shard "+
				"evaluation is not deterministic at scale", g, hp, hs)
		}
	}
	// Anti-vacuity: a frozen board makes hash equality free.
	cells := gridCellsAt(t, par)
	if lifePopulation(cells) == 0 {
		t.Fatal("VACUOUS: 64×64 grid extinct; the parallel==serial hash proves nothing")
	}
	t.Logf("64×64: parallel == serial at the boundary, k=%d, %d generations", k, gens)
}

// BenchmarkMountShardScale gets a rough feel for the cost as the grid widens.
// Run WITHOUT -race (sqlite is ~17× slower under it — AGENTS perf note):
//
//	make go ARGS="test ./workbench -run XXXNONE -bench BenchmarkMountShardScale -benchtime 10x -count=1"
func BenchmarkMountShardScale(b *testing.B) {
	seed := uint64(12345)
	cases := []struct{ w, h, k int }{
		{32, 32, 4}, {48, 48, 4}, {64, 64, 8},
	}
	for _, tc := range cases {
		for _, parallel := range []bool{false, true} {
			mode := "serial"
			if parallel {
				mode = "parallel"
			}
			b.Run(fmt.Sprintf("%dx%d/k%d/%s", tc.w, tc.h, tc.k, mode), func(b *testing.B) {
				ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = ap.Close() }()
				root := fmt.Sprintf("app/life-bench-%dx%d-%s", tc.w, tc.h, mode)
				descPath, err := AuthorLifeSharded(ap, root, seed, tc.w, tc.h, tc.k)
				if err != nil {
					b.Fatal(err)
				}
				host, err := Mount(ap, descPath)
				if err != nil {
					b.Fatal(err)
				}
				defer host.Close()
				host.parallelShards = parallel
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
}
