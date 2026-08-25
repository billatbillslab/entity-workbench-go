package workbench

// AsteroidsGameModel tests — model-tier (TESTING-STRATEGY): the runtime harness
// semantics (seed, tick, held-key input port, display-list derivation, restart,
// listener wake). Program-correctness (oracle equivalence, replay determinism,
// shard identity, port cost) is proven at the experiment tier in
// entitysdk/exp_compute_asteroids_test.go — not re-proven here.

import (
	"testing"
	"time"

	"entity-workbench-go/entitysdk"
)

func newAsteroidsTestModel(t *testing.T) (*AsteroidsGameModel, *entitysdk.AppPeer) {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	m, err := NewAsteroidsGameModel(ap, "app/asteroids/test", 4242)
	if err != nil {
		t.Fatalf("NewAsteroidsGameModel: %v", err)
	}
	t.Cleanup(m.Close)
	return m, ap
}

func astCountKind(out AsteroidsGameOutput, kind uint64) int {
	n := 0
	for _, q := range out.Quads {
		if q.Kind == kind {
			n++
		}
	}
	return n
}

// TestAsteroidsGameModel_SeedRendersDisplayList is the load-bearing model test:
// the panel's entire contract is Render(), so the seed frame must already be a
// drawable display list — 4 vertices per live actor, in world space, tagged by
// kind. If this frame were empty the panel would draw nothing and the failure
// would look like a UI bug.
func TestAsteroidsGameModel_SeedRendersDisplayList(t *testing.T) {
	m, _ := newAsteroidsTestModel(t)

	out := m.Render()
	if out.Running || out.Status != 0 || out.Ticks != 0 {
		t.Fatalf("unexpected seed frame scalars: %+v", out)
	}
	if out.World != astWorld {
		t.Fatalf("world extent %d, want %d", out.World, astWorld)
	}
	// The seed is 1 ship + 4 asteroids; free slots are NOT drawables.
	if got := len(out.Quads); got != 5 {
		t.Fatalf("seed frame has %d quads, want 5 (1 ship + 4 asteroids)", got)
	}
	if got := astCountKind(out, AsteroidsShip); got != 1 {
		t.Fatalf("seed frame has %d ship quads, want 1", got)
	}
	if got := astCountKind(out, AsteroidsAsteroid); got != 4 {
		t.Fatalf("seed frame has %d asteroid quads, want 4", got)
	}

	// Every quad must be real geometry: 4 distinct vertices inside the world.
	// An all-zero quad is a display list that "renders" nothing.
	for i, q := range out.Quads {
		if len(q.X) != 4 || len(q.Y) != 4 {
			t.Fatalf("quad %d has %d/%d vertices, want 4/4", i, len(q.X), len(q.Y))
		}
		distinct := map[[2]int64]bool{}
		for v := range q.X {
			if q.X[v] < 0 || q.X[v] >= out.World || q.Y[v] < 0 || q.Y[v] >= out.World {
				t.Fatalf("quad %d vertex %d at (%d,%d) is outside the world (0..%d)",
					i, v, q.X[v], q.Y[v], out.World)
			}
			distinct[[2]int64{q.X[v], q.Y[v]}] = true
		}
		if len(distinct) != 4 {
			t.Fatalf("quad %d (kind=%d) has %d distinct vertices, want 4 — a "+
				"degenerate quad draws as a dot or nothing", i, q.Kind, len(distinct))
		}
	}
}

// TestAsteroidsGameModel_TicksAndDisplayFollows: the display list is DERIVED
// from state each tick, so drifting actors must move the quads. A display list
// that never changes would render a frozen picture over a live sim.
func TestAsteroidsGameModel_TicksAndDisplayFollows(t *testing.T) {
	m, _ := newAsteroidsTestModel(t)
	before := m.Render()

	for i := 1; i <= 3; i++ {
		if !m.tickOnce() {
			t.Fatalf("tick %d ended the loop: %s", i, m.Render().Err)
		}
	}
	after := m.Render()
	if after.Err != "" {
		t.Fatalf("tick error: %s", after.Err)
	}
	if after.Ticks != 3 {
		t.Fatalf("expected 3 ticks, got %d", after.Ticks)
	}
	if len(after.Quads) != len(before.Quads) {
		t.Fatalf("actor count changed without a shot fired: %d -> %d",
			len(before.Quads), len(after.Quads))
	}
	moved := 0
	for i := range after.Quads {
		if after.Quads[i].X[0] != before.Quads[i].X[0] ||
			after.Quads[i].Y[0] != before.Quads[i].Y[0] {
			moved++
		}
	}
	// The ship is stationary (no input); the 4 asteroids drift.
	if moved < 4 {
		t.Fatalf("only %d/5 quads moved over 3 ticks — the display list is not "+
			"following state", moved)
	}
}

