// Share records — APP-CONVENTION-SHARE v0.1 (arch `bb86cd1`, routed as
// ROUTING-2026-08-18-i §4).
//
// A share is a **titled grant** over the sharer's own content, plus a
// record that makes it addressable and enumerable. Three facts drive
// every decision in this file, and each is a MUST in the convention:
//
//  1. **The audience is the minted token's `grantee`.** Never the
//     grant's `peers` scope. See ValidateShareGrants — we refuse rather
//     than emit a grant that DENYs at the cross-peer seam.
//  2. **The type tag is the cross-impl contract, not the path.**
//     Cross-peer aggregation is a `type_filter` query with NO peer
//     filter, so the tag is the index key. A tag under our own app
//     prefix would make browser<->go aggregation impossible even with a
//     perfect mirror (§2).
//  3. **A group audience is a snapshot at authoring time**, materialized
//     as one audience-entry + one minted token per member. There is no
//     group identifier anywhere in the check path (§3), and a member who
//     joins later receives nothing until the share is re-authored — a
//     gap an implementation MUST surface rather than imply away (§3.2).
//
// NOT LOCKED: `ShareFollowData.Strategy` (§5). The follow vocabulary
// awaits convergence with entity-browser-rust; the rest of the
// convention is settled and this field's openness does not extend to it.
package entitysdk

