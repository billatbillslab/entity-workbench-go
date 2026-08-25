package programs

// TIER: integration, concurrency (TESTING-STRATEGY).
//
// Why this file exists: the Xvfb smoke run of the generic panel segfaulted at
// teardown (exit 139) ONCE, then passed three times running. The legacy panels
// never did. An intermittent SIGSEGV that "passes on retry" is exactly the shape
// of a bug that ships — so rather than retry until green, put -race on the
// lifecycle the panel actually drives.
//
// The panel's teardown order is: cancel the OnChange listener, host.Close()
// (which joins the tick goroutine), then join the wake goroutine. What the smoke
// harness adds is that the window closes WHILE the clock is still ticking, so
// Close races a tick in flight.

import (
	"sync"
	"testing"
	"time"
)

// TestHost_CloseRacesTick drives the exact race the smoke harness hit: a running
// clock torn down mid-tick, with a listener attached, repeatedly.
func TestHost_CloseRacesTick(t *testing.T) {
	for i := 0; i < 8; i++ {
		ap := newTestPeer(t)
		descPath, err := AuthorAsteroids(ap, AsteroidsRoot, 12345)
		if err != nil {
			t.Fatalf("AuthorAsteroids: %v", err)
		}
		h, err := Mount(ap, descPath)
		if err != nil {
			t.Fatalf("Mount: %v", err)
		}

		// A listener, as the bridge attaches one.
		var seen int
		var mu sync.Mutex
		cancel := h.OnChange(func() {
			mu.Lock()
			seen++
			mu.Unlock()
		})

		h.Start()
		// Let the clock actually get in flight — Asteroids is 12 ticks/s, so
		// this lands mid-tick rather than before the first one.
		time.Sleep(120 * time.Millisecond)

		// The panel's teardown order.
		cancel()
		h.Close()
	}
}

// TestHost_ConcurrentRenderDuringTick pins the other lifecycle the panel drives:
// the UI thread calls Render() on every wake while the clock keeps ticking.
func TestHost_ConcurrentRenderDuringTick(t *testing.T) {
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

	h.Start()
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					// Read every port, as a driver does.
					for _, pv := range h.Render().Ports {
						_ = pv.Data
						_ = pv.Scene
					}
				}
			}
		}()
	}
	time.Sleep(250 * time.Millisecond)
	close(done)
	wg.Wait()
	h.Stop()
}

// TestHost_RestartRacesTick pins Restart landing while the clock runs — the
// panel's Restart button, pressed without pausing first.
func TestHost_RestartRacesTick(t *testing.T) {
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

	for i := 0; i < 4; i++ {
		h.Start()
		time.Sleep(60 * time.Millisecond)
		if err := h.Restart(); err != nil {
			t.Fatalf("Restart while running: %v", err)
		}
	}
}
