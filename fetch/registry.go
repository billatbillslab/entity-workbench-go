package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	cbor "github.com/fxamacker/cbor/v2"
	"golang.org/x/text/unicode/norm"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk/publishedroot"
)

// registry.go — hop 1 of the journey: a **name**, resolved against a
// statically-published registry, over the same verifying reader the rest
// of this package uses for content.
//
// `EXTENSION-REGISTRY` §6a is the contract. A registry is just a peer
// (§1 position 4) and its bindings are ordinary entities, so this file
// adds no transport and no new artifact — it is `Consumer` pointed at a
// different tree, plus the §6a.4 trust logic, which the spec is explicit
// is *"the ONLY registry-specific logic; this IS the backend"*.
//
// # Why this exists next to core-go's backend rather than instead of it
//
// `entity-core-go`'s `ext/registry/peerissued.Backend` implements §6a.4
// completely, including the association check, and ships an
// `HTTPPollReader` for exactly the static-registry case. **Where a peer
// exists, use it** — `entitysdk.AppPeer.PinRegistry` registers that
// backend into the meta-resolver and this file resolves nothing. Its
// `Resolve` takes a `*handler.HandlerContext` and requires a store + a
// location index, because §6a.4's precede path caches into them; that is
// the right shape inside a peer and the wrong one for `entity-fetch`,
// which deliberately links no peer, no store and no sqlite.
//
// So the split is by *where it runs*, not by *who wrote it first*:
//
//   - peer present  → core-go's Backend, through PinRegistry. No copy.
//   - peer absent   → this file.
//
// **The two are held together by a differential test, not by good
// intentions.** `workbench/registry_differential_test.go` runs both
// implementations over the same frozen bytes and requires the same
// verdict. It is the enforcement point for this decision (a discipline
// with no enforcement point is theater); if it is ever deleted, this
// file has to go with it.
//
// **What it currently records is a divergence, not agreement**, and the
// distinction matters when reading it: core-go's backend cannot decode
// a binding emitted by `entity-core-rust` at all (see [NameBinding]), so
// its half of the *positive* comparison is pinned as measured state and
// its half of the *negative* one is presently vacuous — it refuses
// everything from that registry. The test says so in both places and
// fails if the divergence widens OR silently closes.
//
// # What this adds that no seat in the cohort has
//
// §6a.3a — **enumeration**. *"The authenticated form of 'what names does
// this registry carry' is a walk of the published trie from
// `published-root.root_hash` … a served listing artifact is a
// transport-trusted convenience and MUST NOT be presented as the
// registry's authoritative contents."*
//
// core-go resolves one name at a time (the §6a.3 floor, the DNS model)
// and optionally reads a §6a.7 signed manifest. Neither answers *what is
// here*. [Registry.Enumerate] does, off [Consumer.Walk] — which fails
// closed, so a withheld node is a visibly incomplete answer instead of a
// short one. That property is the whole reason the spec calls the walk
// the authority, and it is the only reason a registry browser can show a
// list of names without teaching the operator something false.

// ErrNameNotFound is the §6a.4 step-1 dead end: the registry serves no
// by-name pointer for this name.
//
// It is deliberately distinct from every trust failure below it. §6a.4
// makes the *chain* value undifferentiated on purpose, and then says in
// the same breath that local diagnostics SHOULD tell them apart —
// because "the registry is broken" is what an operator sees when a unit
// error, a clock skew and an active substitution attempt collapse into
// one dead end during the incident where telling them apart matters.
var ErrNameNotFound = errors.New("fetch: registry serves no binding for this name")

// ErrNameAssociation is §6a.4's association check failing: the binding
// the registry served under this name says it was issued for a
// different one.
//
// **This is the one failure on the path that means someone is trying.**
// Signature valid, signer correct, unexpired, unrevoked — and the wrong
// name, which is what a hostile byte-server produces by repointing one
// by-name file at another binding the registry legitimately signed. A
// resolver that does not compare cannot see it, because every other
// check passes.
var ErrNameAssociation = errors.New("fetch: binding was issued for a different name (§6a.4 association check)")

// ErrNameExpired / ErrNameRevoked / ErrNameNullTTL are the remaining
// §6a.4 refusals, split for the same diagnostic reason.
var (
	ErrNameExpired = errors.New("fetch: binding is past issued_at + ttl")
	ErrNameRevoked = errors.New("fetch: registry has published a revocation targeting this binding")
	// ErrNameNullTTL is §6a.3's MUST. A peer-issued binding with no TTL
	// is not weakly revocable, it is **permanently unrevokable**: the
	// only other check on this path (revocation) asks the very party who
	// can withhold the answer forever, so the expiry is the sole bound a
	// hostile origin cannot influence.
	ErrNameNullTTL = errors.New("fetch: peer-issued binding carries no ttl, so no revocation bound exists (§6a.3)")
)

// Registry is a verifying reader bound to one **pinned** registry.
//
// The pin is the registry's peer-id and nothing else. For the v1
// identity-multihash form the peer-id *carries* the public key (§6a.5),
// so there is no key-distribution step and the origin serving the bytes
// is trusted for nothing — it is `EXTENSION-REGISTRY` §6a.1a's *fourth
// actor*, which may choose which signed artifact answers a read and may
// withhold one indefinitely, and may do nothing else.
type Registry struct {
	*Consumer

	pub     []byte
	keyType byte

	// Now is the clock the TTL check runs against. Nil means
	// time.Now. Injectable because `issued_at + ttl` is the only check
	// on this path a hostile byte-server cannot influence, which also
	// makes it the only one whose test needs a clock it controls.
	Now func() time.Time
}

