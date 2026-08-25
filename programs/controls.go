package programs

// THE STANDARD CONTROLLER — the input PRESENTATION contract.
//
// Raised by the browser host (PROPOSAL-GENERIC-HOST-INPUT-DEVICE-MODEL), and it
// is a real gap in what we author. The problem, exactly:
//
//	A generic host binds a port to a driver by its SHAPE. But a `key-set` port's
//	scene.keymap only NAMES its bits (`{"0":"left","3":"fire"}`) — it does not
//	say which bits are DIRECTIONAL (rotate/thrust → belong on a d-pad, where
//	simultaneous presses are natural) and which are DISCRETE ACTIONS (fire → a
//	button). So a host either renders one bespoke button per bit (Asteroids came
//	out as four unlabeled buttons — the "looks app-specific" complaint) or it
//	guesses from the bit's NAME STRING, which is exactly the app-aware coloring a
//	blind host must not do.
//
// The fix keeps TRANSPORT and splits PRESENTATION:
//
//	Transport stays the shapes we have. `key-set`'s bitmask is still the wire
//	form for simultaneous held inputs (Asteroids genuinely needs thrust + rotate
//	+ fire at once, which the single-valued `direction` shape cannot express).
//	Presentation is driven by a declared CONTROL ROLE per bit. Directional bits
//	render on the ONE standard directional control (a d-pad that allows
//	simultaneous presses, satisfying rotate+thrust); action bits render as
//	buttons, labelled/glyphed from the manifest. Every program gets the SAME
//	controller; only the bindings differ.
//
// So this file is the generalization of `scene.keymap`: from `{bit → name}` to
// `{bit → control role}`. It is the REFERENCE parser — a generic host (browser,
// native, Godot, Avalonia) mirrors ParseKeymap's semantics, so keep them
// byte/behaviour-identical across impls the same way the direction enum is. See
// docs/architecture/reviews/RESPONSE-GENERIC-HOST-INPUT-DEVICE-MODEL-2026-07-24.md.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Control roles — how a key-set bit presents on the standard controller.
const (
	ControlRoleAxis   = "axis"   // a directional input → the d-pad / stick
	ControlRoleAction = "action" // a discrete action → a button
)

// Axis positions on the standard directional control. These are the SAME four
// positions the `direction` shape's enum names (DirUp/Right/Down/Left) — a
// directional key-set bit and a `direction` port drive the identical control;
// only the transport differs (an accumulating bitmask vs. a latched value).
const (
	AxisUp    = "up"
	AxisDown  = "down"
	AxisLeft  = "left"
	AxisRight = "right"
)

// Action behaviours — how a host treats a held vs. tapped action button.
const (
	BehaviorMomentary = "momentary" // bit set while held, cleared on release (fire, thrust-as-action)
	BehaviorToggle    = "toggle"    // one tap flips the bit (pause)
)

// The standard action vocabulary. A small well-known set so common actions
// render consistently across programs and a host's overflow policy is
// predictable. A program MAY use an action name outside this set; it just does
// not get a default glyph. Labels/glyphs on the binding always win over these.
const (
	ActionFire    = "fire"
	ActionStart   = "start"
	ActionSelect  = "select"
	ActionPause   = "pause"
	ActionRestart = "restart"
)

// StandardActionGlyph is the host-agnostic default glyph for a standard action,
// used when a binding declares none. A host may substitute its own iconography,
// but the DEFAULT is pinned so every implementer's unstyled controller reads the
// same.
var StandardActionGlyph = map[string]string{
	ActionFire:    "\U0001F525", // 🔥
	ActionStart:   "▶",          // ▶
	ActionSelect:  "◉",          // ◉
	ActionPause:   "⏸",          // ⏸
	ActionRestart: "↻",          // ↻
}

// validAxis reports whether s is one of the four axis positions.
func validAxis(s string) bool {
	switch s {
	case AxisUp, AxisDown, AxisLeft, AxisRight:
		return true
	}
	return false
}

// ControlBinding is one key-set bit's presentation role, parsed from a
// scene.keymap entry. The bit stays the transport (bit i in the held mask is
// 1<<i); this says how a host renders it on the standard controller and — for
// the transport layer — what NAME a source presses to set it.
type ControlBinding struct {
	Bit  int    // bit index in the held-key mask (mask contribution = 1<<Bit)
	Role string // ControlRoleAxis | ControlRoleAction

	// Axis is the d-pad position (ControlRoleAxis only).
	Axis string

	// Action is the action name (ControlRoleAction only).
	Action string

	// Label / Glyph present the control. For an action button: the button face.
	// For an axis, presentation is the d-pad position itself, so these are
	// usually empty. Glyph falls back to StandardActionGlyph[Action] when empty.
	Label string
	Glyph string

	// Behavior is BehaviorMomentary (default) or BehaviorToggle.
	Behavior string
}

