package entitysdk

import (
	"fmt"
	"net/http"
	"strings"

	cbor "github.com/fxamacker/cbor/v2"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/httplive"
	"go.entitychurch.org/entity-core-go/ext/registry/peerissued"
)

// registry_pin.go — the peer-issued backend, wired into this peer's
// meta-resolver (EXTENSION-REGISTRY §6a).
//
// # What this file is, and what it deliberately is not
//
// Before it, `AppPeer.ResolveName` could only answer for names **this
// peer had bound itself**: `CreatePeer` registers exactly one backend,
// `localname` (app.go), so the resolver chain had one rung and it was
// the user's own name book. There was no way to consult a *remote name
// authority* at all — the vocabulary for it existed in
// `resolver_config.go` (`BackendKindPeerIssued` appears in the §4.1a
// dispatch list we ship) and nothing implemented it, which is exactly
// the shape D23 warns about from the other side: a configured backend
// with no backend behind it is a config that silently matches nothing.
//
// **The §6a.4 algorithm is NOT implemented here and must not be.**
// `entity-core-go` ships it complete — signature, the association
// check, revocation, the TTL MUST, the fail-closed chain advance — in
// `ext/registry/peerissued`, together with an `HTTPPollReader` for the
// static coral-reef case. This file is ~100 lines of construction: build
// the profile, build the reader, build the backend, register it, write
// the chain entry. Pricing this work against our own tree would have
// budgeted for a resolver that was already written (D20).
//
// The one thing we do *not* take from the kernel is URL assembly. The
// profile handed to `httplive.Outbound` is built here with **absolute**
// prefixes, joined from the operator's origin, because a consumer that
// derives a URL works against exactly one publisher (AP21) and the join
// rule is a three-row table that has already bitten us once (AP30).
//
// # Where the wire consumer lives instead
//
// `fetch.Registry` implements the same §6a.4 checks for the peer-**less**
// case (`entity-fetch` links no peer, no store, no sqlite; the kernel's
// `Backend.Resolve` requires a `handler.HandlerContext` with both). The
// two are held together by `workbench/registry_differential_test.go`,
// which runs both over the same frozen bytes and fails if their verdicts
// differ. It currently records a LIVE divergence rather than agreement:
// core-go cannot decode a binding emitted by entity-core-rust
// (`transports`, §3), so a Go peer pinning the cohort's only live
// federation gets a CBOR error for every name. Routed as
// `reviews/REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21.md`.

// PinnedRegistry describes a registry this peer will consult.
type PinnedRegistry struct {
	// PeerID is the registry's Base58 peer-id — the trust root, the
	// `backend_id`, and (for the v1 identity-multihash form) the key
	// itself. Nothing is fetched to learn who the registry is.
	PeerID string
	// Origin is where its bytes are served from. A different fact from
	// who signs them: §6a.1a's *fourth actor* lives in the gap, and it
	// may choose which signed artifact answers a read and may withhold
	// one indefinitely.
	Origin string
	// Endpoint is the layout. Prefixes may be origin-relative; they are
	// joined to Origin here.
	Endpoint types.TransportEndpoint
	// AllowPlaintextHTTP permits an `http://` origin. Off by default —
	// TLS does not authenticate anything here (the signature does) but
	// it does stop a passive observer learning every name this peer
	// looks up, which §4.1's whole name-disclosure discipline is about.
	AllowPlaintextHTTP bool
	// Client overrides the HTTP client. Nil takes httplive's default.
	Client *http.Client
	// Priority is the chain position (§4, lower first). Zero puts the
	// registry ahead of the local name book, which is usually wrong:
	// a user's own binding should beat a stranger's. Default 10.
	Priority int

	// MaxTTLMillis caps how long a resolution from THIS registry may be
	// treated as valid, regardless of the ttl the registry itself signed
	// (REGISTRY §4 `hints.max_ttl`, v1.16 — the ceiling is the durable
	// chain entry's, read fresh at resolution, never a construction-time
	// option).
	//
	// **It exists because pinning is what makes the control live.** The
	// binding's own `ttl` is the *issuer's* statement about how long to
	// trust it; this is the *receiver's*, and a receiver who cannot
	// state one has no way to disagree with an issuer that writes a
	// year. Zero means "no local ceiling", which is the honest default
	// and is what this seat authored before today — arch flagged the
	// absence (R-9) as becoming live the moment we started writing
	// chain entries, which is this file.
	MaxTTLMillis uint64

	// NegTTLMillis caps how long a *negative* answer from this registry
	// may be cached (§2.1 `neg_ttl`). Zero means no local ceiling.
	NegTTLMillis uint64
}

