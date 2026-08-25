package workbench

// LifeGameModel — the renderer-neutral model for Conway's Game of Life
// as a hostable compute program, and the second product consumer of the
// compute-program runtime contract (arch:
// EXPLORATION-COMPUTE-PROGRAM-RUNTIME-CONTRACT §6,
// PROPOSAL-APP-CONVENTION-COMPUTE-PROGRAM).
//
// Same three-layer split as SnakeGameModel (program_snake.go), with one
// instructive difference: Life has NO input port. It is a closed system —
// state₀ plus the step is the whole program, so the runtime here seeds
// one port instead of two and the renderer is display-only. That makes
// the pair a useful contrast for the descriptor's port-role hints: the
// input-port machinery is a per-program property, not a runtime tax.
//
//   - the PROGRAM is a pure compute step expression (grid → grid') living
//     in the tree, plus state at a tree path — built by buildLifeStepExpr.
//   - the RUNTIME is this model: it seeds state₀, clocks the tick
//     (host-owned time, §4), evals, and writes the materialized grid back.
//   - the RENDERER (Avalonia panel, …) only consumes Render() — thin I/O.
//
// The step expression is the product copy of the Exp-D probe's `arith`
// lowering (entitysdk/exp_compute_life_test.go — the frozen experiment
// record, which also carries the blinker/glider oracle checks, the
// arith-vs-table equivalence proof, and the budget-cliff map; lowering
// rule F-D1 is annotated there and in
// docs/architecture/reviews/COMPUTE-PROGRAM-POC-FINDINGS-2026-07-15.md).
// Change them together.
//
// Sizing (Exp-D measured, i5-11400, no -race): arith runs 36 gens/s at
// 16x16 and the DefaultMaxOps=100k budget caps arith at 24x24. 16x16 at
// 6 ticks/s therefore has ~6x headroom. The `table` lowering is NOT the
// product choice: it is ops-cheaper (one budget rung further) but
// wall-slower above ~8x8 because of F-D2.
//
// Tick loop = host-clocked explicit eval + tree put (NEVER a reactive
// install on the state path — the §4 cascade rule).

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

const (
	lifeGridType = "app/life/grid"

	// 16x16: comfortably under the arith budget cliff (24x24) with ~6x
	// wall-time headroom at 6 ticks/s. One source of truth — the authoring
	// step and the legacy model must instantiate the same program.
	lifeWidth  = 16
	lifeHeight = 16

	lifeLCGMul = uint64(1103515245)
	lifeLCGAdd = uint64(12345)
	lifeLCGMod = uint64(2147483648)
)

// Life run-stop reasons — why the clock stopped on its own.
const (
	LifeRunning    = uint64(0) // still evolving
	LifeExtinct    = uint64(1) // population hit 0
	LifeStillLife  = uint64(2) // grid identical to the previous generation
)

// LifeGameOutput is the renderer contract: everything a display driver
// needs to draw one frame, nothing else. Cells is the grid (0 = dead,
// 1 = alive), row-major.
type LifeGameOutput struct {
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	Cells      []uint64 `json:"cells"`
	Population uint64   `json:"population"`
	Status     uint64   `json:"status"` // LifeRunning / LifeExtinct / LifeStillLife
	Running    bool     `json:"running"`
	Generation uint64   `json:"generation"`
	Err        string   `json:"err,omitempty"`
}

// lifeWireState is the CBOR shape of the state entity at the state path
// (bare — construct materializes exactly this; v3.19c).
type lifeWireState struct {
	Width  uint64   `cbor:"width"`
	Height uint64   `cbor:"height"`
	Cells  []uint64 `cbor:"cells"`
}

// LifeGameModel drives one Life program instance on one peer.
type LifeGameModel struct {
	ap        *entitysdk.AppPeer
	w, h      int
	statePath string
	stepPath  string

	mu         sync.Mutex
	last       lifeWireState
	running    bool
	generation uint64
	status     uint64
	lastErr    string
	rngSeed    uint64
	listeners  []func()

	stopCh chan struct{} // non-nil while the tick goroutine runs
	doneCh chan struct{}

	tickInterval time.Duration
}

