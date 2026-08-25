package shellcmd

import (
	"testing"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/crypto"
)

// TestShellWorkspace_TwoSessionsShareAliases verifies that two Shell
// sessions over the same ShellWorkspace observe a shared alias table.
// Adding a connection through the workspace is visible from both
// shells; per-shell WD stays independent. This is the substrate
// guarantee multi-panel renderers (canvas, console) rely on.
func TestShellWorkspace_TwoSessionsShareAliases(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	ws := NewShellWorkspace(ap, "self", "")
	shA := NewShellInWorkspace(ws)
	shB := NewShellInWorkspace(ws)

	if shA.WD != "/" || shB.WD != "/" {
		t.Fatalf("expected both shells to start at /, got %q and %q", shA.WD, shB.WD)
	}
	shA.WD = Path("/" + ap.PeerID() + "/foo")
	if shB.WD != "/" {
		t.Fatalf("shell B's WD changed when only A's was mutated: %q", shB.WD)
	}

	fakePeerID := "z00fakepeerid000000000000000000000000000000000"
	ws.addConn(&PeerConn{
		Alias:   "remote",
		Address: "127.0.0.1:9999",
		PeerID:  fakePeerID,
		Peer:    ap,
	})

	if pc, ok := shA.Conns["remote"]; !ok || pc.PeerID != fakePeerID {
		t.Fatalf("shell A doesn't see remote alias added via workspace")
	}
	if pc, ok := shB.Conns["remote"]; !ok || pc.PeerID != fakePeerID {
		t.Fatalf("shell B doesn't see remote alias added via workspace")
	}
	if shA.AliasFor(fakePeerID) != "remote" || shB.AliasFor(fakePeerID) != "remote" {
		t.Fatalf("AliasFor disagrees between shells")
	}

	ws.removeConn("remote")
	if _, ok := shA.Conns["remote"]; ok {
		t.Fatalf("shell A still sees remote alias after removal")
	}
	if _, ok := shB.Conns["remote"]; ok {
		t.Fatalf("shell B still sees remote alias after removal")
	}
}

// TestShellWorkspace_ConnHooks verifies OnConnAdded fires on addConn
// and OnConnRemoved fires on removeConn. Embedding renderers rely on
// these to persist alias bindings.
func TestShellWorkspace_ConnHooks(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	ws := NewShellWorkspace(ap, "self", "")

	var added []*PeerConn
	var removed []string
	ws.OnConnAdded = func(pc *PeerConn) { added = append(added, pc) }
	ws.OnConnRemoved = func(alias string) { removed = append(removed, alias) }

	pc := &PeerConn{Alias: "remote", Address: "127.0.0.1:9999", PeerID: "peer-x", Peer: ap}
	ws.addConn(pc)
	if len(added) != 1 || added[0].Alias != "remote" {
		t.Fatalf("OnConnAdded fired %d times, got %+v", len(added), added)
	}

	ws.removeConn("remote")
	if len(removed) != 1 || removed[0] != "remote" {
		t.Fatalf("OnConnRemoved fired %d times, got %v", len(removed), removed)
	}

	// removeConn for unknown alias must not fire.
	ws.removeConn("nope")
	if len(removed) != 1 {
		t.Errorf("OnConnRemoved fired for unknown alias: %v", removed)
	}
}

// TestShell_SetWDFiresHook verifies SetWD invokes OnWDChanged with the
// previous and new working directory. Embedding panels rely on this to
// publish WD changes into their presentation context's selection slot.
func TestShell_SetWDFiresHook(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	sh := NewShell(ap, "self", "")

	var got struct {
		prev, next Path
		fired      int
	}
	sh.OnWDChanged = func(prev, next Path) {
		got.prev = prev
		got.next = next
		got.fired++
	}

	target := Path("/" + ap.PeerID() + "/foo")
	sh.SetWD(target)
	if got.fired != 1 {
		t.Fatalf("hook fired %d times, want 1", got.fired)
	}
	if got.prev != "/" || got.next != target {
		t.Errorf("hook got (prev=%q next=%q), want (/ → %q)", got.prev, got.next, target)
	}

	// Nil hook is a no-op (standalone REPL case).
	sh.OnWDChanged = nil
	sh.SetWD("/")
	if sh.WD != "/" {
		t.Errorf("SetWD with nil hook didn't update field: %q", sh.WD)
	}
}
