package publishedroot_test

import (
	"errors"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk/publishedroot"
)

// The gates, tested where they are reachable.
//
// **This is the package's whole reason to be a package.** These checks
// have two callers — `entitysdk`'s store-side reader and `fetch`'s wire
// consumer — and several of the branches cannot be provoked through
// either one: a real store computes content hashes rather than accepting
// claimed ones, so no store-backed test can serve a published-root whose
// `content_hash` disagrees with its bytes. An unreachable check is an
// unverified check, and this one is the reason the whole chain is not
// self-referential (a host that could alter the payload and restate the
// hash would otherwise be verifying its own claim against itself).
//
// Tier: unit.

type publisher struct {
	kp     crypto.Keypair
	peerID string
}

func newPublisher(t *testing.T) publisher {
	t.Helper()
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	return publisher{kp: kp, peerID: string(kp.PeerID())}
}

// root builds a well-formed published-root entity with its content hash
// computed the way a real store would.
func (p publisher) root(t *testing.T, prefix string) entity.Entity {
	t.Helper()
	d := types.PublishedRootData{
		PeerID:      p.peerID,
		RootHash:    hash.NewSHA256([32]byte{0xAB}),
		Prefix:      prefix,
		Seq:         7,
		PublishedAt: 1787279734361,
	}
	ent, err := d.ToEntity()
	if err != nil {
		t.Fatalf("build published-root: %v", err)
	}
	h, err := hash.Compute(ent.Type, ent.Data)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	ent.ContentHash = h
	return ent
}

// sign builds the §5.2 signature entity over target.
func (p publisher) sign(t *testing.T, target hash.Hash) entity.Entity {
	t.Helper()
	d := types.SignatureData{
		Target:    target,
		Algorithm: "ed25519",
		Signature: p.kp.Sign(target.Bytes()),
	}
	ent, err := d.ToEntity()
	if err != nil {
		t.Fatalf("build signature: %v", err)
	}
	return ent
}

func faultCode(t *testing.T, err error) string {
	t.Helper()
	var f *publishedroot.Fault
	if !errors.As(err, &f) {
		t.Fatalf("error is not a Fault: %v", err)
	}
	return f.Code
}

