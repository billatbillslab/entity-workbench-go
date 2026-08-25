package programs

// AUTHORING — writing a program into the tree as durable, content-addressed
// artifacts, so a host can mount it without ever calling a Go builder.
//
// This file is the other half of arch's §3 ruling, and the ruling's point is the
// SPLIT, not the descriptors:
//
//	Before: NewLifeGameModel() both ASSEMBLED the step IR (buildLifeStepExpr)
//	        and RAN it. The program was reconstructed by Go at every boot. It
//	        was in the tree without being addressable as a program — nobody
//	        could pick it up and wire it to their own front-end.
//
//	After:  Author*() runs ONCE and leaves (step IR + projections + state₀ +
//	        descriptor) in the tree. Mount() reads the descriptor and runs the
//	        program. The host calls no builder and imports no program symbol.
//
// **Phase 2 depends on this completely.** A Rust host fetches (descriptor + step
// IR + initial_state) by hash and cannot run `buildAsteroidsStepExpr`. If the
// builders stayed at boot, "fetch by hash" would have nothing to fetch and
// transferable compute would stay an assertion. That is why this is deliverable
// 1 and the three descriptor files are deliverable 3.
//
// **Per-program Go lives HERE and that is correct.** The falsifiable claim is
// that the *host* has none (program_host.go). Authoring is per-program by
// definition — it is how a program gets written. What matters is that it runs
// once, ahead of time, and that its OUTPUT is what the host consumes.
//
// ─── Where the per-program knowledge went ──────────────────────────────────
//
// Binding Life and Snake to `text` (arch's §2 ruling) is not a relabel. The
// 0/1 → glyph mapping used to live in a C# panel. Now it is a compute
// projection, in the tree, content-addressed, and a Rust host gets it for free
// by fetching the program. The per-program knowledge did not disappear — it
// moved from a renderer nobody else can run into the program itself. That
// relocation IS the rung.

import (
	"context"
	"fmt"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"

	"github.com/fxamacker/cbor/v2"

	"entity-workbench-go/entitysdk"
)

// Display-list kinds for the grid programs. A `kind` is a COLOUR INDEX into the
// host's pen palette (shapes.go), never an object type — the driver stays
// program-blind. The grid projections are DENSE (one quad per cell — see
// buildGridDisplayExpr for why), so an empty cell is carried as kind 0
// (DisplayKindBackground), which the host does not draw.
//
// Life/Snake were bound to `text` (arch §2's I/O-lineage ruling — a Life grid is
// a pencil grid). In the generic GUI host that renders as a `<pre>` character
// grid — a terminal, legible but not presentable. Rebound to `display-list` (a
// filled grid of cell quads); the §2 revisit is routed to arch in the response
// doc. The head distinction Snake's state already makes — thrown away by the old
// text projection — survives as its own kind.
const (
	lifeKindAlive = uint64(1) // a live Life cell

	snakeKindBody = uint64(1) // a snake body segment
	snakeKindHead = uint64(2) // the snake's head (its state already marks it)
	snakeKindFood = uint64(3) // the food pellet
)

// Status-line glyphs — the run-state indicator on a program's `text` status
// port. Code points, like the grid glyphs, rendered by the blind text driver.
const (
	glyphStatusPlay = uint64('▶') // U+25B6 — running / alive
	glyphStatusOver = uint64('✖') // U+2716 — game over / dead / extinct
)

