package programs

// TIER: integration (TESTING-STRATEGY) — a real AppPeer, real store, real compute
// evaluator, and for one of these a REAL RUNNING CLOCK.
//
// These pin Host.Input's queue: every value a driver offers to an input port is
// observed by exactly one tick, in order.
//
// ─── Why this file exists, and why the tests already here could not have found
//     the bug it pins ───────────────────────────────────────────────────────────
//
// life_edit_test.go's `press` helper writes the mask and then calls tickOnce()
// itself, synchronously. So does host_test.go's direction case. Both are correct
// tests of the STEP, and both are structurally incapable of observing the
// sampler: they hand the tick its input and then run the tick, so the window in
// which a real driver's write can be superseded does not exist for them.
//
// It existed for the operator. Measured 2026-08-21 on interactive Life through a
// clocked host, before Input queued:
//
//	25 ms press (a real mouse click):  1 of 6 d-pad presses moved the cursor
//	400 ms press (held past a tick):   6 of 6
//
// The tick interval is 166.7 ms at Life's 6 Hz rate hint, and 25/166.7 ≈ 15%,
// which is what 1-in-6 is. Nothing was wrong with the C# panel, the bridge, the
// keymap or the step — the input was correct, arrived, and was overwritten
// before any tick read it.
//
// **D24.** The four existing life-edit tests were green throughout. A negative
// result is evidence only about the region the instrument can reach, and no
// instrument in this repo drove a clocked host from outside until this one.

import (
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"

	"entity-workbench-go/entitysdk"
)

// offer writes a held-key mask to the input port WITHOUT ticking — the thing a
// real driver does, and the thing `press` (life_edit_test.go) deliberately does
// not do.
func offer(t *testing.T, h *Host, mask uint64) {
	t.Helper()
	typ, raw, err := EncodeKeySet(mask)
	if err != nil {
		t.Fatalf("EncodeKeySet: %v", err)
	}
	if err := h.Input("keys", typ, raw); err != nil {
		t.Fatalf("Input: %v", err)
	}
}

// readKeysPort decodes the current value of the key-set input port from the
// tree — what the step will read on the next tick.
func readKeysPort(t *testing.T, ap *entitysdk.AppPeer, h *Host) uint64 {
	t.Helper()
	var path string
	for _, p := range h.Descriptor().InputPorts {
		if p.Name == "keys" {
			path = p.Path
		}
	}
	if path == "" {
		t.Fatal(`no input port named "keys"`)
	}
	ent, ok, err := ap.Get(path)
	if err != nil || !ok {
		t.Fatalf("read input port: ok=%v err=%v", ok, err)
	}
	var ks KeySet
	if err := ecf.Decode(ent.Data, &ks); err != nil {
		t.Fatalf("decode key-set: %v", err)
	}
	return ks.Keys
}

