package programs

// TIER: integration (TESTING-STRATEGY) — a real AppPeer, a real store, the real
// compute evaluator. Naming the tier is the discipline.
//
// These tests carry the phase-1 falsification (arch HANDOFF-2026-07-17 §5.4):
// all three programs mount through ONE host with no per-program Go, or the exact
// missing field/shape is named.
//
// The load-bearing one is TestMount_LifeMatchesHardCodedModel: the descriptor
// changes HOW a program is wired, never WHAT it computes. If the mounted state
// hashes diverge from the hard-coded model's at any tick, the split broke the
// program and the whole rung is void.

import (
	"encoding/json"
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/hash"

	"entity-workbench-go/entitysdk"
)

func newTestPeer(t *testing.T) *entitysdk.AppPeer {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	return ap
}

// stateHashAt reads the state entity's content hash — the boundary the oracle
// discipline compares on.
func stateHashAt(t *testing.T, ap *entitysdk.AppPeer, path string) hash.Hash {
	t.Helper()
	ent, ok, err := ap.Get(path)
	if err != nil || !ok {
		t.Fatalf("read state at %s: ok=%v err=%v", path, ok, err)
	}
	return ent.ContentHash
}

// TestMount_LifeMatchesHardCodedModel is the acceptance criterion: the mounted
// program's state-hash sequence must equal the hard-coded model's, tick for
// tick. Same seed, same program, different wiring.
//
// ANTI-VACUITY: the grid must actually evolve. A Life that goes extinct on tick
// 1 would match trivially (both sides frozen), and this track has been burned by
// exactly that class of green — an assertion that cannot come out any other way
// is not a measurement. We assert the hash CHANGES across the run before we
// assert the two sides agree.
func TestMount_LifeMatchesHardCodedModel(t *testing.T) {
	const seed = uint64(12345)
	const ticks = 12

	// The hard-coded model: builds its IR at boot and ticks it.
	apOld := newTestPeer(t)
	old, err := NewLifeGameModel(apOld, "legacy/life", seed)
	if err != nil {
		t.Fatalf("NewLifeGameModel: %v", err)
	}
	wantHashes := make([]hash.Hash, 0, ticks)
	for i := 0; i < ticks; i++ {
		if !old.tickOnce() {
			t.Fatalf("legacy model stopped at tick %d (status=%d err=%q)",
				i, old.Render().Status, old.Render().Err)
		}
		wantHashes = append(wantHashes, stateHashAt(t, apOld, "legacy/life/state"))
	}

	// The mounted program: authored once, mounted from its descriptor.
	apNew := newTestPeer(t)
	descPath, err := AuthorLife(apNew, LifeRoot, seed)
	if err != nil {
		t.Fatalf("AuthorLife: %v", err)
	}
	h, err := Mount(apNew, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(h.Close)

	gotHashes := make([]hash.Hash, 0, ticks)
	for i := 0; i < ticks; i++ {
		if !h.tickOnce() {
			t.Fatalf("mounted program faulted at tick %d: %s", i, h.Render().Err)
		}
		gotHashes = append(gotHashes, stateHashAt(t, apNew, h.Descriptor().StatePath))
	}

	// Anti-vacuity: the program must actually be evolving, or "identical" is free.
	distinct := map[string]bool{}
	for _, hh := range wantHashes {
		distinct[hh.String()] = true
	}
	if len(distinct) < ticks/2 {
		t.Fatalf("VACUOUS: only %d distinct states across %d ticks — the grid is not evolving, "+
			"so hash equality proves nothing", len(distinct), ticks)
	}

	for i := range wantHashes {
		if wantHashes[i] != gotHashes[i] {
			t.Fatalf("tick %d: mounted state hash %s != hard-coded %s — the descriptor changed "+
				"WHAT the program computes, not just how it is wired",
				i, gotHashes[i], wantHashes[i])
		}
	}
}

// TestMount_LifeTextPortIsDriverReadable proves the `text` binding: the host
// materializes a character grid a blind driver can render, with no idea what
// Life is.
func TestMount_LifeTextPortIsDriverReadable(t *testing.T) {
	ap := newTestPeer(t)
	descPath, err := AuthorLife(ap, LifeRoot, 12345)
	if err != nil {
		t.Fatalf("AuthorLife: %v", err)
	}
	h, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(h.Close)

	frame := h.Render()
	pv, ok := frame.Ports["display"]
	if !ok {
		t.Fatalf("no display port in frame; ports=%v", frame.Ports)
	}
	if pv.Shape != ShapeText {
		t.Fatalf("display port shape = %q, want %q", pv.Shape, ShapeText)
	}
	tf, err := DecodeTextFrame(pv)
	if err != nil {
		t.Fatalf("DecodeTextFrame: %v", err)
	}
	if tf.Cols != lifeWidth || tf.Rows != lifeHeight {
		t.Fatalf("text frame %dx%d, want %dx%d", tf.Cols, tf.Rows, lifeWidth, lifeHeight)
	}
	if SceneString(pv.Scene, "mode", "") != TextModeGrid {
		t.Fatalf("scene.mode = %q, want %q", SceneString(pv.Scene, "mode", ""), TextModeGrid)
	}

	// Every cell must be a glyph the projection emits — nothing else can leak
	// through, or the driver would be rendering program internals.
	alive := 0
	for _, ch := range tf.Cells {
		switch ch {
		case glyphAlive:
			alive++
		case glyphDead:
		default:
			t.Fatalf("text frame carries non-glyph code point %d — the projection leaked state", ch)
		}
	}
	// Anti-vacuity: an all-dead frame would pass the glyph check trivially.
	if alive == 0 {
		t.Fatal("VACUOUS: text frame has no live cells; the glyph assertion proves nothing")
	}
}

// TestMount_RefusesUnsupportedShape proves admission (exploration §6): a host
// refuses a shape it cannot drive and says why — never a half-mount.
//
// This is also the mechanical form of arch's §2 ruling: `raw-state` is in the
// vocabulary but NOT advertised by this host, so the falsification cannot run
// through the escape hatch even by accident.
func TestMount_RefusesUnsupportedShape(t *testing.T) {
	ap := newTestPeer(t)
	descPath, err := AuthorLife(ap, LifeRoot, 12345)
	if err != nil {
		t.Fatalf("AuthorLife: %v", err)
	}
	ent, ok, err := ap.Get(descPath)
	if err != nil || !ok {
		t.Fatalf("read descriptor: ok=%v err=%v", ok, err)
	}
	d, err := DecodeDescriptor(ent)
	if err != nil {
		t.Fatalf("DecodeDescriptor: %v", err)
	}
	d.OutputPorts[0].Shape = ShapeRawState
	badEnt, err := d.Entity()
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if _, err := ap.PutEntity(descPath, badEnt); err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, err := Mount(ap, descPath); err == nil {
		t.Fatal("Mount accepted an unadvertised shape (raw-state) — admission is not enforced, " +
			"so the falsification could run through the escape hatch")
	}
}

// TestMount_AllThreeMountThroughOneHost is THE falsification (arch §5.4):
// Snake, Life and Asteroids all mount through one host with zero per-program Go,
// through generic shapes, or the exact missing field/shape is named.
//
// Note what this test can and cannot show. It cannot prove the host has no
// per-program code — only reading program_host.go can (it imports no program
// symbol and names no program). What it proves is the operational half: one
// Mount call, three programs, three shape sets, no branch on identity.
func TestMount_AllThreeMountThroughOneHost(t *testing.T) {
	cases := []struct {
		name       string
		author     func(*entitysdk.AppPeer, string, uint64) (string, error)
		root       string
		wantShapes map[string]string // port name → shape
		wantInputs int
	}{
		{"life", AuthorLife, LifeRoot,
			map[string]string{"display": ShapeText}, 0},
		{"snake", AuthorSnake, SnakeRoot,
			map[string]string{"display": ShapeText}, 1},
		{"asteroids", AuthorAsteroids, AsteroidsRoot,
			map[string]string{"display": ShapeDisplayList}, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ap := newTestPeer(t)
			descPath, err := tc.author(ap, tc.root, 12345)
			if err != nil {
				t.Fatalf("author %s: %v", tc.name, err)
			}
			h, err := Mount(ap, descPath)
			if err != nil {
				t.Fatalf("MOUNT FAILED for %s — this is the useful negative; "+
					"name the missing field/shape: %v", tc.name, err)
			}
			t.Cleanup(h.Close)

			if got := len(h.Descriptor().InputPorts); got != tc.wantInputs {
				t.Fatalf("%s: %d input ports, want %d", tc.name, got, tc.wantInputs)
			}
			for i := 0; i < 3; i++ {
				if !h.tickOnce() {
					t.Fatalf("%s: faulted at tick %d: %s", tc.name, i, h.Render().Err)
				}
			}
			frame := h.Render()
			for name, wantShape := range tc.wantShapes {
				pv, ok := frame.Ports[name]
				if !ok {
					t.Fatalf("%s: no port %q; got %v", tc.name, name, frame.Ports)
				}
				if pv.Shape != wantShape {
					t.Fatalf("%s: port %q shape %q, want %q", tc.name, name, pv.Shape, wantShape)
				}
				// A driver must be able to decode it knowing only the shape.
				switch pv.Shape {
				case ShapeText:
					if _, err := DecodeTextFrame(pv); err != nil {
						t.Fatalf("%s: blind text driver cannot decode: %v", tc.name, err)
					}
				case ShapeDisplayList:
					if _, err := DecodeDisplayList(pv); err != nil {
						t.Fatalf("%s: blind display-list driver cannot decode: %v", tc.name, err)
					}
				}
			}
		})
	}
}

