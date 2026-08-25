package entitysdk_test

// EXPERIMENT-SNAKE (Exp-E) — Snake as a hostable compute program: the
// first program with an INPUT PORT, completing the four-seam contract
// (state + step + tick + output) with the input seam Life lacked.
//
// Arch context (entity-system-architecture):
//   docs/research/explorations/EXPLORATION-COMPUTE-PROGRAM-RUNTIME-CONTRACT.md
//     §6.2  the Snake sketch this implements (grid-of-timers state, LCG
//           food RNG, snapshot input port, 180°-reversal guard)
//     §8    snapshot port semantics (tree path, last-write-wins)
//     §10.3 the closing invariant: all non-determinism enters via input
//           ports recorded in the input stream; the sim is a pure function
//           of (state, input-stream) — E5 replays a full game to prove it.
//   docs/proposals/PROPOSAL-APP-CONVENTION-COMPUTE-PROGRAM.md §3 (port kinds)
//
// The input port, descriptor-faithfully:
//   - a tree path ("app/snake/.../input") holding {dir: 0..3};
//     kind=snapshot: the HOST writes it (PutEntity, last-write-wins), the
//     step reads it via lookup/tree, sampled once at the tick boundary.
//   - input here is SIMULATED (scripted writes) — the point is the port
//     mechanics and the interface contract, not device wiring; a real
//     driver would do the identical PutEntity from a key handler.
//
// Step encoding notes (state fields all uint64; dir 0=up 1=right 2=down
// 3=left; status 0=playing 1=dead; cells = grid of timers, tail=1..head=len):
//   - movement deltas via nested if (bool is not numeric — §6.2 finding 5);
//     negative deltas make nx/ny signed; wall test is signed lt/gte.
//   - floor-div y = div(sub(head, x), W) — the F-D1 lowering rule (Exp-D).
//   - hit_self = if(hit_wall, false, gt(index(cells, nhead), …)) — the
//     index MUST be guarded behind the lazy `if`: error-as-value means an
//     unguarded out-of-range index poisons the whole eval (lowering rule:
//     partial ops go in the branch that establishes their precondition).
//   - the food filter lives INSIDE if(ate, …): let-bindings are eager, if
//     branches are lazy — expensive subexpressions used on rare ticks
//     belong in the branch, not a let (lowering rule; saves ~12 ops/cell
//     on every non-eat tick).
//   - the frozen-when-dead tick returns `s` (the looked-up state entity)
//     verbatim — same entity, same hash, visible in E4.
//
// Findings fed forward (write-up follows this experiment):
//   F-E1 (runtime contract): a lookup/tree on an unseeded input port is a
//        compute/error — PORT INITIALIZATION IS THE RUNTIME'S JOB before
//        the first tick (descriptor follow-on: seed value or default field).
//   F-E2 (lowering rules): the two laziness rules above (guarded partial
//        ops; branch-local lets) — both consequences of error-as-value +
//        eager let, and both belong in the lowering toolkit alongside F-D1.
//
// MEASURED — 2026-07-15, i5-11400, in-memory store, no -race: full tick
// (input write + eval + decode + put) on 8x8 = 3.5ms → 286 ticks/s. A
// playable Snake needs ~8: the EXPLORATION §6.2 "simple games are
// comfortably realtime on the Stage-1 interpreter" calibration holds
// with ~35x headroom.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

// --- state shape -----------------------------------------------------------

const (
	snakeStateType = "app/snake/state"
	snakeInputType = "app/snake/input"

	snakeUp    = uint64(0)
	snakeRight = uint64(1)
	snakeDown  = uint64(2)
	snakeLeft  = uint64(3)

	snakeLCGMulC = uint64(1103515245)
	snakeLCGAddC = uint64(12345)
	snakeLCGModC = uint64(2147483648)
)

