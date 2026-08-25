package shellboot_test

import (
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/shellboot"
)

// TestCreate_AdvertisesTheListenerItBound is the real-session pin for
// §6.5.1a D1 self-publication: a peer created with a listener now tells
// the tree how to reach it, which is the precondition for every
// connectivity piece above it (a browser cannot maintain, meet, or
// report liveness against a peer that advertises no dial address).
//
// Tier: real-session (TESTING-STRATEGY) — it boots a peer through the
// same PeerManager.Create path a frontend uses, binds a real listener
// on an ephemeral loopback port, and reads the profile back out of the
// store. Not a unit test of the derivation helper.
func TestCreate_AdvertisesTheListenerItBound(t *testing.T) {
	m := shellboot.NewPeerManager("test-advertise")
	h, err := m.Create(shellboot.Config{
		// Port 0 is a real bind: the kernel picks the port. The
		// advertised URL therefore carries the pre-bind form, which is
		// the known limitation named in AdvertisedURL's doc — it is
		// asserted here rather than hidden, so a fix has a pin to move.
		ListenAddr:   "ws://127.0.0.1:19100/ws",
		AdvertiseURL: "",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	hp := m.Get(h)
	if hp == nil {
		t.Fatal("Get: hosted peer missing after Create")
	}
	defer m.Destroy(h)

	if hp.AdvertiseErr != nil {
		t.Fatalf("AdvertiseErr = %v, want nil for a concrete bind host", hp.AdvertiseErr)
	}
	if hp.AdvertisedURL != "ws://127.0.0.1:19100/ws" {
		t.Errorf("AdvertisedURL = %q, want the derived dial URL", hp.AdvertisedURL)
	}

	ap := hp.AppPeer
	idHash, err := types.ComputePeerIdentityHashFromPeerID(crypto.PeerID(ap.PeerID()))
	if err != nil {
		t.Fatalf("identity hash: %v", err)
	}
	path := "system/peer/transport/" + types.PeerIdentityHashHex(idHash) + "/primary-ws"
	ph, ok := ap.RawLocationIndex().Get(path)
	if !ok {
		t.Fatalf("no websocket profile bound at %s — the peer listens and says nothing", path)
	}
	ent, ok := ap.RawContentStore().Get(ph)
	if !ok {
		t.Fatalf("profile %s bound but not stored", ph)
	}
	data, err := types.WebSocketProfileDataFromEntity(ent)
	if err != nil {
		t.Fatalf("decode websocket profile: %v", err)
	}
	if data.PeerID != ap.PeerID() {
		t.Errorf("profile.peer_id = %q, want %q", data.PeerID, ap.PeerID())
	}
	if data.Endpoint.URL != "ws://127.0.0.1:19100/ws" {
		t.Errorf("profile.endpoint.url = %q", data.Endpoint.URL)
	}
	if data.Freshness != "live" {
		t.Errorf("profile.freshness = %q, want live", data.Freshness)
	}
}

// TestCreate_WildcardBindIsNotAdvertised pins the non-fatal refusal: a
// peer that binds 0.0.0.0 still listens, still works for anyone told
// its address out-of-band, and publishes NO profile — because a durable
// claim that peers can dial 0.0.0.0 is worse than silence.
func TestCreate_WildcardBindIsNotAdvertised(t *testing.T) {
	m := shellboot.NewPeerManager("test-advertise-wildcard")
	h, err := m.Create(shellboot.Config{ListenAddr: "0.0.0.0:19101"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	hp := m.Get(h)
	if hp == nil {
		t.Fatal("Get: hosted peer missing after Create")
	}
	defer m.Destroy(h)

	// The listener is up — advertising is a separate concern and its
	// failure must not take the peer down with it.
	if hp.ListenScheme != "tcp" {
		t.Errorf("ListenScheme = %q, want tcp — the listener should still have bound", hp.ListenScheme)
	}
	if hp.AdvertisedURL != "" {
		t.Errorf("AdvertisedURL = %q, want empty for a wildcard bind", hp.AdvertisedURL)
	}
	if hp.AdvertiseErr == nil {
		t.Fatal("AdvertiseErr = nil; a wildcard bind must not silently publish a profile")
	}
	if !strings.Contains(hp.AdvertiseErr.Error(), "unroutable_advertisement") {
		t.Errorf("AdvertiseErr does not name the reason: %v", hp.AdvertiseErr)
	}
}
