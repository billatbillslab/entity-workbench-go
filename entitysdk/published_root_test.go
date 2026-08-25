package entitysdk

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/tree"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/publishedroot"
)

// Tier: Go unit + two-peer integration (`make test-sdk`).
//
// The consumer-side published-root reader — N3 of
// REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17, and step 3a of
// its §5.1, which gates the target-prefix work on the sync surface.
//
// **The publisher in these tests is core-go's real one.**
// `ext/publishedroot.Publisher` is what an entity peer actually runs;
// we drive it directly rather than hand-minting the bytes we expect.
// That is deliberate and it is the load-bearing choice in this file:
// a reader tested against a writer we wrote ourselves proves only that
// we are self-consistent, and the failure mode it misses — our idea of
// the storage path, the signature-pointer path, the signed message, or
// the prefix convention drifting from core-go's — is exactly the class
// of bug this reader exists to not have. If core-go moves
// `PublishedRootStoragePath` or `LocalSignaturePath`, or changes what
// the signature covers, TestReadPublishedRoot_RemoteOverTheWire goes
// red. That is the pin.
//
// The hand-built roots below (forgery, missing signature, bad prefix,
// peer-id mismatch) are the negative half. They cannot come from the
// real publisher, because the real publisher does not emit them — the
// point of each is that a *hostile or broken* publisher can, and the
// reader has to refuse.

// startPublisher wires core-go's real published-root publisher onto an
// AppPeer's own store and location index, returning it so a test can
// publish more than once against a single monotonic seq counter.
func startPublisher(t *testing.T, p *AppPeer) *publishedroot.Publisher {
	t.Helper()
	return startPublisherFor(t, p, publishedroot.PrefixForLocalPeer)
}

// startPublisherFor is startPublisher for a publisher that declares a
// prefix other than the default peer-relative "system/" — the shape a
// peer takes when it publishes a specific shared subtree.
func startPublisherFor(t *testing.T, p *AppPeer, prefix string) *publishedroot.Publisher {
	t.Helper()
	tracker := tree.NewRootTracker(p.RawContentStore(), p.PeerID(), nil)
	pub := publishedroot.NewPublisher(
		p.RawContentStore(), tracker, prefix, nil,
		publishedroot.WithDebounce(0))
	if err := pub.SetupAuthority(
		p.RawLocationIndex(), p.RawPeer().Keypair(), p.RawPeer().Identity(), false,
	); err != nil {
		t.Fatalf("publisher SetupAuthority: %v", err)
	}
	return pub
}

// someRoot returns a real content hash to stand in as a tree root. Any
// hash works — the reader verifies the signature over the
// published-root entity, not the reachability of what it points at.
func someRoot(t *testing.T, p *AppPeer, name string) hash.Hash {
	t.Helper()
	h, err := p.Put("published-root-test/"+name, "test/scalar", name)
	if err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return h
}

// bindRoot writes an arbitrary published-root payload into host's tree
// at host's own canonical storage path, optionally binding a signature
// produced by signer. This is the hostile-publisher rig: every negative
// test differs from a legitimate publish in exactly one field.
func bindRoot(t *testing.T, host *AppPeer, data types.PublishedRootData, signer *crypto.Keypair) entity.Entity {
	t.Helper()
	ent, err := data.ToEntity()
	if err != nil {
		t.Fatalf("encode published-root: %v", err)
	}
	if _, err := host.RawContentStore().Put(ent); err != nil {
		t.Fatalf("store published-root: %v", err)
	}
	if signer != nil {
		sigEnt, err := types.SignatureData{
			Target:    ent.ContentHash,
			Signer:    host.RawPeer().Identity().ContentHash,
			Algorithm: crypto.KeyTypeString(signer.KeyType),
			Signature: signer.Sign(ent.ContentHash.Bytes()),
		}.ToEntity()
		if err != nil {
			t.Fatalf("encode signature: %v", err)
		}
		if _, err := host.RawContentStore().Put(sigEnt); err != nil {
			t.Fatalf("store signature: %v", err)
		}
		if err := host.RawLocationIndex().Set(
			types.LocalSignaturePath(ent.ContentHash), sigEnt.ContentHash,
		); err != nil {
			t.Fatalf("bind signature: %v", err)
		}
	}
	if err := host.RawLocationIndex().Set(
		types.PublishedRootStoragePath(), ent.ContentHash,
	); err != nil {
		t.Fatalf("bind published-root: %v", err)
	}
	return ent
}

