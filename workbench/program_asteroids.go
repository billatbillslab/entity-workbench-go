package workbench

// AsteroidsGameModel — the renderer-neutral model for the Asteroids hostable
// compute program: the first HETEROGENEOUS, VARIABLE-ACTOR-SET program in the
// workbench, and the first to bind a DISPLAY-LIST output port.
//
// Arch context (entity-system-architecture):
//   docs/research/explorations/EXPLORATION-COMPUTE-PROGRAM-RUNTIME-CONTRACT.md
//     §7.1  the Doom frontier: variable actor set + cross-actor spatial query
//     §8    the port taxonomy (snapshot vs stream; display/framebuffer)
//     §10.3 the non-determinism boundary — RNG enters as INPUT
//   docs/status/HANDOFF-2026-07-16-compute-heterogeneous-actor-probe.md — the ask
// Our findings + the port measurement:
//   docs/architecture/reviews/COMPUTE-ASTEROIDS-PORT-TAXONOMY-2026-07-16.md
//
// Three-layer split, descriptor-faithful (same as Snake/Life):
//   - the PROGRAM is a pure compute step expression living in the tree, plus
//     state at a tree path — built by buildAsteroidsStepExpr below;
//   - the RUNTIME is this model: it seeds the ports, clocks the tick (host-owned
//     time), writes the snapshot input port, and reads state back;
//   - the RENDERER (Avalonia panel) only consumes Render() and forwards input.
//
// WHY THIS MODEL RENDERS THROUGH A DISPLAY LIST — and why that is the point.
//
// Life and Snake never had to answer "what is an output port?", because for a
// grid game the STATE *is* the display, and their output port points straight at
// the state path. Asteroids is the first program where they come apart: the
// state is an actor array; the display is not. Something must map one to the
// other, and where it sits decides whether renderers stay generic.
//
// So the tick evaluates TWO expressions: the step (state' = f(state, input)) and
// a separate display-list expression (frame = g(state')). The display list emits
// world-space polygon vertices + a kind tag — so the panel's contract is "draw
// these polylines, colour by kind", a fixed vocabulary with NO asteroid-specific
// knowledge. That is the claim from the port review, and this model is where it
// gets dogfooded: if the panel ends up needing to know what an asteroid is, the
// claim was wrong.
//
// The honest cost: a derived port is ADDITIONAL work per tick, not a substitute
// — two evals instead of one. Measured (F5): step 39,082 ops, display list 6,092
// ops, i.e. the port adds ~16%.
//
// The step expression is the product copy of the Exp-F probe
// (entitysdk/exp_compute_asteroids_test.go — the frozen experiment record that
// also carries the oracle-equivalence, replay-determinism, and shard-identity
// proofs). CHANGE THEM TOGETHER.
//
// Tick loop = host-clocked explicit eval + tree put (NEVER a reactive install on
// the state path — the §4 cascade rule).

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

const (
	astStateType   = "app/asteroids/state"
	astInputType   = "app/asteroids/input"
	astDisplayType = "app/asteroids/display"

	// actor kinds. 0 = free slot (the live flag, inverted).
	AsteroidsFree     = uint64(0)
	AsteroidsShip     = uint64(1)
	AsteroidsAsteroid = uint64(2)
	AsteroidsBullet   = uint64(3)

	// fixed-point: 1 world unit = astFP sub-units.
	astFP    = int64(256)
	astWorld = int64(256) * astFP

	astRotSteps = uint64(16)

	astThrustAcc = int64(24)
	astBulletSpd = int64(768)
	astBulletTTL = uint64(18)

	astShipRadius = int64(3) * astFP
	astAstRadius  = int64(6) * astFP

	// AsteroidsKey* are the held-key input-port bit positions. The port is a
	// SNAPSHOT of a key SET (a bitmask), not a stream of events: Snake's
	// single last-write-wins direction cannot express "thrust + rotate + fire
	// at once", but a bitmask can, and it needs no new port kind.
	AsteroidsKeyLeft   = 0
	AsteroidsKeyRight  = 1
	AsteroidsKeyThrust = 2
	AsteroidsKeyFire   = 3

	astLCGMul = uint64(1103515245)
	astLCGAdd = uint64(12345)
	astLCGMod = uint64(2147483648)

	// astCapacity is the fixed mobj cap. Slot 0 is the ship by convention.
	// A fixed-capacity array with a live flag is how the VARIABLE actor set is
	// expressed without an array-concat primitive (Doom uses the same trick).
	astCapacity = 24
)

// AsteroidsQuad is one drawable: a closed 4-vertex polygon in world space plus
// the kind tag the renderer colours by. This is the whole renderer contract.
type AsteroidsQuad struct {
	Kind uint64  `json:"kind"`
	X    []int64 `json:"x"`
	Y    []int64 `json:"y"`
}

// AsteroidsGameOutput is the renderer contract: everything a display driver
// needs to draw one frame, nothing else. Note what is ABSENT — positions,
// velocities, sizes, the actor array. The renderer draws polylines.
type AsteroidsGameOutput struct {
	World int64 `json:"world"` // world extent, in FP units
	// Wrap says the world is a TORUS. An actor's centre wraps, but its outline
	// is centre + offsets and is deliberately NOT wrapped — wrapping vertices
	// individually would tear the polygon. The renderer tiles the outline at the
	// seams instead. Scene-level, so it stays within the display-list contract:
	// the renderer learns the space wraps, not what an asteroid is.
	Wrap    bool            `json:"wrap"`
	Quads   []AsteroidsQuad `json:"quads"`
	Score   uint64          `json:"score"`
	Status  uint64          `json:"status"` // 0 = playing, 1 = ship destroyed
	Running bool            `json:"running"`
	Ticks   uint64          `json:"ticks"`
	Err     string          `json:"err,omitempty"`
}

