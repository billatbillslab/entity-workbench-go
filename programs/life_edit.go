package programs

// INTERACTIVE LIFE — Conway's Life you can steer: regenerate the soup and edit
// live cells, all program-owned, mounted through the UNCHANGED generic host.
//
// Why this is a separate program from AuthorLife (app/life), not an edit of it:
//
//   - AuthorLife stays the phase-1 falsification fixture. TestMount_LifeMatches-
//     HardCodedModel proves mount(descriptor) computes the SAME bytes as the
//     boot-time LifeGameModel, tick for tick. That proof is about pure Life; an
//     interactive Life carries extra state (cursor/gen/pkeys/paused) and reads an
//     input port, so it is a DIFFERENT program and must not masquerade as that
//     fixture.
//   - A fourth program — with the richest input set yet (a d-pad + three action
//     buttons on one key-set) — mounting through program_host.go with ZERO host
//     change is more evidence the host is generic, not less. That is the whole
//     bet, dogfooded again.
//
// ─── What is interactive, and how it stays inside the input contract ────────
//
// One `key-set` input port (the standard controller, controls.go): four axis
// bits drive a d-pad that moves a cursor; three action bits toggle the cell under
// the cursor, regenerate the soup, and pause the simulation. Every one of those
// is a bit in the held-key mask the browser and Avalonia already drive today — no
// new shape, no host change, no admission gap. The program owns ALL of it: the
// host writes the mask blind and the step decides what a bit means.
//
// ─── The honest gap this does NOT close ─────────────────────────────────────
//
// A cursor moved by a d-pad is a real feature, but it is NOT a mouse click. A true
// click has to name WHICH cell — an (x,y)/index coordinate — and neither input
// shape we have (`key-set`, a 64-bit mask; `direction`, a 0..3 enum) can carry a
// coordinate. That is not a Life problem; it is the generic host missing its third
// input device. shapes.go's lineage note already anticipates it ("keyboard →
// pointer → events"). The pointer/`click` shape is routed to arch + the browser as
// a real capability gap of the host, tracked separately — this program is what we
// can honestly ship on the input vocabulary that exists, not a stand-in that
// pretends the gap is closed.

import (
	"context"
	"fmt"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"

	"github.com/fxamacker/cbor/v2"

	"entity-workbench-go/entitysdk"
)

// LifeEditRoot is the well-known root of the interactive Life program. It is a
// SIBLING of LifeRoot (app/life), not a child — app/life owns app/life/state
// etc., so the editable variant needs its own namespace.
const LifeEditRoot = "app/life-edit"

// Held-key bit positions for the interactive Life controller. Bits 0..3 are the
// d-pad (axis role); bits 4..6 are action buttons. The step reads each bit by
// position (lifeBit), exactly as Asteroids reads its keys — the mask is transport,
// the role is presentation (controls.go).
const (
	LifeKeyUp     = 0 // d-pad up    → cursor up
	LifeKeyDown   = 1 // d-pad down  → cursor down
	LifeKeyLeft   = 2 // d-pad left  → cursor left
	LifeKeyRight  = 3 // d-pad right → cursor right
	LifeKeyToggle = 4 // action: flip the cell under the cursor
	LifeKeyRegen  = 5 // action: regenerate the soup (new pattern)
	LifeKeyPause  = 6 // action: pause/resume the simulation
)

// Display kinds unique to interactive Life: the cursor is its own colour so you
// can see what you are about to edit, and it distinguishes an empty target cell
// from a live one (lifeKindAlive/DisplayKindBackground come from authoring.go).
const (
	lifeKindCursorDead  = uint64(2) // cursor over an empty cell
	lifeKindCursorAlive = uint64(3) // cursor over a live cell
)

// glyphStatusPause is the status-line glyph for a paused simulation (⏸, the
// standard pause action glyph, controls.go). ▶/✖ come from authoring.go.
const glyphStatusPause = uint64('⏸')

// lifeMixC mixes the generation counter with the cell index in the regen hash. A
// large odd multiplier (Knuth's 2654435761) so adjacent (gen, i) scatter — a
// per-index counter-based hash, not the sequential LCG lifeSeedState uses, because
// a `map` over indices cannot carry a running LCG state between cells.
const lifeMixC = uint64(2654435761)

