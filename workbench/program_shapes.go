package workbench

// THE SHAPE DRIVER REGISTRY — the I/O ABI.
//
// Exploration §4/§6. `role` is the coarse hint; **`shape` selects the driver.**
// The vocabulary is grounded in the lineage of computer I/O (text → raster →
// vector → audio; keyboard → pointer → events), not in games — which is why
// `text` is here and first-class, and why the set is a REGISTRY EXTENDED BY A
// RULE rather than a closed enum:
//
//	name the interface lineage → pin what the port carries + its scene fields
//	→ define what a driver does with it.
//
// Nothing else changes: the mount loop is shape-agnostic and dispatches on the
// field. Touch, VR, haptics, camera-in join exactly this way, without touching
// the descriptor format or any existing program.
//
// **Admission is what makes the open vocabulary transfer-safe** (§6): a host
// advertises the shapes it drives; mounting checks the descriptor's declared
// shapes against that set. A host that lacks a shape refuses the whole program
// and says why — never a half-render. That is why `Mount` calls `admit()` before
// it seeds anything.
//
// ─── Why these decoders live here and not in a panel ───────────────────────
//
// A driver binds to a SHAPE, not to a program. `DecodeTextFrame` knows what a
// character grid is; it does not know what Life is. That is the whole
// distinction the §2 ruling turned on: a "program-aware render" driver IS
// per-program code wearing a driver's coat, which is why `raw-state` was demoted
// to a declared escape hatch the falsification does not run through.
//
// The per-program knowledge did not vanish — it MOVED, into the compute
// projection that turns a Life grid into a character grid (program_authoring.go).
// It now lives in the tree, content-addressed and transferable, instead of in a
// C# panel that only Avalonia can run. That relocation is the thesis of this
// whole rung.

import (
	"fmt"
	"sort"

	"go.entitychurch.org/entity-core-go/core/ecf"

	"github.com/fxamacker/cbor/v2"
)

// Shape entity types — what each port carries (exploration §4's table).
const (
	TextFrameType   = "app/shape/text-frame"
	DisplayListType = "app/shape/display-list"
	KeySetType      = "app/shape/key-set"
	DirectionType   = "app/shape/direction"
)

// ─── Why these types carry BOTH cbor and json tags ────────────────────────
//
// A shape payload crosses two seams: CBOR into the tree (what the program
// constructs and the host stores) and JSON out to a renderer (the bridge).
// They are different wire formats with different field-name rules, and a struct
// that names its fields for only one of them is silently wrong on the other.
//
// This bit us exactly once, and it is worth recording HOW: with cbor tags only,
// encoding/json fell back to Go field names (`{"Cols":16,...}`) while the C#
// driver read `cols`. System.Text.Json is case-sensitive by default, so every
// field deserialized to zero, Cols==0, and the renderer bailed before drawing.
//
// The failure mode is the point: the smoke log said `tick 150 · running ·
// shapes: text` and looked perfect, because `shape` and `ticks` live on the
// wrapper DTOs which DID have json tags. Only the screenshot showed a blank
// board. A green log and an empty screen — which is why the Avalonia
// validation looks at pixels and not just at logs.

// TextFrame is what a `text` port carries: a character grid.
//
// Lineage: teletype → glass TTY → VT100. The oldest interface there is, and the
// one the game probes never needed — which is exactly why it was the gap.
//
// Cells are code points in row-major order, len == Cols*Rows. Code points
// rather than bytes because a grid cell is a character, not a byte; the driver
// renders them and needs no idea what they mean.
type TextFrame struct {
	Cols  uint64   `cbor:"cols" json:"cols"`
	Rows  uint64   `cbor:"rows" json:"rows"`
	Cells []uint64 `cbor:"cells" json:"cells"`
}

// Scene fields for `text`: mode ("grid" | "stream"), cursor (optional).
const (
	TextModeGrid   = "grid"
	TextModeStream = "stream"
)

