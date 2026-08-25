package workbench

// CARRY CHAIN — a dependency-CHAIN program, the experimental control for the
// TIME axis (the mirror of program_heavyfield.go, which was the SPACE axis).
//
// §5/§5a of reviews/COMPUTE-SHARDING-INTO-HOST-2026-07-18.md measured the space
// axis: a wide `map` (independent per cell) shards into k pieces that run in
// parallel, and the speedup climbs toward k× as the per-cell op-cost rises. The
// serial residue there was reads + the O(N·k) stitch. That axis is the POSITIVE:
// independent work parallelizes.
//
// This program is the NEGATIVE on the other axis. It puts two components in one
// tick, deliberately isolated, so a single test can watch them behave oppositely
// under the same sharded host:
//
//   - cells — computed by a `map` over the grid (the SPACE component). Each cell
//     is an independent function of the current cell, so it shards exactly and is
//     k-INDEPENDENT: sharded == unsharded == parallel == serial. Cheap.
//   - carry — computed by a `fold` threaded shard→shard (the TIME component). A
//     fold is a strict left-to-right accumulator chain of length N: step s+1 needs
//     step s's accumulator. You cannot split a fold into independent shards — shard
//     j's carry-IN is shard j-1's carry-OUT. So the k shards form a DEPENDENCY
//     CHAIN, and the only correct evaluation order is serial. The `iters` knob makes
//     each fold step heavy, so a single-eval fold over N busts the 100k budget and
//     sharding becomes load-bearing — the same lever heavyfield used, on the axis
//     that resists it.
//
// The whole point (program_host_chain_test.go): under SERIAL shard evaluation the
// carry threads correctly and the fold is exact — sharded carry == unsharded
// (k=1) carry, k-independent, because segmenting a fold and threading the carry is
// the identity. Under PARALLEL shard evaluation shard j reads a STALE
// frag{j-1}.carry (the host evals all k concurrently, then writes), so the carry
// chain is corrupted — while the map cells come out BYTE-IDENTICAL either way.
// One program, one field parallelizes and one does not, and the difference is
// exactly `map` vs `fold`. That is the Amdahl serial fraction made physical on the
// time axis: the dependency chain's serial floor is the k carry barriers + one
// stitch = O(1) in N, not O(cells).
//
// It reuses the host sharding path unchanged — same `shard` block, same
// self-wrapping fragments, same program-owned gather stitch — so the numbers come
// from the REAL host tick (Host.tickShardedOnce), not a bespoke rig. The only
// twist is that the shard expression reads the PREVIOUS shard's fragment for its
// carry-in, which is what turns the k independent evals into a chain.

import (
	"context"
	"fmt"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"

	"github.com/fxamacker/cbor/v2"

	"entity-workbench-go/entitysdk"
)

const (
	chainGridType     = "app/chain/grid"
	chainFragmentType = "app/chain/fragment"

	// The churn family (same power-of-two-modulus LCG as the heavy field — the
	// point here is the chain STRUCTURE, not the arithmetic). Named chain* so the
	// Go oracle is independent of heavyfield's constants.
	chainMul = uint64(1103515245)
	chainAdd = uint64(12345)
	chainMod = uint64(2147483648) // 2^31

	// The fixed seed the carry chain starts from at the head of every tick. A
	// constant (not the previous carry) keeps the fold a clean length-N intra-tick
	// chain; the field still evolves because the fold reads the current cells.
	chainSeed = uint64(2463534242)
)

// chainCellChurn is the SPACE component's per-cell kernel: a cheap, independent
// function of the cell and its index. A `map` over this shards exactly and is
// k-independent — it is the part that parallelizes.
func chainCellChurn(v, i uint64) uint64 {
	return ((v%chainMod)*chainMul + chainAdd + i) % chainMod
}

// chainFoldStep is the TIME component's per-element kernel: `iters` rounds of
// churn folded into the accumulator. This is the heavy step that (a) makes the
// fold bust the budget so sharding is load-bearing and (b) is the link in the
// dependency chain — acc_{s+1} = chainFoldStep(acc_s, elem). The compute lowering
// (buildChainShardFragment's fold) must match this exactly; it is the oracle.
func chainFoldStep(acc, elem uint64, iters int) uint64 {
	a := acc % chainMod
	for r := 0; r < iters; r++ {
		a = (a*chainMul + chainAdd + (elem % chainMod)) % chainMod
	}
	return a
}

