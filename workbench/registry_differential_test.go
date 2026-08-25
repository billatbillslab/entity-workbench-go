package workbench_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/httplive"
	"go.entitychurch.org/entity-core-go/ext/registry/peerissued"

	"entity-workbench-go/fetch"
)

// registry_differential_test.go — **two §6a.4 resolvers, the same bytes,
// and the place their verdicts are required to match.**
//
// There are two peer-issued resolvers in reach of this repo:
//
//	entity-core-go  ext/registry/peerissued.Backend  — needs a peer
//	                (a handler.HandlerContext with a store + a location
//	                index, because §6a.4's precede path caches into them)
//	entity-workbench-go  fetch.Registry              — needs nothing
//	                (entity-fetch links no peer, no store, no sqlite)
//
// We use the kernel's wherever a peer exists (`entitysdk/registry_pin.go`)
// and ours where one does not. That split is defensible right up until
// the two disagree about a binding — at which point the same name
// resolves differently depending on which surface a user opened, and
// nothing says so. This file is the enforcement point for the split. A
// discipline with no enforcement point is theater.
//
// **It lives in `workbench` for a structural reason worth stating:**
// the module graph runs fetch → entitysdk, so entitysdk cannot import
// fetch. `workbench` is the one module that already sees both, and
// putting the test here keeps `fetch`'s module requirements free of
// `ext` — the dependency `entity-fetch` exists to avoid.
//
// # What it currently measures: a live divergence, not agreement
//
// Run against `entity-browser-rust`'s frozen federation, **core-go's
// backend cannot resolve a single name.** It fails inside its own
// binding decode:
//
//	peerissued: decode binding: hash unmarshal:
//	cbor: cannot unmarshal map into Go value of type []uint8
//
// The cause is `transports`, which EXTENSION-REGISTRY §3 declares as
// `[<endpoint per NETWORK §6.5>]`. `entity-core-rust` reads it as
// `Vec<Value>` (opaque, passed through) and their emitter writes whole
// http-poll profiles inline; core-go's `types.BindingData` fixes it to
// `[]hash.Hash`. So a Go peer pinning a Rust registry gets a CBOR error
// rather than a resolution — for every name, on the only live federation
// in this cohort.
//
// Routed as `reviews/REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21.md`.
// The tests below pin the divergence as *measured state*: they fail if
// it widens AND they fail if it silently closes, because the day
// core-go can decode these bindings is the day this file goes back to
// asserting full agreement and the packet gets closed.
//
// Tier: integration + cross-implementation differential.

const (
	diffRegistryPeer = "2KBLkCxvkgobuauPA6zPfKarpuRRnnWHL98n8Gv1GNmybr"
	diffFixtureRoot  = "../fetch/testdata/crossimpl-rust-federation"
)

var diffNames = map[string]string{
	"entitychurch.org":          "2KGTrr4LxfFJjUBUD74XJZoze74TrQFpXqWA19qeskz9sS",
	"protocol.entitychurch.org": "2KAdu6wwTNAoQiqXmN93vbjHxk3QosG7hxhXGZtN8wZF31",
	"docs.entitychurch.org":     "2KFRBJ9feEPCZZiNaCEKsCAVkGkp1htWZk9a8jz5n5D2sS",
	"lab.entitychurch.org":      "2KEb7HgmoCRF1VpNeCYusiubnn94ke4uK1hUxBcQ1PTtNA",
}

