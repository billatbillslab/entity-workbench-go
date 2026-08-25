package workbench

// SnakeGameModel — the renderer-neutral model for the Snake hostable
// compute program, and the first product consumer of the compute-program
// runtime contract (arch: EXPLORATION-COMPUTE-PROGRAM-RUNTIME-CONTRACT §6.2,
// PROPOSAL-APP-CONVENTION-COMPUTE-PROGRAM).
//
// Three-layer split, descriptor-faithful:
//   - the PROGRAM is a pure compute step expression (grid-of-timers Snake)
//     living in the tree, plus state at a tree path — built by
//     buildSnakeStepExpr below;
//   - the RUNTIME (per the proposal §5, "a harness, not a VM") is this
//     model: it seeds the ports, clocks the tick (host-owned time, §4),
//     writes the snapshot input port, and reads state back;
//   - the RENDERER (Avalonia panel, console, …) only consumes Render()
//     and forwards direction input — thin I/O per the workbench brain/
//     renderer discipline.
//
// The step expression is the product copy of the Exp-E probe
// (entitysdk/exp_compute_snake_test.go — the frozen experiment record
// that also carries the oracle-equivalence + replay-determinism proofs;
// lowering rules F-D1/F-E2 are annotated there and in
// docs/architecture/reviews/COMPUTE-PROGRAM-POC-FINDINGS-2026-07-15.md).
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
	snakeStateType = "app/snake/state"
	snakeInputType = "app/snake/input"

	// SnakeUp/Right/Down/Left are the input-port direction values.
	SnakeUp    = uint64(0)
	SnakeRight = uint64(1)
	SnakeDown  = uint64(2)
	SnakeLeft  = uint64(3)

	snakeLCGMul = uint64(1103515245)
	snakeLCGAdd = uint64(12345)
	snakeLCGMod = uint64(2147483648)
)

// SnakeGameOutput is the renderer contract: everything a display driver
// needs to draw one frame, nothing else. Cells is the grid of body
// timers (0 = empty, >0 = segment; the head cell holds Length).
type SnakeGameOutput struct {
	Width   int      `json:"width"`
	Height  int      `json:"height"`
	Cells   []uint64 `json:"cells"`
	Head    uint64   `json:"head"`
	Food    uint64   `json:"food"`
	Length  uint64   `json:"length"`
	Status  uint64   `json:"status"` // 0 = playing, 1 = dead
	Running bool     `json:"running"`
	Ticks   uint64   `json:"ticks"`
	Err     string   `json:"err,omitempty"`
}

