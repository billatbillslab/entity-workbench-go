package programs

import (
	"reflect"
	"testing"
)

// TestParseKeymap_RoledForm is the executable spec of the standard-controller
// contract: the re-declared Asteroids keymap parses to three axis bindings on
// the d-pad + one labelled fire action, with the held-mask transport (bit k =
// 1<<k) unchanged. A generic host mirrors these semantics.
func TestParseKeymap_RoledForm(t *testing.T) {
	scene := map[string]interface{}{
		"keymap": map[string]interface{}{
			"0": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisLeft},
			"1": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisRight},
			"2": map[string]interface{}{"role": ControlRoleAxis, "axis": AxisUp},
			"3": map[string]interface{}{
				"role": ControlRoleAction, "action": ActionFire,
				"label": "Fire", "glyph": StandardActionGlyph[ActionFire],
				"behavior": BehaviorMomentary,
			},
		},
	}
	got, err := ParseKeymap(scene)
	if err != nil {
		t.Fatalf("ParseKeymap: %v", err)
	}
	want := []ControlBinding{
		{Bit: 0, Role: ControlRoleAxis, Axis: AxisLeft},
		{Bit: 1, Role: ControlRoleAxis, Axis: AxisRight},
		{Bit: 2, Role: ControlRoleAxis, Axis: AxisUp},
		{Bit: 3, Role: ControlRoleAction, Action: ActionFire, Label: "Fire",
			Glyph: StandardActionGlyph[ActionFire], Behavior: BehaviorMomentary},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bindings:\n got %+v\nwant %+v", got, want)
	}

	// The transport seam is preserved: names route presses, bit k = 1<<k.
	abm := ActionBitMap(got)
	wantABM := []ActionBitEntry{
		{Name: "left", Bit: 1}, {Name: "right", Bit: 2},
		{Name: "up", Bit: 4}, {Name: "fire", Bit: 8},
	}
	if !reflect.DeepEqual(abm, wantABM) {
		t.Fatalf("action-bit map:\n got %+v\nwant %+v", abm, wantABM)
	}
}

// TestParseKeymap_LegacyForm proves an un-migrated program (a bare {bit:name}
// keymap) still parses — as momentary actions — so migration is incremental,
// not a flag day.
func TestParseKeymap_LegacyForm(t *testing.T) {
	scene := map[string]interface{}{
		"keymap": map[string]interface{}{"0": "left", "3": "fire"},
	}
	got, err := ParseKeymap(scene)
	if err != nil {
		t.Fatalf("ParseKeymap: %v", err)
	}
	want := []ControlBinding{
		{Bit: 0, Role: ControlRoleAction, Action: "left", Label: "Left", Behavior: BehaviorMomentary},
		{Bit: 3, Role: ControlRoleAction, Action: "fire", Label: "Fire", Behavior: BehaviorMomentary},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy bindings:\n got %+v\nwant %+v", got, want)
	}
}

// TestParseKeymap_Rejects proves malformed roles surface as errors, never a
// silent drop — a declared control a host cannot parse must be loud.
func TestParseKeymap_Rejects(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"unknown role":   {"role": "wiggle"},
		"axis no dir":    {"role": ControlRoleAxis},
		"bad axis":       {"role": ControlRoleAxis, "axis": "northwest"},
		"action no name": {"role": ControlRoleAction},
		"bad behavior":   {"role": ControlRoleAction, "action": "fire", "behavior": "sticky"},
	}
	for name, entry := range cases {
		scene := map[string]interface{}{"keymap": map[string]interface{}{"0": entry}}
		if _, err := ParseKeymap(scene); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
	// A non-map keymap is also an error.
	if _, err := ParseKeymap(map[string]interface{}{"keymap": "nope"}); err == nil {
		t.Error("string keymap: expected an error, got nil")
	}
}

// TestParseKeymap_AsteroidsDescriptor closes the loop against what AuthorAsteroids
// actually writes: author the program, decode the descriptor (which normalizes
// the scene the way any consumer receives it), and parse the real keymap. This
// is the end-to-end check that the re-declaration and the parser agree.
func TestParseKeymap_AsteroidsDescriptor(t *testing.T) {
	ap := newTestPeer(t)
	descPath, err := AuthorAsteroids(ap, AsteroidsRoot, 0x5eed)
	if err != nil {
		t.Fatalf("AuthorAsteroids: %v", err)
	}
	ent, ok, err := ap.Get(descPath)
	if err != nil || !ok {
		t.Fatalf("read descriptor: ok=%v err=%v", ok, err)
	}
	d, err := DecodeDescriptor(ent)
	if err != nil {
		t.Fatalf("DecodeDescriptor: %v", err)
	}

	var keys *ProgramPort
	for i := range d.InputPorts {
		if d.InputPorts[i].Shape == ShapeKeySet {
			keys = &d.InputPorts[i]
		}
	}
	if keys == nil {
		t.Fatal("Asteroids has no key-set input port")
	}

	// The wire field is pinned to `keys`, derivable from the shape (not hardcoded).
	if got := InputField(keys.Shape); got != KeySetField {
		t.Fatalf("InputField(key-set) = %q, want %q", got, KeySetField)
	}
	if KeySetField != "keys" {
		t.Fatalf("KeySetField drifted to %q, want %q", KeySetField, "keys")
	}

	bindings, err := ParseKeymap(keys.Scene)
	if err != nil {
		t.Fatalf("ParseKeymap(authored): %v", err)
	}
	// Exactly one directional-per-position + one fire action, on the standard controller.
	axes, actions := 0, 0
	sawFire := false
	for _, b := range bindings {
		switch b.Role {
		case ControlRoleAxis:
			axes++
			if !validAxis(b.Axis) {
				t.Errorf("bit %d: invalid axis %q", b.Bit, b.Axis)
			}
		case ControlRoleAction:
			actions++
			if b.Action == ActionFire {
				sawFire = true
				if b.EffectiveGlyph() == "" {
					t.Errorf("fire action has no glyph")
				}
			}
		}
	}
	if axes != 3 || actions != 1 || !sawFire {
		t.Fatalf("Asteroids controller: axes=%d actions=%d fire=%v, want 3/1/true", axes, actions, sawFire)
	}
}