func diffServer(t *testing.T, hook func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	files := http.FileServer(http.Dir(diffFixtureRoot))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil && hook(w, r) {
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// kernelBackend builds core-go's peer-issued backend against the
// fixture, with a memory store standing in for a peer's.
//
// The memory store is the whole reason `fetch.Registry` exists: it is
// enough to satisfy the `HandlerContext` the backend requires, and
// `core/store` is the package `sqlite.go` lives in — the dependency
// `entity-fetch` refuses to link. Here it costs nothing.
func kernelBackend(t *testing.T, origin string) (*peerissued.Backend, *handler.HandlerContext) {
	t.Helper()

	registryPeer, err := diffIdentityEntity(diffRegistryPeer)
	if err != nil {
		t.Fatalf("registry identity entity: %v", err)
	}
	profile := types.HTTPPollProfileData{
		PeerID:        diffRegistryPeer,
		TransportType: "http-poll",
		Endpoint: types.TransportEndpoint{
			// **The peer-id is absent here and present in `ourRegistry`
			// below, and that is not a typo.**
			//
			// core-go's `httplive.Outbound.treeLeafURL` builds
			// `{tree_url_prefix}/{peer_id}/{path}{suffix}` — appending
			// the peer segment unconditionally. `fetch.Layout` reads the
			// same field the other way for one of the two live shapes:
			// when the prefix's LAST SEGMENT IS the peer-id it is
			// already peer-rooted and nothing is appended (AP30 —
			// `entity-browser-rust`'s emitted shape; our own publisher
			// emits the bare-origin shape core-go assumes).
			//
			// So one advertised profile means two different URLs to the
			// two readers. That ambiguity is already routed — arch owns
			// the §6.5.3.1 Amendment 5 ruling that will kill one branch —
			// and this test deliberately does not re-litigate it: each
			// resolver is handed the prefix in the form it reads, so what
			// is measured below is the §6a.4 CHECKS and not a URL join
			// with an open ruling over it.
			TreeURLPrefix:     origin + "/registry",
			ContentURLPrefix:  origin + "/registry/content",
			ManifestURLPrefix: origin + "/registry/" + diffRegistryPeer + "/system/peer/published-root",
			ContentLayout:     types.ContentLayoutSharded24,
			TreeLeafSuffix:    ".bin",
			TreeListingSuffix: ".list",
		},
		SupportedOps: []string{types.OpTreeGet, types.OpContentGet, types.OpManifestGet},
	}
	out := httplive.NewOutbound(profile,
		httplive.WithPinnedIdentity(registryPeer),
		httplive.WithOutboundAllowHTTP(true))

	be, err := peerissued.New(registryPeer, diffRegistryPeer,
		peerissued.NewHTTPPollReader(out, diffRegistryPeer),
		// Both resolvers get the SAME clock. A differential test whose
		// two halves disagree about the time is measuring the clock.
		peerissued.WithClock(func() uint64 { return 0 }),
		// No precede caching: a cache would make the second resolve of a
		// name read the store instead of the wire, so a substitution
		// introduced between two calls would be invisible.
		peerissued.WithCacheOnResolve(false),
	)
	if err != nil {
		t.Fatalf("peerissued.New: %v", err)
	}
	return be, &handler.HandlerContext{
		Store:         store.NewMemoryContentStore(),
		LocationIndex: store.NewMemoryLocationIndex(),
	}
}

// kernelVerdict collapses the kernel backend's answer into the three
// states this file cares about.
type kernelVerdict struct {
	resolved bool
	peerID   string
	// foundPointer is true when step 1 located a by-name pointer at all
	// — the distinction between "no such name" and "found it and then
	// something went wrong". It is what makes the normalization test
	// possible while the decode divergence stands.
	foundPointer bool
	err          error
}

func askKernel(be *peerissued.Backend, hctx *handler.HandlerContext, name string) kernelVerdict {
	res, err := be.Resolve(hctx, name, nil)
	v := kernelVerdict{err: err}
	if err != nil {
		// Every post-lookup failure is an error return; a name that is
		// simply absent is a clean not_found result instead.
		v.foundPointer = true
		return v
	}
	v.resolved = res.Status == types.ResolutionStatusResolved
	v.peerID = res.PeerID
	v.foundPointer = v.resolved
	return v
}

// TestOurResolverResolvesEveryRustName is our side of the contract,
// stated on its own so the divergence test below cannot hide a
// regression in ours behind a known failure in theirs.
func TestOurResolverResolvesEveryRustName(t *testing.T) {
	srv := diffServer(t, nil)
	ours := ourRegistry(t, srv.URL)

	for name, wantPeer := range diffNames {
		got, err := ours.Resolve(context.Background(), name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got.PeerID() != wantPeer {
			t.Errorf("%s → %s, MAPPING.txt says %s", name, got.PeerID(), wantPeer)
		}
		if got.TrustAnchor != types.PeerIssuedTrustAnchor(diffRegistryPeer) {
			t.Errorf("%s: trust anchor %q", name, got.TrustAnchor)
		}
	}
}

// TestKernelCannotDecodeARustBinding pins the divergence.
//
// **This test failing is good news in one direction and bad in the
// other, and it says which.** If core-go starts resolving these
// bindings, the packet is answered: delete this test, restore full
// agreement assertions, and close
// `reviews/REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21.md`. If it
// starts failing for a *different* reason, the divergence moved and the
// packet is stale.
func TestKernelCannotDecodeARustBinding(t *testing.T) {
	srv := diffServer(t, nil)
	be, hctx := kernelBackend(t, srv.URL)

	for name := range diffNames {
		v := askKernel(be, hctx, name)
		if v.resolved {
			t.Fatalf("core-go RESOLVED %s → %s. The transports divergence has closed: restore the "+
				"full agreement assertions in this file, delete this test, and close "+
				"reviews/REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21.md", name, v.peerID)
		}
		if v.err == nil {
			t.Fatalf("%s: core-go returned a clean not_found rather than a decode failure — the "+
				"divergence has MOVED (it used to fail inside the binding decode); re-measure "+
				"before trusting the packet", name)
		}
		if !strings.Contains(v.err.Error(), "decode binding") {
			t.Errorf("%s: core-go failed as %q; the pinned divergence is a `decode binding` "+
				"failure on `transports`", name, v.err)
		}
		// Step 1 DID find the pointer — the failure is the body, not the
		// name. Stated because "core-go can't find these names" and
		// "core-go can't read these bindings" are different bug reports
		// and only the second one is true.
		if !v.foundPointer {
			t.Errorf("%s: core-go did not locate the by-name pointer at all", name)
		}
	}
}

// TestBothResolversRefuseASubstitutedBinding — the negative half.
//
// Agreement on an honest origin is cheap: a resolver that skips every
// check agrees too. What has to match is the REFUSAL.
//
// **And there is a caveat this test states rather than hides.** While
// the decode divergence stands, core-go refuses this substitution for
// the wrong reason — it refuses *everything* from this registry — so its
// half of this assertion is currently vacuous. Ours is not: it is
// checked against the specific §6a.4 association failure. When the
// divergence closes, core-go's half becomes meaningful and this comment
// comes out.
func TestBothResolversRefuseASubstitutedBinding(t *testing.T) {
	swapped := filepath.Join(diffFixtureRoot, "registry", diffRegistryPeer,
		"system", "registry", "binding", "by-name", "lab.entitychurch.org.bin")
	srv := diffServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/by-name/docs.entitychurch.org.bin") {
			return false
		}
		http.ServeFile(w, r, swapped)
		return true
	})
	be, hctx := kernelBackend(t, srv.URL)
	ours := ourRegistry(t, srv.URL)

	kernel := askKernel(be, hctx, "docs.entitychurch.org")
	if kernel.resolved {
		t.Errorf("core-go RESOLVED a substituted binding to %s — the association check "+
			"(§6a.4, CAP registry F1/D1) did not fire", kernel.peerID)
	}

	got, err := ours.Resolve(context.Background(), "docs.entitychurch.org")
	if err == nil {
		t.Fatalf("ours resolved a substituted binding to %s", got.PeerID())
	}
	if !strings.Contains(err.Error(), "association") {
		t.Errorf("ours refused for %q; the refusal has to be the association check specifically, "+
			"because every other check on this path passes", err)
	}
	if kernel.resolved != (err == nil) {
		t.Fatalf("THE TWO RESOLVERS DISAGREE on a substitution: core-go resolved=%v, ours resolved=%v",
			kernel.resolved, err == nil)
	}
}