// chainNextCells is the independent Go oracle for the SPACE component: a plain
// map, k-independent by construction.
func chainNextCells(cur []uint64) []uint64 {
	next := make([]uint64, len(cur))
	for i, v := range cur {
		next[i] = chainCellChurn(v, uint64(i))
	}
	return next
}

// chainNextCarry is the independent Go oracle for the TIME component: the GLOBAL
// fold over the whole grid, from chainSeed. This value is what the serial sharded
// host must reproduce (fold-exactness: threading the carry through k segments in
// order yields the identical result), and what a parallel sharded host CANNOT,
// because the segments then run without the prior carry.
func chainNextCarry(cur []uint64, iters int) uint64 {
	acc := chainSeed
	for _, v := range cur {
		acc = chainFoldStep(acc, v, iters)
	}
	return acc
}

// AuthorChainSharded writes a sharded carry-chain program and returns its
// descriptor path. `iters` is the fold-step op-cost knob: a bigger iters makes the
// fold heavier, so k=1 (one fold over all N) busts the budget while k>1 clears it —
// exactly as heavyfield's iters gates its cliff, but on the fold.
func AuthorChainSharded(ap *entitysdk.AppPeer, root string, rngSeed uint64, w, h, k, iters int) (string, error) {
	if ap == nil {
		return "", fmt.Errorf("AuthorChainSharded: nil AppPeer")
	}
	if rngSeed == 0 {
		return "", fmt.Errorf("AuthorChainSharded: rngSeed must be non-zero")
	}
	if k < 1 || iters < 1 {
		return "", fmt.Errorf("AuthorChainSharded: k and iters must be >= 1")
	}
	p := pathsFor(root)
	statePath := p.state
	fragmentBase := root + "/fragments"

	// state₀ — a deterministic soup, carry seeded to 0 (it is recomputed by the
	// fold every tick, so state₀'s carry value is immaterial; 0 keeps it obvious).
	s0 := chainSeedState(w, h, rngSeed)
	ent, err := chainStateEntity(w, h, s0, 0)
	if err != nil {
		return "", fmt.Errorf("AuthorChainSharded: state₀: %w", err)
	}
	state0Hash, err := ap.PutEntity(p.state0, ent)
	if err != nil {
		return "", fmt.Errorf("AuthorChainSharded: put state₀: %w", err)
	}

	n := w * h
	shardPaths := make([]string, k)
	fragPaths := make([]string, k)
	for j := 0; j < k; j++ {
		shardPaths[j] = fmt.Sprintf("%s/shard%d", root, j)
		fragPaths[j] = fmt.Sprintf("%s/frag%d", fragmentBase, j)
	}
	// Seed each fragment so a PARALLEL tick-1 shard read of frag{j-1}.carry finds a
	// value (a stale one — chainSeed) instead of faulting on a missing path. A
	// serial tick never reads these: shard j reads frag{j-1} that shard j-1 wrote
	// THIS tick. So the seed is only ever the stale carry the parallel path trips
	// on — which is the divergence the negative test asserts.
	for j := 0; j < k; j++ {
		seedFrag, err := chainFragmentEntity(nil, chainSeed)
		if err != nil {
			return "", fmt.Errorf("AuthorChainSharded: seed fragment %d: %w", j, err)
		}
		if _, err := ap.PutEntity(fragPaths[j], seedFrag); err != nil {
			return "", fmt.Errorf("AuthorChainSharded: put seed fragment %d: %w", j, err)
		}
	}

	for j := 0; j < k; j++ {
		i0 := j * n / k
		i1 := (j + 1) * n / k
		prevFrag := ""
		if j > 0 {
			prevFrag = fragPaths[j-1]
		}
		if _, err := buildChainShardFragment(ap, statePath, i0, i1, iters, prevFrag).
			Build(context.Background(), shardPaths[j]); err != nil {
			return "", fmt.Errorf("AuthorChainSharded: build shard %d: %w", j, err)
		}
	}

	stitchPath := root + "/stitch"
	if _, err := buildChainGatherStitch(ap, w, h, k, fragPaths).
		Build(context.Background(), stitchPath); err != nil {
		return "", fmt.Errorf("AuthorChainSharded: build stitch: %w", err)
	}

	if _, err := buildFieldTextExpr(ap, w, h, statePath).Build(context.Background(), p.displayExpr); err != nil {
		return "", fmt.Errorf("AuthorChainSharded: build text projection: %w", err)
	}

	d := &ProgramDescriptor{
		StatePath:        statePath,
		InitialState:     p.state0,
		Step:             stitchPath, // sharded: the host runs the shard family, not Step
		InitialStateHash: hashString(state0Hash),
		OutputPorts: []ProgramPort{{
			Name:    "display",
			Path:    p.display,
			Source:  p.displayExpr,
			TypeRef: TextFrameType,
			Kind:    KindSnapshot,
			Role:    RoleDisplay,
			Shape:   ShapeText,
			Scene: map[string]interface{}{
				"mode": TextModeGrid,
				"cols": uint64(w),
				"rows": uint64(h),
			},
		}},
		Tick: ProgramTick{Mode: TickClockDriven, RateHint: 6},
		Shard: &ProgramShard{
			Form:          ShardStaticK,
			Orchestration: OrchestrationHostManaged,
			K:             uint64(k),
			StitchOwner:   StitchOwnerComputeGather,
			Shards:        shardPaths,
			FragmentBase:  fragmentBase,
			Stitch:        stitchPath,
		},
	}
	return writeDescriptor(ap, p.descriptor, d)
}

