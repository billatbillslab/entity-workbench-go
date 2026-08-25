package programs

// SHARDED LIFE AUTHORING — the host-managed static-k floor as a real mounted
// program, not a test rig.
//
// This is arch's next-build item 1 (HANDOFF-2026-07-18-…-rulings §5.1): lift the
// host-managed sharding proven in entitysdk/axis1_shard_test.go out of the test
// and into an authored program the generic host mounts and ticks. The Q5 reversal
// (§2 of that handoff) cleared it: the static-k Option-B floor lands now, un-gated
// from `range` and from `array-concat`, because the stitch is expressible in pure
// compute (the concat-free gather, entitysdk/continuation_shard_test.go stage 4).
//
// ─── What is authored, and where the host reads it ─────────────────────────
//
//	state0            state₀ (past the budget cliff — 32×32 by default)
//	shard{j}          k shard expressions; each evals a self-wrapping FRAGMENT
//	                  entity (app/life/fragment{cells}) over its index range
//	fragments/frag{j} where the host WRITES fragment j each tick (FragmentBase)
//	stitch            the program-owned gather: reads the k fragments, produces
//	                  the whole app/life/grid — the boundary owner (§4 Q4)
//	display-expr      the text projection (identical to unsharded Life)
//	interface         the descriptor, carrying the shard block
//
// The host loops shard{0..k} → fragments, then evals stitch → state. It decodes
// none of it (program_host.go tickShardedOnce). The one thing that makes that
// possible is that a shard SELF-WRAPS into a fragment entity, so the host writes
// it verbatim exactly as it writes an unsharded step result.
//
// ─── Relationship to the frozen probe ──────────────────────────────────────
//
// The per-cell rule and the shard split are the same arithmetic as
// entitysdk/axis1_shard_test.go's buildLifeShardArith and the gather from
// continuation_shard_test.go's buildGatherStitch — but those live in the frozen
// experiment package. This is the product copy (same relationship buildLifeStepExpr
// has to the Exp-D probe): change them together if the lowering ever moves.

import (
	"context"
	"fmt"

	"entity-workbench-go/entitysdk"
)

const (
	// lifeFragmentType is a shard's partial output: a field-accessible entity the
	// gather stitch reads via lookup/tree + field("cells"). A shard produces this,
	// NOT a bare array — that is what lets the host write it without decoding it.
	lifeFragmentType = "app/life/fragment"

	// lifeShardWidth/Height: the sharded program is deliberately sized PAST the
	// arith budget cliff (24×24), so the unsharded step returns budget_exhausted
	// and only the sharded tick works. That is the whole point — a program that
	// cannot mount unsharded, mounting through the floor.
	lifeShardWidth  = 32
	lifeShardHeight = 32

	// lifeShardK: k=4 gives each 32×32 shard 256 cells, comfortably under the cap
	// (the same k the budget-cliff prototype cleared 32×32 with).
	lifeShardK = 4
)

