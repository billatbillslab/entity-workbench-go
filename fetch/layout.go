package fetch

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// ProfileFile is the object name both publishers in this cohort emit the
// http-poll transport profile under, relative to the origin root.
//
// **This is the one convention left, and it is deliberately the only
// one.** A consumer handed nothing but an origin has to enter somewhere;
// after this object every URL comes from what the publisher advertised.
// The spec's own entry points — a profile read out of the publisher's
// tree, or handed over by a registry binding — are strictly better and
// are what a peer uses; this is the cold-start path for an operator with
// a URL and no prior relationship (§6.5.3's Mode A2 consumer).
const ProfileFile = "transport-profile"

// Layout is a publisher's advertised endpoint, bound to the origin it
// was read from — the Go counterpart of browser-rust's `PublishLayout`.
//
// **Every URL this package builds comes from here, and none of it is
// derived by convention.** That is the whole point of the type: the
// previous version of this package built `{base}/{peer}/tree/{path}.bin`
// and `{base}/content/{2}/{rest}` out of thin air, and could not fetch a
// single byte from our own publisher — which emits neither shape. A
// consumer that derives is a consumer that works against exactly one
// publisher, and only until that publisher moves.
type Layout struct {
	// Origin is where the profile was read from. Relative prefixes in
	// the endpoint block resolve against it; absolute ones ignore it.
	Origin string
	// PeerID is the publisher's Base58 peer-id, as the profile states
	// it. A caller-supplied id is cross-checked against this, never
	// substituted for it.
	PeerID   string
	Endpoint types.TransportEndpoint
	// Freshness and SignedPointer are carried verbatim for the caller's
	// disposition. `signed_pointer` present means the publisher claims
	// to have shipped the §6.5.3 closure; it is a claim, not a proof.
	Freshness     string
	SignedPointer string
}

// LoadLayout fetches {origin}/transport-profile and decodes it.
//
// The profile is a `system/peer/transport/http-poll` entity in ECF, and
// it is decoded with core-go's own type — no local struct, so a field
// the kernel adds arrives here without a change. `HTTPPollProfileDataFromEntity`
// enforces the D-5 transport_type/type-suffix match and fails closed.
func LoadLayout(ctx context.Context, origin string, client *http.Client) (Layout, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := strings.TrimRight(origin, "/") + "/" + ProfileFile
	raw, err := httpGet(ctx, client, url)
	if err != nil {
		return Layout{}, fmt.Errorf("fetch: transport profile: %w", err)
	}
	var ent entity.Entity
	if err := ecf.Decode(raw, &ent); err != nil {
		return Layout{}, fmt.Errorf("fetch: decode transport profile: %w", err)
	}
	if ent.Type != types.TypePeerTransportHTTPPoll {
		return Layout{}, fmt.Errorf("fetch: %s holds type %q, want %s",
			url, ent.Type, types.TypePeerTransportHTTPPoll)
	}
	d, err := types.HTTPPollProfileDataFromEntity(ent)
	if err != nil {
		return Layout{}, fmt.Errorf("fetch: decode http-poll profile data: %w", err)
	}
	return LayoutFromProfile(origin, d)
}

// LayoutFromProfile binds an already-decoded profile to an origin.
func LayoutFromProfile(origin string, d types.HTTPPollProfileData) (Layout, error) {
	if d.PeerID == "" {
		return Layout{}, fmt.Errorf("fetch: transport profile carries no peer_id")
	}
	if err := d.Endpoint.Validate(); err != nil {
		return Layout{}, fmt.Errorf("fetch: %w", err)
	}
	return Layout{
		Origin:        strings.TrimRight(origin, "/"),
		PeerID:        d.PeerID,
		Endpoint:      d.Endpoint,
		Freshness:     d.Freshness,
		SignedPointer: d.SignedPointer,
	}, nil
}