type snakeState struct {
	Width  uint64   `cbor:"width"`
	Height uint64   `cbor:"height"`
	Cells  []uint64 `cbor:"cells"`
	Head   uint64   `cbor:"head"`
	Dir    uint64   `cbor:"dir"`
	Food   uint64   `cbor:"food"`
	Length uint64   `cbor:"length"`
	RNG    uint64   `cbor:"rng"`
	Status uint64   `cbor:"status"`
}

func snakeStateEntity(s snakeState) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{
		"width": s.Width, "height": s.Height, "cells": s.Cells,
		"head": s.Head, "dir": s.Dir, "food": s.Food,
		"length": s.Length, "rng": s.RNG, "status": s.Status,
	})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(snakeStateType, cbor.RawMessage(raw))
}

// snakeSeed builds a horizontal length-3 snake heading right:
// tail…head at (headX-2..headX, headY), food wherever the caller says.
func snakeSeed(w, h, headX, headY int, food uint64, rng uint64) snakeState {
	cells := make([]uint64, w*h)
	head := headY*w + headX
	cells[head] = 3
	cells[head-1] = 2
	cells[head-2] = 1
	return snakeState{
		Width: uint64(w), Height: uint64(h), Cells: cells,
		Head: uint64(head), Dir: snakeRight, Food: food,
		Length: 3, RNG: rng, Status: 0,
	}
}

// --- Go-side oracle ----------------------------------------------------------

// snakeNext mirrors the compute step EXACTLY (same branch structure, same
// LCG, same free-cell selection) — any divergence is a step bug.
func snakeNext(s snakeState, inp uint64) snakeState {
	if s.Status == 1 {
		return s
	}
	w, h := int(s.Width), int(s.Height)

	ndir := inp
	if inp == (s.Dir+2)%4 { // 180° reversal guard
		ndir = s.Dir
	}
	dx, dy := 0, 0
	switch ndir {
	case snakeRight:
		dx = 1
	case snakeLeft:
		dx = -1
	case snakeDown:
		dy = 1
	case snakeUp:
		dy = -1
	}
	hx, hy := int(s.Head)%w, int(s.Head)/w
	nx, ny := hx+dx, hy+dy
	hitWall := nx < 0 || nx >= w || ny < 0 || ny >= h
	nhead := ny*w + nx
	// mirrors the step exactly: ate is bit-equality on nhead vs food,
	// computed unconditionally (a negative nhead wraps and never matches).
	ate := uint64(nhead) == s.Food
	hitSelf := false
	if !hitWall {
		thr := uint64(1) // tail vacates when not eating
		if ate {
			thr = 0
		}
		hitSelf = s.Cells[nhead] > thr
	}
	if hitWall || hitSelf {
		out := s
		out.Dir = ndir
		out.Status = 1
		return out
	}

	nlen := s.Length
	if ate {
		nlen++
	}
	ncells := make([]uint64, len(s.Cells))
	for i, v := range s.Cells {
		switch {
		case i == nhead:
			ncells[i] = nlen
		case ate:
			ncells[i] = v
		case v > 0:
			ncells[i] = v - 1
		}
	}
	nrng := (s.RNG*snakeLCGMulC + snakeLCGAddC) % snakeLCGModC
	nfood := s.Food
	if ate {
		free := make([]uint64, 0, len(ncells))
		for i, v := range ncells {
			if v == 0 && i != nhead {
				free = append(free, uint64(i))
			}
		}
		nfood = free[nrng%uint64(len(free))]
	}
	return snakeState{
		Width: s.Width, Height: s.Height, Cells: ncells,
		Head: uint64(nhead), Dir: ndir, Food: nfood,
		Length: nlen, RNG: nrng, Status: 0,
	}
}

