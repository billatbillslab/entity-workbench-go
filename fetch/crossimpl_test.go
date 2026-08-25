package fetch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/fetch"
)

// The frozen browser-rust emission — see testdata/crossimpl-rust-site/README.md
// for provenance (their `dev` @ fbc2c5c, clean tree, 2026-08-19).
const rustPeer = "2KEE55MMWBvXE8ozrm45kwTchzFaQP1dzBnU9rifGTdmUa"

func rustSite(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir("testdata/crossimpl-rust-site")))
	t.Cleanup(srv.Close)
	return srv
}

// TestConsumeBrowserRustSite is the direction neither arm had run: a Go
// consumer entering a Rust publisher's static emission with nothing but
// an origin.
//
// browser-rust asked for exactly this (ROUTING-2026-08-19-d §5) after
// closing the mirror of it — their shipped consumer over our origin. A
// signed root verified only by its own language's reader is
// cohort-consistent, not independent convergence (ADR-0012), and until
// this test ran, every claim either arm made about the other's bytes was
// a reading of source rather than a measurement (D19).
//
// Nothing here is derived: the peer-id, all three URL prefixes, the
// content layout and both suffixes come out of their profile. The one
// convention is the well-known object the profile itself sits at.
//
// Tier: integration (real HTTP over another implementation's bytes).
func TestConsumeBrowserRustSite(t *testing.T) {
	srv := rustSite(t)
	ctx := context.Background()

	layout, err := fetch.LoadLayout(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout over the Rust emission: %v", err)
	}
	if layout.PeerID != rustPeer {
		t.Fatalf("profile advertises peer %s, fixture is %s", layout.PeerID, rustPeer)
	}

	// Their layout, as they wrote it. Asserted because these are the
	// values the walk below depends on, and a fixture re-cut that
	// changed one of them should say so here rather than as a 404 four
	// assertions later.
	if got, want := layout.Endpoint.TreeURLPrefix, "/"+rustPeer; got != want {
		t.Errorf("tree_url_prefix = %q, want %q — their emission is PEER-ROOTED, which is the "+
			"join this consumer has to bridge", got, want)
	}
	if got, want := layout.Endpoint.ContentLayout, types.ContentLayoutSharded24; got != want {
		t.Errorf("content_layout = %q, want %q", got, want)
	}
	if layout.SignedPointer == "" {
		t.Error("their profile advertises no signed_pointer")
	}

	// The signed root, through their manifest_url_prefix — which is
	// NOT `{origin}/manifest` (ours) but the published-root's own tree
	// location. A consumer deriving the manifest URL by our convention
	// gets a 404 here; this one reads theirs.
	root, err := fetch.SignedRoot(ctx, layout, nil)
	if err != nil {
		t.Fatalf("SignedRoot over the Rust emission: %v", err)
	}
	if root.PeerID != rustPeer {
		t.Errorf("signed root names peer %s, want %s", root.PeerID, rustPeer)
	}
	if root.PublishedAt == 0 {
		t.Error("their signed root carries no published_at")
	}

	// Four authored entities across two of their sites, one of them two
	// path segments deep, each hash-verified against what their tree
	// bound.
	for _, tc := range []struct{ path, wantType string }{
		{"sites/demo/manifest", "app/site-manifest"},
		{"sites/demo/pages/index", "app/site-page"},
		{"sites/demo/pages/guide/intro", "app/site-page"},
		{"sites/entity-info/pages/why", "app/site-page"},
	} {
		res, err := fetch.Fetch(ctx, fetch.Opts{Path: tc.path, Layout: &layout})
		if err != nil {
			t.Errorf("fetch %s: %v", tc.path, err)
			continue
		}
		if res.Entity.Type != tc.wantType {
			t.Errorf("%s: type = %q, want %q", tc.path, res.Entity.Type, tc.wantType)
		}
		if len(res.Entity.Data) == 0 {
			t.Errorf("%s: resolved to an empty body", tc.path)
		}
	}

	// A key absent from their tree is ABSENT, not a transport failure.
	// The mirror of browser-rust's own assertion against our origin: a
	// consumer that reports "the origin is down" for a key that was
	// never published sends an operator to the wrong machine.
	if _, err := fetch.Fetch(ctx, fetch.Opts{Path: "sites/demo/pages/not-a-page", Layout: &layout}); err == nil {
		t.Error("an unpublished path resolved")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("an unpublished path failed as %v, want a 404", err)
	}
}

