package programs

// TIER: integration (TESTING-STRATEGY) — a real AppPeer, real store, real compute
// evaluator. These pin the interactive Life step: regen, toggle, cursor movement,
// and pause are program-owned behaviours the generic host relays blind.

import (
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"

	"entity-workbench-go/entitysdk"
)

// lifeEditWire decodes an interactive Life state entity (an app/life/grid with
// the editor fields appended).
type lifeEditWire struct {
	Width  uint64   `cbor:"width"`
	Height uint64   `cbor:"height"`
	Cells  []uint64 `cbor:"cells"`
	Cursor uint64   `cbor:"cursor"`
	Gen    uint64   `cbor:"gen"`
	Pkeys  uint64   `cbor:"pkeys"`
	Paused uint64   `cbor:"paused"`
}

// lifeScrambleGo is the Go reference for the regen soup — the exact per-index
// counter hash buildLifeEditStepExpr lowers into compute. It is the oracle the
// regen test compares against, so a change to the compute lowering that silently
// altered the pattern would be caught here.
func lifeScrambleGo(gen uint64, w, h int) []uint64 {
	n := w * h
	cells := make([]uint64, n)
	for i := 0; i < n; i++ {
		h0 := ((gen%lifeLCGMod)*lifeMixC + uint64(i)) % lifeLCGMod
		h1 := (h0*lifeLCGMul + lifeLCGAdd) % lifeLCGMod
		if (h1/65536)%8 < 3 {
			cells[i] = 1
		}
	}
	return cells
}

func mountLifeEdit(t *testing.T, seed uint64) (*entitysdk.AppPeer, *Host) {
	t.Helper()
	ap := newTestPeer(t)
	descPath, err := AuthorLifeInteractive(ap, LifeEditRoot, seed)
	if err != nil {
		t.Fatalf("AuthorLifeInteractive: %v", err)
	}
	h, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(h.Close)
	return ap, h
}

func readLifeEdit(t *testing.T, ap *entitysdk.AppPeer, h *Host) lifeEditWire {
	t.Helper()
	ent, ok, err := ap.Get(h.Descriptor().StatePath)
	if err != nil || !ok {
		t.Fatalf("read state: ok=%v err=%v", ok, err)
	}
	var s lifeEditWire
	if err := ecf.Decode(ent.Data, &s); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return s
}

// press writes the held-key mask to the input port, then ticks once so the step
// samples it. Returns after the tick.
func press(t *testing.T, h *Host, mask uint64) {
	t.Helper()
	typ, raw, err := EncodeKeySet(mask)
	if err != nil {
		t.Fatalf("EncodeKeySet: %v", err)
	}
	if err := h.Input("keys", typ, raw); err != nil {
		t.Fatalf("Input: %v", err)
	}
	if !h.tickOnce() {
		t.Fatalf("faulted: %s", h.Render().Err)
	}
}

// TestLifeEdit_MountsWithController is the mount check: the interactive Life
// carries exactly one input port (the key-set controller) and renders a filled
// display list with the cursor as its own kind — through the unchanged host.
func TestLifeEdit_MountsWithController(t *testing.T) {
	ap, h := mountLifeEdit(t, 12345)

	if got := len(h.Descriptor().InputPorts); got != 1 {
		t.Fatalf("%d input ports, want 1 (the key-set controller)", got)
	}
	if s := h.Descriptor().InputPorts[0].Shape; s != ShapeKeySet {
		t.Fatalf("input shape %q, want %q", s, ShapeKeySet)
	}

	// The keymap must parse as the standard controller (four axes + three actions).
	binds, err := ParseKeymap(h.Descriptor().InputPorts[0].Scene)
	if err != nil {
		t.Fatalf("ParseKeymap: %v", err)
	}
	if len(binds) != 7 {
		t.Fatalf("%d control bindings, want 7 (d-pad + toggle/regen/pause)", len(binds))
	}

	for i := 0; i < 3; i++ {
		if !h.tickOnce() {
			t.Fatalf("faulted at tick %d: %s", i, h.Render().Err)
		}
	}

	pv, ok := h.Render().Ports["display"]
	if !ok {
		t.Fatal("no display port")
	}
	if DisplayRender(pv.Scene) != RenderFill {
		t.Fatalf("scene.render = %q, want fill", DisplayRender(pv.Scene))
	}
	dl, err := DecodeDisplayList(pv)
	if err != nil {
		t.Fatalf("DecodeDisplayList: %v", err)
	}
	if uint64(len(dl.Kinds)) != lifeWidth*lifeHeight {
		t.Fatalf("display list has %d quads, want one per cell (%d)", len(dl.Kinds), lifeWidth*lifeHeight)
	}
	// Exactly one cell is the cursor, so exactly one quad carries a cursor kind.
	cursorQuads := 0
	for i, k := range dl.Kinds {
		switch k {
		case DisplayKindBackground, lifeKindAlive:
		case lifeKindCursorDead, lifeKindCursorAlive:
			cursorQuads++
		default:
			t.Fatalf("quad %d has kind %d — not empty/alive/cursor", i, k)
		}
	}
	if cursorQuads != 1 {
		t.Fatalf("%d cursor quads, want exactly 1", cursorQuads)
	}
	_ = ap
}

