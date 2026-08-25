package entitysdk_test

// CONTINUATION-MANAGED SHARDING — proving the fork/join model end to end.
//
// COMPUTE-PARALLELIZATION-TWO-MODELS-2026-07-17.md §7 step 2. axis1_shard_test.go
// proved the HOST-managed model: the Go host loops the k shards, stitches in Go,
// writes once. This file proves the CONTINUATION-managed model, where the barrier
// and the stitch dispatch are protocol primitives, not host code:
//
//   - a standing system/continuation/join declares Expected = [shard0..shardk-1]
//     and a stitch handler as its Target;
//   - each shard, once evaluated, advances its own join SLOT;
//   - the join accumulates slots under a per-path lock and, when the last slot
//     lands, dispatches ONCE to the stitch with the assembled results
//     (ext/continuation/advance.go advanceJoinSlot @ core-go a0d9ea6).
//
// STAGE 1 (this file): the barrier + the stitch dispatch — the novel primitive.
// The host still evaluates each shard and fires the slot advance (the fork
// TRIGGER is host-driven; the fork wiring via deliver_to is stage 2). What is
// under test is that the JOIN orchestrates the barrier and the stitch
// deterministically, producing a state hash byte-identical to the host-managed
// serial path and to the Life oracle. Determinism is the whole product
// (POC §1.4 — replay/lockstep); a join that produced a different or run-varying
// hash would trade it away.
//
// The stitch is a Go/app handler here, per §3.1: entity-compute has no array-
// concat primitive (Axis-1 §4.2), so the aggregation bottoms out on host code in
// BOTH models until concat lands. This stage does not pretend otherwise — it
// proves the fork/barrier is a protocol concern; the stitch's home is arch's
// open question (§6.2).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

// stitchOut is what the stitch handler hands back to the test over a buffered
// channel. The channel MUST be buffered: the join fires the stitch synchronously
// inside the last slot's Advance call, so an unbuffered send would deadlock the
// advance against the test's later receive (the A4 recCh pattern,
// exp_compute_chain_step_test.go).
type stitchOut struct {
	cells []uint64
	hash  hash.Hash
	err   error
}

// contShardRig is a host-managed shardRig plus a standing join and a Go stitch
// handler wired as the join's target.
type contShardRig struct {
	*shardRig
	joinPath string
	out      chan stitchOut
}

// newContShardRig builds the k shard steps (reusing newShardRig — same split,
// same seed, same statePath, so its hashes are directly comparable), then
// registers a stitch handler and installs a standing join targeting it.
func newContShardRig(t testing.TB, w, h, k int, seed []uint64) *contShardRig {
	t.Helper()
	base := newShardRig(t, w, h, k, seed)

	root := fmt.Sprintf("app/life/shard-cont-%dx%d-k%d", w, h, k)
	stitchPat := root + "/stitch"
	joinPath := root + "/join"
	out := registerStitch(t, base.ap, stitchPat, w, h, k)

	expected := make([]string, k)
	for j := 0; j < k; j++ {
		expected[j] = fmt.Sprintf("shard%d", j)
	}
	jd := types.ContinuationJoinData{
		Expected:  expected,
		Target:    stitchPat,
		Operation: "stitch",
		// No Params, no ResultField → assembleParams passes the received map
		// through as the stitch's params (ext/continuation/helpers.go:277).
	}
	entitysdk.SetDefaultDispatchCapJoin(base.ap.OwnerCapability().ContentHash, &jd)
	je, err := jd.ToEntity()
	if err != nil {
		t.Fatalf("build join entity: %v", err)
	}
	if _, err := base.ap.Continuation().Install(context.Background(), joinPath, je); err != nil {
		t.Fatalf("install join: %v", err)
	}

	return &contShardRig{shardRig: base, joinPath: joinPath, out: out}
}