// astWireState is the CBOR shape of the state entity — struct-of-arrays.
//
// SoA is deliberate: the natural array-of-structs encoding materializes each
// actor to its own content-addressed entity and puts 33-byte hash refs in the
// array, costing the host astCapacity content-store lookups per frame. The step
// builds actors AoS in-flight and projects them to flat arrays at the boundary.
type astWireState struct {
	Kinds  []uint64 `cbor:"kinds"`
	Xs     []int64  `cbor:"xs"`
	Ys     []int64  `cbor:"ys"`
	Vxs    []int64  `cbor:"vxs"`
	Vys    []int64  `cbor:"vys"`
	Rots   []uint64 `cbor:"rots"`
	Szs    []uint64 `cbor:"szs"`
	Ttls   []uint64 `cbor:"ttls"`
	RNG    uint64   `cbor:"rng"`
	Score  uint64   `cbor:"score"`
	Status uint64   `cbor:"status"`
}

// astWireDisplay is the CBOR shape of the display-list entity.
type astWireDisplay struct {
	Kinds []uint64 `cbor:"kinds"`
	X0    []int64  `cbor:"x0"`
	Y0    []int64  `cbor:"y0"`
	X1    []int64  `cbor:"x1"`
	Y1    []int64  `cbor:"y1"`
	X2    []int64  `cbor:"x2"`
	Y2    []int64  `cbor:"y2"`
	X3    []int64  `cbor:"x3"`
	Y3    []int64  `cbor:"y3"`
}

// AsteroidsGameModel drives one Asteroids program instance on one peer.
type AsteroidsGameModel struct {
	ap          *entitysdk.AppPeer
	statePath   string
	inputPath   string
	stepPath    string
	displayPath string

	mu        sync.Mutex
	last      astWireDisplay
	score     uint64
	status    uint64
	running   bool
	ticks     uint64
	lastErr   string
	rngSeed   uint64
	listeners []func()

	stopCh chan struct{}
	doneCh chan struct{}

	tickInterval time.Duration
}

// NewAsteroidsGameModel seeds the program (state + input port + step + display
// expressions) under root and returns the stopped model. rngSeed picks the
// asteroid layout; pass 0 for a clock-derived seed. The seed is the one place
// non-determinism enters — it is recorded in state₀, so a game is still a pure
// function of (state₀, input-stream).
func NewAsteroidsGameModel(ap *entitysdk.AppPeer, root string, rngSeed uint64) (*AsteroidsGameModel, error) {
	if ap == nil {
		return nil, fmt.Errorf("AsteroidsGameModel: nil AppPeer")
	}
	if rngSeed == 0 {
		rngSeed = uint64(time.Now().UnixNano()) % astLCGMod
	}
	m := &AsteroidsGameModel{
		ap:           ap,
		statePath:    root + "/state",
		inputPath:    root + "/input",
		stepPath:     root + "/step",
		displayPath:  root + "/display",
		rngSeed:      rngSeed,
		tickInterval: time.Second / 12, // rate_hint: 12 ticks/s
	}
	if err := m.seed(); err != nil {
		return nil, err
	}
	if _, err := buildAsteroidsStepExpr(ap, m.statePath, m.inputPath).
		Build(context.Background(), m.stepPath); err != nil {
		return nil, fmt.Errorf("AsteroidsGameModel: build step: %w", err)
	}
	if _, err := buildAsteroidsDisplayExpr(ap, m.statePath).
		Build(context.Background(), m.displayPath); err != nil {
		return nil, fmt.Errorf("AsteroidsGameModel: build display: %w", err)
	}
	if err := m.refreshDisplay(); err != nil {
		return nil, err
	}
	return m, nil
}

// seed writes state₀ and initializes the input port (port initialization is the
// runtime's job — an unseeded input port read is a compute/error).
func (m *AsteroidsGameModel) seed() error {
	s := astSeedState(m.rngSeed)
	ent, err := astStateEntity(s)
	if err != nil {
		return fmt.Errorf("AsteroidsGameModel: seed state: %w", err)
	}
	if _, err := m.ap.PutEntity(m.statePath, ent); err != nil {
		return fmt.Errorf("AsteroidsGameModel: put seed state: %w", err)
	}
	if err := m.putInput(0); err != nil {
		return err
	}
	m.mu.Lock()
	m.score, m.status = 0, 0
	m.ticks = 0
	m.lastErr = ""
	m.mu.Unlock()
	return nil
}