// DisplayList is what a `display-list` port carries: closed quads with kind tags.
//
// Lineage: the vector display. Asteroids ran on a literal one, so the fit is
// exact rather than analogical. Resolution-independent and O(actors) — which is
// why it beats a framebuffer at any resolution worth drawing.
//
// Struct-of-arrays: drawable i is the CLOSED polygon
// (X0[i],Y0[i]) → (X1[i],Y1[i]) → (X2[i],Y2[i]) → (X3[i],Y3[i]) → back to 0,
// tagged Kinds[i]. Same SoA reasoning as the state entity: an array of
// constructed values materializes each element to its own content-addressed
// entity and puts 33-byte hash refs in the array.
//
// ─── The 4-vertex limit is real, and it is a finding ───────────────────────
//
// Arch's exploration §4 specifies this shape as carrying `[{verts, kind}]` —
// verts plural and variable, i.e. an arbitrary polyline. What the shipped
// Asteroids expression actually emits is FIXED at four
// (program_asteroids.go's `for k := 0; k < 4` over x0..x3/y0..y3), because
// every drawable is a quad: an arrowhead for the ship, a diamond for
// everything else.
//
// So the shape as pinned here is narrower than the shape as specified. A
// program wanting a triangle or an octagon cannot express it, and a driver
// written to this struct would silently drop the extra vertices — which is
// exactly how this bit us: the first cut of this struct declared only x0/y0/x1/
// y1, CBOR quietly dropped x2/y2/x3/y3 (a decoder does not know what it was not
// told to expect), and the renderer drew ONE EDGE of each quad. It looked like
// a scattering of unrelated diagonal strokes and nothing failed.
//
// Reported to arch: either the shape is `quad-list` and should say so, or it is
// a real variable-length polyline and needs a nested-array lowering the current
// primitive set may not reach.
type DisplayList struct {
	Kinds []uint64 `cbor:"kinds" json:"kinds"`
	X0    []int64  `cbor:"x0" json:"x0"`
	Y0    []int64  `cbor:"y0" json:"y0"`
	X1    []int64  `cbor:"x1" json:"x1"`
	Y1    []int64  `cbor:"y1" json:"y1"`
	X2    []int64  `cbor:"x2" json:"x2"`
	Y2    []int64  `cbor:"y2" json:"y2"`
	X3    []int64  `cbor:"x3" json:"x3"`
	Y3    []int64  `cbor:"y3" json:"y3"`
}

// Verts returns drawable i's four vertices in winding order. A driver draws the
// closed polygon through them.
func (d DisplayList) Verts(i int) [4][2]int64 {
	return [4][2]int64{
		{d.X0[i], d.Y0[i]},
		{d.X1[i], d.Y1[i]},
		{d.X2[i], d.Y2[i]},
		{d.X3[i], d.Y3[i]},
	}
}

// Scene fields for `display-list`: wrap (seam-tile), bounds (world extent).
//
// `wrap` is the field that proved scene properties are necessary at all: an
// actor's centre wraps around the world seam but its outline must be tiled at
// the edge, and no renderer can infer that from vertices alone.

// KeySet is what a `key-set` port carries: a held-key bitmask snapshot.
//
// Lineage: the keyboard — but the HELD half of it. Typed characters are a
// stream; held keys are a snapshot. Two shapes, one device, which is why the §8
// input row wanted splitting: discrete events and held state are different
// things and held state belongs on the snapshot side.
type KeySet struct {
	Bits uint64 `cbor:"bits" json:"bits"`
}

// Direction is what a `direction` port carries: a latched enum snapshot.
//
// Lineage: the d-pad / menu pick — a latched choice, not an ordered event.
type Direction struct {
	Dir uint64 `cbor:"dir" json:"dir"`
}

// The `direction` enum is part of the SHAPE contract, not any program's.
//
// A blind driver has to turn an arrow key into a number, so the shape must fix
// what the number means — otherwise every program would need its own driver to
// say "for me, 2 is down", and the driver stops being blind. This is the
// smallest possible instance of the whole vocabulary argument: a shape is only
// an ABI if its payload means the same thing on every host.
//
// Snake's SnakeUp/Right/Down/Left already agree with these; that is a
// coincidence we are now pinning rather than relying on.
const (
	DirUp    = uint64(0)
	DirRight = uint64(1)
	DirDown  = uint64(2)
	DirLeft  = uint64(3)
)

