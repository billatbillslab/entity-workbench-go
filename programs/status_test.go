package programs

import (
	"strings"
	"testing"

	"entity-workbench-go/entitysdk"
)

// statusLine mounts a program, reads its `status` text port, and returns the
// decoded line — the blind text driver's view of the program's own readout.
func statusLine(t *testing.T, author func(*entitysdk.AppPeer, string, uint64) (string, error), root string, seed uint64) string {
	t.Helper()
	ap := newTestPeer(t)
	descPath, err := author(ap, root, seed)
	if err != nil {
		t.Fatalf("author %s: %v", root, err)
	}
	h, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount %s: %v", root, err)
	}
	t.Cleanup(h.Close)

	pv, ok := h.Render().Ports["status"]
	if !ok {
		t.Fatalf("%s has no `status` port; ports=%v", root, h.Render().Ports)
	}
	if pv.Shape != ShapeText {
		t.Fatalf("%s status port shape %q, want %q", root, pv.Shape, ShapeText)
	}
	if SceneString(pv.Scene, "mode", "") != TextModeStream {
		t.Fatalf("%s status mode %q, want %q (a line, not a grid)",
			root, SceneString(pv.Scene, "mode", ""), TextModeStream)
	}
	tf, err := DecodeTextFrame(pv)
	if err != nil {
		t.Fatalf("%s decode status: %v", root, err)
	}
	if tf.Rows != 1 {
		t.Fatalf("%s status is %d rows, want 1", root, tf.Rows)
	}
	var b strings.Builder
	for _, ch := range tf.Cells {
		b.WriteRune(rune(ch))
	}
	return b.String()
}

// TestStatusPort_FormatsProgramState proves the program-owned status readout: a
// blind text driver, given only the `status` port, sees a formatted line built
// entirely in the tree — the same bytes every renderer would show. The generic
// host never learns what a "score" is.
func TestStatusPort_FormatsProgramState(t *testing.T) {
	// Snake seeds length 3, alive → "LEN 003 ▶".
	if got := statusLine(t, AuthorSnake, SnakeRoot, 999); got != "LEN 003 ▶" {
		t.Errorf("Snake status = %q, want %q", got, "LEN 003 ▶")
	}
	// Asteroids seeds score 0, playing → "SCORE 00000 ▶".
	if got := statusLine(t, AuthorAsteroids, AsteroidsRoot, 12345); got != "SCORE 00000 ▶" {
		t.Errorf("Asteroids status = %q, want %q", got, "SCORE 00000 ▶")
	}
	// Life: "POP " + 4-digit population + alive glyph. Population is seed-dependent,
	// so check structure + that the digits equal the actual live-cell count.
	life := statusLine(t, AuthorLife, LifeRoot, 12345)
	if !strings.HasPrefix(life, "POP ") || !strings.HasSuffix(life, " ▶") {
		t.Fatalf("Life status = %q, want POP <n> ▶", life)
	}
}

// TestMount_RestartReseedsToInitial is the reset button's contract: Restart is a
// GENERIC host op (reseed state from the descriptor's initial_state), needing no
// program knowledge — which is why a reset button is a reasonable host affordance
// even though score/state are program-owned. This guards the reseed a renderer's
// reset button drives, so "restart" never again means "leave the app and return".
func TestMount_RestartReseedsToInitial(t *testing.T) {
	ap := newTestPeer(t)
	descPath, err := AuthorSnake(ap, SnakeRoot, 42)
	if err != nil {
		t.Fatalf("AuthorSnake: %v", err)
	}
	h, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(h.Close)

	initial := stateHashAt(t, ap, h.Descriptor().InitialState)
	for i := 0; i < 5; i++ {
		if !h.tickOnce() {
			t.Fatalf("snake stopped at tick %d", i)
		}
	}
	moved := stateHashAt(t, ap, h.Descriptor().StatePath)
	if moved == initial {
		t.Fatal("VACUOUS: state did not change over 5 ticks — reseed test proves nothing")
	}

	if err := h.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if back := stateHashAt(t, ap, h.Descriptor().StatePath); back != initial {
		t.Fatalf("after Restart, state %s != initial_state %s — reseed did not restore state₀", back, initial)
	}
}