// astSeedState: ship at world centre facing up, plus four size-3 asteroids on a
// ring around it with distinct drift headings. Placement is deterministic by
// construction (no RNG at seed time), so the layout is inspectable.
func astSeedState(rngSeed uint64) astWireState {
	s := astWireState{
		Kinds: make([]uint64, astCapacity),
		Xs:    make([]int64, astCapacity),
		Ys:    make([]int64, astCapacity),
		Vxs:   make([]int64, astCapacity),
		Vys:   make([]int64, astCapacity),
		Rots:  make([]uint64, astCapacity),
		Szs:   make([]uint64, astCapacity),
		Ttls:  make([]uint64, astCapacity),
		RNG:   rngSeed % astLCGMod,
	}
	s.Kinds[0] = AsteroidsShip
	s.Xs[0] = astWorld / 2
	s.Ys[0] = astWorld / 2

	const nAst = 4
	for i := 0; i < nAst; i++ {
		slot := i + 1
		ang := 2 * math.Pi * float64(i) / float64(nAst)
		s.Kinds[slot] = AsteroidsAsteroid
		s.Xs[slot] = astWorld/2 + int64(math.Round(math.Cos(ang)*float64(80*astFP)))
		s.Ys[slot] = astWorld/2 + int64(math.Round(math.Sin(ang)*float64(80*astFP)))
		s.Vxs[slot] = int64(math.Round(math.Cos(ang+1.0) * 96))
		s.Vys[slot] = int64(math.Round(math.Sin(ang+1.0) * 96))
		s.Szs[slot] = 3
	}
	return s
}

func astStateEntity(s astWireState) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{
		"kinds": s.Kinds, "xs": s.Xs, "ys": s.Ys,
		"vxs": s.Vxs, "vys": s.Vys, "rots": s.Rots,
		"szs": s.Szs, "ttls": s.Ttls,
		"rng": s.RNG, "score": s.Score, "status": s.Status,
	})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(astStateType, cbor.RawMessage(raw))
}

// putInput is the snapshot-input-port write (last-write-wins tree put).
func (m *AsteroidsGameModel) putInput(keys uint64) error {
	raw, err := ecf.Encode(map[string]interface{}{"keys": keys})
	if err != nil {
		return fmt.Errorf("AsteroidsGameModel: encode input: %w", err)
	}
	ent, err := entity.NewEntity(astInputType, cbor.RawMessage(raw))
	if err != nil {
		return fmt.Errorf("AsteroidsGameModel: input entity: %w", err)
	}
	if _, err := m.ap.PutEntity(m.inputPath, ent); err != nil {
		return fmt.Errorf("AsteroidsGameModel: put input: %w", err)
	}
	return nil
}

// Input is the renderer's input driver: write the currently-held key SET to the
// input port as a bitmask. Sampled by the step at the next tick boundary.
//
// The renderer's job is to track keydown/keyup and hand over the current set —
// it must NOT synthesize per-tick events. Held state is a snapshot, not a stream.
func (m *AsteroidsGameModel) Input(keys uint64) error {
	const maxKeys = (1 << 4) - 1
	if keys > maxKeys {
		return fmt.Errorf("AsteroidsGameModel: key bitmask %d out of range", keys)
	}
	return m.putInput(keys)
}

// Start clocks the tick (clock-driven mode). No-op if already running.
func (m *AsteroidsGameModel) Start() {
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
func (m *AsteroidsGameModel) Stop() {
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
func (m *AsteroidsGameModel) Restart() error {
	m.Stop()
	m.mu.Lock()
	m.rngSeed = (m.rngSeed*astLCGMul + astLCGAdd) % astLCGMod
	if m.rngSeed == 0 {
		m.rngSeed = 1
	}
	m.mu.Unlock()
	if err := m.seed(); err != nil {
		return err
	}
	if err := m.refreshDisplay(); err != nil {
		return err
	}
	m.Start()
	return nil
}

// tickOnce runs one host tick: eval the step, write the materialized state
// entity back, re-derive the display list, notify listeners. Returns false when
// the loop should end (ship destroyed or tick error).
func (m *AsteroidsGameModel) tickOnce() bool {
	s, err := m.evalStep()
	cont := true
	if err == nil {
		err = m.refreshDisplay()
	}
	m.mu.Lock()
	if err != nil {
		m.lastErr = err.Error()
		m.running = false
		m.stopCh, m.doneCh = nil, nil
		cont = false
	} else {
		m.score, m.status = s.Score, s.Status
		m.ticks++
		m.lastErr = ""
		if s.Status == 1 { // ship destroyed — stop the clock; Restart revives
			m.running = false
			m.stopCh, m.doneCh = nil, nil
			cont = false
		}
	}
	m.mu.Unlock()
	m.notify()
	return cont
}

// evalExpr dispatches one compute eval and returns the raw result data.
func (m *AsteroidsGameModel) evalExpr(path, wantType string) ([]byte, error) {
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	resp, err := m.ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{path}})
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
	if resp.Type != wantType {
		return nil, fmt.Errorf("expected %s result, got type=%s", wantType, resp.Type)
	}
	return resp.Data, nil
}

func (m *AsteroidsGameModel) evalStep() (*astWireState, error) {
	data, err := m.evalExpr(m.stepPath, astStateType)
	if err != nil {
		return nil, err
	}
	var s astWireState
	if err := ecf.Decode(data, &s); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	ent, err := entity.NewEntity(astStateType, cbor.RawMessage(data))
	if err != nil {
		return nil, err
	}
	if _, err := m.ap.PutEntity(m.statePath, ent); err != nil {
		return nil, fmt.Errorf("put state: %w", err)
	}
	return &s, nil
}

// refreshDisplay re-derives the display-list output port from the current state.
// This is the SECOND eval per tick — the honest, measured cost of a derived port
// (~16% on top of the step).
func (m *AsteroidsGameModel) refreshDisplay() error {
	data, err := m.evalExpr(m.displayPath, astDisplayType)
	if err != nil {
		return err
	}
	var d astWireDisplay
	if err := ecf.Decode(data, &d); err != nil {
		return fmt.Errorf("decode display list: %w", err)
	}
	m.mu.Lock()
	m.last = d
	m.mu.Unlock()
	return nil
}

