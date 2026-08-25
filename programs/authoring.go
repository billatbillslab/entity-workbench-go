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

// Glyphs the text projections emit. Code points, not bytes — a grid cell is a
// character (program_shapes.go).
const (
	glyphDead  = uint64('.')
	glyphAlive = uint64('#')
	glyphFood  = uint64('*')
)

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
		input:       root + "/input",
		input0:      root + "/input0",
		descriptor:  root + DescriptorLeaf,
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

	// The text projection — the per-program knowledge that used to be in C#.
	if _, err := buildLifeTextExpr(ap, w, h, p.state).Build(context.Background(), p.displayExpr); err != nil {
		return "", fmt.Errorf("AuthorLife: build text projection: %w", err)
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

// buildLifeTextExpr projects a Life grid to a character grid.
//
// This is Life bound to `text` (arch §2). A Life grid IS a character grid — the
// I/O-lineage argument applied consistently: Life was published as a pencil grid
// in Scientific American, and the same reasoning that makes Asteroids a
// display-list (it ran on a literal vector display) makes Life text.
//
// A straight value map: cells carry 0/1, so no index is needed. Snake's
// projection cannot do this (see buildSnakeTextExpr).
func buildLifeTextExpr(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
	c := ap.Compute()
	return c.Let(map[string]*entitysdk.Builder{
		"g": c.LookupTreeLocal(statePath),
	}, c.Construct(TextFrameType, map[string]*entitysdk.Builder{
		"cols": c.Literal(uint64(w)),
		"rows": c.Literal(uint64(h)),
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Field(c.LookupScope("g"), "cells"),
			"fn": c.Lambda([]string{"v"}, c.If(
				c.Compare("eq", c.LookupScope("v"), c.Literal(uint64(1))),
				c.Literal(glyphAlive),
				c.Literal(glyphDead))),
		}),
	}))
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
	if _, err := buildSnakeTextExpr(ap, w, h, p.state).Build(context.Background(), p.displayExpr); err != nil {
		return "", fmt.Errorf("AuthorSnake: build text projection: %w", err)
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
	}
	return writeDescriptor(ap, p.descriptor, d)
}

// buildSnakeTextExpr projects a Snake state to a character grid.
//
// Unlike Life's, this projection CANNOT be a straight value map: food is a
// separate index in state, so a cell's glyph depends on its INDEX, not just its
// value. So it maps over a literal index array — `[0…N-1]`, materialized in Go
// at authoring time.
//
// That literal is exactly what `compute/range` retires (arch reclassified it
// load-bearing, §1 ruling). It is worth noticing where it turned up: not in the
// sharding contract that forced the reclassification, but in an ordinary
// projection. Two independent uses is a decent argument the primitive is real.
func buildSnakeTextExpr(ap *entitysdk.AppPeer, w, h int, statePath string) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope

	indices := make([]uint64, w*h)
	for i := range indices {
		indices[i] = uint64(i)
	}

	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"cells": c.Field(sc("s"), "cells"),
		"food":  c.Field(sc("s"), "food"),
	}, c.Construct(TextFrameType, map[string]*entitysdk.Builder{
		"cols": c.Literal(uint64(w)),
		"rows": c.Literal(uint64(h)),
		"cells": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn": c.Lambda([]string{"i"}, c.If(
				c.Compare("eq", sc("i"), sc("food")),
				c.Literal(glyphFood),
				c.If(
					c.Compare("gt", c.Index(sc("cells"), sc("i")), c.Literal(uint64(0))),
					c.Literal(glyphAlive),
					c.Literal(glyphDead)))),
		}),
	})))
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
				// keymap: bit → semantic action. The driver ORs held keys into
				// this bitmask; it never learns what "thrust" does.
				"keymap": map[string]interface{}{
					"0": "left", "1": "right", "2": "thrust", "3": "fire",
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
			},
		}},
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