// TestLifeEdit_RegenReplacesBoard proves the regen action: pressing it replaces
// the board with the deterministic soup for the current generation, and that soup
// differs from the opening board (so "regen" is not a no-op).
func TestLifeEdit_RegenReplacesBoard(t *testing.T) {
	const seed = uint64(777)
	ap, h := mountLifeEdit(t, seed)

	before := readLifeEdit(t, ap, h)
	if before.Gen != seed%lifeLCGMod {
		t.Fatalf("state0 gen = %d, want %d", before.Gen, seed%lifeLCGMod)
	}

	// Regen on the first tick: pressed (pkeys started clear), so the step uses the
	// generation READ this tick — the state0 gen.
	press(t, h, uint64(1)<<LifeKeyRegen)
	after := readLifeEdit(t, ap, h)

	want := lifeScrambleGo(before.Gen, lifeWidth, lifeHeight)
	if len(after.Cells) != len(want) {
		t.Fatalf("regen produced %d cells, want %d", len(after.Cells), len(want))
	}
	for i := range want {
		if after.Cells[i] != want[i] {
			t.Fatalf("regen cell %d = %d, want %d (compute lowering diverged from the oracle)",
				i, after.Cells[i], want[i])
		}
	}
	// Anti-vacuity: the regenerated soup is non-empty and actually differs from the
	// opening board — otherwise "regen changed the board" proves nothing.
	if pop := lifePopulation(after.Cells); pop == 0 {
		t.Fatal("VACUOUS: regen produced an empty board")
	}
	if lifeCellsEqual(before.Cells, after.Cells) {
		t.Fatal("regen reproduced the opening board — the action did nothing")
	}
}

// TestLifeEdit_ToggleFlipsCursorCell proves the toggle edit in isolation: with the
// simulation paused (so the B3/S23 rule cannot also change cells), one toggle
// press flips exactly the cursor cell and leaves every other cell untouched.
func TestLifeEdit_ToggleFlipsCursorCell(t *testing.T) {
	ap, h := mountLifeEdit(t, 999)

	// Pause, then release, so the board is frozen and pkeys is clear for the toggle.
	press(t, h, uint64(1)<<LifeKeyPause) // paused → 1, board held
	press(t, h, 0)                       // release pause; board still held
	frozen := readLifeEdit(t, ap, h)
	if frozen.Paused != 1 {
		t.Fatalf("paused = %d, want 1", frozen.Paused)
	}
	cur := int(frozen.Cursor)

	press(t, h, uint64(1)<<LifeKeyToggle) // flip the cursor cell
	edited := readLifeEdit(t, ap, h)

	if edited.Cells[cur] != 1-frozen.Cells[cur] {
		t.Fatalf("cursor cell %d = %d, want flipped from %d", cur, edited.Cells[cur], frozen.Cells[cur])
	}
	for i := range frozen.Cells {
		if i == cur {
			continue
		}
		if edited.Cells[i] != frozen.Cells[i] {
			t.Fatalf("toggle changed non-cursor cell %d (%d → %d) — a paused board must hold except the edit",
				i, frozen.Cells[i], edited.Cells[i])
		}
	}
}

// TestLifeEdit_CursorMoves proves the d-pad moves the cursor one cell per tap,
// wrapping within the row/column.
func TestLifeEdit_CursorMoves(t *testing.T) {
	ap, h := mountLifeEdit(t, 555)

	start := readLifeEdit(t, ap, h).Cursor
	w := uint64(lifeWidth)
	col0, row0 := start%w, start/w

	press(t, h, uint64(1)<<LifeKeyRight)
	afterRight := readLifeEdit(t, ap, h).Cursor
	wantRight := row0*w + (col0+1)%w
	if afterRight != wantRight {
		t.Fatalf("after right: cursor %d, want %d", afterRight, wantRight)
	}

	press(t, h, uint64(1)<<LifeKeyUp)
	afterUp := readLifeEdit(t, ap, h).Cursor
	h64 := uint64(lifeHeight)
	wantUp := ((row0+h64-1)%h64)*w + (col0+1)%w
	if afterUp != wantUp {
		t.Fatalf("after up: cursor %d, want %d", afterUp, wantUp)
	}
}