// AuthorLifeSharded writes a sharded Life program and returns its descriptor
// path. Idempotent for a given (root, seed, w, h, k): the IR is content-addressed
// and state₀ is a pure function of rngSeed.
//
// The default caller (the bridge / a panel) passes the package sizing
// (lifeShardWidth × lifeShardHeight, k=lifeShardK); w/h/k are parameters so a
// test can pin sharded == unsharded at a size the unsharded step also fits.
func AuthorLifeSharded(ap *entitysdk.AppPeer, root string, rngSeed uint64, w, h, k int) (string, error) {
	if ap == nil {
		return "", fmt.Errorf("AuthorLifeSharded: nil AppPeer")
	}
	if rngSeed == 0 {
		return "", fmt.Errorf("AuthorLifeSharded: rngSeed must be non-zero (state₀ must be reproducible)")
	}
	if k < 1 {
		return "", fmt.Errorf("AuthorLifeSharded: k must be >= 1")
	}
	p := pathsFor(root)
	statePath := p.state
	fragmentBase := root + "/fragments"

	// state₀ — authored, not host-constructed. Same seed function as unsharded
	// Life, so a sharded and an unsharded program with the same rngSeed share
	// state₀ byte-for-byte (the licence for the sharded == unsharded gate).
	s0 := lifeSeedState(w, h, rngSeed)
	ent, err := lifeStateEntity(s0)
	if err != nil {
		return "", fmt.Errorf("AuthorLifeSharded: state₀: %w", err)
	}
	state0Hash, err := ap.PutEntity(p.state0, ent)
	if err != nil {
		return "", fmt.Errorf("AuthorLifeSharded: put state₀: %w", err)
	}

	// The k shard expressions and the fragment paths they will be written to.
	n := w * h
	shardPaths := make([]string, k)
	fragPaths := make([]string, k)
	for j := 0; j < k; j++ {
		i0 := j * n / k
		i1 := (j + 1) * n / k
		shardPaths[j] = fmt.Sprintf("%s/shard%d", root, j)
		fragPaths[j] = fmt.Sprintf("%s/frag%d", fragmentBase, j)
		if _, err := buildLifeShardFragment(ap, w, h, statePath, i0, i1).
			Build(context.Background(), shardPaths[j]); err != nil {
			return "", fmt.Errorf("AuthorLifeSharded: build shard %d: %w", j, err)
		}
	}

	// The program-owned stitch: the concat-free gather over the k fragment paths.
	stitchPath := root + "/stitch"
	if _, err := buildLifeGatherStitch(ap, w, h, k, fragPaths).
		Build(context.Background(), stitchPath); err != nil {
		return "", fmt.Errorf("AuthorLifeSharded: build stitch: %w", err)
	}

	// The text projection — identical to unsharded Life: it reads the stitched
	// grid at statePath, which is an ordinary app/life/grid.
	if _, err := buildLifeTextExpr(ap, w, h, statePath).Build(context.Background(), p.displayExpr); err != nil {
		return "", fmt.Errorf("AuthorLifeSharded: build text projection: %w", err)
	}

	d := &ProgramDescriptor{
		StatePath:    statePath,
		InitialState: p.state0,
		// Step is required by Validate, but a sharded program has no single
		// unsharded step the host evals — it runs the shard family. We name the
		// stitch (the one evaluable that yields state'), so a reader/tool asking
		// "what advances state" gets a sensible answer; the host ignores Step when
		// Shard is present. Flagged to arch as the §3 note.
		Step:             stitchPath,
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
		Tick: ProgramTick{
			Mode:     TickClockDriven,
			RateHint: 6,
		},
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

// buildLifeShardFragment builds a shard step: it computes cells [i0,i1) only and
// Constructs a FRAGMENT entity {cells: [...]} — the self-wrapping the host relies
// on (program_descriptor.go's ProgramShard header).
//
// The per-cell rule is the same arith lowering as buildLifeStepExpr; the only
// differences from the unsharded step are (a) the collection literal is the
// shard's index range, and (b) the result is a fragment, not the whole grid. Each
// shard still reads the FULL grid (a cell's neighbors may live in another strip),
// which is the k-times read sharding trades for budget headroom (Axis-1 §4).
func buildLifeShardFragment(ap *entitysdk.AppPeer, w, h int, statePath string, i0, i1 int) *entitysdk.Builder {
	c := ap.Compute()
	W, H := uint64(w), uint64(h)

	neighbor := func(dx, dy int) *entitysdk.Builder {
		nx := c.Arithmetic("mod",
			c.Arithmetic("add", c.LookupScope("x"), c.Literal(uint64(w+dx))),
			c.Literal(W))
		ny := c.Arithmetic("mod",
			c.Arithmetic("add", c.LookupScope("y"), c.Literal(uint64(h+dy))),
			c.Literal(H))
		ni := c.Arithmetic("add", c.Arithmetic("mul", ny, c.Literal(W)), nx)
		return c.Index(c.LookupScope("cells"), ni)
	}
	terms := make([]*entitysdk.Builder, 0, 8)
	for _, d := range lifeOffsets {
		terms = append(terms, neighbor(d[0], d[1]))
	}

	perCell := c.Let(map[string]*entitysdk.Builder{
		"x": c.Arithmetic("mod", c.LookupScope("i"), c.Literal(W)),
		"y": c.Arithmetic("div",
			c.Arithmetic("sub", c.LookupScope("i"), c.LookupScope("x")),
			c.Literal(W)),
	}, c.Let(map[string]*entitysdk.Builder{
		"count": lifeAddTree(c, terms),
		"alive": c.Compare("eq",
			c.Index(c.LookupScope("cells"), c.LookupScope("i")),
			c.Literal(uint64(1))),
	}, lifeRule(c)))

	indices := make([]uint64, 0, i1-i0)
	for i := i0; i < i1; i++ {
		indices = append(indices, uint64(i))
	}

	return c.Let(map[string]*entitysdk.Builder{
		"g": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"cells": c.Field(c.LookupScope("g"), "cells"),
	}, c.Construct(lifeFragmentType, map[string]*entitysdk.Builder{
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         c.Lambda([]string{"i"}, perCell),
		}),
	})))
}

// buildLifeGatherStitch is the concat-free stitch (continuation_shard_test.go
// stage 4, product copy): a compute step that reads the k shard fragments and
// produces the flat grid via map + conditional gather. Boundaries lo_j = j*N/k,
// hi_j = (j+1)*N/k match AuthorLifeSharded's split, including ragged k.
//
// For global index i the last shard is the else-branch (no upper bound); each
// earlier shard j guards with `i < hi_j`. The fragments are bound once (O(k) tree
// reads) and the map lambda closes over them, so it is O(k) reads and O(N·k)
// compares — under one budget for realistic k (≤8) up to 64×64.
func buildLifeGatherStitch(ap *entitysdk.AppPeer, w, h, k int, fragPaths []string) *entitysdk.Builder {
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
	body := c.Construct(lifeGridType, map[string]*entitysdk.Builder{
		"width":  c.Literal(uint64(w)),
		"height": c.Literal(uint64(h)),
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         c.Lambda([]string{"i"}, gather),
		}),
	})
	return c.Let(frag, body)
}