// NewRegistry pins a registry origin.
//
// It fails rather than proceeding when the peer-id is not identity-form:
// a registry whose key is not derivable from its pin has no trust root
// here, and the alternative to failing is verifying nothing while
// looking like it verified something.
func NewRegistry(layout Layout, client *http.Client) (*Registry, error) {
	pub, keyType, err := publishedroot.DeriveKey(layout.PeerID)
	if err != nil {
		return nil, fmt.Errorf("fetch: pinning registry %s: %w", layout.PeerID, err)
	}
	return &Registry{Consumer: NewConsumer(layout, client), pub: pub, keyType: keyType}, nil
}

// PeerID is the pinned registry's peer-id — the trust root and the
// `backend_id` a resolver-chain entry would carry (§6a.2).
func (r *Registry) PeerID() string { return r.Layout.PeerID }

// TrustAnchor is the `peer_issued:{registry}` string §2.4 puts on a
// resolution result and in `accepted_trust_anchors`.
func (r *Registry) TrustAnchor() string { return types.PeerIssuedTrustAnchor(r.Layout.PeerID) }

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// -------------------------------------------------------------------
// §6a.3a — enumeration
// -------------------------------------------------------------------

// NameEntry is one name the signed root commits to, with the binding
// hash it commits to for it.
//
// The hash is what the *trie* bound, never what a leaf URL advertised.
// Where the two disagree the registry is serving two answers and only
// one of them is signed — see [NameSet.Reconciled].
type NameEntry struct {
	Name        string
	BindingHash hash.Hash
}

// NameSet is the answer to *"what names does this registry carry"*, with
// the provenance of the answer attached because the two available
// provenances are not equally worth anything.
type NameSet struct {
	// Root is the signed root the names were enumerated under. Every
	// statement in this struct is scoped to it: a negative means "this
	// root does not bind that name", never "the registry does not"
	// (EXTENSION-TREE §3.3a, and arch's `[MUST]` on scoping a negative).
	Root VerifiedRoot
	// Walk is the traversal that produced Names — kept so a surface can
	// show what the answer cost and which interior nodes it rests on.
	Walk WalkResult

	// Names is the **authoritative** set: keys committed by the signed
	// root under the by-name prefix. A hostile origin cannot shorten it
	// without the walk failing.
	Names []NameEntry

	// Listing is the served `by-name.list` artifact — a *menu*. §6a.3a:
	// keep it (one fetch instead of O(N), and the right first paint),
	// and never present it as the registry's contents.
	Listing    []string
	ListingURL string
	// ListingErr records why the listing was unavailable, if it was.
	// An absent listing is not an error for the caller: the walk already
	// answered the question, and the listing is the optional half.
	ListingErr error

	// AdvertisedOnly is listed-but-not-committed — the origin is
	// advertising a name the registry has not signed a binding into this
	// root for. Resolving it will dead-end; showing it as available is
	// the lie the §6a.3a rule exists to prevent.
	AdvertisedOnly []string
	// CommittedOnly is committed-but-not-listed. Benign on its own (a
	// stale listing), and the shape a **targeted withholding** takes: a
	// name hidden from the menu that the walk still proves is there.
	CommittedOnly []string
}

// Reconciled reports whether the served menu and the signed key set
// agree exactly.
func (n NameSet) Reconciled() bool {
	return n.ListingErr == nil && len(n.AdvertisedOnly) == 0 && len(n.CommittedOnly) == 0
}

// Lookup finds a committed name in the set.
func (n NameSet) Lookup(name string) (NameEntry, bool) {
	for _, e := range n.Names {
		if e.Name == name {
			return e, true
		}
	}
	return NameEntry{}, false
}

// NameStrings returns the committed names, sorted.
func (n NameSet) NameStrings() []string {
	out := make([]string, len(n.Names))
	for i, e := range n.Names {
		out[i] = e.Name
	}
	return out
}

// Enumerate walks the registry's signed root and returns every name it
// commits to, plus the served listing for contrast.
//
// The walk fails closed. That is the entire value of the operation: a
// registry origin that omits an entry from `by-name.list` cannot be
// caught, and one that omits a trie node makes this return an error
// instead of a shorter list. **Silently hidden becomes visibly
// incomplete**, which §6a.3a calls the strongest completeness property a
// static origin admits of — and it is a ceiling, not a guarantee: this
// still cannot tell a complete root from a *stale* one.
func (r *Registry) Enumerate(ctx context.Context) (NameSet, error) {
	root, err := r.VerifiedRoot(ctx)
	if err != nil {
		return NameSet{}, err
	}
	walk, err := r.Walk(ctx, root.Data.RootHash)
	if err != nil {
		return NameSet{Root: root}, err
	}

	set := NameSet{Root: root, Walk: walk}
	absPrefix := AbsolutePrefix(root.Data.Prefix, r.Layout.PeerID)
	wantPrefix := "/" + r.Layout.PeerID + "/" + types.PeerIssuedByNamePrefix

	for _, b := range walk.Bindings {
		// §3.3's reconstruction, then the by-name filter. Going through
		// the absolute form is what makes this work for all three
		// admissible `prefix` shapes at once — including the §6a.3a
		// SHOULD, where the registry publishes the by-name prefix
		// itself and the committed keys ARE the bare names.
		abs := absPrefix + b.Key
		if !strings.HasPrefix(abs, wantPrefix) {
			continue
		}
		name := strings.TrimPrefix(abs, wantPrefix)
		if name == "" || strings.Contains(name, "/") {
			// A key one level deeper than a name is not a name. Skip it
			// rather than surface a half-path as a browsable entry.
			continue
		}
		set.Names = append(set.Names, NameEntry{Name: name, BindingHash: b.Hash})
	}
	sort.Slice(set.Names, func(i, j int) bool { return set.Names[i].Name < set.Names[j].Name })

	set.Listing, set.ListingURL, set.ListingErr = r.listNames(ctx)
	if set.ListingErr == nil {
		set.AdvertisedOnly, set.CommittedOnly = diffNames(set.Listing, set.NameStrings())
	}
	return set, nil
}