func snakeASCII(s snakeState) string {
	w, h := int(s.Width), int(s.Height)
	var sb strings.Builder
	sb.WriteByte('\n')
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			switch {
			case uint64(i) == s.Head && s.Cells[i] > 0:
				sb.WriteString("@")
			case s.Cells[i] > 0:
				sb.WriteString("o")
			case uint64(i) == s.Food:
				sb.WriteString("*")
			default:
				sb.WriteString("·")
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// --- the step lowering --------------------------------------------------------

// buildSnakeStep lowers §6.2's step with existing primitives only.
// Nested Lets form dependency layers; within one Let, bindings evaluate
// sequentially in SORTED name order (let*), so intra-layer names are
// chosen to sort in dependency order where needed.
func buildSnakeStep(ap *entitysdk.AppPeer, w, h int, statePath, inputPath string) *entitysdk.Builder {
	c := ap.Compute()
	W, H := uint64(w), uint64(h)
	n := w * h
	indices := make([]uint64, n)
	for i := range indices {
		indices[i] = uint64(i)
	}
	sc := c.LookupScope // brevity

	// per-cell timer update: λi. if i==nhead → nlen; else if ate → keep;
	// else decrement toward 0. (v bound once — three reads otherwise.)
	cellRule := c.Lambda([]string{"i"},
		c.If(c.Compare("eq", sc("i"), sc("nhead")),
			sc("nlen"),
			c.Let(map[string]*entitysdk.Builder{
				"v": c.Index(sc("cells"), sc("i")),
			}, c.If(sc("ate"),
				sc("v"),
				c.If(c.Compare("gt", sc("v"), c.Literal(uint64(0))),
					c.Arithmetic("sub", sc("v"), c.Literal(uint64(1))),
					c.Literal(uint64(0)))))))

	// food placement, only evaluated on eat ticks (lazy if-branch let —
	// F-E2): free = filter(indices, cell empty && not new head), pick
	// free[nrng % len(free)].
	freeFilter := c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
		"collection": c.Literal(indices),
		"fn": c.Lambda([]string{"i"},
			c.Logic("and",
				c.Compare("eq", c.Index(sc("ncells"), sc("i")), c.Literal(uint64(0))),
				c.Compare("neq", sc("i"), sc("nhead")))),
	})
	pickFood := c.Let(map[string]*entitysdk.Builder{
		"freec": freeFilter,
	}, c.Index(sc("freec"), c.Arithmetic("mod", sc("nrng"), c.Length(sc("freec")))))

	aliveConstruct := c.Construct(snakeStateType, map[string]*entitysdk.Builder{
		"width": c.Literal(W), "height": c.Literal(H),
		"cells": sc("ncells"), "head": sc("nhead"), "dir": sc("ndir"),
		"food": sc("nfood"), "length": sc("nlen"), "rng": sc("nrng"),
		"status": c.Literal(uint64(0)),
	})

	aliveExpr := c.Let(map[string]*entitysdk.Builder{
		"nlen": c.If(sc("ate"),
			c.Arithmetic("add", sc("leng"), c.Literal(uint64(1))),
			sc("leng")),
	}, c.Let(map[string]*entitysdk.Builder{
		"ncells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         cellRule,
		}),
		"nrng": c.Arithmetic("mod",
			c.Arithmetic("add",
				c.Arithmetic("mul", sc("rng"), c.Literal(snakeLCGMulC)),
				c.Literal(snakeLCGAddC)),
			c.Literal(snakeLCGModC)),
	}, c.Let(map[string]*entitysdk.Builder{
		"nfood": c.If(sc("ate"), pickFood, sc("food")),
	}, aliveConstruct)))

	deadConstruct := c.Construct(snakeStateType, map[string]*entitysdk.Builder{
		"width": c.Literal(W), "height": c.Literal(H),
		"cells": sc("cells"), "head": sc("head"), "dir": sc("ndir"),
		"food": sc("food"), "length": sc("leng"), "rng": sc("rng"),
		"status": c.Literal(uint64(1)),
	})

	// movement: deltas via nested if; y via F-D1 floor-div; wall test
	// signed; nhead/self-collision guarded behind the wall check (F-E2).
	movement :=
		c.Let(map[string]*entitysdk.Builder{
			"ndir": c.If(
				c.Compare("eq", sc("inp"),
					c.Arithmetic("mod",
						c.Arithmetic("add", sc("dirv"), c.Literal(uint64(2))),
						c.Literal(uint64(4)))),
				sc("dirv"),
				sc("inp")),
		}, c.Let(map[string]*entitysdk.Builder{
			// sorted order: dx < dy < hx < hy — hy references hx (let*).
			"dx": c.If(c.Compare("eq", sc("ndir"), c.Literal(snakeRight)), c.Literal(int64(1)),
				c.If(c.Compare("eq", sc("ndir"), c.Literal(snakeLeft)), c.Literal(int64(-1)),
					c.Literal(int64(0)))),
			"dy": c.If(c.Compare("eq", sc("ndir"), c.Literal(snakeDown)), c.Literal(int64(1)),
				c.If(c.Compare("eq", sc("ndir"), c.Literal(snakeUp)), c.Literal(int64(-1)),
					c.Literal(int64(0)))),
			"hx": c.Arithmetic("mod", sc("head"), c.Literal(W)),
			"hy": c.Arithmetic("div",
				c.Arithmetic("sub", sc("head"), sc("hx")),
				c.Literal(W)),
		}, c.Let(map[string]*entitysdk.Builder{
			"nx": c.Arithmetic("add", sc("hx"), sc("dx")),
			"ny": c.Arithmetic("add", sc("hy"), sc("dy")),
		}, c.Let(map[string]*entitysdk.Builder{
			// sorted order: nhead < wall — independent.
			"nhead": c.Arithmetic("add",
				c.Arithmetic("mul", sc("ny"), c.Literal(W)),
				sc("nx")),
			"wall": c.Logic("or",
				c.Logic("or",
					c.Compare("lt", sc("nx"), c.Literal(int64(0))),
					c.Compare("gte", sc("nx"), c.Literal(W))),
				c.Logic("or",
					c.Compare("lt", sc("ny"), c.Literal(int64(0))),
					c.Compare("gte", sc("ny"), c.Literal(H)))),
		}, c.Let(map[string]*entitysdk.Builder{
			// sorted order: ate < hits — hits references ate (let*).
			// ate is computed unconditionally (like the sketch); on a wall
			// hit it may be garbage-true but the death branch never reads it.
			"ate": c.Compare("eq", sc("nhead"), sc("food")),
			"hits": c.If(sc("wall"),
				c.Literal(false),
				c.Compare("gt",
					c.Index(sc("cells"), sc("nhead")),
					c.If(sc("ate"), c.Literal(uint64(0)), c.Literal(uint64(1))))),
		}, c.If(c.Logic("or", sc("wall"), sc("hits")),
			deadConstruct,
			aliveExpr))))))

	// layer 2: unpack state fields + sample the input port (snapshot
	// read, once per tick); layer 1: the two impure lookups' root.
	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"cells":  c.Field(sc("s"), "cells"),
		"dirv":   c.Field(sc("s"), "dir"),
		"food":   c.Field(sc("s"), "food"),
		"head":   c.Field(sc("s"), "head"),
		"inp":    c.Field(c.LookupTreeLocal(inputPath), "dir"),
		"leng":   c.Field(sc("s"), "length"),
		"rng":    c.Field(sc("s"), "rng"),
		"status": c.Field(sc("s"), "status"),
	}, c.If(c.Compare("eq", sc("status"), c.Literal(uint64(1))),
		sc("s"), // frozen when dead: the state entity itself, unchanged
		movement)))
}