// TestMount_SnakeInputPortDrivesTheProgram proves the input half end-to-end: a
// blind driver writes a `direction` shape and the program responds.
//
// ANTI-VACUITY: the two runs must actually diverge. If the snake ignored input,
// both would match and the test would pass while proving nothing.
func TestMount_SnakeInputPortDrivesTheProgram(t *testing.T) {
	run := func(dir uint64) hash.Hash {
		ap := newTestPeer(t)
		descPath, err := AuthorSnake(ap, SnakeRoot, 999)
		if err != nil {
			t.Fatalf("AuthorSnake: %v", err)
		}
		h, err := Mount(ap, descPath)
		if err != nil {
			t.Fatalf("Mount: %v", err)
		}
		t.Cleanup(h.Close)

		typ, data, err := EncodeDirection(dir)
		if err != nil {
			t.Fatalf("EncodeDirection: %v", err)
		}
		if err := h.Input("dir", typ, data); err != nil {
			t.Fatalf("Input: %v", err)
		}
		for i := 0; i < 4; i++ {
			if !h.tickOnce() {
				t.Fatalf("snake faulted at tick %d: %s", i, h.Render().Err)
			}
		}
		return stateHashAt(t, ap, h.Descriptor().StatePath)
	}

	right := run(SnakeRight)
	down := run(SnakeDown)
	if right == down {
		t.Fatal("VACUOUS: steering right and down produced identical state — the input port " +
			"is not reaching the program, so this test proves nothing")
	}
}