// Render returns the current frame: live quads only. Pure read of model state.
func (m *AsteroidsGameModel) Render() AsteroidsGameOutput {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.last
	quads := make([]AsteroidsQuad, 0, len(d.Kinds))
	for i, k := range d.Kinds {
		if k == AsteroidsFree {
			continue // free slots are not drawables
		}
		quads = append(quads, AsteroidsQuad{
			Kind: k,
			X:    []int64{d.X0[i], d.X1[i], d.X2[i], d.X3[i]},
			Y:    []int64{d.Y0[i], d.Y1[i], d.Y2[i], d.Y3[i]},
		})
	}
	return AsteroidsGameOutput{
		World:   astWorld,
		Wrap:    true, // the step wraps every position mod astWorld
		Quads:   quads,
		Score:   m.score,
		Status:  m.status,
		Running: m.running,
		Ticks:   m.ticks,
		Err:     m.lastErr,
	}
}

// OnChange registers a listener fired after every tick and every run-state
// change. Returns a cancel func. Same contract as the other workbench models.
func (m *AsteroidsGameModel) OnChange(h func()) func() {
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

func (m *AsteroidsGameModel) notify() {
	m.mu.Lock()
	ls := append([]func(){}, m.listeners...)
	m.mu.Unlock()
	for _, l := range ls {
		if l != nil {
			l()
		}
	}
}

// Close stops the loop. The tree paths stay — they are ordinary entities under
// the program root.
func (m *AsteroidsGameModel) Close() { m.Stop() }

// --- trig tables (pre-scaled; emitted as compute literals) ------------------

// astTable builds a length-astRotSteps table of round(mag*cos(theta+phase)).
// Pre-scaling by `mag` is what removes DIVISION from the whole program: a
// velocity or vertex offset is table[idx] (or table[idx] * an integer count),
// never a divide. Doom's finesine table, same trick — and it sidesteps the
// "div is true division" trap rather than lowering around it.
func astTable(mag int64, phase float64) []int64 {
	t := make([]int64, astRotSteps)
	for i := range t {
		ang := 2*math.Pi*float64(i)/float64(astRotSteps) + phase
		t[i] = int64(math.Round(float64(mag) * math.Cos(ang)))
	}
	return t
}

// Heading rot=i is angle θ=2πi/16 clockwise from "up", so the unit direction is
// (sin θ, -cos θ) and rot 0 = up (-y), rot 4 = right (+x). Both components are a
// phase-shifted cos so one builder serves both.
func astCosTable(mag int64) []int64 { return astTable(mag, -math.Pi/2) } // → dx
func astSinTable(mag int64) []int64 { return astTable(mag, math.Pi) }    // → dy

// --- the step expression (the PROGRAM) --------------------------------------
//
// Product copy of the Exp-F probe's builder (entitysdk/exp_compute_asteroids_test.go).
// That file is the frozen experiment record carrying the oracle-equivalence,
// replay-determinism and shard-identity proofs — CHANGE THEM TOGETHER.
// The probe's shard-range parameter is dropped here: the panel ticks whole.

func buildAsteroidsStepExpr(ap *entitysdk.AppPeer, statePath, inputPath string) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope
	const full = true // the panel always ticks the whole actor set

	// idxLit — the FULL actor set. Every cross-actor query reads all of it,
	// in a shard exactly as in a full tick.
	indices := make([]uint64, astCapacity)
	for i := range indices {
		indices[i] = uint64(i)
	}
	idxLit := c.Literal(indices)

	// slotLit — the slots THIS eval is responsible for producing.
	slots := make([]uint64, 0, astCapacity)
	for i := 0; i < astCapacity; i++ {
		slots = append(slots, uint64(i))
	}
	slotLit := c.Literal(slots)

	// floorDiv: F-D1. Only sound for non-negative a (the bitmask case).
	floorDiv := func(a, b *entitysdk.Builder) *entitysdk.Builder {
		return c.Arithmetic("div", c.Arithmetic("sub", a, c.Arithmetic("mod", a, b)), b)
	}
	// bit k of the held-key snapshot.
	bit := func(k int) *entitysdk.Builder {
		return c.Compare("eq",
			c.Arithmetic("mod", floorDiv(sc("keys"), c.Literal(uint64(1)<<uint(k))), c.Literal(uint64(2))),
			c.Literal(uint64(1)))
	}

	at := func(arr string, i *entitysdk.Builder) *entitysdk.Builder {
		return c.Index(sc(arr), i)
	}

	// wrap: (v + delta + WORLD) mod WORLD. Keeps the operand non-negative so
	// `mod` never sees a negative (|delta| < WORLD always holds here).
	wrap := func(v, d *entitysdk.Builder) *entitysdk.Builder {
		return c.Arithmetic("mod",
			c.Arithmetic("add", c.Arithmetic("add", v, d), c.Literal(astWorld)),
			c.Literal(astWorld))
	}

	// distSq(i, j) over the FROZEN previous state. No wrap-around distance —
	// collisions across the world seam are missed; noted as a probe
	// simplification, not a lowering constraint (see the findings block).
	distSq := func(i, j *entitysdk.Builder) *entitysdk.Builder {
		dx := c.Arithmetic("sub", at("xs", i), at("xs", j))
		dy := c.Arithmetic("sub", at("ys", i), at("ys", j))
		return c.Arithmetic("add",
			c.Arithmetic("mul", dx, dx),
			c.Arithmetic("mul", dy, dy))
	}

	// astHitRadiusSq(j): squared collision radius of asteroid j, scaled by size.
	astHitRadiusSq := func(j *entitysdk.Builder) *entitysdk.Builder {
		r := c.Arithmetic("mul", c.Literal(astAstRadius), at("szs", j))
		return c.Arithmetic("mul", r, r)
	}

	// --- cross-actor spatial query (the O(N^2) all-pairs core) --------------

	// bulletHits(j): is asteroid j hit by any live bullet? filter+length over
	// the whole frozen actor set. This is the capturing-closure shape F-D2
	// punishes (the enclosing let's arrays are captured per element) — which is
	// exactly why this probe is worth measuring on Axis-1.
	bulletHitsRaw := func(j *entitysdk.Builder) *entitysdk.Builder {
		return c.Compare("gt",
			c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
				"collection": idxLit,
				"fn": c.Lambda([]string{"b"},
					c.Logic("and",
						c.Compare("eq", at("kinds", sc("b")), c.Literal(AsteroidsBullet)),
						c.Compare("lt", distSq(j, sc("b")), astHitRadiusSq(j)))),
			})),
			c.Literal(uint64(0)))
	}

	// bulletHits indexes the PRECOMPUTED per-actor hit array ("hitb", bound once
	// below) instead of re-scanning every bullet.
	//
	// Whether asteroid j was hit is a function of the frozen previous state
	// alone, but it is needed in THREE places (the split scan, the asteroid's
	// own think, and the score tally) — so the naive lowering ran the same
	// O(CAP) scan three times per asteroid. Hoisting is a pure refactor, and
	// the probe's F2 differential against the Go oracle is what proves it.
	bulletHits := func(j *entitysdk.Builder) *entitysdk.Builder {
		return c.Index(sc("hitb"), j)
	}

	// bulletSpent(b): did bullet b hit any asteroid this tick?
	bulletSpent := func(b *entitysdk.Builder) *entitysdk.Builder {
		return c.Compare("gt",
			c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
				"collection": idxLit,
				"fn": c.Lambda([]string{"a"},
					c.Logic("and",
						c.Compare("eq", at("kinds", sc("a")), c.Literal(AsteroidsAsteroid)),
						c.Compare("lt", distSq(sc("a"), b), astHitRadiusSq(sc("a"))))),
			})),
			c.Literal(uint64(0)))
	}

	// shipHit: does any asteroid overlap the ship (slot 0)?
	shipHit := c.Compare("gt",
		c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
			"collection": idxLit,
			"fn": c.Lambda([]string{"a"},
				c.Logic("and",
					c.Compare("eq", at("kinds", sc("a")), c.Literal(AsteroidsAsteroid)),
					c.Compare("lt",
						distSq(sc("a"), c.Literal(uint64(0))),
						c.Arithmetic("mul",
							c.Arithmetic("add", c.Literal(astShipRadius),
								c.Arithmetic("mul", c.Literal(astAstRadius), at("szs", sc("a")))),
							c.Arithmetic("add", c.Literal(astShipRadius),
								c.Arithmetic("mul", c.Literal(astAstRadius), at("szs", sc("a")))))))),
		})),
		c.Literal(uint64(0)))

	// --- the fixed-cap slot allocator (the concat sidestep) -----------------
	//
	// `splits` = the asteroids that were hit AND are big enough to split; each
	// contributes exactly ONE new asteroid (the other half reuses the source's
	// own slot). A free slot claims a spawn by its RANK among free slots:
	// rank 0 goes to the new bullet (if firing), the rest to splits in order.
	//
	// This is the fixed-cap tax, and it is O(CAP) per free slot (the rank
	// filter) on top of O(CAP) per actor (collision) — see F-F2.
	splits := c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
		"collection": idxLit,
		"fn": c.Lambda([]string{"j"},
			c.Logic("and",
				c.Logic("and",
					c.Compare("eq", at("kinds", sc("j")), c.Literal(AsteroidsAsteroid)),
					c.Compare("gt", at("szs", sc("j")), c.Literal(uint64(1)))),
				bulletHits(sc("j")))),
	})

	// rank(i): how many free slots precede slot i.
	rank := func(i *entitysdk.Builder) *entitysdk.Builder {
		return c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
			"collection": idxLit,
			"fn": c.Lambda([]string{"j"},
				c.Logic("and",
					c.Compare("lt", sc("j"), i),
					c.Compare("eq", at("kinds", sc("j")), c.Literal(AsteroidsFree)))),
		}))
	}

	free := c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
		"kind": c.Literal(AsteroidsFree),
		"x":    c.Literal(int64(0)), "y": c.Literal(int64(0)),
		"vx": c.Literal(int64(0)), "vy": c.Literal(int64(0)),
		"rot": c.Literal(uint64(0)), "sz": c.Literal(uint64(0)),
		"ttl": c.Literal(uint64(0)),
	})

	// --- per-kind think -----------------------------------------------------

	// ship: rotate (held left/right), thrust along heading, wrap, die on hit.
	shipThink := c.Let(map[string]*entitysdk.Builder{
		"nrot": c.Arithmetic("mod",
			c.Arithmetic("add", at("rots", sc("i")),
				c.If(bit(AsteroidsKeyRight), c.Literal(uint64(1)),
					c.If(bit(AsteroidsKeyLeft), c.Arithmetic("sub", c.Literal(astRotSteps), c.Literal(uint64(1))),
						c.Literal(uint64(0))))),
			c.Literal(astRotSteps)),
	}, c.Let(map[string]*entitysdk.Builder{
		"nvx": c.If(bit(AsteroidsKeyThrust),
			c.Arithmetic("add", at("vxs", sc("i")),
				c.Index(c.Literal(astCosTable(astThrustAcc)), sc("nrot"))),
			at("vxs", sc("i"))),
		"nvy": c.If(bit(AsteroidsKeyThrust),
			c.Arithmetic("add", at("vys", sc("i")),
				c.Index(c.Literal(astSinTable(astThrustAcc)), sc("nrot"))),
			at("vys", sc("i"))),
	}, c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
		"kind": c.If(shipHit, c.Literal(AsteroidsFree), c.Literal(AsteroidsShip)),
		"x":    wrap(at("xs", sc("i")), sc("nvx")),
		"y":    wrap(at("ys", sc("i")), sc("nvy")),
		"vx":   sc("nvx"), "vy": sc("nvy"),
		"rot": sc("nrot"), "sz": c.Literal(uint64(0)),
		"ttl": c.Literal(uint64(0)),
	})))

	// asteroid: drift + wrap; on hit either shrink in place (sz>1) or die.
	asteroidThink := c.If(bulletHits(sc("i")),
		c.If(c.Compare("gt", at("szs", sc("i")), c.Literal(uint64(1))),
			// shrink in place; velocity deflects one rotation step.
			c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
				"kind": c.Literal(AsteroidsAsteroid),
				"x":    at("xs", sc("i")), "y": at("ys", sc("i")),
				"vx":  c.Arithmetic("sub", c.Literal(int64(0)), at("vys", sc("i"))),
				"vy":  at("vxs", sc("i")),
				"rot": at("rots", sc("i")),
				"sz":  c.Arithmetic("sub", at("szs", sc("i")), c.Literal(uint64(1))),
				"ttl": c.Literal(uint64(0)),
			}),
			free),
		c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
			"kind": c.Literal(AsteroidsAsteroid),
			"x":    wrap(at("xs", sc("i")), at("vxs", sc("i"))),
			"y":    wrap(at("ys", sc("i")), at("vys", sc("i"))),
			"vx":   at("vxs", sc("i")), "vy": at("vys", sc("i")),
			"rot": at("rots", sc("i")), "sz": at("szs", sc("i")),
			"ttl": c.Literal(uint64(0)),
		}))

	// bullet: move + wrap, age out, die on impact.
	bulletThink := c.If(
		c.Logic("or",
			c.Compare("lte", at("ttls", sc("i")), c.Literal(uint64(1))),
			bulletSpent(sc("i"))),
		free,
		c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
			"kind": c.Literal(AsteroidsBullet),
			"x":    wrap(at("xs", sc("i")), at("vxs", sc("i"))),
			"y":    wrap(at("ys", sc("i")), at("vys", sc("i"))),
			"vx":   at("vxs", sc("i")), "vy": at("vys", sc("i")),
			"rot": at("rots", sc("i")), "sz": c.Literal(uint64(0)),
			"ttl": c.Arithmetic("sub", at("ttls", sc("i")), c.Literal(uint64(1))),
		}))

	// free slot: claim a spawn by rank, else stay free. All the expensive work
	// (rank, splits) sits INSIDE branches per F-E2b where it is affordable to,
	// but `rank` is needed by both arms so it binds once here.
	freeThink := c.Let(map[string]*entitysdk.Builder{
		"r": rank(sc("i")),
	}, c.If(c.Logic("and", sc("firing"), c.Compare("eq", sc("r"), c.Literal(uint64(0)))),
		// the new bullet: spawns at the ship's nose, along the ship's heading.
		c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
			"kind": c.Literal(AsteroidsBullet),
			"x": wrap(at("xs", c.Literal(uint64(0))),
				c.Index(c.Literal(astCosTable(astBulletSpd)), at("rots", c.Literal(uint64(0))))),
			"y": wrap(at("ys", c.Literal(uint64(0))),
				c.Index(c.Literal(astSinTable(astBulletSpd)), at("rots", c.Literal(uint64(0))))),
			"vx":  c.Index(c.Literal(astCosTable(astBulletSpd)), at("rots", c.Literal(uint64(0)))),
			"vy":  c.Index(c.Literal(astSinTable(astBulletSpd)), at("rots", c.Literal(uint64(0)))),
			"rot": at("rots", c.Literal(uint64(0))),
			"sz":  c.Literal(uint64(0)),
			"ttl": c.Literal(astBulletTTL),
		}),
		// otherwise: the j-th split claims this slot, if there is a j-th split.
		// F-E2a — index(splits, j) is ONLY evaluated inside the lt guard.
		c.Let(map[string]*entitysdk.Builder{
			"j": c.Arithmetic("sub", sc("r"),
				c.If(sc("firing"), c.Literal(uint64(1)), c.Literal(uint64(0)))),
		}, c.If(c.Compare("lt", sc("j"), c.Length(sc("splits"))),
			c.Let(map[string]*entitysdk.Builder{
				"src": c.Index(sc("splits"), sc("j")),
			}, c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
				"kind": c.Literal(AsteroidsAsteroid),
				"x":    at("xs", sc("src")), "y": at("ys", sc("src")),
				// the mirror half: deflects opposite the in-place half.
				"vx":  at("vys", sc("src")),
				"vy":  c.Arithmetic("sub", c.Literal(int64(0)), at("vxs", sc("src"))),
				"rot": at("rots", sc("src")),
				"sz":  c.Arithmetic("sub", at("szs", sc("src")), c.Literal(uint64(1))),
				"ttl": c.Literal(uint64(0)),
			})),
			free))))

	// --- the actor map (array-of-structs, IN-FLIGHT) ------------------------
	//
	// Heterogeneous dispatch = a chain of `if` on kind. Arch's expectation
	// (§3.3: "is `if` enough?") — confirmed, see F-F4.
	actorFn := c.Lambda([]string{"i"},
		c.If(c.Compare("eq", at("kinds", sc("i")), c.Literal(AsteroidsShip)), shipThink,
			c.If(c.Compare("eq", at("kinds", sc("i")), c.Literal(AsteroidsAsteroid)), asteroidThink,
				c.If(c.Compare("eq", at("kinds", sc("i")), c.Literal(AsteroidsBullet)), bulletThink,
					freeThink))))

	// project(field): pull one field out of the in-flight actor structs into a
	// flat array. compute/field works directly on *constructedValue, so the
	// per-actor logic above runs ONCE and these are cheap.
	project := func(field string) *entitysdk.Builder {
		return c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": sc("acts"),
			"fn":         c.Lambda([]string{"a"}, c.Field(sc("a"), field)),
		})
	}

	// scored: how many asteroids died this tick (hit and size 1).
	scored := c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
		"collection": idxLit,
		"fn": c.Lambda([]string{"j"},
			c.Logic("and",
				c.Logic("and",
					c.Compare("eq", at("kinds", sc("j")), c.Literal(AsteroidsAsteroid)),
					c.Compare("eq", at("szs", sc("j")), c.Literal(uint64(1)))),
				bulletHits(sc("j")))),
	}))

	alive := c.Let(map[string]*entitysdk.Builder{
		// the ONLY place the shard range appears: this eval produces its slots.
		"acts": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": slotLit,
			"fn":         actorFn,
		}),
		// ⚠️ HIGH bits — (s>>16) % astRotSteps. F-D3: the low bits of a
		// power-of-two-modulus LCG have period 2^k and look plausible.
		"nrng": c.Arithmetic("mod",
			c.Arithmetic("add",
				c.Arithmetic("mul", sc("rng"), c.Literal(astLCGMul)),
				c.Literal(astLCGAdd)),
			c.Literal(astLCGMod)),
	}, c.Construct(astStateType, map[string]*entitysdk.Builder{
		"kinds": project("kind"),
		"xs":    project("x"), "ys": project("y"),
		"vxs": project("vx"), "vys": project("vy"),
		"rots": project("rot"), "szs": project("sz"), "ttls": project("ttl"),
		"rng":   sc("nrng"),
		"score": c.Arithmetic("add", sc("score"), scored),
		"status": c.If(shipHit, c.Literal(uint64(1)),
			c.Literal(uint64(0))),
	}))

	// F-E2b: `firing`/`splits` bind INSIDE the live branch. let is eager, so
	// binding them outside would run the whole O(N^2) split scan on every tick
	// of a finished game.
	live := c.Let(map[string]*entitysdk.Builder{
		"firing": bit(AsteroidsKeyFire),
		// let* evaluates in SORTED name order, so "hitb" lands before "splits"
		// and splits can see it. The name is chosen to sort that way.
		"hitb": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": idxLit,
			"fn":         c.Lambda([]string{"j"}, bulletHitsRaw(sc("j"))),
		}),
		"splits": splits,
	}, alive)

	// The frozen-when-dead tick returns `s` verbatim — same entity, same hash
	// (cf. Exp-E E4). A SHARD must not: `s` is the whole state, and returning it
	// as a fragment would hand the host a full state to stitch as a strip. Only
	// the full step owns the dead check; the shard rig asserts a live game.
	body := live
	if full {
		body = c.If(c.Compare("eq", sc("status"), c.Literal(uint64(1))), sc("s"), live)
	}

	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"keys":   c.Field(c.LookupTreeLocal(inputPath), "keys"),
		"kinds":  c.Field(sc("s"), "kinds"),
		"rng":    c.Field(sc("s"), "rng"),
		"rots":   c.Field(sc("s"), "rots"),
		"score":  c.Field(sc("s"), "score"),
		"status": c.Field(sc("s"), "status"),
		"szs":    c.Field(sc("s"), "szs"),
		"ttls":   c.Field(sc("s"), "ttls"),
		"vxs":    c.Field(sc("s"), "vxs"),
		"vys":    c.Field(sc("s"), "vys"),
		"xs":     c.Field(sc("s"), "xs"),
		"ys":     c.Field(sc("s"), "ys"),
	}, body))
}

