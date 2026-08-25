package programs

// TIER: integration (TESTING-STRATEGY) — a real AppPeer, real store, real compute
// evaluator. These pin the interactive Life step: regen, toggle, cursor movement,
// and pause are program-owned behaviours the generic host relays blind.

import (
	"math"
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
		h1 := (h0*h0 + (gen%lifeLCGMod)*lifeLCGMul + lifeLCGAdd) % lifeLCGMod
		h2 := (h1/lifeMixFold + h1*lifeLCGMul + lifeLCGAdd) % lifeLCGMod
		if (h2/65536)%8 < 3 {
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

// lifeBestCyclicMatch reports the highest agreement rate between two boards over
// every cyclic index shift, and the shift that achieved it. Cyclic (rather than
// sliding) so every offset compares all n cells — a partial-overlap search has a
// selection-bias tail that, on a 256-cell board, is wide enough to swallow the
// signal.
//
// This is the instrument that names the 2026-08-22 regen defect. A soup hash that
// is linear in (gen, i) under a power-of-two modulus produces a new board only in
// the sense that the WINDOW MOVED: `a` and `b` agree at chance cell-for-cell while
// agreeing at ~0.97 under some shift. Comparing boards elementwise cannot see
// that — TestLifeEdit_RegenReplacesBoard was green throughout — and this can.
//
// Calibration over 2500 board pairs at generation gaps 1/2/3/6/12 (16×16 board,
// 3/8 density): the defective hash scored 0.953–0.992, the fixed one 0.578–0.734.
// The 0.85 threshold below sits between those two ranges with margin on each side.
func lifeBestCyclicMatch(a, b []uint64) (float64, int) {
	n := len(a)
	best, bestShift := 0.0, 0
	for s := 0; s < n; s++ {
		same := 0
		for i := 0; i < n; i++ {
			if a[(i+s)%n] == b[i] {
				same++
			}
		}
		if r := float64(same) / float64(n); r > best {
			best, bestShift = r, s
		}
	}
	return best, bestShift
}

// TestLifeEdit_RegenIsNotATranslation is the regression gate for the 2026-08-22
// defect: pressing Regen repeatedly produced the SAME board slid over a few
// cells, because the soup hash was linear in (gen, i).
//
// Part 1 drives the REAL host — two regens separated by real ticks, which is what
// an operator hitting the button twice actually does. Parts 2 and 3 sweep the Go
// oracle, which TestLifeEdit_RegenReplacesBoard pins equal to the compute
// lowering, so a property proved on it holds of the shipped program.
//
// Part 3 is the cheaper of the two instruments and worth keeping for that reason:
// a translation preserves the live-cell count almost exactly, so the defective
// hash's population across regens had σ = 0.76 where an independent draw at 3/8
// density has σ = 7.75. Every board it ever produced had 96 cells alive.
func TestLifeEdit_RegenIsNotATranslation(t *testing.T) {
	ap, h := mountLifeEdit(t, 424242)

	// Part 1 — through the host, the way the button is actually pressed.
	press(t, h, uint64(1)<<LifeKeyRegen)
	first := readLifeEdit(t, ap, h).Cells
	press(t, h, 0) // release, so the second press is a rising edge again
	for i := 0; i < 4; i++ {
		if !h.tickOnce() {
			t.Fatalf("faulted between regens: %s", h.Render().Err)
		}
	}
	press(t, h, uint64(1)<<LifeKeyRegen)
	second := readLifeEdit(t, ap, h).Cells

	if lifeCellsEqual(first, second) {
		t.Fatal("two regens produced the identical board")
	}
	if r, s := lifeBestCyclicMatch(first, second); r > 0.85 {
		t.Fatalf("two host regens agree at %.3f under a cyclic shift of %d cells — regen is "+
			"sliding one fixed pattern, not generating a new soup (want < 0.85)", r, s)
	}

	// Part 2 — the oracle across a spread of generation gaps, including the
	// adjacent ones an operator hits when tapping the button quickly.
	n := lifeWidth * lifeHeight
	base := lifeScrambleGo(1000, lifeWidth, lifeHeight)
	for _, gap := range []uint64{1, 2, 3, 6, 12, 60, 1000} {
		other := lifeScrambleGo(1000+gap, lifeWidth, lifeHeight)
		if r, s := lifeBestCyclicMatch(base, other); r > 0.85 {
			t.Fatalf("gen gap %d: boards agree at %.3f under a cyclic shift of %d cells (want < 0.85)",
				gap, r, s)
		}
	}

	// Part 3 — population must VARY like an independent draw. Mean 3/8·n with
	// σ = sqrt(n·3/8·5/8); at n=256 that is 96 ± 7.75. The defective hash held
	// σ = 0.76. The bounds are deliberately wide (σ > 4, mean within ±5) so this
	// asserts "the boards are independently drawn", not a particular RNG.
	const regens = 400
	sum, sumsq := 0.0, 0.0
	for g := uint64(0); g < regens; g++ {
		pop := float64(lifePopulation(lifeScrambleGo(2000+g, lifeWidth, lifeHeight)))
		sum, sumsq = sum+pop, sumsq+pop*pop
	}
	mean := sum / regens
	sd := math.Sqrt(sumsq/regens - mean*mean)
	wantMean := 0.375 * float64(n)
	if sd < 4.0 {
		t.Fatalf("population σ across %d regens is %.2f (want > 4.0; an independent draw gives "+
			"%.2f) — the boards are translations of one pattern, which preserves the count",
			regens, sd, math.Sqrt(float64(n)*0.375*0.625))
	}
	if mean < wantMean-5 || mean > wantMean+5 {
		t.Fatalf("population mean across %d regens is %.1f, want %.1f±5 — the 3/8 density moved",
			regens, mean, wantMean)
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
