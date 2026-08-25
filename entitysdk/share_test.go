package entitysdk

import (
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// TestShare4_PeersPopulatedIsRefused is conformance vector **SHARE-4**
// (APP-CONVENTION-SHARE §8), and arch named it as one of the two that
// "fail loudly on the intuitive-but-wrong reading".
//
// The intuitive-but-wrong reading is that `peers` carries the audience.
// It does not: `peers` is the NETWORK dimension, matched against the
// peer extracted from the request URI, and the audience is the minted
// token's `grantee` (§1.1, MUST).
//
// **The reason this vector has to exist rather than being left to
// review** is that the wrong shape PASSES local testing. Locally root,
// grantee and sole in-chain granter collapse onto one identity, so a
// share tested against one's own peer passes with `peers` populated any
// way at all. It springs apart only at the cross-peer seam, where it
// DENYs 403 carrying no indication which dimension was wrong.
//
// Tier: contract pin (cross-impl conformance vector).
func TestShare4_PeersPopulatedIsRefused(t *testing.T) {
	const local = "2KLocalPeerIdForTest"
	const bob = "2KBobTheAudienceMember"

	grants, err := ShareGrants(PrefixTarget("albums/summer/"), nil)
	if err != nil {
		t.Fatalf("ShareGrants: %v", err)
	}

	// The correct shape: `peers` omitted entirely.
	if grants[0].Peers != nil {
		t.Fatalf("ShareGrants emitted a non-nil peers scope %v — §1.1 requires it OMITTED for a share "+
			"of the sharer's own content", grants[0].Peers.Include)
	}
	if err := ValidateShareGrants(grants, local); err != nil {
		t.Fatalf("the grants we build must pass our own validator: %v", err)
	}

	// The wrong shape: the audience smuggled into the network dimension.
	wrong := append([]types.GrantEntry(nil), grants...)
	wrong[0].Peers = &types.CapabilityScope{Include: []string{bob}}

	err = ValidateShareGrants(wrong, local)
	if err == nil {
		t.Fatal("SHARE-4: a grant carrying the audience in `peers` was ACCEPTED. Every cross-peer " +
			"presentation of the resulting token DENYs 403, and local tests cannot see it.")
	}
	if !strings.Contains(err.Error(), "share_peers_populated") {
		t.Errorf("error does not carry the code: %v", err)
	}
	// The message has to name the right dimension, or the next person
	// re-derives the same bug from a vague error.
	msg := err.Error()
	for _, want := range []string{"grantee", "§1.1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message does not mention %q — it must point at the correct carrier: %s", want, msg)
		}
	}

	// `peers: {include: [local]}` is the documented default written out.
	// Harmless, and NOT what the rule is about — refusing it would be a
	// false positive.
	spelled := append([]types.GrantEntry(nil), grants...)
	spelled[0].Peers = &types.CapabilityScope{Include: []string{local}}
	if err := ValidateShareGrants(spelled, local); err != nil {
		t.Errorf("`peers: {include: [local_peer_id]}` is the §3.6 default spelled out and must be "+
			"accepted; refusing it is a false positive: %v", err)
	}
}

// TestShare6_WithdrawalClaimsBothHalves is conformance vector
// **SHARE-6** (§8) — both halves of withdrawal.
//
// §3.1: a complete withdrawal is the policy write PLUS a revoke of the
// delivered capability, and the two mint paths differ. A
// `capability:request`-minted token is NOT recallable at all — it is
// returned inline with no tree write, so the granter never holds its
// hash and `revoke`, keyed by hash, cannot name it.
//
// **A conformant implementation MUST NOT present withdrawal as ending
// existing access on the `request` path.** This pins the sentences, not
// the mechanism, because the wrong sentence IS the conformance failure:
// a user told "access ended" when it has not is misinformed by the
// product, not by the protocol.
//
// Tier: contract pin (cross-impl conformance vector).
func TestShare6_WithdrawalClaimsBothHalves(t *testing.T) {
	cases := []struct {
		name   string
		notice ShareWithdrawalNotice
		want   string
		reject []string // substrings that MUST NOT appear
	}{
		{
			name:   "nothing done yet",
			notice: ShareWithdrawalNotice{},
			want:   "Nothing was withdrawn.",
		},
		{
			name:   "policy written only — no new access, but existing stands",
			notice: ShareWithdrawalNotice{PolicyWritten: true},
			want:   "No new access.",
			reject: []string{"ended"},
		},
		{
			name: "both halves on the recallable path",
			notice: ShareWithdrawalNotice{
				PolicyWritten:              true,
				DeliveredCapabilityRevoked: true,
			},
			want: "Access ended.",
		},
		{
			// The MUST NOT case, and the whole point of the vector.
			name: "request-minted tokens outstanding — MUST NOT claim access ended",
			notice: ShareWithdrawalNotice{
				PolicyWritten:              true,
				DeliveredCapabilityRevoked: true,
				RequestMintedOutstanding:   true,
			},
			want:   "No new access. Access already granted cannot be withdrawn and remains until it expires.",
			reject: []string{"Access ended."},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.notice.UserFacingClaim()
			if got != tc.want {
				t.Errorf("claim = %q, want %q", got, tc.want)
			}
			for _, bad := range tc.reject {
				if strings.Contains(got, bad) {
					t.Errorf("SHARE-6: claim %q contains %q — §3.1 forbids presenting withdrawal as "+
						"ending existing access on this path", got, bad)
				}
			}
			// §3.1's explicit prohibition: no latency may be quoted,
			// because nothing in §6.2 bounds a request-minted token's
			// lifetime by the policy entry's ttl_ms.
			for _, unit := range []string{"second", "minute", "hour", "within", "immediately"} {
				if strings.Contains(strings.ToLower(got), unit) {
					t.Errorf("claim %q quotes a withdrawal latency (%q); §3.1 forbids it — "+
						"\"bounded by TTL\" is a promise the substrate does not keep", got, unit)
				}
			}
		})
	}
}

