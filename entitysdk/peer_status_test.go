package entitysdk

import (
	"context"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/types"
)

// TestPeerLiveness_RealHandshakeWritesConnected is the real-session pin
// for the liveness read-model. It runs the OPERATION — two peers, a real
// listener, a real dial — rather than seeding a status entity and
// reading it back, because the claim being made is "core/peer writes
// this transition and our reader sees it", and a hand-seeded entity
// proves only that our own decoder round-trips (D19 / AP10: the layer
// under the layer you read).
//
// Tier: real-session. It also pins the three properties the read-model
// exists to carry: the entity is the source (not the connection pool),
// absence is reported as absence, and the subscription fires on the
// transition rather than on a poll.
func TestPeerLiveness_RealHandshakeWritesConnected(t *testing.T) {
	serverKP, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	clientKP, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}

	server, err := CreatePeer(PeerConfig{
		Keypair:    &serverKP,
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	})
	if err != nil {
		t.Fatalf("server CreatePeer: %v", err)
	}
	defer server.Close()

	client, err := CreatePeer(PeerConfig{Keypair: &clientKP})
	if err != nil {
		t.Fatalf("client CreatePeer: %v", err)
	}
	defer client.Close()

	serverPeerID := string(serverKP.PeerID())

	// Absence is absence. Before any contact the client has no
	// transition recorded for the server, and that MUST NOT read as
	// "disconnected" — a stranger and a goodbye are different facts.
	if live, found, err := client.PeerLivenessOf(serverPeerID); err != nil {
		t.Fatalf("PeerLivenessOf before contact: %v", err)
	} else if found {
		t.Fatalf("found = true before any contact, with status %q", live.Status)
	}

	// Subscribe BEFORE connecting, so what arrives is the transition
	// itself and not a replay of state that was already there.
	transitions := make(chan PeerLiveness, 8)
	cancelSub := client.OnPeerLivenessChange(func(l PeerLiveness) {
		select {
		case transitions <- l:
		default:
		}
	})
	defer cancelSub()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ready := make(chan struct{})
	listenErr := make(chan error, 1)
	go func() { listenErr <- server.ListenReady(ctx, ready) }()
	select {
	case <-ready:
	case err := <-listenErr:
		t.Fatalf("server listen: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("server not ready in 2s")
	}

	conn, err := client.Connect(ctx, server.Addr().String())
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer conn.Close()

	// The handshake write is what we are asserting on, and it lands
	// asynchronously relative to Connect's return.
	var live PeerLiveness
	deadline := time.After(5 * time.Second)
	for {
		var found bool
		live, found, err = client.PeerLivenessOf(serverPeerID)
		if err != nil {
			t.Fatalf("PeerLivenessOf: %v", err)
		}
		if found && live.Connected() {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no connected status for %s within 5s (found=%v status=%q)", serverPeerID, found, live.Status)
		case <-time.After(20 * time.Millisecond):
		}
	}

	if live.PeerID != serverPeerID {
		t.Errorf("liveness.PeerID = %q, want %q — the id comes from the entity field, not the path hash", live.PeerID, serverPeerID)
	}
	if live.Status != types.PeerStatusConnected {
		t.Errorf("status = %q, want %q", live.Status, types.PeerStatusConnected)
	}
	if live.Failing() {
		t.Errorf("Failing() true on a connected peer (failing_since=%d)", live.FailingSince)
	}

	// The prefix subscription fired on the transition. This is the
	// property that makes the read-model worth having over the pool
	// snapshot: no poll, no scan.
	select {
	case got := <-transitions:
		if got.PeerID != serverPeerID {
			t.Errorf("subscription delivered peer %q, want %q", got.PeerID, serverPeerID)
		}
		if got.Status != types.PeerStatusConnected {
			t.Errorf("subscription delivered status %q, want connected", got.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnPeerLivenessChange never fired for the handshake transition")
	}

	// PeerLivenessAll sees the same peer.
	all, err := client.PeerLivenessAll()
	if err != nil {
		t.Fatalf("PeerLivenessAll: %v", err)
	}
	var seen bool
	for _, l := range all {
		if l.PeerID == serverPeerID {
			seen = true
		}
	}
	if !seen {
		t.Errorf("PeerLivenessAll did not include %s (got %d entries)", serverPeerID, len(all))
	}
}

// TestPeerLiveness_RejectsNonStatusEntity pins the decode guard. A
// zero-valued PeerLiveness is indistinguishable from "not connected" at
// every call site, so a wrong-typed or status-less entity must be an
// error rather than a quiet false.
func TestPeerLiveness_RejectsNonStatusEntity(t *testing.T) {
	ap, err := CreatePeer(PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	other, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	idHash, err := types.ComputePeerIdentityHashFromPeerID(other.PeerID())
	if err != nil {
		t.Fatal(err)
	}
	statusPath := peerStatusPrefix + types.PeerIdentityHashHex(idHash)

	// Wrong type at the right path.
	if _, err := ap.Store().Put(statusPath, "test/not-a-status", map[string]string{"x": "1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := ap.PeerLivenessOf(string(other.PeerID())); err == nil {
		t.Error("PeerLivenessOf accepted a non-status entity")
	}

	// Right type, empty status — the field a caller switches on.
	empty := types.PeerStatusData{PeerID: string(other.PeerID())}
	if _, err := ap.Store().Put(statusPath, types.TypePeerStatus, empty); err != nil {
		t.Fatalf("seed empty status: %v", err)
	}
	if _, _, err := ap.PeerLivenessOf(string(other.PeerID())); err == nil {
		t.Error("PeerLivenessOf accepted a status entity with no status")
	}

	// A malformed entry must not blind the whole view.
	if all, err := ap.PeerLivenessAll(); err != nil {
		t.Errorf("PeerLivenessAll errored on a malformed entry: %v", err)
	} else if len(all) != 0 {
		t.Errorf("PeerLivenessAll returned %d entries, want 0 (the only entry is malformed)", len(all))
	}
}