// --- the host runtime ---------------------------------------------------------

type snakeProgram struct {
	ap        *entitysdk.AppPeer
	statePath string
	inputPath string
	stepPath  string
}

func snakeSetup(t testing.TB, ap *entitysdk.AppPeer, root string, seed snakeState) *snakeProgram {
	t.Helper()
	p := &snakeProgram{ap: ap,
		statePath: root + "/state",
		inputPath: root + "/input",
		stepPath:  root + "/step"}

	ent, err := snakeStateEntity(seed)
	if err != nil {
		t.Fatalf("seed state entity: %v", err)
	}
	if _, err := ap.PutEntity(p.statePath, ent); err != nil {
		t.Fatalf("put seed state: %v", err)
	}
	// F-E1: the runtime MUST initialize input ports before the first tick
	// (an unseeded port read is a compute/error). Seed with current dir.
	p.writeInput(t, seed.Dir)
	if _, err := buildSnakeStep(ap, int(seed.Width), int(seed.Height),
		p.statePath, p.inputPath).Build(context.Background(), p.stepPath); err != nil {
		t.Fatalf("build step: %v", err)
	}
	return p
}

// writeInput is the (simulated) input driver: one PutEntity at the
// declared port path — last-write-wins snapshot semantics.
func (p *snakeProgram) writeInput(t testing.TB, dir uint64) {
	t.Helper()
	raw, err := ecf.Encode(map[string]interface{}{"dir": dir})
	if err != nil {
		t.Fatalf("encode input: %v", err)
	}
	ent, err := entity.NewEntity(snakeInputType, cbor.RawMessage(raw))
	if err != nil {
		t.Fatalf("input entity: %v", err)
	}
	if _, err := p.ap.PutEntity(p.inputPath, ent); err != nil {
		t.Fatalf("put input: %v", err)
	}
}

