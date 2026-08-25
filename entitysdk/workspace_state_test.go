package entitysdk

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func testWorkspaceState(t *testing.T) (*WorkspaceState, *PeerContext) {
	t.Helper()
	pc, _, _ := testPeerContext(t)
	_ = pc // cache removed
	ws := NewWorkspaceState(pc.Store())
	return ws, pc
}

func TestWorkspaceState_Setting(t *testing.T) {
	ws, _ := testWorkspaceState(t)

	// Read non-existent setting
	if v := ws.ReadSetting("foo"); v != "" {
		t.Errorf("expected empty, got %q", v)
	}

	// Write and read back
	ws.SaveSetting("log-level", "verbose")
	if v := ws.ReadSetting("log-level"); v != "verbose" {
		t.Errorf("expected verbose, got %q", v)
	}

	// Overwrite
	ws.SaveSetting("log-level", "debug")
	if v := ws.ReadSetting("log-level"); v != "debug" {
		t.Errorf("expected debug, got %q", v)
	}
}

func TestWorkspaceState_WindowSetting(t *testing.T) {
	ws, _ := testWorkspaceState(t)

	ws.SaveWindowSetting(42, "log-display-level", "info")
	if v := ws.ReadWindowSetting(42, "log-display-level"); v != "info" {
		t.Errorf("expected info, got %q", v)
	}

	// Different window
	ws.SaveWindowSetting(43, "log-display-level", "debug")
	if v := ws.ReadWindowSetting(43, "log-display-level"); v != "debug" {
		t.Errorf("expected debug, got %q", v)
	}
	// First window unchanged
	if v := ws.ReadWindowSetting(42, "log-display-level"); v != "info" {
		t.Errorf("expected info still, got %q", v)
	}
}

func TestWorkspaceState_WindowContent(t *testing.T) {
	ws, pc := testWorkspaceState(t)

	ws.SaveWindowContent(1, "tree-browser")
	_ = pc // cache removed

	// Bundled state lives at windows/{id}/state with type
	// app/state/window. The content-type is one field in the bundle.
	r, ok := pc.Resolve(ws.WindowStatePath(1))
	if !ok {
		t.Fatal("expected entity at window state path")
	}
	if r.Entity.Type != "app/state/window" {
		t.Errorf("type = %q, want app/state/window", r.Entity.Type)
	}
	m, ok := r.Decoded.(map[interface{}]interface{})
	if !ok {
		t.Fatal("expected map")
	}
	if m["content-type"] != "tree-browser" {
		t.Errorf("content-type = %v, want tree-browser", m["content-type"])
	}
}

// TestWorkspaceState_WindowBundle exercises the bundling: multiple
// keys for the same window must coexist in a single entity, and
// updates must preserve unrelated fields.
func TestWorkspaceState_WindowBundle(t *testing.T) {
	ws, pc := testWorkspaceState(t)

	ws.SaveWindowContent(5, "log-viewer")
	ws.SaveWindowSetting(5, "log-display-level", "verbose")
	ws.SaveWindowScreen(5, 2)
	_ = pc // cache removed

	r, ok := pc.Resolve(ws.WindowStatePath(5))
	if !ok {
		t.Fatal("expected bundled entity")
	}
	m, ok := r.Decoded.(map[interface{}]interface{})
	if !ok {
		t.Fatal("expected map")
	}
	if m["content-type"] != "log-viewer" {
		t.Errorf("content-type = %v", m["content-type"])
	}
	if m["log-display-level"] != "verbose" {
		t.Errorf("log-display-level = %v", m["log-display-level"])
	}
	if m["screen"] != "2" {
		t.Errorf("screen = %v", m["screen"])
	}

	// Updating one field must not erase the others.
	ws.SaveWindowSetting(5, "log-display-level", "debug")
	if v := ws.ReadWindowSetting(5, "log-display-level"); v != "debug" {
		t.Errorf("after update: log-display-level = %q", v)
	}
	_ = pc // cache removed
	r, _ = pc.Resolve(ws.WindowStatePath(5))
	m = r.Decoded.(map[interface{}]interface{})
	if m["content-type"] != "log-viewer" {
		t.Errorf("content-type lost on update: %v", m["content-type"])
	}
	if m["screen"] != "2" {
		t.Errorf("screen lost on update: %v", m["screen"])
	}
}

