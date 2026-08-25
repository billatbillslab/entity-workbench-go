package fetch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/fetch"
)

// registry_test.go — hop 1, against another implementation's bytes.
//
// The fixture is `entity-browser-rust`'s `make federation` emission,
// frozen: a static registry signing four names, and the four static
// domains those names point at, each under its own key. Provenance is in
// testdata/crossimpl-rust-federation/README.md.
//
// **What makes these tests worth their runtime is the negative
// controls.** A resolver that returns the right peer-id for four names
// has demonstrated nothing a resolver that ignores every check does not
// also demonstrate — the emission is honest, so the happy path is green
// either way. The controls below construct the dishonest origins at read
// time (substitution, withholding, a lie in the menu) and require a
// refusal.
//
// Tier: integration (real HTTP over another implementation's bytes).

const (
	fedRegistryPeer = "2KBLkCxvkgobuauPA6zPfKarpuRRnnWHL98n8Gv1GNmybr"
	fedFoundation   = "2KGTrr4LxfFJjUBUD74XJZoze74TrQFpXqWA19qeskz9sS"
	fedProtocol     = "2KAdu6wwTNAoQiqXmN93vbjHxk3QosG7hxhXGZtN8wZF31"
	fedDocs         = "2KFRBJ9feEPCZZiNaCEKsCAVkGkp1htWZk9a8jz5n5D2sS"
	fedLab          = "2KEb7HgmoCRF1VpNeCYusiubnn94ke4uK1hUxBcQ1PTtNA"
)

const fedRoot = "testdata/crossimpl-rust-federation"

// fedMapping is MAPPING.txt, transcribed. The fixture ships it as prose;
// asserting against a transcription is what turns "the names resolved"
// into "the names resolved to what their emitter said they would".
var fedMapping = map[string]string{
	"entitychurch.org":          fedFoundation,
	"protocol.entitychurch.org": fedProtocol,
	"docs.entitychurch.org":     fedDocs,
	"lab.entitychurch.org":      fedLab,
}

