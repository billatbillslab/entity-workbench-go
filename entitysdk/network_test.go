package entitysdk

import (
	"context"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/types"
)

// TestNetworkClient_MaintainAndRelease runs the maintain-peer /
// status / release-peer surface against two real peers over a real
// listener.
//
// Tier: real-session. The operation is what is under test — that the
// system/network handler is registered AND bound, that a maintain-peer
// dispatch installs a session, and that release-peer takes it down. A
// handler registered but never Bind'ed returns 500 on every op, which
// is precisely the failure a construction-only test would miss (the
// Bind is a separate post-peer.New step, the same shape as clock's
// SetupAdvancement).
func TestNetworkClient_MaintainAndRelease(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	serverPeerID := string(serverKP.PeerID())
	net := client.Network()

	// Nothing is maintained before we ask.
	if st, err := net.Status(ctx); err != nil {
		t.Fatalf("Status before maintain: %v", err)
	} else if len(st.MaintainedPeers) != 0 {
		t.Errorf("MaintainedPeers = %d before any maintain-peer, want 0", len(st.MaintainedPeers))
	}

	res, err := net.MaintainPeer(ctx, serverPeerID, MaintainOpts{Address: server.Addr().String()})
	if err != nil {
		t.Fatalf("MaintainPeer: %v", err)
	}
	if res.PeerID != serverPeerID {
		t.Errorf("result.PeerID = %q, want %q", res.PeerID, serverPeerID)
	}
	if res.SessionID == "" {
		t.Error("result.SessionID is empty — nothing identifies the maintenance session")
	}
	if res.ChainID == "" {
		t.Error("result.ChainID is empty — chain-error markers for a failing reconnect would be unfindable")
	}

	st, err := net.Status(ctx)
	if err != nil {
		t.Fatalf("Status after maintain: %v", err)
	}
	var found bool
	for _, s := range st.MaintainedPeers {
		if s.PeerID == serverPeerID {
			found = true
		}
	}
	if !found {
		t.Fatalf("maintained peer %s missing from status (%d rows)", serverPeerID, len(st.MaintainedPeers))
	}

	// The liveness read-model and the maintained set are different
	// questions, and the test asserts they are both answered rather
	// than assuming one implies the other.
	live, ok, err := client.PeerLivenessOf(serverPeerID)
	if err != nil {
		t.Fatalf("PeerLivenessOf: %v", err)
	}
	if !ok {
		t.Fatal("no liveness recorded after maintain-peer dialed the server")
	}
	if live.Status != types.PeerStatusConnected {
		t.Errorf("liveness status = %q, want connected", live.Status)
	}

	// MaintainedPeers is the filtered view, and it agrees while the
	// session is live.
	if maintained, err := net.MaintainedPeers(ctx); err != nil {
		t.Fatalf("MaintainedPeers: %v", err)
	} else if len(maintained) != 1 || maintained[0].PeerID != serverPeerID {
		t.Errorf("MaintainedPeers = %+v, want exactly %s", maintained, serverPeerID)
	}

	if err := net.ReleasePeer(ctx, serverPeerID, "shutdown"); err != nil {
		t.Fatalf("ReleasePeer: %v", err)
	}

	// After release the peer is STILL a row in Status — §4.3
	// enumerates system/peer/status/, not the session set, so a
	// released peer keeps its row and loses only its session_id. This
	// assertion is deliberately the surprising one: it is what the
	// implementation does, verified by running it, and the reason
	// MaintainedPeers exists.
	st, err = net.Status(ctx)
	if err != nil {
		t.Fatalf("Status after release: %v", err)
	}
	var row *types.NetworkPeerSummaryData
	for i := range st.MaintainedPeers {
		if st.MaintainedPeers[i].PeerID == serverPeerID {
			row = &st.MaintainedPeers[i]
		}
	}
	if row == nil {
		t.Fatalf("peer %s vanished from Status after release; expected a row with no session_id", serverPeerID)
	}
	if row.SessionID != "" {
		t.Errorf("session_id = %q after ReleasePeer, want empty — it is the only thing that says the session ended", row.SessionID)
	}
	if maintained, err := net.MaintainedPeers(ctx); err != nil {
		t.Fatalf("MaintainedPeers after release: %v", err)
	} else if len(maintained) != 0 {
		t.Errorf("MaintainedPeers = %+v after release, want empty", maintained)
	}
}

// TestNetworkClient_RejectsEmptyPeer pins the argument guards. An empty
// peer-id reaching the handler is a 400 there too, but failing at the
// call site keeps a typo out of the dispatch path entirely.
func TestNetworkClient_RejectsEmptyPeer(t *testing.T) {
	ap, err := CreatePeer(PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	ctx := context.Background()
	net := ap.Network()
	if _, err := net.MaintainPeer(ctx, "", MaintainOpts{}); err == nil {
		t.Error("MaintainPeer(\"\") returned nil error")
	}
	if err := net.ReleasePeer(ctx, "", "shutdown"); err == nil {
		t.Error("ReleasePeer(\"\") returned nil error")
	}
	if err := net.Close(ctx, "", "shutdown"); err == nil {
		t.Error("Close(\"\") returned nil error")
	}
}
