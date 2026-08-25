package fetch_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/fetch"
)

// The verifying consume stack, driven over another implementation's
// bytes — `entity-core-go`'s WS-A ask (HANDOFF-2026-08-20-workbench-go-
// federation-consume-leg §2/§3) run against the one live foreign
// emission we hold offline.
//
// **What is new here versus TestConsumeBrowserRustSite.** That test
// resolves four paths through the advertised tree-leaf URLs and
// hash-verifies each body. Every assertion in it is satisfied by an
// origin that serves a signed root committing to nothing at all,
// because it never walks the root. These tests walk it.

// copyFixture materializes the frozen rust site into a temp dir so a
// test can withhold or corrupt a byte without touching testdata. The
// fixture is another team's emission and is never edited in place (see
// its README).
func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := "testdata/crossimpl-rust-site"
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func serveDir(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv
}

// blobPath is the on-disk location of a content blob in the fixture's
// `sharded-2-4` store, built the same way the URL is — from the 66-char
// wire hex, format byte included.
func blobPath(dir string, h hash.Hash) string {
	hx := hexOf(h)
	return filepath.Join(dir, "content", hx[0:2], hx[2:4], hx)
}

func hexOf(h hash.Hash) string {
	const digits = "0123456789abcdef"
	b := h.Bytes()
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// TestConsumeBrowserRustSignedRoot is the flagship: a **Go** consumer
// walking the CHAMP trie of a **Rust** publisher's signed root, over
// HTTP, verifying the signature against the key their peer-id itself
// carries — no key distribution, no shared code, no shared process.
//
// This is the operation both our halves skipped until today.
// `entitysdk.ReadPublishedRoot` verified signatures but only over the
// local store; `fetch.Fetch` crossed a wire but resolved by advertised
// leaf path and never touched an interior node. Joined, the chain is
// manifest → signature → trie → leaf, and only the trie step can tell a
// complete origin from a withholding one.
//
// Tier: integration (real HTTP, another implementation's frozen bytes).
func TestConsumeBrowserRustSignedRoot(t *testing.T) {
	srv := rustSite(t)
	ctx := context.Background()

	layout, err := fetch.LoadLayout(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout: %v", err)
	}
	c := fetch.NewConsumer(layout, nil)

	root, err := c.VerifiedRoot(ctx)
	if err != nil {
		t.Fatalf("VerifiedRoot over the Rust emission: %v", err)
	}
	// The signature crossed the language boundary. Recorded loudly
	// because it is the ADR-0012 claim: two implementations on one
	// chain, and the reader is not the emitter's language.
	t.Logf("verified rust signed root: peer=%s seq=%d prefix=%q root_hash=%s alg=%s",
		root.Data.PeerID, root.Data.Seq, root.Data.Prefix, root.Data.RootHash, root.Signature.Algorithm)
	if root.Data.RootHash.IsZero() {
		t.Fatal("their signed root commits to the zero hash")
	}

	walk, err := c.Walk(ctx, root.Data.RootHash)
	if err != nil {
		t.Fatalf("walk their signed root: %v", err)
	}
	if len(walk.Bindings) == 0 {
		t.Fatal("their signed root resolved zero keys — a root that commits to nothing is the " +
			"defect this walk exists to catch, not a small site")
	}
	t.Logf("walked %d CHAMP nodes → %d committed keys", walk.Nodes(), len(walk.Bindings))
	for _, k := range walk.Keys() {
		t.Logf("  committed: %s", k)
	}

	// Every committed key's body resolves and is its own pre-image.
	for _, b := range walk.Bindings {
		ent, err := c.Blob(ctx, b.Hash)
		if err != nil {
			t.Errorf("committed key %q does not resolve: %v", b.Key, err)
			continue
		}
		if ent.Type == "" {
			t.Errorf("committed key %q resolved to a typeless body", b.Key)
		}
	}

	// The absent-key control, gated behind the non-empty enumeration
	// above: a key nobody published is Absent, which is a different
	// answer from "the origin is down".
	if _, err := walk.Lookup("sites/demo/pages/not-a-page"); !errors.Is(err, fetch.ErrAbsent) {
		t.Errorf("lookup of an unpublished key = %v, want ErrAbsent", err)
	}
}

// TestWalkFailsClosedOnAWithheldNode is the mutation that makes the
// walk a measurement rather than a decoration.
//
// **An origin serving a correctly-signed root whose closure it does not
// serve is byte-identical to an honest one at every step except this.**
// `entity-core-go` shipped exactly that shape (a constant root hash,
// fixed at `dabd076`) and their own per-leaf fixture consumer reported
// green against it. Here we withhold one interior CHAMP node from a
// site that is otherwise perfect, and require the consumer to say
// `tree/incomplete-walk` rather than report a smaller site.
//
// This is also the reason the walk is not `core/tree.CollectAllBindings`
// over an HTTP-backed store: that helper skips nodes it cannot load, so
// this test would pass with fewer keys and no error.
//
// Tier: integration + mutation control.
func TestWalkFailsClosedOnAWithheldNode(t *testing.T) {
	ctx := context.Background()

	// First, the honest baseline — and capture the nodes it depends on.
	base := copyFixture(t)
	layout, err := fetch.LoadLayout(ctx, serveDir(t, base).URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout: %v", err)
	}
	c := fetch.NewConsumer(layout, nil)
	root, err := c.VerifiedRoot(ctx)
	if err != nil {
		t.Fatalf("VerifiedRoot: %v", err)
	}
	good, err := c.Walk(ctx, root.Data.RootHash)
	if err != nil {
		t.Fatalf("baseline walk: %v", err)
	}

	// Withhold the LAST node visited rather than the root: a missing
	// root is the easy case that even a lazy consumer notices (nothing
	// resolves at all). An interior node is the one that degrades into
	// "fewer keys" if the walk is best-effort.
	//
	// Their site's 15 keys fit in one CHAMP node, so on THIS fixture the
	// two coincide. The interior case is covered where a deep trie is
	// ours to build — `publish`'s TestConsumeWithholdingInteriorNode.
	if len(good.NodeHashes) == 1 {
		t.Logf("their trie is a single node (%d keys), so the withheld node is the root",
			len(good.Bindings))
	}
	victim := good.NodeHashes[len(good.NodeHashes)-1]

	withheld := copyFixture(t)
	if err := os.Remove(blobPath(withheld, victim)); err != nil {
		t.Fatalf("withhold node %s: %v", victim, err)
	}
	layout2, err := fetch.LoadLayout(ctx, serveDir(t, withheld).URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout (withheld): %v", err)
	}
	c2 := fetch.NewConsumer(layout2, nil)

	// The root still verifies. That is the whole point: the signature is
	// over the published-root entity, not over the closure, so a
	// withholding origin passes every check up to the walk.
	root2, err := c2.VerifiedRoot(ctx)
	if err != nil {
		t.Fatalf("a withholding origin's root should still verify (the signature is not over the "+
			"closure) — got %v", err)
	}
	if root2.Data.RootHash != root.Data.RootHash {
		t.Fatalf("mutation changed the signed root; the test is measuring the wrong thing")
	}

	got, err := c2.Walk(ctx, root2.Data.RootHash)
	if err == nil {
		t.Fatalf("walking a withholding origin succeeded with %d keys (baseline %d) — a best-effort "+
			"walk reports a withholding origin as a smaller site", len(got.Bindings), len(good.Bindings))
	}
	if !errors.Is(err, fetch.ErrIncompleteWalk) {
		t.Fatalf("withheld node failed as %v, want ErrIncompleteWalk", err)
	}
	t.Logf("withheld %s → %v", victim, err)
}

// TestConsumeReconcilesTrieAgainstAdvertisedLeaf drives the check only a
// consumer holding BOTH resolution paths can make.
//
// The trie says which bytes the publisher SIGNED for a key; the
// advertised tree-leaf URL says which bytes the publisher SERVES for it.
// An origin answering differently on the two is serving two trees and
// only one of them is signed. `entity-browser-rust` cross-checks the
// trie against their site *contract*; a per-leaf consumer checks
// neither. This is our unique arm of the ask.
//
// Tier: integration.
func TestConsumeReconcilesTrieAgainstAdvertisedLeaf(t *testing.T) {
	ctx := context.Background()
	layout, err := fetch.LoadLayout(ctx, rustSite(t).URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout: %v", err)
	}
	c := fetch.NewConsumer(layout, nil)

	rep, err := c.Consume(ctx, fetch.ConsumeOpts{
		Bodies:      true,
		Reconcile:   true,
		AbsentProbe: "sites/demo/pages/not-a-page",
	})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !rep.AbsentCorrect {
		t.Errorf("absent-key control did not fire: %v", rep.AbsentErr)
	}

	// Reconciliation is reported per key rather than asserted wholesale:
	// which keys a publisher ALSO exposes at a tree-leaf URL is their
	// layout decision, and a key committed in the trie with no leaf URL
	// is not a defect. What would be a defect is a key exposed at both
	// with two different hashes — that is ErrContractMismatch, and it is
	// the only failure this test refuses.
	var reconciled, unreachable int
	for _, k := range rep.Keys {
		switch {
		case errors.Is(k.Err, fetch.ErrContractMismatch):
			t.Errorf("%s: %v", k.Key, k.Err)
		case k.Err != nil:
			unreachable++
		case k.Reconciled:
			reconciled++
		}
	}
	t.Logf("reconciled %d/%d committed keys against their advertised leaf URLs (%d not exposed there)",
		reconciled, len(rep.Keys), unreachable)
	if reconciled == 0 {
		t.Error("not one committed key reconciled against an advertised leaf URL — the join is " +
			"probably wrong rather than the publisher being sparse")
	}
}

// TestPinnedLayoutEqualsDiscoveredLayout proves the hand-supplied path
// `entity-core-go`'s federation origin forces us onto is the SAME
// consumer, not a second one.
//
// Their origin serves no `transport-profile` (§6.5.4 makes profile
// distribution out-of-band in v1, so that is conformant), which is arch
// R-28: a consumer handed only `(origin, peer_id)` has nowhere to learn
// `content_layout` from, and a wrong guess is byte-identical to a
// withholding origin. [fetch.PinnedLayout] is the honest answer — the
// layout came from a human. This test pins that the pinned path and the
// discovered path produce the same walk over the same bytes.
//
// Tier: contract pin.
func TestPinnedLayoutEqualsDiscoveredLayout(t *testing.T) {
	ctx := context.Background()
	srv := rustSite(t)

	discovered, err := fetch.LoadLayout(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout: %v", err)
	}

	pinned, err := fetch.PinnedLayout(srv.URL, rustPeer, types.TransportEndpoint{
		TreeURLPrefix:     "/" + rustPeer,
		ContentURLPrefix:  "/content",
		ManifestURLPrefix: "/" + rustPeer + "/system/peer/published-root",
		ContentLayout:     types.ContentLayoutSharded24,
		TreeLeafSuffix:    ".bin",
		TreeListingSuffix: ".list",
	})
	if err != nil {
		t.Fatalf("PinnedLayout: %v", err)
	}

	da, err := fetch.NewConsumer(discovered, nil).Consume(ctx, fetch.ConsumeOpts{})
	if err != nil {
		t.Fatalf("consume (discovered): %v", err)
	}
	pa, err := fetch.NewConsumer(pinned, nil).Consume(ctx, fetch.ConsumeOpts{})
	if err != nil {
		t.Fatalf("consume (pinned): %v", err)
	}
	if da.Root.Data.RootHash != pa.Root.Data.RootHash {
		t.Errorf("pinned root %s != discovered root %s", pa.Root.Data.RootHash, da.Root.Data.RootHash)
	}
	if got, want := strings.Join(pa.Walk.Keys(), ","), strings.Join(da.Walk.Keys(), ","); got != want {
		t.Errorf("pinned walk %q != discovered walk %q", got, want)
	}
}

// TestAbsolutePrefixResolvesAllThreeShapes pins EXTENSION-TREE §3.3's
// reconstruction table, because **both live publishers in this cohort
// pick a different row of it** and the field on the wire is the
// *configured* prefix rather than the absolute one.
//
// browser-rust publishes the peer-qualified shape (`/{peer}/`); our own
// `publish/` publishes the peer-relative shape (`docs/`). A consumer
// that concatenates the field verbatim is right for one and silently
// wrong for the other, and the failure is a 404 — indistinguishable at
// the consumer from a withholding origin.
//
// The universal row is the one that must NOT be peer-joined: its trim is
// a no-op, so its keys are already fully qualified, and joining a
// peer-id there produces a mixed key space (§3.3 rules that out).
//
// Tier: contract pin.
func TestAbsolutePrefixResolvesAllThreeShapes(t *testing.T) {
	const peer = "2KEE55MMWBvXE8ozrm45kwTchzFaQP1dzBnU9rifGTdmUa"
	for _, tc := range []struct {
		name, prefix, key, want string
	}{
		{"peer-relative (ours)", "docs/", "section/page", "/" + peer + "/docs/section/page"},
		{"peer-qualified (browser-rust)", "/" + peer + "/", "sites/demo/index", "/" + peer + "/sites/demo/index"},
		{"universal — no-op trim, keys already qualified", "/", "/" + peer + "/system/attestation", "/" + peer + "/system/attestation"},
	} {
		if got := fetch.AbsolutePath(tc.prefix, peer, tc.key); got != tc.want {
			t.Errorf("%s: AbsolutePath(%q, %q) = %q, want %q", tc.name, tc.prefix, tc.key, got, tc.want)
		}
	}
}