// wantSDKError asserts err is an *Error with the given code.
func wantSDKError(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %q, got nil", code)
	}
	if got := CodeOf(err); got != code {
		t.Fatalf("error code = %q, want %q (err: %v)", got, code, err)
	}
}

// TestReadPublishedRoot_RemoteOverTheWire is the primary result: one
// peer publishes with core-go's publisher, another peer reads and
// verifies it across a real connection. Everything the sync surface
// needs — the declared prefix and the signed root hash — comes back
// authenticated.
func TestReadPublishedRoot_RemoteOverTheWire(t *testing.T) {
	client, server, addr := twoPeers(t)

	root := someRoot(t, server, "over-the-wire")
	pub := startPublisher(t, server)
	published, err := pub.Publish(root)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := client.Connect(ctx, addr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	pr, err := client.ReadPublishedRoot(ctx, server.PeerID())
	if err != nil {
		t.Fatalf("ReadPublishedRoot: %v", err)
	}

	if pr.Data.PeerID != server.PeerID() {
		t.Errorf("peer_id = %q, want %q", pr.Data.PeerID, server.PeerID())
	}
	if pr.Data.RootHash != root {
		t.Errorf("root_hash = %s, want %s", pr.Data.RootHash, root)
	}
	// core-go publishes the peer-relative subtree, so the declared
	// prefix is "system/" — §3.3's first table row. This is the value
	// arch's amendment makes the sync surface's target prefix, which
	// is why it is asserted literally and not just for shape.
	if pr.Data.Prefix != publishedroot.PrefixForLocalPeer {
		t.Errorf("prefix = %q, want %q", pr.Data.Prefix, publishedroot.PrefixForLocalPeer)
	}
	if !strings.HasSuffix(pr.Data.Prefix, "/") {
		t.Errorf("prefix %q does not end in / (EXTENSION-TREE §3.3a MUST)", pr.Data.Prefix)
	}
	if pr.Entity.ContentHash != published.ContentHash {
		t.Errorf("content hash = %s, want %s", pr.Entity.ContentHash, published.ContentHash)
	}
	if pr.Signature.Target != published.ContentHash {
		t.Errorf("signature target = %s, want %s", pr.Signature.Target, published.ContentHash)
	}

	// The freshness floor advanced to what we accepted.
	if seq, ok := client.PublishedRootSeqFloor(server.PeerID()); !ok || seq != pr.Data.Seq {
		t.Errorf("seq floor = (%d,%v), want (%d,true)", seq, ok, pr.Data.Seq)
	}
	t.Logf("verified published-root from %s: seq=%d prefix=%q root=%s",
		pr.Data.PeerID, pr.Data.Seq, pr.Data.Prefix, pr.Data.RootHash)
}

// TestReadPublishedRoot_LocalNamespace: the same read against a
// namespace already in our own tree. This is the mirror case — a
// consumer that has synced /{them}/… reads the published-root from
// local storage with no connection — and it must take the identical
// path, which it does because the read is peer-qualified either way.
func TestReadPublishedRoot_LocalNamespace(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	root := someRoot(t, p, "local")
	pub := startPublisher(t, p)
	if _, err := pub.Publish(root); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	pr, err := p.ReadPublishedRoot(context.Background(), p.PeerID())
	if err != nil {
		t.Fatalf("ReadPublishedRoot: %v", err)
	}
	if pr.Data.RootHash != root {
		t.Errorf("root_hash = %s, want %s", pr.Data.RootHash, root)
	}
}

// TestReadPublishedRoot_NoneBound: a peer that has never published.
// 404 with a distinguishable code, not a zero value the caller might
// mistake for an empty root.
func TestReadPublishedRoot_NoneBound(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	_, err = p.ReadPublishedRoot(context.Background(), p.PeerID())
	wantSDKError(t, err, "no_published_root")
	if StatusOf(err) != 404 {
		t.Errorf("status = %d, want 404", StatusOf(err))
	}
}

// TestReadPublishedRoot_ForgedSignature: the payload is well-formed
// and claims the host's own peer-id, and there IS a signature bound at
// the invariant pointer — it is simply not the publisher's. This is
// the attack the whole entity exists to defend against (§1.1 binding
// fabrication), and a reader that only checked "a signature is
// present" would pass it.
func TestReadPublishedRoot_ForgedSignature(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	attacker, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	bindRoot(t, p, types.PublishedRootData{
		PeerID:   p.PeerID(),
		RootHash: someRoot(t, p, "forged"),
		Prefix:   "system/",
		Seq:      1,
	}, &attacker)

	_, err = p.ReadPublishedRoot(context.Background(), p.PeerID())
	wantSDKError(t, err, "signature_invalid")
}

// TestReadPublishedRoot_MissingSignature: a root bound with no
// signature at all is refused, not returned as unverified. This is the
// assertion behind the API having no `Verified` field.
func TestReadPublishedRoot_MissingSignature(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	bindRoot(t, p, types.PublishedRootData{
		PeerID:   p.PeerID(),
		RootHash: someRoot(t, p, "unsigned"),
		Prefix:   "system/",
		Seq:      1,
	}, nil)

	_, err = p.ReadPublishedRoot(context.Background(), p.PeerID())
	wantSDKError(t, err, "missing_signature")
}

// TestReadPublishedRoot_PrefixDiscipline: `prefix` absent, and
// `prefix` present but without its trailing slash. Both are refused
// rather than defaulted. EXTENSION-TREE §3.3a made the field REQUIRED
// precisely because any default silently promotes one impl's
// convention, and a prefix missing its slash concatenates into a wrong
// absolute path instead of failing.
func TestReadPublishedRoot_PrefixDiscipline(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, code string
	}{
		{"absent", "", "missing_prefix"},
		{"no trailing slash", "system", "invalid_prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewAppPeer()
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			kp := p.RawPeer().Keypair()
			bindRoot(t, p, types.PublishedRootData{
				PeerID:   p.PeerID(),
				RootHash: someRoot(t, p, "prefix"),
				Prefix:   tc.prefix,
				Seq:      1,
			}, &kp)

			_, err = p.ReadPublishedRoot(context.Background(), p.PeerID())
			wantSDKError(t, err, tc.code)
		})
	}
}