// AuthorLifeInteractive writes the interactive Life program into the tree and
// returns its descriptor path. Same authoring contract as AuthorLife: state₀ +
// step IR + projections + descriptor land as durable content-addressed artifacts;
// the host reads the descriptor and runs it, calling no builder.
//
// rngSeed picks BOTH the opening soup and the starting generation counter, so a
// regen produces a different (but reproducible) board for a given seed.
func AuthorLifeInteractive(ap *entitysdk.AppPeer, root string, rngSeed uint64) (string, error) {
	if ap == nil {
		return "", fmt.Errorf("AuthorLifeInteractive: nil AppPeer")
	}
	if rngSeed == 0 {
		return "", fmt.Errorf("AuthorLifeInteractive: rngSeed must be non-zero (state₀ must be reproducible)")
	}
	const w, h = lifeWidth, lifeHeight
	p := pathsFor(root)

	// state₀ — the opening soup plus the editor fields (cursor at board centre,
	// gen seeded from rngSeed, no keys seen yet, running).
	cells := lifeSeedState(w, h, rngSeed).Cells
	cursor := uint64((h/2)*w + w/2)
	ent, err := lifeEditStateEntity(cells, w, h, cursor, rngSeed%lifeLCGMod)
	if err != nil {
		return "", fmt.Errorf("AuthorLifeInteractive: state₀: %w", err)
	}
	state0Hash, err := ap.PutEntity(p.state0, ent)
	if err != nil {
		return "", fmt.Errorf("AuthorLifeInteractive: put state₀: %w", err)
	}

	// The input port's F-E1 seed — an all-clear held-key mask, in the shape's
	// canonical type (see authorInputSeed).
	if err := authorInputSeed(ap, p.input0, KeySetType, map[string]interface{}{KeySetField: uint64(0)}); err != nil {
		return "", fmt.Errorf("AuthorLifeInteractive: input seed: %w", err)
	}

	stepHash, err := buildLifeEditStepExpr(ap, w, h, p.state, p.input).Build(context.Background(), p.step)
	if err != nil {
		return "", fmt.Errorf("AuthorLifeInteractive: build step: %w", err)
	}
	if _, err := buildLifeEditDisplayExpr(ap, w, h, p.state).Build(context.Background(), p.displayExpr); err != nil {
		return "", fmt.Errorf("AuthorLifeInteractive: build display: %w", err)
	}

	// The status line — population + a play/pause/extinct glyph. Program-owned;
	// the host relays it blind (same as every program's status port).
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
			c.Literal(glyphStatusOver),
			c.If(c.Compare("eq", c.Field(sc("s"), "paused"), c.Literal(uint64(1))),
				c.Literal(glyphStatusPause),
				c.Literal(glyphStatusPlay)))
		if _, err := buildStatusTextExpr(c, p.state, "POP ", 4, pop, glyph).
			Build(context.Background(), p.statusExpr); err != nil {
			return "", fmt.Errorf("AuthorLifeInteractive: build status projection: %w", err)
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
				// The standard controller (controls.go): the d-pad bits are axis
				// roles (move the cursor); toggle/regen/pause are action buttons.
				// toggle and regen are outside the standard action vocabulary, so
				// they carry explicit glyphs; pause is the standard ActionPause.
				"keymap": map[string]interface{}{
					"0": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisUp},
					"1": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisDown},
					"2": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisLeft},
					"3": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisRight},
					"4": map[string]interface{}{
						"role": ControlRoleAction, "action": "toggle",
						"label": "Toggle", "glyph": "✏", "behavior": BehaviorMomentary,
					},
					"5": map[string]interface{}{
						"role": ControlRoleAction, "action": "regen",
						"label": "Regen", "glyph": "\U0001F3B2", "behavior": BehaviorMomentary,
					},
					"6": map[string]interface{}{
						"role": ControlRoleAction, "action": ActionPause,
						"label": "Pause", "glyph": StandardActionGlyph[ActionPause], "behavior": BehaviorMomentary,
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
				"render": RenderFill,
				"bounds": uint64(w),
			},
		}, statusPort(p)},
		Tick: ProgramTick{Mode: TickClockDriven, RateHint: 6},
	}
	return writeDescriptor(ap, p.descriptor, d)
}

// lifeEditStateEntity builds an interactive Life state entity. It is an ordinary
// app/life/grid (so the status fold and the grid geometry stay shared) with the
// editor fields appended — compute field access is structural, so the extra
// fields cost the display/status projections nothing they do not read.
func lifeEditStateEntity(cells []uint64, w, h int, cursor, gen uint64) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{
		"width":  uint64(w),
		"height": uint64(h),
		"cells":  cells,
		"cursor": cursor,
		"gen":    gen,
		"pkeys":  uint64(0), // no keys seen yet (edge detection baseline)
		"paused": uint64(0), // running
	})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(lifeGridType, cbor.RawMessage(raw))
}