func TestWorkspaceState_Selection(t *testing.T) {
	ws, _ := testWorkspaceState(t)

	// Read non-existent
	sel, ok := ws.ReadSelection(0)
	if ok || sel.Path != "" {
		t.Errorf("expected no selection, got %+v ok=%v", sel, ok)
	}

	// Write and read back — minimal payload (Path only). UpdatedAt
	// must be auto-filled.
	ws.SaveSelection(0, Selection{Path: "test/hello"})
	sel, ok = ws.ReadSelection(0)
	if !ok || sel.Path != "test/hello" {
		t.Errorf("expected test/hello, got %+v ok=%v", sel, ok)
	}
	if sel.UpdatedAt == 0 {
		t.Errorf("expected UpdatedAt auto-fill, got 0")
	}

	// Update
	ws.SaveSelection(0, Selection{Path: "test/other"})
	sel, _ = ws.ReadSelection(0)
	if sel.Path != "test/other" {
		t.Errorf("expected test/other, got %q", sel.Path)
	}

	// Per-screen scoping: writing to screen 1 must not affect screen 0.
	ws.SaveSelection(1, Selection{Path: "other-screen/path"})
	sel, _ = ws.ReadSelection(0)
	if sel.Path != "test/other" {
		t.Errorf("screen 0 selection clobbered: %q", sel.Path)
	}
	sel, _ = ws.ReadSelection(1)
	if sel.Path != "other-screen/path" {
		t.Errorf("screen 1 selection wrong: %q", sel.Path)
	}
}

// TestWorkspaceState_SelectionFullSchema covers all surviving Selection
// fields after the SHELL-DIRECTION.md §8.4 simplification: path, type,
// peer_id, updated_at. content_type, source_window, paths were dropped.
func TestWorkspaceState_SelectionFullSchema(t *testing.T) {
	ws, _ := testWorkspaceState(t)

	in := Selection{
		Path:      "tree/foo",
		Type:      "entity",
		PeerID:    "peer-abc",
		UpdatedAt: 1_700_000_000_000,
	}
	ws.SaveSelection(0, in)
	out, ok := ws.ReadSelection(0)
	if !ok {
		t.Fatal("expected selection present")
	}
	if out.Path != in.Path {
		t.Errorf("Path = %q, want %q", out.Path, in.Path)
	}
	if out.Type != in.Type {
		t.Errorf("Type = %q, want %q", out.Type, in.Type)
	}
	if out.PeerID != in.PeerID {
		t.Errorf("PeerID = %q, want %q", out.PeerID, in.PeerID)
	}
	if out.UpdatedAt != in.UpdatedAt {
		t.Errorf("UpdatedAt = %d, want %d", out.UpdatedAt, in.UpdatedAt)
	}
}

// TestWorkspaceState_SelectionOmitsEmptyOptionals verifies that
// optional fields are not written when zero/empty — guide §5.4 treats
// absence as unset.
func TestWorkspaceState_SelectionOmitsEmptyOptionals(t *testing.T) {
	ws, pc := testWorkspaceState(t)

	ws.SaveSelection(0, Selection{Path: "tree/foo"})
	_ = pc // cache removed

	r, ok := pc.Resolve(ws.screenSelectionPath(0))
	if !ok {
		t.Fatal("expected entity at selection path")
	}
	m, ok := r.Decoded.(map[interface{}]interface{})
	if !ok {
		t.Fatal("expected map payload")
	}
	for _, k := range []string{"type", "peer_id"} {
		if _, present := m[k]; present {
			t.Errorf("optional field %q should be absent when zero, got %v", k, m[k])
		}
	}
	if _, present := m["updated_at"]; !present {
		t.Errorf("updated_at must always be present (auto-filled when zero)")
	}
}