// chainSeedState is a deterministic 31-bit soup (same high-bit LCG as the field).
func chainSeedState(w, h int, rngSeed uint64) []uint64 {
	cells := make([]uint64, w*h)
	s := rngSeed
	for i := range cells {
		s = (s*chainMul + chainAdd) % chainMod
		cells[i] = s
	}
	return cells
}

func chainStateEntity(w, h int, cells []uint64, carry uint64) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{
		"width": uint64(w), "height": uint64(h), "cells": cells, "carry": carry,
	})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(chainGridType, cbor.RawMessage(raw))
}

func chainFragmentEntity(cells []uint64, carry uint64) (entity.Entity, error) {
	if cells == nil {
		cells = []uint64{}
	}
	raw, err := ecf.Encode(map[string]interface{}{"cells": cells, "carry": carry})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(chainFragmentType, cbor.RawMessage(raw))
}

// buildChainShardFragment builds shard j over the index range [i0,i1). It emits a
// self-wrapping {cells, carry} fragment:
//
//   - cells — a `map` over the slice: chainCellChurn(cur[i], i). Independent per
//     cell, so this is the k-independent SPACE component.
//   - carry — a `fold` over the slice threaded from carry_in. carry_in is
//     chainSeed for shard 0, else frag{j-1}.carry — the read that turns the k
//     evals into a DEPENDENCY CHAIN. In serial mode frag{j-1} is the value shard
//     j-1 wrote this tick (correct); in parallel mode it is the previous tick's
//     stale value (corrupt). The fold body is chainFoldStep unrolled `iters` times.
//
// The unroll lives in the shared fold lambda, so the IR is O(iters), not
// O(N·iters) — same discipline as the heavy field.
func buildChainShardFragment(ap *entitysdk.AppPeer, statePath string, i0, i1, iters int, prevFragPath string) *entitysdk.Builder {
	c := ap.Compute()

	indices := make([]uint64, 0, i1-i0)
	for i := i0; i < i1; i++ {
		indices = append(indices, uint64(i))
	}

	// carry_in: the head of the chain (shard 0) or the previous shard's carry-out.
	var carryIn *entitysdk.Builder
	if prevFragPath == "" {
		carryIn = c.Literal(chainSeed)
	} else {
		carryIn = c.Field(c.LookupTreeLocal(prevFragPath), "carry")
	}

	// cells = map(indices, i -> chainCellChurn(cells[i], i)). Cheap, independent.
	cellsExpr := c.BuiltinsCall("map", map[string]*entitysdk.Builder{
		"collection": c.Literal(indices),
		"fn": c.Lambda([]string{"i"}, c.Arithmetic("mod",
			c.Arithmetic("add",
				c.Arithmetic("add",
					c.Arithmetic("mul",
						c.Arithmetic("mod", c.Index(c.LookupScope("cells"), c.LookupScope("i")), c.Literal(chainMod)),
						c.Literal(chainMul)),
					c.Literal(chainAdd)),
				c.LookupScope("i")),
			c.Literal(chainMod))),
	})

	// carry = fold(indices, carry_in, (acc, idx) -> chainFoldStep(acc, cells[idx])).
	// The fold is the chain: each step references acc exactly once, threaded left to
	// right by the fold builtin. `iters` rounds unrolled in the lambda body.
	carryExpr := entitysdk.LowerFold(c, c.Literal(indices), carryIn,
		func(acc, idx *entitysdk.Builder) *entitysdk.Builder {
			// Bind elem once (a Let), so the iters-round unroll references it by name
			// rather than duplicating the Index subgraph iters times — the IR stays
			// O(iters), the same discipline as the heavy field's leaf references.
			step := c.Arithmetic("mod", acc, c.Literal(chainMod))
			for r := 0; r < iters; r++ {
				step = c.Arithmetic("mod",
					c.Arithmetic("add",
						c.Arithmetic("add",
							c.Arithmetic("mul", step, c.Literal(chainMul)),
							c.Literal(chainAdd)),
						c.LookupScope("elemv")),
					c.Literal(chainMod))
			}
			return c.Let(map[string]*entitysdk.Builder{
				"elemv": c.Arithmetic("mod", c.Index(c.LookupScope("cells"), idx), c.Literal(chainMod)),
			}, step)
		})

	return c.Let(map[string]*entitysdk.Builder{
		"g": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"cells": c.Field(c.LookupScope("g"), "cells"),
	}, c.Construct(chainFragmentType, map[string]*entitysdk.Builder{
		"cells": cellsExpr,
		"carry": carryExpr,
	})))
}

