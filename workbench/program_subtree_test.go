package workbench

// TIER: integration (TESTING-STRATEGY) — real peer, store, evaluator.
//
// THE SUBTREE-STATE PROBE (arch's follow-on to §9 / Finding on the whole-state
// floor). §9 measured the post-Axis-1 floor as a fixed O(N)-per-tick cost — three
// full-grid passes (shard map + gather stitch + display projection), each with a
// whole-grid store put — and framed it as inherent to the immutable-entity model.
// Arch corrected the framing: the O(N) cost is the MONOLITHIC-ENTITY default, not
// the immutable model. The tree is already `path → hash` with content dedup
// (entitysdk/store.go: Put → content.Put dedups by hash → locationIndex.Set), so a
// state split into per-tile subtree entities gives an O(changed) tick — only the
// tiles that changed get new content; unchanged tiles keep their hash for free.
// The ask: re-run the sharded tick with per-tile state and see if the floor
// collapses.
//
// This file is that probe. It compares two representations of the SAME field on
// the SAME engine (Axis-1):
//
//   - MONOLITHIC (the current model): state is one {width,height,cells:[N]} entity.
//     A tick evals k shards (each re-reads the WHOLE grid), gathers them (O(N·k)),
//     and puts one N-cell state entity. Every tick rewrites all N cells.
//   - SUBTREE (the probe): state is k tile entities at `state/tile{j}`, each
//     {cells:[N/k]}. heavyKernel is per-cell independent, so tile j evolves from
//     ONLY its own cells — no whole-grid read, no gather, no monolithic put. A tick
//     re-evals + re-puts only the tiles that changed.
//
// The subtree shard uses the GLOBAL cell index (offset + local), so the k tiles
// concatenated are byte-identical to the monolithic grid — same computation, two
// representations (TestSubtree_EquivalentAndDedup pins this). The benchmark then
// reads whether the fixed floor is representation-reducible: dense (all tiles
// change) drops the gather + whole-grid read; sparse (c of k change) drops to
// O(changed). If it collapses, the Doom-realtime question moves from "beat O(N)"
// to "sparse state + the rasterizer seam" — arch's reframe, measured.

import (
	"context"
	"fmt"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/types"

	"github.com/fxamacker/cbor/v2"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/entitysdk/axis1"
)

// subtreeRig carries one peer with BOTH representations of the same field authored
// on it (shared seed), so a bench can drive either over the identical Axis-1 path.
type subtreeRig struct {
	ap        *entitysdk.AppPeer
	host      *Host    // the monolithic heavy field, mounted (for its shard/gather/state paths)
	tilePaths []string // subtree state: state/tile{j}
	tileShard []string // subtree step: reads tile{j} → writes next tile{j}
	k         int
	tileLen   int
}

// authorSubtreeRig authors the monolithic heavy field and the per-tile subtree
// twin on one peer, both seeded identically.
func authorSubtreeRig(t testing.TB, w, h, k, iters int, seed uint64) *subtreeRig {
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

	// Monolithic twin (reuse the §5a program).
	monoRoot := fmt.Sprintf("app/subtree-mono-%dx%d-k%d", w, h, k)
	descPath, err := AuthorHeavyFieldSharded(ap, monoRoot, seed, w, h, k, iters)
	if err != nil {
		t.Fatalf("AuthorHeavyFieldSharded: %v", err)
	}
	host, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(host.Close)
	host.computePattern = ComputeEngineAxis1

	// Subtree twin: k tiles + k tile-shards, seeded from the SAME soup so the
	// concatenation equals the monolithic state₀.
	n := w * h
	tileLen := n / k
	if tileLen*k != n {
		t.Fatalf("subtree probe needs k | N (N=%d, k=%d)", n, k)
	}
	soup := fieldSeedState(w, h, seed)
	tileRoot := fmt.Sprintf("app/subtree-tiles-%dx%d-k%d", w, h, k)
	tilePaths := make([]string, k)
	tileShard := make([]string, k)
	for j := 0; j < k; j++ {
		offset := j * tileLen
		tilePaths[j] = fmt.Sprintf("%s/tile%d", tileRoot, j)
		tileShard[j] = fmt.Sprintf("%s/tileshard%d", tileRoot, j)
		ent, err := fieldFragmentEntityCells(soup[offset : offset+tileLen])
		if err != nil {
			t.Fatalf("tile%d seed: %v", j, err)
		}
		if _, err := ap.PutEntity(tilePaths[j], ent); err != nil {
			t.Fatalf("put tile%d: %v", j, err)
		}
		if _, err := buildTileShard(ap, tilePaths[j], offset, tileLen, iters).
			Build(context.Background(), tileShard[j]); err != nil {
			t.Fatalf("build tileshard%d: %v", j, err)
		}
	}
	return &subtreeRig{ap: ap, host: host, tilePaths: tilePaths, tileShard: tileShard, k: k, tileLen: tileLen}
}