// listNames fetches the served by-name listing artifact — the *menu*.
//
// The URL is the tree-listing form of the by-name prefix with its
// trailing slash removed: EXTENSION-NETWORK §6.5.3.1 Amendment 5 makes
// listings **named objects** with no trailing slash, so `by-name.list`
// sits beside the `by-name/` directory.
//
// # Two body formats are live and only one is the spec's
//
// A listing is a **`system/tree/listing` entity** —
// `{path, entries: {name → {hash, has_children}}, count, offset}`
// (`ENTITY-SYSTEM-REFERENCE` §168, and Amendment 5's pagination note
// hangs `next_page` off the same type). Our own publisher emits that.
//
// **`entity-browser-rust`'s static emitter writes newline-delimited
// plain text instead**, which is not that type and does not carry the
// per-entry hashes. Measured 2026-08-21 against their frozen federation;
// routed with the other findings.
//
// So this reads both, and reports which it got. **The alternative is
// worse than it sounds:** a consumer that assumes one format silently
// parses the other into nonsense — feeding a CBOR entity to a newline
// splitter yields a list of binary fragments, which then "disagree" with
// the walk and produce a reconciliation warning about names that do not
// exist. That is a false alarm about the *other side's honesty*, which is
// the most expensive kind of bug this package can emit.
//
// None of this touches the authority: the walk decides, and a listing
// that fails to parse at all is not an error for the caller (§6a.3a
// makes it a convenience).
func (r *Registry) listNames(ctx context.Context) ([]string, string, error) {
	rel := strings.TrimSuffix(types.PeerIssuedByNamePrefix, "/")
	url := r.Layout.ListingURL(rel)
	body, err := httpGet(ctx, r.Client, url)
	if err != nil {
		return nil, url, err
	}
	names, err := parseListing(body)
	if err != nil {
		return nil, url, err
	}
	sort.Strings(names)
	return names, url, nil
}

