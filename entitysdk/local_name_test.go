package entitysdk

import (
	"testing"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/hash"
)

// local_name_test.go — the name book's upkeep ops, end to end against a
// real registry-wired peer.
//
// Tier: integration (TESTING-STRATEGY.md) — real handler, real store, real
// dispatch. Not a unit test with a faked executor: the whole point of these
// three wrappers is that the kernel's handler answers them, and a fake would
// pass whether or not `update-transports` is even a declared operation.

// resolverPeer stands up a single peer with the registry substrate wired and
// the local-name backend active in the chain. It is the minimum a user needs
// for `name` to do anything, which makes it the right fixture: if this setup
// is more than two calls, the shell verb inherits that.
func resolverPeer(t *testing.T) *AppPeer {
	t.Helper()
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := CreatePeer(PeerConfig{
		Keypair:    &kp,
		Extensions: ExtensionsConfig{Registry: &RegistryConfig{}},
	})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { ap.Close() })
	if err := ap.EnableLocalNameResolver(); err != nil {
		t.Fatalf("EnableLocalNameResolver: %v", err)
	}
	return ap
}

// findBinding returns the named binding from a list, or fails.
func findBinding(t *testing.T, book []LocalNameBinding, name string) LocalNameBinding {
	t.Helper()
	for _, b := range book {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("binding %q absent from name book %+v", name, book)
	return LocalNameBinding{}
}

// TestLocalName_BookLifecycle walks the full upkeep cycle: an empty book,
// a bind with notes, list, re-point the transports, and unbind back to
// empty. Each assertion is on what a USER would see through the `name`
// verb, because that is the surface these wrappers exist to serve.
func TestLocalName_BookLifecycle(t *testing.T) {
	ap := resolverPeer(t)
	target := "z6MkTargetPeerIdentifierForTest"

	// An empty book is a value, not an error — the state every peer starts
	// in, and the one a name list must be able to render.
	book, err := ap.ListLocalNames()
	if err != nil {
		t.Fatalf("ListLocalNames on a fresh peer: %v", err)
	}
	if len(book) != 0 {
		t.Fatalf("fresh peer's name book is not empty: %+v", book)
	}

	bindHash, err := ap.BindLocalName("bills-lab", target, nil, WithNotes("the lab box in the basement"))
	if err != nil {
		t.Fatalf("BindLocalName: %v", err)
	}

	book, err = ap.ListLocalNames()
	if err != nil {
		t.Fatalf("ListLocalNames after bind: %v", err)
	}
	if len(book) != 1 {
		t.Fatalf("want 1 binding after one bind, got %d: %+v", len(book), book)
	}
	b := findBinding(t, book, "bills-lab")
	if b.TargetPeerID != target {
		t.Errorf("target_peer_id = %q, want %q", b.TargetPeerID, target)
	}
	// Notes are the field that makes a stale name book diagnosable, and the
	// one most likely to be dropped by a wrapper that only maps the fields
	// it happened to need. Pinned explicitly.
	if b.Notes != "the lab box in the basement" {
		t.Errorf("notes = %q, want the note passed to WithNotes — it did not survive bind→list", b.Notes)
	}
	if b.Hash != bindHash {
		t.Errorf("listed hash %s != hash returned by bind %s", b.Hash, bindHash)
	}

	// Re-point the transports. The name→peer_id association must be
	// untouched and the hash MUST move: the kernel issues a successor
	// carrying supersedes, and a hash that did not change would mean the
	// update silently did nothing.
	newHash, err := ap.UpdateLocalNameTransports("bills-lab", []hash.Hash{})
	if err != nil {
		t.Fatalf("UpdateLocalNameTransports: %v", err)
	}
	book, err = ap.ListLocalNames()
	if err != nil {
		t.Fatalf("ListLocalNames after update: %v", err)
	}
	b = findBinding(t, book, "bills-lab")
	if b.TargetPeerID != target {
		t.Errorf("update-transports moved target_peer_id to %q; it must re-point reach, not identity", b.TargetPeerID)
	}
	if b.Hash != newHash {
		t.Errorf("listed hash %s != successor hash %s returned by update-transports", b.Hash, newHash)
	}

	// The name resolves for as long as it is bound...
	res, err := ap.ResolveName("bills-lab")
	if err != nil {
		t.Fatalf("ResolveName while bound: %v", err)
	}
	if res.PeerID != target {
		t.Errorf("resolved peer_id = %q, want %q", res.PeerID, target)
	}

	// ...and fails closed at the name rung once it is not.
	if err := ap.UnbindLocalName("bills-lab"); err != nil {
		t.Fatalf("UnbindLocalName: %v", err)
	}
	book, err = ap.ListLocalNames()
	if err != nil {
		t.Fatalf("ListLocalNames after unbind: %v", err)
	}
	if len(book) != 0 {
		t.Fatalf("name book not empty after unbinding its only entry: %+v", book)
	}
	if _, err := ap.ResolveName("bills-lab"); err == nil {
		t.Fatal("ResolveName succeeded after unbind — the binding outlived its removal")
	} else if o := AsOutcome(err); o == nil || o.Kind != OutcomeNotFound || o.Rung != RungName {
		t.Errorf("post-unbind resolve gave %v; want a typed Outcome{not_found, name}", err)
	}
}

// TestLocalName_UnbindUnknownNameIsIdempotent pins the kernel's answer to
// a question §6.5 does not settle: unbinding a name that was never bound
// SUCCEEDS. `Handler.Unbind` is a TreeRemove of the pointer path, and
// TreeRemove no-ops on an absent path.
//
// This pin exists in the direction that will catch a change. If core-go
// later decides absent-unbind is a 404 — a defensible reading, and the one
// this test originally assumed — that is a cross-impl behavior change and
// the SDK doc comment plus the `name unbind` message both become wrong. A
// green suite should not be the thing that hides it.
func TestLocalName_UnbindUnknownNameIsIdempotent(t *testing.T) {
	ap := resolverPeer(t)
	if err := ap.UnbindLocalName("never-bound"); err != nil {
		t.Fatalf("unbind of an unbound name reported %v (status %d); the kernel's "+
			"TreeRemove-based unbind was idempotent when this was written — if that "+
			"changed deliberately, UnbindLocalName's doc comment and the `name unbind` "+
			"verb's not-found message both need updating with it", err, StatusOf(err))
	}
}

// TestLocalName_EmptyNameRefusedBeforeDispatch checks the two guards that
// keep a malformed request off the wire. Cheap, and they are the reason a
// `name unbind` with a missing argument reads as a usage error rather than
// a handler error.
func TestLocalName_EmptyNameRefusedBeforeDispatch(t *testing.T) {
	ap := resolverPeer(t)
	if err := ap.UnbindLocalName(""); err == nil {
		t.Error("UnbindLocalName(\"\") was accepted")
	} else if StatusOf(err) != 400 {
		t.Errorf("UnbindLocalName(\"\") status = %d, want 400", StatusOf(err))
	}
	if _, err := ap.UpdateLocalNameTransports("", nil); err == nil {
		t.Error("UpdateLocalNameTransports(\"\") was accepted")
	} else if StatusOf(err) != 400 {
		t.Errorf("UpdateLocalNameTransports(\"\") status = %d, want 400", StatusOf(err))
	}
}
