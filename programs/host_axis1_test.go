package programs

// TIER: integration (TESTING-STRATEGY) — real peer, store, evaluator.
//
// THE AXIS-1-ENGINE HOST (reviews/COMPUTE-SHARDING-INTO-HOST-2026-07-18.md §6
// item 2). Every sharded wall-time number so far (§5 Life 64×64, §5a the heavy
// field, §7 the chain) was taken with the host evaling through Stage-1
// `system/compute`. The review flagged this as the interpreter floor, not the
// parallel ceiling: the Axis-1 engine (entitysdk/axis1) evaluates side-effect-free
// (no per-closure store writes), so it should (a) lift every absolute wall-time
// number and (b) lift the *parallel* speedup — because shrinking the fixed
// per-shard read/materialize cost shrinks the serial region the speedup fights
// against (Amdahl, §5a).
//
// The host gained NO engine knowledge to make this measurable: `computePattern`
// selects which resource `ExecuteOnResource` targets, and both engines are
// registered on the same peer over a byte-identical protocol path (executor →
// dispatch → handler → eval → materialize → wrap → decode → put). Only the
// evaluator downstream of dispatch differs — the same design axis1_tick_test.go
// used for the unsharded tick, now driving the generic host's sharded tick.
//
//   - TestAxis1Host_ShardedEqualsSystemCompute — the sharded heavy-field state
//     hash is identical on both engines, generation by generation. Without this
//     the benchmark would time an unvalidated path.
//   - BenchmarkAxis1HostShardScaling — §5a re-run on both engines: 32×32 / k=8,
//     sweep engine × {serial, parallel}. Read two things off it: the absolute
//     ms/tick drop (axis1 vs system), and whether parallel/serial rises on axis1
//     (the lifted ceiling).

import (
	"fmt"
	"testing"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/entitysdk/axis1"
)

// mountHeavyFieldOnEngine authors + mounts a heavy field on a peer that carries
// BOTH engines (system/compute by default + the Axis-1 handler at
// ComputeEngineAxis1), then points the host's eval at `enginePattern`. The peer
// always registers axis1 so the two engines are compared over one identical peer
// setup; only h.computePattern varies.
func mountHeavyFieldOnEngine(t testing.TB, w, h, k, iters int, seed uint64, parallel bool, enginePattern string) *Host {
	t.Helper()
	eng := axis1.NewEngine()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		Handlers: []entitysdk.HandlerRegistration{
			{Pattern: ComputeEngineAxis1, Handler: axis1.NewHandler(eng, ComputeEngineAxis1)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	root := fmt.Sprintf("app/field-axis1-%dx%d-k%d-i%d-%t-%s", w, h, k, iters, parallel, sanitizePattern(enginePattern))
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
	host.computePattern = enginePattern
	return host
}

func sanitizePattern(p string) string {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, '-')
		} else {
			out = append(out, p[i])
		}
	}
	return string(out)
}

// TestAxis1Host_ShardedEqualsSystemCompute pins that the generic host's sharded
// tick is byte-identical on the two engines — same state hash every generation.
// This is the licence for the benchmark: a wall-time comparison across engines is
// only meaningful if both engines compute the same thing through the same host.
func TestAxis1Host_ShardedEqualsSystemCompute(t *testing.T) {
	const seed = uint64(0xA715)
	const w, h, k, iters = 32, 32, 8, 20 // past the 32×32 cliff (§5a), so sharding is live
	const gens = 4

	sys := mountHeavyFieldOnEngine(t, w, h, k, iters, seed, true, ComputeEngineSystem)
	ax := mountHeavyFieldOnEngine(t, w, h, k, iters, seed, true, ComputeEngineAxis1)

	distinct := map[hashKey]bool{}
	for g := 1; g <= gens; g++ {
		if !sys.tickOnce() {
			t.Fatalf("system/compute host faulted at gen %d: %s", g, sys.Render().Err)
		}
		if !ax.tickOnce() {
			t.Fatalf("axis1 host faulted at gen %d: %s", g, ax.Render().Err)
		}
		hs := stateHashAt(t, sys.ap, sys.desc.StatePath)
		ha := stateHashAt(t, ax.ap, ax.desc.StatePath)
		if hs != ha {
			t.Fatalf("gen %d: axis1 host state hash %s != system/compute %s — the engines "+
				"diverged, the benchmark below would be timing two different computations", g, ha, hs)
		}
		distinct[hashKey(hs.String())] = true
	}
	// Anti-vacuity: the field must actually be evolving, or hash agreement on a
	// fixed point proves nothing.
	if len(distinct) < 2 {
		t.Fatalf("VACUOUS: field not evolving across %d gens", gens)
	}
}