// parseListing decodes the two live listing shapes.
func parseListing(body []byte) ([]string, error) {
	// The spec shape first: a `system/tree/listing` entity.
	var ent entity.Entity
	if err := ecf.Decode(body, &ent); err == nil && ent.Type == types.TypeTreeListing {
		var listing struct {
			Entries map[string]cbor.RawMessage `cbor:"entries"`
		}
		if err := ecf.Decode(ent.Data, &listing); err != nil {
			return nil, fmt.Errorf("decode %s body: %w", types.TypeTreeListing, err)
		}
		out := make([]string, 0, len(listing.Entries))
		for name := range listing.Entries {
			out = append(out, name)
		}
		return out, nil
	}

	// The plain-text shape. Guarded by a printability check so a CBOR
	// body that failed the branch above is reported as unreadable rather
	// than split into binary fragments.
	if !isPrintableText(body) {
		return nil, fmt.Errorf("listing is neither a %s entity nor printable text (%d bytes)",
			types.TypeTreeListing, len(body))
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func isPrintableText(b []byte) bool {
	for _, c := range b {
		if c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func diffNames(listed, committed []string) (advertisedOnly, committedOnly []string) {
	inCommitted := map[string]bool{}
	for _, c := range committed {
		inCommitted[c] = true
	}
	inListed := map[string]bool{}
	for _, l := range listed {
		inListed[l] = true
		if !inCommitted[l] {
			advertisedOnly = append(advertisedOnly, l)
		}
	}
	for _, c := range committed {
		if !inListed[c] {
			committedOnly = append(committedOnly, c)
		}
	}
	return advertisedOnly, committedOnly
}

// -------------------------------------------------------------------
// §6a.4 — resolve
// -------------------------------------------------------------------

// -------------------------------------------------------------------
// the §3 binding body, as a consumer has to read it in this cohort
// -------------------------------------------------------------------

// NameBinding is the `system/registry/binding` body (§3).
//
// # Why this is not `types.BindingData`
//
// **The two live implementations disagree about `transports`, and the
// disagreement is a hard interop break — measured 2026-08-21, by
// resolving a name.**
//
//	spec §3         transports: [<endpoint per NETWORK §6.5>]  ; preferred order
//	entity-core-rust  pub transports: Vec<Value>               ; opaque, passed through
//	                  (extensions/registry/src/data.rs, BindingData)
//	entity-core-go    Transports []hash.Hash                   ; bare system/hash
//	                  (core/types/registry_ext.go, BindingData)
//
// `entity-browser-rust`'s registry emitter writes the **whole http-poll
// profile inline** — `{peer_id, transport_type, endpoint{…}, …}` — which
// is what the spec's "endpoint per NETWORK §6.5" reads as, and what a
// static registry needs, because a bare hash names an entity a consumer
// must then fetch *from the registry* and there is no rule that says the
// registry holds it.
//
// `types.BindingDataFromEntity` therefore **fails outright** on every
// binding in this cohort's only live federation:
//
//	cbor: cannot unmarshal map into Go value of type []uint8
//
// A Go peer cannot resolve a Rust registry's name at all, and the
// failure surfaces as a decode error rather than as anything an operator
// could route. Filed to arch (the spec sentence admits both readings)
// with core-go and browser-rust cc'd, in
// `reviews/REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21.md`.
//
// **Our posture until it is ruled: read both.** A consumer being liberal
// about a field the spec itself calls an opaque endpoint descriptor is
// not us picking a winner — it is the only reading under which the
// federation works today, and [TransportRef] keeps the two forms
// distinguishable so a surface can say which one it got instead of
// flattening them.
type NameBinding struct {
	Name         string
	Kind         string
	TargetPeerID string
	Transports   []TransportRef
	IssuedAt     uint64
	TTL          *uint64
	Supersedes   *hash.Hash
	Metadata     cbor.RawMessage
}

// TransportRefKind is which of the two admissible `transports` element
// shapes an entry turned out to be.
type TransportRefKind string

const (
	// TransportInline is an endpoint object carried in the binding body
	// itself — self-contained, and the shape the only live federation
	// publishes.
	TransportInline TransportRefKind = "inline"
	// TransportByHash is a bare `system/hash` naming a
	// transport-profile entity to be fetched. Self-describing, smaller,
	// and it presumes the consumer can reach whoever holds it.
	TransportByHash TransportRefKind = "hash"
	// TransportUnreadable is neither — kept rather than dropped, so a
	// binding carrying a shape from a newer vocabulary reports as
	// "three transports, none of them ones we speak" instead of as
	// "no transports", which is a different operator problem.
	TransportUnreadable TransportRefKind = "unreadable"
)

// TransportRef is one entry of a binding's `transports`.
type TransportRef struct {
	Kind    TransportRefKind
	Hash    hash.Hash
	Profile types.HTTPPollProfileData
	Raw     cbor.RawMessage
	Note    string
}

// bindingFromEntity decodes a §3 body permissively.
//
// Every field except `transports` is decoded exactly as core-go decodes
// it; only the one the cohort disagrees about is widened.
func bindingFromEntity(ent entity.Entity) (NameBinding, error) {
	var wire struct {
		Name         string            `cbor:"name"`
		Kind         string            `cbor:"kind"`
		TargetPeerID string            `cbor:"target_peer_id"`
		Transports   []cbor.RawMessage `cbor:"transports,omitempty"`
		IssuedAt     uint64            `cbor:"issued_at"`
		TTL          *uint64           `cbor:"ttl,omitempty"`
		Supersedes   *hash.Hash        `cbor:"supersedes,omitempty"`
		Metadata     cbor.RawMessage   `cbor:"metadata,omitempty"`
	}
	if err := ecf.Decode(ent.Data, &wire); err != nil {
		return NameBinding{}, fmt.Errorf("decode registry binding body: %w", err)
	}
	out := NameBinding{
		Name:         wire.Name,
		Kind:         wire.Kind,
		TargetPeerID: wire.TargetPeerID,
		IssuedAt:     wire.IssuedAt,
		TTL:          wire.TTL,
		Supersedes:   wire.Supersedes,
		Metadata:     wire.Metadata,
	}
	for _, raw := range wire.Transports {
		out.Transports = append(out.Transports, decodeTransportRef(raw))
	}
	return out, nil
}

// decodeTransportRef classifies one `transports` element.
//
// Order matters: a bare `system/hash` is a CBOR byte string and an
// endpoint is a map, so the two are distinguishable by major type and
// neither branch can shadow the other. Nothing here guesses.
func decodeTransportRef(raw cbor.RawMessage) TransportRef {
	var h hash.Hash
	if err := cbor.Unmarshal(raw, &h); err == nil {
		return TransportRef{Kind: TransportByHash, Hash: h, Raw: raw}
	}
	var prof types.HTTPPollProfileData
	if err := cbor.Unmarshal(raw, &prof); err == nil && prof.TransportType != "" {
		return TransportRef{Kind: TransportInline, Profile: prof, Raw: raw}
	}
	// A bare endpoint (the URL prefixes with no profile envelope) is the
	// other reading of §3's "endpoint per NETWORK §6.5". Accepted, with
	// the peer-id left empty — the caller cross-checks it against the
	// binding's `target_peer_id`, and an absent one simply cannot
	// contradict it.
	var ep types.TransportEndpoint
	if err := cbor.Unmarshal(raw, &ep); err == nil && ep.TreeURLPrefix != "" {
		return TransportRef{Kind: TransportInline, Raw: raw,
			Profile: types.HTTPPollProfileData{
				// "http-poll" is the value core-go's own
				// HTTPPollProfileData.Validate insists on
				// (core/types/network.go); there is no exported constant
				// for it, so this is a transcription of that check's
				// literal rather than a name we invented for it.
				TransportType: "http-poll",
				Endpoint:      ep,
			}}
	}
	return TransportRef{Kind: TransportUnreadable, Raw: raw,
		Note: "neither a bare system/hash nor an endpoint object"}
}

// ResolveSource records which of the two admissible reads located the
// binding. It is on the result rather than in a log because the two are
// not equally strong and a surface that hides the difference is
// overclaiming.
type ResolveSource string

const (
	// SourcePointer is §6a.4 step 1 verbatim — the by-name tree pointer,
	// one fetch, the DNS model, and the conformant floor. **The pointer
	// is transport-supplied**, which is exactly why step 3 has an
	// association check.
	SourcePointer ResolveSource = "pointer"
	// SourceWalk located the binding in the signed key set instead
	// (§6a.3a). Strictly stronger and strictly more expensive: the
	// origin cannot substitute *which* binding answers a name without
	// breaking the root signature, and cannot hide a name without the
	// walk failing. The association check still runs — it costs nothing
	// and a defence that only works on the cheap path is a defence with
	// a bypass.
	SourceWalk ResolveSource = "walk"
)

// RevocationCheck is what §6a.6 could and could not establish.
//
// It is a struct rather than a bool because the honest answers are
// three, not two, and the spec is unusually direct about it:
// **presence proves revocation, absence proves nothing.**
type RevocationCheck struct {
	// Present means a revocation was found AND verified against the
	// pinned key. This is the only value in this struct that is
	// evidence.
	Present bool
	// Probed means the by-target index was queried.
	Probed bool
	// URL is where it was queried.
	URL string
	// WalkCovered means the revocation prefix lies inside the key set
	// the signed root commits to, so an absence here is an absence the
	// *registry signed*, not merely one the origin served.
	//
	// This is the upgrade §6a.6's last paragraph describes — *"a
	// resolver that wants better than the TTL bound walks the published
	// trie … where withholding a node makes the walk fail visibly."*
	// It is available exactly when the registry's published `prefix`
	// covers the revocation prefix, which is a publisher's choice, so it
	// is reported rather than assumed.
	WalkCovered bool
}

// Bound describes, in one sentence, what this check is worth.
func (rc RevocationCheck) Bound() string {
	switch {
	case rc.Present:
		return "revoked — a registry-signed revocation targets this binding"
	case rc.WalkCovered:
		return "no revocation in the signed key set; the origin cannot withhold one from a walk without the walk failing"
	case rc.Probed:
		return "no revocation served; absence proves nothing (§6a.6) — the bound on a withheld revocation is the binding's ttl"
	default:
		return "not probed"
	}
}

// NameResolution is a `name → peer-id` binding and every check that
// produced it.
//
// **The error is the gate; this value is also the diagnostic.** On a
// refusal it is returned *populated as far as the checks got* — which is
// deliberate and is §6a.4's own instruction: *"a resolver SHOULD surface
// which require failed to its own operator"*, because a unit error, a
// clock skew and an active substitution attempt are otherwise
// indistinguishable during the incident where telling them apart
// matters. A caller that ignores the error and reads [NameResolution.PeerID]
// gets the substituted target, which is why the error is not optional
// and why no surface in this repo renders one of these without its
// verdict beside it.
//
// The value never crosses a peer boundary: the same §6a.4 paragraph
// makes the chain value undifferentiated, and this is local reporting to
// the operator running the resolver.
type NameResolution struct {
	Name       string // as asked
	Normalized string // NFC, §6.3 name-path safe

	Source      ResolveSource
	BindingHash hash.Hash
	PointerURL  string // populated on the pointer path

	Binding   NameBinding
	Signature types.SignatureData

	Revocation RevocationCheck

	// IssuedAt / ExpiresAt come from inside the signed body, checked
	// against our own clock — the one step here a hostile byte-server
	// cannot touch.
	IssuedAt  time.Time
	ExpiresAt time.Time
	CheckedAt time.Time

	// TrustAnchor is `peer_issued:{registry}` (§2.4).
	TrustAnchor string
}

// PeerID is the target this name resolved to — the thing the whole hop
// exists to produce.
func (n NameResolution) PeerID() string { return n.Binding.TargetPeerID }

// Transports are the ways the registry says the target can be reached.
// §6a.3 makes these a MUST on a peer-issued binding, and the reason is
// this exact journey: a statically-published peer has no profile to
// discover (NETWORK §6.5.4 puts distribution out of band in v1), so a
// consumer that resolves a peer-id and stops has learned a fact it
// cannot act on.
func (n NameResolution) Transports() []TransportRef { return n.Binding.Transports }

// Fresh reports whether the binding is unexpired at t.
func (n NameResolution) Fresh(t time.Time) bool { return t.Before(n.ExpiresAt) }

// Resolve implements §6a.4 against the pinned registry, locating the
// binding through the by-name pointer — the conformant floor, one
// round-trip, no enumeration.
func (r *Registry) Resolve(ctx context.Context, name string) (NameResolution, error) {
	normalized, err := NormalizeName(name)
	if err != nil {
		return NameResolution{}, err
	}
	rel := types.PeerIssuedByNamePath(normalized)
	url := r.Layout.TreeLeafURL(rel)
	raw, err := httpGet(ctx, r.Client, url)
	if err != nil {
		return NameResolution{}, fmt.Errorf("%w: %s: %w", ErrNameNotFound, url, err)
	}
	bindingHash, err := crackPointer(raw)
	if err != nil {
		return NameResolution{}, fmt.Errorf("%s: %w", url, err)
	}
	res, err := r.verify(ctx, name, normalized, bindingHash, SourcePointer, nil)
	res.PointerURL = url
	return res, err
}

// ResolveIn implements §6a.4 with step 1 answered from an enumeration
// instead of a served pointer (§6a.3a).
//
// The binding hash comes out of the signed key set, so the substitution
// the association check exists to catch cannot be mounted at all on this
// path — and the check still runs. Pass the [NameSet] from [Enumerate];
// its walk also tells the revocation probe whether an absence is worth
// anything.
func (r *Registry) ResolveIn(ctx context.Context, set NameSet, name string) (NameResolution, error) {
	normalized, err := NormalizeName(name)
	if err != nil {
		return NameResolution{}, err
	}
	entry, ok := set.Lookup(normalized)
	if !ok {
		return NameResolution{}, fmt.Errorf("%w: %q is not among the %d names root %s commits to",
			ErrNameNotFound, normalized, len(set.Names), set.Root.Data.RootHash)
	}
	return r.verify(ctx, name, normalized, entry.BindingHash, SourceWalk, &set)
}

// verify is §6a.4 step 3 — the whole of the registry-specific logic,
// applied identically whichever read located the binding.
//
// Order matters and follows the spec's: signature before decode-driven
// checks, association before the checks that depend on trusting the
// body's other fields, revocation before expiry. Every refusal is
// fail-closed and none of them downgrades to a pin (§6a.4's explicit
// MUST NOT — a pin matches only when it is configured as its own chain
// entry).
func (r *Registry) verify(ctx context.Context, asked, normalized string, bindingHash hash.Hash,
	src ResolveSource, set *NameSet) (NameResolution, error) {

	res := NameResolution{
		Name:        asked,
		Normalized:  normalized,
		Source:      src,
		BindingHash: bindingHash,
		TrustAnchor: r.TrustAnchor(),
		CheckedAt:   r.now(),
	}

	// The body. Content-addressed, so the bytes are self-verifying
	// against the hash we asked for — an origin cannot answer this with
	// a different binding, only with none.
	bodyEnt, err := r.Blob(ctx, bindingHash)
	if err != nil {
		return res, fmt.Errorf("registry binding %s: %w", bindingHash, err)
	}
	if bodyEnt.Type != types.TypeRegistryBinding {
		return res, fmt.Errorf("registry binding %s is type %q, want %s",
			bindingHash, bodyEnt.Type, types.TypeRegistryBinding)
	}

	// The signature, at the §5.2 invariant pointer, resolved from the
	// hash we asked for rather than one the origin offered.
	sigEnt, err := r.leafAt(ctx, publishedroot.SignatureRelPath(bindingHash))
	if err != nil {
		return res, fmt.Errorf("registry binding %s: resolving its §5.2 signature pointer: %w",
			bindingHash, err)
	}
	sig, err := publishedroot.VerifySignatureOver("registry binding",
		fmt.Sprintf("system/signature/%s", bindingHash), bodyEnt, sigEnt, r.pub, r.keyType)
	if err != nil {
		return res, err
	}
	res.Signature = sig

	body, err := bindingFromEntity(bodyEnt)
	if err != nil {
		return res, fmt.Errorf("registry binding %s: %w", bindingHash, err)
	}
	res.Binding = body

	// THE ASSOCIATION CHECK (§6a.4 `[MUST]`).
	//
	// The signature above proves who issued this binding. It never
	// proves what it was issued *for* — and the association is not
	// missing from the commitment, it is discarded by verifiers: the
	// signed body carries `name`, so sig(R, {name, target}) IS the
	// registry's assertion of the pairing. Comparing costs one string
	// compare and zero fetches.
	if body.Name != normalized {
		return res, fmt.Errorf("%w: served under %q, issued for %q — a valid, correctly-signed, "+
			"unexpired binding answering the wrong question",
			ErrNameAssociation, normalized, body.Name)
	}

	if body.Kind != "" && body.Kind != types.BackendKindPeerIssued {
		return res, fmt.Errorf("registry binding %s is kind %q, not %q — a registry's own bindings "+
			"are the issued kind; anything else here is a local-trust artifact that a remote "+
			"authority has no standing to assert", bindingHash, body.Kind, types.BackendKindPeerIssued)
	}

	// §6a.3's ttl MUST comes BEFORE the revocation probe, which is
	// §6a.4's own order and not arbitrary: a null-ttl binding is
	// permanently unrevokable, so asking about its revocation first
	// would report the symptom (or, worse, "not revoked") for a binding
	// whose defect is that the question cannot be answered.
	if body.TTL == nil {
		return res, fmt.Errorf("%w: binding %s", ErrNameNullTTL, bindingHash)
	}

	res.Revocation, err = r.checkRevoked(ctx, bindingHash, set)
	if err != nil {
		return res, err
	}
	if res.Revocation.Present {
		return res, fmt.Errorf("%w: binding %s", ErrNameRevoked, bindingHash)
	}

	res.IssuedAt = time.UnixMilli(int64(body.IssuedAt)).UTC()
	res.ExpiresAt = time.UnixMilli(int64(body.IssuedAt + *body.TTL)).UTC()
	if !res.CheckedAt.Before(res.ExpiresAt) {
		return res, fmt.Errorf("%w: issued %s, expired %s, checked %s",
			ErrNameExpired,
			res.IssuedAt.Format(time.RFC3339),
			res.ExpiresAt.Format(time.RFC3339),
			res.CheckedAt.Format(time.RFC3339))
	}

	if body.TargetPeerID == "" {
		return res, fmt.Errorf("registry binding %s names no target peer", bindingHash)
	}
	if len(body.Transports) == 0 {
		// §6a.3 `[MUST]`. Refusing is arguable — the peer-id did
		// resolve — but a peer-issued binding with no transports is the
		// registry asserting a target and providing no way to reach it,
		// and NETWORK §6.5.4 offers a static consumer nothing to fall
		// back to. Surfacing it as resolved would hand a caller a fact
		// it cannot act on and call the journey successful.
		return res, fmt.Errorf("registry binding %s carries no transports; §6a.3 makes them a MUST "+
			"on a peer-issued binding because a statically-published target has no profile to "+
			"discover", bindingHash)
	}
	return res, nil
}

// checkRevoked is §6a.6.
//
// A found revocation is verified against the pinned key before it is
// believed — the index is served by the same party that would like to
// forge one, and an unverified revocation is a denial-of-service handed
// to the origin.
func (r *Registry) checkRevoked(ctx context.Context, bindingHash hash.Hash, set *NameSet) (RevocationCheck, error) {
	rel := types.PeerIssuedRevocationByTargetPath(bindingHash)
	out := RevocationCheck{Probed: true, URL: r.Layout.TreeLeafURL(rel)}

	if set != nil {
		out.WalkCovered = set.covers(r.Layout.PeerID, rel)
		if out.WalkCovered {
			// The signed key set is the answer, and it is a better one:
			// this absence is the registry's, not the origin's.
			if _, ok := set.lookupRel(r.Layout.PeerID, rel); !ok {
				return out, nil
			}
		}
	}

	revEnt, err := r.leafAt(ctx, rel)
	if err != nil {
		// Absent — which proves nothing, and Bound() says so.
		return out, nil
	}
	if revEnt.Type != types.TypeRegistryRevocation {
		return out, fmt.Errorf("revocation index for %s holds type %q, want %s",
			bindingHash, revEnt.Type, types.TypeRegistryRevocation)
	}
	sigEnt, err := r.leafAt(ctx, publishedroot.SignatureRelPath(revEnt.ContentHash))
	if err != nil {
		// A revocation nobody signed is not a revocation. Refuse to act
		// on it rather than letting the origin revoke by assertion.
		return out, nil
	}
	if _, err := publishedroot.VerifySignatureOver("registry revocation",
		fmt.Sprintf("system/signature/%s", revEnt.ContentHash), revEnt, sigEnt, r.pub, r.keyType); err != nil {
		return out, nil
	}
	rev, err := types.RevocationDataFromEntity(revEnt)
	if err != nil {
		return out, fmt.Errorf("decode revocation for %s: %w", bindingHash, err)
	}
	if rev.Revokes != bindingHash {
		// Indexed under one binding, targeting another — the same
		// substitution shape the association check catches, one artifact
		// over. Not evidence about this binding.
		return out, nil
	}
	out.Present = true
	return out, nil
}

// covers reports whether a peer-relative tree path lies inside the key
// space the signed root commits to.
func (n NameSet) covers(peerID, rel string) bool {
	abs := "/" + peerID + "/" + rel
	return strings.HasPrefix(abs, AbsolutePrefix(n.Root.Data.Prefix, peerID))
}

// lookupRel finds a peer-relative tree path in the committed key set.
func (n NameSet) lookupRel(peerID, rel string) (hash.Hash, bool) {
	absPrefix := AbsolutePrefix(n.Root.Data.Prefix, peerID)
	want := "/" + peerID + "/" + rel
	for _, b := range n.Walk.Bindings {
		if absPrefix+b.Key == want {
			return b.Hash, true
		}
	}
	return hash.Hash{}, false
}

// NormalizeName applies NFC plus REGISTRY §6.3 name-path safety.
//
// **This is a Layer-2 algorithm** in the sense `AGENTS.md` fixes: a
// canonicalization whose output selects a tree path, so two impls that
// disagree resolve differently for the same input and the disagreement
// surfaces as a 404. It is a transcription of
// `entity-core-go`'s `ext/registry/peerissued.normalizeName`, which is
// unexported — the transcription is why
// `workbench/registry_differential_test.go` feeds both this and the
// kernel's resolver the same names, and why an ask to export it went
// back to core-go rather than being noted and forgotten.
//
// No case-folding: §6a.4 normalizes with NFC only, and a registry that
// wants case-insensitivity folds before it authors the pointer.
func NormalizeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: the empty name", ErrNameNotFound)
	}
	if !norm.NFC.IsNormalString(name) {
		name = norm.NFC.String(name)
	}
	// A name-path-safety failure is a NOT-FOUND, not a distinct error
	// class. §6a.4 says so in as many words — *"name-path safety failure
	// → treat as not_found (we cannot route this name to the registry);
	// chain advances"* — and core-go's backend does exactly that. The
	// message stays specific for the operator; the wrapped sentinel is
	// what the chain sees, so both resolvers advance identically.
	for _, r := range name {
		if r == '/' {
			return "", fmt.Errorf("%w: name %q contains '/', forbidden by REGISTRY §6.3 — "+
				"a name is one path segment", ErrNameNotFound, name)
		}
		if r <= 0x0020 || r == 0x007F {
			return "", fmt.Errorf("%w: name %q contains control character U+%04X",
				ErrNameNotFound, name, r)
		}
	}
	return name, nil
}

// -------------------------------------------------------------------
// hop 2 — the binding's transports become a layout
// -------------------------------------------------------------------

// Origin is a resolved way to reach a target peer: the http-poll profile
// a binding pointed at, turned into the [Layout] the content half of
// this package already speaks.
//
// This is the join the cohort did not have. REGISTRY stops at *"a
// peer-id and how to reach it"* and the CDN corridor starts at *"a
// layout"*, and the step between them — decode the transport-profile
// entities the binding commits to, pick the one this consumer can
// actually use — belonged to nobody.
type Origin struct {
	Layout  Layout
	Profile types.HTTPPollProfileData
	// Ref is which of the binding's transports produced it, including
	// which of the two cohort shapes it was carried in.
	Ref TransportRef
	// ProfileHash is set only when the transport was carried by hash.
	ProfileHash hash.Hash
	// Skipped records transports that were not usable, and why —
	// surfaced rather than swallowed, because "no reachable transport"
	// and "three transports we don't speak" are different operator
	// problems.
	Skipped []SkippedTransport
}

// SkippedTransport is one transport-profile the consumer declined.
type SkippedTransport struct {
	Hash   hash.Hash
	Reason string
}

// OriginFor resolves a binding's transports into a reachable http-poll
// layout for the target.
//
// `origin` is where the target's URLs are rooted. It matters because
// **every static emitter in this cohort publishes origin-relative
// prefixes** (`/docs/2KFRB…`, `/content`), and an origin-relative URL is
// relative to a *scheme://host:port*, never to a path — so a registry
// served at `https://host/registry` names domains at `https://host/docs`,
// and the origin to hand back is `https://host`. Pass "" to take that
// default off the registry's own origin; pass a value when the
// deployment splits the two across hosts.
//
// It deliberately does not fall back to deriving a layout from the
// target's peer-id. That is AP21 in one line: a consumer that derives a
// layout works against exactly one publisher, and against every other
// one it produces a 404 that reads as a withholding origin.
func (r *Registry) OriginFor(ctx context.Context, res NameResolution, origin string) (Origin, error) {
	if origin == "" {
		origin = OriginRoot(r.Layout.Origin)
	}
	out := Origin{}
	for _, ref := range res.Binding.Transports {
		prof, err := r.profileOf(ctx, ref)
		if err != nil {
			out.Skipped = append(out.Skipped, SkippedTransport{Hash: ref.Hash, Reason: err.Error()})
			continue
		}
		// The registry signed a binding to peer X carrying a profile
		// that says peer Y. Refuse: the profile's peer-id is the key the
		// target's content signature gets checked against, so following
		// it would verify the wrong publisher perfectly.
		if prof.PeerID != "" && prof.PeerID != res.Binding.TargetPeerID {
			out.Skipped = append(out.Skipped, SkippedTransport{Hash: ref.Hash,
				Reason: fmt.Sprintf("profile is for peer %s, binding targets %s",
					prof.PeerID, res.Binding.TargetPeerID)})
			continue
		}
		if prof.PeerID == "" {
			// A bare endpoint carries no peer-id; the binding's target is
			// the only identity in play, and it is the registry's signed
			// assertion. Adopt it explicitly rather than letting Layout
			// fail on an empty one.
			prof.PeerID = res.Binding.TargetPeerID
		}
		at := origin
		if abs, ok := absoluteOrigin(prof); ok {
			// A profile with absolute URL prefixes carries its own host
			// and does not need ours (§6.5.3 lets the three prefixes sit
			// on entirely separate origins).
			at = abs
		}
		layout, err := LayoutFromProfile(at, prof)
		if err != nil {
			out.Skipped = append(out.Skipped, SkippedTransport{Hash: ref.Hash, Reason: err.Error()})
			continue
		}
		out.Layout, out.Profile, out.ProfileHash, out.Ref = layout, prof, ref.Hash, ref
		return out, nil
	}
	return out, fmt.Errorf("no usable http-poll transport among the %d the binding for %q commits to (%s)",
		len(res.Binding.Transports), res.Normalized, skippedSummary(out.Skipped))
}

// profileOf turns one transports entry into a profile, fetching it from
// the registry when the entry is a bare hash.
//
// The by-hash branch reads from the **registry's** content store, which
// is a real limitation worth naming rather than hiding: nothing in §3
// says the registry holds the entity a binding's hash names, so a
// by-hash transport can dead-end at a registry that published the
// reference and not the referent. The inline shape has no such hole,
// which is one of the arguments in the divergence packet.
func (r *Registry) profileOf(ctx context.Context, ref TransportRef) (types.HTTPPollProfileData, error) {
	switch ref.Kind {
	case TransportInline:
		return ref.Profile, nil
	case TransportByHash:
		ent, err := r.Blob(ctx, ref.Hash)
		if err != nil {
			return types.HTTPPollProfileData{}, fmt.Errorf(
				"the binding names transport-profile %s and the registry does not serve it: %w", ref.Hash, err)
		}
		return httpPollProfile(ent)
	default:
		return types.HTTPPollProfileData{}, errors.New(ref.Note)
	}
}

// httpPollProfile decodes a transport-profile entity, insisting it is
// the http-poll shape this consumer can actually drive.
func httpPollProfile(ent entity.Entity) (types.HTTPPollProfileData, error) {
	if ent.Type != types.TypePeerTransportHTTPPoll {
		return types.HTTPPollProfileData{}, fmt.Errorf("transport is type %q, not %s",
			ent.Type, types.TypePeerTransportHTTPPoll)
	}
	return types.HTTPPollProfileDataFromEntity(ent)
}

// absoluteOrigin reports the origin a profile's own URL prefixes name,
// when they are absolute.
func absoluteOrigin(prof types.HTTPPollProfileData) (string, bool) {
	ep := prof.Endpoint
	for _, p := range []string{ep.ManifestURLPrefix, ep.TreeURLPrefix, ep.ContentURLPrefix} {
		if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
			return OriginRoot(p), true
		}
	}
	return "", false
}

// OriginRoot reduces a URL to its origin — scheme://host[:port].
//
// This is the operation that makes an origin-relative transport profile
// usable: `/docs/content` is an absolute *path*, so it resolves against
// the origin the profile was learned from and not against whatever path
// that fetch happened to sit under.
func OriginRoot(u string) string {
	for _, scheme := range []string{"https://", "http://"} {
		if !strings.HasPrefix(u, scheme) {
			continue
		}
		rest := u[len(scheme):]
		if i := strings.Index(rest, "/"); i >= 0 {
			return scheme + rest[:i]
		}
		return u
	}
	return strings.TrimRight(u, "/")
}

func skippedSummary(sk []SkippedTransport) string {
	if len(sk) == 0 {
		return "the binding carries none"
	}
	parts := make([]string, len(sk))
	for i, s := range sk {
		parts[i] = s.Reason
	}
	return strings.Join(parts, "; ")
}