// TestBothResolversRefuseAnUnknownName pins the one shape where §6a.4
// distinguishes "no such name" from a trust failure, and requires both
// to reach it. This one is NOT vacuous under the divergence: an absent
// name never reaches the binding decode.
func TestBothResolversRefuseAnUnknownName(t *testing.T) {
	srv := diffServer(t, nil)
	be, hctx := kernelBackend(t, srv.URL)
	ours := ourRegistry(t, srv.URL)

	kernel := askKernel(be, hctx, "nobody.entitychurch.org")
	if kernel.err != nil {
		t.Fatalf("core-go errored on an absent name instead of reporting not_found: %v", kernel.err)
	}
	if kernel.resolved {
		t.Fatalf("core-go resolved a name the registry does not carry, to %s", kernel.peerID)
	}
	if kernel.foundPointer {
		t.Error("core-go reports having located a by-name pointer for a name that does not exist")
	}
	if _, err := ours.Resolve(context.Background(), "nobody.entitychurch.org"); err == nil {
		t.Error("ours resolved a name the registry does not carry")
	}
}

// TestNameNormalizationMatchesTheKernel.
//
// `fetch.NormalizeName` is a transcription of the kernel's UNEXPORTED
// `normalizeName`, and canonicalization that selects a tree path is a
// Layer-2 algorithm: two impls that disagree resolve differently for the
// same input, and the disagreement surfaces as a 404 rather than as a
// mismatch — which is to say, as the other side's defect.
//
// The comparison is on **whether each resolver located a by-name
// pointer**, which is exactly the step normalization controls and the
// one signal still available on both sides while the decode divergence
// stands. An ask to export the kernel's function went back to core-go
// rather than being noted and forgotten; until then this is the pin.
func TestNameNormalizationMatchesTheKernel(t *testing.T) {
	srv := diffServer(t, nil)
	be, hctx := kernelBackend(t, srv.URL)
	ours := ourRegistry(t, srv.URL)

	for _, tc := range []struct {
		name       string
		wantLocate bool
	}{
		{"docs.entitychurch.org", true},
		{"lab.entitychurch.org", true},
		{"nobody.entitychurch.org", false},
		// §6.3 name-path safety: a name is one path segment. Both must
		// refuse to turn this into a lookup.
		{"docs/entitychurch.org", false},
		// Case is NOT folded by either (§6a.4 normalizes with NFC only;
		// a registry wanting case-insensitivity folds before it authors
		// the pointer).
		{"DOCS.entitychurch.org", false},
	} {
		kernel := askKernel(be, hctx, tc.name)
		if kernel.foundPointer != tc.wantLocate {
			t.Errorf("%q: core-go located=%v, want %v (err=%v)",
				tc.name, kernel.foundPointer, tc.wantLocate, kernel.err)
		}

		_, err := ours.Resolve(context.Background(), tc.name)
		// "Located" means step 1 found a by-name pointer. Every way of
		// not locating one — an absent name, a name §6.3 refuses to
		// route — is ErrNameNotFound on our side, which is §6a.4's own
		// instruction and what makes this comparable to the kernel's
		// not_found at all.
		ourLocate := !errors.Is(err, fetch.ErrNameNotFound)
		if ourLocate != tc.wantLocate {
			t.Errorf("%q: ours located=%v, want %v (err=%v)", tc.name, ourLocate, tc.wantLocate, err)
		}
		if ourLocate != kernel.foundPointer {
			t.Errorf("%q: NORMALIZATION DIVERGES — core-go located=%v, ours located=%v",
				tc.name, kernel.foundPointer, ourLocate)
		}
	}
}