// PinRegistry registers a peer-issued backend for one registry and adds
// it to this peer's resolver chain.
//
// After it returns, [AppPeer.ResolveName] consults the registry for
// names the §4.1a dispatch list routes to `peer-issued`, and the result
// carries `trust_anchor: peer_issued:{registry}`.
//
// **It verifies nothing at call time, on purpose.** A pin is a key, not
// a claim about an origin: the first thing that checks anything is the
// first resolve. A `PinRegistry` that reached out and validated would
// make the *absence* of an error read as "this registry is good", which
// is a claim no consumer can make about an origin it has not asked a
// question of yet.
func (a *AppPeer) PinRegistry(p PinnedRegistry) error {
	if a.resolverHandler == nil {
		return NewError(500, "no_registry_handler",
			"this peer was built without the registry handler, so there is no meta-resolver to "+
				"register a backend with")
	}
	if p.PeerID == "" {
		return NewError(400, "missing_registry_peer",
			"a registry pin needs the registry's peer-id — it is the trust root, and without it "+
				"there is nothing to verify signatures against")
	}
	if p.Origin == "" {
		return NewError(400, "missing_registry_origin",
			"a registry pin needs an origin: nothing in a peer-id says where its bytes are served, "+
				"and NETWORK §6.5.4 makes profile distribution out-of-band in v1")
	}

	pub, keyType, ok := crypto.DerivePeerFromPeerID(crypto.PeerID(p.PeerID))
	if !ok {
		return NewError(400, "unverifiable_registry_peer_id",
			fmt.Sprintf("registry peer-id %s is not identity-multihash form, so its public key is "+
				"not derivable from the pin; the config-carried-key path is deferred in v1 (§6a.5)",
				p.PeerID))
	}
	registryPeer, err := types.PeerData{
		PublicKey: pub,
		KeyType:   crypto.KeyTypeString(keyType),
	}.ToEntity()
	if err != nil {
		return WrapError(500, "identity_entity_failed", "build the registry's identity entity", err)
	}

	profile, err := registryProfile(p)
	if err != nil {
		return err
	}

	opts := []httplive.OutboundOption{httplive.WithPinnedIdentity(registryPeer)}
	if p.AllowPlaintextHTTP {
		opts = append(opts, httplive.WithOutboundAllowHTTP(true))
	}
	if p.Client != nil {
		opts = append(opts, httplive.WithOutboundHTTPClient(p.Client))
	}
	backend, err := peerissued.New(registryPeer, p.PeerID,
		peerissued.NewHTTPPollReader(httplive.NewOutbound(profile, opts...), p.PeerID))
	if err != nil {
		return WrapError(500, "backend_build_failed",
			fmt.Sprintf("build the peer-issued backend for %s", p.PeerID), err)
	}
	a.resolverHandler.RegisterBackend(backend)

	return a.addPeerIssuedChainEntry(p)
}

// registryProfile builds the dial profile, joining every advertised
// prefix to the origin.
//
// The join is [absolutePrefix]'s, not a concatenation: a prefix that is
// already absolute stands alone (§6.5.3 lets the three prefixes sit on
// entirely separate origins), and an origin-relative one is relative to
// the ORIGIN — scheme://host:port — never to a path the registry
// happened to be fetched under.
func registryProfile(p PinnedRegistry) (types.HTTPPollProfileData, error) {
	ep := p.Endpoint
	if ep.TreeURLPrefix == "" {
		return types.HTTPPollProfileData{}, NewError(400, "missing_tree_prefix",
			"a registry pin needs at least tree_url_prefix; the consumer will not derive one, "+
				"because a derived URL works against exactly one publisher (AP21)")
	}
	if ep.ContentURLPrefix == "" {
		return types.HTTPPollProfileData{}, NewError(400, "missing_content_prefix",
			"a registry pin needs content_url_prefix — deriving it as `{tree}/content` is the "+
				"exact derivation core-go removed from its own call site")
	}
	if ep.ContentLayout == "" {
		ep.ContentLayout = types.ContentLayoutSharded24
	}
	if ep.TreeLeafSuffix == "" {
		ep.TreeLeafSuffix = ".bin"
	}
	if ep.TreeListingSuffix == "" {
		ep.TreeListingSuffix = ".list"
	}

	origin := strings.TrimRight(p.Origin, "/")
	ep.TreeURLPrefix = absolutePrefix(origin, ep.TreeURLPrefix)
	ep.ContentURLPrefix = absolutePrefix(origin, ep.ContentURLPrefix)
	if ep.ManifestURLPrefix != "" {
		ep.ManifestURLPrefix = absolutePrefix(origin, ep.ManifestURLPrefix)
	}

	return types.HTTPPollProfileData{
		PeerID:        p.PeerID,
		TransportType: "http-poll",
		Endpoint:      ep,
		SupportedOps:  []string{types.OpTreeGet, types.OpContentGet, types.OpManifestGet},
	}, nil
}