// NewLifeGameModel seeds the program (state₀ + step expression) under
// root and returns the stopped model. rngSeed picks the starting grid;
// pass 0 for a clock-derived seed. The seed is the one place
// non-determinism enters — it is recorded in state₀, so a run is still
// a pure function of state₀ (and, with no input port, of nothing else).
func NewLifeGameModel(ap *entitysdk.AppPeer, root string, rngSeed uint64) (*LifeGameModel, error) {
	if ap == nil {
		return nil, fmt.Errorf("LifeGameModel: nil AppPeer")
	}
	if rngSeed == 0 {
		rngSeed = uint64(time.Now().UnixNano()) % lifeLCGMod
	}
	const w, h = lifeWidth, lifeHeight
	m := &LifeGameModel{
		ap: ap, w: w, h: h,
		statePath:    root + "/state",
		stepPath:     root + "/step",
		rngSeed:      rngSeed,
		tickInterval: time.Second / 6, // rate_hint: 6 ticks/s
	}
	if err := m.seed(); err != nil {
		return nil, err
	}
	if _, err := buildLifeStepExpr(ap, w, h, m.statePath).
		Build(context.Background(), m.stepPath); err != nil {
		return nil, fmt.Errorf("LifeGameModel: build step: %w", err)
	}
	return m, nil
}

// seed writes state₀. Unlike Snake there is no input port to initialize
// (F-E1 applies per-port; Life simply has none).
func (m *LifeGameModel) seed() error {
	s := lifeSeedState(m.w, m.h, m.rngSeed)
	ent, err := lifeStateEntity(s)
	if err != nil {
		return fmt.Errorf("LifeGameModel: seed state: %w", err)
	}
	if _, err := m.ap.PutEntity(m.statePath, ent); err != nil {
		return fmt.Errorf("LifeGameModel: put seed state: %w", err)
	}
	m.mu.Lock()
	m.last = s
	m.generation = 0
	m.status = LifeRunning
	m.lastErr = ""
	m.mu.Unlock()
	return nil
}

// lifeSeedState: a deterministic pseudo-random soup at ~37% density.
//
// The density test reads the LCG's HIGH bits ((s>>16)%8), not its low
// ones. This is load-bearing, not style: a power-of-two-modulus LCG has
// period 2^k in its low k bits, so `s%8` cycles with period 8 in the
// cell index — which on a width that 8 divides (16 here) makes every row
// identical, i.e. vertical stripes rather than a soup. Those stripes are
// a near-instant extinction on a torus (measured: gen 6, from a 96-cell
// start), so the panel would open on a board that dies before you can
// look at it. The Exp-D probe's lifeRandCells has the low-bit form; see
// the findings doc — its perf/budget numbers are unaffected (op count is
// pattern-independent) but its D3 seed is degenerate.
func lifeSeedState(w, h int, rngSeed uint64) lifeWireState {
	cells := make([]uint64, w*h)
	s := rngSeed
	for i := range cells {
		s = (s*lifeLCGMul + lifeLCGAdd) % lifeLCGMod
		if (s>>16)%8 < 3 {
			cells[i] = 1
		}
	}
	return lifeWireState{Width: uint64(w), Height: uint64(h), Cells: cells}
}

func lifeStateEntity(s lifeWireState) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{
		"width": s.Width, "height": s.Height, "cells": s.Cells,
	})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(lifeGridType, cbor.RawMessage(raw))
}

// Start clocks the tick (clock-driven mode). No-op if already running.
func (m *LifeGameModel) Start() {
	m.mu.Lock()
	if m.running || m.stopCh != nil {
		m.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	m.stopCh, m.doneCh = stop, done
	m.running = true
	interval := m.tickInterval
	m.mu.Unlock()
	m.notify()

	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if !m.tickOnce() {
					return
				}
			}
		}
	}()
}

