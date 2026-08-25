package publish_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.entitychurch.org/entity-core-go/core/hash"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/fetch"
	"entity-workbench-go/publish"
)

// Our two halves, finally meeting over the operation that matters: the
// publisher emits a signed root plus its §6.5.3 closure, and the
// verifying consumer walks the CHAMP trie from that signed root rather
// than resolving by the advertised leaf path.
//
// `TestPublishThenFetch_TheTwoHalvesOfOurOwnCorridor` (consume_test.go)
// already proves the corridor carries bytes. **It passes against an
// origin whose signed root commits to nothing**, because it never walks
// the root — which is precisely the shape `entity-core-go` shipped for a
// week and nobody's per-leaf consumer noticed. These tests are the
// difference.
//
// Tier: integration (real HTTP, real emitted directory) + mutation
// controls.

// deepSite seeds enough entities that the published CHAMP trie is more
// than one node, so the withheld-interior-node control below has an
// interior node to withhold. Their frozen fixture's 15 keys fit in a
// single node; ours is the deep case.
func deepSite(t *testing.T) (*entitysdk.AppPeer, []string) {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { ap.Close() })

	var keys []string
	for i := 0; i < 64; i++ {
		p := fmt.Sprintf("docs/section-%02d/page-%02d", i%8, i)
		if _, err := ap.Store().Put(p, "test/note", map[string]string{
			"title": fmt.Sprintf("page %d", i),
			"body":  "the closure has to cover this",
		}); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
		keys = append(keys, p)
	}
	return ap, keys
}