// TestWorkspaceState_OnSelectionChange verifies the callback wrapper
// over Store.Watch: SaveSelection at a watched path fires the handler
// with the decoded Selection, and cancel stops further fires.
func TestWorkspaceState_OnSelectionChange(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	ws := NewWorkspaceState(ap.Store())
	path := ws.screenSelectionPath(0)

	var (
		mu   sync.Mutex
		got  []Selection
		gate = make(chan struct{}, 8)
	)
	cancel := ws.OnSelectionChange(path, func(sel Selection) {
		mu.Lock()
		got = append(got, sel)
		mu.Unlock()
		gate <- struct{}{}
	})
	defer cancel()

	ws.SaveSelection(0, Selection{Path: "tree/foo", PeerID: "peer-abc"})
	select {
	case <-gate:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first OnSelectionChange callback")
	}

	mu.Lock()
	if len(got) != 1 || got[0].Path != "tree/foo" || got[0].PeerID != "peer-abc" {
		t.Errorf("first callback got %+v", got)
	}
	mu.Unlock()

	ws.SaveSelection(0, Selection{Path: "tree/bar"})
	select {
	case <-gate:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second OnSelectionChange callback")
	}

	mu.Lock()
	if len(got) != 2 || got[1].Path != "tree/bar" {
		t.Errorf("second callback got %+v", got)
	}
	mu.Unlock()

	// Cancel; further saves must not deliver to the handler.
	cancel()
	ws.SaveSelection(0, Selection{Path: "tree/baz"})
	select {
	case <-gate:
		t.Fatal("handler fired after cancel")
	case <-time.After(100 * time.Millisecond):
	}

	// Idempotent cancel.
	cancel()
}

// TestWorkspaceState_OnPrefixChange verifies the prefix-scoped
// subscription callback wrapper.
//
// The contract is idempotent "current state of path = X" semantics —
// not delta semantics. The watch hub may deliver duplicate events for
// pre-existing paths (events buffered in the hub sink before subscribe
// + seed both reach the handler). Handlers must therefore be safe to
// run multiple times on the same path with the same value, which is
// the natural shape for "render the entity at this path."
//
// Pinned properties:
//
//   - SEED: paths existing under the prefix at attach time are
//     delivered as ChangePut events.
//   - LIVE: subsequent Put / Remove at matching paths fire the handler
//     with the correct event type.
//   - NON-MATCHING: paths outside the prefix never fire the handler.
//   - CANCEL: idempotent, stops further fires.
//   - IDEMPOTENT: the FINAL event seen for each path matches that
//     path's actual state (Put → ChangePut latest hash, Remove →
//     ChangeRemove).
func TestWorkspaceState_OnPrefixChange(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	ws := NewWorkspaceState(ap.Store())
	store := ap.Store()

	// Seed two paths under the watched prefix BEFORE attaching the watch,
	// plus one outside the prefix that must not fire.
	store.Put("docs/seed-a", "test/note", "A")
	store.Put("docs/seed-b", "test/note", "B")
	store.Put("other/ignored", "test/note", "ignored")

	var (
		mu         sync.Mutex
		lastByPath = make(map[string]ChangeEvent) // final event seen per path
		anyEvent   = make(chan struct{}, 64)      // signal on every fire
		nonMatch   []string                       // paths that fired but shouldn't have
	)
	cancel := ws.OnPrefixChange("docs/", func(ev ChangeEvent) {
		mu.Lock()
		lastByPath[bareSuffix(ev.Path)] = ev
		// Sanity guard: anything not under docs/ is a bug.
		if !hasSuffixAfterPeer(ev.Path, "docs/") {
			nonMatch = append(nonMatch, ev.Path)
		}
		mu.Unlock()
		select {
		case anyEvent <- struct{}{}:
		default:
		}
	})
	defer cancel()

	// Wait until both seed paths have fired at least once.
	waitFor := func(predicate func() bool, timeout time.Duration, what string) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if predicate() {
				return
			}
			select {
			case <-anyEvent:
			case <-time.After(50 * time.Millisecond):
			}
		}
		t.Fatalf("timed out waiting for: %s", what)
	}
	waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		_, a := lastByPath["docs/seed-a"]
		_, b := lastByPath["docs/seed-b"]
		return a && b
	}, 2*time.Second, "seed for docs/seed-a and docs/seed-b")

	// Final event for each seed path must be ChangePut.
	mu.Lock()
	if e := lastByPath["docs/seed-a"]; e.EventType != ChangePut {
		t.Errorf("docs/seed-a final event type=%q, want %q", e.EventType, ChangePut)
	}
	if e := lastByPath["docs/seed-b"]; e.EventType != ChangePut {
		t.Errorf("docs/seed-b final event type=%q, want %q", e.EventType, ChangePut)
	}
	mu.Unlock()

	// Live Put — should appear as ChangePut.
	store.Put("docs/live-c", "test/note", "C")
	waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		_, ok := lastByPath["docs/live-c"]
		return ok
	}, 2*time.Second, "live-c")
	mu.Lock()
	if e := lastByPath["docs/live-c"]; e.EventType != ChangePut {
		t.Errorf("docs/live-c event type=%q, want %q", e.EventType, ChangePut)
	}
	mu.Unlock()

	// Live Put OUTSIDE prefix — must never fire. Quiet check + sanity
	// guard via nonMatch slice.
	store.Put("other/never", "test/note", "never")
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	if _, ok := lastByPath["other/never"]; ok {
		t.Errorf("non-matching path fired handler: other/never")
	}
	if len(nonMatch) > 0 {
		t.Errorf("non-matching paths leaked: %v", nonMatch)
	}
	mu.Unlock()

	// Live Remove — final event for seed-a must be ChangeRemove.
	store.Remove("docs/seed-a")
	waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return lastByPath["docs/seed-a"].EventType == ChangeRemove
	}, 2*time.Second, "remove of docs/seed-a")

	// Cancel; further mutations must not change lastByPath.
	cancel()
	mu.Lock()
	preCancel := len(lastByPath)
	mu.Unlock()
	store.Put("docs/post-cancel", "test/note", "after")
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	if _, ok := lastByPath["docs/post-cancel"]; ok {
		t.Errorf("handler fired after cancel: post-cancel was delivered")
	}
	if len(lastByPath) != preCancel {
		t.Errorf("lastByPath grew after cancel: %d → %d", preCancel, len(lastByPath))
	}
	mu.Unlock()

	// Idempotent cancel.
	cancel()
}

