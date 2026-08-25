package entitysdk_test

import (
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// TestAdvertiseTransport_SelfPublishesUnderOurOwnPeerID pins the
// direction that did not exist before 2026-08-18: a profile for THIS
// peer, at system/peer/transport/{our-peer-id}/{profile-id}, per
// EXTENSION-NETWORK §6.5.1a D1.
//
// Tier: contract pin. It asserts on the STORE (L0), not through
// AppPeer.Get — a peer-qualified dispatched read of our own namespace
// would be answered by us either way, so it could not distinguish a
// written profile from a fabricated one (AP11).
//
// The websocket case is the load-bearing one: §6.5.2b is the only
// browser transport that carries server→client push, so it is how a
// browser peer learns a Go peer is reachable at all.
func TestAdvertiseTransport_SelfPublishesUnderOurOwnPeerID(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	for _, tc := range []struct {
		name      string
		dialURL   string
		profileID string
		wantType  string
		wantURL   string
	}{
		{"websocket", "ws://127.0.0.1:9100/ws", "primary-ws", types.TypePeerTransportWebSocket, "ws://127.0.0.1:9100/ws"},
		{"tcp", "tcp://127.0.0.1:9101", "primary", types.TypePeerTransportTCP, "tcp://127.0.0.1:9101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ap.AdvertiseTransport(tc.dialURL); err != nil {
				t.Fatalf("AdvertiseTransport(%q): %v", tc.dialURL, err)
			}
			// Path segment is the peer IDENTITY-HASH hex, not the
			// Base58 peer-id: core/types/crypto.go pins the hex form
			// for "non-root path positions (system/peer/status,
			// system/connection, …)", and core-go's
			// transportProfilePrefix follows it. The Base58 form lives
			// in the profile's own peer_id FIELD, checked below.
			idHash, err := types.ComputePeerIdentityHashFromPeerID(crypto.PeerID(ap.PeerID()))
			if err != nil {
				t.Fatalf("identity hash: %v", err)
			}
			path := "system/peer/transport/" + types.PeerIdentityHashHex(idHash) + "/" + tc.profileID
			h, ok := ap.RawLocationIndex().Get(path)
			if !ok {
				t.Fatalf("no profile bound at %s", path)
			}
			ent, ok := ap.RawContentStore().Get(h)
			if !ok {
				t.Fatalf("profile %s bound but not stored", h)
			}
			if ent.Type != tc.wantType {
				t.Errorf("profile type = %q, want %q", ent.Type, tc.wantType)
			}
			// One decoder for both: the two profiles share the live
			// single-{url} endpoint shape pinned by §6.5.1a D4.
			data, err := types.WebSocketProfileDataFromEntity(ent)
			if err != nil {
				// TCP entities decode through the TCP helper; the field
				// layout is identical, the type check is not.
				tcpData, terr := types.TCPProfileDataFromEntity(ent)
				if terr != nil {
					t.Fatalf("decode profile: ws=%v tcp=%v", err, terr)
				}
				if tcpData.PeerID != ap.PeerID() {
					t.Errorf("profile.peer_id = %q, want our own %q", tcpData.PeerID, ap.PeerID())
				}
				if tcpData.Endpoint.URL != tc.wantURL {
					t.Errorf("profile.endpoint.url = %q, want %q", tcpData.Endpoint.URL, tc.wantURL)
				}
				return
			}
			if data.PeerID != ap.PeerID() {
				t.Errorf("profile.peer_id = %q, want our own %q", data.PeerID, ap.PeerID())
			}
			if data.Endpoint.URL != tc.wantURL {
				t.Errorf("profile.endpoint.url = %q, want %q", data.Endpoint.URL, tc.wantURL)
			}
		})
	}
}

// TestAdvertiseTransport_RejectsUnroutable pins the refusal that
// matters more than the happy path: a durable profile carrying a
// wildcard bind address is worse than no profile, because a consumer
// reading it cannot distinguish a wrong address from an unreachable
// peer (§6.5.1a D1 selection just fails through the candidate list).
// Loopback is allowed on purpose — it is the browser↔Go dev loop.
func TestAdvertiseTransport_RejectsUnroutable(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	for _, tc := range []struct {
		name    string
		dialURL string
		code    string
	}{
		{"wildcard v4", "ws://0.0.0.0:9100/ws", "unroutable_advertisement"},
		{"wildcard v6", "tcp://[::]:9100", "unroutable_advertisement"},
		{"no host", "tcp://:9100", "unroutable_advertisement"},
		{"no scheme", "127.0.0.1:9100", "unsupported_scheme"},
		{"empty", "", "invalid_peer_or_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ap.AdvertiseTransport(tc.dialURL)
			if err == nil {
				t.Fatalf("AdvertiseTransport(%q) = nil, want a refusal", tc.dialURL)
			}
			if tc.code != "invalid_peer_or_url" && !strings.Contains(err.Error(), tc.code) {
				t.Errorf("error %q does not carry code %q", err, tc.code)
			}
		})
	}

	// Loopback is NOT rejected — the dev loop depends on it.
	if err := ap.AdvertiseTransport("ws://127.0.0.1:9100/ws"); err != nil {
		t.Errorf("loopback advertisement refused: %v", err)
	}
}