// buildStatusTextExpr builds a program's `text` STATUS port: one line —
// a static LABEL, a VALUE as a fixed-width decimal, and a run-state GLYPH.
//
// This is the "score/state to make it complete" surface, and it is deliberately
// PROGRAM-OWNED, not host-owned: the projection formats the program's OWN state
// into a text line, and the generic host relays it blind (it already drives
// `text`) exactly as it relays the display port. The host never learns what a
// "score" is — a program that wants a readout declares one, a simulation that
// doesn't simply omits the port. Formatting lives in the tree, so every renderer
// shows the byte-identical line (internal consistency by construction). It also
// gives the `text` shape the genuine exemplar it lost when Life/Snake moved their
// DISPLAY to display-list.
//
// The compute DSL has no string or concat primitive (concat is arch-gated), so
// the line is assembled by an indexed map over fixed positions — the same
// pattern as the grid projections: each position is a static label char, a
// computed decimal digit, or the status glyph. `value` and `statusGlyph` are
// built by the caller over the state binding "s"; `value` is also bound as "v"
// so the caller's glyph expression can read it (Life's "extinct" is value==0).
func buildStatusTextExpr(c *entitysdk.ComputeBuilder, statePath, label string, digits int, value, statusGlyph *entitysdk.Builder) *entitysdk.Builder {
	sc := c.LookupScope
	runes := []rune(label)
	w := len(runes) + digits + 2 // [label][digits][space][glyph]

	static := make([]uint64, w) // code point at a static position, else 0
	kind := make([]uint64, w)   // 0 = static, 1 = digit, 2 = glyph
	pow := make([]uint64, w)    // 10^p at a digit position
	for i, r := range runes {
		static[i] = uint64(r)
	}
	for k := 0; k < digits; k++ {
		pos := len(runes) + k
		kind[pos] = 1
		p := uint64(1)
		for j := 0; j < digits-1-k; j++ {
			p *= 10
		}
		pow[pos] = p // leftmost digit = highest power
	}
	static[len(runes)+digits] = uint64(' ')
	kind[w-1] = 2

	positions := make([]uint64, w)
	for i := range positions {
		positions[i] = uint64(i)
	}

	// decimal digit at position i: (v / pow[i]) mod 10 + '0'. compute `div` is
	// TRUE division (a non-integer quotient makes `mod` fault), so floor-divide
	// first — a - (a mod b) is divisible by b — exactly as Asteroids does.
	floorDiv := func(a, b *entitysdk.Builder) *entitysdk.Builder {
		return c.Arithmetic("div", c.Arithmetic("sub", a, c.Arithmetic("mod", a, b)), b)
	}
	digitAt := func(i *entitysdk.Builder) *entitysdk.Builder {
		return c.Arithmetic("add",
			c.Arithmetic("mod",
				floorDiv(sc("v"), c.Index(c.Literal(pow), i)),
				c.Literal(uint64(10))),
			c.Literal(uint64('0')))
	}

	cells := c.BuiltinsCall("map", map[string]*entitysdk.Builder{
		"collection": c.Literal(positions),
		"fn": c.Lambda([]string{"i"}, c.If(
			c.Compare("eq", c.Index(c.Literal(kind), sc("i")), c.Literal(uint64(1))),
			digitAt(sc("i")),
			c.If(
				c.Compare("eq", c.Index(c.Literal(kind), sc("i")), c.Literal(uint64(2))),
				statusGlyph,
				c.Index(c.Literal(static), sc("i"))))),
	})

	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"v": value,
	}, c.Construct(TextFrameType, map[string]*entitysdk.Builder{
		"cols":  c.Literal(uint64(w)),
		"rows":  c.Literal(uint64(1)),
		"cells": cells,
	})))
}

// LifeRoot etc. are the well-known program roots. Descriptor discovery (arch's
// §7 Q3) is an open question; phase 1 mounts by a pinned path convention —
// `<root>/interface` — and reports it.
const (
	LifeRoot      = "app/life"
	SnakeRoot     = "app/snake"
	AsteroidsRoot = "app/asteroids"

	// DescriptorLeaf is the pinned well-known leaf under a program root.
	DescriptorLeaf = "/interface"
)

// programPaths is the standard artifact layout under a program root. The
// authoring step writes these; the descriptor names them; the host only ever
// learns them from the descriptor.
type programPaths struct {
	root        string
	state       string // live state — the step writes here every tick
	state0      string // state₀ — immutable; Restart restores from it
	step        string // the step expression
	displayExpr string // the display projection expression
	display     string // the materialized display port value
	statusExpr  string // the status-line projection expression
	status      string // the materialized status port value
	input       string // the live input port
	input0      string // the input port's F-E1 seed
	descriptor  string
}

func pathsFor(root string) programPaths {
	return programPaths{
		root:        root,
		state:       root + "/state",
		state0:      root + "/state0",
		step:        root + "/step",
		displayExpr: root + "/display-expr",
		display:     root + "/display",
		statusExpr:  root + "/status-expr",
		status:      root + "/status",
		input:       root + "/input",
		input0:      root + "/input0",
		descriptor:  root + DescriptorLeaf,
	}
}

