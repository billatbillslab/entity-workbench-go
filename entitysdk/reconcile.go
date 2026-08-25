package entitysdk

// State catch-up after silent saturation drops or peer-restart downtime.
// Wraps the documented `revision:fetch-diff + tree:merge` chain from
// GUIDE-CONTINUATIONS-WORKBENCH §5 + REVISION v3.4 §4.4.19 as a single
// SDK call.
//
// Use cases:
//   1. After a saturation burst: subscription dropped some notifications;
//      caller wants to make sure local tree state matches publisher.
//   2. After a peer restart: caller called RestorePriorSubscriptions but
//      still missed writes that happened during downtime.
//   3. Periodic reconciliation: belt-and-suspenders for long-lived
//      collaborative workspaces.

import (
	"context"
	"fmt"
	"strings"

	"github.com/fxamacker/cbor/v2"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// ReconcileResult summarizes a reconciliation pass.
type ReconcileResult struct {
	// Prefix that was reconciled.
	Prefix string

	// RemotePeerID we reconciled against.
	RemotePeerID string

	// BaseHash supplied to FetchDiff (zero = full closure).
	BaseHash hash.Hash

	// EntitiesIngested counts entities pulled across the wire (trie
	// nodes + leaves combined). A rough measure of "how much state
	// changed since the base."
	EntitiesIngested int

	// TargetPrefix is where the pulled subtree was merged. For
	// ReconcileSinceLastSeen it equals Prefix, which — being relative
	// — resolves into the LOCAL namespace. For MirrorSinceLastSeen it
	// is absolute and lands in the publisher's own namespace.
	TargetPrefix string
}

// MirrorResult is a ReconcileResult plus the published-root the
// destination was derived from.
type MirrorResult struct {
	ReconcileResult

	// PublishedRoot is the verified published-root that supplied the
	// publisher's declared prefix. Its RootHash is the root the
	// publisher signed at the moment we resolved the destination; a
	// caller doing its own chain verification starts there.
	PublishedRoot PublishedRoot
}

// ReconcileSinceLastSeen pulls the delta between the caller-supplied
// base hash and the remote peer's current head for prefix, applying
// the result via tree:merge. Wraps the chain documented in
// GUIDE-CONTINUATIONS-WORKBENCH §5.
//
// Pre-conditions:
//   - Local peer has an open connection to remotePeerID
//     (AppPeer.Connect).
//   - Both peers agree on prefix.
//
// `lastSeen = hash.Hash{}` (zero) reconciles against the full current
// closure — equivalent to a bootstrap pull. Pass the last-seen
// revision hash for an incremental sync.
//
// Closes Stage 5 findings F3 (no implicit catch-up after saturation
// drops) and F7 (missed writes recoverable but only via explicit
// pull) from the consumer-side.
func (a *AppPeer) ReconcileSinceLastSeen(ctx context.Context, remotePeerID, prefix string, lastSeen hash.Hash) (ReconcileResult, error) {
	return a.reconcile(ctx, remotePeerID, prefix, prefix, lastSeen)
}

// MirrorSinceLastSeen is ReconcileSinceLastSeen with the destination
// the multi-peer model actually calls for: the pulled subtree lands in
// the PUBLISHER's namespace, at the publisher's own path, rather than
// at the same relative prefix inside ours.
//
// Why this exists as a second entry point rather than a `target`
// argument. `ReconcileSinceLastSeen` sets source and target to the one
// prefix it is given, and because every caller passes a relative
// prefix ("watched/", "collab/", "mirror/"),
// `NamespacedIndex.canonicalize` resolves it against the LOCAL peer —
// so Bob's subtree, followed by Alice, lands at `/{alice}/watched/…`.
// That is `entity-browser-rust`'s F1 reproduced on our sync surface
// (W2 of REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17): under
// V7 §1.4 a followed subtree is a cached copy of a peer's own data and
// belongs at `/{them}/…`, at the path they use for it.
//
// The fix needs no wire change — `applyPrefix` (core/tree/operations.go)
// is plain concatenation, and an absolute `target_prefix` passes
// through the local index untouched. What was missing was entirely our
// API shape, which could not express source ≠ target.
//
// **The destination is derived, never supplied.** Arch's amendment to
// §5.1 step 3, adopted verbatim: an API that takes an arbitrary target
// string gets pointed at `app/share/` by the first caller who reads a
// convention as a path. So the anchor comes from the publisher's own
// signed `system/peer/published-root` — its `prefix` field
// (EXTENSION-TREE §3.3a, REQUIRED) is what the publisher declares its
// keys are relative to, and this call refuses any source prefix
// outside it. Passing an empty prefix mirrors the whole declared
// subtree.
//
// **What this does and does not authenticate.** It authenticates the
// DESTINATION: the prefix the data lands under is one the publisher
// signed, read through ReadPublishedRoot's full verification. It does
// NOT yet verify that the merged content hash-chains from the signed
// `root_hash` — `revision:fetch-diff` returns an envelope we merge on
// the strength of the connection, exactly as ReconcileSinceLastSeen
// always has. Closing that is the next rung and is stated here rather
// than left to be assumed from the word "verified".
func (a *AppPeer) MirrorSinceLastSeen(ctx context.Context, remotePeerID, prefix string, lastSeen hash.Hash) (MirrorResult, error) {
	dest, err := a.MirrorDestination(ctx, remotePeerID, prefix)
	if err != nil {
		return MirrorResult{}, err
	}
	mergeCap, err := a.MintMirrorCapability(remotePeerID)
	if err != nil {
		return MirrorResult{}, err
	}
	res, err := a.reconcileAs(ctx, mergeCap, remotePeerID, dest.SourcePrefix, dest.TargetPrefix, lastSeen)
	return MirrorResult{ReconcileResult: res, PublishedRoot: dest.PublishedRoot}, err
}

// MirrorDest is where a mirror of a remote peer's subtree belongs,
// resolved from that peer's own signed published-root.
type MirrorDest struct {
	// SourcePrefix is the prefix to pull from the publisher, relative
	// to their namespace.
	SourcePrefix string

	// TargetPrefix is the absolute local path the pull lands at:
	// /{them}/{SourcePrefix}.
	TargetPrefix string

	// PublishedRoot is the verified root the destination came from.
	PublishedRoot PublishedRoot
}

// MirrorDestination resolves the source and target prefixes for
// mirroring remotePeerID's subtree, from the publisher's signed
// published-root. An empty prefix means the publisher's whole declared
// subtree.
//
// This is the single place the destination is derived. Both the
// one-shot pull (MirrorSinceLastSeen) and the standing follow chain
// (shellcmd.InstallRevisionMirrorChain) go through it, because two
// derivations of "where does their data live in our tree" is one more
// than can stay in agreement.
func (a *AppPeer) MirrorDestination(ctx context.Context, remotePeerID, prefix string) (MirrorDest, error) {
	if remotePeerID == "" {
		return MirrorDest{}, NewError(400, "invalid_peer", "remotePeerID is empty")
	}
	if remotePeerID == a.PeerID() {
		return MirrorDest{}, NewError(400, "invalid_peer",
			"cannot mirror from self — the destination would be the source")
	}
	pr, err := a.ReadPublishedRoot(ctx, remotePeerID)
	if err != nil {
		return MirrorDest{}, err
	}
	source, err := mirrorSourcePrefix(prefix, pr.Data.Prefix)
	if err != nil {
		return MirrorDest{}, err
	}
	return MirrorDest{
		SourcePrefix:  source,
		TargetPrefix:  "/" + remotePeerID + "/" + source,
		PublishedRoot: pr,
	}, nil
}

// MintMirrorCapability mints the self-capability a mirror's merge step
// runs under: system/tree merge+put over /{remotePeerID}/* and nothing
// else.
//
// It is needed because the peer's owner self-cap cannot express it.
// The owner cap's resources are `["*"]`, and §PR-8 (V7 §5.5) makes a
// bare `*` in a capability RESOURCE peer-LOCAL — it canonicalizes to
// `/{me}/*`, so `tree:merge`'s per-path put pre-check
// (core/tree/operations.go) refuses every `/{them}/…` target with a
// 403. Writing into our own cached copy of another peer's namespace is
// V7 §1.4 layer-1 authority over our own local tree, but it has to be
// said in a form the capability grammar can carry, and the only such
// form is an absolute named-peer resource.
func (a *AppPeer) MintMirrorCapability(remotePeerID string) (entity.Entity, error) {
	if remotePeerID == "" {
		return entity.Entity{}, NewError(400, "invalid_peer", "remotePeerID is empty")
	}
	capEnt, err := a.MintChainCapability([]types.GrantEntry{{
		Handlers:   types.CapabilityScope{Include: []string{"system/tree"}},
		Operations: types.CapabilityScope{Include: []string{"merge", "put"}},
		Resources:  types.CapabilityScope{Include: []string{"/" + remotePeerID + "/*"}},
	}})
	if err != nil {
		return entity.Entity{}, WrapError(statusOrDefault(err, 500), "mint_mirror_cap",
			"mint the mirror-namespace capability", err)
	}
	return capEnt, nil
}

// mirrorSourcePrefix resolves the caller's requested prefix against
// the publisher's declared one: empty means "all of it", and anything
// outside it is refused rather than silently mirrored to a path the
// publisher never claimed.
func mirrorSourcePrefix(requested, declared string) (string, error) {
	if declared == "" {
		return "", NewError(500, "missing_prefix",
			"publisher's published-root declares no prefix")
	}
	// "/" designates the universal tree (EXTENSION-TREE §3.3a), which
	// is not itself a usable relative source prefix — it means the
	// publisher tracks everything, so the caller must name a subtree.
	universal := declared == "/"
	if requested == "" {
		if universal {
			return "", NewError(400, "invalid_prefix",
				"publisher declares the universal tree (\"/\"); name the subtree to mirror")
		}
		return declared, nil
	}
	if !strings.HasSuffix(requested, "/") {
		requested += "/"
	}
	if !universal && !strings.HasPrefix(requested, declared) {
		return "", NewError(400, "prefix_not_published",
			fmt.Sprintf("prefix %q is outside the publisher's declared prefix %q — "+
				"the publisher has not signed a root covering it", requested, declared))
	}
	return requested, nil
}

// reconcile is the shared body: pull the delta for sourcePrefix from
// the remote and merge it at targetPrefix. The two prefixes are
// separate parameters precisely so the destination can be somewhere
// other than the source's own relative path.
func (a *AppPeer) reconcile(ctx context.Context, remotePeerID, sourcePrefix, targetPrefix string, lastSeen hash.Hash) (ReconcileResult, error) {
	return a.reconcileAs(ctx, entity.Entity{}, remotePeerID, sourcePrefix, targetPrefix, lastSeen)
}

// reconcileAs is reconcile under an explicit caller capability for the
// tree:merge step. A zero capability means "the executor's standing
// owner cap", which is what the same-namespace path wants.
func (a *AppPeer) reconcileAs(ctx context.Context, mergeCap entity.Entity, remotePeerID, sourcePrefix, targetPrefix string, lastSeen hash.Hash) (ReconcileResult, error) {
	prefix := sourcePrefix
	if remotePeerID == "" {
		return ReconcileResult{}, NewError(400, "invalid_peer", "remotePeerID is empty")
	}
	if prefix == "" {
		return ReconcileResult{}, NewError(400, "invalid_prefix", "prefix is empty")
	}
	if targetPrefix == "" {
		return ReconcileResult{}, NewError(400, "invalid_prefix", "targetPrefix is empty")
	}

	res := ReconcileResult{
		Prefix:       prefix,
		RemotePeerID: remotePeerID,
		BaseHash:     lastSeen,
		TargetPrefix: targetPrefix,
	}

	// 1. revision:fetch-diff against the remote. Side effect:
	//    env.Included is already ingested into the local content
	//    store by the FetchDiff wrapper.
	envEnt, err := a.RevisionAt(remotePeerID).FetchDiff(ctx, types.RevisionFetchDiffParamsData{
		Prefix: prefix,
		Base:   lastSeen,
	})
	if err != nil {
		// Preserve the underlying status per SDK-OPERATIONS §12.3 —
		// flattening a 403 into a 500 hides an authorization failure
		// behind an internal-error, which is exactly the distinction a
		// caller needs to act on.
		return res, WrapError(statusOrDefault(err, 500), "fetch_diff_failed",
			fmt.Sprintf("revision:fetch-diff %s prefix=%s", remotePeerID, prefix), err)
	}

	// Decode the envelope shape just for the metric — duplicates a
	// small amount of work FetchDiff already did. The wrapper doesn't
	// expose the count so we re-decode rather than alter its surface.
	var env entity.Envelope
	if err := ecf.Decode(envEnt.Data, &env); err == nil {
		res.EntitiesIngested = len(env.Included)
	}

	// 2. tree:merge the envelope into local prefix. Same shape as
	//    shellcmd/cmd_revision_follow.go::bootstrapFollow.
	envEntRaw, err := cbor.Marshal(envEnt)
	if err != nil {
		return res, WrapError(500, "encode_envelope", "marshal fetch-diff envelope", err)
	}
	mergeReq := types.MergeRequestData{
		Strategy:       "source-wins",
		SourcePrefix:   prefix,
		TargetPrefix:   targetPrefix,
		SourceEnvelope: cbor.RawMessage(envEntRaw),
	}
	mergeParamEnt, err := mergeReq.ToEntity()
	if err != nil {
		return res, WrapError(500, "encode_merge_params", "encode tree:merge params", err)
	}
	resp, err := a.executor.executeAs(mergeCap, "system/tree", "merge", mergeParamEnt, nil)
	if err != nil {
		return res, WrapError(statusOrDefault(err, 500), "tree_merge_failed", "tree:merge failed", err)
	}
	if resp == nil {
		return res, NewError(500, "nil_merge_response", "tree:merge returned nil response")
	}
	if resp.Status >= 400 {
		return res, NewError(resp.Status, "tree_merge_status",
			fmt.Sprintf("tree:merge status=%d", resp.Status))
	}
	return res, nil
}

// statusOrDefault returns err's SDK status when it carries one, else
// def. Used where a wrapper adds context to an error that already has
// a meaningful status.
func statusOrDefault(err error, def uint) uint {
	if s := StatusOf(err); s != 0 {
		return s
	}
	return def
}
