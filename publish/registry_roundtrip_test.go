package publish_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/fetch"
	"entity-workbench-go/publish"
)

// registry_roundtrip_test.go — **we can be a registry, and our own
// consumer can browse it.**
//
// Every other test in this repo's naming stack consumes another
// implementation's bytes. This one closes the loop on our own side:
// mint a name with `AppPeer.IssueBinding` (§6a.8 curated registration),
// publish `system/registry/` as a static coral reef, serve it over HTTP,
// and drive `fetch.Registry` at it holding nothing but the peer-id.
//
// **It is the weaker evidence of the two and says so.** Our emitter and
// our consumer share a repo, a language and an author, so agreement here
// is cohort-consistency at best (ADR-0012) — what it proves is that our
// *emitter* produces the artifacts §6a describes, which is exactly the
// claim the cross-impl fixture cannot make about us. The two together
// are the pair: `fetch/registry_test.go` proves we can read theirs, this
// proves ours is readable.
//
// Tier: integration (real HTTP over our own emission).

// newRegistryPeer is an in-memory peer standing in for an operator's.
func newRegistryPeer(t *testing.T) *entitysdk.AppPeer {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	return ap
}

func TestIssueThenPublishThenBrowseOurOwnRegistry(t *testing.T) {
	ctx := context.Background()
	registry := newRegistryPeer(t)
	target := newRegistryPeer(t)

	// The target's advertised reach, as a registry would learn it: a
	// profile the TARGET published about itself, not one the registry
	// composed. A registry asserting a reach it read is asserting
	// something it checked.
	targetProfile := types.HTTPPollProfileData{
		PeerID:        target.PeerID(),
		TransportType: "http-poll",
		Endpoint: types.TransportEndpoint{
			TreeURLPrefix:     "http://target.invalid/" + target.PeerID(),
			ContentURLPrefix:  "http://target.invalid/content",
			ManifestURLPrefix: "http://target.invalid/manifest",
			ContentLayout:     types.ContentLayoutSharded24,
			TreeLeafSuffix:    ".bin",
			TreeListingSuffix: ".list",
		},
		SupportedOps: []string{types.OpTreeGet, types.OpContentGet, types.OpManifestGet},
	}

	// `At` is pinned for AP18's reason: a fresh clock per run moves the
	// body hash, and with it the signature path and the by-name target,
	// so the emission would be unfreezable and this test's assertions
	// would drift.
	issuedAt := time.UnixMilli(1_780_000_000_000).UTC()
	issued, err := registry.IssueBinding(entitysdk.IssueOpts{
		Name:         "example.test",
		TargetPeerID: target.PeerID(),
		Transports:   []types.HTTPPollProfileData{targetProfile},
		TTL:          30 * 24 * time.Hour,
		At:           issuedAt,
	})
	if err != nil {
		t.Fatalf("IssueBinding: %v", err)
	}
	if issued.TransportShape != "inline" {
		t.Errorf("TransportShape = %q; the emitter writes inline and must report it", issued.TransportShape)
	}

	// Publish the registry subtree. This is §7.4's "the registry is
	// itself a coral reef" — no live peer at the far end, just files.
	// The server comes up FIRST, because the emission has to carry the
	// origin it will actually be served from: http.FileServer reads from
	// disk per request, so publishing into a directory it is already
	// serving is fine, and it is the only way the profile's absolute
	// prefixes can name the real host.
	out := t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(out)))
	t.Cleanup(srv.Close)

	// **Prefix `system/`, not `system/registry/`, and that is a finding
	// rather than a preference.** A binding's signature lives at
	// `system/signature/{hex}` (V7 §5.2's invariant pointer), which is
	// OUTSIDE `system/registry/` — so a registry published at the
	// narrower prefix emits its bindings and not the signatures over
	// them, and every §6a.4 verification 404s at step 3. Measured, with
	// its own control, in TestNarrowRegistryPrefixOmitsTheSignatures.
	if _, err := publish.Publish(ctx, publish.Opts{
		Peer:      registry,
		Prefix:    "system/",
		OutputDir: out,
		OriginURL: srv.URL,
		At:        issuedAt,
	}); err != nil {
		t.Fatalf("publish the registry subtree: %v", err)
	}

	// A consumer holding ONLY the registry's peer-id. The layout is
	// discovered here — our publisher emits a transport-profile, unlike
	// browser-rust's registry, so this arm exercises the discovered path
	// that the cross-impl fixture's registry cannot.
	layout, err := fetch.LoadLayout(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout over our own registry emission: %v", err)
	}
	if layout.PeerID != registry.PeerID() {
		t.Fatalf("profile advertises %s, the registry is %s", layout.PeerID, registry.PeerID())
	}
	reg, err := fetch.NewRegistry(layout, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg.Now = func() time.Time { return issuedAt.Add(time.Hour) }

	// §6a.3a — the walk.
	set, err := reg.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate our own registry: %v", err)
	}
	if got := set.NameStrings(); len(got) != 1 || got[0] != "example.test" {
		t.Fatalf("walk committed %v, want [example.test]", got)
	}
	if set.Names[0].BindingHash != issued.BindingHash {
		t.Errorf("the walk committed binding %s; we issued %s",
			set.Names[0].BindingHash, issued.BindingHash)
	}

	// The served menu agrees with the signed key set. **This is the arm
	// that catches a listing-FORMAT mistake**, which is a different bug
	// from a listing that lies: our publisher emits the spec's
	// `system/tree/listing` entity and `entity-browser-rust` emits
	// newline text, and a consumer that assumes one silently parses the
	// other into binary fragments — which then "disagree" with the walk
	// and raise a false alarm about the other side's honesty. Measured
	// 2026-08-21, against our own emission, from the shell.
	if set.ListingErr != nil {
		t.Errorf("our own by-name listing did not parse: %v", set.ListingErr)
	}
	if !set.Reconciled() {
		t.Errorf("our own menu disagrees with our own signed key set: advertised-only %v, "+
			"committed-only %v", set.AdvertisedOnly, set.CommittedOnly)
	}

	// §6a.4 — both paths, and they must agree.
	viaWalk, err := reg.ResolveIn(ctx, set, "example.test")
	if err != nil {
		t.Fatalf("resolve through the signed key set: %v", err)
	}
	viaPointer, err := reg.Resolve(ctx, "example.test")
	if err != nil {
		t.Fatalf("resolve through the by-name pointer: %v", err)
	}
	if viaWalk.BindingHash != viaPointer.BindingHash {
		t.Errorf("the two paths disagree: walk %s, pointer %s",
			viaWalk.BindingHash, viaPointer.BindingHash)
	}
	if viaWalk.PeerID() != target.PeerID() {
		t.Errorf("example.test → %s, want %s", viaWalk.PeerID(), target.PeerID())
	}

	// The transport survived the round trip as an inline profile, and it
	// is the one the target advertised — not a composed one.
	trs := viaWalk.Transports()
	if len(trs) != 1 || trs[0].Kind != fetch.TransportInline {
		t.Fatalf("transports = %+v, want one inline entry", trs)
	}
	origin, err := reg.OriginFor(ctx, viaWalk, "")
	if err != nil {
		t.Fatalf("OriginFor: %v", err)
	}
	if origin.Layout.PeerID != target.PeerID() {
		t.Errorf("the resolved origin is for %s, want %s", origin.Layout.PeerID, target.PeerID())
	}
	if origin.Layout.Endpoint.TreeURLPrefix != targetProfile.Endpoint.TreeURLPrefix {
		t.Errorf("tree prefix round-tripped as %q, target advertised %q",
			origin.Layout.Endpoint.TreeURLPrefix, targetProfile.Endpoint.TreeURLPrefix)
	}

	// Freshness, from inside the signed body.
	if !viaWalk.IssuedAt.Equal(issuedAt) {
		t.Errorf("issued_at round-tripped as %s, want %s", viaWalk.IssuedAt, issuedAt)
	}
}