// statusPort is the shared declaration of a program's `text` status port — the
// second output port, a one-line readout the host relays blind. Programs differ
// only in the projection at statusExpr (label + value + state glyph).
func statusPort(p programPaths) ProgramPort {
	return ProgramPort{
		Name:    "status",
		Path:    p.status,
		Source:  p.statusExpr,
		TypeRef: TextFrameType,
		Kind:    KindSnapshot,
		Role:    RoleDisplay,
		Shape:   ShapeText,
		Scene: map[string]interface{}{
			// A single status line, not a grid. A renderer shows it as a caption
			// beside/under the display (it is the program's own score/state, so
			// the host stays out of it).
			"mode": TextModeStream,
			"rows": uint64(1),
		},
	}
}

// AuthorLife writes the Life program into the tree and returns its descriptor
// path. Idempotent: authoring the same seed twice writes identical artifacts
// (the IR is content-addressed; state₀ is a pure function of rngSeed).
//
// Life has NO input ports — F-E1 is per-port, and Life simply has none, which
// is the same evidence that ports are a per-program property rather than a
// runtime tax.
func AuthorLife(ap *entitysdk.AppPeer, root string, rngSeed uint64) (string, error) {
	if ap == nil {
		return "", fmt.Errorf("AuthorLife: nil AppPeer")
	}
	if rngSeed == 0 {
		return "", fmt.Errorf("AuthorLife: rngSeed must be non-zero (state₀ must be reproducible)")
	}
	const w, h = lifeWidth, lifeHeight
	p := pathsFor(root)

	// state₀ — authored, not host-constructed. The host only relocates it.
	s0 := lifeSeedState(w, h, rngSeed)
	ent, err := lifeStateEntity(s0)
	if err != nil {
		return "", fmt.Errorf("AuthorLife: state₀: %w", err)
	}
	state0Hash, err := ap.PutEntity(p.state0, ent)
	if err != nil {
		return "", fmt.Errorf("AuthorLife: put state₀: %w", err)
	}

	// The step IR — authored once, durable, content-addressed.
	stepHash, err := buildLifeStepExpr(ap, w, h, p.state).Build(context.Background(), p.step)
	if err != nil {
		return "", fmt.Errorf("AuthorLife: build step: %w", err)
	}

	// The display projection — the per-program knowledge that used to be in C#.
	if _, err := buildLifeDisplayExpr(ap, w, h, p.state).Build(context.Background(), p.displayExpr); err != nil {
		return "", fmt.Errorf("AuthorLife: build display projection: %w", err)
	}

	// The status line — population (a fold over the grid) + an alive/extinct
	// glyph. Program-owned; the host relays it blind.
	{
		c := ap.Compute()
		sc := c.LookupScope
		pop := c.BuiltinsCall("fold", map[string]*entitysdk.Builder{
			"collection": c.Field(sc("s"), "cells"),
			"initial":    c.Literal(uint64(0)),
			"fn": c.Lambda([]string{"acc", "elem"},
				c.Arithmetic("add", sc("acc"), sc("elem"))),
		})
		glyph := c.If(c.Compare("eq", sc("v"), c.Literal(uint64(0))),
			c.Literal(glyphStatusOver), c.Literal(glyphStatusPlay))
		if _, err := buildStatusTextExpr(c, p.state, "POP ", 4, pop, glyph).
			Build(context.Background(), p.statusExpr); err != nil {
			return "", fmt.Errorf("AuthorLife: build status projection: %w", err)
		}
	}

	d := &ProgramDescriptor{
		StatePath:        p.state,
		InitialState:     p.state0,
		Step:             p.step,
		StepHash:         hashString(stepHash),
		InitialStateHash: hashString(state0Hash),
		OutputPorts: []ProgramPort{{
			Name:    "display",
			Path:    p.display,
			Source:  p.displayExpr,
			TypeRef: DisplayListType,
			Kind:    KindSnapshot,
			Role:    RoleDisplay,
			Shape:   ShapeDisplayList,
			Scene: map[string]interface{}{
				// Filled grid of cell quads (shapes.go presentation contract): the
				// host reads `render:fill` and paints solid cells, not wireframe.
				// `bounds` is the square world extent — the grid is w cells wide,
				// each a unit quad, so the SVG/viewport is w×w.
				"render": RenderFill,
				"bounds": uint64(w),
			},
		}, statusPort(p)},
		Tick: ProgramTick{
			Mode:     TickClockDriven,
			RateHint: 6, // was hard-coded in the host; a transferable program carries its own clock
			// OpCost is deliberately UNSET. The Axis-1 number for arith Life at
			// 16x16 is 153 ops/cell, and this is arith Life at 16x16 — but that
			// was measured on exp_compute_life_test.go's lowering, not on this
			// one, and we have not re-measured it here. Unused by the base host
			// (no sharding), so an unverified value would be pure downside:
			// absent reads as "unknown", a wrong value reads as authoritative.
			//
			// This is not fastidiousness — it is arch's §3.2 ask showing up as a
			// concrete blocker. op_cost cannot be authored honestly today: the
			// only way to measure eval cost is to bisect the budget cap (17
			// evals per number). A descriptor field that asks the author for a
			// number the platform gives no way to measure will be populated by
			// guesswork. See the phase-1 report.
		},
	}
	return writeDescriptor(ap, p.descriptor, d)
}