// snakeWireState is the CBOR shape of the state entity at the state path.
type snakeWireState struct {
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

// SnakeGameModel drives one Snake program instance on one peer.
type SnakeGameModel struct {
	ap        *entitysdk.AppPeer
	w, h      int
	statePath string
	inputPath string
	stepPath  string

	mu        sync.Mutex
	last      snakeWireState
	running   bool
	ticks     uint64
	lastErr   string
	rngSeed   uint64
	listeners []func()

	stopCh chan struct{} // non-nil while the tick goroutine runs
	doneCh chan struct{}

	tickInterval time.Duration
}

// NewSnakeGameModel seeds the program (state + input port + step
// expression) under root and returns the stopped model. rngSeed picks
// the food sequence; pass 0 for a clock-derived seed. The seed is the
// one place non-determinism enters — it is recorded in state₀, so a
// game is still a pure function of (state₀, input-stream).
func NewSnakeGameModel(ap *entitysdk.AppPeer, root string, rngSeed uint64) (*SnakeGameModel, error) {
	if ap == nil {
		return nil, fmt.Errorf("SnakeGameModel: nil AppPeer")
	}
	if rngSeed == 0 {
		rngSeed = uint64(time.Now().UnixNano()) % snakeLCGMod
	}
	const w, h = 12, 12
	m := &SnakeGameModel{
		ap: ap, w: w, h: h,
		statePath:    root + "/state",
		inputPath:    root + "/input",
		stepPath:     root + "/step",
		rngSeed:      rngSeed,
		tickInterval: time.Second / 6, // rate_hint: 6 ticks/s
	}
	if err := m.seed(); err != nil {
		return nil, err
	}
	if _, err := buildSnakeStepExpr(ap, w, h, m.statePath, m.inputPath).
		Build(context.Background(), m.stepPath); err != nil {
		return nil, fmt.Errorf("SnakeGameModel: build step: %w", err)
	}
	return m, nil
}

// seed writes state₀ and initializes the input port (F-E1: port
// initialization is the runtime's job — an unseeded input port read is
// a compute/error).
func (m *SnakeGameModel) seed() error {
	s := snakeSeedState(m.w, m.h, m.rngSeed)
	ent, err := snakeStateEntity(s)
	if err != nil {
		return fmt.Errorf("SnakeGameModel: seed state: %w", err)
	}
	if _, err := m.ap.PutEntity(m.statePath, ent); err != nil {
		return fmt.Errorf("SnakeGameModel: put seed state: %w", err)
	}
	if err := m.putInput(s.Dir); err != nil {
		return err
	}
	m.mu.Lock()
	m.last = s
	m.ticks = 0
	m.lastErr = ""
	m.mu.Unlock()
	return nil
}

// snakeSeedState: length-3 snake heading right from the board center,
// food up-and-right of it.
func snakeSeedState(w, h int, rngSeed uint64) snakeWireState {
	cells := make([]uint64, w*h)
	head := (h/2)*w + w/2
	cells[head] = 3
	cells[head-1] = 2
	cells[head-2] = 1
	food := (h/4)*w + (3*w)/4
	return snakeWireState{
		Width: uint64(w), Height: uint64(h), Cells: cells,
		Head: uint64(head), Dir: SnakeRight, Food: uint64(food),
		Length: 3, RNG: rngSeed % snakeLCGMod, Status: 0,
	}
}

func snakeStateEntity(s snakeWireState) (entity.Entity, error) {
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

// putInput is the snapshot-input-port write (last-write-wins tree put).
func (m *SnakeGameModel) putInput(dir uint64) error {
	raw, err := ecf.Encode(map[string]interface{}{"dir": dir})
	if err != nil {
		return fmt.Errorf("SnakeGameModel: encode input: %w", err)
	}
	ent, err := entity.NewEntity(snakeInputType, cbor.RawMessage(raw))
	if err != nil {
		return fmt.Errorf("SnakeGameModel: input entity: %w", err)
	}
	if _, err := m.ap.PutEntity(m.inputPath, ent); err != nil {
		return fmt.Errorf("SnakeGameModel: put input: %w", err)
	}
	return nil
}

// Input is the renderer's input driver: write the latest direction to
// the input port. Sampled by the step at the next tick boundary; the
// 180°-reversal guard lives in the program, not here.
func (m *SnakeGameModel) Input(dir uint64) error {
	if dir > SnakeLeft {
		return fmt.Errorf("SnakeGameModel: direction %d out of range", dir)
	}
	return m.putInput(dir)
}

// Start clocks the tick (clock-driven mode). No-op if already running.
func (m *SnakeGameModel) Start() {
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
func (m *SnakeGameModel) Stop() {
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

// Restart reseeds state₀ (fresh RNG seed) and starts the loop.
func (m *SnakeGameModel) Restart() error {
	m.Stop()
	m.mu.Lock()
	m.rngSeed = (m.rngSeed*snakeLCGMul + snakeLCGAdd) % snakeLCGMod
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
// state entity back, refresh the cached frame, notify listeners.
// Returns false when the loop should end (death or tick error).
func (m *SnakeGameModel) tickOnce() bool {
	s, err := m.evalStep()
	cont := true
	m.mu.Lock()
	if err != nil {
		m.lastErr = err.Error()
		m.running = false
		m.stopCh, m.doneCh = nil, nil
		cont = false
	} else {
		m.last = *s
		m.ticks++
		m.lastErr = ""
		if s.Status == 1 { // dead — stop the clock; Restart revives
			m.running = false
			m.stopCh, m.doneCh = nil, nil
			cont = false
		}
	}
	m.mu.Unlock()
	m.notify()
	return cont
}

func (m *SnakeGameModel) evalStep() (*snakeWireState, error) {
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
	if resp.Type != snakeStateType {
		return nil, fmt.Errorf("expected %s result, got type=%s", snakeStateType, resp.Type)
	}
	var s snakeWireState
	if err := ecf.Decode(resp.Data, &s); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	ent, err := entity.NewEntity(snakeStateType, cbor.RawMessage(resp.Data))
	if err != nil {
		return nil, err
	}
	if _, err := m.ap.PutEntity(m.statePath, ent); err != nil {
		return nil, fmt.Errorf("put state: %w", err)
	}
	return &s, nil
}

// Render returns the current frame. Pure read of model state.
func (m *SnakeGameModel) Render() SnakeGameOutput {
	m.mu.Lock()
	defer m.mu.Unlock()
	cells := make([]uint64, len(m.last.Cells))
	copy(cells, m.last.Cells)
	return SnakeGameOutput{
		Width: m.w, Height: m.h, Cells: cells,
		Head: m.last.Head, Food: m.last.Food,
		Length: m.last.Length, Status: m.last.Status,
		Running: m.running, Ticks: m.ticks, Err: m.lastErr,
	}
}

// OnChange registers a listener fired after every tick and every
// run-state change. Returns a cancel func. Same contract as the other
// workbench models (see SiteModel.OnChange).
func (m *SnakeGameModel) OnChange(h func()) func() {
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

func (m *SnakeGameModel) notify() {
	m.mu.Lock()
	ls := append([]func(){}, m.listeners...)
	m.mu.Unlock()
	for _, l := range ls {
		if l != nil {
			l()
		}
	}
}

// Close stops the loop. The tree paths (state/input/step) stay — they
// are ordinary entities under the program root.
func (m *SnakeGameModel) Close() { m.Stop() }

// --- the step expression (the PROGRAM) --------------------------------------

// buildSnakeStepExpr lowers the §6.2 Snake step with existing compute
// primitives only. Product copy of the Exp-E probe's builder
// (entitysdk/exp_compute_snake_test.go keeps the annotated experiment
// record + oracle/replay proofs; change them together).
//
// Lowering rules honored (POC findings review doc):
//   F-D1  integer floor-div = div(sub(a, mod(a,b)), b)
//   F-E2a partial ops guarded behind lazy if (index only when in-bounds)
//   F-E2b rare-path work (food filter) inside the if branch, not a let
//   let* binding order is SORTED-name order — names picked to match.
func buildSnakeStepExpr(ap *entitysdk.AppPeer, w, h int, statePath, inputPath string) *entitysdk.Builder {
	c := ap.Compute()
	W, H := uint64(w), uint64(h)
	n := w * h
	indices := make([]uint64, n)
	for i := range indices {
		indices[i] = uint64(i)
	}
	sc := c.LookupScope

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
				c.Arithmetic("mul", sc("rng"), c.Literal(snakeLCGMul)),
				c.Literal(snakeLCGAdd)),
			c.Literal(snakeLCGMod)),
	}, c.Let(map[string]*entitysdk.Builder{
		"nfood": c.If(sc("ate"), pickFood, sc("food")),
	}, aliveConstruct)))

	deadConstruct := c.Construct(snakeStateType, map[string]*entitysdk.Builder{
		"width": c.Literal(W), "height": c.Literal(H),
		"cells": sc("cells"), "head": sc("head"), "dir": sc("ndir"),
		"food": sc("food"), "length": sc("leng"), "rng": sc("rng"),
		"status": c.Literal(uint64(1)),
	})

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
			"dx": c.If(c.Compare("eq", sc("ndir"), c.Literal(SnakeRight)), c.Literal(int64(1)),
				c.If(c.Compare("eq", sc("ndir"), c.Literal(SnakeLeft)), c.Literal(int64(-1)),
					c.Literal(int64(0)))),
			"dy": c.If(c.Compare("eq", sc("ndir"), c.Literal(SnakeDown)), c.Literal(int64(1)),
				c.If(c.Compare("eq", sc("ndir"), c.Literal(SnakeUp)), c.Literal(int64(-1)),
					c.Literal(int64(0)))),
			"hx": c.Arithmetic("mod", sc("head"), c.Literal(W)),
			"hy": c.Arithmetic("div",
				c.Arithmetic("sub", sc("head"), sc("hx")),
				c.Literal(W)),
		}, c.Let(map[string]*entitysdk.Builder{
			"nx": c.Arithmetic("add", sc("hx"), sc("dx")),
			"ny": c.Arithmetic("add", sc("hy"), sc("dy")),
		}, c.Let(map[string]*entitysdk.Builder{
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
			"ate": c.Compare("eq", sc("nhead"), sc("food")),
			"hits": c.If(sc("wall"),
				c.Literal(false),
				c.Compare("gt",
					c.Index(sc("cells"), sc("nhead")),
					c.If(sc("ate"), c.Literal(uint64(0)), c.Literal(uint64(1))))),
		}, c.If(c.Logic("or", sc("wall"), sc("hits")),
			deadConstruct,
			aliveExpr))))))

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
		sc("s"),
		movement)))
}
