package shellcmd

import (
	"context"
	"fmt"
	"strings"

	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

// followChainInboxBase returns the parent path under which this
// follow chain's inboxes are bound. Used to scope the chain cap's
// system/inbox:receive grant.
func followChainInboxBase(remoteID, prefix string) string {
	return "system/inbox/follow/" + remoteID + "/" + sanitizeChainSlug(prefix)
}

// sanitizeChainSlug converts a tree prefix into a filesystem-safe
// slug for use in capability paths and chain-error namespace keys.
// Lowercase; non-alphanumeric runs collapse to single hyphens.
func sanitizeChainSlug(s string) string {
	var b strings.Builder
	lastHyphen := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteRune('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// cmdRevisionFollow installs the canonical 2-step incremental
// content-mirror continuation chain — `revision:fetch-diff →
// tree:merge` — that auto-mirrors a remote peer's subtree state
// when its revision head advances. This is the GUIDE-REVISION-AUTO-
// VERSION §4 "Form 1" recipe: chain-expressible, single-dynamic-
// field, O(diff) bandwidth per advance.
//
// **Why this shape (REVISION v3.4):** `revision:fetch-diff` (§4.4.19)
// bundles only the content closure between the caller's base version
// and the remote's current head. Combined with `tree:merge`, two
// chain steps cover the bandwidth-efficient happy path. The earlier
// `tree:extract` recipe (full closure on every commit) was retired
// in favor of this once v3.4 ratified + cross-impl validated.
//
// Chain shape:
//
//  0. SubscribeRawAt(remote, system/revision/{prefix-hash}/head,
//     deliverTo = local fetch-diff inbox).
//  1. Cross-peer continuation: `revision:fetch-diff` on the remote
//     with `prefix=<static>`, `base=$notification.previous_hash`
//     (single dynamic field via inject mode + extract). Dispatch cap
//     is B-rooted re-attenuation (V7 §4.2 case 3) scoped to
//     `system/revision:fetch-diff`.
//  2. Local continuation: `tree:merge`. `ResultField:
//     "source_envelope"` plumbs the fetch-diff envelope into merge's
//     params. Strategy `source-wins` for follower semantics.
//
// **Form 1 caveat — reliable delivery required.** Per
// GUIDE-REVISION-AUTO-VERSION §4.1: `base=$notification.previous_hash`
// is correct ONLY when no notifications drop. Under unreliable
// delivery, `previous_hash` can be ahead of B's actual head and the
// returned diff is incomplete (silent mixed-state). The drop-tolerant
// "Form 2" reads B's own head via a thin custom handler — not chain-
// expressible. Workbench picks Form 1 for the cross-impl POC topology;
// production-on-unreliable-links would want Form 2.
//
// **What this gives up vs DAG-mirror:** revision DAG state (head
// pointer + version entries + branch refs) is not converged by this
// chain — only tree content under the remote peer's namespace is.
// For workspace-mirror semantics (the common case) that's what
// callers want. For DAG-mirror semantics (each peer integrates the
// other's commit DAG into its own), use a `subscribe head →
// revision:pull` chain instead (REVISION §4.4.8). The bidirectional
// E2E tests `shellcmd/cmd_local_files_bidirectional_test.go::
// installFollow` and friends show the wiring.
func cmdRevisionFollow(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision follow <prefix> <remote-alias>")
	}
	prefix, alias := args[0], args[1]

	pc, ok := sh.Conns[alias]
	if !ok {
		return Result{}, fmt.Errorf("not connected: %s", alias)
	}
	if pc.PeerID == sh.Local.PeerID {
		return Result{}, fmt.Errorf("cannot follow self")
	}

	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	local := sh.Local.Peer
	remoteID := pc.PeerID
	ctx := context.Background()

	rawSub, err := InstallRevisionFollowChain(ctx, local, remoteID, prefix)
	if err != nil {
		return Result{}, err
	}

	fetchPath := followFetchPath(remoteID, prefix)
	mergePath := followMergePath(remoteID, prefix)

	// Bootstrap: fetch the remote's full current closure (base=zero)
	// and merge locally, without waiting for the next commit.
	// Subsequent commits are picked up by the standing chain
	// incrementally. Per GUIDE-REVISION-AUTO-VERSION §4.3.
	//
	// "Locally" is literal, and it used to say "under the remote's
	// namespace", which was never true: the prefix is relative, so
	// `NamespacedIndex.canonicalize` resolves it against US and the
	// mirror lands at `/{us}/{prefix}`. That gap between the comment
	// and the code is W2 of
	// REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17 — the intent
	// was right and the destination was not. `revision mirror` (see
	// InstallRevisionMirrorChain) is the form that does what this
	// comment claimed.
	if err := bootstrapFollow(ctx, local, remoteID, prefix); err != nil {
		return LinesResult([]string{
			fmt.Sprintf("following %s @ %s (sub=%s)", prefix, alias, rawSub.ID()),
			fmt.Sprintf("  fetch-diff inbox: %s", fetchPath),
			fmt.Sprintf("  merge inbox:      %s", mergePath),
			fmt.Sprintf("  WARN: bootstrap failed: %v (future commits will trigger the chain)", err),
		}), nil
	}

	return LinesResult([]string{
		fmt.Sprintf("following %s @ %s (sub=%s)", prefix, alias, rawSub.ID()),
		fmt.Sprintf("  fetch-diff inbox: %s", fetchPath),
		fmt.Sprintf("  merge inbox:      %s", mergePath),
		"  bootstrap: ok",
	}), nil
}

// cmdRevisionMirror is `revision follow` with the destination the
// multi-peer model calls for: the remote's subtree lands under
// `/{them}/…` in our tree, at the publisher's own path, instead of
// under our own namespace.
//
// The prefix argument is optional. Omitted, it mirrors the whole
// subtree the publisher declares in its signed published-root; given,
// it must name a subtree of that declaration — a prefix the publisher
// has not signed a root over is refused rather than mirrored to a path
// they never claimed.
//
// Pre-condition beyond `revision follow`'s: the remote must have
// published a signed `system/peer/published-root`. That is where the
// destination comes from, and there is no fallback by design.
//
// Tear-down is `revision unfollow <source-prefix> <alias>` — the chain
// inboxes are keyed by the SOURCE prefix, which is why the source
// prefix is printed even when the caller supplied none.
func cmdRevisionMirror(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: revision mirror <remote-alias> [prefix]")
	}
	alias := args[0]
	prefix := ""
	if len(args) > 1 {
		prefix = args[1]
	}

	pc, ok := sh.Conns[alias]
	if !ok {
		return Result{}, fmt.Errorf("not connected: %s", alias)
	}
	if pc.PeerID == sh.Local.PeerID {
		return Result{}, fmt.Errorf("cannot mirror self")
	}

	local := sh.Local.Peer
	remoteID := pc.PeerID
	ctx := context.Background()

	dest, err := local.MirrorDestination(ctx, remoteID, prefix)
	if err != nil {
		return Result{}, err
	}
	rawSub, err := InstallRevisionMirrorChain(ctx, local, remoteID, prefix)
	if err != nil {
		return Result{}, err
	}

	lines := []string{
		fmt.Sprintf("mirroring %s @ %s (sub=%s)", dest.SourcePrefix, alias, rawSub.ID()),
		fmt.Sprintf("  destination:      %s", dest.TargetPrefix),
		fmt.Sprintf("  publisher root:   %s (seq %d)",
			dest.PublishedRoot.Data.RootHash, dest.PublishedRoot.Data.Seq),
		fmt.Sprintf("  fetch-diff inbox: %s", followFetchPath(remoteID, dest.SourcePrefix)),
		fmt.Sprintf("  merge inbox:      %s", followMergePath(remoteID, dest.SourcePrefix)),
	}
	if err := bootstrapMirror(ctx, local, remoteID, prefix); err != nil {
		return LinesResult(append(lines,
			fmt.Sprintf("  WARN: bootstrap failed: %v (future commits will trigger the chain)", err))), nil
	}
	return LinesResult(append(lines, "  bootstrap: ok")), nil
}

// InstallRevisionFollowChain installs the canonical 2-step
// `revision:fetch-diff → tree:merge` follow chain that auto-mirrors
// `prefix` from `remoteID` whenever the remote's revision head
// advances. Returns the head-path subscription so the caller can
// cancel it on tear-down.
//
// Same chain shape as
// `entitysdk/tree_follow_since_wiring_test.go::TestTreeFollowSinceWiring_RevisionFetchDiff`
// — that test is the proven prior art and the regression assertion
// for this code path.
//
// See `cmdRevisionFollow` for the full design notes (Form 1 vs Form
// 2 trade-off; DAG-mirror separation).
func InstallRevisionFollowChain(ctx context.Context, local *entitysdk.AppPeer, remoteID, prefix string) (*entitysdk.RawSubscription, error) {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return installFollowChain(ctx, local, remoteID, prefix, prefix, local.OwnerCapability())
}

// InstallRevisionMirrorChain is InstallRevisionFollowChain with the
// destination the multi-peer model calls for: the standing chain
// merges each delta into the PUBLISHER's namespace at the publisher's
// own path (`/{them}/…`), not into ours.
//
// It is the follow-chain half of §5.1 step 3 of
// REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17, and it takes
// both of that step's constraints from the SDK rather than restating
// them: `MirrorDestination` derives the target from the publisher's
// signed `published-root` (never a caller-chosen string), and
// `MintMirrorCapability` supplies the namespace-scoped cap the merge
// step needs — the owner self-cap cannot authorize a `/{them}/…`
// write, and a chain whose merge step 403s does it silently, into an
// error inbox nobody is watching.
//
// prefix may be empty to mirror the publisher's whole declared
// subtree.
func InstallRevisionMirrorChain(ctx context.Context, local *entitysdk.AppPeer, remoteID, prefix string) (*entitysdk.RawSubscription, error) {
	dest, err := local.MirrorDestination(ctx, remoteID, prefix)
	if err != nil {
		return nil, err
	}
	mergeCap, err := local.MintMirrorCapability(remoteID)
	if err != nil {
		return nil, err
	}
	return installFollowChain(ctx, local, remoteID, dest.SourcePrefix, dest.TargetPrefix, mergeCap)
}

// installFollowChain is the shared body. sourcePrefix is what we pull
// from the remote; targetPrefix is where it lands locally; mergeCap is
// the capability the local merge step dispatches under. The two
// prefixes are separate parameters precisely so the destination can be
// somewhere other than the source's own relative path.
func installFollowChain(ctx context.Context, local *entitysdk.AppPeer, remoteID, sourcePrefix, targetPrefix string, mergeCapEnt entity.Entity) (*entitysdk.RawSubscription, error) {
	prefix := sourcePrefix
	localID := local.PeerID()
	fetchPath := followFetchPath(remoteID, prefix)
	mergePath := followMergePath(remoteID, prefix)

	localCap := mergeCapEnt.ContentHash
	crossPeerGrants := []types.GrantEntry{{
		Handlers:   types.CapabilityScope{Include: []string{"system/revision"}},
		Operations: types.CapabilityScope{Include: []string{"fetch-diff"}},
		Resources:  types.CapabilityScope{Include: []string{"*"}},
	}}
	crossPeerCapEnt, err := local.MintCrossPeerChainCapability(remoteID, crossPeerGrants, nil)
	if err != nil {
		return nil, fmt.Errorf("mint cross-peer follow-chain cap: %w", err)
	}
	crossPeerCap := crossPeerCapEnt.ContentHash

	// Step 2 (local merge inbox): tree:merge with the fetch-diff
	// envelope as source_envelope. Strategy source-wins because the
	// follower's job is to mirror the source's state.
	mergeParams, err := cbor.Marshal(types.MergeRequestData{
		Strategy:     "source-wins",
		SourcePrefix: prefix,
		TargetPrefix: targetPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("encode merge params: %w", err)
	}
	// OnError routes a non-2xx merge response to a chain-error inbox
	// instead of letting it silently cascade. Without this, a malformed
	// envelope or capability failure on merge returns a code+message
	// that the chain framework forwards as a successful "advance" with
	// no observable surface — operators see "chain stalled silently"
	// (cost evidence: today's session, lost-error marker bound at
	// system/runtime/chain-errors/lost/.../snapshot_not_found/... was
	// the only signal that anything had failed). Per the cross-impl
	// chain-diagnostics lessons.
	mergeErrorPath := "system/inbox/follow-errors/" + remoteID + "/" + prefixSlug(prefix) + "/merge-errors"
	mergeData := types.ContinuationData{
		Target:      "system/tree",
		Operation:   "merge",
		Resource:    &types.ResourceTarget{Targets: []string{targetPrefix}},
		Params:      cbor.RawMessage(mergeParams),
		ResultField: "source_envelope",
		OnError: &types.DeliverySpec{
			URI:       fmt.Sprintf("entity://%s/%s", localID, mergeErrorPath),
			Operation: "receive",
		},
	}

	// Step 1 (cross-peer fetch-diff inbox): revision:fetch-diff with
	// prefix=static, base=$notification.previous_hash.
	//   Params=FetchDiffParams{Prefix: prefix}            ← static
	//   ResultTransform.Extract = "previous_hash"         ← navigate
	//                                                       notification
	//                                                       to its
	//                                                       previous_hash
	//                                                       field
	//   ResultField = "base"                              ← inject the
	//                                                       extracted hash
	//                                                       as fetch-diff's
	//                                                       base param
	fetchParams, err := cbor.Marshal(types.RevisionFetchDiffParamsData{Prefix: prefix})
	if err != nil {
		return nil, fmt.Errorf("encode fetch-diff params: %w", err)
	}
	// OnError on the fetch step is the critical observability fix.
	// Without it, a 400 from fetch-diff (e.g., Python's
	// base_not_a_version on incremental base hashes — F-CIMP-7) is
	// forwarded as the merge step's source_envelope, where merge tries
	// to decode an error response as a snapshot and produces a
	// misleading 404 snapshot_not_found. With OnError, the failure
	// surfaces at fetch-error inbox where operators can browse it
	// directly. See REPORT-CROSSIMPL-CHAIN-DIAGNOSTICS-LESSONS-...md §2.
	fetchErrorPath := "system/inbox/follow-errors/" + remoteID + "/" + prefixSlug(prefix) + "/fetch-errors"
	fetchData := types.ContinuationData{
		Target:    fmt.Sprintf("entity://%s/system/revision", remoteID),
		Operation: "fetch-diff",
		Resource:  &types.ResourceTarget{Targets: []string{prefix}},
		Params:    cbor.RawMessage(fetchParams),
		ResultTransform: &types.ContinuationTransformData{
			Extract: "previous_hash",
		},
		ResultField: "base",
		DeliverTo: &types.DeliverySpec{
			URI:       fmt.Sprintf("entity://%s/%s", localID, mergePath),
			Operation: "receive",
		},
		OnError: &types.DeliverySpec{
			URI:       fmt.Sprintf("entity://%s/%s", localID, fetchErrorPath),
			Operation: "receive",
		},
	}

	entitysdk.SetDefaultDispatchCap(localCap, &mergeData)
	entitysdk.SetDefaultDispatchCap(crossPeerCap, &fetchData)

	mergeCont, err := mergeData.ToEntity()
	if err != nil {
		return nil, fmt.Errorf("encode merge continuation: %w", err)
	}
	if _, err := local.Continuation().Install(ctx, mergePath, mergeCont); err != nil {
		return nil, fmt.Errorf("install merge continuation: %w", err)
	}
	fetchCont, err := fetchData.ToEntity()
	if err != nil {
		return nil, fmt.Errorf("encode fetch-diff continuation: %w", err)
	}
	if _, err := local.Continuation().Install(ctx, fetchPath, fetchCont); err != nil {
		return nil, fmt.Errorf("install fetch-diff continuation: %w", err)
	}

	// Subscribe on the remote head so each commit triggers the chain.
	headPath := entitysdk.RevisionHeadPath(remoteID, prefix)
	deliverURI := fmt.Sprintf("entity://%s/%s", localID, fetchPath)
	rawSub, err := local.SubscribeRawAt(remoteID, headPath, deliverURI, "receive",
		entitysdk.SubscribeOpts{Events: []string{"created", "updated"}})
	if err != nil {
		return nil, fmt.Errorf("subscribe on remote head: %w", err)
	}
	return rawSub, nil
}

// bootstrapFollow runs one synchronous content-mirror cycle so the
// local tree reflects the remote's current state without waiting
// for the next commit. base=zero = full current closure per REVISION
// v3.4 §4.4.19 + GUIDE-REVISION-AUTO-VERSION §4.3.
//
// This was a hand-rolled fetch-diff + tree:merge pair, byte-for-byte
// the body of entitysdk.ReconcileSinceLastSeen. Two copies of the
// same chain is one copy too many — when the SDK's grew a separate
// target prefix, this one would have been left behind silently.
func bootstrapFollow(ctx context.Context, local *entitysdk.AppPeer, remoteID, prefix string) error {
	if _, err := local.ReconcileSinceLastSeen(ctx, remoteID, prefix, hash.Hash{}); err != nil {
		return fmt.Errorf("bootstrap follow %s prefix=%s: %w", remoteID, prefix, err)
	}
	return nil
}

// bootstrapMirror is bootstrapFollow's mirror-destination sibling: the
// initial pull lands in the publisher's namespace, matching where the
// standing chain installed by InstallRevisionMirrorChain will put
// every subsequent delta.
func bootstrapMirror(ctx context.Context, local *entitysdk.AppPeer, remoteID, prefix string) error {
	if _, err := local.MirrorSinceLastSeen(ctx, remoteID, prefix, hash.Hash{}); err != nil {
		return fmt.Errorf("bootstrap mirror %s prefix=%s: %w", remoteID, prefix, err)
	}
	return nil
}

// cmdRevisionUnfollow tears down the chain installed by cmdRevisionFollow:
// cancels the head subscription and abandons the two continuations.
// Idempotent — missing pieces are ignored.
func cmdRevisionUnfollow(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision unfollow <prefix> <remote-alias>")
	}
	prefix, alias := args[0], args[1]
	pc, ok := sh.Conns[alias]
	if !ok {
		return Result{}, fmt.Errorf("not connected: %s", alias)
	}

	local := sh.Local.Peer
	remoteID := pc.PeerID
	localID := local.PeerID()
	ctx := context.Background()
	headPattern := entitysdk.RevisionHeadPath(remoteID, prefix)
	fetchPath := followFetchPath(remoteID, prefix)
	mergePath := followMergePath(remoteID, prefix)
	deliverURI := fmt.Sprintf("entity://%s/%s", localID, fetchPath)

	// Find any subscriptions whose deliver URI matches the fetch inbox
	// for this prefix/remote and cancel them.
	cancelled := 0
	subs, err := local.Subscriptions().List(ctx)
	if err == nil {
		for _, s := range subs {
			if s.Pattern == headPattern && s.DeliverURI == deliverURI {
				cancelReq := types.SubscriptionCancelData{SubscriptionID: s.SubscriptionID}
				ent, _ := cancelReq.ToEntity()
				if _, err := local.Executor().ExecuteWithParams(
					"system/subscription", "unsubscribe", ent,
				); err == nil {
					cancelled++
				}
			}
		}
	}

	// Abandon each continuation step. Each abandon is best-effort —
	// a missing step is fine (the chain may already be partially torn
	// down or never fully installed).
	abandoned := 0
	for _, path := range []string{fetchPath, mergePath} {
		if err := local.Continuation().Abandon(ctx, path); err == nil {
			abandoned++
		}
	}

	return MessageResult(fmt.Sprintf("unfollowed %s @ %s (subs cancelled: %d, steps abandoned: %d)",
		prefix, alias, cancelled, abandoned)), nil
}

// cmdRevisionPush wraps RevisionClient.Push — sends locally-known
// versions for prefix to the named remote.
func cmdRevisionPush(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision push <remote-alias> <prefix>")
	}
	alias, prefix := args[0], args[1]
	pc, ok := sh.Conns[alias]
	if !ok {
		return Result{}, fmt.Errorf("not connected: %s", alias)
	}
	res, err := sh.Local.Peer.Revision().Push(context.Background(), types.RevisionPushParamsData{
		Remote: pc.PeerID,
		Prefix: prefix,
	})
	if err != nil {
		return Result{}, fmt.Errorf("revision push: %w", err)
	}
	lines := []string{fmt.Sprintf("push status: %s", res.Status)}
	if res.Versions != nil {
		lines = append(lines, fmt.Sprintf("versions: %d", *res.Versions))
	}
	if res.Message != "" {
		lines = append(lines, "msg: "+res.Message)
	}
	return LinesResult(lines), nil
}

// followFetchPath / followMergePath compute the canonical inbox
// paths for the revision-follow chain keyed by remote peer-id +
// prefix. Two steps: cross-peer fetch-diff → local merge.
func followFetchPath(remoteID, prefix string) string {
	return "system/inbox/follow/" + remoteID + "/" + prefixSlug(prefix) + "/fetch-diff"
}

func followMergePath(remoteID, prefix string) string {
	return "system/inbox/follow/" + remoteID + "/" + prefixSlug(prefix) + "/merge"
}

// prefixSlug renders a prefix as a path segment safe for nesting
// inside an inbox path — strips leading + trailing slashes,
// preserves internal slashes.
func prefixSlug(prefix string) string {
	return strings.Trim(prefix, "/")
}