// bareSuffix strips the leading "/{peerID}/" from a qualified path
// so test assertions can use peer-id-free keys.
func bareSuffix(p string) string {
	if len(p) == 0 || p[0] != '/' {
		return p
	}
	rest := p[1:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i+1:]
	}
	return rest
}

// hasSuffixAfterPeer reports whether the bare (peer-id-stripped) path
// begins with prefix. Used to sanity-check that the watch only
// delivers matching paths.
func hasSuffixAfterPeer(p, prefix string) bool {
	return hasPrefix(bareSuffix(p), prefix)
}

func TestWorkspaceState_PathsAreAppScoped(t *testing.T) {
	ws, pc := testWorkspaceState(t)

	ws.SaveSetting("theme", "dark")
	ws.SaveWindowContent(7, "tree")
	ws.SaveActiveScreen(1)
	ws.SaveSelection(0, Selection{Path: "workspace/foo"})
	_ = pc // cache removed

	// Every entry must live under app/workbench/, not under the
	// legacy workspace/ or workspace/settings/ prefixes.
	for _, entry := range pc.Store().List("") {
		if entry.Path == "" {
			continue
		}
		// Strip the leading "/{peerID}/" qualification.
		_, bare := splitNamespacePath(entry.Path)
		if bare == "" {
			continue
		}
		if bare == "system" || hasPrefix(bare, "system/") {
			continue // bootstrap entries from the peer builder
		}
		if !hasPrefix(bare, "app/workbench/") {
			t.Errorf("workspace entry %q is not app/workbench/-scoped", entry.Path)
		}
	}
}