// buildGridDisplayExpr projects a w×h grid program to a display-list of cell
// quads. It is the shared skeleton for Life and Snake — the two grids agree on
// everything except WHICH kind occupies each cell, so the caller supplies only a
// `kinds` expression (a length-w*h per-cell kind array, kind 0 = empty).
//
// ─── Dense, constant geometry — and why ────────────────────────────────────
//
// The list is DENSE — one quad per cell, every cell — and the quad geometry is a
// pure function of the cell index (col = i mod w, row = i div w, unit edge), so
// the corner coordinates are CONSTANT: authored once as literals, zero per-tick
// eval. Only the kinds recompute. An empty cell carries kind 0
// (DisplayKindBackground), which the host does not draw (shapes.go).
//
// This is deliberate, and it is a measured choice. The first cut was SPARSE (a
// `filter` to drop empty cells + a per-cell quad `Construct` + a projection pass
// per output array), mirroring the Asteroids live-actor lowering. But Asteroids
// has ~24 actors and a grid has thousands of cells: the sparse lowering cost
// ~10x the old text projection and blew the compute budget at 64×64
// (budget_exhausted on the single display eval, which is not sharded). Dense
// constant-geometry is as cheap as the text projection was — one kinds map plus
// constant coordinate arrays — so it fits the same budget the text projection
// did. The O(changed)-per-tick optimisation is the separate subtree-state track,
// not this projection's job.
func buildGridDisplayExpr(c *entitysdk.ComputeBuilder, statePath string, w, h int, kinds *entitysdk.Builder) *entitysdk.Builder {
	n := w * h
	x0 := make([]int64, n)
	y0 := make([]int64, n)
	x1 := make([]int64, n)
	y1 := make([]int64, n)
	x2 := make([]int64, n)
	y2 := make([]int64, n)
	x3 := make([]int64, n)
	y3 := make([]int64, n)
	for i := 0; i < n; i++ {
		col, row := int64(i%w), int64(i/w)
		x0[i], y0[i] = col, row     // top-left
		x1[i], y1[i] = col+1, row   // top-right
		x2[i], y2[i] = col+1, row+1 // bottom-right
		x3[i], y3[i] = col, row+1   // bottom-left
	}

	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Construct(DisplayListType, map[string]*entitysdk.Builder{
		"kinds": kinds,
		"x0":    c.Literal(x0), "y0": c.Literal(y0),
		"x1": c.Literal(x1), "y1": c.Literal(y1),
		"x2": c.Literal(x2), "y2": c.Literal(y2),
		"x3": c.Literal(x3), "y3": c.Literal(y3),
	}))
}

// buildLifeDisplayExpr projects a Life grid to filled cell quads. Life's kind is
// a straight value map: a cell carries 0/1, so kind = alive when the cell is 1,
// else empty. (Snake's cannot — food/head depend on the index, not the value.)
func buildLifeDisplayExpr(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope
	kinds := c.BuiltinsCall("map", map[string]*entitysdk.Builder{
		"collection": c.Field(sc("s"), "cells"),
		"fn": c.Lambda([]string{"v"}, c.If(
			c.Compare("eq", sc("v"), c.Literal(uint64(1))),
			c.Literal(lifeKindAlive),
			c.Literal(DisplayKindBackground))),
	})
	return buildGridDisplayExpr(c, statePath, w, h, kinds)
}