// buildLifeEditStepExpr lowers the interactive Life step: state' = f(state, keys).
//
// The whole step is EDGE-TRIGGERED off the previous held-key mask (pkeys, carried
// in state): an action fires on the tick a bit goes 0→1, not every tick it is
// held, so one button press does one thing. That is what makes the d-pad move the
// cursor one cell per tap and the toggle flip a cell once instead of strobing it.
//
// Priority when several fire at once: regen (replace the whole board) > toggle
// (edit one cell) > paused-hold (freeze) > the B3/S23 rule (advance). Cursor
// movement, gen++, pkeys and paused update every tick regardless.
func buildLifeEditStepExpr(ap *entitysdk.AppPeer, w, h int, statePath, inputPath string) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope
	W, H := uint64(w), uint64(h)
	n := w * h

	// integer floor-division (compute `div` is true division; F-D1).
	floorDiv := func(a, b *entitysdk.Builder) *entitysdk.Builder {
		return c.Arithmetic("div", c.Arithmetic("sub", a, c.Arithmetic("mod", a, b)), b)
	}
	// bit k of a named mask binding: mod(floorDiv(mask, 1<<k), 2) — the same
	// arithmetic bit test Asteroids uses (asteroids.go), no bitwise primitive.
	bit := func(mask string, k int) *entitysdk.Builder {
		return c.Arithmetic("mod", floorDiv(sc(mask), c.Literal(uint64(1)<<uint(k))), c.Literal(uint64(2)))
	}
	// pressed = rising edge: set now, clear in the previous mask.
	pressed := func(k int) *entitysdk.Builder {
		return c.Logic("and",
			c.Compare("eq", bit("keys", k), c.Literal(uint64(1))),
			c.Compare("eq", bit("pkeys", k), c.Literal(uint64(0))))
	}

	indices := make([]uint64, n)
	for i := range indices {
		indices[i] = uint64(i)
	}

	// The B3/S23 next-cells map, reading a "cells" binding in scope. This is the
	// same toroidal-neighbour arithmetic as buildLifeStepExpr (life.go) and
	// buildLifeShardFragment (life_shard.go); it is duplicated here on purpose,
	// the same product-copy relationship those two already have — change them
	// together if the rule ever moves.
	nextCells := func() *entitysdk.Builder {
		neighbor := func(dx, dy int) *entitysdk.Builder {
			nx := c.Arithmetic("mod",
				c.Arithmetic("add", sc("x"), c.Literal(uint64(w+dx))), c.Literal(W))
			ny := c.Arithmetic("mod",
				c.Arithmetic("add", sc("y"), c.Literal(uint64(h+dy))), c.Literal(H))
			ni := c.Arithmetic("add", c.Arithmetic("mul", ny, c.Literal(W)), nx)
			return c.Index(sc("cells"), ni)
		}
		terms := make([]*entitysdk.Builder, 0, 8)
		for _, dxy := range lifeOffsets {
			terms = append(terms, neighbor(dxy[0], dxy[1]))
		}
		perCell := c.Let(map[string]*entitysdk.Builder{
			"x": c.Arithmetic("mod", sc("i"), c.Literal(W)),
			"y": floorDiv(sc("i"), c.Literal(W)),
		}, c.Let(map[string]*entitysdk.Builder{
			"count": lifeAddTree(c, terms),
			"alive": c.Compare("eq", c.Index(sc("cells"), sc("i")), c.Literal(uint64(1))),
		}, lifeRule(c)))
		return c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         c.Lambda([]string{"i"}, perCell),
		})
	}

	// The regen soup: a per-index counter-based hash of (gen, i), thresholded at
	// the SAME 3/8 density lifeSeedState uses, so a regenerated board looks like
	// the opening one. Sequential-LCG soup is impossible under `map` (no carry),
	// so each cell hashes its own index against the generation counter.
	regenCells := c.BuiltinsCall("map", map[string]*entitysdk.Builder{
		"collection": c.Literal(indices),
		"fn": c.Lambda([]string{"i"}, c.Let(map[string]*entitysdk.Builder{
			"h0": c.Arithmetic("mod",
				c.Arithmetic("add",
					c.Arithmetic("mul", c.Arithmetic("mod", sc("gen"), c.Literal(lifeLCGMod)), c.Literal(lifeMixC)),
					sc("i")),
				c.Literal(lifeLCGMod)),
		}, c.Let(map[string]*entitysdk.Builder{
			"h1": c.Arithmetic("mod",
				c.Arithmetic("add", c.Arithmetic("mul", sc("h0"), c.Literal(lifeLCGMul)), c.Literal(lifeLCGAdd)),
				c.Literal(lifeLCGMod)),
		}, c.If(
			c.Compare("lt",
				c.Arithmetic("mod", floorDiv(sc("h1"), c.Literal(uint64(65536))), c.Literal(uint64(8))),
				c.Literal(uint64(3))),
			c.Literal(uint64(1)), c.Literal(uint64(0)))))),
	})

	// The toggle edit: flip the cell at the (post-move) cursor, all others held.
	// sub(1, v) flips 0↔1.
	toggleCells := c.BuiltinsCall("map", map[string]*entitysdk.Builder{
		"collection": c.Literal(indices),
		"fn": c.Lambda([]string{"i"}, c.If(
			c.Compare("eq", sc("i"), sc("ncursor")),
			c.Arithmetic("sub", c.Literal(uint64(1)), c.Index(sc("cells"), sc("i"))),
			c.Index(sc("cells"), sc("i")))),
	})

	// newCells resolves the priority ladder. npaused is the pause flag AFTER this
	// tick's pause press, so pressing pause freezes the board on the same tick.
	newCells := c.If(pressed(LifeKeyRegen), regenCells,
		c.If(pressed(LifeKeyToggle), toggleCells,
			c.If(c.Compare("eq", sc("npaused"), c.Literal(uint64(1))), sc("cells"),
				nextCells())))

	// The construct — the whole ladder plus the always-updated editor fields.
	body := c.Construct(lifeGridType, map[string]*entitysdk.Builder{
		"width":  c.Literal(W),
		"height": c.Literal(H),
		"cells":  newCells,
		"cursor": sc("ncursor"),
		"gen":    c.Arithmetic("mod", c.Arithmetic("add", sc("gen"), c.Literal(uint64(1))), c.Literal(lifeLCGMod)),
		"pkeys":  sc("keys"),
		"paused": sc("npaused"),
	})

	// Cursor movement: one cell per d-pad tap, wrapping within row/column. Built
	// as nested Lets (col/row → ncol/nrow → ncursor) so each layer can read the
	// previous, not relying on sibling sort-order.
	moved := c.Let(map[string]*entitysdk.Builder{
		"col": c.Arithmetic("mod", sc("cursor"), c.Literal(W)),
		"row": floorDiv(sc("cursor"), c.Literal(W)),
	}, c.Let(map[string]*entitysdk.Builder{
		"ncol": c.If(pressed(LifeKeyLeft), c.Arithmetic("mod", c.Arithmetic("add", sc("col"), c.Literal(W-1)), c.Literal(W)),
			c.If(pressed(LifeKeyRight), c.Arithmetic("mod", c.Arithmetic("add", sc("col"), c.Literal(uint64(1))), c.Literal(W)),
				sc("col"))),
		"nrow": c.If(pressed(LifeKeyUp), c.Arithmetic("mod", c.Arithmetic("add", sc("row"), c.Literal(H-1)), c.Literal(H)),
			c.If(pressed(LifeKeyDown), c.Arithmetic("mod", c.Arithmetic("add", sc("row"), c.Literal(uint64(1))), c.Literal(H)),
				sc("row"))),
	}, c.Let(map[string]*entitysdk.Builder{
		"ncursor": c.Arithmetic("add", c.Arithmetic("mul", sc("nrow"), c.Literal(W)), sc("ncol")),
		"npaused": c.If(pressed(LifeKeyPause),
			c.Arithmetic("sub", c.Literal(uint64(1)), sc("paused")), sc("paused")),
	}, body)))

	// Bind the state fields and the held-key mask once, then run.
	return c.Let(map[string]*entitysdk.Builder{
		"s":     c.LookupTreeLocal(statePath),
		"input": c.LookupTreeLocal(inputPath),
	}, c.Let(map[string]*entitysdk.Builder{
		"cells":  c.Field(sc("s"), "cells"),
		"cursor": c.Field(sc("s"), "cursor"),
		"gen":    c.Field(sc("s"), "gen"),
		"keys":   c.Field(sc("input"), KeySetField),
		"paused": c.Field(sc("s"), "paused"),
		"pkeys":  c.Field(sc("s"), "pkeys"),
	}, moved))
}

