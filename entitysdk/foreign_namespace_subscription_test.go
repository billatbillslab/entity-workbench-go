package entitysdk

import (
	"errors"
	"testing"
	"time"
)

// Tier: Go unit (`make test-sdk`).
//
// The foreign-namespace subscription gate — `entity-browser-rust`'s
// §8 dependency, measured on the Go arm.
//
// What it pins: a prefix subscription on ANOTHER peer's namespace,
// held in OUR local tree, delivers events. This is the mechanism the
// multi-peer mirror design rests on — V7 §1.4's cached-remote model
// puts a copy of Bob's data at `/{bob}/…` in Alice's tree, and a
// consumer of that mirror has to be able to subscribe it.
//
// Why it needed a test rather than a reading. We told browser-rust
// the Go arm supported this "by construction": `parsePattern`
// (watch.go) canonicalizes only RELATIVE patterns — `if
// !strings.HasPrefix(abs, "/") { abs = "/" + localPeerID + "/" + abs
// }` — and passes an absolute one through untouched, and
// `Store.OnPrefixChange` assumes the local peer-id only in its
// empty-prefix branch. That reasoning is correct and it is not a
// measurement; the whole point of the exchange that produced this
// file is that an argument from source is not a result. This is the
// result.
//
// The negative half is the load-bearing half. A watcher that fires on
// everything would satisfy the positive assertion while proving
// nothing, so each test asserts BOTH that the intended watcher fired
// and that the other one did not. That is what shows the two
// namespaces are actually separate rather than incidentally
// overlapping.

// TestForeignNamespaceSubscription_AbsolutePrefixDelivers: a write
// into a remote peer's namespace, in our own tree, reaches a watcher
// registered on that absolute prefix — and does NOT reach a watcher
// on the same-looking relative prefix, which resolves to our own
// namespace.
func TestForeignNamespaceSubscription_AbsolutePrefixDelivers(t *testing.T) {
	alice, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()

	// A second peer purely for a structurally valid peer-id: the
	// store's canonicalize PANICS on a path whose first segment is
	// not a real peer-id, so a made-up string would not test this.
	bob, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	st := alice.Store()
	foreignPrefix := "/" + bob.PeerID() + "/app/share/"

	foreign := make(chan ChangeEvent, 8)
	cancelForeign := st.OnPrefixChange(foreignPrefix, func(ev ChangeEvent) {
		foreign <- ev
	})
	defer cancelForeign()

	// The control watcher. "app/share/" is the SAME suffix, relative
	// — parsePattern resolves it to "/{alice}/app/share/*", a
	// different namespace. It must stay silent.
	local := make(chan ChangeEvent, 8)
	cancelLocal := st.OnPrefixChange("app/share/", func(ev ChangeEvent) {
		local <- ev
	})
	defer cancelLocal()

	// Write Bob's entity into Bob's namespace in Alice's tree. V7
	// §1.4 authority layer 1: "A peer has full write access to its
	// entire local tree. It can write to any path ... Nothing in the
	// protocol prevents this."
	foreignPath := foreignPrefix + "manifest-1"
	if _, err := st.Put(foreignPath, "app/share/manifest", "bob's share"); err != nil {
		t.Fatalf("put into foreign namespace %s: %v", foreignPath, err)
	}

	select {
	case ev := <-foreign:
		if ev.Path != foreignPath {
			t.Errorf("foreign watcher path = %q, want %q", ev.Path, foreignPath)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no event on foreign-namespace watcher for %s within 2s "+
			"(prefix=%s) — the subscription surface does NOT support "+
			"foreign-namespace prefixes", foreignPath, foreignPrefix)
	}

	// The negative: a local-namespace watcher must not see a write
	// that landed in another peer's namespace.
	select {
	case ev := <-local:
		t.Errorf("relative-prefix watcher fired on a foreign-namespace write: %q "+
			"— the two namespaces are not separated", ev.Path)
	default:
	}
}

// TestForeignNamespaceSubscription_LocalWriteStaysLocal is the same
// separation from the other side: our own write under the same
// suffix must reach the relative watcher and not the foreign one.
// Without this direction, a foreign watcher that silently matched
// everything would still pass the test above.
func TestForeignNamespaceSubscription_LocalWriteStaysLocal(t *testing.T) {
	alice, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()

	bob, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	st := alice.Store()

	foreign := make(chan ChangeEvent, 8)
	cancelForeign := st.OnPrefixChange("/"+bob.PeerID()+"/app/share/", func(ev ChangeEvent) {
		foreign <- ev
	})
	defer cancelForeign()

	local := make(chan ChangeEvent, 8)
	cancelLocal := st.OnPrefixChange("app/share/", func(ev ChangeEvent) {
		local <- ev
	})
	defer cancelLocal()

	if _, err := st.Put("app/share/mine-1", "app/share/manifest", "alice's share"); err != nil {
		t.Fatalf("put into local namespace: %v", err)
	}

	wantPath := "/" + alice.PeerID() + "/app/share/mine-1"
	select {
	case ev := <-local:
		if ev.Path != wantPath {
			t.Errorf("local watcher path = %q, want %q", ev.Path, wantPath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event on local-namespace watcher within 2s")
	}

	select {
	case ev := <-foreign:
		t.Errorf("foreign-namespace watcher fired on a local write: %q "+
			"— the prefix match is not namespace-scoped", ev.Path)
	default:
	}
}

// TestSubscribeOptsEventsValidation pins the §3 `events` vocabulary
// at both SDK entry points.
//
// Before this, `SubscribeOpts.Events` was an unvalidated passthrough:
// a typo reached the wire, matched no event, and produced a
// subscription that silently delivered nothing. A caller bug should
// surface as a 400 at the call, not as silence at runtime. Routed by
// arch as the same fix core-rust owes on its own wrapper.
//
// Tier: Go unit (`make test-sdk`).
func TestSubscribeOptsEventsValidation(t *testing.T) {
	ap, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	bad := []string{"create", "put", "modified", "Created", ""}
	for _, v := range bad {
		opts := SubscribeOpts{Events: []string{v}}

		if _, err := ap.SubscribeAt(ap.PeerID(), "docs/*", opts); err == nil {
			t.Errorf("SubscribeAt accepted invalid event %q", v)
		} else if e := asSDKError(t, err); e != nil && e.Status != 400 {
			t.Errorf("SubscribeAt(%q) status = %d, want 400", v, e.Status)
		}

		if _, err := ap.SubscribeRawAt(ap.PeerID(), "docs/*", "system/inbox/x", "receive", opts); err == nil {
			t.Errorf("SubscribeRawAt accepted invalid event %q", v)
		} else if e := asSDKError(t, err); e != nil && e.Status != 400 {
			t.Errorf("SubscribeRawAt(%q) status = %d, want 400", v, e.Status)
		}
	}

	// The whole valid vocabulary, and a mixed subset, must pass
	// validation — the guard must not narrow what the spec allows.
	// (Empty stays valid and means "all three": absent-on-the-wire and
	// an explicit list are different requests once a server tightens
	// its own default, so the SDK must not substitute one for the other.)
	for _, ok := range [][]string{
		{EventCreated, EventUpdated, EventDeleted},
		{EventDeleted},
		{EventCreated, EventCreated}, // redundant, not ambiguous
		{},
		nil,
	} {
		if verr := validateEvents(ok); verr != nil {
			t.Errorf("validateEvents(%v) rejected a valid vocabulary: %v", ok, verr)
		}
	}
}

func asSDKError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Errorf("error %T is not *entitysdk.Error: %v", err, err)
		return nil
	}
	return e
}