// AuthorSnake writes the Snake program into the tree and returns its descriptor
// path.
//
// Snake is the program that proves the INPUT half of the vocabulary: one
// `direction` input port, latched (a snapshot, not a stream — a held/latched
// choice, not an ordered event).
func AuthorSnake(ap *entitysdk.AppPeer, root string, rngSeed uint64) (string, error) {
	if ap == nil {
		return "", fmt.Errorf("AuthorSnake: nil AppPeer")
	}
	if rngSeed == 0 {
		return "", fmt.Errorf("AuthorSnake: rngSeed must be non-zero (state₀ must be reproducible)")
	}
	const w, h = snakeWidth, snakeHeight
	p := pathsFor(root)

	s0 := snakeSeedState(w, h, rngSeed)
	ent, err := snakeStateEntity(s0)
	if err != nil {
		return "", fmt.Errorf("AuthorSnake: state₀: %w", err)
	}
	state0Hash, err := ap.PutEntity(p.state0, ent)
	if err != nil {
		return "", fmt.Errorf("AuthorSnake: put state₀: %w", err)
	}

	// The input port's F-E1 seed, in the SHAPE's canonical type — see
	// authorInputSeed's note on why this is not the program's private type.
	if err := authorInputSeed(ap, p.input0, DirectionType, map[string]interface{}{"dir": SnakeRight}); err != nil {
		return "", fmt.Errorf("AuthorSnake: input seed: %w", err)
	}

	stepHash, err := buildSnakeStepExpr(ap, w, h, p.state, p.input).Build(context.Background(), p.step)
	if err != nil {
		return "", fmt.Errorf("AuthorSnake: build step: %w", err)
	}
	if _, err := buildSnakeDisplayExpr(ap, w, h, p.state).Build(context.Background(), p.displayExpr); err != nil {
		return "", fmt.Errorf("AuthorSnake: build display projection: %w", err)
	}

	// The status line — snake length + a playing/dead glyph (Snake state carries
	// `status`: 0 playing, 1 dead).
	{
		c := ap.Compute()
		sc := c.LookupScope
		length := c.Field(sc("s"), "length")
		glyph := c.If(c.Compare("eq", c.Field(sc("s"), "status"), c.Literal(uint64(1))),
			c.Literal(glyphStatusOver), c.Literal(glyphStatusPlay))
		if _, err := buildStatusTextExpr(c, p.state, "LEN ", 3, length, glyph).
			Build(context.Background(), p.statusExpr); err != nil {
			return "", fmt.Errorf("AuthorSnake: build status projection: %w", err)
		}
	}

	d := &ProgramDescriptor{
		StatePath:        p.state,
		InitialState:     p.state0,
		Step:             p.step,
		StepHash:         hashString(stepHash),
		InitialStateHash: hashString(state0Hash),
		InputPorts: []ProgramPort{{
			Name:    "dir",
			Path:    p.input,
			Initial: p.input0,
			TypeRef: DirectionType,
			Kind:    KindSnapshot,
			Role:    RoleInput,
			Shape:   ShapeDirection,
		}},
		OutputPorts: []ProgramPort{{
			Name:    "display",
			Path:    p.display,
			Source:  p.displayExpr,
			TypeRef: DisplayListType,
			Kind:    KindSnapshot,
			Role:    RoleDisplay,
			Shape:   ShapeDisplayList,
			Scene: map[string]interface{}{
				// Filled grid (shapes.go): head/body/food are distinct kinds →
				// distinct colours. `bounds` is the square world extent (w cells).
				"render": RenderFill,
				"bounds": uint64(w),
			},
		}, statusPort(p)},
		Tick: ProgramTick{Mode: TickClockDriven, RateHint: 6},
	}
	return writeDescriptor(ap, p.descriptor, d)
}