// TestMount_AsteroidsSceneCarriesWrap pins the scene property that proved scene
// metadata is necessary at all: a renderer cannot infer from vertices that the
// world is a torus.
func TestMount_AsteroidsSceneCarriesWrap(t *testing.T) {
	ap := newTestPeer(t)
	descPath, err := AuthorAsteroids(ap, AsteroidsRoot, 12345)
	if err != nil {
		t.Fatalf("AuthorAsteroids: %v", err)
	}
	h, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(h.Close)

	pv := h.Render().Ports["display"]
	if !SceneBool(pv.Scene, "wrap", false) {
		t.Fatalf("display port scene lacks wrap=true; scene=%v", pv.Scene)
	}
	dl, err := DecodeDisplayList(pv)
	if err != nil {
		t.Fatalf("DecodeDisplayList: %v", err)
	}
	if len(dl.Kinds) == 0 {
		t.Fatal("VACUOUS: display list is empty; the decode assertion proves nothing")
	}
}

// TestDescriptor_NestedSceneSurvivesRoundTripToJSON pins the other seam bug: a
// nested scene value comes back from CBOR interface-keyed, and encoding/json
// refuses it ("json: unsupported type: map[interface {}]interface {}").
//
// Asteroids' scene.keymap is the only nested scene among the three, so only
// Asteroids hit it — the same pattern this rung keeps producing, where the two
// grids agree with each other and hide the question and the third program asks
// it. Assert a descriptor's scene survives the round trip a driver actually
// makes: author -> tree -> decode -> JSON.
func TestDescriptor_NestedSceneSurvivesRoundTripToJSON(t *testing.T) {
	ap := newTestPeer(t)
	descPath, err := AuthorAsteroids(ap, AsteroidsRoot, 12345)
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

	var keymapPort *ProgramPort
	for i := range d.InputPorts {
		if _, has := d.InputPorts[i].Scene["keymap"]; has {
			keymapPort = &d.InputPorts[i]
		}
	}
	if keymapPort == nil {
		t.Fatal("VACUOUS: no input port carries a nested scene.keymap, so this test " +
			"would pass even with the bug present")
	}

	if _, err := json.Marshal(keymapPort.Scene); err != nil {
		t.Fatalf("scene will not marshal to JSON (%v) — a driver reading it over the "+
			"bridge gets an error envelope instead of a frame", err)
	}
}