// publishTo emits a site into a fresh directory already being served,
// and returns the origin. The origin must be known BEFORE the publish
// because it is baked into the emitted profile.
func publishTo(t *testing.T, ap *entitysdk.AppPeer) (origin, dir string) {
	t.Helper()
	dir = t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	if _, err := publish.Publish(context.Background(), publish.Opts{
		Peer: ap, Prefix: "docs/", OutputDir: dir, OriginURL: srv.URL,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return srv.URL, dir
}

func consumerFor(t *testing.T, origin string) *fetch.Consumer {
	t.Helper()
	layout, err := fetch.LoadLayout(context.Background(), origin, nil)
	if err != nil {
		t.Fatalf("LoadLayout: %v", err)
	}
	return fetch.NewConsumer(layout, nil)
}

// blobFile is the on-disk path of a content blob in our emitted
// `sharded-2-4` store — the same join the URL uses, over the 66-char
// wire hex.
func blobFile(dir string, h hash.Hash) string {
	hx := fmt.Sprintf("%x", h.Bytes())
	return filepath.Join(dir, "content", hx[0:2], hx[2:4], hx)
}

// TestConsumeOurOwnSignedRoot walks our publisher's emission the way a
// foreign consumer does: manifest → signature → trie → leaf, then
// enumerate the exact committed key set and reconcile each key against
// the leaf URL the same publisher advertises.
func TestConsumeOurOwnSignedRoot(t *testing.T) {
	ap, seeded := deepSite(t)
	origin, _ := publishTo(t, ap)
	c := consumerFor(t, origin)
	ctx := context.Background()

	rep, err := c.Consume(ctx, fetch.ConsumeOpts{
		Bodies:      true,
		Reconcile:   true,
		AbsentProbe: "section-00/never-published",
	})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	t.Logf("verified own signed root: seq=%d prefix=%q root=%s → %d CHAMP nodes, %d keys",
		rep.Root.Data.Seq, rep.Root.Data.Prefix, rep.Root.Data.RootHash,
		rep.Walk.Nodes(), len(rep.Walk.Bindings))

	// A trie deep enough for the interior-node control to mean
	// something. If a future CHAMP change flattens this, the control
	// below silently degrades into the root case — so it is asserted
	// here rather than assumed there.
	if rep.Walk.Nodes() < 2 {
		t.Errorf("published trie is %d node(s); this fixture exists to be deeper", rep.Walk.Nodes())
	}

	// Enumeration is EXACT: every seeded key committed, and nothing
	// committed that was not seeded. "No more, none hidden behind an
	// unresolvable link" is the clause the walk discharges.
	committed := map[string]bool{}
	for _, k := range rep.Walk.Keys() {
		committed[k] = true
	}
	for _, s := range seeded {
		// Keys are relative to the published-root's prefix.
		rel := s[len("docs/"):]
		if !committed[rel] {
			t.Errorf("seeded %q is not committed by the signed root", rel)
		}
		delete(committed, rel)
	}
	for extra := range committed {
		t.Errorf("the signed root commits to %q, which was never published", extra)
	}

	if fails := rep.Failures(); len(fails) > 0 {
		for _, f := range fails {
			t.Errorf("key %q: %v", f.Key, f.Err)
		}
	}
	if !rep.AbsentCorrect {
		t.Errorf("absent-key control did not fire: %v", rep.AbsentErr)
	}
}

// TestConsumeWithholdingInteriorNode is the control the frozen rust
// fixture is too small to carry: withhold one **interior** CHAMP node
// from an otherwise perfect site.
//
// A best-effort walk (which is what `core/tree.CollectAllBindings` does,
// by documented design — "Missing nodes are skipped") reports this as a
// site with fewer keys and no error. The consumer must call it
// `tree/incomplete-walk` instead: the signed root commits to that node,
// so an origin that will not serve it is withholding, whatever its
// reason.
func TestConsumeWithholdingInteriorNode(t *testing.T) {
	ap, _ := deepSite(t)
	origin, dir := publishTo(t, ap)
	c := consumerFor(t, origin)
	ctx := context.Background()

	root, err := c.VerifiedRoot(ctx)
	if err != nil {
		t.Fatalf("VerifiedRoot: %v", err)
	}
	good, err := c.Walk(ctx, root.Data.RootHash)
	if err != nil {
		t.Fatalf("baseline walk: %v", err)
	}
	if good.Nodes() < 2 {
		t.Fatalf("need an interior node to withhold; the trie has %d", good.Nodes())
	}

	// Not the root: a missing root is the easy case any consumer
	// notices. NodeHashes[0] is the root, so anything after it is
	// interior.
	victim := good.NodeHashes[1]
	if err := os.Remove(blobFile(dir, victim)); err != nil {
		t.Fatalf("withhold interior node %s: %v", victim, err)
	}

	// The root still verifies — the signature is over the published-root
	// entity, not over the closure. Every gate before the walk passes.
	if _, err := c.VerifiedRoot(ctx); err != nil {
		t.Fatalf("withholding a node broke the signature check; the test is measuring the wrong "+
			"thing: %v", err)
	}

	got, err := c.Walk(ctx, root.Data.RootHash)
	if err == nil {
		t.Fatalf("walk succeeded with %d of %d keys after an interior node was withheld — that is "+
			"a withholding origin reported as a smaller site", len(got.Bindings), len(good.Bindings))
	}
	if !errors.Is(err, fetch.ErrIncompleteWalk) {
		t.Fatalf("withheld interior node failed as %v, want ErrIncompleteWalk", err)
	}
}

// TestConsumeCorruptedLeafBodyIsRefused withholds nothing and changes
// one byte instead: a leaf body that is no longer its own pre-image.
//
// The walk still completes — the trie structure is intact — so this is
// the failure mode that must be reported PER KEY rather than as a dead
// origin. A report that dies on the first bad body cannot say how much
// of a site is good.
func TestConsumeCorruptedLeafBodyIsRefused(t *testing.T) {
	ap, _ := deepSite(t)
	origin, dir := publishTo(t, ap)
	c := consumerFor(t, origin)
	ctx := context.Background()

	root, err := c.VerifiedRoot(ctx)
	if err != nil {
		t.Fatalf("VerifiedRoot: %v", err)
	}
	walk, err := c.Walk(ctx, root.Data.RootHash)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	victim := walk.Bindings[0]
	path := blobFile(dir, victim.Hash)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read leaf body: %v", err)
	}
	body[len(body)-1] ^= 0xff
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("corrupt leaf body: %v", err)
	}

	rep, err := c.Consume(ctx, fetch.ConsumeOpts{Bodies: true})
	if err != nil {
		t.Fatalf("Consume should survive one bad body: %v", err)
	}
	fails := rep.Failures()
	if len(fails) != 1 || fails[0].Key != victim.Key {
		t.Fatalf("corrupting %q produced %d failures %v, want exactly that key",
			victim.Key, len(fails), fails)
	}
	if len(rep.Keys) != len(walk.Bindings) {
		t.Errorf("report covers %d keys, walk committed %d — a bad body must not truncate the report",
			len(rep.Keys), len(walk.Bindings))
	}
}

// TestEmptyEnumerationGatesTheAbsentControl pins the trap the absent-key
// control exists to avoid: an empty root answers Absent to everything,
// so a control that runs before enumeration measures nothing at all.
//
// Tier: contract pin.
func TestEmptyEnumerationGatesTheAbsentControl(t *testing.T) {
	var empty fetch.WalkResult
	if _, err := empty.Lookup("anything"); !errors.Is(err, fetch.ErrEmptyEnumeration) {
		t.Fatalf("lookup against an empty enumeration = %v, want ErrEmptyEnumeration", err)
	}
	nonEmpty := fetch.WalkResult{Bindings: []fetch.Binding{{Key: "a"}}}
	if _, err := nonEmpty.Lookup("b"); !errors.Is(err, fetch.ErrAbsent) {
		t.Fatalf("lookup of a missing key in a non-empty enumeration = %v, want ErrAbsent", err)
	}
}