// fieldFragmentEntityCells wraps a cell slice as an app/field/fragment (the tile's
// on-disk shape — same {cells} the shard produces, so a tick overwrites verbatim).
func fieldFragmentEntityCells(cells []uint64) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{"cells": append([]uint64(nil), cells...)})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(fieldFragmentType, cbor.RawMessage(raw))
}

// buildTileShard is the subtree step for one tile: read tile{j}'s own cells, map
// heavyKernel over them using the GLOBAL index (offset + local) so the result
// matches the monolithic grid slice, and Construct the next tile. No whole-grid
// read, no gather — the tile is the unit of state.
func buildTileShard(ap *entitysdk.AppPeer, tilePath string, offset, tileLen, iters int) *entitysdk.Builder {
	c := ap.Compute()
	locals := make([]uint64, tileLen)
	for l := range locals {
		locals[l] = uint64(l)
	}
	// perCell churns cell[l] using global index gi = l + offset (bound once, so the
	// iters-round unroll stays O(iters)).
	perCell := c.Arithmetic("mod", c.Index(c.LookupScope("cells"), c.LookupScope("l")), c.Literal(fieldMod))
	for r := 0; r < iters; r++ {
		perCell = c.Arithmetic("mod",
			c.Arithmetic("add",
				c.Arithmetic("add",
					c.Arithmetic("mul", perCell, c.Literal(fieldMul)),
					c.Literal(fieldAdd)),
				c.LookupScope("gi")),
			c.Literal(fieldMod))
	}
	body := c.Construct(fieldFragmentType, map[string]*entitysdk.Builder{
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(locals),
			"fn": c.Lambda([]string{"l"}, c.Let(map[string]*entitysdk.Builder{
				"gi": c.Arithmetic("add", c.LookupScope("l"), c.Literal(uint64(offset))),
			}, perCell)),
		}),
	})
	return c.Let(map[string]*entitysdk.Builder{
		"t": c.LookupTreeLocal(tilePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"cells": c.Field(c.LookupScope("t"), "cells"),
	}, body))
}

// evalAt runs one eval over the Axis-1 engine and returns the result (type, data).
// Mirrors Host.eval so the probe drives the same protocol path as a real tick.
func evalAt(tb testing.TB, ap *entitysdk.AppPeer, path string) (string, cbor.RawMessage) {
	tb.Helper()
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		tb.Fatal(err)
	}
	resp, err := ap.Executor().ExecuteOnResource(ComputeEngineAxis1, "eval", req,
		&types.ResourceTarget{Targets: []string{path}})
	if err != nil {
		tb.Fatalf("eval %s: %v", path, err)
	}
	if resp.Status != 200 || resp.Type == types.TypeComputeError {
		tb.Fatalf("eval %s: status %d type %s", path, resp.Status, resp.Type)
	}
	return resp.Type, resp.Data
}

// tickSubtree ticks the subtree state: re-eval + re-put the `changed` tiles only.
// Unchanged tiles are not touched — the O(changed) tick the probe measures.
func (r *subtreeRig) tickSubtree(tb testing.TB, changed int) {
	for j := 0; j < changed; j++ {
		typ, data := evalAt(tb, r.ap, r.tileShard[j])
		ent, err := entity.NewEntity(typ, data)
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := r.ap.PutEntity(r.tilePaths[j], ent); err != nil {
			tb.Fatalf("put tile%d: %v", j, err)
		}
	}
}

// tickMonolithicState ticks the monolithic state: k shard evals (each re-reads the
// whole grid) + fragment puts + the O(N·k) gather + one whole-grid state put. This
// is the current host's sharded state-tick (Host.tickShardedOnce + the state put),
// minus the display projection (excluded from both sides so the comparison is the
// STATE representation, not rendering).
func (r *subtreeRig) tickMonolithicState(tb testing.TB) {
	typ, data, err := r.host.tickShardedOnce()
	if err != nil {
		tb.Fatalf("monolithic tick: %v", err)
	}
	ent, err := entity.NewEntity(typ, data)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := r.host.ap.PutEntity(r.host.desc.StatePath, ent); err != nil {
		tb.Fatalf("put state: %v", err)
	}
}