// Name is the token a source presses/releases to drive this bit. For an axis it
// is the axis position ("up"); for an action it is the action name ("fire").
// This is what feeds the action→bit map the transport layer holds — a host maps
// a physical input (arrow key, d-pad button) to a Name, never to a bit directly.
func (b ControlBinding) Name() string {
	if b.Role == ControlRoleAxis {
		return b.Axis
	}
	return b.Action
}

// EffectiveGlyph is the glyph to render, falling back to the standard-action
// default. Empty for an axis (the d-pad position is self-presenting) with no
// declared glyph.
func (b ControlBinding) EffectiveGlyph() string {
	if b.Glyph != "" {
		return b.Glyph
	}
	if b.Role == ControlRoleAction {
		return StandardActionGlyph[b.Action]
	}
	return ""
}

// ParseKeymap reads a key-set port's scene.keymap into per-bit control bindings,
// sorted by bit. This is the reference parser the standard-controller contract
// is defined by; a generic host mirrors these semantics.
//
// It accepts BOTH forms so programs migrate incrementally:
//
//	legacy: {"<bit>": "<name>"}                       — a momentary ACTION named <name>
//	roled:  {"<bit>": {role, axis|action, label?, glyph?, behavior?}}
//
// The legacy string form is exactly today's Asteroids keymap; it reads as an
// action so an un-migrated program keeps working (as four buttons), which is the
// honest reflection of a program that has not declared roles yet.
//
// scene is assumed already normalized (DecodeDescriptor normalizes every scene),
// so nested maps are string-keyed. A malformed entry is an error, not a silent
// drop — a control the author declared but a host cannot parse must surface.
func ParseKeymap(scene map[string]interface{}) ([]ControlBinding, error) {
	if scene == nil {
		return nil, nil
	}
	raw, ok := scene["keymap"]
	if !ok || raw == nil {
		return nil, nil
	}
	km, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("scene.keymap is %T, want a {bit: role} map", raw)
	}

	out := make([]ControlBinding, 0, len(km))
	for bitKey, v := range km {
		bit, err := strconv.Atoi(bitKey)
		if err != nil || bit < 0 || bit > 63 {
			return nil, fmt.Errorf("scene.keymap key %q is not a bit index 0..63", bitKey)
		}
		b, err := parseBinding(bit, v)
		if err != nil {
			return nil, fmt.Errorf("scene.keymap[%d]: %w", bit, err)
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bit < out[j].Bit })
	return out, nil
}

func parseBinding(bit int, v interface{}) (ControlBinding, error) {
	switch t := v.(type) {
	case string:
		// Legacy form: a bare action name.
		if t == "" {
			return ControlBinding{}, fmt.Errorf("empty action name")
		}
		return ControlBinding{
			Bit:      bit,
			Role:     ControlRoleAction,
			Action:   t,
			Label:    titleCase(t),
			Behavior: BehaviorMomentary,
		}, nil
	case map[string]interface{}:
		b := ControlBinding{Bit: bit}
		b.Role, _ = t["role"].(string)
		switch b.Role {
		case ControlRoleAxis:
			b.Axis, _ = t["axis"].(string)
			if !validAxis(b.Axis) {
				return b, fmt.Errorf("axis role needs axis up|down|left|right, got %q", b.Axis)
			}
		case ControlRoleAction:
			b.Action, _ = t["action"].(string)
			if b.Action == "" {
				return b, fmt.Errorf("action role needs a non-empty action name")
			}
			b.Label, _ = t["label"].(string)
			if b.Label == "" {
				b.Label = titleCase(b.Action)
			}
			b.Glyph, _ = t["glyph"].(string)
			b.Behavior, _ = t["behavior"].(string)
			if b.Behavior == "" {
				b.Behavior = BehaviorMomentary
			}
			if b.Behavior != BehaviorMomentary && b.Behavior != BehaviorToggle {
				return b, fmt.Errorf("unknown behavior %q", b.Behavior)
			}
		default:
			return b, fmt.Errorf("role must be %q or %q, got %q", ControlRoleAxis, ControlRoleAction, b.Role)
		}
		return b, nil
	default:
		return ControlBinding{}, fmt.Errorf("entry is %T, want a name string or a {role:...} map", v)
	}
}

// ActionBitEntry pairs a press-token with its held-mask contribution — the
// transport layer's action→bit map (the browser's InputTarget holds the same
// pairing). A source presses Name; the target ORs Bit into the held mask.
type ActionBitEntry struct {
	Name string
	Bit  uint64
}

// ActionBitMap projects control bindings to the name→bit pairs the transport
// layer needs, ordered by bit. Axis bindings contribute their position name,
// action bindings their action name — so a host maps a physical input to a Name
// and never needs to know a bit number. This is the seam the control-role split
// preserves: presentation gains roles; the held-mask transport is unchanged.
func ActionBitMap(bindings []ControlBinding) []ActionBitEntry {
	out := make([]ActionBitEntry, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, ActionBitEntry{Name: b.Name(), Bit: uint64(1) << uint(b.Bit)})
	}
	return out
}

// titleCase upper-cases the first rune — a plain default label from an action
// name ("fire" → "Fire"). A host that wants richer labels declares them.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