import (
	"fmt"
	"sort"

	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// The `app/share/*` type vocabulary (§2). These strings are the
// cross-implementation contract — they are the `type_filter` index key
// that makes a share authored in Go visible to a Rust consumer. Do not
// re-prefix them under an application namespace, and do not move them
// under `system/` (a convention adds no kernel surface).
const (
	TypeShareRecord   = "app/share/record"
	TypeShareAudience = "app/share/audience-entry"
	TypeShareFollow   = "app/share/follow"
)

// Share target tags (§2.2). TAGGED so there is no untagged ambiguity
// between "a content-addressed object" and "a subtree".
const (
	ShareTargetBlob   = "blob"
	ShareTargetPrefix = "prefix"
)

// Audience origin values (§2.3). Informative, for UI only — `group`
// records that a group membership produced this entry AT AUTHORING
// TIME. It is never resolved at check time.
const (
	AudienceOriginDirect = "direct"
	AudienceOriginGroup  = "group"
)

// ShareTarget is what is shared — a tagged union (§2.2).
//
// The record does NOT restate the grant's scope: the grant is the
// authority and the record is the label. **A consumer MUST NOT infer
// authorization from the record.**
type ShareTarget struct {
	Tag  string     `cbor:"tag"`
	Hash *hash.Hash `cbor:"hash,omitempty"` // blob-target
	Path string     `cbor:"path,omitempty"` // prefix-target
}

// BlobTarget shares one content-addressed object.
func BlobTarget(h hash.Hash) ShareTarget { return ShareTarget{Tag: ShareTargetBlob, Hash: &h} }

// PrefixTarget shares a subtree.
func PrefixTarget(path string) ShareTarget {
	return ShareTarget{Tag: ShareTargetPrefix, Path: path}
}

// Validate checks the tagged-union discipline: exactly one arm, and the
// arm's field present.
func (t ShareTarget) Validate() error {
	switch t.Tag {
	case ShareTargetBlob:
		if t.Hash == nil || t.Hash.IsZero() {
			return NewError(400, "invalid_share_target",
				"blob-target carries no hash (APP-CONVENTION-SHARE §2.2)")
		}
		if t.Path != "" {
			return NewError(400, "invalid_share_target",
				"blob-target also carries a path; the target is a tagged union with exactly one arm (§2.2)")
		}
	case ShareTargetPrefix:
		if t.Path == "" {
			return NewError(400, "invalid_share_target",
				"prefix-target carries no path (APP-CONVENTION-SHARE §2.2)")
		}
		if t.Hash != nil {
			return NewError(400, "invalid_share_target",
				"prefix-target also carries a hash; the target is a tagged union with exactly one arm (§2.2)")
		}
	default:
		return NewError(400, "invalid_share_target",
			fmt.Sprintf("share target tag %q is neither %q nor %q (§2.2)",
				t.Tag, ShareTargetBlob, ShareTargetPrefix))
	}
	return nil
}

// AudienceEntryData is one audience member's binding within a record
// (§2.3). `Grantee` IS the audience — it matches the minted token's
// grantee, which §5.2 step 3 hard-DENYs unless it equals the executing
// author.
type AudienceEntryData struct {
	Grantee string `cbor:"grantee"`
	Via     string `cbor:"via,omitempty"`
	AddedAt uint64 `cbor:"added_at"`
}

// ShareRecordData is the share itself (§2.2).
type ShareRecordData struct {
	Title     string              `cbor:"title"`
	Target    ShareTarget         `cbor:"target"`
	Audience  []AudienceEntryData `cbor:"audience"`
	Note      string              `cbor:"note,omitempty"`
	CreatedAt uint64              `cbor:"created_at"`
}

// ShareFollowData is a consumer's subscription to another peer's share
// (§2.4). **Strategy is NOT LOCKED** — §5 leaves the vocabulary
// undefined on purpose pending peer convergence. Do not read the rest of
// the convention's firmness as extending to it.
type ShareFollowData struct {
	Publisher string     `cbor:"publisher"`
	Record    hash.Hash  `cbor:"record"`
	Strategy  string     `cbor:"strategy,omitempty"`
	Since     *hash.Hash `cbor:"since,omitempty"`
}

// ToEntity encodes the record as an `app/share/record` entity.
func (d ShareRecordData) ToEntity() (entity.Entity, error) {
	if err := d.Validate(); err != nil {
		return entity.Entity{}, err
	}
	return encodeAsEntity(TypeShareRecord, d)
}

// ToEntity encodes an `app/share/audience-entry` entity.
func (d AudienceEntryData) ToEntity() (entity.Entity, error) {
	if d.Grantee == "" {
		return entity.Entity{}, NewError(400, "invalid_audience_entry",
			"audience-entry has no grantee; the grantee IS the audience (§2.3)")
	}
	return encodeAsEntity(TypeShareAudience, d)
}

// ToEntity encodes an `app/share/follow` entity.
func (d ShareFollowData) ToEntity() (entity.Entity, error) {
	if d.Publisher == "" {
		return entity.Entity{}, NewError(400, "invalid_follow", "follow names no publisher (§2.4)")
	}
	return encodeAsEntity(TypeShareFollow, d)
}

// Validate enforces the record-shape rules of §2.2.
//
// An EMPTY audience is legal — "an authored share with no members yet"
// is explicitly permitted by the CDDL. What is not legal is a malformed
// target or a duplicate grantee.
func (d ShareRecordData) Validate() error {
	if d.Title == "" {
		return NewError(400, "invalid_share_record",
			"share record has no title (§2.2); the title is human-facing and NOT an identifier")
	}
	if err := d.Target.Validate(); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(d.Audience))
	for i, a := range d.Audience {
		if a.Grantee == "" {
			return NewError(400, "invalid_share_record",
				fmt.Sprintf("audience entry %d has no grantee (§2.3)", i))
		}
		if _, dup := seen[a.Grantee]; dup {
			return NewError(400, "invalid_share_record",
				fmt.Sprintf("audience lists grantee %q twice; one entry per member, each with its own "+
					"minted token (§3)", a.Grantee))
		}
		seen[a.Grantee] = struct{}{}
		switch a.Via {
		case "", AudienceOriginDirect, AudienceOriginGroup:
		default:
			return NewError(400, "invalid_share_record",
				fmt.Sprintf("audience entry %d has via=%q; §2.3 admits only %q or %q",
					i, a.Via, AudienceOriginDirect, AudienceOriginGroup))
		}
	}
	return nil
}