// serveFed serves the WHOLE frozen federation from one origin —
// `/registry`, `/foundation`, `/docs`, `/protocol`, `/lab` — optionally
// with an interception hook that lets a test build a dishonest origin
// without touching a byte on disk.
//
// One origin is not a convenience: their bindings carry **origin-
// relative** URL prefixes (`/docs/content`), and an origin-relative URL
// resolves against a scheme://host:port, so the registry and the domains
// it names must share one for the federation to be reachable at all.
// That is the deployment shape their `tools/local-federation.sh` emits
// and the one a static host actually produces.
func serveFed(t *testing.T, hook func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	t.Helper()
	files := http.FileServer(http.Dir(fedRoot))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil && hook(w, r) {
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pinFedRegistry builds the PINNED layout for the registry origin.
//
// The registry serves **no** `transport-profile` — conformant per
// NETWORK §6.5.4 (profile distribution is out-of-band in v1) and exactly
// arch's R-28. So the layout is a pin, and every surface that reports on
// this run has to say so: a wrong pin and a withholding origin are
// byte-identical from here.
func pinFedRegistry(t *testing.T, origin string) fetch.Layout {
	t.Helper()
	layout, err := fetch.PinnedLayout(origin, fedRegistryPeer, types.TransportEndpoint{
		TreeURLPrefix:     "/registry/" + fedRegistryPeer,
		ContentURLPrefix:  "/registry/content",
		ManifestURLPrefix: "/registry/" + fedRegistryPeer + "/system/peer/published-root",
		ContentLayout:     types.ContentLayoutSharded24,
		TreeLeafSuffix:    ".bin",
		TreeListingSuffix: ".list",
	})
	if err != nil {
		t.Fatalf("PinnedLayout for the Rust registry: %v", err)
	}
	return layout
}

func fedRegistry(t *testing.T, hook func(http.ResponseWriter, *http.Request) bool) (*fetch.Registry, *httptest.Server) {
	t.Helper()
	srv := serveFed(t, hook)
	reg, err := fetch.NewRegistry(pinFedRegistry(t, srv.URL), nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg, srv
}

// atIssue pins the clock just after a resolved binding was issued.
//
// The fixture's bindings carry a real 30-day TTL from the day they were
// emitted, so a test that used the wall clock would pass now and fail
// silently later — the fixture would rot into a broken build with no
// diff to blame. Expiry is asserted deliberately in
// TestFedExpiredBindingIsRefused instead, which is the honest place for
// it: one test proves the check fires, and no test depends on today.
func atIssue(reg *fetch.Registry, ctx context.Context, t *testing.T, name string) {
	t.Helper()
	// A first pass with a clock far in the past cannot expire; read the
	// issue time off it, then pin an hour later for the real assertions.
	reg.Now = func() time.Time { return time.UnixMilli(0) }
	res, err := reg.Resolve(ctx, name)
	if err != nil && !errors.Is(err, fetch.ErrNameExpired) {
		t.Fatalf("probing %s for its issue time: %v", name, err)
	}
	issued := res.IssuedAt
	reg.Now = func() time.Time { return issued.Add(time.Hour) }
}

// TestFedEnumerateIsTheWalkNotTheListing is §6a.3a: the authoritative
// answer to "what names does this registry carry" is the walk of the
// signed trie, and the served listing is a menu.
func TestFedEnumerateIsTheWalkNotTheListing(t *testing.T) {
	reg, _ := fedRegistry(t, nil)
	ctx := context.Background()

	set, err := reg.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate the Rust registry: %v", err)
	}

	got := set.NameStrings()
	if len(got) != len(fedMapping) {
		t.Fatalf("walk committed %d names %v, their emitter published %d", len(got), got, len(fedMapping))
	}
	for _, n := range got {
		if _, ok := fedMapping[n]; !ok {
			t.Errorf("walk committed a name the fixture's MAPPING.txt does not carry: %q", n)
		}
	}

	// The walk cost something, and the something is the point: those
	// interior nodes are what a withholding origin has to serve.
	if set.Walk.Nodes() == 0 {
		t.Error("the walk resolved no CHAMP nodes, which means it did not happen")
	}

	// The listing is present here and agrees. Agreement is the expected
	// state for an honest origin, and it is NOT what makes the answer
	// trustworthy — see TestFedListingCanLieAndTheWalkCannot.
	if set.ListingErr != nil {
		t.Fatalf("their emitter publishes a by-name listing; fetching it failed: %v", set.ListingErr)
	}
	if !set.Reconciled() {
		t.Errorf("menu and signed key set disagree: advertised-only %v, committed-only %v",
			set.AdvertisedOnly, set.CommittedOnly)
	}
}

// TestFedListingCanLieAndTheWalkCannot is the §6a.3a asymmetry, made
// into a measurement.
//
// The origin advertises a fifth name it has no signed binding for, and
// hides one it does. A consumer that presents the served listing as the
// registry's contents shows the operator a name that cannot resolve and
// hides one that can. The walk is unmoved by both, and the disagreement
// is reported rather than silently reconciled — because which of the two
// is wrong is not ours to decide, only ours to surface.
func TestFedListingCanLieAndTheWalkCannot(t *testing.T) {
	reg, _ := fedRegistry(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/system/registry/binding/by-name.list") {
			return false
		}
		// docs.* removed, ghost.* invented. Both undetectable in the
		// listing itself: it is transport-supplied and signs nothing.
		_, _ = w.Write([]byte("entitychurch.org\nprotocol.entitychurch.org\nlab.entitychurch.org\nghost.entitychurch.org\n"))
		return true
	})
	ctx := context.Background()

	set, err := reg.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(set.Names) != 4 {
		t.Errorf("the walk yielded %d names; the listing lie must not move it", len(set.Names))
	}
	if want := []string{"ghost.entitychurch.org"}; !equalStrings(set.AdvertisedOnly, want) {
		t.Errorf("AdvertisedOnly = %v, want %v — a name on the menu with no signed binding", set.AdvertisedOnly, want)
	}
	if want := []string{"docs.entitychurch.org"}; !equalStrings(set.CommittedOnly, want) {
		t.Errorf("CommittedOnly = %v, want %v — a name hidden from the menu that the root still commits to",
			set.CommittedOnly, want)
	}
	if set.Reconciled() {
		t.Error("Reconciled() is true over a listing that both invented and hid a name")
	}

	// And the hidden name still resolves through the walk, which is the
	// half a listing-driven browser loses.
	atIssue(reg, ctx, t, "docs.entitychurch.org")
	res, err := reg.ResolveIn(ctx, set, "docs.entitychurch.org")
	if err != nil {
		t.Fatalf("resolving the hidden name through the walk: %v", err)
	}
	if res.PeerID() != fedDocs {
		t.Errorf("docs.entitychurch.org → %s, want %s", res.PeerID(), fedDocs)
	}
}