// waitDrained blocks until the clock has consumed every queued input value, or
// fails the test. The tick must also COMPLETE after consuming the last value,
// so this waits one further tick beyond the empty queue.
func waitDrained(t *testing.T, h *Host) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		h.mu.Lock()
		depth := len(h.pending["keys"])
		ticks := h.ticks
		h.mu.Unlock()
		if depth == 0 {
			for time.Now().Before(deadline) {
				h.mu.Lock()
				done := h.ticks > ticks
				h.mu.Unlock()
				if done {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("input queue still holds %d values after 20s — the clock is not draining it", depth)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHostInput_PressAndReleaseBetweenTicksIsStillObserved is the mechanism,
// deterministically: a driver presses and releases with no tick in between —
// exactly what a sub-tick mouse click looks like from the host's side — and the
// press must still reach the step.
//
// Before the queue this asserted nothing, because the release overwrote the
// press in the tree and the tick read 0.
func TestHostInput_PressAndReleaseBetweenTicksIsStillObserved(t *testing.T) {
	ap, h := mountLifeEdit(t, 12345)
	start := readLifeEdit(t, ap, h).Cursor

	offer(t, h, 1<<LifeKeyRight) // press
	offer(t, h, 0)               // release, before ANY tick

	if !h.tickOnce() {
		t.Fatalf("faulted: %s", h.Render().Err)
	}
	if got := readLifeEdit(t, ap, h).Cursor; got != start+1 {
		t.Fatalf("cursor %d after the press tick, want %d — the press was dropped by the sampler", got, start+1)
	}

	// The release is the next queued value, so the following tick sees the bit
	// clear. That is what makes the NEXT press a fresh 0->1 edge rather than a
	// continuous hold.
	if !h.tickOnce() {
		t.Fatalf("faulted: %s", h.Render().Err)
	}
	if got := readKeysPort(t, ap, h); got != 0 {
		t.Fatalf("port mask %d after the release tick, want 0", got)
	}
	if got := readLifeEdit(t, ap, h).Cursor; got != start+1 {
		t.Fatalf("cursor %d after the release tick, want %d — a release must not move it", got, start+1)
	}

	// Queue drained: further ticks advance Life and leave the cursor alone.
	if !h.tickOnce() {
		t.Fatalf("faulted: %s", h.Render().Err)
	}
	if got := readLifeEdit(t, ap, h).Cursor; got != start+1 {
		t.Fatalf("cursor %d on an empty queue, want %d", got, start+1)
	}
}

// TestHostInput_SubTickClicksReachAClockedProgram is the region test — a real
// running tick clock, driven from outside exactly as the Avalonia panel and the
// bridge drive it. It is the shape of instrument that was missing.
//
// Not timing-sensitive despite the sleeps: the queue makes observation
// independent of when the press lands relative to the tick, which is the whole
// property under test. Before the queue this measured 1 of 6.
func TestHostInput_SubTickClicksReachAClockedProgram(t *testing.T) {
	ap, h := mountLifeEdit(t, 12345)
	start := readLifeEdit(t, ap, h).Cursor

	h.Start()
	defer h.Stop()
	time.Sleep(400 * time.Millisecond) // let the clock get going

	const presses = 6
	for i := 0; i < presses; i++ {
		offer(t, h, 1<<LifeKeyRight)
		time.Sleep(25 * time.Millisecond) // a real mouse click, well under a tick
		offer(t, h, 0)
		// Wait for the clock to consume both values rather than sleeping a
		// fixed span. A fixed span couples this test to the tick COST, which
		// -race inflates enough to build a backlog and hit the queue cap —
		// that would be the test measuring the wrong property.
		waitDrained(t, h)
	}

	// The cursor wraps within its row (width 16), so compare modulo the row.
	got := readLifeEdit(t, ap, h).Cursor
	row := start / lifeWidth
	want := row*lifeWidth + (start+presses)%lifeWidth
	if got != want {
		t.Fatalf("cursor %d after %d sub-tick clicks, want %d (started at %d) — "+
			"presses are being dropped between ticks", got, presses, want, start)
	}
}

// TestHostInput_BacklogCoalescesAtTheTail pins the bound. A driver that outruns
// the clock cannot grow the queue without limit, and what it loses is the
// MIDDLE of the backlog, never the newest value — a port that ends up
// disagreeing with the operator's hand is the one failure this must not have.
func TestHostInput_BacklogCoalescesAtTheTail(t *testing.T) {
	ap, h := mountLifeEdit(t, 12345)

	const offers = inputQueueMax + 5
	for i := 1; i <= offers; i++ {
		offer(t, h, uint64(i)) // distinct masks, no ticks at all
	}

	h.mu.Lock()
	depth := len(h.pending["keys"])
	h.mu.Unlock()
	if depth > inputQueueMax {
		t.Fatalf("queue depth %d exceeds the %d cap", depth, inputQueueMax)
	}

	// Drain, then the port must hold the LAST value offered.
	for i := 0; i < depth; i++ {
		if !h.tickOnce() {
			t.Fatalf("faulted: %s", h.Render().Err)
		}
	}
	if got := readKeysPort(t, ap, h); got != offers {
		t.Fatalf("port mask %d after draining, want %d (the newest offered value)", got, offers)
	}
}

// TestHostInput_RestartDiscardsQueuedInput — queued presses belong to the run
// that was discarded. Replaying them into a freshly seeded program would make
// Restart non-deterministic for exactly the operators most likely to hit it
// (the ones mashing a button at a program that stopped responding).
func TestHostInput_RestartDiscardsQueuedInput(t *testing.T) {
	ap, h := mountLifeEdit(t, 12345)

	offer(t, h, 1<<LifeKeyRight)
	offer(t, h, 1<<LifeKeyDown)
	if err := h.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	seeded := readLifeEdit(t, ap, h).Cursor
	if !h.tickOnce() {
		t.Fatalf("faulted: %s", h.Render().Err)
	}
	if got := readLifeEdit(t, ap, h).Cursor; got != seeded {
		t.Fatalf("cursor moved to %d on the first tick after Restart (seeded at %d) — "+
			"the previous run's queued input was replayed", got, seeded)
	}
}