// tick runs one continuation-managed generation: eval each shard (host trigger),
// advance its join slot; the join fires the stitch once all k land; read the
// stitched grid and write it back to advance the state.
func (r *contShardRig) tick(pattern string) ([]uint64, hash.Hash, error) {
	ctx := context.Background()
	for j, path := range r.shards {
		part, err := r.evalShard(pattern, path)
		if err != nil {
			return nil, hash.Hash{}, fmt.Errorf("shard %d eval: %w", j, err)
		}
		raw, err := ecf.Encode(part)
		if err != nil {
			return nil, hash.Hash{}, fmt.Errorf("encode shard %d: %w", j, err)
		}
		resEnt, err := entity.NewEntity("primitive/any", cbor.RawMessage(raw))
		if err != nil {
			return nil, hash.Hash{}, err
		}
		slot := fmt.Sprintf("%s/shard%d", r.joinPath, j)
		if err := r.ap.Continuation().Advance(ctx, slot, resEnt, 200); err != nil {
			return nil, hash.Hash{}, fmt.Errorf("advance slot %d: %w", j, err)
		}
	}

	select {
	case so := <-r.out:
		if so.err != nil {
			return nil, hash.Hash{}, fmt.Errorf("stitch: %w", so.err)
		}
		ent, err := lifeGridEntity(r.w, r.h, so.cells)
		if err != nil {
			return nil, hash.Hash{}, err
		}
		h, err := r.ap.PutEntity(r.statePath, ent)
		if err != nil {
			return nil, hash.Hash{}, err
		}
		if !hashEq(h, so.hash) {
			return nil, hash.Hash{}, fmt.Errorf("stitched hash %s != put hash %s", so.hash, h)
		}
		return so.cells, h, nil
	case <-time.After(5 * time.Second):
		return nil, hash.Hash{}, fmt.Errorf("join never fired the stitch (5s) — barrier did not complete")
	}
}

// stitchReceived decodes the join's received map and concatenates the k shard
// cell-arrays in index order. received is {shardN: <cbor array of cells>}, the
// map advanceJoinSlot assembled from the slot advances.
func stitchReceived(paramsData []byte, w, h, k int) ([]uint64, error) {
	var recv map[string]cbor.RawMessage
	if err := ecf.Decode(paramsData, &recv); err != nil {
		return nil, fmt.Errorf("decode received map: %w", err)
	}
	cells := make([]uint64, 0, w*h)
	for j := 0; j < k; j++ {
		slot := fmt.Sprintf("shard%d", j)
		raw, ok := recv[slot]
		if !ok {
			return nil, fmt.Errorf("received map missing %s (have %d slots)", slot, len(recv))
		}
		part, err := cellsFromSlot(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", slot, err)
		}
		cells = append(cells, part...)
	}
	if len(cells) != w*h {
		return nil, fmt.Errorf("stitched %d cells, want %d", len(cells), w*h)
	}
	return cells, nil
}

// cellsFromSlot extracts a shard's cell array from whatever a slot advance
// deposited. Two payload shapes reach here depending on the model:
//   - stage 1 (direct slot advance): a bare CBOR array of cells;
//   - stage 2 (deliver_to fork): the eval's result delivered as an encoded
//     entity.Entity{Type: "compute/result", Data: ComputeResultData{Value}} —
//     because deliverToInbox sets InboxDeliveryData.Result = ecf.Encode(resp.Result)
//     (core-go async.go), and advance reads that through the {result,status}
//     field overlap between InboxDeliveryData and ContinuationAdvanceRequestData.
//
// Tried in order so one decoder serves both models.
func cellsFromSlot(raw cbor.RawMessage) ([]uint64, error) {
	// Shape A: an encoded entity wrapping a compute/result.
	var ent entity.Entity
	if err := ecf.Decode(raw, &ent); err == nil && ent.Type == types.TypeComputeResult {
		var rd types.ComputeResultData
		if err := ecf.Decode(ent.Data, &rd); err == nil {
			if arr, ok := rd.Value.([]interface{}); ok {
				return toCells(arr)
			}
		}
	}
	// Shape B: a bare compute/result payload (no entity wrapper).
	var rd types.ComputeResultData
	if err := ecf.Decode(raw, &rd); err == nil && rd.Value != nil {
		if arr, ok := rd.Value.([]interface{}); ok {
			return toCells(arr)
		}
	}
	// Shape C: a bare array of cells.
	var arr []interface{}
	if err := ecf.Decode(raw, &arr); err == nil {
		return toCells(arr)
	}
	return nil, fmt.Errorf("slot payload not a recognized shape (%d bytes)", len(raw))
}