// shapeDrivers is the set of shapes this host can drive. A shape is present iff
// this host can bind a port of that shape to real I/O.
//
// `raw-state` is deliberately ABSENT. It remains in the vocabulary
// (ShapeRawState) as the declared escape hatch, but this host does not advertise
// it, so a descriptor that binds an output port to `raw-state` is refused at
// admission rather than mounted. That is the §2 ruling made mechanical: the
// falsification cannot run through the hatch even by accident.
var shapeDrivers = map[string]bool{
	ShapeText:        true,
	ShapeDisplayList: true,
	ShapeKeySet:      true,
	ShapeDirection:   true,
}

// DriverSupports reports whether this host can drive the shape.
func DriverSupports(shape string) bool { return shapeDrivers[shape] }

// SupportedShapes lists the shapes this host advertises, sorted. This is the
// host's capability advertisement — the thing admission checks against.
func SupportedShapes() []string {
	out := make([]string, 0, len(shapeDrivers))
	for s := range shapeDrivers {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// DecodeTextFrame decodes a `text` port's value. Shape-aware, program-blind.
func DecodeTextFrame(pv PortValue) (TextFrame, error) {
	var tf TextFrame
	if pv.Shape != ShapeText {
		return tf, fmt.Errorf("port %q is shape %q, not %q", pv.Name, pv.Shape, ShapeText)
	}
	if err := ecf.Decode(pv.Data, &tf); err != nil {
		return tf, fmt.Errorf("port %q: decode text frame: %w", pv.Name, err)
	}
	if uint64(len(tf.Cells)) != tf.Cols*tf.Rows {
		return tf, fmt.Errorf("port %q: text frame is %dx%d but carries %d cells",
			pv.Name, tf.Cols, tf.Rows, len(tf.Cells))
	}
	return tf, nil
}

// DecodeDisplayList decodes a `display-list` port's value.
func DecodeDisplayList(pv PortValue) (DisplayList, error) {
	var dl DisplayList
	if pv.Shape != ShapeDisplayList {
		return dl, fmt.Errorf("port %q is shape %q, not %q", pv.Name, pv.Shape, ShapeDisplayList)
	}
	if err := ecf.Decode(pv.Data, &dl); err != nil {
		return dl, fmt.Errorf("port %q: decode display list: %w", pv.Name, err)
	}
	// Check EVERY vertex array against kinds. The first cut of this checked only
	// x0/y0/x1/y1 — the four it knew about — so when x2/y2/x3/y3 were silently
	// dropped by the decoder, this passed and the renderer drew one edge per
	// quad. A validator that only checks the fields you remembered is not a
	// validator; it is a restatement of your assumption.
	n := len(dl.Kinds)
	for _, a := range []struct {
		name string
		got  int
	}{
		{"x0", len(dl.X0)}, {"y0", len(dl.Y0)},
		{"x1", len(dl.X1)}, {"y1", len(dl.Y1)},
		{"x2", len(dl.X2)}, {"y2", len(dl.Y2)},
		{"x3", len(dl.X3)}, {"y3", len(dl.Y3)},
	} {
		if a.got != n {
			return dl, fmt.Errorf("port %q: display list has %d kinds but %d %s — "+
				"a quad needs all four vertices", pv.Name, n, a.got, a.name)
		}
	}
	return dl, nil
}

// SceneBool reads a bool scene field, with a default when absent.
func SceneBool(scene map[string]interface{}, key string, def bool) bool {
	if scene == nil {
		return def
	}
	if v, ok := scene[key].(bool); ok {
		return v
	}
	return def
}

// SceneString reads a string scene field, with a default when absent.
func SceneString(scene map[string]interface{}, key, def string) string {
	if scene == nil {
		return def
	}
	if v, ok := scene[key].(string); ok {
		return v
	}
	return def
}

// EncodeKeySet encodes a held-key bitmask for an input port write.
func EncodeKeySet(bits uint64) (string, cbor.RawMessage, error) {
	raw, err := ecf.Encode(KeySet{Bits: bits})
	if err != nil {
		return "", nil, err
	}
	return KeySetType, cbor.RawMessage(raw), nil
}

// EncodeDirection encodes a latched direction for an input port write.
func EncodeDirection(dir uint64) (string, cbor.RawMessage, error) {
	raw, err := ecf.Encode(Direction{Dir: dir})
	if err != nil {
		return "", nil, err
	}
	return DirectionType, cbor.RawMessage(raw), nil
}