// buildSnakeDisplayExpr projects a Snake state to filled cell quads.
//
// Unlike Life's, the kind CANNOT be a straight value map: food and head are
// separate indices in state, so a cell's kind depends on its INDEX, not just its
// value. So the kinds are a map over a literal index array — `[0…N-1]` — testing
// each index. Priority: food → head → body → empty (the head cell is also a body
// cell, so it must be tested before the body test).
//
// ─── The head was in the state and thrown away ─────────────────────────────
//
// Snake state carries `head` (the head cell's index; the head cell also holds
// the max body timer — snake.go). The old text projection collapsed EVERY
// occupied cell to one glyph, so the snake rendered as an undifferentiated blob
// with no visible head. The head is a free distinction the state already makes;
// here it is its own kind (its own colour), so the snake reads head-first.
func buildSnakeDisplayExpr(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope

	n := w * h
	indices := make([]uint64, n)
	for i := range indices {
		indices[i] = uint64(i)
	}

	kinds := c.Let(map[string]*entitysdk.Builder{
		"cells": c.Field(sc("s"), "cells"),
		"food":  c.Field(sc("s"), "food"),
		"head":  c.Field(sc("s"), "head"),
	}, c.BuiltinsCall("map", map[string]*entitysdk.Builder{
		"collection": c.Literal(indices),
		"fn": c.Lambda([]string{"i"}, c.If(
			c.Compare("eq", sc("i"), sc("food")),
			c.Literal(snakeKindFood),
			c.If(
				c.Compare("eq", sc("i"), sc("head")),
				c.Literal(snakeKindHead),
				c.If(
					c.Compare("gt", c.Index(sc("cells"), sc("i")), c.Literal(uint64(0))),
					c.Literal(snakeKindBody),
					c.Literal(DisplayKindBackground))))),
	}))
	return buildGridDisplayExpr(c, statePath, w, h, kinds)
}

// AuthorAsteroids writes the Asteroids program into the tree and returns its
// descriptor path.
//
// Asteroids is the stress case and the reason this rung is falsifiable at all:
// it is the only one of the three whose descriptor carries non-obvious content —
// a `key-set` input (a snapshot of a SET, not a latched choice), a
// `display-list` output with a scene property no renderer can infer (`wrap`),
// and a rate the host used to hard-code. Life and Snake are grids, where the
// state IS the display, which made the port question look settled when it was a
// coincidence.
func AuthorAsteroids(ap *entitysdk.AppPeer, root string, rngSeed uint64) (string, error) {
	if ap == nil {
		return "", fmt.Errorf("AuthorAsteroids: nil AppPeer")
	}
	if rngSeed == 0 {
		return "", fmt.Errorf("AuthorAsteroids: rngSeed must be non-zero (state₀ must be reproducible)")
	}
	p := pathsFor(root)

	s0 := astSeedState(rngSeed)
	ent, err := astStateEntity(s0)
	if err != nil {
		return "", fmt.Errorf("AuthorAsteroids: state₀: %w", err)
	}
	state0Hash, err := ap.PutEntity(p.state0, ent)
	if err != nil {
		return "", fmt.Errorf("AuthorAsteroids: put state₀: %w", err)
	}

	if err := authorInputSeed(ap, p.input0, KeySetType, map[string]interface{}{"keys": uint64(0)}); err != nil {
		return "", fmt.Errorf("AuthorAsteroids: input seed: %w", err)
	}

	stepHash, err := buildAsteroidsStepExpr(ap, p.state, p.input).Build(context.Background(), p.step)
	if err != nil {
		return "", fmt.Errorf("AuthorAsteroids: build step: %w", err)
	}
	if _, err := buildAsteroidsDisplayExpr(ap, p.state, DisplayListType).Build(context.Background(), p.displayExpr); err != nil {
		return "", fmt.Errorf("AuthorAsteroids: build display: %w", err)
	}

	// The status line — score + a playing/destroyed glyph (Asteroids state carries
	// `score` and `status`: 0 playing, 1 ship destroyed).
	{
		c := ap.Compute()
		sc := c.LookupScope
		score := c.Field(sc("s"), "score")
		glyph := c.If(c.Compare("eq", c.Field(sc("s"), "status"), c.Literal(uint64(1))),
			c.Literal(glyphStatusOver), c.Literal(glyphStatusPlay))
		if _, err := buildStatusTextExpr(c, p.state, "SCORE ", 5, score, glyph).
			Build(context.Background(), p.statusExpr); err != nil {
			return "", fmt.Errorf("AuthorAsteroids: build status projection: %w", err)
		}
	}

	d := &ProgramDescriptor{
		StatePath:        p.state,
		InitialState:     p.state0,
		Step:             p.step,
		StepHash:         hashString(stepHash),
		InitialStateHash: hashString(state0Hash),
		InputPorts: []ProgramPort{{
			Name:    "keys",
			Path:    p.input,
			Initial: p.input0,
			TypeRef: KeySetType,
			Kind:    KindSnapshot,
			Role:    RoleInput,
			Shape:   ShapeKeySet,
			Scene: map[string]interface{}{
				// keymap: bit → CONTROL ROLE (the standard-controller contract,
				// controls.go). The bitmask is still the transport — the driver
				// ORs held bits and never learns what a bit does — but the ROLE
				// tells a host how to present the bit: a directional bit lands on
				// the one standard d-pad (which allows simultaneous presses, so
				// rotate+thrust works), a discrete action becomes a labelled
				// button. Bit positions are unchanged (the step reads bit k by
				// position, asteroids.go); only presentation is declared.
				//
				// This is the re-declaration the browser host asked for: rotate
				// (left/right) and thrust ARE directional and belong on the
				// d-pad; only fire is a genuine action. Same controller as every
				// other program; only the bindings differ.
				"keymap": map[string]interface{}{
					"0": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisLeft},  // rotate left
					"1": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisRight}, // rotate right
					"2": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisUp},    // thrust (forward)
					"3": map[string]interface{}{
						"role":     ControlRoleAction,
						"action":   ActionFire,
						"label":    "Fire",
						"glyph":    StandardActionGlyph[ActionFire],
						"behavior": BehaviorMomentary,
					},
				},
			},
		}},
		OutputPorts: []ProgramPort{{
			Name:    "display",
			Path:    p.display,
			Source:  p.displayExpr,
			TypeRef: DisplayListType,
			Kind:    KindSnapshot,
			Role:    RoleDisplay,
			Shape:   ShapeDisplayList,
			Scene: map[string]interface{}{
				// wrap: the world is a TORUS. An actor's centre wraps but its
				// outline must be tiled at the seam — the renderer cannot learn
				// that from vertices. This is the field that proved scene
				// properties are necessary at all.
				"wrap":   true,
				"bounds": uint64(astWorld),
				// Wireframe — Asteroids ran on a vector display. This is the
				// default (shapes.go), stated explicitly to contrast the filled
				// grids.
				"render": RenderStroke,
			},
		}, statusPort(p)},
		Tick: ProgramTick{Mode: TickClockDriven, RateHint: 12},
	}
	return writeDescriptor(ap, p.descriptor, d)
}