// TestAsteroidsGameModel_HeldKeySetInput exercises the finding that motivated
// this panel: Snake's port is one last-write-wins direction and cannot express
// simultaneous keys. The bitmask port must accept a SET and the step must act on
// every bit in it — here thrust+rotate together, which no Snake-shaped port
// could carry.
func TestAsteroidsGameModel_HeldKeySetInput(t *testing.T) {
	m, _ := newAsteroidsTestModel(t)
	shipAt := func() (int64, int64) {
		out := m.Render()
		for _, q := range out.Quads {
			if q.Kind == AsteroidsShip {
				return q.X[0], q.Y[0]
			}
		}
		t.Fatal("no ship in the display list")
		return 0, 0
	}
	x0, y0 := shipAt()

	// Hold thrust AND rotate at once — two bits in one snapshot write.
	keys := uint64(1)<<AsteroidsKeyThrust | uint64(1)<<AsteroidsKeyRight
	if err := m.Input(keys); err != nil {
		t.Fatalf("Input(thrust|right): %v", err)
	}
	for i := 0; i < 4; i++ {
		if !m.tickOnce() {
			t.Fatalf("tick %d ended: %s", i, m.Render().Err)
		}
	}
	x1, y1 := shipAt()
	if x1 == x0 && y1 == y0 {
		t.Fatal("ship did not move while thrust was held — the held-key bitmask " +
			"port is not reaching the step")
	}

	// Releasing the set is also a snapshot write; the ship keeps its velocity
	// (no drag), so this asserts the port ACCEPTS the empty set, not that it stops.
	if err := m.Input(0); err != nil {
		t.Fatalf("Input(0): %v", err)
	}
	if err := m.Input(1 << 4); err == nil {
		t.Fatal("expected an out-of-range error for a bitmask above the 4 defined keys")
	}
}

// TestAsteroidsGameModel_FireSpawnsBullet — the variable actor set, through the
// panel's own contract: firing must ADD a drawable that was not there before.
func TestAsteroidsGameModel_FireSpawnsBullet(t *testing.T) {
	m, _ := newAsteroidsTestModel(t)
	if got := astCountKind(m.Render(), AsteroidsBullet); got != 0 {
		t.Fatalf("seed frame already has %d bullets", got)
	}
	if err := m.Input(uint64(1) << AsteroidsKeyFire); err != nil {
		t.Fatalf("Input(fire): %v", err)
	}
	if !m.tickOnce() {
		t.Fatalf("tick ended: %s", m.Render().Err)
	}
	if got := astCountKind(m.Render(), AsteroidsBullet); got != 1 {
		t.Fatalf("firing produced %d bullets, want 1 — the spawn path is not "+
			"reaching the display list", got)
	}
}

// TestAsteroidsGameModel_RestartReseeds: Restart must return to a fresh seed
// frame (and a different layout, since the RNG seed advances).
func TestAsteroidsGameModel_RestartReseeds(t *testing.T) {
	m, _ := newAsteroidsTestModel(t)
	for i := 0; i < 2; i++ {
		if !m.tickOnce() {
			t.Fatalf("tick %d ended: %s", i, m.Render().Err)
		}
	}
	if err := m.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	t.Cleanup(m.Stop)
	out := m.Render()
	if out.Ticks != 0 {
		t.Fatalf("Restart left ticks=%d, want 0", out.Ticks)
	}
	if len(out.Quads) != 5 {
		t.Fatalf("Restart frame has %d quads, want 5", len(out.Quads))
	}
	if !out.Running {
		t.Fatal("Restart should leave the loop running")
	}
}

// TestAsteroidsGameModel_OnChangeFires — the panel wakes on this; if it never
// fires, the UI renders the seed frame forever.
func TestAsteroidsGameModel_OnChangeFires(t *testing.T) {
	m, _ := newAsteroidsTestModel(t)
	fired := make(chan struct{}, 8)
	cancel := m.OnChange(func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	defer cancel()

	if !m.tickOnce() {
		t.Fatalf("tick ended: %s", m.Render().Err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange did not fire after a tick")
	}
}
