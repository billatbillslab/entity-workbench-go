package entitysdk

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// Tier: Go unit + two-peer integration (`make test-sdk`).
//
// MirrorSinceLastSeen — §5.1 step 3 of
// REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17, the sync surface
// gaining the ability to express source ≠ target, with the target's
// value taken from the publisher's signed published-root rather than
// from the caller.
//
// The load-bearing assertion in the integration test is the NEGATIVE
// one: that nothing landed under our own relative prefix. W2 is the
// finding that a followed subtree lands at `/{us}/watched/…` instead
// of `/{them}/…`, and a test that only checked the mirror arrived
// would pass just as happily if the data had been written to both
// places. Same discipline as the foreign-namespace subscription gate:
// assert the intended destination AND the silence of the other one.

// mirrorPair stands up a publisher (server) that has revisioned data
// under prefix and has published a signed root declaring it, plus a
// connected follower (client).
func mirrorPair(t *testing.T, ctx context.Context, prefix string, seed int) (client, server *AppPeer) {
	t.Helper()
	client, server, addr := twoPeers(t)

	autoTrue := true
	if _, err := server.Revision().ConfigPut(ctx, "mirror-test", types.RevisionConfigData{
		Prefix:      prefix,
		AutoVersion: &autoTrue,
	}, nil); err != nil {
		t.Fatalf("ConfigPut: %v", err)
	}

	for i := 0; i < seed; i++ {
		path := fmt.Sprintf("%sdoc-%02d", prefix, i)
		if _, err := server.Put(path, "test/file", map[string]interface{}{
			"i": i, "body": fmt.Sprintf("content-%d", i),
		}); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	// Auto-version settles asynchronously; the revision head has to
	// exist before fetch-diff has anything to hand over.
	time.Sleep(500 * time.Millisecond)

	// Publish a root for the shared prefix. The root hash is the
	// prefix's actual revision head, so the published claim and the
	// data being mirrored are the same state — the mirror does not yet
	// verify that correspondence (see MirrorSinceLastSeen's doc), but
	// staging it wrong would bake a falsehood into the fixture.
	st, err := server.Revision().Status(ctx, prefix)
	if err != nil {
		t.Fatalf("revision status: %v", err)
	}
	if st.Head.IsZero() {
		t.Fatalf("no revision head for %s after seeding", prefix)
	}
	pub := startPublisherFor(t, server, prefix)
	if _, err := pub.Publish(st.Head); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if _, err := client.Connect(ctx, addr); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return client, server
}

// TestMirrorSinceLastSeen_LandsInPublisherNamespace is the W2 fix,
// measured: the pulled subtree appears at /{them}/{their path} and
// nowhere in our own namespace.
func TestMirrorSinceLastSeen_LandsInPublisherNamespace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const prefix = "shared/"
	const seed = 5
	client, server := mirrorPair(t, ctx, prefix, seed)

	res, err := client.MirrorSinceLastSeen(ctx, server.PeerID(), "", hash.Hash{})
	if err != nil {
		t.Fatalf("MirrorSinceLastSeen: %v", err)
	}

	wantTarget := "/" + server.PeerID() + "/" + prefix
	if res.TargetPrefix != wantTarget {
		t.Errorf("TargetPrefix = %q, want %q", res.TargetPrefix, wantTarget)
	}
	if res.Prefix != prefix {
		t.Errorf("source Prefix = %q, want %q", res.Prefix, prefix)
	}
	// The destination came from the publisher's signed declaration,
	// not from us.
	if res.PublishedRoot.Data.Prefix != prefix {
		t.Errorf("published prefix = %q, want %q", res.PublishedRoot.Data.Prefix, prefix)
	}

	// Read the mirror from OUR OWN store, not through List(). A
	// peer-qualified path names another peer, so the dispatched List
	// routes to the publisher and would happily report the
	// publisher's own tree back to us — an assertion that passes
	// whether or not anything was ever mirrored. Found by a mutation
	// check: reverting the target prefix left this assertion green.
	mirrored := client.Store().List(wantTarget)
	if len(mirrored) != seed {
		t.Errorf("mirror holds %d entries at %s in OUR store, want %d", len(mirrored), wantTarget, seed)
	}

	// The negative half. "shared/" is relative, so it resolves to
	// /{client}/shared/ — the exact place the old single-prefix call
	// would have put it. It must be empty.
	local := client.Store().List("/" + client.PeerID() + "/" + prefix)
	if len(local) != 0 {
		t.Errorf("%d entries landed in OUR namespace at %q — W2 not fixed", len(local), prefix)
	}
	t.Logf("mirrored %d entries to %s; local %q empty", len(mirrored), wantTarget, prefix)
}

// TestReconcileSinceLastSeen_StillLandsLocally pins the OTHER half of
// the split. ReconcileSinceLastSeen keeps its local-prefix semantics —
// its callers ("watched/", "collab/") are pulling into scratch space of
// their own, which is a legitimate thing to want, and quietly changing
// where their data lands would be its own bug. The two calls differ in
// destination and this test is what says so out loud.
func TestReconcileSinceLastSeen_StillLandsLocally(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const prefix = "shared/"
	const seed = 3
	client, server := mirrorPair(t, ctx, prefix, seed)

	res, err := client.ReconcileSinceLastSeen(ctx, server.PeerID(), prefix, hash.Hash{})
	if err != nil {
		t.Fatalf("ReconcileSinceLastSeen: %v", err)
	}
	if res.TargetPrefix != prefix {
		t.Errorf("TargetPrefix = %q, want %q (unchanged behavior)", res.TargetPrefix, prefix)
	}

	local := client.Store().List("/" + client.PeerID() + "/" + prefix)
	if len(local) != seed {
		t.Errorf("local %q holds %d entries, want %d", prefix, len(local), seed)
	}
}