// TestOurRegistryRefusesTheThingsSpecCallsMUSTs — the emitter's own
// negative controls.
//
// §6a.3's two MUSTs are on the ISSUE side, and an emitter that quietly
// defaults them is how an unrevokable binding gets published. Each of
// these is a refusal, not a default.
func TestOurRegistryRefusesTheThingsSpecCallsMUSTs(t *testing.T) {
	registry := newRegistryPeer(t)
	target := newRegistryPeer(t)
	prof := types.HTTPPollProfileData{
		PeerID:        target.PeerID(),
		TransportType: "http-poll",
		Endpoint: types.TransportEndpoint{
			TreeURLPrefix:    "http://t.invalid/" + target.PeerID(),
			ContentURLPrefix: "http://t.invalid/content",
			ContentLayout:    types.ContentLayoutSharded24,
		},
		SupportedOps: []string{types.OpTreeGet},
	}
	base := entitysdk.IssueOpts{
		Name: "a.test", TargetPeerID: target.PeerID(),
		Transports: []types.HTTPPollProfileData{prof}, TTL: time.Hour,
	}

	t.Run("null ttl", func(t *testing.T) {
		o := base
		o.TTL = 0
		if _, err := registry.IssueBinding(o); err == nil {
			t.Fatal("issued a peer-issued binding with no ttl — §6a.3 makes it permanently unrevokable")
		}
	})
	t.Run("no transports", func(t *testing.T) {
		o := base
		o.Transports = nil
		if _, err := registry.IssueBinding(o); err == nil {
			t.Fatal("issued a binding with no transports — a target nobody can reach")
		}
	})
	t.Run("name with a slash", func(t *testing.T) {
		o := base
		o.Name = "a/b.test"
		if _, err := registry.IssueBinding(o); err == nil {
			t.Fatal("issued a binding whose name is two path segments (§6.3)")
		}
	})
	t.Run("issuing is deterministic", func(t *testing.T) {
		// The inline shape cannot go through `types.BindingData` (its
		// Transports is `[]hash.Hash`), so the body is encoded from a
		// map — and a map encoded with a NON-deterministic CBOR mode
		// hashes differently run to run. Nothing local would notice:
		// our own verification hashes what we wrote. This is the pin
		// that `ecf.Encode`'s CoreDet mode is the one being used.
		o := base
		o.At = time.UnixMilli(1_780_000_000_000)
		first, err := registry.IssueBinding(o)
		if err != nil {
			t.Fatalf("first issue: %v", err)
		}
		second, err := registry.IssueBinding(o)
		if err != nil {
			t.Fatalf("second issue: %v", err)
		}
		if first.BindingHash != second.BindingHash {
			t.Errorf("two identical issues produced different hashes:\n  %s\n  %s\n"+
				"the body encoding is not deterministic, so no other implementation "+
				"reproduces our content addresses", first.BindingHash, second.BindingHash)
		}
	})
	t.Run("by-hash transports are refused with the reason", func(t *testing.T) {
		o := base
		o.TransportsByHash = true
		_, err := registry.IssueBinding(o)
		if err == nil {
			t.Fatal("emitted the by-hash shape, whose serving path §3 does not specify")
		}
		if !strings.Contains(err.Error(), "does not say where") {
			t.Errorf("the refusal does not name what is missing: %v", err)
		}
	})
}