// buildChainGatherStitch gathers the k fragments into the whole {width, height,
// cells, carry} state. cells is the concat-free gather (same structure as the
// field/Life stitch). carry is frag{k-1}.carry — the tail of the chain, i.e. the
// global fold value, which is where the serial/parallel divergence lands.
func buildChainGatherStitch(ap *entitysdk.AppPeer, w, h, k int, fragPaths []string) *entitysdk.Builder {
	c := ap.Compute()
	n := w * h

	frag := make(map[string]*entitysdk.Builder, k)
	for j := 0; j < k; j++ {
		frag[fmt.Sprintf("f%d", j)] = c.Field(c.LookupTreeLocal(fragPaths[j]), "cells")
	}

	lo := func(j int) uint64 { return uint64(j * n / k) }
	hi := func(j int) uint64 { return uint64((j + 1) * n / k) }

	gather := c.Index(c.LookupScope(fmt.Sprintf("f%d", k-1)),
		c.Arithmetic("sub", c.LookupScope("i"), c.Literal(lo(k-1))))
	for j := k - 2; j >= 0; j-- {
		gather = c.If(
			c.Compare("lt", c.LookupScope("i"), c.Literal(hi(j))),
			c.Index(c.LookupScope(fmt.Sprintf("f%d", j)),
				c.Arithmetic("sub", c.LookupScope("i"), c.Literal(lo(j)))),
			gather)
	}

	indices := make([]uint64, n)
	for i := range indices {
		indices[i] = uint64(i)
	}
	body := c.Construct(chainGridType, map[string]*entitysdk.Builder{
		"width":  c.Literal(uint64(w)),
		"height": c.Literal(uint64(h)),
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         c.Lambda([]string{"i"}, gather),
		}),
		// The chain's tail: the last shard's carry IS the global fold, because the
		// carry threaded shard 0 → … → shard k-1. Reading it here is what makes the
		// corrupted parallel carry reach the state hash.
		"carry": c.Field(c.LookupTreeLocal(fragPaths[k-1]), "carry"),
	})
	return c.Let(frag, body)
}