// TestShareRecord_TargetIsATaggedUnion pins §2.2's tagging discipline.
// An untagged target would make "a hash" and "a path" ambiguous at the
// consumer, which is exactly what the tag exists to prevent.
func TestShareRecord_TargetIsATaggedUnion(t *testing.T) {
	if err := PrefixTarget("albums/summer/").Validate(); err != nil {
		t.Errorf("a well-formed prefix-target was refused: %v", err)
	}
	if err := (ShareTarget{Tag: "prefix"}).Validate(); err == nil {
		t.Error("a prefix-target with no path was accepted")
	}
	if err := (ShareTarget{Tag: "blob"}).Validate(); err == nil {
		t.Error("a blob-target with no hash was accepted")
	}
	if err := (ShareTarget{Tag: "subtree", Path: "x/"}).Validate(); err == nil {
		t.Error("an unknown tag was accepted; §2.2 admits exactly blob and prefix")
	}
	// Both arms populated is the ambiguity the tag exists to forbid.
	h, err := hash.Compute("test/note", cbor.RawMessage{0xa0})
	if err != nil {
		t.Fatalf("compute hash: %v", err)
	}
	if err := (ShareTarget{Tag: ShareTargetPrefix, Path: "x/", Hash: &h}).Validate(); err == nil {
		t.Error("both arms populated was accepted")
	}
	if err := (ShareTarget{Tag: ShareTargetBlob, Hash: &h, Path: "x/"}).Validate(); err == nil {
		t.Error("both arms populated was accepted")
	}
	if err := BlobTarget(h).Validate(); err != nil {
		t.Errorf("a well-formed blob-target was refused: %v", err)
	}
}

// TestShareRecord_ValidatesAudience pins §2.2/§2.3: an empty audience is
// LEGAL ("an authored share with no members yet"), a duplicate grantee
// is not, and `via` is closed to two values.
func TestShareRecord_ValidatesAudience(t *testing.T) {
	base := ShareRecordData{
		Title:     "Summer album",
		Target:    PrefixTarget("albums/summer/"),
		CreatedAt: 1000,
	}

	// Empty audience — explicitly permitted by the CDDL.
	if err := base.Validate(); err != nil {
		t.Errorf("an authored share with no members yet must validate (§2.2): %v", err)
	}

	dup := base
	dup.Audience = []AudienceEntryData{{Grantee: "2KBob", AddedAt: 1}, {Grantee: "2KBob", AddedAt: 2}}
	if err := dup.Validate(); err == nil {
		t.Error("a duplicate grantee was accepted; §3 is one entry + one token per member")
	}

	badVia := base
	badVia.Audience = []AudienceEntryData{{Grantee: "2KBob", Via: "team", AddedAt: 1}}
	if err := badVia.Validate(); err == nil {
		t.Error("via=\"team\" was accepted; §2.3 admits only direct or group")
	}

	noTitle := base
	noTitle.Title = ""
	if err := noTitle.Validate(); err == nil {
		t.Error("a record with no title was accepted")
	}
}

// TestShareRecord_GroupMembershipIsASnapshot pins §3.2 — the join-side
// gap. A member who joins the group AFTER authoring receives nothing
// until the share is re-authored, and an implementation MUST surface
// that rather than implying it away.
//
// The predicate exists so a renderer can say so; this pin is what stops
// it from being quietly dropped as "unused".
func TestShareRecord_GroupMembershipIsASnapshot(t *testing.T) {
	direct := ShareRecordData{
		Title:    "Direct only",
		Target:   PrefixTarget("albums/"),
		Audience: []AudienceEntryData{{Grantee: "2KBob", Via: AudienceOriginDirect, AddedAt: 1}},
	}
	if direct.HasGroupDerivedMembers() {
		t.Error("a wholly direct audience reported group-derived members")
	}

	grouped := direct
	grouped.Audience = append(append([]AudienceEntryData(nil), direct.Audience...),
		AudienceEntryData{Grantee: "2KCarol", Via: AudienceOriginGroup, AddedAt: 2})
	if !grouped.HasGroupDerivedMembers() {
		t.Error("§3.2: an audience with a group-derived member must report it so the UI can surface " +
			"the join-side gap — a later joiner receives nothing until the share is re-authored")
	}
}