// TestFedResolveEveryName walks the four names both ways and requires
// the two paths to agree.
//
// The pointer path is §6a.4's floor (one fetch, the DNS model); the walk
// path is §6a.3a's strengthening. They must produce the same binding for
// an honest origin — where they diverge, the origin is serving two trees
// and only one of them is signed.
func TestFedResolveEveryName(t *testing.T) {
	reg, _ := fedRegistry(t, nil)
	ctx := context.Background()

	set, err := reg.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}

	for name, wantPeer := range fedMapping {
		atIssue(reg, ctx, t, name)

		viaPointer, err := reg.Resolve(ctx, name)
		if err != nil {
			t.Fatalf("%s via the by-name pointer: %v", name, err)
		}
		viaWalk, err := reg.ResolveIn(ctx, set, name)
		if err != nil {
			t.Fatalf("%s via the signed key set: %v", name, err)
		}

		if viaPointer.BindingHash != viaWalk.BindingHash {
			t.Errorf("%s: pointer says binding %s, signed root says %s — two trees, one signature",
				name, viaPointer.BindingHash, viaWalk.BindingHash)
		}
		if viaPointer.PeerID() != wantPeer {
			t.Errorf("%s → %s, MAPPING.txt says %s", name, viaPointer.PeerID(), wantPeer)
		}
		if viaPointer.Source != fetch.SourcePointer || viaWalk.Source != fetch.SourceWalk {
			t.Errorf("%s: sources mislabelled (%s / %s)", name, viaPointer.Source, viaWalk.Source)
		}
		if viaPointer.TrustAnchor != "peer_issued:"+fedRegistryPeer {
			t.Errorf("%s: trust anchor %q", name, viaPointer.TrustAnchor)
		}
		if len(viaPointer.Transports()) == 0 {
			t.Errorf("%s: no transports — §6a.3 makes them a MUST, and without them the "+
				"resolution is a fact nobody can act on", name)
		}
		if viaPointer.ExpiresAt.Sub(viaPointer.IssuedAt) <= 0 {
			t.Errorf("%s: ttl is not positive", name)
		}

		// §6a.6, and the distinction the spec insists on: the walk
		// covers the revocation prefix here (their root's prefix is the
		// whole peer subtree), so this absence is one the *registry*
		// signed rather than one the origin merely served.
		if !viaWalk.Revocation.WalkCovered {
			t.Errorf("%s: revocation absence via the walk is not marked walk-covered, "+
				"but their published prefix %q covers it", name, set.Root.Data.Prefix)
		}
		if viaWalk.Revocation.Present {
			t.Errorf("%s: reported revoked against an emission that publishes no revocations", name)
		}
	}
}

// TestFedSubstitutedByNamePointerIsRefused is the §6a.4 association
// check, against the exact attack it was written for.
//
// The origin repoints one by-name file at a binding the registry
// *legitimately signed* — for a different name. Everything else passes:
// the body is content-addressed and intact, the signature verifies
// against the pinned key, it is unexpired and unrevoked. Only the field
// binding the answer to the question disagrees, and a resolver that does
// not compare it hands the caller `lab`'s peer-id when they asked for
// `docs`.
func TestFedSubstitutedByNamePointerIsRefused(t *testing.T) {
	swapped, err := os.ReadFile(filepath.Join(fedRoot, "registry", fedRegistryPeer,
		"system/registry/binding/by-name/lab.entitychurch.org.bin"))
	if err != nil {
		t.Fatalf("reading the binding pointer to substitute: %v", err)
	}
	reg, _ := fedRegistry(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/by-name/docs.entitychurch.org.bin") {
			return false
		}
		_, _ = w.Write(swapped)
		return true
	})
	ctx := context.Background()
	atIssue(reg, ctx, t, "entitychurch.org")

	res, err := reg.Resolve(ctx, "docs.entitychurch.org")
	if !errors.Is(err, fetch.ErrNameAssociation) {
		t.Fatalf("resolve of a substituted pointer returned (%v, %v); want ErrNameAssociation. "+
			"Every other check on this path passes — that is the whole point of the attack",
			res.PeerID(), err)
	}
	// The refusal is the gate; the populated result is the diagnostic
	// §6a.4 asks for. An operator staring at this needs to see *which*
	// binding answered — "issued for lab.entitychurch.org" is what turns
	// "the registry is broken" into "someone repointed a file".
	if res.Binding.Name != "lab.entitychurch.org" {
		t.Errorf("the refusal names binding %q; it should carry the substituted binding so an "+
			"operator can see what actually answered", res.Binding.Name)
	}
	if res.PeerID() != fedLab {
		t.Errorf("diagnostic target = %s, want the substituted %s", res.PeerID(), fedLab)
	}

	// The walk path is immune by construction rather than by check: the
	// binding hash came out of the signed key set, so there was no
	// host-chosen association to substitute. Asserted so a future
	// refactor that routes ResolveIn through the pointer is caught.
	set, err := reg.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate under the substituting origin: %v", err)
	}
	viaWalk, err := reg.ResolveIn(ctx, set, "docs.entitychurch.org")
	if err != nil {
		t.Fatalf("the walk path should be unaffected by a repointed by-name file: %v", err)
	}
	if viaWalk.PeerID() != fedDocs {
		t.Errorf("walk path resolved docs.* to %s, want %s", viaWalk.PeerID(), fedDocs)
	}
}

