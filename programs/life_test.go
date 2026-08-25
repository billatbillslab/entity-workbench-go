package programs

// LifeGameModel tests — model-tier (TESTING-STRATEGY): the runtime
// harness semantics (seed, tick, the two fixed-point stops, restart,
// listener wake). Program-correctness (blinker/glider oracle checks,
// arith-vs-table equivalence, the budget-cliff map) is proven at the
// experiment tier in entitysdk/exp_compute_life_test.go — not re-proven
// here. What IS re-checked below is the tiny slice of program behavior
// the stop conditions depend on (a block is stable, a blinker is not),
// because the runtime's halt decision is only as good as that read.

import (
	"testing"
	"time"

	"entity-workbench-go/entitysdk"
)

func newLifeTestModel(t *testing.T) *LifeGameModel {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	m, err := NewLifeGameModel(ap, "app/life/test", 4242)
	if err != nil {
		t.Fatalf("NewLifeGameModel: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// setLifeState overwrites state₀ with a hand-built grid so a test can
// pin a specific pattern instead of the pseudo-random soup.
func setLifeState(t *testing.T, m *LifeGameModel, live ...[2]int) {
	t.Helper()
	cells := make([]uint64, m.w*m.h)
	for _, p := range live {
		cells[p[1]*m.w+p[0]] = 1
	}
	s := lifeWireState{Width: uint64(m.w), Height: uint64(m.h), Cells: cells}
	ent, err := lifeStateEntity(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ap.PutEntity(m.statePath, ent); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.last = s
	m.generation = 0
	m.status = LifeRunning
	m.lastErr = ""
	m.mu.Unlock()
}

func TestLifeGameModel_SeedFrame(t *testing.T) {
	m := newLifeTestModel(t)

	out := m.Render()
	if out.Width != 16 || out.Height != 16 {
		t.Fatalf("expected a 16x16 board, got %dx%d", out.Width, out.Height)
	}
	if out.Running || out.Generation != 0 || out.Status != LifeRunning {
		t.Fatalf("expected a stopped gen-0 frame, got %+v", out)
	}
	if len(out.Cells) != 16*16 {
		t.Fatalf("expected 256 cells, got %d", len(out.Cells))
	}
	// The soup is ~37% density; a plausible band, not an exact count.
	density := float64(out.Population) / float64(len(out.Cells))
	if density < 0.2 || density > 0.55 {
		t.Fatalf("seed density %.2f outside the plausible soup band (pop %d)",
			density, out.Population)
	}
}

// Regression: the seed must be a SOUP, not a lattice.
//
// The first cut of lifeSeedState tested the LCG's low bits (`s%8`). A
// power-of-two-modulus LCG has period 8 in its low 3 bits, so on a
// width divisible by 8 every row came out identical — vertical stripes,
// which die on a torus in ~6 generations. A population/density check
// does NOT catch this (stripes sit at a perfectly plausible 37%), so
// this test asserts the structure the density check cannot see.
func TestLifeGameModel_SeedIsNotAStripedLattice(t *testing.T) {
	m := newLifeTestModel(t)
	out := m.Render()

	rowKey := func(y int) string {
		b := make([]byte, out.Width)
		for x := 0; x < out.Width; x++ {
			b[x] = byte('0' + out.Cells[y*out.Width+x])
		}
		return string(b)
	}
	distinct := map[string]bool{}
	for y := 0; y < out.Height; y++ {
		distinct[rowKey(y)] = true
	}
	// A real soup at 16 wide effectively never repeats a row; a low-bit
	// LCG collapses to exactly 1. Anything in single digits is a lattice.
	if len(distinct) < out.Height/2 {
		t.Fatalf("seed has only %d distinct rows out of %d — this is a lattice, "+
			"not a soup (LCG low-bit periodicity?):\n%s",
			len(distinct), out.Height, lifeASCIIForTest(out))
	}
}

// The seed must also SURVIVE — the panel opens on this board, so a soup
// that self-destructs before the user looks at it is a product bug even
// though every rule-level test passes. 40 generations at 6 ticks/s is
// ~7 seconds of watching.
func TestLifeGameModel_SeedSurvivesManyGenerations(t *testing.T) {
	m := newLifeTestModel(t)
	for i := 1; i <= 40; i++ {
		if !m.tickOnce() {
			out := m.Render()
			t.Fatalf("seed soup reached a fixed point after only %d generations "+
				"(status=%d population=%d err=%q):\n%s",
				i, out.Status, out.Population, out.Err, lifeASCIIForTest(out))
		}
	}
	if out := m.Render(); out.Generation != 40 {
		t.Fatalf("expected 40 generations, got %d", out.Generation)
	}
}

// lifeASCIIForTest renders a frame for failure messages — the thinnest
// possible display driver, and the one that makes a lattice obvious at
// a glance.
func lifeASCIIForTest(out LifeGameOutput) string {
	b := make([]byte, 0, (out.Width+1)*out.Height)
	for y := 0; y < out.Height; y++ {
		for x := 0; x < out.Width; x++ {
			if out.Cells[y*out.Width+x] == 1 {
				b = append(b, '#')
			} else {
				b = append(b, '.')
			}
		}
		b = append(b, '\n')
	}
	return string(b)
}

// A blinker oscillates with period 2: it is neither extinct nor a still
// life, so the runtime must keep clocking it.
func TestLifeGameModel_BlinkerKeepsRunning(t *testing.T) {
	m := newLifeTestModel(t)
	setLifeState(t, m, [2]int{4, 3}, [2]int{4, 4}, [2]int{4, 5}) // vertical

	if !m.tickOnce() {
		t.Fatalf("blinker tick 1 ended the loop: %+v", m.Render())
	}
	out := m.Render()
	if out.Err != "" {
		t.Fatalf("tick error: %s", out.Err)
	}
	if out.Population != 3 {
		t.Fatalf("expected population 3 after a blinker tick, got %d", out.Population)
	}
	// Rotated to horizontal — the three cells the oracle predicts.
	for _, p := range [][2]int{{3, 4}, {4, 4}, {5, 4}} {
		if out.Cells[p[1]*out.Width+p[0]] != 1 {
			t.Fatalf("expected a live cell at (%d,%d) after rotation: %v", p[0], p[1], out.Cells)
		}
	}
	if !m.tickOnce() {
		t.Fatalf("blinker tick 2 ended the loop: %+v", m.Render())
	}
	if out := m.Render(); out.Generation != 2 || out.Status != LifeRunning {
		t.Fatalf("expected a still-running gen-2 blinker, got %+v", out)
	}
}

// A 2x2 block is a fixed point — the runtime stops rather than clock a
// grid that reproduces its own bytes forever.
func TestLifeGameModel_StillLifeStopsTheClock(t *testing.T) {
	m := newLifeTestModel(t)
	setLifeState(t, m, [2]int{4, 4}, [2]int{5, 4}, [2]int{4, 5}, [2]int{5, 5})

	if m.tickOnce() {
		t.Fatalf("expected the block to stop the loop, got %+v", m.Render())
	}
	out := m.Render()
	if out.Err != "" {
		t.Fatalf("tick error: %s", out.Err)
	}
	if out.Status != LifeStillLife || out.Running {
		t.Fatalf("expected still-life+stopped, got %+v", out)
	}
	if out.Population != 4 {
		t.Fatalf("expected the block to survive intact, got population %d", out.Population)
	}
}

// A lone cell dies of underpopulation — population 0 is the other
// fixed point, and it must report extinction rather than still-life
// (both are stable; the status must name which one).
func TestLifeGameModel_ExtinctionStopsTheClock(t *testing.T) {
	m := newLifeTestModel(t)
	setLifeState(t, m, [2]int{7, 7})

	if m.tickOnce() {
		t.Fatalf("expected extinction to stop the loop, got %+v", m.Render())
	}
	out := m.Render()
	if out.Err != "" {
		t.Fatalf("tick error: %s", out.Err)
	}
	if out.Status != LifeExtinct || out.Running {
		t.Fatalf("expected extinct+stopped, got %+v", out)
	}
	if out.Population != 0 {
		t.Fatalf("expected population 0, got %d", out.Population)
	}
}

func TestLifeGameModel_RestartReseeds(t *testing.T) {
	m := newLifeTestModel(t)
	setLifeState(t, m, [2]int{7, 7})
	if m.tickOnce() {
		t.Fatal("expected extinction")
	}

	if err := m.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	defer m.Stop()
	out := m.Render()
	if out.Status != LifeRunning || !out.Running {
		t.Fatalf("expected a fresh running board after restart, got %+v", out)
	}
	if out.Population == 0 {
		t.Fatalf("restart should reseed a live soup, got an empty board")
	}
}

func TestLifeGameModel_ClockedLoopFiresListeners(t *testing.T) {
	m := newLifeTestModel(t)

	wakes := make(chan struct{}, 16)
	cancel := m.OnChange(func() {
		select {
		case wakes <- struct{}{}:
		default:
		}
	})
	defer cancel()

	m.Start()
	defer m.Stop()

	// At 6 ticks/s the first wake (Start itself notifies) is immediate
	// and tick wakes follow within ~170ms; allow generous slack.
	deadline := time.After(5 * time.Second)
	got := 0
	for got < 3 {
		select {
		case <-wakes:
			got++
		case <-deadline:
			t.Fatalf("expected >=3 wakes from the clocked loop, got %d (err=%q)", got, m.Render().Err)
		}
	}
	m.Stop()
	out := m.Render()
	if out.Running {
		t.Fatal("expected stopped after Stop")
	}
	if out.Err != "" {
		t.Fatalf("unexpected tick error: %s", out.Err)
	}
	if out.Generation == 0 {
		t.Fatal("expected at least one clocked generation")
	}
}