type hashKey string

// BenchmarkAxis1HostShardKSweep isolates the k-DEPENDENT cost from the FIXED
// per-tick cost. Fix N (32×32) and op-cost (iters=10) and sweep k on BOTH engines,
// serial (so goroutine overhead doesn't confound). Total eval work is ~constant in
// k (the same N cells are mapped either way); what grows with k is the O(N·k)
// gather-stitch If-cascade + the k fragment puts + the k full-grid re-reads. So the
// slope over k is the sharding overhead, and the k=1 intercept is the fixed cost
// (one shard-map + a trivial gather + the display projection + their puts).
//
// The measurement (§9): the slope is SHALLOW on both engines (~1.2× system, ~1.6×
// axis1 across k=1→16), and even at k=1 axis1 is ~33× faster — so the tick is
// dominated by the FIXED whole-grid work, not the k-dependent sharding overhead.
// The O(N·k) gather (the concat lever, item 3) is real but ~20%, not the mass.
// Run WITHOUT -race:
//
//	make go ARGS="test ./workbench -run XXXNONE -bench BenchmarkAxis1HostShardKSweep -benchtime 40x -count=1"
func BenchmarkAxis1HostShardKSweep(b *testing.B) {
	const w, h, iters = 32, 32, 10
	seed := uint64(12345)
	for _, engine := range []string{ComputeEngineSystem, ComputeEngineAxis1} {
		for _, k := range []int{1, 2, 4, 8, 16} {
			b.Run(fmt.Sprintf("%s/k%d", sanitizePattern(engine), k), func(b *testing.B) {
				host := mountHeavyFieldOnEngine(b, w, h, k, iters, seed, false, engine)
				if !host.tickOnce() {
					b.Fatalf("warm tick faulted (k=%d): %s", k, host.Render().Err)
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

// BenchmarkAxis1HostShardScaling re-runs §5a on both engines. 32×32 / k=8, sweep
// engine × {serial, parallel} × ops-cost. Run WITHOUT -race:
//
//	make go ARGS="test ./workbench -run XXXNONE -bench BenchmarkAxis1HostShardScaling -benchtime 15x -count=1"
//
// Two reads: (1) axis1 ms/op vs system ms/op at the same cell — the absolute
// lift; (2) the parallel/serial ratio per engine. §8: axis1 lifts the absolute
// number ~20-28× but LOWERS the parallel ratio, because eval stops dominating.
func BenchmarkAxis1HostShardScaling(b *testing.B) {
	const w, h, k = 32, 32, 8
	seed := uint64(12345)
	for _, iters := range []int{10, 30} {
		for _, engine := range []string{ComputeEngineSystem, ComputeEngineAxis1} {
			for _, parallel := range []bool{false, true} {
				mode := "serial"
				if parallel {
					mode = "parallel"
				}
				name := fmt.Sprintf("iters%d/%s/%s", iters, sanitizePattern(engine), mode)
				b.Run(name, func(b *testing.B) {
					host := mountHeavyFieldOnEngine(b, w, h, k, iters, seed, parallel, engine)
					if !host.tickOnce() {
						b.Fatalf("warm tick faulted (%s): %s", name, host.Render().Err)
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
}