// TestFedWithheldRegistryNodeFailsClosed: enumeration over an origin
// that stops serving one interior node the signed root commits to.
//
// A best-effort traversal reports a *smaller registry* here and no
// error — the operator sees three names, believes that is what the
// registry carries, and nothing anywhere says otherwise. This is AP29 in
// its native habitat, one layer up from where we first met it.
func TestFedWithheldRegistryNodeFailsClosed(t *testing.T) {
	reg, _ := fedRegistry(t, nil)
	ctx := context.Background()
	set, err := reg.Enumerate(ctx)
	if err != nil {
		t.Fatalf("baseline Enumerate: %v", err)
	}
	if set.Walk.Nodes() == 0 {
		t.Fatal("no nodes to withhold")
	}
	withheld := set.Walk.NodeHashes[len(set.Walk.NodeHashes)-1]

	blocked, _ := fedRegistry(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, hexOf(withheld)) {
			http.NotFound(w, r)
			return true
		}
		return false
	})
	if _, err := blocked.Enumerate(ctx); err == nil {
		t.Fatalf("enumeration succeeded against an origin withholding CHAMP node %s — "+
			"a withheld node has to be a visibly incomplete answer, never a shorter one", withheld)
	} else if !errors.Is(err, fetch.ErrIncompleteWalk) {
		t.Errorf("withheld node produced %v, want ErrIncompleteWalk", err)
	}
}

// TestFedExpiredBindingIsRefused drives the one check on this path a
// hostile byte-server cannot influence.
//
// It cannot forge a signature, alter a body, or move our clock — so
// `issued_at + ttl` is the entire bound on a revocation it withholds
// (§6a.1a). A resolver that skips it converts "revocable" into
// "permanently trusted".
func TestFedExpiredBindingIsRefused(t *testing.T) {
	reg, _ := fedRegistry(t, nil)
	ctx := context.Background()

	reg.Now = func() time.Time { return time.UnixMilli(0) }
	res, err := reg.Resolve(ctx, "docs.entitychurch.org")
	if err != nil {
		t.Fatalf("baseline resolve: %v", err)
	}
	expires := res.ExpiresAt

	reg.Now = func() time.Time { return expires.Add(time.Millisecond) }
	if _, err := reg.Resolve(ctx, "docs.entitychurch.org"); !errors.Is(err, fetch.ErrNameExpired) {
		t.Fatalf("one millisecond past issued_at+ttl returned %v, want ErrNameExpired", err)
	}
	reg.Now = func() time.Time { return expires.Add(-time.Millisecond) }
	if _, err := reg.Resolve(ctx, "docs.entitychurch.org"); err != nil {
		t.Fatalf("one millisecond before expiry returned %v, want a resolution", err)
	}
}

