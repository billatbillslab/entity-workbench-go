package shellcmd

import (
	"testing"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/crypto"
)

// TestPersistAliases_RoundTrip verifies the integration helper wires
// addConn/removeConn to SaveAlias/RemoveAlias on the workspace state.
func TestPersistAliases_RoundTrip(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	state := entitysdk.NewWorkspaceState(ap.Store())
	ws := NewShellWorkspace(ap, "self", "")
	ws.PersistAliases(state)

	ws.addConn(&PeerConn{Alias: "mydev", Address: "127.0.0.1:9000", PeerID: "peer-x", Peer: ap})

	a, ok := state.ReadAlias("mydev")
	if !ok {
		t.Fatal("expected mydev persisted after addConn")
	}
	if a.PeerID != "peer-x" || a.Address != "127.0.0.1:9000" {
		t.Errorf("alias persisted as %+v", a)
	}

	ws.removeConn("mydev")
	if _, ok := state.ReadAlias("mydev"); ok {
		t.Error("expected mydev gone after removeConn")
	}

	// Nil state detaches.
	ws.PersistAliases(nil)
	if ws.OnConnAdded != nil || ws.OnConnRemoved != nil {
		t.Error("PersistAliases(nil) should clear hooks")
	}
}

// TestPublishWDForPanel verifies the helper writes the new WD into
// both the publishing panel's own slot and the active screen aggregate.
func TestPublishWDForPanel(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	pc := ap.PeerContext()
	state := entitysdk.NewWorkspaceState(ap.Store())

	const panelID = uint32(11)
	activeScreen := 2
	publish := PublishWDForPanel(state, panelID, func() int { return activeScreen })
	if publish == nil {
		t.Fatal("PublishWDForPanel returned nil with non-nil state")
	}

	target := Path("/peer-abc/foo/bar")
	publish("/", target)
	_ = pc // cache removed

	// Screen aggregate.
	sel, ok := state.ReadSelection(activeScreen)
	if !ok {
		t.Fatal("expected selection on active screen aggregate")
	}
	if sel.Path != string(target) {
		t.Errorf("aggregate Path = %q, want %q", sel.Path, target)
	}
	if sel.PeerID != "peer-abc" {
		t.Errorf("aggregate PeerID = %q, want peer-abc", sel.PeerID)
	}
	if sel.Type != "entity" {
		t.Errorf("aggregate Type = %q, want entity", sel.Type)
	}

	// Panel's own slot.
	pSel, ok := state.ReadPanelSelection(panelID)
	if !ok {
		t.Fatal("expected selection on panel slot")
	}
	if pSel.Path != string(target) {
		t.Errorf("panel Path = %q, want %q", pSel.Path, target)
	}
	if pSel.Type != "entity" {
		t.Errorf("panel Type = %q, want entity", pSel.Type)
	}

	// Other screens unaffected.
	if _, ok := state.ReadSelection(0); ok {
		t.Error("publish leaked to screen 0")
	}

	// Nil state → nil callback.
	if PublishWDForPanel(nil, panelID, func() int { return 0 }) != nil {
		t.Error("PublishWDForPanel(nil, …) should return nil")
	}
}