// TestReadPublishedRoot_PeerIDMismatch: a correctly-signed root whose
// payload names a different peer, served from this peer's own storage
// path. Holding another peer's root is legitimate; serving it as your
// own is not, and the reader is asked for a specific peer's root.
func TestReadPublishedRoot_PeerIDMismatch(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	other, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	kp := p.RawPeer().Keypair()
	bindRoot(t, p, types.PublishedRootData{
		PeerID:   other.PeerID(),
		RootHash: someRoot(t, p, "mismatch"),
		Prefix:   "system/",
		Seq:      1,
	}, &kp)

	_, err = p.ReadPublishedRoot(context.Background(), p.PeerID())
	wantSDKError(t, err, "peer_id_mismatch")
}

// TestReadPublishedRoot_SeqRollbackRejected: publish twice, read the
// newer, then re-bind the older at the storage path. The older root is
// perfectly signed — rollback is an attack made entirely out of
// authentic messages, which is why the seq floor exists and why
// signature verification alone is not sufficient.
func TestReadPublishedRoot_SeqRollbackRejected(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	pub := startPublisher(t, p)
	oldEnt, err := pub.Publish(someRoot(t, p, "rollback-1"))
	if err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	if _, err := pub.Publish(someRoot(t, p, "rollback-2")); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}

	newer, err := p.ReadPublishedRoot(context.Background(), p.PeerID())
	if err != nil {
		t.Fatalf("ReadPublishedRoot (newer): %v", err)
	}
	if newer.Data.Seq < 2 {
		t.Fatalf("expected the second publish to be visible, got seq %d", newer.Data.Seq)
	}

	// Roll the binding back to the first published-root.
	if err := p.RawLocationIndex().Set(
		types.PublishedRootStoragePath(), oldEnt.ContentHash,
	); err != nil {
		t.Fatalf("rebind older root: %v", err)
	}

	_, err = p.ReadPublishedRoot(context.Background(), p.PeerID())
	wantSDKError(t, err, "stale_published_root")

	// The floor did not regress on the rejected read.
	if seq, ok := p.PublishedRootSeqFloor(p.PeerID()); !ok || seq != newer.Data.Seq {
		t.Errorf("seq floor = (%d,%v), want (%d,true)", seq, ok, newer.Data.Seq)
	}
}