// UNITS (not FP sub-units), so it can multiply a per-unit trig table without
// any division. Mirrors astShipRadius/astAstRadius/1-unit bullets.
func astRadiusUnitsExpr(c *entitysdk.ComputeBuilder, kind, sz *entitysdk.Builder) *entitysdk.Builder {
	return c.If(c.Compare("eq", kind, c.Literal(AsteroidsAsteroid)),
		c.Arithmetic("mul", c.Literal(int64(6)), sz),
		c.If(c.Compare("eq", kind, c.Literal(AsteroidsShip)),
			c.Literal(int64(3)),
			c.Literal(int64(1)))) // bullet (and free slots, unused)
}

// buildAsteroidsDisplayList lowers the DISPLAY-LIST output port: for each slot
// it emits a kind tag plus a world-space quad (4 vertices), rotated by the
// actor's heading and scaled by its radius.
//
// The renderer's contract becomes "draw these polylines, colour by kind" — a
// fixed vocabulary that is identical for Asteroids, Snake, Life, or Doom's
// automap. No asteroid-specific knowledge crosses the boundary.
//
// Note the trig table is scaled by astFP (one world unit), so a vertex offset
// is table[idx] * radiusUnits — still no division anywhere.
func buildAsteroidsDisplayExpr(ap *entitysdk.AppPeer, statePath string) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope

	indices := make([]uint64, astCapacity)
	for i := range indices {
		indices[i] = uint64(i)
	}
	unitCos := c.Literal(astCosTable(astFP))
	unitSin := c.Literal(astSinTable(astFP))

	at := func(arr string, i *entitysdk.Builder) *entitysdk.Builder {
		return c.Index(sc(arr), i)
	}

	// The SHIP is drawn as an arrowhead so its heading is visible; everything
	// else is a symmetric diamond.
	//
	// This is worth noticing as a port result, not just a cosmetic fix: a
	// diamond has its vertices 90 degrees apart, so a rotated diamond is the
	// SAME diamond and the ship's heading was invisible on screen. The fix is
	// here, in the PROGRAM, because the outline of a ship is the program's
	// business — the renderer only draws a polyline through whatever vertices
	// arrive. Making the ship directional changed zero lines of C#.
	//
	// Offsets are in 1/16 turns from the heading; radii are in whole world
	// units (the trig table is pre-scaled by one unit, so offset = table[idx] *
	// radiusUnits and no division appears).
	//   nose (0) --- 6 --- tail notch (8) --- 10 --- back to nose
	//
	// The drawn outline is deliberately LARGER than the ship's 3-unit collision
	// radius (astShipRadius): the nose reaches 9 units. That is the classic
	// forgiving hitbox — you die when the ship's BODY is hit, not when the nose
	// tip clips an asteroid — and it is a display decision, so it lives here in
	// the display list and does not touch the step.
	shipVertOff := []uint64{0, 6, 8, 10}
	shipVertRad := []int64{9, 4, 3, 4}

	// vertex k: heading + this kind's k-th offset, at this kind's k-th radius.
	vert := func(tbl *entitysdk.Builder, axis string, k int) *entitysdk.Builder {
		isShip := c.Compare("eq", at("kinds", sc("i")), c.Literal(AsteroidsShip))
		off := c.If(isShip,
			c.Literal(shipVertOff[k]),
			c.Literal(uint64(k)*(astRotSteps/4)))
		radu := c.If(isShip, c.Literal(shipVertRad[k]), sc("radu"))
		idx := c.Arithmetic("mod",
			c.Arithmetic("add", at("rots", sc("i")), off),
			c.Literal(astRotSteps))
		return c.Arithmetic("add", at(axis, sc("i")),
			c.Arithmetic("mul", c.Index(tbl, idx), radu))
	}

	quadFields := map[string]*entitysdk.Builder{
		"kind": at("kinds", sc("i")),
	}
	for k := 0; k < 4; k++ {
		quadFields[fmt.Sprintf("x%d", k)] = vert(unitCos, "xs", k)
		quadFields[fmt.Sprintf("y%d", k)] = vert(unitSin, "ys", k)
	}

	quadFn := c.Lambda([]string{"i"},
		c.Let(map[string]*entitysdk.Builder{
			"radu": astRadiusUnitsExpr(c, at("kinds", sc("i")), at("szs", sc("i"))),
		}, c.Construct(astDisplayType+"/quad", quadFields)))

	project := func(field string) *entitysdk.Builder {
		return c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": sc("quads"),
			"fn":         c.Lambda([]string{"q"}, c.Field(sc("q"), field)),
		})
	}

	outFields := map[string]*entitysdk.Builder{"kinds": project("kind")}
	for k := 0; k < 4; k++ {
		outFields[fmt.Sprintf("x%d", k)] = project(fmt.Sprintf("x%d", k))
		outFields[fmt.Sprintf("y%d", k)] = project(fmt.Sprintf("y%d", k))
	}

	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"kinds": c.Field(sc("s"), "kinds"),
		"rots":  c.Field(sc("s"), "rots"),
		"szs":   c.Field(sc("s"), "szs"),
		"xs":    c.Field(sc("s"), "xs"),
		"ys":    c.Field(sc("s"), "ys"),
	}, c.Let(map[string]*entitysdk.Builder{
		"quads": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         quadFn,
		}),
	}, c.Construct(astDisplayType, outFields))))
}

// buildAsteroidsFramebuffer lowers the FRAMEBUFFER output port: a fbW x fbH
// paletted bitmap, one `kind` per pixel (0 = background). The renderer blits
// and knows nothing whatsoever about the program.
//
// This is the shape Doom's port would be, and the cost shape is the point: a
// pure (state) -> frame function cannot SCATTER into a mutable buffer, so it
// must GATHER — every output pixel asks "which actors cover me?" — making the
// port O(pixels x LIVE actors) rather than O(actors).
//
// Note "LIVE", not "slots": the first version of this measurement gathered over
// all CAP slots and recomputed per-actor constants inside the pixel loop, which
// inflated the price 13x and produced a materially wrong conclusion (F6 is the
