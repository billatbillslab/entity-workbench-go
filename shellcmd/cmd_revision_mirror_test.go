package shellcmd

import (
	"context"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/tree"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/publishedroot"

	"entity-workbench-go/entitysdk"
)

// Tier: two-peer integration (`make test-shellcmd`).
//
// InstallRevisionMirrorChain — the standing-chain half of §5.1 step 3
// of REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17.
//
// The one-shot pull (entitysdk MirrorSinceLastSeen) and the standing
// chain fail in different ways, which is why both are measured. The
// one-shot returns its error to the caller. The chain does not: a
// merge step that 403s routes to `system/inbox/follow-errors/…` and
// the chain keeps looking installed, so a capability defect here
// presents as "the mirror is just empty" — the silent-failure shape
// the follow chain's own OnError wiring exists to fight. This test
// asserts data actually arrives, and asserts it arrives in the
// publisher's namespace and not ours.

// mirrorChainPeer is a listening, open-access peer.
func newMirrorChainPeer(t *testing.T, ctx context.Context, name string) *entitysdk.AppPeer {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	})
	if err != nil {
		t.Fatalf("CreatePeer %s: %v", name, err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	ready := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- ap.ListenReady(ctx, ready) }()
	select {
	case <-ready:
	case err := <-errCh:
		t.Fatalf("%s listen: %v", name, err)
	case <-time.After(3 * time.Second):
		t.Fatalf("%s not ready in 3s", name)
	}
	return ap
}

// TestInstallRevisionMirrorChain_DeliversIntoPublisherNamespace: a
// commit made AFTER the chain is installed reaches the follower's tree
// under /{publisher}/{prefix}, and never under the follower's own.
func TestInstallRevisionMirrorChain_DeliversIntoPublisherNamespace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const prefix = "shared/"

	publisher := newMirrorChainPeer(t, ctx, "publisher")
	follower := newMirrorChainPeer(t, ctx, "follower")

	// Both directions: the follower dispatches fetch-diff to the
	// publisher, and the publisher delivers head notifications back.
	if _, err := follower.Connect(ctx, publisher.Addr().String()); err != nil {
		t.Fatalf("follower→publisher connect: %v", err)
	}
	if _, err := publisher.Connect(ctx, follower.Addr().String()); err != nil {
		t.Fatalf("publisher→follower connect: %v", err)
	}

	autoTrue := true
	if _, err := publisher.Revision().ConfigPut(ctx, "mirror-chain", types.RevisionConfigData{
		Prefix:      prefix,
		AutoVersion: &autoTrue,
	}, nil); err != nil {
		t.Fatalf("ConfigPut: %v", err)
	}

	// Seed one entity so the prefix has a revision head, then publish
	// a signed root declaring it — the chain's destination comes from
	// that declaration, so it has to exist before the install.
	if _, err := publisher.Put(prefix+"seed", "test/file", "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	st, err := publisher.Revision().Status(ctx, prefix)
	if err != nil {
		t.Fatalf("revision status: %v", err)
	}
	tracker := tree.NewRootTracker(publisher.RawContentStore(), publisher.PeerID(), nil)
	pub := publishedroot.NewPublisher(publisher.RawContentStore(), tracker, prefix, nil,
		publishedroot.WithDebounce(0))
	if err := pub.SetupAuthority(publisher.RawLocationIndex(), publisher.RawPeer().Keypair(),
		publisher.RawPeer().Identity(), false); err != nil {
		t.Fatalf("publisher SetupAuthority: %v", err)
	}
	if _, err := pub.Publish(st.Head); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	sub, err := InstallRevisionMirrorChain(ctx, follower, publisher.PeerID(), prefix)
	if err != nil {
		t.Fatalf("InstallRevisionMirrorChain: %v", err)
	}
	t.Logf("mirror chain installed (sub=%s)", sub.ID())

	// A commit made after the install is what the standing chain is
	// for. Give the subscription a moment to be live first.
	time.Sleep(500 * time.Millisecond)
	if _, err := publisher.Put(prefix+"after-install", "test/file", "post"); err != nil {
		t.Fatalf("post-install put: %v", err)
	}

	// Read the follower's OWN store, never List(): a peer-qualified
	// path names another peer, so a dispatched List routes to the
	// publisher and reports the publisher's own tree — an assertion
	// that is green whether or not anything was mirrored. A mutation
	// check caught exactly that here.
	target := "/" + publisher.PeerID() + "/" + prefix
	deadline := time.Now().Add(20 * time.Second)
	var got []store.LocationEntry
	for time.Now().Before(deadline) {
		got = follower.Store().List(target)
		if len(got) >= 2 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(got) < 2 {
		paths := make([]string, 0, len(got))
		for _, e := range got {
			paths = append(paths, e.Path)
		}
		t.Fatalf("mirror at %s holds %d entries %v after 20s, want the seed + the post-install commit",
			target, len(got), paths)
	}

	// The negative half: nothing landed in the follower's own
	// namespace at the same relative prefix.
	local := follower.Store().List("/" + follower.PeerID() + "/" + prefix)
	if len(local) != 0 {
		t.Errorf("%d entries landed in the FOLLOWER's namespace at %q", len(local), prefix)
	}
	t.Logf("standing chain delivered %d entries to %s; follower's own %q empty",
		len(got), target, prefix)
}

// TestInstallRevisionMirrorChain_RequiresPublishedRoot: without a
// published root there is no signed destination, so the chain refuses
// to install rather than defaulting to somewhere plausible.
func TestInstallRevisionMirrorChain_RequiresPublishedRoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	publisher := newMirrorChainPeer(t, ctx, "publisher")
	follower := newMirrorChainPeer(t, ctx, "follower")
	if _, err := follower.Connect(ctx, publisher.Addr().String()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	_, err := InstallRevisionMirrorChain(ctx, follower, publisher.PeerID(), "shared/")
	if err == nil {
		t.Fatal("want an error installing a mirror chain against an unpublished peer")
	}
	if entitysdk.CodeOf(err) != "no_published_root" {
		t.Fatalf("code = %q, want no_published_root (err: %v)", entitysdk.CodeOf(err), err)
	}
}