func toCells(arr []interface{}) ([]uint64, error) {
	out := make([]uint64, 0, len(arr))
	for i, v := range arr {
		switch n := v.(type) {
		case uint64:
			out = append(out, n)
		case int64:
			out = append(out, uint64(n))
		default:
			return nil, fmt.Errorf("cell[%d] is %T, want integer", i, v)
		}
	}
	return out, nil
}

func mustEncodeMap(m map[string]interface{}) cbor.RawMessage {
	raw, err := ecf.Encode(m)
	if err != nil {
		panic(err)
	}
	return cbor.RawMessage(raw)
}

// registerStitch registers a Go stitch handler at pat that assembles the join's
// received map into a grid and reports it (grid + hash) over a buffered channel.
// Buffered because the join fires the stitch synchronously inside the final slot
// advance; an unbuffered send would deadlock that advance against the test's
// later receive.
func registerStitch(t testing.TB, ap *entitysdk.AppPeer, pat string, w, h, k int) chan stitchOut {
	t.Helper()
	out := make(chan stitchOut, 1)
	handle, err := ap.RegisterHandler(entitysdk.HandlerSpec{
		Pattern: pat,
		Name:    "life-stitch",
		Operations: map[string]types.HandlerOperationSpec{
			"stitch": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, func(ctx context.Context, req *handler.Request) (*handler.Response, error) {
		cells, err := stitchReceived(req.Params.Data, w, h, k)
		so := stitchOut{cells: cells, err: err}
		if err == nil {
			if ent, e := lifeGridEntity(w, h, cells); e != nil {
				so.err = e
			} else {
				so.hash = ent.ContentHash
			}
		}
		out <- so
		ack, _ := entity.NewEntity("primitive/any", mustEncodeMap(map[string]interface{}{"ack": true}))
		return &handler.Response{Status: 200, Result: ack}, nil
	})
	if err != nil {
		t.Fatalf("register stitch handler: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	return out
}

// TestContShard_JoinEqualsHostManaged — the barrier proof.
//
// The continuation-managed tick (fork → join barrier → stitch dispatch) must
// produce the SAME state hash as the host-managed serial tick, generation by
// generation, on both engines — and each must match the Life oracle (anti-
// vacuity: a dead grid or a divergence from lifeNext fails before any hash
// comparison can pass trivially).
func TestContShard_JoinEqualsHostManaged(t *testing.T) {
	const w, h = 24, 24
	seed := lifeRandCells(w, h, 42)

	for _, pattern := range []string{"system/compute", axis1Pattern} {
		host := newShardRig(t, w, h, 6, seed)
		cont := newContShardRig(t, w, h, 6, seed)

		cur := seed
		for gen := 1; gen <= 3; gen++ {
			want := lifeNext(w, h, cur)
			if lifePop(want) == 0 {
				t.Fatalf("%s gen %d: oracle grid dead — agreement would be vacuous", pattern, gen)
			}

			_, hostHash, err := host.tick(pattern)
			if err != nil {
				t.Fatalf("%s gen %d host-managed tick: %v", pattern, gen, err)
			}
			gotCells, contHash, err := cont.tick(pattern)
			if err != nil {
				t.Fatalf("%s gen %d continuation-managed tick: %v", pattern, gen, err)
			}

			if !cellsEq(gotCells, want) {
				t.Fatalf("%s gen %d: continuation-managed result diverged from the Life oracle", pattern, gen)
			}
			if !hashEq(contHash, hostHash) {
				t.Fatalf("%s gen %d: continuation-managed hash %s != host-managed %s — "+
					"the join is not boundary-equivalent to host stitching",
					pattern, gen, contHash, hostHash)
			}
			cur = want
		}
		t.Logf("%s: continuation-managed join == host-managed serial at the boundary, k=6, 3 generations", pattern)
	}
}

// ---- STAGE 2: the deliver_to fork ------------------------------------------
//
// Stage 1 kept the FORK host-driven: the host evaluated each shard and advanced
// its slot. Stage 2 removes that. Each shard is a standing FORWARD continuation
// whose Target is the eval handler and whose deliver_to routes the eval result
// straight into a join slot. Per tick the host fires k advances (the fork
// TRIGGER only) and the system does the rest: k evals run concurrently on the
// async-dispatch pool, each delivers its result to a slot, the join barrier
// fires the stitch once all k land. The host never touches a shard result.
//
// Two wrinkles this proves out (COMPUTE-PARALLELIZATION-TWO-MODELS §1):
//   - the join must live UNDER system/continuation/ so a deliver_to whose URI is
//     the slot PATH tree-walks to the advance handler (async.go sets
//     resource = {DeliverTo.URI} and dispatches the delivery locally);
//   - the delivered payload is the eval's result wrapped through
//     InboxDeliveryData, reaching advance via the {result,status} field overlap
//     — cellsFromSlot handles the shape.

type contForkRig struct {
	*shardRig
	joinPath string
	fwdPaths []string
	out      chan stitchOut
}

// newContForkRig builds the shards, a stitch handler, a standing join under
// system/continuation/, and k standing forward continuations that eval-then-
// deliver into the join's slots. pattern (the engine) is baked into each
// forward continuation's Target.
func newContForkRig(t testing.TB, w, h, k int, seed []uint64, pattern string) *contForkRig {
	t.Helper()
	base := newShardRig(t, w, h, k, seed)

	stitchPat := fmt.Sprintf("app/life/fork-stitch-k%d", k)
	out := registerStitch(t, base.ap, stitchPat, w, h, k)

	// Under system/continuation/ so slot paths route to the advance handler.
	root := fmt.Sprintf("system/continuation/life-fork-k%d", k)
	joinPath := root + "/join"

	expected := make([]string, k)
	for j := 0; j < k; j++ {
		expected[j] = fmt.Sprintf("shard%d", j)
	}
	jd := types.ContinuationJoinData{
		Expected:  expected,
		Target:    stitchPat,
		Operation: "stitch",
	}
	entitysdk.SetDefaultDispatchCapJoin(base.ap.OwnerCapability().ContentHash, &jd)
	je, err := jd.ToEntity()
	if err != nil {
		t.Fatalf("build join entity: %v", err)
	}
	if _, err := base.ap.Continuation().Install(context.Background(), joinPath, je); err != nil {
		t.Fatalf("install join: %v", err)
	}

	emptyParams := mustEncodeMap(map[string]interface{}{})
	fwdPaths := make([]string, k)
	for j := 0; j < k; j++ {
		slot := fmt.Sprintf("%s/shard%d", joinPath, j)
		cd := types.ContinuationData{
			Target:    pattern,
			Operation: "eval",
			Resource:  &types.ResourceTarget{Targets: []string{base.shards[j]}},
			Params:    emptyParams,
			DeliverTo: &types.DeliverySpec{URI: slot, Operation: "advance"},
		}
		entitysdk.SetDefaultDispatchCap(base.ap.OwnerCapability().ContentHash, &cd)
		ce, err := cd.ToEntity()
		if err != nil {
			t.Fatalf("build forward continuation %d: %v", j, err)
		}
		fwdPaths[j] = fmt.Sprintf("%s/fwd%d", root, j)
		if _, err := base.ap.Continuation().Install(context.Background(), fwdPaths[j], ce); err != nil {
			t.Fatalf("install forward continuation %d: %v", j, err)
		}
	}

	return &contForkRig{shardRig: base, joinPath: joinPath, fwdPaths: fwdPaths, out: out}
}

// tick fires the k forward continuations (the fork trigger) and waits for the
// join to fire the stitch. The host does not evaluate or stitch anything.
func (r *contForkRig) tick() ([]uint64, hash.Hash, error) {
	ctx := context.Background()
	for j, fwd := range r.fwdPaths {
		if err := r.ap.Continuation().Advance(ctx, fwd, entity.Entity{}, 0); err != nil {
			return nil, hash.Hash{}, fmt.Errorf("advance forward %d: %w", j, err)
		}
	}
	select {
	case so := <-r.out:
		if so.err != nil {
			return nil, hash.Hash{}, fmt.Errorf("stitch: %w", so.err)
		}
		ent, err := lifeGridEntity(r.w, r.h, so.cells)
		if err != nil {
			return nil, hash.Hash{}, err
		}
		h, err := r.ap.PutEntity(r.statePath, ent)
		if err != nil {
			return nil, hash.Hash{}, err
		}
		return so.cells, h, nil
	case <-time.After(5 * time.Second):
		// Diagnostic: how many slots actually filled tells fork-vs-barrier apart.
		filled := -1
		if je, found, _ := r.ap.Get(r.joinPath); found {
			if jd, e := types.ContinuationJoinDataFromEntity(je); e == nil {
				filled = len(jd.Received)
			}
		}
		return nil, hash.Hash{}, fmt.Errorf("join never fired the stitch (5s); slots filled: %d/%d", filled, len(r.fwdPaths))
	}
}

// TestContFork_DeliverToEqualsHostManaged — the fork proof. The host fires only
// the k advance triggers; the system evals concurrently, delivers to slots,
// barriers, and stitches. Result must match host-managed serial and the oracle.
func TestContFork_DeliverToEqualsHostManaged(t *testing.T) {
	const w, h = 24, 24
	seed := lifeRandCells(w, h, 42)

	for _, pattern := range []string{"system/compute", axis1Pattern} {
		host := newShardRig(t, w, h, 6, seed)
		fork := newContForkRig(t, w, h, 6, seed, pattern)

		cur := seed
		for gen := 1; gen <= 3; gen++ {
			want := lifeNext(w, h, cur)
			if lifePop(want) == 0 {
				t.Fatalf("%s gen %d: oracle grid dead — agreement would be vacuous", pattern, gen)
			}
			_, hostHash, err := host.tick(pattern)
			if err != nil {
				t.Fatalf("%s gen %d host-managed tick: %v", pattern, gen, err)
			}
			gotCells, forkHash, err := fork.tick()
			if err != nil {
				t.Fatalf("%s gen %d deliver_to fork tick: %v", pattern, gen, err)
			}
			if !cellsEq(gotCells, want) {
				t.Fatalf("%s gen %d: fork result diverged from the Life oracle", pattern, gen)
			}
			if !hashEq(forkHash, hostHash) {
				t.Fatalf("%s gen %d: fork hash %s != host-managed %s", pattern, gen, forkHash, hostHash)
			}
			cur = want
		}
		t.Logf("%s: deliver_to fork == host-managed serial at the boundary, k=6, 3 generations", pattern)
	}
}

// joinSlotsFilled reads the standing join and reports how many slots are
// currently accumulated. -1 if the join can't be read.
func (r *contForkRig) joinSlotsFilled(t testing.TB) int {
	t.Helper()
	je, found, err := r.ap.Get(r.joinPath)
	if err != nil || !found {
		return -1
	}
	jd, err := types.ContinuationJoinDataFromEntity(je)
	if err != nil {
		return -1
	}
	return len(jd.Received)
}

// TestContFork_FailureAsymmetry pins §3.2 as a test rather than prose: the two
// models fail differently, and that difference is the argument for keeping the
// host-managed floor.
//
//   - HOST-MANAGED: a shard that can't evaluate is a synchronous, bounded error
//     the host sees immediately in its own loop.
//   - CONTINUATION-MANAGED: if a slot advance is lost — here, a fork trigger
//     that never fires — the join barrier has NO timeout (confirmed by absence
//     in ext/continuation: only chain-error markers are time-swept, not joins),
//     so it sits partially filled indefinitely. A standing join used per tick is
//     then wedged for every subsequent tick.
func TestContFork_FailureAsymmetry(t *testing.T) {
	const w, h, k = 24, 24, 6
	seed := lifeRandCells(w, h, 42)
	const pattern = axis1Pattern

	// --- Host-managed: bounded, synchronous failure ---------------------------
	host := newShardRig(t, w, h, k, seed)
	start := time.Now()
	_, err := host.evalShard(pattern, "app/life/no-such-shard/step")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("host-managed: expected an error evaluating a missing shard")
	}
	if elapsed > time.Second {
		t.Fatalf("host-managed: failure not bounded (%v) — the floor's whole value is synchronous failure", elapsed)
	}
	t.Logf("host-managed: a broken shard fails synchronously in %v (%v)", elapsed.Round(time.Millisecond), err)

	// --- Continuation-managed: a lost slot advance wedges the barrier ---------
	fork := newContForkRig(t, w, h, k, seed, pattern)

	// Fire only k-1 of the k forks: one shard's trigger is "lost".
	ctx := context.Background()
	for j := 0; j < k-1; j++ {
		if err := fork.ap.Continuation().Advance(ctx, fork.fwdPaths[j], entity.Entity{}, 0); err != nil {
			t.Fatalf("advance forward %d: %v", j, err)
		}
	}

	// The barrier must NOT fire with a slot missing — and nothing reaps it.
	select {
	case <-fork.out:
		t.Fatal("continuation-managed: stitch fired with only k-1 slots — the barrier is broken")
	case <-time.After(2 * time.Second):
		// Expected: no stitch. Give the k-1 async evals time to land first.
	}
	if filled := fork.joinSlotsFilled(t); filled != k-1 {
		t.Fatalf("continuation-managed: expected the join wedged at %d/%d slots, got %d", k-1, k, filled)
	}
	t.Logf("continuation-managed: join wedged at %d/%d slots after 2s, no timeout reaped it — "+
		"a lost slot advance stalls the barrier (and a standing join for every later tick)", k-1, k)
}

// ---- CONCAT-FREE STITCH: are we blocked on core-go? ------------------------
//
// §3.1/§6.2 framed array-concat as the gate on a program-owned (compute) stitch:
// entity-compute has no concat/append/flatten, so the join's target had to be a
// Go handler. This asks whether that is actually true — whether the stitch can be
// expressed in PURE compute with the primitives that already exist.
//
// It can. The flat cells array is `map` over [0..N) where each global index g
// selects from whichever shard fragment covers it — a k-way conditional gather
// (if/compare/index/arithmetic, all present), with the shard boundaries baked in
// as literals since k is known at authoring time. The fragments are read once via
// lookup/tree and captured by the map's closure, so it is O(k) tree reads and
// O(N·k) compares — against O(N) for a hypothetical concat. For realistic k (≤8)
// the whole stitch fits one budget (N·(2k+3) ≪ 100k up to 64×64).
//
// So concat is an OPTIMIZATION (O(N·k) gather → O(N) append), not a GATE: the
// program-owned stitch is buildable today, and this track is NOT blocked on
// core-go for it. Concat is still worth recommending — cleaner authoring, better
// asymptotics, and it removes the "k must be static" constraint — but as an
// improvement, not a dependency.

// fragmentEntity stores a shard's partial cell array as a field-accessible entity
// so the gather stitch can lookup/tree it and field("cells").
func fragmentEntity(cells []uint64) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{"cells": cells})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity("app/life/fragment", cbor.RawMessage(raw))
}

// buildGatherStitch is the concat-free stitch: a compute step that reads k shard
// fragments from tree paths and produces the flat grid via map + conditional
// gather. Boundaries lo_j = j*N/k, hi_j = (j+1)*N/k match newShardRig's split,
// including ragged k that doesn't divide N.
func buildGatherStitch(ap *entitysdk.AppPeer, w, h, k int, fragPaths []string) *entitysdk.Builder {
	c := ap.Compute()
	n := w * h

	// Bind each fragment's cell array once (O(k) tree reads); the map lambda
	// closes over these rather than re-reading per element.
	frag := make(map[string]*entitysdk.Builder, k)
	for j := 0; j < k; j++ {
		frag[fmt.Sprintf("f%d", j)] = c.Field(c.LookupTreeLocal(fragPaths[j]), "cells")
	}

	lo := func(j int) uint64 { return uint64(j * n / k) }
	hi := func(j int) uint64 { return uint64((j + 1) * n / k) }

	// Gather for global index i: the last shard is the else-branch (no bound);
	// each earlier shard j guards with `i < hi_j`.
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

// TestComputeStitch_ConcatFreeGather proves the stitch is expressible in pure
// compute without an array-concat primitive: eval k shards, write their
// fragments, then a compute gather stitch reconstructs the flat grid — hash-
// identical to the unsharded Life step. If this passes, §6.2's concat is an
// optimization, not a blocker, and the program-owned stitch needs nothing from
// core-go.
func TestComputeStitch_ConcatFreeGather(t *testing.T) {
	const w, h, k = 24, 24, 6
	seed := lifeRandCells(w, h, 42)

	for _, pattern := range []string{"system/compute", axis1Pattern} {
		r := newShardRig(t, w, h, k, seed)

		// Fork (host, for this proof) → fragments on the tree.
		fragPaths := make([]string, k)
		for j, sp := range r.shards {
			part, err := r.evalShard(pattern, sp)
			if err != nil {
				t.Fatalf("%s shard %d eval: %v", pattern, j, err)
			}
			fe, err := fragmentEntity(part)
			if err != nil {
				t.Fatalf("fragment %d: %v", j, err)
			}
			fragPaths[j] = fmt.Sprintf("app/life/gather/frag%d", j)
			if _, err := r.ap.PutEntity(fragPaths[j], fe); err != nil {
				t.Fatalf("put fragment %d: %v", j, err)
			}
		}

		// The concat-free stitch, in pure compute.
		stitchPath := "app/life/gather/stitch"
		if _, err := buildGatherStitch(r.ap, w, h, k, fragPaths).Build(context.Background(), stitchPath); err != nil {
			t.Fatalf("%s build gather stitch: %v", pattern, err)
		}
		req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := r.ap.Executor().ExecuteOnResource(pattern, "eval", req,
			&types.ResourceTarget{Targets: []string{stitchPath}})
		if err != nil {
			t.Fatalf("%s eval gather stitch: %v", pattern, err)
		}
		if resp.Status != 200 || resp.Type != lifeGridType {
			t.Fatalf("%s gather stitch: status=%d type=%s (want 200/%s)", pattern, resp.Status, resp.Type, lifeGridType)
		}

		// Boundary-equivalent to the unsharded Life step?
		gotEnt, err := entity.NewEntity(lifeGridType, resp.Data)
		if err != nil {
			t.Fatal(err)
		}
		wantEnt, err := lifeGridEntity(w, h, lifeNext(w, h, seed))
		if err != nil {
			t.Fatal(err)
		}
		if !hashEq(gotEnt.ContentHash, wantEnt.ContentHash) {
			t.Fatalf("%s: concat-free gather stitch hash %s != unsharded grid %s — not boundary-equivalent",
				pattern, gotEnt.ContentHash, wantEnt.ContentHash)
		}
		t.Logf("%s: concat-free compute stitch == unsharded grid — program-owned stitch needs no core-go concat", pattern)
	}
}