func absolutePrefix(origin, prefix string) string {
	if strings.HasPrefix(prefix, "http://") || strings.HasPrefix(prefix, "https://") {
		return prefix
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return origin + prefix
}

// addPeerIssuedChainEntry installs the §4 resolver-chain entry carrying
// its §2.4 trust anchor.
//
// The two travel together and must: a chain entry with no accepted trust
// anchor resolves nothing (§6a.4 fails closed on an empty set), and an
// anchor with no chain entry is never consulted. Writing one without the
// other is a config that looks installed and matches nothing.
//
// The existing config is read and extended rather than replaced — a peer
// may have several registries pinned, and a pin that silently evicted
// the previous one would be a very quiet way to change which authority
// answers.
func (a *AppPeer) addPeerIssuedChainEntry(p PinnedRegistry) error {
	cfg, found, err := a.ResolverConfig()
	if err != nil || !found {
		cfg = DefaultResolverConfig()
	}
	prio := p.Priority
	if prio == 0 {
		prio = 10
	}

	for _, e := range cfg.ResolverChain {
		if e.BackendKind == types.BackendKindPeerIssued && e.BackendID == p.PeerID {
			return nil // already pinned; idempotent
		}
	}
	entry := types.ResolverChainEntry{
		BackendKind: types.BackendKindPeerIssued,
		BackendID:   p.PeerID,
		Priority:    uint32(prio),
		// §6a.4's `require ("peer_issued:" + registry) in
		// config.accepted_trust_anchors` — and the set is EMPTY-MEANS-
		// FAIL-CLOSED, so omitting this does not make the entry
		// permissive, it makes it inert. The anchor is per chain ENTRY
		// rather than global, which is the right shape: pinning two
		// registries must not make either of them vouch for the other.
		AcceptedTrustAnchors: []string{types.PeerIssuedTrustAnchor(p.PeerID)},
	}
	if hints := ttlHints(p); len(hints) > 0 {
		entry.Hints = hints
	}
	cfg.ResolverChain = append(cfg.ResolverChain, entry)

	// **The existing entries go back unmodified, field for field.** This
	// is R-9's whole subject: a layer that rebuilds a chain entry
	// attribute-by-attribute drops the `hints` it does not know about,
	// and dropping `max_ttl` disarms a receiver's own ceiling **while
	// every test stays green** — the config still validates, resolution
	// still works, and the only thing that changed is that a control
	// silently stopped applying. Appending to the decoded slice is what
	// keeps that from being possible here; `types.ResolverConfigData`
	// carries `Hints` as `map[string]cbor.RawMessage`, so an unknown key
	// survives a decode/encode round trip untouched.
	return a.InstallResolverConfig(cfg)
}

// ttlHints encodes the receiver-side ceilings §4 carries as `hints`.
//
// Absent keys, not zero values: §4.2 makes an unrecognized hint inert,
// and a `max_ttl: 0` would not be inert — it would be a ceiling of zero,
// which refuses every resolution the backend ever returns.
func ttlHints(p PinnedRegistry) map[string]cbor.RawMessage {
	out := map[string]cbor.RawMessage{}
	if p.MaxTTLMillis > 0 {
		if b, err := cbor.Marshal(p.MaxTTLMillis); err == nil {
			out["max_ttl"] = b
		}
	}
	if p.NegTTLMillis > 0 {
		if b, err := cbor.Marshal(p.NegTTLMillis); err == nil {
			out["neg_ttl"] = b
		}
	}
	return out
}

// registryIdentityEntity is exported-in-spirit for the differential
// test: it is how a pinned peer-id becomes the identity entity the
// kernel's backend verifies against, and the test needs the identical
// construction to drive that backend directly.
func registryIdentityEntity(peerID string) (entity.Entity, error) {
	pub, keyType, ok := crypto.DerivePeerFromPeerID(crypto.PeerID(peerID))
	if !ok {
		return entity.Entity{}, fmt.Errorf("peer-id %s is not identity-multihash form", peerID)
	}
	return types.PeerData{PublicKey: pub, KeyType: crypto.KeyTypeString(keyType)}.ToEntity()
}