// tileCells reads a tile's current cells.
func (r *subtreeRig) tileCells(tb testing.TB, j int) []uint64 {
	tb.Helper()
	ent, ok, err := r.ap.Get(r.tilePaths[j])
	if err != nil || !ok {
		tb.Fatalf("read tile%d: ok=%v err=%v", j, ok, err)
	}
	var s struct {
		Cells []uint64 `cbor:"cells"`
	}
	if err := ecf.Decode(ent.Data, &s); err != nil {
		tb.Fatalf("decode tile%d: %v", j, err)
	}
	return s.Cells
}

// TestSubtree_EquivalentAndDedup pins the two claims the probe rests on:
//  1. EQUIVALENCE — ticking every tile evolves the field identically to the Go
//     monolithic oracle (heavyFieldNextGen), so the subtree representation computes
//     the same thing, tile for tile.
//  2. DEDUP → O(changed) — a tick that re-puts only `c` changed tiles grows the
//     content store by ~c tiles' worth, NOT N; re-putting an UNCHANGED tile's
//     identical content adds no new entity. This is the store-level proof that a
//     subtree tick is O(changed), not O(N).
func TestSubtree_EquivalentAndDedup(t *testing.T) {
	const seed = uint64(0x5175B)
	const w, h, k, iters = 16, 16, 4, 20
	r := authorSubtreeRig(t, w, h, k, iters, seed)

	// (1) Equivalence: tick all tiles for a few gens; concatenation == Go oracle.
	cur := fieldSeedState(w, h, seed)
	for g := 1; g <= 3; g++ {
		r.tickSubtree(t, k) // all tiles
		want := heavyFieldNextGen(cur, iters)
		got := make([]uint64, 0, w*h)
		for j := 0; j < k; j++ {
			got = append(got, r.tileCells(t, j)...)
		}
		if len(got) != len(want) {
			t.Fatalf("gen %d: %d cells, want %d", g, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("gen %d cell %d: subtree %d != monolithic oracle %d", g, i, got[i], want[i])
			}
		}
		cur = want
	}

	// (2) Dedup → O(changed). Re-put tile 0 with its OWN current content (no change):
	// the content store must not grow — identical content dedups by hash.
	before := r.ap.Store().EntityCount()
	cur0 := r.tileCells(t, 0)
	same, err := fieldFragmentEntityCells(cur0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ap.PutEntity(r.tilePaths[0], same); err != nil {
		t.Fatal(err)
	}
	if after := r.ap.Store().EntityCount(); after != before {
		t.Fatalf("re-putting identical tile content grew the content store %d → %d — dedup "+
			"not holding, so a subtree tick would NOT be O(changed)", before, after)
	}

	// A tick of ONE tile adds exactly one new tile entity (the changed content),
	// not N — the O(changed) property, measured at the store.
	before = r.ap.Store().EntityCount()
	r.tickSubtree(t, 1)
	added := r.ap.Store().EntityCount() - before
	if added > 1 {
		t.Fatalf("one-tile tick added %d content entities, want ≤1 — a sparse tick is not O(changed)", added)
	}
	t.Logf("dedup holds: identical re-put added 0 entities; a one-tile tick added %d (O(changed), not O(N=%d))", added, w*h)
}

// BenchmarkSubtreeVsMonolithic is arch's ask — the sharded tick re-run with
// per-tile state. N=1024, k=16, Axis-1, serial. Read the collapse off it:
//
//   - monolithic: the current whole-state tick (shards re-read the whole grid +
//     O(N·k) gather + one N-cell put).
//   - subtree/dense: all 16 tiles re-evaled + re-put (no gather, no whole-grid read).
//   - subtree/sparse-c: only c of 16 tiles change — the Doom-class regime.
//
// Run WITHOUT -race:
//
//	make go ARGS="test ./workbench -run XXXNONE -bench BenchmarkSubtreeVsMonolithic -benchtime 40x -count=1"
func BenchmarkSubtreeVsMonolithic(b *testing.B) {
	const w, h, k, iters = 32, 32, 16, 10
	seed := uint64(12345)

	b.Run("monolithic", func(b *testing.B) {
		r := authorSubtreeRig(b, w, h, k, iters, seed)
		r.tickMonolithicState(b)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			r.tickMonolithicState(b)
		}
	})
	for _, changed := range []int{16, 4, 1} {
		label := "subtree/dense-k16"
		if changed != k {
			label = fmt.Sprintf("subtree/sparse-c%d", changed)
		}
		b.Run(label, func(b *testing.B) {
			r := authorSubtreeRig(b, w, h, k, iters, seed)
			r.tickSubtree(b, changed)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.tickSubtree(b, changed)
			}
		})
	}
}