// splitNamespacePath mirrors store.SplitNamespace without importing
// store directly into the test.
func splitNamespacePath(p string) (ns, bare string) {
	if len(p) == 0 || p[0] != '/' {
		return "", p
	}
	p = p[1:]
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[:i], p[i+1:]
		}
	}
	return p, ""
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// TestWorkspaceState_ShellAlias round-trips the alias binding through
// the tree slot, verifies multi-alias coexistence, and confirms
// RemoveAlias clears the slot.
func TestWorkspaceState_ShellAlias(t *testing.T) {
	ws, _ := testWorkspaceState(t)

	if _, ok := ws.ReadAlias("missing"); ok {
		t.Error("expected ReadAlias for unknown to return ok=false")
	}

	ws.SaveAlias("mydev", "peer-abc", "127.0.0.1:9000")
	a, ok := ws.ReadAlias("mydev")
	if !ok {
		t.Fatal("expected alias present")
	}
	if a.Alias != "mydev" || a.PeerID != "peer-abc" || a.Address != "127.0.0.1:9000" {
		t.Errorf("alias round-trip mismatch: %+v", a)
	}

	// Second alias coexists.
	ws.SaveAlias("prod", "peer-xyz", "10.0.0.5:9000")
	a2, _ := ws.ReadAlias("prod")
	if a2.PeerID != "peer-xyz" {
		t.Errorf("second alias PeerID = %q", a2.PeerID)
	}
	a, _ = ws.ReadAlias("mydev")
	if a.PeerID != "peer-abc" {
		t.Errorf("first alias clobbered: %+v", a)
	}

	// Remove drops the slot.
	ws.RemoveAlias("mydev")
	if _, ok := ws.ReadAlias("mydev"); ok {
		t.Error("expected mydev gone after RemoveAlias")
	}
	if _, ok := ws.ReadAlias("prod"); !ok {
		t.Error("RemoveAlias clobbered the wrong alias")
	}
}

// TestWorkspaceState_SelectionLegacyFieldsLogViolation verifies arch's
// landed Amendment A (Option 2): reading a Selection entity that carries
// any retired field (content_type/source_window/source_panel/paths) MUST
// log a WARN-level violation naming the path + field. Silent tolerance
// is NON-CONFORMANT pre-publication.
//
// See GUIDE-ENTITY-WORKBENCH-APP §5.4 (absorption) and
// feedback_no_legacy_pre_release in workbench-go auto-memory.
func TestWorkspaceState_SelectionLegacyFieldsLogViolation(t *testing.T) {
	ws, pc := testWorkspaceState(t)

	// Write an entity with legacy fields directly via Store.Put,
	// bypassing SaveSelection's clean writer. This mimics receiving
	// a legacy record from a non-conformant emitter.
	path := ws.screenSelectionPath(0)
	payload := map[string]interface{}{
		"path":          "tree/foo",
		"updated_at":    uint64(1_700_000_000_000),
		"content_type":  "entity",             // legacy
		"source_window": uint32(7),            // legacy
		"source_panel":  uint32(3),            // legacy
		"paths":         []string{"tree/foo"}, // legacy
	}
	if _, err := ws.store.Put(path, "app/state/selection", payload); err != nil {
		t.Fatal(err)
	}
	_ = pc

	// Capture log output during ReadSelection.
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	sel, ok := ws.ReadSelection(0)
	if !ok {
		t.Fatal("ReadSelection returned !ok (should still decode the live fields)")
	}
	if sel.Path != "tree/foo" {
		t.Errorf("Path = %q, want %q (live field should still decode)", sel.Path, "tree/foo")
	}

	logOutput := buf.String()
	for _, field := range []string{"content_type", "source_window", "source_panel", "paths"} {
		if !strings.Contains(logOutput, field) {
			t.Errorf("expected WARN log naming legacy field %q; got %q", field, logOutput)
		}
	}
	if !strings.Contains(logOutput, "NON-CONFORMANT") {
		t.Errorf("expected WARN log to mark legacy emit as NON-CONFORMANT; got %q", logOutput)
	}
}

func TestWorkspaceState_ActiveScreen(t *testing.T) {
	ws, _ := testWorkspaceState(t)

	// Default
	if s := ws.ReadActiveScreen(); s != 0 {
		t.Errorf("expected 0, got %d", s)
	}

	ws.SaveActiveScreen(2)
	if s := ws.ReadActiveScreen(); s != 2 {
		t.Errorf("expected 2, got %d", s)
	}
}
