package entitysdk_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

// TestTreeFollowSinceWiring_RevisionFetchDiff exercises the
// chain-expressible incremental-sync wiring against `revision:fetch-diff`
// (EXTENSION-REVISION v3.4) — the canonical replacement for the
// withdrawn `tree:extract.since` (EXTENSION-TREE v3.15). Putting the op
// in revision (which natively derefs version → root) and making target
// implicit (= executing peer's current head) lets the chain author wire
// `base=$notification.previous_hash` — a single dynamic field — into
// continuation inject-mode.
//
// Chain (canonical "Form 1" per GUIDE-REVISION-AUTO-VERSION §4):
//
//	subscribe alice's revision head
//	  → revision:fetch-diff(prefix=static, base=$notification.previous_hash) cross-peer to alice
//	  → tree:merge(source_envelope=$result) locally on bob
//
// **Form 1 caveat (the guide's footgun, not exercised here).** This
// test asserts convergence on the *reliable-in-order-delivery* happy
// path. `notification.previous_hash` equals the follower's actual
// current head only when no notifications are missed; under
// drop/reorder, `base=$previous_hash` computes an incomplete diff and
// `tree:merge` leaves the follower in a silent mixed state. The
// drop-tolerant alternative ("Form 2") reads the follower's own head
// via a thin custom handler — not chain-expressible, see the guide.
// Production should pick the form by delivery guarantees.
func TestTreeFollowSinceWiring_RevisionFetchDiff(t *testing.T) {
	alice, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alice.Close() })

	bob, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bob.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, ap := range []*entitysdk.AppPeer{alice, bob} {
		ready := make(chan struct{})
		errCh := make(chan error, 1)
		go func(p *entitysdk.AppPeer) { errCh <- p.ListenReady(ctx, ready) }(ap)
		select {
		case <-ready:
		case err := <-errCh:
			t.Fatalf("listen: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("listen timeout")
		}
	}
	if _, err := bob.Connect(ctx, alice.Addr().String()); err != nil {
		t.Fatalf("bob connect: %v", err)
	}
	if _, err := alice.Connect(ctx, bob.Addr().String()); err != nil {
		t.Fatalf("alice connect: %v", err)
	}

	const prefix = "deep/"
	aliceID := alice.PeerID()
	bobID := bob.PeerID()

	// Alice writes 50-leaf workspace + commits X. Bob pulls X so he has
	// the same version entry alice references as previous_hash on commit Y.
	for sub := 0; sub < 5; sub++ {
		for leaf := 0; leaf < 10; leaf++ {
			path := fmt.Sprintf("deep/sub-%d/leaf-%02d", sub, leaf)
			if _, err := alice.Put(path, "test/note", "leaf body"); err != nil {
				t.Fatalf("alice put: %v", err)
			}
		}
	}
	if _, err := alice.Revision().Commit(ctx, prefix, "initial"); err != nil {
		t.Fatalf("alice commit X: %v", err)
	}
	if _, err := bob.Revision().Pull(ctx, prefix, aliceID); err != nil {
		t.Fatalf("bob initial pull: %v", err)
	}

	localCap := bob.OwnerCapability().ContentHash
	// Cross-peer cap scoped to system/revision:fetch-diff on alice (not
	// system/tree:extract as the pre-Amendment-1 wiring used).
	// Resources use the explicit /{remotePeerID}/* form. Bare `*` would
	// canonicalize against the leaf's granter (bob) per V7 v7.73 §PR-8,
	// not the dispatch target (alice). MintCrossPeerChainCapability also
	// rewrites bare `*` to this form, but the canonical shape is named
	// here so the test documents what's required.
	crossPeerGrants := []types.GrantEntry{{
		Handlers:   types.CapabilityScope{Include: []string{"system/revision"}},
		Operations: types.CapabilityScope{Include: []string{"fetch-diff"}},
		Resources:  types.CapabilityScope{Include: []string{fmt.Sprintf("/%s/*", aliceID)}},
	}}
	crossPeerCapEnt, err := bob.MintCrossPeerChainCapability(aliceID, crossPeerGrants, nil)
	if err != nil {
		t.Fatalf("mint cross-peer cap: %v", err)
	}
	crossPeerCap := crossPeerCapEnt.ContentHash

	fetchDiffPath := "system/inbox/sincewire/" + aliceID + "/deep/fetch-diff"
	mergePath := "system/inbox/sincewire/" + aliceID + "/deep/merge"

	mergeParams, _ := cbor.Marshal(types.MergeRequestData{
		Strategy:     "source-wins",
		SourcePrefix: prefix,
		TargetPrefix: prefix,
	})
	mergeData := types.ContinuationData{
		Target:      "system/tree",
		Operation:   "merge",
		Resource:    &types.ResourceTarget{Targets: []string{prefix}},
		Params:      cbor.RawMessage(mergeParams),
		ResultField: "source_envelope",
	}

	// The chain-expressible Shape B wiring:
	//   Params=FetchDiffParams{Prefix=prefix}   ← static
	//   ResultTransform.Extract = "previous_hash"
	//   ResultField = "base"                    ← single dynamic field
	fetchDiffParams, _ := cbor.Marshal(types.RevisionFetchDiffParamsData{Prefix: prefix})
	fetchDiffData := types.ContinuationData{
		Target:    fmt.Sprintf("entity://%s/system/revision", aliceID),
		Operation: "fetch-diff",
		Resource:  &types.ResourceTarget{Targets: []string{prefix}},
		Params:    cbor.RawMessage(fetchDiffParams),
		ResultTransform: &types.ContinuationTransformData{
			Extract: "previous_hash",
		},
		ResultField: "base",
		DeliverTo: &types.DeliverySpec{
			URI:       fmt.Sprintf("entity://%s/%s", bobID, mergePath),
			Operation: "receive",
		},
	}

	entitysdk.SetDefaultDispatchCap(localCap, &mergeData)
	entitysdk.SetDefaultDispatchCap(crossPeerCap, &fetchDiffData)

	mergeCont, _ := mergeData.ToEntity()
	if _, err := bob.Continuation().Install(ctx, mergePath, mergeCont); err != nil {
		t.Fatalf("install merge: %v", err)
	}
	fetchDiffCont, _ := fetchDiffData.ToEntity()
	if _, err := bob.Continuation().Install(ctx, fetchDiffPath, fetchDiffCont); err != nil {
		t.Fatalf("install fetch-diff: %v", err)
	}

	// Subscribe alice's revision head → bob's fetch-diff inbox.
	headPattern := entitysdk.RevisionHeadPath(aliceID, prefix)
	deliverURI := fmt.Sprintf("entity://%s/%s", bobID, fetchDiffPath)
	rawSub, err := bob.SubscribeRawAt(aliceID, headPattern, deliverURI, "receive",
		entitysdk.SubscribeOpts{Events: []string{"created", "updated"}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = rawSub.Close() })

	// Alice's tiny change → commits Y. Notification fires the chain.
	const targetLeaf = "deep/sub-2/leaf-05"
	const updatedBody = "alice's UPDATED leaf"
	if _, err := alice.Put(targetLeaf, "test/note", updatedBody); err != nil {
		t.Fatalf("alice update: %v", err)
	}
	if _, err := alice.Revision().Commit(ctx, prefix, "tiny"); err != nil {
		t.Fatalf("alice commit Y: %v", err)
	}

	// Wait for the chain's tree:merge to bind the updated leaf. tree:merge
	// applies the source envelope under (executing peer's namespace +
	// TargetPrefix), so bob mirrors under /{bobID}/deep/ — same namespace
	// the eager Pull populated. Convergence here means the chain updated
	// the existing binding with commit-Y's body.
	mirroredPath := fmt.Sprintf("/%s/%s", bobID, targetLeaf)
	deadline := time.Now().Add(10 * time.Second)
	converged := false
	for time.Now().Before(deadline) {
		if h, ok := bob.RawLocationIndex().Get(mirroredPath); ok {
			if ent, ok := bob.Store().GetByHash(h); ok && len(ent.Data) > 0 {
				// Verify it's the updated body, not the initial commit X.
				var got string
				if err := cbor.Unmarshal(ent.Data, &got); err == nil && got == updatedBody {
					converged = true
					break
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Check for chain-error-lost markers — any indicates the chain
	// dispatched but a step returned non-2xx.
	errEntries := bob.RawLocationIndex().List("system/runtime/chain-errors/lost/")
	if len(errEntries) > 0 {
		for _, e := range errEntries {
			t.Logf("  chain-error-lost: %s", e.Path)
			if ent, ok := bob.Store().GetByHash(e.Hash); ok {
				var md types.ChainErrorLostData
				if err := cbor.Unmarshal(ent.Data, &md); err == nil {
					t.Logf("    reason=%q status=%d code=%q failed_uri=%q chain_id=%q step=%q target_peer=%q rejected_marker=%v",
						md.Reason, md.Status, md.Code, md.TargetURI,
						md.ChainID, md.StepIndex, md.TargetPeerID, md.RejectedMarkerHash)
				}
			}
		}
		// Probe Alice's side for the receiver-bound rejected marker.
		aliceRejected := alice.RawLocationIndex().List("system/runtime/chain-errors/rejected/")
		t.Logf("alice has %d rejected-marker entries", len(aliceRejected))
		for _, e := range aliceRejected {
			t.Logf("  alice rejected-marker: %s", e.Path)
			if ent, ok := alice.Store().GetByHash(e.Hash); ok {
				var md types.ChainErrorLostData
				if err := cbor.Unmarshal(ent.Data, &md); err == nil {
					t.Logf("    reason=%q status=%d code=%q failed_uri=%q chain_id=%q step=%q requesting_peer=%q attempted_uri=%q",
						md.Reason, md.Status, md.Code, md.TargetURI,
						md.ChainID, md.StepIndex, md.RequestingPeerID, md.AttemptedURI)
				}
			}
		}
		t.Fatalf("chain bound %d chain-error-lost markers; the fetch-diff wiring is rejected somewhere", len(errEntries))
	}

	if !converged {
		t.Fatalf("bob's mirror at %s did not converge to updated body within deadline", mirroredPath)
	}
}