// TestReadPublishedRoot_CallerSeqFloor: a caller-persisted floor above
// the peer's in-memory one is honored. This is the restart case — the
// in-memory floor is empty, and without the caller's value a rollback
// to any earlier seq would be accepted.
func TestReadPublishedRoot_CallerSeqFloor(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	pub := startPublisher(t, p)
	if _, err := pub.Publish(someRoot(t, p, "floor")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if _, err := p.ReadPublishedRoot(context.Background(), p.PeerID(), WithSeqFloor(99)); err == nil {
		t.Fatal("want stale_published_root with a floor of 99")
	} else {
		wantSDKError(t, err, "stale_published_root")
	}

	// The same read without the caller floor succeeds — proving the
	// rejection came from the supplied floor and not from the payload.
	if _, err := p.ReadPublishedRoot(context.Background(), p.PeerID()); err != nil {
		t.Fatalf("ReadPublishedRoot without caller floor: %v", err)
	}
}

// TestReadPublishedRoot_InvalidPeerID: garbage in fails at the input
// gate rather than becoming a store path. `NamespacedIndex.canonicalize`
// panics on a first segment that is not a peer-id, so this check is
// load-bearing and not merely tidy.
func TestReadPublishedRoot_InvalidPeerID(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, id := range []string{"", "not-a-peer-id", "@alias"} {
		if _, err := p.ReadPublishedRoot(context.Background(), id); err == nil {
			t.Errorf("peerID %q: want error, got nil", id)
		} else if CodeOf(err) != "invalid_peer" {
			t.Errorf("peerID %q: code = %q, want invalid_peer", id, CodeOf(err))
		}
	}
}

// TestCheckPublishedRootEntity_ContentHashMismatch: bytes that do not
// hash to the claimed hash are refused.
//
// This one is a direct unit test rather than a served read, and the
// reason is worth stating: a real store computes content hashes, so no
// peer in this repo can produce the condition. It takes a hostile or
// broken *remote* — which is precisely the party this check exists
// for. Leaving it untested because it is awkward to stage would leave
// the one branch that matters against a lying host unverified.
func TestCheckPublishedRootEntity_ContentHashMismatch(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ent, err := types.PublishedRootData{
		PeerID:   p.PeerID(),
		RootHash: someRoot(t, p, "corrupt"),
		Prefix:   "system/",
		Seq:      1,
	}.ToEntity()
	if err != nil {
		t.Fatal(err)
	}

	// Flip one bit of the claimed hash — the shape of a host that
	// serves one payload and names another.
	tampered := ent
	tampered.ContentHash.Digest[0] ^= 0xFF

	if _, _, err := checkPublishedRootEntity(p.PeerID(), "test/path", tampered); err == nil {
		t.Fatal("want content_hash_mismatch, got nil")
	} else {
		wantSDKError(t, err, "content_hash_mismatch")
	}

	// The untampered entity passes the same function, so the failure
	// above is the hash check and not something else in the path.
	if _, _, err := checkPublishedRootEntity(p.PeerID(), "test/path", ent); err != nil {
		t.Fatalf("untampered entity rejected: %v", err)
	}
}

// TestCheckPublishedRootEntity_WrongType: the storage path holding
// something that is not a published-root at all.
func TestCheckPublishedRootEntity_WrongType(t *testing.T) {
	ent := entity.Entity{Type: "test/scalar"}
	if _, _, err := checkPublishedRootEntity("irrelevant", "test/path", ent); err == nil {
		t.Fatal("want unexpected_result_type, got nil")
	} else {
		wantSDKError(t, err, "unexpected_result_type")
	}
}