// TestFedUnknownNameIsNotFound: the §6a.4 step-1 dead end, distinct from
// every trust failure above it.
func TestFedUnknownNameIsNotFound(t *testing.T) {
	reg, _ := fedRegistry(t, nil)
	ctx := context.Background()
	if _, err := reg.Resolve(ctx, "nobody.entitychurch.org"); !errors.Is(err, fetch.ErrNameNotFound) {
		t.Fatalf("unknown name returned %v, want ErrNameNotFound", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFedTheWholeJourney is the operation this session set out to make
// possible: **one pinned string in, a page out.**
//
//	pin the registry's peer-id
//	  → walk its signed root                     (what names exist)
//	  → resolve one                              (§6a.4, every require)
//	  → follow the binding's transport            (hop 2 begins)
//	  → verify THAT peer's signed root            (a different key)
//	  → walk it                                   (what the site contains)
//	  → read a page                               (bytes, hash-verified)
//
// Two keys, two signed roots, four verification steps between the name
// typed and the bytes shown, and **not one of them trusts the origin**.
// Every artifact here was emitted by `entity-browser-rust`; every check
// was run by Go. That is the claim `crossimpl-rust-site` could not make,
// because it started from an origin somebody had already told it.
func TestFedTheWholeJourney(t *testing.T) {
	reg, srv := fedRegistry(t, nil)
	ctx := context.Background()

	set, err := reg.Enumerate(ctx)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	atIssue(reg, ctx, t, "docs.entitychurch.org")
	res, err := reg.ResolveIn(ctx, set, "docs.entitychurch.org")
	if err != nil {
		t.Fatalf("resolve docs.entitychurch.org: %v", err)
	}

	// The transports came inline in the binding body — their shape, not
	// core-go's. Asserted so a fixture re-cut that moved to bare hashes
	// says so here rather than as an empty origin three lines down.
	if got := res.Transports(); len(got) == 0 || got[0].Kind != fetch.TransportInline {
		t.Fatalf("expected an inline transport; got %+v", got)
	}

	origin, err := reg.OriginFor(ctx, res, "")
	if err != nil {
		t.Fatalf("resolving the target's origin from its binding: %v", err)
	}
	if origin.Layout.PeerID != fedDocs {
		t.Fatalf("origin layout is for peer %s, the binding named %s", origin.Layout.PeerID, fedDocs)
	}
	if origin.Layout.Origin != fetch.OriginRoot(srv.URL) {
		t.Errorf("origin = %q, want the federation root %q — an origin-relative prefix resolves "+
			"against a host, not against the path the registry sat under",
			origin.Layout.Origin, fetch.OriginRoot(srv.URL))
	}

	// Hop 2. A DIFFERENT key: the docs domain signs its own root, and
	// the registry's signature has no standing over it.
	site := fetch.NewConsumer(origin.Layout, nil)
	root, err := site.VerifiedRoot(ctx)
	if err != nil {
		t.Fatalf("the target's signed root: %v", err)
	}
	if root.Data.PeerID != fedDocs {
		t.Errorf("target root names %s, want %s", root.Data.PeerID, fedDocs)
	}
	walk, err := site.Walk(ctx, root.Data.RootHash)
	if err != nil {
		t.Fatalf("walking the target's trie: %v", err)
	}

	// The key set IS the site's metadata (arch STATUS-2026-08-19-b §3):
	// nothing asked the publisher what it holds, we walked what it
	// signed. Find a SITE-convention page in it and read the bytes.
	var page string
	for _, k := range walk.Keys() {
		abs := fetch.AbsolutePath(root.Data.Prefix, fedDocs, k)
		if strings.Contains(abs, "/sites/") && strings.Contains(abs, "/pages/") {
			page = k
			break
		}
	}
	if page == "" {
		t.Fatalf("the docs domain's signed root commits to no SITE page; keys: %v", walk.Keys())
	}
	h, err := walk.Lookup(page)
	if err != nil {
		t.Fatalf("looking up %s in the committed set: %v", page, err)
	}
	ent, err := site.Blob(ctx, h)
	if err != nil {
		t.Fatalf("fetching page %s: %v", page, err)
	}
	if ent.Type != "app/site-page" {
		t.Errorf("page %s is type %q, want app/site-page", page, ent.Type)
	}
	if len(ent.Data) == 0 {
		t.Errorf("page %s verified but carries no body", page)
	}
	t.Logf("docs.entitychurch.org → %s → %s (%s, %d bytes) — two keys, two roots, zero trusted origins",
		fedDocs, page, ent.Type, len(ent.Data))
}