func (p *snakeProgram) tick() (*snakeState, hash.Hash, error) {
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return nil, hash.Hash{}, err
	}
	resp, err := p.ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{p.stepPath}})
	if err != nil {
		return nil, hash.Hash{}, fmt.Errorf("eval dispatch: %w", err)
	}
	if resp.Status != 200 {
		return nil, hash.Hash{}, fmt.Errorf("eval status %d (type=%s)", resp.Status, resp.Type)
	}
	if resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		return nil, hash.Hash{}, fmt.Errorf("compute/error code=%s message=%q", ed.Code, ed.Message)
	}
	if resp.Type != snakeStateType {
		return nil, hash.Hash{}, fmt.Errorf("expected %s result, got type=%s", snakeStateType, resp.Type)
	}
	var s snakeState
	if err := ecf.Decode(resp.Data, &s); err != nil {
		return nil, hash.Hash{}, fmt.Errorf("decode state: %w", err)
	}
	ent, err := entity.NewEntity(snakeStateType, cbor.RawMessage(resp.Data))
	if err != nil {
		return nil, hash.Hash{}, err
	}
	h, err := p.ap.PutEntity(p.statePath, ent)
	if err != nil {
		return nil, hash.Hash{}, fmt.Errorf("put state: %w", err)
	}
	return &s, h, nil
}

// assertStateMatches compares a compute tick result against the oracle,
// field by field, plus the materialized-boundary hash check.
func assertSnakeState(t *testing.T, gen int, got *snakeState, gotHash hash.Hash, want snakeState) {
	t.Helper()
	if got.Head != want.Head || got.Dir != want.Dir || got.Food != want.Food ||
		got.Length != want.Length || got.RNG != want.RNG || got.Status != want.Status ||
		!cellsEq(got.Cells, want.Cells) {
		t.Fatalf("tick %d: compute diverged from oracle\ngot:  %+v\nwant: %+v", gen, *got, want)
	}
	wantEnt, err := snakeStateEntity(want)
	if err != nil {
		t.Fatal(err)
	}
	if !hashEq(gotHash, wantEnt.ContentHash) {
		t.Fatalf("tick %d: constructed state hash != hand-built oracle hash", gen)
	}
}

// --- E1: movement + timers ----------------------------------------------------

func TestExpSnakeE1_MovesAndTimers(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	seed := snakeSeed(8, 8, 3, 4, uint64(2*8+6), 99) // food far away
	p := snakeSetup(t, ap, "app/snake/e1", seed)
	t.Logf("tick 0:%s", snakeASCII(seed))

	cur := seed
	for gen := 1; gen <= 3; gen++ {
		want := snakeNext(cur, cur.Dir) // input unchanged
		got, gh, err := p.tick()
		if err != nil {
			t.Fatalf("tick %d: %v", gen, err)
		}
		t.Logf("tick %d:%s", gen, snakeASCII(*got))
		assertSnakeState(t, gen, got, gh, want)
		cur = want
	}
	t.Logf("PASS E1: snake advances right, tail vacates (timers), state hash == hand-built every tick")
}

