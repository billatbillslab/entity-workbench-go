package programs

// HEAVY FIELD — a compute-INTENSIVE wide map, the experimental control for
// "true parallel cost vs. store-read cost" (arch §5.3 first bullet).
//
// The 64×64 Life scaling probe (program_host_shard_scale_test.go) measured a flat
// ~1.8× parallel speedup regardless of k, and named the suspected cause: Life's
// per-cell op count is LOW (~153 ops/cell), so each sharded tick is dominated by
// the fixed cost of every shard re-reading the grid from the shared store, not by
// compute — a read-bound workload does not parallelize past the read.
//
// This program is the control that isolates the variable. It is the same sharded
// shape (state grid, k shard fragments, gather stitch) but with a **tunable
// per-cell op cost**: each cell runs `iters` rounds of an arithmetic churn, so
// ops/cell ≈ 4·iters. Turning `iters` up makes the tick compute-bound while the
// per-shard read cost stays fixed. The prediction the measurement checks
// (program_host_heavyfield_test.go): as ops/cell rises, the parallel speedup
// should climb toward k×, because the parallel region (compute) grows against the
// fixed serial region (reads + the O(N·k) stitch). If it does, Life's ~1.8× was
// the reads, not a ceiling on entity-compute parallelism.
//
// It reuses the host sharding path unchanged — same `shard` block, same
// self-wrapping fragments, same program-owned gather stitch — so the numbers come
// from the REAL host tick (Host.tickShardedOnce), not a bespoke rig.

import (
	"context"
	"fmt"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"

	"github.com/fxamacker/cbor/v2"

	"entity-workbench-go/entitysdk"
)

const (
	fieldGridType     = "app/field/grid"
	fieldFragmentType = "app/field/fragment"

	// The churn constants. A power-of-two modulus keeps values bounded to 31 bits
	// (so they stay valid uint cell values) while mixing enough to evolve. The
	// per-round `+ i` keeps cells distinct rather than collapsing to a common
	// orbit, which is what keeps the grid's state hash changing every tick (the
	// anti-vacuity property the measurement relies on).
	fieldMul = uint64(1103515245)
	fieldAdd = uint64(12345)
	fieldMod = uint64(2147483648) // 2^31
)

// AuthorHeavyFieldSharded writes a sharded heavy-field program and returns its
// descriptor path. `iters` is the per-cell op-cost knob: ops/cell ≈ 4·iters, so
// the budget cliff (and thus the k a size needs) is a function of iters — a small
// grid with a big `iters` shards as hard as a big grid with a cheap kernel.
func AuthorHeavyFieldSharded(ap *entitysdk.AppPeer, root string, rngSeed uint64, w, h, k, iters int) (string, error) {
	if ap == nil {
		return "", fmt.Errorf("AuthorHeavyFieldSharded: nil AppPeer")
	}
	if rngSeed == 0 {
		return "", fmt.Errorf("AuthorHeavyFieldSharded: rngSeed must be non-zero")
	}
	if k < 1 || iters < 1 {
		return "", fmt.Errorf("AuthorHeavyFieldSharded: k and iters must be >= 1")
	}
	p := pathsFor(root)
	statePath := p.state
	fragmentBase := root + "/fragments"

	// state₀ — a deterministic soup (a pure function of rngSeed), so the field's
	// dynamics are reproducible.
	s0 := fieldSeedState(w, h, rngSeed)
	ent, err := fieldStateEntity(w, h, s0)
	if err != nil {
		return "", fmt.Errorf("AuthorHeavyFieldSharded: state₀: %w", err)
	}
	state0Hash, err := ap.PutEntity(p.state0, ent)
	if err != nil {
		return "", fmt.Errorf("AuthorHeavyFieldSharded: put state₀: %w", err)
	}

	n := w * h
	shardPaths := make([]string, k)
	fragPaths := make([]string, k)
	for j := 0; j < k; j++ {
		i0 := j * n / k
		i1 := (j + 1) * n / k
		shardPaths[j] = fmt.Sprintf("%s/shard%d", root, j)
		fragPaths[j] = fmt.Sprintf("%s/frag%d", fragmentBase, j)
		if _, err := buildHeavyFieldShardFragment(ap, statePath, i0, i1, iters).
			Build(context.Background(), shardPaths[j]); err != nil {
			return "", fmt.Errorf("AuthorHeavyFieldSharded: build shard %d: %w", j, err)
		}
	}

	stitchPath := root + "/stitch"
	if _, err := buildFieldGatherStitch(ap, w, h, k, fragPaths).
		Build(context.Background(), stitchPath); err != nil {
		return "", fmt.Errorf("AuthorHeavyFieldSharded: build stitch: %w", err)
	}

	if _, err := buildFieldTextExpr(ap, w, h, statePath).Build(context.Background(), p.displayExpr); err != nil {
		return "", fmt.Errorf("AuthorHeavyFieldSharded: build text projection: %w", err)
	}

	d := &ProgramDescriptor{
		StatePath:        statePath,
		InitialState:     p.state0,
		Step:             stitchPath, // sharded: the host runs the shard family, not Step (see program_life_shard.go)
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

// fieldSeedState is a deterministic soup of 31-bit values (high-bit LCG, same
// reasoning as lifeSeedState's density fix — low bits cycle on a power-of-two
// modulus, but here we keep the whole 31-bit value as the cell).
func fieldSeedState(w, h int, rngSeed uint64) []uint64 {
	cells := make([]uint64, w*h)
	s := rngSeed
	for i := range cells {
		s = (s*fieldMul + fieldAdd) % fieldMod
		cells[i] = s
	}
	return cells
}

func fieldStateEntity(w, h int, cells []uint64) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{
		"width": uint64(w), "height": uint64(h), "cells": cells,
	})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(fieldGridType, cbor.RawMessage(raw))
}