// TestTreeBaseBridgesBothJoins pins the `tree_url_prefix` bridge on
// both live emissions at once, because the two arms of this cohort emit
// opposite joins and the spec's own section supports both readings
// (browser-rust ROUTING-2026-08-19-d §4 — arch's to rule, ours to
// survive in the meantime).
//
// The discriminator is exactness: the prefix's LAST segment being the
// peer means the prefix is peer-rooted and complete. Not `Contains` — a
// peer-id appearing in a CDN path or a bucket name is not that claim.
// And the empty-head case is legal: `/{peer_id}` is browser-rust's
// same-origin form, and requiring a non-empty head refuses it (their
// audit F6 did, and it cost them seven gates).
//
// Tier: contract pin.
func TestTreeBaseBridgesBothJoins(t *testing.T) {
	const origin = "https://cdn.example"
	for _, tc := range []struct {
		name, prefix, want string
	}{
		{
			name:   "peer-rooted, absolute (browser-rust with an origin)",
			prefix: origin + "/" + rustPeer,
			want:   origin + "/" + rustPeer + "/sites/demo/pages/index.bin",
		},
		{
			name:   "peer-rooted, origin-relative with an EMPTY head (browser-rust same-origin)",
			prefix: "/" + rustPeer,
			want:   origin + "/" + rustPeer + "/sites/demo/pages/index.bin",
		},
		{
			name:   "origin-rooted (ours) — the consumer appends the peer",
			prefix: origin,
			want:   origin + "/" + rustPeer + "/sites/demo/pages/index.bin",
		},
		{
			name:   "origin-rooted under a path — still appends",
			prefix: origin + "/mirrors/entity",
			want:   origin + "/mirrors/entity/" + rustPeer + "/sites/demo/pages/index.bin",
		},
		{
			name:   "the peer-id appears mid-prefix, not last — NOT peer-rooted",
			prefix: origin + "/" + rustPeer + "/mirror",
			want:   origin + "/" + rustPeer + "/mirror/" + rustPeer + "/sites/demo/pages/index.bin",
		},
	} {
		l, err := fetch.LayoutFromProfile(origin, types.HTTPPollProfileData{
			PeerID: rustPeer,
			Endpoint: types.TransportEndpoint{
				TreeURLPrefix:    tc.prefix,
				ContentURLPrefix: "/content",
				ContentLayout:    types.ContentLayoutSharded24,
			},
		})
		if err != nil {
			t.Errorf("%s: LayoutFromProfile: %v", tc.name, err)
			continue
		}
		if got := l.TreeLeafURL("sites/demo/pages/index"); got != tc.want {
			t.Errorf("%s:\n  got  %s\n  want %s", tc.name, got, tc.want)
		}
	}
}

// TestContentURLUsesWireHexNotDigestHex pins the hex convention against
// the real fixture, because it is a MUST that two implementations get
// right and the kernel helper gets wrong.
//
// EXTENSION-NETWORK §6.5.3.1: "Hash hex includes the format-code byte …
// NOT the 64-char digest-only hex" — 66 chars beginning `00` under
// ECFv1-SHA-256. core-go's types.BuildContentURL hexes EffectiveDigest()
// (64 chars, no format byte), which resolves against neither publisher.
// If this test ever starts failing because we delegated to it, the
// kernel fix landed and the delegation needs the round trip re-measured,
// not the assertion relaxed.
//
// Tier: contract pin (cross-impl).
func TestContentURLUsesWireHexNotDigestHex(t *testing.T) {
	srv := rustSite(t)
	ctx := context.Background()
	layout, err := fetch.LoadLayout(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout: %v", err)
	}
	res, err := fetch.Fetch(ctx, fetch.Opts{Path: "sites/demo/pages/index", Layout: &layout})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	last := res.BlobURL[strings.LastIndex(res.BlobURL, "/")+1:]
	if len(last) != 66 {
		t.Errorf("content URL leaf %q is %d hex chars, want 66 (33-byte wire form)", last, len(last))
	}
	if !strings.HasPrefix(last, "00") {
		t.Errorf("content URL leaf %q does not carry the ECFv1-SHA-256 format byte", last)
	}
	kernel, err := types.BuildContentURL("/content", types.ContentLayoutSharded24, res.Hash)
	if err != nil {
		t.Fatalf("BuildContentURL: %v", err)
	}
	if strings.HasSuffix(kernel, last) {
		t.Log("core-go's BuildContentURL now agrees with the emitted layout — the routed fix " +
			"landed; fetch.contentURL can become a delegation once the round trip is re-measured")
	}
}