// --- E2: the input port (steering, last-write-wins, reversal guard) ------------

func TestExpSnakeE2_InputPort(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	seed := snakeSeed(8, 8, 3, 4, uint64(0), 99) // food in a corner, out of the way
	p := snakeSetup(t, ap, "app/snake/e2", seed)
	cur := seed

	step := func(gen int, inp uint64) {
		t.Helper()
		want := snakeNext(cur, inp)
		got, gh, err := p.tick()
		if err != nil {
			t.Fatalf("tick %d: %v", gen, err)
		}
		t.Logf("tick %d (inp=%d):%s", gen, inp, snakeASCII(*got))
		assertSnakeState(t, gen, got, gh, want)
		cur = want
	}

	// (a) steer down: one write, one tick.
	p.writeInput(t, snakeDown)
	step(1, snakeDown)
	if cur.Dir != snakeDown {
		t.Fatalf("expected dir=down after steer, got %d", cur.Dir)
	}

	// (b) last-write-wins: two writes between ticks; only the second is
	// sampled (snapshot-port semantics — §8; a dropped key is CORRECT here).
	p.writeInput(t, snakeLeft)
	p.writeInput(t, snakeRight)
	step(2, snakeRight)
	if cur.Dir != snakeRight {
		t.Fatalf("expected dir=right (last write wins), got %d", cur.Dir)
	}

	// (c) 180° reversal guard: heading right, press left — ignored, keeps right.
	p.writeInput(t, snakeLeft)
	step(3, snakeLeft)
	if cur.Dir != snakeRight {
		t.Fatalf("expected reversal to be ignored (dir stays right), got %d", cur.Dir)
	}
	t.Logf("PASS E2: snapshot input port — steer, last-write-wins, reversal guard all oracle-exact")
}

// --- E3: eating (deterministic RNG food placement) ------------------------------

func TestExpSnakeE3_EatsAndGrows(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	// food directly in the path: head (3,4) heading right, food at (5,4).
	seed := snakeSeed(8, 8, 3, 4, uint64(4*8+5), 4242)
	p := snakeSetup(t, ap, "app/snake/e3", seed)
	t.Logf("tick 0:%s", snakeASCII(seed))

	cur := seed
	for gen := 1; gen <= 3; gen++ { // tick 2 eats
		want := snakeNext(cur, cur.Dir)
		got, gh, err := p.tick()
		if err != nil {
			t.Fatalf("tick %d: %v", gen, err)
		}
		t.Logf("tick %d:%s", gen, snakeASCII(*got))
		assertSnakeState(t, gen, got, gh, want)
		cur = want
	}
	if cur.Length != 4 {
		t.Fatalf("expected length 4 after eating, got %d", cur.Length)
	}
	if cur.Food == seed.Food {
		t.Fatalf("food should have been re-placed by the LCG")
	}
	t.Logf("PASS E3: eat grows to length 4; LCG food re-placement matches oracle exactly (food=%d rng=%d)", cur.Food, cur.RNG)
}

// --- E4: death + frozen state ----------------------------------------------------

