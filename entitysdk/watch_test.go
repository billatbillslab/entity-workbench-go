package entitysdk

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func drainEvent(t *testing.T, w *StoreWatch) (ChangeEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-w.Events():
		return ev, ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for event")
		return ChangeEvent{}, false
	}
}

func TestWatchExactMatch(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	w, err := ap.Store().Watch("workspace/settings/theme")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := ap.Store().Put("workspace/settings/theme", "app/state/setting",
		map[string]interface{}{"key": "theme", "value": "dark"}); err != nil {
		t.Fatal(err)
	}

	ev, ok := drainEvent(t, w)
	if !ok {
		t.Fatal("event channel closed")
	}
	if ev.EventType != ChangePut {
		t.Errorf("got EventType %q, want put", ev.EventType)
	}
	if !strings.HasSuffix(ev.Path, "/workspace/settings/theme") {
		t.Errorf("event path %q does not end with /workspace/settings/theme", ev.Path)
	}
}

func TestWatchPrefixMatch(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	w, err := ap.Store().Watch("workspace/settings/*")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	paths := []string{
		"workspace/settings/theme",
		"workspace/settings/font",
	}
	for _, p := range paths {
		if _, err := ap.Store().Put(p, "app/state/setting",
			map[string]interface{}{"key": "k", "value": "v"}); err != nil {
			t.Fatal(err)
		}
	}

	got := 0
	deadline := time.After(500 * time.Millisecond)
	for got < len(paths) {
		select {
		case ev := <-w.Events():
			if !strings.Contains(ev.Path, "/workspace/settings/") {
				t.Errorf("unexpected event path %q", ev.Path)
			}
			got++
		case <-deadline:
			t.Fatalf("only saw %d events, want %d", got, len(paths))
		}
	}
}

func TestWatchIgnoresUnmatchedPaths(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	w, err := ap.Store().Watch("workspace/*")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := ap.Store().Put("elsewhere/foo", "test/v", 1); err != nil {
		t.Fatal(err)
	}
	// Then a matching write so we have something to wait on.
	if _, err := ap.Store().Put("workspace/foo", "test/v", 1); err != nil {
		t.Fatal(err)
	}

	ev, ok := drainEvent(t, w)
	if !ok {
		t.Fatal("channel closed")
	}
	if !strings.Contains(ev.Path, "/workspace/foo") {
		t.Errorf("first event was %q, expected workspace/foo", ev.Path)
	}

	select {
	case ev := <-w.Events():
		t.Errorf("received unexpected second event: %q", ev.Path)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatchRemoveEvent(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	// Register the watch before the Put so we see both events in order
	// and can drain the Put before issuing Remove.
	w, err := ap.Store().Watch("tmp/*")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := ap.Store().Put("tmp/x", "test/v", 1); err != nil {
		t.Fatal(err)
	}
	if putEv, ok := drainEvent(t, w); !ok || putEv.EventType != ChangePut {
		t.Fatalf("expected put event first, got %+v ok=%v", putEv, ok)
	}

	if !ap.Store().Remove("tmp/x") {
		t.Fatal("Remove returned false for existing path")
	}

	ev, ok := drainEvent(t, w)
	if !ok {
		t.Fatal("channel closed")
	}
	if ev.EventType != ChangeRemove {
		t.Errorf("got %q, want remove", ev.EventType)
	}
	if !ev.NewHash.IsZero() {
		t.Error("remove event carried a non-zero NewHash")
	}
}

func TestUnwatchClosesChannel(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	w, err := ap.Store().Watch("anything/*")
	if err != nil {
		t.Fatal(err)
	}
	ap.Store().Unwatch(w)

	select {
	case _, ok := <-w.Events():
		if ok {
			t.Error("channel was not closed after Unwatch")
		}
	case <-time.After(50 * time.Millisecond):
		t.Error("channel did not close promptly after Unwatch")
	}
}

func TestWatchPatternValidation(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	cases := []string{
		"",
		"workspace/*/theme",
		"*",
		"*/settings",
	}
	for _, p := range cases {
		_, err := ap.Store().Watch(p)
		if err == nil {
			t.Errorf("pattern %q unexpectedly accepted", p)
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Status != 400 {
			t.Errorf("pattern %q returned %T %v, want 400 Error", p, err, err)
		}
	}
}

func TestPeerCloseClosesWatches(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}

	w, err := ap.Store().Watch("anything/*")
	if err != nil {
		t.Fatal(err)
	}

	ap.Close()

	select {
	case _, ok := <-w.Events():
		if ok {
			t.Error("channel was not closed after peer Close")
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("channel did not close after peer Close")
	}
}

// TestWatchCloseWithSenderParkedOnFullBuffer pins the fix for a
// send-on-closed-channel defect in the hub.
//
// The bug: watchHub.run sampled w.closed under w.mu, released w.mu,
// and only then sent on w.events. A cancel landing in that window
// closed the channel out from under the sender — "send on closed
// channel", a real panic rather than only a -race report. The fix
// closes w.done before w.events and holds w.mu across an abortable
// `select { case events <- ce: case <-done: }`.
//
// This drives the window deterministically instead of racing for it:
// a watch with NO consumer, more writes than the 64-entry buffer, so
// the hub goroutine is parked inside the send when Close arrives.
// (The foreign-namespace gate surfaced the defect first, but only on
// roughly one run in three — a pin that flaky is not a pin.)
//
// It also covers the property the fix must not break: Close must not
// deadlock behind a consumer that has stopped reading.
func TestWatchCloseWithSenderParkedOnFullBuffer(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	st := ap.Store()

	w, err := st.Watch("parked/*")
	if err != nil {
		t.Fatal(err)
	}

	// No consumer. Writes run on their own goroutine because once the
	// hub parks, the core sink backs up and Put blocks too.
	writesDone := make(chan struct{})
	go func() {
		defer close(writesDone)
		for i := 0; i < 300; i++ {
			_, _ = st.Put("parked/e", "test/note", i)
		}
	}()

	// Let the buffer fill and the hub park in the send.
	time.Sleep(250 * time.Millisecond)

	// Close with a sender parked on the channel.
	w.Close()

	select {
	case <-writesDone:
	case <-time.After(10 * time.Second):
		t.Fatal("writes never drained after Close — the hub is stuck in a send " +
			"(Close must abort the in-flight delivery, not wait on a consumer)")
	}

	// The channel is closed and drained, and a second Close is a
	// no-op rather than a double-close panic.
	for range w.Events() {
	}
	w.Close()
}