// authorInputSeed writes an input port's F-E1 seed.
//
// The seed carries the SHAPE's canonical type (app/shape/direction,
// app/shape/key-set), not the program's private type (app/snake/input,
// app/asteroids/input). That is deliberate and it is an ABI decision:
//
//	If a port's type_ref were the program's own type, a driver would have to
//	construct a program-specific entity to write input — which makes the driver
//	program-aware, the exact thing the §2 ruling demoted `raw-state` for.
//
// It works because compute field access is STRUCTURAL, not typed: the step does
// Field(LookupTreeLocal(inputPath), "dir") and never inspects the entity's type
// (entity-core-go/ext/compute/eval.go:117 → evalField). So the shape owns the
// type and the field names; the program just reads them.
//
// Reported to arch: the proposal says type_ref is "the entity type carried" but
// does not say whose. It must be the shape's, or drivers cannot stay blind.
func authorInputSeed(ap *entitysdk.AppPeer, path, typeRef string, fields map[string]interface{}) error {
	raw, err := ecf.Encode(fields)
	if err != nil {
		return err
	}
	ent, err := entity.NewEntity(typeRef, cbor.RawMessage(raw))
	if err != nil {
		return err
	}
	if _, err := ap.PutEntity(path, ent); err != nil {
		return fmt.Errorf("put seed at %s: %w", path, err)
	}
	return nil
}

// writeDescriptor validates and puts a descriptor, returning its path. Validate
// before writing: a descriptor that cannot mount should never reach the tree.
func writeDescriptor(ap *entitysdk.AppPeer, path string, d *ProgramDescriptor) (string, error) {
	if err := d.Validate(); err != nil {
		return "", fmt.Errorf("authoring: %w", err)
	}
	ent, err := d.Entity()
	if err != nil {
		return "", err
	}
	if _, err := ap.PutEntity(path, ent); err != nil {
		return "", fmt.Errorf("authoring: put descriptor at %s: %w", path, err)
	}
	return path, nil
}

func hashString(h hash.Hash) string { return h.String() }