// HasGroupDerivedMembers reports whether any audience entry was derived
// from a group membership at authoring time.
//
// **This is the join-side gap predicate (§3.2), and it exists to be
// rendered.** A member who joins the group AFTER the share is authored
// receives nothing until someone re-authors it — a group audience is a
// snapshot, not a live binding. An implementation MUST surface that in
// the UI rather than implying it away, so a renderer calls this and says
// so. It is not decoration.
func (d ShareRecordData) HasGroupDerivedMembers() bool {
	for _, a := range d.Audience {
		if a.Via == AudienceOriginGroup {
			return true
		}
	}
	return false
}

// ShareGrants builds the grant entries for a share of the sharer's own
// content — the "what is shared / how may it be touched" half of §1.
//
// **`Peers` is deliberately left nil, and that is normative** (§1.1).
// `peers` is the NETWORK dimension — which peer the grant may be used
// *against* — matched against the peer extracted from the request URI.
// For a share of our own content on our own peer, absent already means
// `{include: [local_peer_id]}`, which is exactly right. Populating it
// with the audience makes every cross-peer presentation DENY 403 with no
// indication which dimension was wrong, and it PASSES local testing,
// because locally root/grantee/granter collapse onto one identity.
//
// Operations default to read-only; a share that hands out write is an
// explicit act by the caller.
func ShareGrants(target ShareTarget, operations []string) ([]types.GrantEntry, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	if len(operations) == 0 {
		operations = []string{"tree:get", "content:get"}
	}
	var resource string
	switch target.Tag {
	case ShareTargetPrefix:
		resource = target.Path
		if len(resource) > 0 && resource[len(resource)-1] == '/' {
			resource += "*"
		}
	case ShareTargetBlob:
		// A blob share is authorized by content, not by path: the
		// resource dimension cannot name a hash, so the grant scopes
		// the content handler and the record's target carries the hash.
		resource = "*"
	}
	return []types.GrantEntry{{
		Handlers:   types.CapabilityScope{Include: []string{"system/tree", "system/content"}},
		Resources:  types.CapabilityScope{Include: []string{resource}},
		Operations: types.CapabilityScope{Include: operations},
		// Peers: INTENTIONALLY OMITTED — see the doc comment. This is
		// the single most-repeated error in this area (§1.1).
	}}, nil
}

// ValidateShareGrants refuses a share grant that carries the audience in
// its `peers` scope — APP-CONVENTION-SHARE §1.1, a MUST.
//
// We refuse rather than silently strip. Stripping would produce a
// working grant from a caller who believes they scoped it to one peer,
// which is a worse outcome than an error: they asked for something and
// would not learn they did not get it. This is the same
// refuse-don't-normalize posture as the resolver-config catch-all.
//
// **This is conformance vector SHARE-4**, named in the convention's §8 as
// one of the two that "fail loudly on the intuitive-but-wrong reading".
func ValidateShareGrants(grants []types.GrantEntry, localPeerID string) error {
	for i, g := range grants {
		if g.Peers == nil {
			continue
		}
		// `peers: {include: [local_peer_id]}` is the documented default
		// spelled out. Harmless, and not what the rule is about.
		if len(g.Peers.Include) == 1 && g.Peers.Include[0] == localPeerID && len(g.Peers.Exclude) == 0 {
			continue
		}
		return NewError(400, "share_peers_populated",
			fmt.Sprintf("share grant %d populates `peers` with %v. `peers` is the NETWORK dimension "+
				"(which peer the grant may be used AGAINST), not the audience — the audience is the "+
				"minted token's `grantee` (APP-CONVENTION-SHARE §1.1, MUST). A token minted this way "+
				"DENYs 403 at every cross-peer presentation, carrying no indication that the wrong "+
				"dimension was populated, and it PASSES local testing because root/grantee/granter "+
				"collapse onto one identity there. Omit `peers` entirely.", i, g.Peers.Include))
	}
	return nil
}