// diffIdentityEntity mirrors entitysdk's construction of a pinned
// registry's identity entity: for the v1 identity-multihash form the
// peer-id CARRIES the key, so this is a decode, not a fetch.
func diffIdentityEntity(peerID string) (entity.Entity, error) {
	pub, keyType, ok := crypto.DerivePeerFromPeerID(crypto.PeerID(peerID))
	if !ok {
		return entity.Entity{}, fmt.Errorf("peer-id %s is not identity-multihash form", peerID)
	}
	return types.PeerData{PublicKey: pub, KeyType: crypto.KeyTypeString(keyType)}.ToEntity()
}

// ourRegistry builds fetch.Registry against the same origin, with the
// SAME clock the kernel backend gets.
func ourRegistry(t *testing.T, origin string) *fetch.Registry {
	t.Helper()
	layout, err := fetch.PinnedLayout(origin, diffRegistryPeer, types.TransportEndpoint{
		TreeURLPrefix:     "/registry/" + diffRegistryPeer,
		ContentURLPrefix:  "/registry/content",
		ManifestURLPrefix: "/registry/" + diffRegistryPeer + "/system/peer/published-root",
		ContentLayout:     types.ContentLayoutSharded24,
		TreeLeafSuffix:    ".bin",
		TreeListingSuffix: ".list",
	})
	if err != nil {
		t.Fatalf("PinnedLayout: %v", err)
	}
	reg, err := fetch.NewRegistry(layout, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg.Now = func() time.Time { return time.UnixMilli(0) }
	return reg
}
