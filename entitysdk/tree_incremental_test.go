package entitysdk_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// TestTreeIncrementalSync_ExistingOpsCompose probes whether the existing
// tree-extension op set composes into an efficient incremental sync using
// `tree:diff + tree:extract(Paths=...)`. Validates the data-flow story we
// plan to take to the arch team:
//
//  1. alice has snapshot X (after initial commit) and snapshot Y (after
//     a tiny incremental commit).
//  2. bob has X (synced via the canonical tree:extract → tree:merge).
//  3. bob asks alice: tree:diff(base=X, target=Y). Alice computes diff
//     from her local store (she has both). Returns DiffData with the
//     changed paths only.
//  4. bob asks alice: tree:extract(prefix, Paths=changed_paths). Alice
//     bundles only the changed paths + the trie nodes needed to walk to
//     them.
//  5. bob does tree:merge locally.
//
// The bandwidth assertion: step 4's envelope should be O(diff), not
// O(workspace). If true, the existing primitives compose into efficient
// incremental sync — we just need a way to drive step (4)'s Paths input
// from step (3)'s output (the missing transform op).
//
// If the bandwidth assertion fails, the existing ops don't compose
// efficiently and a new server-side op (e.g., tree:extract since=X) is
// required.
func TestTreeIncrementalSync_ExistingOpsCompose(t *testing.T) {
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

	const prefix = "deep/"
	aliceID := alice.PeerID()

	// Alice writes the full 50-leaf workspace + commits → snapshot X.
	const numSubdirs = 5
	const leavesPerSub = 10
	for sub := 0; sub < numSubdirs; sub++ {
		for leaf := 0; leaf < leavesPerSub; leaf++ {
			path := fmt.Sprintf("deep/sub-%d/leaf-%02d", sub, leaf)
			body := fmt.Sprintf("alice's leaf at %s", path)
			if _, err := alice.Put(path, "test/note", body); err != nil {
				t.Fatalf("alice put %s: %v", path, err)
			}
		}
	}
	commitX, err := alice.Revision().Commit(ctx, prefix, "initial")
	if err != nil {
		t.Fatalf("alice commit X: %v", err)
	}
	t.Logf("commit X (initial): version=%s", commitX.Version)

	// Bob does a full initial sync via Pull.
	if _, err := bob.Revision().Pull(ctx, prefix, aliceID); err != nil {
		t.Fatalf("bob initial pull: %v", err)
	}

	// Alice makes a tiny incremental change — overwrites ONE leaf.
	const targetLeaf = "deep/sub-2/leaf-05"
	if _, err := alice.Put(targetLeaf, "test/note", "alice's UPDATED leaf"); err != nil {
		t.Fatalf("alice update: %v", err)
	}
	commitY, err := alice.Revision().Commit(ctx, prefix, "tiny update")
	if err != nil {
		t.Fatalf("alice commit Y: %v", err)
	}
	t.Logf("commit Y (incremental): version=%s", commitY.Version)
	if commitX.Version == commitY.Version {
		t.Fatal("commit X and Y are the same — incremental commit didn't produce a new version")
	}

	// ---- step 1: bob asks alice for the revision-level diff(X, Y) ----
	//
	// revision:diff (vs tree:diff) takes VERSION hashes and computes the
	// diff between their trie roots server-side. tree:diff requires
	// system/tree/snapshot entities, which the revision DAG doesn't
	// create directly — version entities reference trie roots via their
	// `Root` field, not via snapshot entities. So revision:diff is the
	// right op for "diff between two committed versions."
	//
	// Bandwidth: result is metadata only — paths + hashes, no content.
	diffParams := types.RevisionDiffParamsData{
		Prefix: prefix,
		Base:   commitX.Version,
		Target: commitY.Version,
	}
	diffParamEnt, _ := diffParams.ToEntity()
	diffResultEnt, err := executeOnRemote(ctx, bob, aliceID, "system/revision", "diff", diffParamEnt)
	if err != nil {
		t.Fatalf("revision:diff: %v", err)
	}
	var diffData types.DiffData
	if err := decodeEntity(diffResultEnt, &diffData); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	t.Logf("tree:diff result: added=%d removed=%d changed=%d unchanged=%d",
		len(diffData.Added), len(diffData.Removed), len(diffData.Changed), diffData.Unchanged)
	if len(diffData.Changed) != 1 {
		t.Errorf("expected 1 changed path (the one leaf alice updated); got %d", len(diffData.Changed))
	}
	if _, ok := diffData.Changed["sub-2/leaf-05"]; !ok {
		t.Logf("changed map keys: %v", mapKeys(diffData.Changed))
	}

	// ---- step 2 (manual): aggregate diff path keys into Paths array ----
	//
	// This is the step transform_ops cannot currently express: collecting
	// map keys from DiffData.Added + .Changed into a []string. We do it
	// in Go here so the rest of the flow can run.
	var changedPaths []string
	for path := range diffData.Added {
		changedPaths = append(changedPaths, path)
	}
	for path := range diffData.Changed {
		changedPaths = append(changedPaths, path)
	}
	t.Logf("aggregated changed paths: %d entries", len(changedPaths))

	// ---- step 3: tree:extract(prefix, Paths=changedPaths) ----
	//
	// The diff-only extraction. Alice should bundle only the entities
	// for the changed paths + the trie nodes needed to walk to them.
	extractParams := types.ExtractRequestData{
		Prefix: prefix,
		Paths:  changedPaths,
	}
	extractParamEnt, _ := extractParams.ToEntity()
	extractResultEnt, err := executeOnRemote(ctx, bob, aliceID, "system/tree", "extract", extractParamEnt)
	if err != nil {
		t.Fatalf("tree:extract incremental: %v", err)
	}
	incrementalEntities := countEnvelopeIncluded(t, extractResultEnt)
	t.Logf("INCREMENTAL extract envelope: %d entities (for 1 changed leaf)", incrementalEntities)

	// ---- control: full extract (no paths filter) ----
	fullExtractParams := types.ExtractRequestData{Prefix: prefix}
	fullExtractParamEnt, _ := fullExtractParams.ToEntity()
	fullExtractResultEnt, err := executeOnRemote(ctx, bob, aliceID, "system/tree", "extract", fullExtractParamEnt)
	if err != nil {
		t.Fatalf("tree:extract full: %v", err)
	}
	fullEntities := countEnvelopeIncluded(t, fullExtractResultEnt)
	t.Logf("FULL extract envelope: %d entities (for the whole workspace)", fullEntities)

	// ---- bandwidth verdict ----
	if incrementalEntities >= fullEntities {
		t.Fatalf("incremental envelope is NOT smaller (%d) than full envelope (%d) — paths filter isn't helping",
			incrementalEntities, fullEntities)
	}
	ratio := float64(incrementalEntities) / float64(fullEntities)
	t.Logf("bandwidth ratio incremental/full: %.2f (smaller is better)", ratio)
	if ratio > 0.25 {
		t.Logf("WARN: incremental still pays %.0f%% of full — diff-aware extraction would do better", ratio*100)
	}
}