func TestCheck_AcceptsAWellFormedRoot(t *testing.T) {
	p := newPublisher(t)
	ent, data, err := publishedroot.Check(p.peerID, "wherever", p.root(t, "docs/"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if data.Prefix != "docs/" || data.Seq != 7 {
		t.Errorf("decoded payload is %+v", data)
	}
	if ent.ContentHash.IsZero() {
		t.Error("Check returned an entity with no content hash")
	}
}

func TestCheck_RefusesEveryByteDecidableDefect(t *testing.T) {
	p := newPublisher(t)
	other := newPublisher(t)

	corrupted := p.root(t, "docs/")
	corrupted.ContentHash = hash.NewSHA256([32]byte{0x99}) // a hash for other bytes

	wrongPeer := p.root(t, "docs/")
	wrongPeer = reseal(t, wrongPeer, types.PublishedRootData{
		PeerID: other.peerID, RootHash: hash.NewSHA256([32]byte{0xAB}), Prefix: "docs/",
	})

	noPrefix := reseal(t, p.root(t, "docs/"), types.PublishedRootData{
		PeerID: p.peerID, RootHash: hash.NewSHA256([32]byte{0xAB}),
	})

	badPrefix := reseal(t, p.root(t, "docs/"), types.PublishedRootData{
		PeerID: p.peerID, RootHash: hash.NewSHA256([32]byte{0xAB}), Prefix: "docs",
	})

	wrongType := p.root(t, "docs/")
	wrongType.Type = "system/hash"

	for _, tc := range []struct {
		name, code string
		ent        entity.Entity
		why        string
	}{
		{"wrong type", "unexpected_result_type", wrongType,
			"a host serving something else at the manifest URL"},
		{"claimed hash disagrees with bytes", "content_hash_mismatch", corrupted,
			"the branch no store-backed test can reach, and the one that stops the chain " +
				"from verifying a host's claim against itself"},
		{"payload names another peer", "peer_id_mismatch", wrongPeer,
			"a peer MAY hold other peers' roots; its OWN storage path may only hold its own"},
		{"no prefix", "missing_prefix", noPrefix,
			"§3.3a REQUIRED — without it the relative keys cannot be rebuilt into paths"},
		{"prefix without a trailing slash", "invalid_prefix", badPrefix,
			"§3.3a MUST — concatenation would produce a wrong path, not a missing one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := publishedroot.Check(p.peerID, "wherever", tc.ent)
			if err == nil {
				t.Fatalf("accepted: %s", tc.why)
			}
			if got := faultCode(t, err); got != tc.code {
				t.Errorf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestDeriveKey_RefusesAPeerIDItCannotDeriveFrom(t *testing.T) {
	p := newPublisher(t)
	if _, _, err := publishedroot.DeriveKey(p.peerID); err != nil {
		t.Fatalf("identity-form peer-id did not derive: %v", err)
	}
	// Not a peer-id at all. The point is that the failure is an error
	// and not a zero key: a consumer that carried on with one would be
	// verifying against nothing and reporting success.
	if _, _, err := publishedroot.DeriveKey("not-a-peer-id"); err == nil {
		t.Fatal("derived a key from a non-peer-id")
	} else if code := faultCode(t, err); code != "unverifiable_peer_id" {
		t.Errorf("code = %q, want unverifiable_peer_id", code)
	}
}

func TestVerifySignature_HoldsAndRefuses(t *testing.T) {
	p := newPublisher(t)
	other := newPublisher(t)
	root := p.root(t, "docs/")
	pub, keyType, err := publishedroot.DeriveKey(p.peerID)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}

	if _, err := publishedroot.VerifySignature("sig", root, p.sign(t, root.ContentHash),
		pub, keyType); err != nil {
		t.Fatalf("a genuine signature did not verify: %v", err)
	}

	// Signed by someone else, over the right target. This is the check
	// the whole corridor rests on — the key comes from the peer-id, so
	// a wrong signer cannot be papered over by shipping a key.
	if _, err := publishedroot.VerifySignature("sig", root, other.sign(t, root.ContentHash),
		pub, keyType); err == nil {
		t.Error("another publisher's signature verified")
	} else if code := faultCode(t, err); code != "signature_invalid" {
		t.Errorf("code = %q, want signature_invalid", code)
	}

	// A genuine signature over DIFFERENT bytes. Refused on the target,
	// before the crypto: a host that steers a consumer at a signature it
	// prepared earlier must not get a pass because that signature is
	// itself valid.
	elsewhere := p.sign(t, hash.NewSHA256([32]byte{0x11}))
	if _, err := publishedroot.VerifySignature("sig", root, elsewhere, pub, keyType); err == nil {
		t.Error("a valid signature over other bytes was accepted")
	} else if code := faultCode(t, err); code != "signature_target_mismatch" {
		t.Errorf("code = %q, want signature_target_mismatch", code)
	}

	notASig := root
	if _, err := publishedroot.VerifySignature("sig", root, notASig, pub, keyType); err == nil {
		t.Error("a non-signature entity was accepted as a signature")
	}
}

// TestSignatureRelPath_DerivesFromTheGivenHash pins the one ordering
// rule both consumers depend on: the pointer path comes from the hash
// the consumer RECOMPUTED, never the one the host served.
func TestSignatureRelPath_DerivesFromTheGivenHash(t *testing.T) {
	h := hash.NewSHA256([32]byte{0x42})
	if got, want := publishedroot.SignatureRelPath(h), types.LocalSignaturePath(h); got != want {
		t.Errorf("SignatureRelPath = %q, want %q", got, want)
	}
}

// reseal replaces an entity's payload and recomputes its content hash,
// so a malformed-payload case is not accidentally also a hash-mismatch
// case. Two defects in one fixture would let either check pass for the
// other's reason.
func reseal(t *testing.T, ent entity.Entity, d types.PublishedRootData) entity.Entity {
	t.Helper()
	raw, err := ecf.Encode(d)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	ent.Data = cbor.RawMessage(raw)
	h, err := hash.Compute(ent.Type, ent.Data)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	ent.ContentHash = h
	return ent
}
