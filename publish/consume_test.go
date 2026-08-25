package publish_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/fetch"
	"entity-workbench-go/publish"
)

// TestPublishThenFetch_TheTwoHalvesOfOurOwnCorridor is the direction
// this repo had never run: `publish` emits, `fetch` consumes, over a
// real HTTP server, with nothing shared between them but the emitted
// bytes.
//
// It was never run, and both halves were green the whole time. `publish`
// had a test asserting its emitted layout; `fetch` had a test asserting
// its constructed URLs; nothing asserted they were the SAME layout, and
// they were not — four ways (no `tree/` segment vs one, `sharded-2-4`
// vs `sharded-2-flat`, wire hex vs digest-only hex, Amendment 6 pointer
// vs raw 33 bytes). Two green halves of one corridor is not a working
// corridor, and it took another implementation asking us to consume
// them to notice it about ourselves.
//
// Tier: integration (real HTTP, real emitted directory, no fakes).
func TestPublishThenFetch_TheTwoHalvesOfOurOwnCorridor(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	seeds := map[string]map[string]string{
		"docs/index":       {"title": "Welcome", "body": "the corridor closes here"},
		"docs/deep/nested": {"title": "Nested", "body": "two segments down"},
	}
	for p, data := range seeds {
		if _, err := ap.Store().Put(p, "test/note", data); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	out := t.TempDir()

	// The origin has to be known before the publish, because it is
	// baked into the profile the consumer reads. A test server's URL
	// is only known after it starts, so start it over the (still
	// empty) directory first and publish into it.
	srv := httptest.NewServer(http.FileServer(http.Dir(out)))
	defer srv.Close()

	if _, err := publish.Publish(context.Background(), publish.Opts{
		Peer:      ap,
		Prefix:    "docs/",
		OutputDir: out,
		OriginURL: srv.URL,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ctx := context.Background()

	// Entry point: the origin and nothing else. No peer-id, no path
	// convention, no layout knowledge — the consumer learns all three
	// from what the publisher advertised.
	layout, err := fetch.LoadLayout(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout: %v", err)
	}
	if layout.PeerID != ap.PeerID() {
		t.Fatalf("profile advertises peer %s, we published as %s", layout.PeerID, ap.PeerID())
	}
	if layout.Endpoint.ContentLayout != types.ContentLayoutSharded24 {
		t.Errorf("content_layout = %q, want %q", layout.Endpoint.ContentLayout, types.ContentLayoutSharded24)
	}
	if layout.SignedPointer == "" {
		t.Error("profile advertises no signed_pointer, but this publish emits a signed root")
	}

	// The signed root, through manifest_url_prefix.
	root, err := fetch.SignedRoot(ctx, layout, nil)
	if err != nil {
		t.Fatalf("SignedRoot: %v", err)
	}
	if root.PeerID != ap.PeerID() {
		t.Errorf("signed root names peer %s, want %s", root.PeerID, ap.PeerID())
	}
	if root.PublishedAt == 0 {
		t.Error("signed root carries no published_at; a consumer has no moment to bound freshness by")
	}

	// Every authored page, over the advertised layout.
	for path, want := range seeds {
		res, err := fetch.Fetch(ctx, fetch.Opts{BaseURL: srv.URL, Path: path, Layout: &layout})
		if err != nil {
			t.Errorf("fetch %s: %v", path, err)
			continue
		}
		if res.Entity.Type != "test/note" {
			t.Errorf("%s: type = %q, want test/note", path, res.Entity.Type)
			continue
		}
		var got map[string]string
		if err := ecf.Decode(res.Entity.Data, &got); err != nil {
			t.Errorf("%s: decode data: %v", path, err)
			continue
		}
		if got["title"] != want["title"] || got["body"] != want["body"] {
			t.Errorf("%s: round-trip mismatch: got %v, want %v", path, got, want)
		}
	}

	// A path that was never published is ABSENT, not a transport
	// error. Across implementations the distinction is worth its own
	// assertion (browser-rust asserts the mirror of it): a consumer
	// that reports "the origin is down" for a key that simply is not
	// there sends an operator to the wrong machine.
	_, err = fetch.Fetch(ctx, fetch.Opts{BaseURL: srv.URL, Path: "docs/never-published", Layout: &layout})
	if err == nil {
		t.Error("an unpublished path resolved")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("an unpublished path failed as %v, want a 404 — absence and unreachability "+
			"must not report the same way", err)
	}

	// The peer-id cross-check: a caller naming a different publisher
	// is refused rather than quietly served this origin's bytes.
	_, err = fetch.Fetch(ctx, fetch.Opts{
		BaseURL: srv.URL,
		PeerID:  "2KsomeOtherPeerEntirely",
		Path:    "docs/index",
		Layout:  &layout,
	})
	if err == nil {
		t.Error("a fetch naming a different peer-id than the profile was served anyway")
	}
}

// TestFetch_EntersThroughTheProfileWithNothingButAnOrigin pins the
// cold-start path end to end — no Layout passed in, so the well-known
// object is the only thing the consumer is told.
//
// Tier: integration.
func TestFetch_EntersThroughTheProfileWithNothingButAnOrigin(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()
	if _, err := ap.Store().Put("docs/only", "test/note", map[string]string{"body": "cold start"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(out)))
	defer srv.Close()
	if _, err := publish.Publish(context.Background(), publish.Opts{
		Peer: ap, Prefix: "docs/", OutputDir: out, OriginURL: srv.URL,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	res, err := fetch.Fetch(context.Background(), fetch.Opts{BaseURL: srv.URL, Path: "docs/only"})
	if err != nil {
		t.Fatalf("cold-start fetch: %v", err)
	}
	if res.Layout.PeerID != ap.PeerID() {
		t.Errorf("resolved under peer %s, want %s", res.Layout.PeerID, ap.PeerID())
	}
	// The URLs the consumer actually used, asserted against the
	// publisher's emission rather than against a convention.
	if !strings.HasPrefix(res.TreeURL, srv.URL+"/"+ap.PeerID()+"/docs/only") {
		t.Errorf("tree URL %q is not the emitted peer-rooted leaf", res.TreeURL)
	}
	if !strings.HasPrefix(res.BlobURL, srv.URL+"/content/") {
		t.Errorf("blob URL %q is not under the advertised content prefix", res.BlobURL)
	}
}