// TestMirrorSinceLastSeen_RequiresPublishedRoot: the dependency §5.1
// names — step 3 does not work without step 3a. A peer that has not
// published a signed root cannot be mirrored, because there is nothing
// that says where its data belongs.
func TestMirrorSinceLastSeen_RequiresPublishedRoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, server, addr := twoPeers(t)
	if _, err := client.Connect(ctx, addr); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	_, err := client.MirrorSinceLastSeen(ctx, server.PeerID(), "shared/", hash.Hash{})
	wantSDKError(t, err, "no_published_root")
}

// TestMirrorSinceLastSeen_RejectsSelf: mirroring yourself would make
// the destination the source.
func TestMirrorSinceLastSeen_RejectsSelf(t *testing.T) {
	p, err := NewAppPeer()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	_, err = p.MirrorSinceLastSeen(context.Background(), p.PeerID(), "x/", hash.Hash{})
	wantSDKError(t, err, "invalid_peer")
}

// TestMirrorSourcePrefix is the resolution rule on its own: which
// requested prefixes are admissible against a publisher's declared
// one, and what the empty request means.
func TestMirrorSourcePrefix(t *testing.T) {
	for _, tc := range []struct {
		name, requested, declared, want, wantCode string
	}{
		{"empty takes the declared prefix", "", "system/", "system/", ""},
		{"exact match", "system/", "system/", "system/", ""},
		{"subtree of the declared prefix", "system/peer/", "system/", "system/peer/", ""},
		{"missing trailing slash is added", "system/peer", "system/", "system/peer/", ""},
		{"outside the declared prefix", "docs/", "system/", "", "prefix_not_published"},
		// A near-miss that string-prefix logic gets wrong if the
		// declared prefix is not slash-terminated. It always is
		// (§3.3a MUST, and ReadPublishedRoot refuses otherwise), which
		// is what makes "system-secret/" fail here rather than pass as
		// a prefix of "system".
		{"sibling with a shared string prefix", "system-secret/", "system/", "", "prefix_not_published"},
		{"universal tree admits any subtree", "docs/", "/", "docs/", ""},
		{"universal tree needs a named subtree", "", "/", "", "invalid_prefix"},
		{"publisher declared nothing", "docs/", "", "", "missing_prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mirrorSourcePrefix(tc.requested, tc.declared)
			if tc.wantCode != "" {
				wantSDKError(t, err, tc.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMirror_ForeignNamespaceMergeNeedsItsOwnCapability pins the
// constraint that made MirrorSinceLastSeen more than an API-shape
// change.
//
// REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17 W3 concluded that
// landing a mirror at /{them}/… needed "no wire change" and that what
// blocked it was "entirely ours, in an SDK signature." The first half
// holds — `applyPrefix` is plain concatenation and the local index
// passes an absolute path through. The second half was incomplete:
// `tree:merge` pre-checks put authorization on every target path
// (core/tree/operations.go), and the peer's owner self-cap has
// `Resources: ["*"]`, which under §PR-8 canonicalizes to `/{me}/*` and
// does not cover `/{them}/…`. So the same argument-from-source that
// was right about the path layer missed the capability layer one step
// down — the same shape of miss as the watch-hub crash.
//
// This test is the measurement. It runs the merge with the standing
// owner cap and asserts the 403, so if core-go ever changes how a
// self-issued grant covers cached-remote namespaces, we find out here
// rather than by the mirror silently starting or stopping to work.
func TestMirror_ForeignNamespaceMergeNeedsItsOwnCapability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const prefix = "shared/"
	client, server := mirrorPair(t, ctx, prefix, 2)
	target := "/" + server.PeerID() + "/" + prefix

	// The internal path with a zero capability = the executor's owner
	// self-cap, which is what an absolute target would have run under
	// if MirrorSinceLastSeen had only changed the signature.
	_, err := client.reconcile(ctx, server.PeerID(), prefix, target, hash.Hash{})
	if err == nil {
		t.Fatal("expected the owner self-cap to be refused on a foreign-namespace merge target")
	}
	if StatusOf(err) != 403 {
		t.Fatalf("status = %d, want 403 (err: %v)", StatusOf(err), err)
	}

	// And the same merge succeeds once a cap naming that namespace is
	// minted — so the refusal is about the capability's scope, not
	// about the path being unwritable.
	if _, err := client.MirrorSinceLastSeen(ctx, server.PeerID(), prefix, hash.Hash{}); err != nil {
		t.Fatalf("MirrorSinceLastSeen with a namespace-scoped cap: %v", err)
	}
	if entries := client.Store().List(target); len(entries) != 2 {
		t.Errorf("mirror holds %d entries in our store, want 2", len(entries))
	}
}