// AuthorShare builds a share record for the given audience, minting one
// capability per member (§3: per-member tokens, no group identifier in
// the check path).
//
// Returns the record entity and the minted token per grantee, in stable
// grantee order. It does NOT write to the tree — persisting the record
// and delivering the tokens are the caller's steps, and keeping them
// separate is what lets the whole authoring path be tested without a
// dispatch round trip.
//
// Withdrawal is asymmetric and this function does not hide it: see
// ShareWithdrawalNotice.
func (a *AppPeer) AuthorShare(title string, target ShareTarget, audience []AudienceEntryData,
	operations []string, createdAt uint64) (entity.Entity, map[string]entity.Entity, error) {

	grants, err := ShareGrants(target, operations)
	if err != nil {
		return entity.Entity{}, nil, err
	}
	if err := ValidateShareGrants(grants, a.PeerID()); err != nil {
		return entity.Entity{}, nil, err
	}

	rec := ShareRecordData{
		Title:     title,
		Target:    target,
		Audience:  append([]AudienceEntryData(nil), audience...),
		CreatedAt: createdAt,
	}
	if err := rec.Validate(); err != nil {
		return entity.Entity{}, nil, err
	}

	grantees := make([]string, 0, len(audience))
	for _, m := range audience {
		grantees = append(grantees, m.Grantee)
	}
	sort.Strings(grantees)

	tokens := make(map[string]entity.Entity, len(grantees))
	for _, g := range grantees {
		tok, err := a.MintCrossPeerChainCapability(g, grants, nil)
		if err != nil {
			return entity.Entity{}, nil, fmt.Errorf("share: mint for %s: %w", g, err)
		}
		tokens[g] = tok
	}

	ent, err := rec.ToEntity()
	if err != nil {
		return entity.Entity{}, nil, err
	}
	return ent, tokens, nil
}

// ShareWithdrawalNotice is what a UI is permitted to say about removing
// a member — APP-CONVENTION-SHARE §3.1, and the reason it is a typed
// value rather than a doc comment is that the wrong sentence here is a
// conformance failure, not a wording preference.
//
// A complete withdrawal is TWO operations: the policy write (which makes
// every subsequent `request` fail subset-validation with 403
// scope_exceeds_authority) PLUS a `revoke` of that peer's delivered
// capability. The two mint paths then behave differently:
//
//   - `system/capability:request`-minted — NOT recallable. Returned
//     inline with no tree write, so the granter never holds the token
//     hash and `revoke`, which is keyed by hash, cannot name it. Bounded
//     only by the token's own expires_at.
//   - the §4.4 authenticate-response capability — recallable, recorded
//     per-peer at mint time, immediate on revoke.
//
// **A conformant implementation MUST NOT present withdrawal as ending
// existing access on the `request` path**, and MUST NOT quote a
// withdrawal latency — nothing in §6.2 bounds a request-minted token's
// lifetime by the policy entry's ttl_ms (it has parent: null, so §5.6
// does not reach it), so "bounded by TTL" is a promise the substrate
// does not keep.
//
// **This is conformance vector SHARE-6** (§8) — both halves.
type ShareWithdrawalNotice struct {
	// PolicyWritten reports the first half: the member's policy entry
	// was removed or emptied, so no NEW token can be requested.
	PolicyWritten bool
	// DeliveredCapabilityRevoked reports the second half, and it is only
	// possible for the §4.4 path.
	DeliveredCapabilityRevoked bool
	// RequestMintedOutstanding is true when tokens may exist that were
	// minted through `capability:request` and therefore cannot be
	// recalled at all.
	RequestMintedOutstanding bool
}

// UserFacingClaim returns the strongest sentence §3.1 permits for this
// withdrawal state. A renderer displays this verbatim; it must not
// compose its own.
func (n ShareWithdrawalNotice) UserFacingClaim() string {
	switch {
	case n.RequestMintedOutstanding:
		// The MUST NOT case. Access already requested cannot be ended.
		return "No new access. Access already granted cannot be withdrawn and remains until it expires."
	case n.PolicyWritten && n.DeliveredCapabilityRevoked:
		return "Access ended."
	case n.PolicyWritten:
		return "No new access."
	default:
		return "Nothing was withdrawn."
	}
}