// TestRevocationIsPublishedAndTheConsumerRefuses.
//
// And the part the test has to be honest about: publishing a revocation
// is not the same as revoking. This proves our emitter writes an artifact
// our consumer honours **when the origin serves it** — which is the only
// case a revocation ever helps in, because a hostile origin withholds it
// and the bound reverts to the ttl (§6a.1a).
func TestRevocationIsPublishedAndTheConsumerRefuses(t *testing.T) {
	ctx := context.Background()
	registry := newRegistryPeer(t)
	target := newRegistryPeer(t)
	at := time.UnixMilli(1_780_000_000_000).UTC()

	issued, err := registry.IssueBinding(entitysdk.IssueOpts{
		Name:         "gone.test",
		TargetPeerID: target.PeerID(),
		Transports: []types.HTTPPollProfileData{{
			PeerID: target.PeerID(), TransportType: "http-poll",
			Endpoint: types.TransportEndpoint{
				TreeURLPrefix:    "http://t.invalid/" + target.PeerID(),
				ContentURLPrefix: "http://t.invalid/content",
				ContentLayout:    types.ContentLayoutSharded24,
			},
			SupportedOps: []string{types.OpTreeGet},
		}},
		TTL: 30 * 24 * time.Hour,
		At:  at,
	})
	if err != nil {
		t.Fatalf("IssueBinding: %v", err)
	}
	if _, err := registry.RevokeBinding(issued.BindingHash, "key rotation", at.Add(time.Minute)); err != nil {
		t.Fatalf("RevokeBinding: %v", err)
	}

	out := t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(out)))
	t.Cleanup(srv.Close)
	if _, err := publish.Publish(ctx, publish.Opts{
		Peer: registry, Prefix: "system/", OutputDir: out,
		OriginURL: srv.URL, At: at,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	layout, err := fetch.LoadLayout(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout: %v", err)
	}
	reg, err := fetch.NewRegistry(layout, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg.Now = func() time.Time { return at.Add(time.Hour) }

	if _, err := reg.Resolve(ctx, "gone.test"); !errors.Is(err, fetch.ErrNameRevoked) {
		t.Fatalf("resolving a revoked name returned %v, want ErrNameRevoked", err)
	}
}

// TestNarrowRegistryPrefixOmitsTheSignatures measures the operator trap
// the round-trip test above sidesteps.
//
// **§6a.3a says a browsable registry SHOULD publish at
// `prefix: "system/registry/binding/by-name/"`, and that instruction is
// not implementable as written** — because a binding's signature is at
// `system/signature/{hex(binding_hash)}` (V7 §5.2's invariant pointer),
// which is outside any `system/registry/…` prefix. Publish at the
// narrower prefix and you emit bindings with no signatures over them:
// enumeration still works, and every resolve 404s at step 3.
//
// The failure is the expensive shape again — **at a consumer, a missing
// signature is indistinguishable from a withholding origin**, so the
// registry operator's publishing mistake reports as the host's fault.
//
// Recorded here rather than asserted in prose, and routed to arch as §6
// of REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21.
func TestNarrowRegistryPrefixOmitsTheSignatures(t *testing.T) {
	ctx := context.Background()
	registry := newRegistryPeer(t)
	target := newRegistryPeer(t)
	at := time.UnixMilli(1_780_000_000_000).UTC()

	if _, err := registry.IssueBinding(entitysdk.IssueOpts{
		Name:         "narrow.test",
		TargetPeerID: target.PeerID(),
		Transports: []types.HTTPPollProfileData{{
			PeerID: target.PeerID(), TransportType: "http-poll",
			Endpoint: types.TransportEndpoint{
				TreeURLPrefix:    "http://t.invalid/" + target.PeerID(),
				ContentURLPrefix: "http://t.invalid/content",
				ContentLayout:    types.ContentLayoutSharded24,
			},
			SupportedOps: []string{types.OpTreeGet},
		}},
		TTL: 30 * 24 * time.Hour,
		At:  at,
	}); err != nil {
		t.Fatalf("IssueBinding: %v", err)
	}

	out := t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(out)))
	t.Cleanup(srv.Close)
	if _, err := publish.Publish(ctx, publish.Opts{
		Peer: registry, Prefix: "system/registry/", OutputDir: out,
		OriginURL: srv.URL, At: at,
	}); err != nil {
		t.Fatalf("publish at the narrow prefix: %v", err)
	}

	layout, err := fetch.LoadLayout(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("LoadLayout: %v", err)
	}
	reg, err := fetch.NewRegistry(layout, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg.Now = func() time.Time { return at.Add(time.Hour) }

	// Enumeration is FINE — the names are all there, which is exactly
	// what makes the trap quiet. A registry browser shows a full list.
	set, err := reg.Enumerate(ctx)
	if err != nil {
		t.Fatalf("enumerating a narrowly-published registry: %v", err)
	}
	if got := set.NameStrings(); len(got) != 1 || got[0] != "narrow.test" {
		t.Fatalf("walk committed %v, want [narrow.test]", got)
	}

	// And resolution is impossible, because the signature was never
	// emitted.
	_, err = reg.ResolveIn(ctx, set, "narrow.test")
	if err == nil {
		t.Fatal("resolved a binding whose signature the publisher never emitted — " +
			"the §6a.4 signature step cannot have run")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("the failure does not name the signature step: %v", err)
	}
	t.Logf("narrow prefix: %d name(s) enumerate, and resolving fails with — %v", len(set.Names), err)
}