// resolvePrefix joins an advertised prefix to the origin it came from.
// An absolute prefix stands alone (§6.5.3: the three prefixes MAY be
// entirely separate origins); a relative one is origin-relative, which
// is what a same-origin publish emits.
func (l Layout) resolvePrefix(prefix string) string {
	switch {
	case prefix == "":
		return l.Origin
	case strings.HasPrefix(prefix, "http://"), strings.HasPrefix(prefix, "https://"):
		return strings.TrimRight(prefix, "/")
	case strings.HasPrefix(prefix, "/"):
		return l.Origin + strings.TrimRight(prefix, "/")
	default:
		return l.Origin + "/" + strings.TrimRight(prefix, "/")
	}
}

// treeBase resolves `tree_url_prefix` to the base the peer's paths hang
// off, appending the peer-id only when the prefix does not already carry
// it.
//
// **This is the `tree_url_prefix` join ambiguity, consumed rather than
// decided** (browser-rust ROUTING-2026-08-19-d §4, still arch's to
// rule). EXTENSION-NETWORK §6.5.3's normative sentence joins
// `{tree_url_prefix}/{peer_id}/{path}` — our publisher's emission, where
// the prefix is the bare origin. The worked example in the same section
// and §6.5.3.1 step 5 bake the peer-id into the prefix — browser-rust's
// emission, `/{peer_id}`. Both are live in this cohort, one per arm.
//
// We read what the publisher wrote: if the prefix's **last segment is
// exactly the peer-id**, it is peer-rooted and complete; otherwise it is
// origin-rooted and the peer-id is ours to append. Exactness is the
// whole discriminator and the reason this is not a `strings.Contains` —
// a peer-id appearing anywhere else in the prefix (a CDN path, a bucket
// name) is not the same claim. Note the empty-head case is legal and
// must not be special-cased away: browser-rust's same-origin publish
// advertises exactly `/{peer_id}`, whose head is empty, and requiring a
// non-empty head refuses it (their own audit F6 did precisely that, and
// it cost them seven gates).
//
// This is a bridge between two readings, not a third reading. When arch
// rules, one branch of it dies.
func (l Layout) treeBase() string {
	base := l.resolvePrefix(l.Endpoint.TreeURLPrefix)
	if lastSegment(base) == l.PeerID {
		return base
	}
	return base + "/" + l.PeerID
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// TreeLeafURL is the URL of the hash-pointer bound at treePath —
// `{tree-base}/{path}{tree_leaf_suffix}` (§6.5.3.1, Amendment 6). The
// suffix is the publisher's, appended literally; URL rewriting at
// consume time is not normative.
func (l Layout) TreeLeafURL(treePath string) string {
	return types.BuildTreeLeafURL(l.treeBase(), treePath, l.Endpoint.TreeLeafSuffix)
}

// ListingURL is the URL of the listing at treePath. An empty treePath is
// the peer-root listing, where the suffix lands on the peer-id segment.
func (l Layout) ListingURL(treePath string) string {
	return types.BuildTreeListingURL(l.treeBase(), treePath, l.Endpoint.TreeListingSuffix)
}

// ContentURL is the URL of the bytes for h, under the publisher's
// declared `content_layout`.
//
// An unknown layout is an error, never a guess — core-go's
// BuildContentURL refuses it, and that refusal is the right one: a
// consumer that falls back to a familiar shape on an unfamiliar
// declaration fetches the wrong object, or nothing, and blames the
// origin either way.
func (l Layout) ContentURL(h hash.Hash) (string, error) {
	prefix := types.EffectiveContentURLPrefix(l.Endpoint)
	if prefix == "" {
		return "", fmt.Errorf("fetch: publisher advertises no content_url_prefix, " +
			"which EXTENSION-SUBSTITUTE §2.2 makes REQUIRED with no derivation default")
	}
	return contentURL(l.resolvePrefix(prefix), l.Endpoint.ContentLayout, h)
}

// ManifestURL is the singular signed-manifest URL — terminal, no suffix
// and no trailing slash (§6.5.3). Empty when the publisher advertises
// none, which a caller must treat as "no signed entry point", not as a
// prompt to derive one: §6.5.3 calls filling `manifest_url_prefix` by
// convention a reasonable mistake and reserves the location.
func (l Layout) ManifestURL() string {
	if l.Endpoint.ManifestURLPrefix == "" {
		return ""
	}
	return l.resolvePrefix(l.Endpoint.ManifestURLPrefix)
}