// Stop halts the tick loop and joins the goroutine.
func (m *LifeGameModel) Stop() {
	m.mu.Lock()
	stop, done := m.stopCh, m.doneCh
	m.stopCh, m.doneCh = nil, nil
	wasRunning := m.running
	m.running = false
	m.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	if wasRunning {
		m.notify()
	}
}

// Restart reseeds state₀ (fresh soup) and starts the loop.
func (m *LifeGameModel) Restart() error {
	m.Stop()
	m.mu.Lock()
	m.rngSeed = (m.rngSeed*lifeLCGMul + lifeLCGAdd) % lifeLCGMod
	if m.rngSeed == 0 {
		m.rngSeed = 1
	}
	m.mu.Unlock()
	if err := m.seed(); err != nil {
		return err
	}
	m.Start()
	return nil
}

// tickOnce runs one host tick: eval the step, write the materialized
// grid back, refresh the cached frame, notify listeners. Returns false
// when the loop should end (tick error, extinction, or a still life).
//
// Extinction and still-life detection are RUNTIME conveniences, not
// program semantics: a dead or frozen board is a fixed point, so
// clocking it forever would burn a tick's worth of eval to reproduce
// the same bytes. The program itself has no halt notion.
func (m *LifeGameModel) tickOnce() bool {
	s, err := m.evalStep()
	cont := true
	m.mu.Lock()
	if err != nil {
		m.lastErr = err.Error()
		m.running = false
		m.stopCh, m.doneCh = nil, nil
		cont = false
	} else {
		still := lifeCellsEqual(m.last.Cells, s.Cells)
		m.last = *s
		m.generation++
		m.lastErr = ""
		switch {
		case lifePopulation(s.Cells) == 0:
			m.status = LifeExtinct
			m.running = false
			m.stopCh, m.doneCh = nil, nil
			cont = false
		case still:
			m.status = LifeStillLife
			m.running = false
			m.stopCh, m.doneCh = nil, nil
			cont = false
		}
	}
	m.mu.Unlock()
	m.notify()
	return cont
}

func lifePopulation(cells []uint64) uint64 {
	var n uint64
	for _, c := range cells {
		if c == 1 {
			n++
		}
	}
	return n
}

func lifeCellsEqual(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *LifeGameModel) evalStep() (*lifeWireState, error) {
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	resp, err := m.ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{m.stepPath}})
	if err != nil {
		return nil, fmt.Errorf("eval dispatch: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("eval status %d (type=%s)", resp.Status, resp.Type)
	}
	if resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		return nil, fmt.Errorf("compute/error code=%s message=%q", ed.Code, ed.Message)
	}
	if resp.Type != lifeGridType {
		return nil, fmt.Errorf("expected %s result, got type=%s", lifeGridType, resp.Type)
	}
	var s lifeWireState
	if err := ecf.Decode(resp.Data, &s); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	ent, err := entity.NewEntity(lifeGridType, cbor.RawMessage(resp.Data))
	if err != nil {
		return nil, err
	}
	if _, err := m.ap.PutEntity(m.statePath, ent); err != nil {
		return nil, fmt.Errorf("put state: %w", err)
	}
	return &s, nil
}

// Render returns the current frame. Pure read of model state.
func (m *LifeGameModel) Render() LifeGameOutput {
	m.mu.Lock()
	defer m.mu.Unlock()
	cells := make([]uint64, len(m.last.Cells))
	copy(cells, m.last.Cells)
	return LifeGameOutput{
		Width: m.w, Height: m.h, Cells: cells,
		Population: lifePopulation(cells),
		Status:     m.status,
		Running:    m.running,
		Generation: m.generation,
		Err:        m.lastErr,
	}
}