// TestShape_PayloadsCarryJSONFieldNames pins the seam that produced this rung's
// nastiest bug: a green log and a blank screen.
//
// The shape payloads cross CBOR (into the tree) AND JSON (out to a renderer).
// With cbor tags only, encoding/json fell back to Go field names — {"Cols":16}
// where the C# driver reads "cols" — so every field deserialized to zero and the
// renderer drew nothing. Nothing detected it: the smoke log read
// "tick 150 · running · shapes: text" because those fields live on wrapper DTOs
// that DID have json tags. Only a screenshot showed the empty board.
//
// So assert the wire format directly. A driver on any host reads these names.
func TestShape_PayloadsCarryJSONFieldNames(t *testing.T) {
	tf, err := json.Marshal(TextFrame{Cols: 2, Rows: 1, Cells: []uint64{46, 35}})
	if err != nil {
		t.Fatalf("marshal TextFrame: %v", err)
	}
	for _, want := range []string{`"cols"`, `"rows"`, `"cells"`} {
		if !strings.Contains(string(tf), want) {
			t.Fatalf("TextFrame JSON %s lacks %s — a renderer reading lowercase names "+
				"would decode zeros and draw nothing", tf, want)
		}
	}

	dl, err := json.Marshal(DisplayList{Kinds: []uint64{0}, X0: []int64{1}, Y0: []int64{2}, X1: []int64{3}, Y1: []int64{4}})
	if err != nil {
		t.Fatalf("marshal DisplayList: %v", err)
	}
	for _, want := range []string{`"kinds"`, `"x0"`, `"y0"`, `"x1"`, `"y1"`} {
		if !strings.Contains(string(dl), want) {
			t.Fatalf("DisplayList JSON %s lacks %s", dl, want)
		}
	}

	for _, tc := range []struct {
		name string
		v    interface{}
		want string
	}{
		{"KeySet", KeySet{Bits: 3}, `"bits"`},
		{"Direction", Direction{Dir: 2}, `"dir"`},
	} {
		b, err := json.Marshal(tc.v)
		if err != nil {
			t.Fatalf("marshal %s: %v", tc.name, err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Fatalf("%s JSON %s lacks %s", tc.name, b, tc.want)
		}
	}
}

// TestShape_DirectionEnumIsTheShapeContract pins what a blind driver relies on:
// the `direction` enum means the same thing regardless of program.
//
// Snake's constants happen to agree with the shape's. That agreement is load-
// bearing — the driver turns an arrow key into DirDown and Snake must read it
// as down — and it was a coincidence until this test. If someone renumbers
// either side, a blind driver silently steers the wrong way, which is the
// nastiest possible failure: no error, just a game that misbehaves.
func TestShape_DirectionEnumIsTheShapeContract(t *testing.T) {
	cases := []struct {
		name             string
		shape, program   uint64
	}{
		{"up", DirUp, SnakeUp},
		{"right", DirRight, SnakeRight},
		{"down", DirDown, SnakeDown},
		{"left", DirLeft, SnakeLeft},
	}
	for _, tc := range cases {
		if tc.shape != tc.program {
			t.Fatalf("direction %q: shape says %d, Snake says %d — a blind driver would steer wrong",
				tc.name, tc.shape, tc.program)
		}
	}
}

// TestDescriptor_ValidateRejectsUnseededInputPort pins F-E1 at the descriptor
// layer: an input port with no seed cannot take tick 0, so such a descriptor
// must never reach the tree.
func TestDescriptor_ValidateRejectsUnseededInputPort(t *testing.T) {
	d := &ProgramDescriptor{
		StatePath:    "app/x/state",
		InitialState: "app/x/state0",
		Step:         "app/x/step",
		InputPorts: []ProgramPort{{
			Name: "input", Path: "app/x/input", TypeRef: DirectionType,
			Kind: KindSnapshot, Role: RoleInput, Shape: ShapeDirection,
			// Initial deliberately empty.
		}},
		Tick: ProgramTick{Mode: TickClockDriven, RateHint: 6},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("Validate accepted an input port with no initial — F-E1 says it cannot tick")
	}
}