// heavyKernel is the Go reference for one cell's per-tick computation: `iters`
// rounds of churn, mixing the cell index in each round. The compute lowering
// (buildHeavyFieldShardFragment) must match this exactly — it is the oracle the
// sharded result is checked against.
func heavyKernel(v uint64, i, iters int) uint64 {
	acc := v % fieldMod
	for r := 0; r < iters; r++ {
		acc = (acc*fieldMul + fieldAdd + uint64(i)) % fieldMod
	}
	return acc
}

// heavyFieldNextGen is the independent Go oracle for one whole-grid tick.
func heavyFieldNextGen(cells []uint64, iters int) []uint64 {
	next := make([]uint64, len(cells))
	for i, v := range cells {
		next[i] = heavyKernel(v, i, iters)
	}
	return next
}

// buildHeavyFieldShardFragment builds a shard step over the index range [i0,i1):
// for each global index i it runs the unrolled `iters`-round churn on cell[i] and
// Constructs a self-wrapping fragment {cells:[...]}. ops/cell ≈ 4·iters.
//
// The churn is a straight nested chain (no Let): each round references the
// previous acc exactly once, so there is no repeated subgraph to bind — unlike
// Life's count/alive, which are read multiple times. The unroll lives in the
// shared map lambda, so the IR is O(iters), not O(N·iters).
func buildHeavyFieldShardFragment(ap *entitysdk.AppPeer, statePath string, i0, i1, iters int) *entitysdk.Builder {
	c := ap.Compute()

	perCell := c.Arithmetic("mod",
		c.Index(c.LookupScope("cells"), c.LookupScope("i")),
		c.Literal(fieldMod))
	for r := 0; r < iters; r++ {
		perCell = c.Arithmetic("mod",
			c.Arithmetic("add",
				c.Arithmetic("add",
					c.Arithmetic("mul", perCell, c.Literal(fieldMul)),
					c.Literal(fieldAdd)),
				c.LookupScope("i")),
			c.Literal(fieldMod))
	}

	indices := make([]uint64, 0, i1-i0)
	for i := i0; i < i1; i++ {
		indices = append(indices, uint64(i))
	}

	return c.Let(map[string]*entitysdk.Builder{
		"g": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"cells": c.Field(c.LookupScope("g"), "cells"),
	}, c.Construct(fieldFragmentType, map[string]*entitysdk.Builder{
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         c.Lambda([]string{"i"}, perCell),
		}),
	})))
}

// buildFieldGatherStitch is the concat-free gather (same structure as
// buildLifeGatherStitch), Constructing a fieldGridType grid from the k fragments.
func buildFieldGatherStitch(ap *entitysdk.AppPeer, w, h, k int, fragPaths []string) *entitysdk.Builder {
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
	body := c.Construct(fieldGridType, map[string]*entitysdk.Builder{
		"width":  c.Literal(uint64(w)),
		"height": c.Literal(uint64(h)),
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         c.Lambda([]string{"i"}, gather),
		}),
	})
	return c.Let(frag, body)
}

// buildFieldTextExpr projects the field to a character grid: a coarse shade by
// the cell's low bits, so the panel shows the churn evolving. Blind `text` driver
// reads it exactly like Life's grid — the shape is the contract, not the program.
func buildFieldTextExpr(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
	c := ap.Compute()
	shades := []uint64{uint64(' '), uint64('.'), uint64(':'), uint64('#')}
	// glyph = shades[v mod 4]. Straight integer `mod` — NOT div-then-mod: compute's
	// `div` is true (float) division, and a float quotient poisons a following `mod`
	// (F-D1, the same gotcha Life's floor-div workaround exists for). We only need a
	// shade bucket, so the low bits do fine and stay integer.
	bucket := c.Arithmetic("mod", c.LookupScope("v"), c.Literal(uint64(4)))
	glyph := c.Literal(shades[3])
	for b := 2; b >= 0; b-- {
		glyph = c.If(
			c.Compare("eq", c.LookupScope("bkt"), c.Literal(uint64(b))),
			c.Literal(shades[b]),
			glyph)
	}
	return c.Let(map[string]*entitysdk.Builder{
		"g": c.LookupTreeLocal(statePath),
	}, c.Construct(TextFrameType, map[string]*entitysdk.Builder{
		"cols": c.Literal(uint64(w)),
		"rows": c.Literal(uint64(h)),
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Field(c.LookupScope("g"), "cells"),
			"fn": c.Lambda([]string{"v"}, c.Let(map[string]*entitysdk.Builder{
				"bkt": bucket,
			}, glyph)),
		}),
	}))
}