// OnChange registers a listener fired after every tick and every
// run-state change. Returns a cancel func. Same contract as the other
// workbench models (see SnakeGameModel.OnChange).
func (m *LifeGameModel) OnChange(h func()) func() {
	if h == nil {
		return func() {}
	}
	m.mu.Lock()
	m.listeners = append(m.listeners, h)
	idx := len(m.listeners) - 1
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if idx < len(m.listeners) {
			m.listeners[idx] = nil
		}
	}
}

func (m *LifeGameModel) notify() {
	m.mu.Lock()
	ls := append([]func(){}, m.listeners...)
	m.mu.Unlock()
	for _, l := range ls {
		if l != nil {
			l()
		}
	}
}

// Close stops the loop. The tree paths (state/step) stay — they are
// ordinary entities under the program root.
func (m *LifeGameModel) Close() { m.Stop() }

// --- the step expression (the PROGRAM) --------------------------------------

var lifeOffsets = [8][2]int{
	{-1, -1}, {0, -1}, {1, -1},
	{-1, 0}, {1, 0},
	{-1, 1}, {0, 1}, {1, 1},
}

// buildLifeStepExpr lowers the Life step (EXPLORATION §6.1, unrolled
// per-neighbor modular arithmetic) with existing compute primitives
// only. Product copy of the Exp-D probe's `arith` builder
// (entitysdk/exp_compute_life_test.go keeps the annotated experiment
// record + the oracle/equivalence proofs; change them together).
//
// Shape:
//
//	let g = lookup/tree(statePath)                 ; the impure edge
//	in let cells = field(g, "cells")               ; bound BEFORE the lambda
//	   in construct(app/life/grid,                 ;   so the closure captures
//	        {width, height,                        ;   plain values
//	         cells: map(indices, λi. perCell)})
//
// Lowering rules honored (POC findings review doc):
//
//	F-D1  integer floor-div = div(sub(a, mod(a,b)), b) — compute `div` is
//	      TRUE division, and a float quotient then poisons `mod`.
//	let* binding order is SORTED-name order — "y" may reference "x".
func buildLifeStepExpr(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
	c := ap.Compute()
	W, H := uint64(w), uint64(h)

	// Width/height are known at build time (the expression is
	// per-grid-size), so the toroidal wrap stays in non-negative uint
	// arithmetic: nx = mod(x + (w+dx), w).
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

	// `count`/`alive` are bound via Let so the shared reads are
	// evaluated once — the interpreter re-evaluates every reference
	// otherwise (DAG sharing does not save evals at Stage 1).
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

	indices := make([]uint64, w*h)
	for i := range indices {
		indices[i] = uint64(i)
	}

	body := c.Construct(lifeGridType, map[string]*entitysdk.Builder{
		"width":  c.Literal(W),
		"height": c.Literal(H),
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         c.Lambda([]string{"i"}, perCell),
		}),
	})

	return c.Let(map[string]*entitysdk.Builder{
		"g": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"cells": c.Field(c.LookupScope("g"), "cells"),
	}, body))
}

// lifeRule wraps a neighbor-count expression with the B3/S23 rule.
func lifeRule(c *entitysdk.ComputeBuilder) *entitysdk.Builder {
	return c.If(
		c.Logic("or",
			c.Compare("eq", c.LookupScope("count"), c.Literal(uint64(3))),
			c.Logic("and",
				c.Compare("eq", c.LookupScope("count"), c.Literal(uint64(2))),
				c.LookupScope("alive"))),
		c.Literal(uint64(1)),
		c.Literal(uint64(0)))
}

// lifeAddTree folds the 8 neighbor reads into a balanced add tree.
func lifeAddTree(c *entitysdk.ComputeBuilder, terms []*entitysdk.Builder) *entitysdk.Builder {
	for len(terms) > 1 {
		next := make([]*entitysdk.Builder, 0, (len(terms)+1)/2)
		for i := 0; i < len(terms); i += 2 {
			if i+1 < len(terms) {
				next = append(next, c.Arithmetic("add", terms[i], terms[i+1]))
			} else {
				next = append(next, terms[i])
			}
		}
		terms = next
	}
	return terms[0]
}