// buildLifeEditDisplayExpr projects interactive Life to filled cell quads, with
// the cursor cell in its own kind so the operator can see the edit target (and
// whether it is currently live). Priority: cursor → alive → empty.
func buildLifeEditDisplayExpr(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope
	n := w * h
	indices := make([]uint64, n)
	for i := range indices {
		indices[i] = uint64(i)
	}
	kinds := c.Let(map[string]*entitysdk.Builder{
		"cells":  c.Field(sc("s"), "cells"),
		"cursor": c.Field(sc("s"), "cursor"),
	}, c.BuiltinsCall("map", map[string]*entitysdk.Builder{
		"collection": c.Literal(indices),
		"fn": c.Lambda([]string{"i"}, c.Let(map[string]*entitysdk.Builder{
			"alive": c.Compare("eq", c.Index(sc("cells"), sc("i")), c.Literal(uint64(1))),
		}, c.If(c.Compare("eq", sc("i"), sc("cursor")),
			c.If(sc("alive"), c.Literal(lifeKindCursorAlive), c.Literal(lifeKindCursorDead)),
			c.If(sc("alive"), c.Literal(lifeKindAlive), c.Literal(DisplayKindBackground))))),
	}))
	return buildGridDisplayExpr(c, statePath, w, h, kinds)
}
