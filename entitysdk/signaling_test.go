package entitysdk

import (
	"bytes"
	"context"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/peer"
)

// meetFixture stands up a peer HOSTING a signaling node plus two
// separate client peers connected to it over TCP.
//
// **Three distinct peers, on the wire, on purpose.** A single peer
// offering and collecting against its own node proves the bucket works
// and proves nothing about a meeting — it is one side of the contract
// asserted by itself, which is the shape this repo just promoted to D22.
// The whole claim here is that two parties who share only a label find
// each other, so the test has to have two parties.
func meetFixture(t *testing.T) (node, a, b *AppPeer) {
	t.Helper()
	node, err := CreatePeer(PeerConfig{
		ListenAddr: "127.0.0.1:0",
		Extensions: ExtensionsConfig{SignalingNode: &SignalingNodeConfig{}},
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	})
	if err != nil {
		t.Fatalf("CreatePeer(node): %v", err)
	}
	t.Cleanup(func() { node.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ready := make(chan struct{})
	listenErr := make(chan error, 1)
	go func() { listenErr <- node.ListenReady(ctx, ready) }()
	select {
	case <-ready:
	case err := <-listenErr:
		t.Fatalf("node listen: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("node not ready in 2s")
	}

	dial := func(name string) *AppPeer {
		p, err := CreatePeer(PeerConfig{})
		if err != nil {
			t.Fatalf("CreatePeer(%s): %v", name, err)
		}
		t.Cleanup(func() { p.Close() })
		conn, err := p.Connect(ctx, node.Addr().String())
		if err != nil {
			t.Fatalf("%s.Connect: %v", name, err)
		}
		t.Cleanup(func() { conn.Close() })
		return p
	}
	return node, dial("alice"), dial("bob")
}

// TestSignaling_TwoPeersMeetAtATag is the carrier gate: two parties
// that share nothing but a label find each other's blob at the same
// rendezvous key.
//
// The carrier is all this proves, and the boundary is the point.
// SIGNALING §1.2 — *"the key introduces; it never authorizes."* Standing
// at a tag says nothing about who you are; the candidate surfacing and
// the admission decision are DISCOVERY's, and the `rendezvous` backend
// that does them is blocked on a spec fold (see signaling.go's header).
//
// Tier: integration (real handler, real dispatch, real key derivation).
func TestSignaling_TwoPeersMeetAtATag(t *testing.T) {
	node, alice, bob := meetFixture(t)
	ctx := context.Background()

	key, err := TagKey("entity-church-demo")
	if err != nil {
		t.Fatalf("TagKey: %v", err)
	}
	if len(key) != 33 {
		t.Fatalf("rendezvous key is %d bytes, want 33 (algorithm‖digest at the SHA-256 floor)", len(key))
	}

	// Both arms target the SAME node — §3.4's same-provider MUST. Each
	// dispatches over its own pooled connection; neither shares state
	// with the other or with the node beyond the bucket.
	aliceSC := alice.Signaling(node.PeerID())
	bobSC := bob.Signaling(node.PeerID())

	// Nobody is standing here yet — and that is an EMPTY LIST and a
	// 200, never a 404 (§4.4, §5 pin 1). A consumer polls an empty
	// mailbox; it does not report the node as broken.
	msgs, err := aliceSC.Collect(ctx, key)
	if err != nil {
		t.Fatalf("Collect on an untouched key errored (%v); an unknown key is empty and 200, "+
			"because a mailbox nobody wrote to and one that does not exist are the same state", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("Collect on an untouched key returned %d messages", len(msgs))
	}

	aliceBlob := []byte("alice-was-here")
	bobBlob := []byte("bob-was-here")
	if err := aliceSC.Offer(ctx, key, aliceBlob); err != nil {
		t.Fatalf("Offer(alice): %v", err)
	}
	if err := bobSC.Offer(ctx, key, bobBlob); err != nil {
		t.Fatalf("Offer(bob): %v", err)
	}

	// The meeting: each arm reads the OTHER's blob out of a bucket it
	// reached by deriving the same key from the same label.
	msgs, err = bobSC.Collect(ctx, key)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("collected %d messages, want 2", len(msgs))
	}
	// Oldest-first (§5 pin 4).
	if !bytes.Equal(msgs[0], aliceBlob) || !bytes.Equal(msgs[1], bobBlob) {
		t.Errorf("bucket order is not arrival order: got %q, %q", msgs[0], msgs[1])
	}

	// Collect is NON-destructive: the second reader sees the same
	// bucket. A destructive read would make the first arm to poll
	// silently consume the second arm's introduction.
	again, err := aliceSC.Collect(ctx, key)
	if err != nil {
		t.Fatalf("Collect (second): %v", err)
	}
	if len(again) != 2 {
		t.Errorf("second Collect saw %d messages, want 2 — collect is non-destructive", len(again))
	}

	// Re-offering identical bytes is idempotent by content hash (§5 pin
	// 1), which is what makes a retry after a timeout safe.
	if err := aliceSC.Offer(ctx, key, aliceBlob); err != nil {
		t.Fatalf("Offer(alice, again): %v", err)
	}
	if dedup, err := aliceSC.Collect(ctx, key); err != nil {
		t.Fatalf("Collect (after re-offer): %v", err)
	} else if len(dedup) != 2 {
		t.Errorf("a duplicate offer added a blob (%d in bucket); §5 pin 1 dedups by content hash", len(dedup))
	}
}

// TestRendezvousKey_TheSilentNeverMeetCases pins the derivation
// properties that have no error path.
//
// Every failure here is invisible at runtime: two peers derive
// different keys, deposit into different buckets, and each sees an
// empty mailbox forever. There is no error to log and no status to
// check — which is exactly why the properties are asserted directly
// rather than through a meeting that "works".
//
// Tier: contract pin.
func TestRendezvousKey_TheSilentNeverMeetCases(t *testing.T) {
	tag, err := TagKey("demo")
	if err != nil {
		t.Fatalf("TagKey: %v", err)
	}
	secret, err := SecretKey("demo")
	if err != nil {
		t.Fatalf("SecretKey: %v", err)
	}
	// Same input string, different MODE — the mode is domain-separated
	// into the payload, so these are different meeting points. Two
	// arms that disagree about the mode never meet.
	if bytes.Equal(tag, secret) {
		t.Error("tag and secret over the same input derived the same key; the mode is supposed to " +
			"be domain-separated into the payload")
	}

	// The lobby default has to be a NAMED constant, and ours has to be
	// the kernel's — "per deployment" with no default is the same
	// silent never-meet one layer up.
	lobby, err := LobbyKey(LobbyDefault)
	if err != nil {
		t.Fatalf("LobbyKey: %v", err)
	}
	if len(lobby) != 33 {
		t.Errorf("lobby key is %d bytes, want 33", len(lobby))
	}

	// Pair mode is symmetric: either peer computes the same key from
	// its own point of view, so neither has to know who "goes first".
	x, err := PairKey("2KAlice", "2KBob")
	if err != nil {
		t.Fatalf("PairKey: %v", err)
	}
	y, err := PairKey("2KBob", "2KAlice")
	if err != nil {
		t.Fatalf("PairKey (reversed): %v", err)
	}
	if !bytes.Equal(x, y) {
		t.Error("PairKey is not symmetric in its arguments; each peer would derive the other's " +
			"key and neither would ever find the other")
	}

	// Determinism across calls — the property a poll loop depends on.
	if again, err := TagKey("demo"); err != nil {
		t.Fatalf("TagKey (again): %v", err)
	} else if !bytes.Equal(tag, again) {
		t.Error("TagKey is not deterministic")
	}
}

// TestSignaling_TheNodeIsPartOfTheAddress pins §3.4's same-provider
// MUST at the surface a caller can actually get wrong.
//
// Two peers on different nodes derive the SAME key and still never
// meet, with no error on either side — the failure is a property of the
// pair, and neither half can see it alone. So the client carries the
// node it targets and says so.
//
// Tier: contract pin.
func TestSignaling_TheNodeIsPartOfTheAddress(t *testing.T) {
	node, alice, _ := meetFixture(t)
	other, err := CreatePeer(PeerConfig{Extensions: ExtensionsConfig{SignalingNode: &SignalingNodeConfig{}}})
	if err != nil {
		t.Fatalf("CreatePeer(other): %v", err)
	}
	defer other.Close()

	ctx := context.Background()
	key, err := TagKey("same-tag-different-node")
	if err != nil {
		t.Fatalf("TagKey: %v", err)
	}

	if err := alice.Signaling(node.PeerID()).Offer(ctx, key, []byte("here")); err != nil {
		t.Fatalf("Offer at node A: %v", err)
	}
	msgs, err := other.Signaling(other.PeerID()).Collect(ctx, key)
	if err != nil {
		t.Fatalf("Collect at node B: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("a blob offered at node A was collected at node B (%d messages); buckets are "+
			"per-node and this test's whole premise is wrong", len(msgs))
	}
	// And the client says which node it is talking to, because that is
	// the only diagnostic either arm has for a meeting that never
	// happens.
	if got := alice.Signaling(node.PeerID()).NodePeerID(); got != node.PeerID() {
		t.Errorf("NodePeerID() = %q, want %q", got, node.PeerID())
	}
}

// TestSignaling_NodeIsOptIn pins the one default in this surface that
// is not like its neighbours: a peer does not run a rendezvous node
// unless asked. Every other extension in ExtensionsConfig is a
// capability the peer HAS; this one is a service it runs for other
// people, holding their blobs.
//
// Tier: contract pin.
func TestSignaling_NodeIsOptIn(t *testing.T) {
	plain, err := CreatePeer(PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer plain.Close()

	key, err := TagKey("nobody-home")
	if err != nil {
		t.Fatalf("TagKey: %v", err)
	}
	if _, err := plain.Signaling(plain.PeerID()).Collect(context.Background(), key); err == nil {
		t.Error("a peer with no SignalingNode config answered a signaling op; hosting a mailbox " +
			"for other peers must be an explicit act")
	}
}
