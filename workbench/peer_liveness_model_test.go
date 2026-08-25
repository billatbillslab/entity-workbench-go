package workbench

import (
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// liveStore builds a store with a real watch hub. The bare
// NewStore(cs, li) test scaffolding has no hub and OnPrefixChange
// degrades to seed-only — which would let every assertion below pass
// on the construction-time seed alone and never exercise a single
// transition. A model whose subscription is dead looks identical to a
// working one under that harness (AP4's shape: the pipeline under test
// was never the pipeline that ran).
func liveStore(t *testing.T) *Store {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { ap.Close() })
	return ap.Store()
}

// awaitStatus polls Render until one peer reaches an expected status.
func awaitStatus(t *testing.T, m *PeerLivenessModel, peerID, want string) PeerLivenessOutput {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		out := m.Render()
		for _, l := range out.Peers {
			if l.PeerID == peerID && l.Status == want {
				return out
			}
		}
		select {
		case <-deadline:
			t.Fatalf("peer %s never reached status %q within 3s (rows: %+v)", peerID, want, out.Peers)
			return out
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// awaitPeers polls Render until the row count settles, because a
// prefix-change event crosses a goroutine boundary. Bounded so a dead
// subscription fails instead of hanging.
func awaitPeers(t *testing.T, m *PeerLivenessModel, want int) PeerLivenessOutput {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		out := m.Render()
		if len(out.Peers) == want {
			return out
		}
		select {
		case <-deadline:
			t.Fatalf("model shows %d peers after 3s, want %d", len(out.Peers), want)
			return out
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// seedPeerStatus writes a status entity the way core/peer's lifecycle
// paths do: at system/peer/status/{remote_identity_hash_hex}, with the
// remote's Base58 id in the entity's own peer_id field.
func seedPeerStatus(t *testing.T, st *Store, remote crypto.PeerID, status, reason string) {
	t.Helper()
	idHash, err := types.ComputePeerIdentityHashFromPeerID(remote)
	if err != nil {
		t.Fatal(err)
	}
	d := types.PeerStatusData{PeerID: string(remote), Status: status, Reason: reason}
	path := types.TypePeerStatus + "/" + types.PeerIdentityHashHex(idHash)
	if _, err := st.Put(path, types.TypePeerStatus, d); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func newTestPeerID(t *testing.T) crypto.PeerID {
	t.Helper()
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return kp.PeerID()
}

func TestPeerLivenessModel_NilStoreAndCloseIdempotent(t *testing.T) {
	m := NewPeerLivenessModel(nil)
	out := m.Render()
	if len(out.Peers) != 0 || out.Connected != 0 || out.SeedError != nil {
		t.Fatalf("nil store Render = %+v, want empty", out)
	}
	m.Close()
	m.Close()
}

// TestPeerLivenessModel_SeedsThenTracksTransitions covers the two
// halves that have to agree: the O(N) seed at construction, and the
// per-transition update. A model that only seeds looks correct in a
// test that writes first and constructs second — which is why this
// writes on BOTH sides of the constructor.
func TestPeerLivenessModel_SeedsThenTracksTransitions(t *testing.T) {
	st := liveStore(t)

	before := newTestPeerID(t)
	seedPeerStatus(t, st, before, PeerStatusConnected, "")

	m := NewPeerLivenessModel(st)
	defer m.Close()

	if out := m.Render(); len(out.Peers) != 1 || out.Connected != 1 {
		t.Fatalf("after seed: %d peers / %d connected, want 1/1", len(out.Peers), out.Connected)
	}

	// A transition arriving after construction.
	after := newTestPeerID(t)
	seedPeerStatus(t, st, after, PeerStatusSuspect, types.PeerStatusReasonTransportError)

	out := awaitPeers(t, m, 2)
	if out.Connected != 1 || out.Suspect != 1 {
		t.Errorf("counts = %d connected / %d suspect / %d disconnected, want 1/1/0",
			out.Connected, out.Suspect, out.Disconnected)
	}

	// Rows are sorted by peer-id so a renderer gets a stable order
	// across renders rather than Go's map iteration.
	if out.Peers[0].PeerID > out.Peers[1].PeerID {
		t.Errorf("rows not sorted by peer-id: %q then %q", out.Peers[0].PeerID, out.Peers[1].PeerID)
	}

	// The reason rides through to the renderer — it is what turns "not
	// connected" into something a user can act on.
	for _, l := range out.Peers {
		if l.PeerID == string(after) {
			if l.Reason != types.PeerStatusReasonTransportError {
				t.Errorf("reason = %q, want %q", l.Reason, types.PeerStatusReasonTransportError)
			}
			if !l.Failing() {
				t.Error("Failing() false on a suspect peer")
			}
		}
	}
}

// TestPeerLivenessModel_LastWriteWins pins that a re-transition
// replaces rather than accumulates. The status entity is
// transition-written, so each event IS the current state; a model that
// appended would show one peer as connected AND disconnected at once.
func TestPeerLivenessModel_LastWriteWins(t *testing.T) {
	st := liveStore(t)

	p := newTestPeerID(t)
	m := NewPeerLivenessModel(st)
	defer m.Close()

	seedPeerStatus(t, st, p, PeerStatusConnected, "")
	seedPeerStatus(t, st, p, PeerStatusSuspect, types.PeerStatusReasonTransportError)
	seedPeerStatus(t, st, p, PeerStatusDisconnected, types.PeerStatusReasonKeepaliveMiss)

	// Waiting on the row COUNT would return after the first
	// transition — one peer is one peer whichever state it is in.
	// The condition has to be the state, or the test asserts on
	// whatever happened to have arrived.
	out := awaitStatus(t, m, string(p), PeerStatusDisconnected)
	if out.Peers[0].Status != PeerStatusDisconnected {
		t.Errorf("status = %q, want the newest (%q)", out.Peers[0].Status, PeerStatusDisconnected)
	}
	if out.Connected != 0 || out.Suspect != 0 || out.Disconnected != 1 {
		t.Errorf("counts = %d/%d/%d, want 0/0/1", out.Connected, out.Suspect, out.Disconnected)
	}
}
