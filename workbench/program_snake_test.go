package workbench

// SnakeGameModel tests — model-tier (TESTING-STRATEGY): the runtime
// harness semantics (seed, tick, input port, death-stop, restart,
// listener wake). Program-correctness (oracle equivalence, replay
// determinism) is proven at the experiment tier in
// entitysdk/exp_compute_snake_test.go — not re-proven here.

import (
	"testing"
	"time"

	"entity-workbench-go/entitysdk"
)

func newSnakeTestModel(t *testing.T) (*SnakeGameModel, *entitysdk.AppPeer) {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	m, err := NewSnakeGameModel(ap, "app/snake/test", 4242)
	if err != nil {
		t.Fatalf("NewSnakeGameModel: %v", err)
	}
	t.Cleanup(m.Close)
	return m, ap
}

func TestSnakeGameModel_SeedAndManualTicks(t *testing.T) {
	m, _ := newSnakeTestModel(t)

	out := m.Render()
	if out.Width != 12 || out.Height != 12 || out.Length != 3 || out.Status != 0 || out.Running {
		t.Fatalf("unexpected seed frame: %+v", out)
	}
	seedHead := out.Head

	// Manual ticks (no wall clock): head advances right along the row.
	for i := 1; i <= 2; i++ {
		if !m.tickOnce() {
			t.Fatalf("tick %d ended the loop: %s", i, m.Render().Err)
		}
	}
	out = m.Render()
	if out.Err != "" {
		t.Fatalf("tick error: %s", out.Err)
	}
	if out.Head != seedHead+2 {
		t.Fatalf("expected head %d after 2 right-ticks, got %d", seedHead+2, out.Head)
	}
	if out.Ticks != 2 {
		t.Fatalf("expected 2 ticks, got %d", out.Ticks)
	}

	// Steer down via the input port; next tick moves one row down.
	if err := m.Input(SnakeDown); err != nil {
		t.Fatal(err)
	}
	if !m.tickOnce() {
		t.Fatalf("tick after steer ended the loop: %s", m.Render().Err)
	}
	prev := out.Head
	out = m.Render()
	if out.Head != prev+uint64(out.Width) {
		t.Fatalf("expected head %d after steering down, got %d", prev+uint64(out.Width), out.Head)
	}
}

func TestSnakeGameModel_DeathStopsAndRestartRevives(t *testing.T) {
	m, _ := newSnakeTestModel(t)

	// Drive right until the wall kills it (12-wide board: <10 ticks).
	dead := false
	for i := 0; i < 12; i++ {
		cont := m.tickOnce()
		if out := m.Render(); out.Err != "" {
			t.Fatalf("tick %d error: %s", i+1, out.Err)
		}
		if !cont {
			dead = true
			break
		}
	}
	if !dead {
		t.Fatalf("snake should have hit the wall")
	}
	out := m.Render()
	if out.Status != 1 || out.Running {
		t.Fatalf("expected dead+stopped, got %+v", out)
	}

	if err := m.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	defer m.Stop()
	out = m.Render()
	if out.Status != 0 || !out.Running || out.Length != 3 {
		t.Fatalf("expected fresh running game after restart, got %+v", out)
	}
}

func TestSnakeGameModel_ClockedLoopFiresListeners(t *testing.T) {
	m, _ := newSnakeTestModel(t)

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
	// and a tick wake follows within ~170ms; allow generous slack.
	deadline := time.After(3 * time.Second)
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
		t.Fatalf("expected stopped after Stop")
	}
	if out.Err != "" {
		t.Fatalf("unexpected tick error: %s", out.Err)
	}
	if out.Ticks == 0 {
		t.Fatalf("expected at least one clocked tick")
	}
}