func TestExpSnakeE4_DiesAtWall(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	// head at (6,4) heading right on 8-wide board: tick 1 → (7,4), tick 2 → wall.
	seed := snakeSeed(8, 8, 6, 4, uint64(0), 7)
	p := snakeSetup(t, ap, "app/snake/e4", seed)

	cur := seed
	var lastHash hash.Hash
	for gen := 1; gen <= 3; gen++ { // tick 2 dies; tick 3 must be frozen
		want := snakeNext(cur, cur.Dir)
		got, gh, err := p.tick()
		if err != nil {
			t.Fatalf("tick %d: %v", gen, err)
		}
		assertSnakeState(t, gen, got, gh, want)
		cur = want
		if gen == 2 {
			if cur.Status != 1 {
				t.Fatalf("expected death at the wall on tick 2, status=%d", cur.Status)
			}
			lastHash = gh
		}
		if gen == 3 {
			// frozen-when-dead: the step returns the state entity verbatim —
			// identical content hash, tick after tick.
			if !hashEq(gh, lastHash) {
				t.Fatalf("dead state should be frozen (same hash); it changed")
			}
		}
	}
	t.Logf("PASS E4: wall death on tick 2; dead state frozen (identical state hash on tick 3)")
}

// --- E5: replay determinism (the §10.3 invariant) --------------------------------

// TestExpSnakeE5_ReplayDeterminism records a full scripted game — turns
// and at least one eat — as (seed, input-stream), then replays it from
// the seed and asserts the ENTIRE state-hash sequence is identical.
// This is §10.3 live: the input stream is the complete record of all
// non-determinism, so the sim is a pure function of (state₀, inputs) —
// which is why Doom-style demo replay, save-state, and lockstep netcode
// fall out of the architecture for free.
func TestExpSnakeE5_ReplayDeterminism(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	seed := snakeSeed(8, 8, 3, 4, uint64(4*8+5), 31337) // food in the path
	script := []uint64{
		snakeRight, snakeRight, // eat at tick 2
		snakeDown, snakeDown,
		snakeLeft, snakeLeft, snakeLeft,
		snakeUp, snakeUp,
		snakeRight,
	}

	run := func(root string) []hash.Hash {
		p := snakeSetup(t, ap, root, seed)
		hashes := make([]hash.Hash, 0, len(script))
		cur := seed
		for gen, inp := range script {
			p.writeInput(t, inp)
			want := snakeNext(cur, inp)
			got, gh, err := p.tick()
			if err != nil {
				t.Fatalf("%s tick %d: %v", root, gen+1, err)
			}
			assertSnakeState(t, gen+1, got, gh, want)
			hashes = append(hashes, gh)
			cur = want
		}
		if cur.Length != 4 {
			t.Fatalf("script should include exactly one eat (length 4), got %d", cur.Length)
		}
		return hashes
	}

	first := run("app/snake/e5-run1")
	second := run("app/snake/e5-run2")
	for i := range first {
		if !hashEq(first[i], second[i]) {
			t.Fatalf("replay diverged at tick %d — determinism over (state₀, input-stream) broken", i+1)
		}
	}
	t.Logf("PASS E5: %d-tick game (with an eat) replays to an identical state-hash sequence — the sim is a pure function of (state₀, input-stream)", len(script))
}

// --- benchmark: the realtime claim -------------------------------------------

// BenchmarkExpSnakeTick measures the full tick (input write + eval +
// decode + put) on an 8x8 board — the EXPLORATION §6.2 calibration
// claimed simple games are "comfortably realtime on the Stage-1
// interpreter"; this is that claim's number. Honest only without -race:
//
//	make go ARGS="test ./entitysdk -run XXXNONE -bench BenchmarkExpSnakeTick -benchtime 100x"
func BenchmarkExpSnakeTick(b *testing.B) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = ap.Close() }()
	// food unreachable in a corner; snake circles a 4-cell loop forever.
	seed := snakeSeed(8, 8, 3, 4, uint64(63), 7)
	p := snakeSetup(b, ap, "app/snake/bench", seed)
	loop := []uint64{snakeDown, snakeLeft, snakeUp, snakeRight}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.writeInput(b, loop[i%4])
		if _, _, err := p.tick(); err != nil {
			b.Fatalf("tick %d: %v", i, err)
		}
	}
	b.StopTimer()
	if secs := b.Elapsed().Seconds(); secs > 0 {
		b.ReportMetric(float64(b.N)/secs, "ticks/s")
	}
}
