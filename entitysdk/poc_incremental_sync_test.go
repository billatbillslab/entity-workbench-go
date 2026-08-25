package entitysdk_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

// poc_incremental_sync_test.go — POC for the incremental-sync-chain handoff.
// Drives the 4-step incremental-sync recipe (Branch A: collect_keys +
// revision:diff-since-local-head) and the legacy full-extract baseline on
// the same workspace, then measures bytes/entities transferred and
// convergence latency. Both recipes are driven via direct dispatches; the
// `collect_keys` transform is exercised inline against real
// `system/tree/diff` output to validate it produces a `paths` array that
// `tree:extract(paths)` accepts. A follow-up exercise (separate test)
// installs the actual continuation chain end-to-end once Branch B
// (`tree:extract.since`) lands.
//
// Workspace: 50-leaf tree (5 subdirs × 10 leaves), 1-leaf tiny update.
// Same shape as TestTreeIncrementalSync_ExistingOpsCompose so numbers are
// directly comparable.

type pocMetrics struct {
	recipe              string
	entitiesTransferred int
	bytesTransferred    int
	rounds              int
	latency             time.Duration
}

func (m pocMetrics) log(t *testing.T) {
	t.Helper()
	t.Logf("[%s] entities=%d  bytes=%d  rounds=%d  latency=%s",
		m.recipe, m.entitiesTransferred, m.bytesTransferred, m.rounds, m.latency)
}

// TestPOC_IncrementalSyncRecipes stands up alice + bob, drives both the
// 4-step recipe (Branch A) and the legacy full-extract recipe against an
// identical workspace+commit sequence, and records side-by-side
// measurements per HANDOFF §3.1.7.
func TestPOC_IncrementalSyncRecipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	alice, bob := setupPeerPair(t, ctx)
	const prefix = "deep/"
	aliceID := alice.PeerID()

	// Alice writes 50-leaf workspace + commits → X.
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
	if _, err := bob.Revision().Pull(ctx, prefix, aliceID); err != nil {
		t.Fatalf("bob initial pull: %v", err)
	}

	// Alice's tiny incremental change.
	const targetLeaf = "deep/sub-2/leaf-05"
	if _, err := alice.Put(targetLeaf, "test/note", "alice's UPDATED leaf"); err != nil {
		t.Fatalf("alice update: %v", err)
	}
	commitY, err := alice.Revision().Commit(ctx, prefix, "tiny update")
	if err != nil {
		t.Fatalf("alice commit Y: %v", err)
	}
	if commitX.Version == commitY.Version {
		t.Fatal("incremental commit didn't produce a new version")
	}

	// Capture bob's "I already have this trie root" value — needed as
	// the `since` parameter for the 2-step recipe. After Pull, bob has
	// alice's commit-X version entry in his content store; the Root
	// field of that entry IS the trie root at the X point. This stands
	// in for the subscription.previous_hash that the production chain
	// would consume.
	bobBaseTrieRoot := loadVersionTrieRoot(t, bob, commitX.Version)

	// --- Recipe A: 4-step ---
	// **Engineering issue surfaced by this POC** (capture in report):
	// PROPOSAL-REVISION-DIFF-SINCE-LOCAL-HEAD.md §2 algorithm reads the
	// caller's local head via local tree.get + loadVersion(target). The
	// HANDOFF §2.1 calls step 1 "cross-peer revision:diff-since-local-head"
	// but the source-peer's "local head" is the target, not the follower's
	// base — the op cannot give a meaningful diff when dispatched cross-peer.
	// For the bandwidth measurement, the POC drives step 1 via the
	// existing cross-peer `revision:diff` with explicit base/target
	// (alice has both versions in her content store; follower can supply
	// its tracked head from its location index). The convenience op is
	// then exercised in its workable shape — locally on bob with target
	// pre-fetched — by a separate sub-test below.
	recipeA := drive4StepRecipe(t, ctx, bob, aliceID, prefix, commitX.Version, commitY.Version)
	recipeA.log(t)

	// --- Recipe B: 2-step (tree:extract.since + merge) ---
	recipeB := drive2StepRecipe(t, ctx, bob, aliceID, prefix, bobBaseTrieRoot)
	recipeB.log(t)

	// --- Baseline: full extract (the current production cmdRevisionFollow shape) ---
	recipeBaseline := driveFullExtract(t, ctx, bob, aliceID, prefix)
	recipeBaseline.log(t)

	// --- Bandwidth verdict ---
	if recipeA.entitiesTransferred >= recipeBaseline.entitiesTransferred {
		t.Fatalf("4-step recipe is NOT smaller (%d) than full extract (%d) — incremental claim invalid",
			recipeA.entitiesTransferred, recipeBaseline.entitiesTransferred)
	}
	if recipeB.entitiesTransferred >= recipeBaseline.entitiesTransferred {
		t.Fatalf("2-step recipe is NOT smaller (%d) than full extract (%d) — since-mode claim invalid",
			recipeB.entitiesTransferred, recipeBaseline.entitiesTransferred)
	}
	ratioA := float64(recipeA.entitiesTransferred) / float64(recipeBaseline.entitiesTransferred)
	ratioB := float64(recipeB.entitiesTransferred) / float64(recipeBaseline.entitiesTransferred)
	t.Logf("4-step / full entity ratio: %.3f  (target: < 0.05)", ratioA)
	t.Logf("2-step / full entity ratio: %.3f  (target: < 0.05)", ratioB)
	t.Logf("4-step vs 2-step byte delta: %d → %d  (one cross-peer round trip saved)",
		recipeA.bytesTransferred, recipeB.bytesTransferred)
	if ratioA > 0.10 {
		t.Errorf("4-step ratio %.3f exceeds 10%% — incremental story degraded vs the original probe", ratioA)
	}
	if ratioB > 0.10 {
		t.Errorf("2-step ratio %.3f exceeds 10%% — since-mode envelope inflated unexpectedly", ratioB)
	}

	// **Findings for the POC report** (the workable-shape verification
	// for `diff-since-local-head` is intentionally NOT run here):
	//
	//   The op as proposed in PROPOSAL-REVISION-DIFF-SINCE-LOCAL-HEAD.md §2
	//   does not compose cleanly into the cross-peer follow scenario:
	//
	//     1. Dispatched cross-peer on alice → "local head" resolves to
	//        alice's own head (target side), giving a self-diff (0 changed).
	//        Confirmed live in the failed first-iteration of this POC.
	//     2. Dispatched locally on bob with absolute prefix `/aliceID/...`
	//        → `CheckPathCapability("diff", "/aliceID/...")` fails with
	//        403 capability_denied. Bob doesn't own that prefix; his
	//        owner-cap doesn't cover it. Confirmed live as well.
	//     3. Dispatched locally on bob with bob-relative prefix → prefix
	//        gets resolved to `/bobID/...` and the head lookup misses
	//        (bob's revision DAG for his own prefix is unrelated).
	//
	//   The bandwidth claim (this test's primary purpose) is unaffected —
	//   the chain shape can use the existing cross-peer `revision:diff`
	//   with explicit base/target. But the convenience op does not
	//   replace it in the cross-peer chain.
}

// drive4StepRecipe executes the four chain steps via direct dispatch:
//
//  1. cross-peer revision:diff(prefix, base=bob_head, target=alice_head)
//     (See note in TestPOC_IncrementalSyncRecipes on the diff-since-local-head
//     "cross-peer" framing issue surfaced by the POC.)
//  2. inline collect_keys{fields:[added,changed], into:paths}
//  3. cross-peer tree:extract(prefix, paths=$paths)
//  4. local tree:merge(source_envelope=$step3_result)
func drive4StepRecipe(t *testing.T, ctx context.Context, bob *entitysdk.AppPeer, aliceID, prefix string, base, target hash.Hash) pocMetrics {
	t.Helper()
	start := time.Now()

	// Step 1: cross-peer revision:diff(base=bob's_view, target=alice's_new).
	diffParams := types.RevisionDiffParamsData{Prefix: prefix, Base: base, Target: target}
	diffParamEnt, _ := diffParams.ToEntity()
	diffResultEnt, err := executeOnRemote(ctx, bob, aliceID, "system/revision", "diff", diffParamEnt)
	if err != nil {
		t.Fatalf("step 1 revision:diff: %v", err)
	}
	diffBytes := len(diffResultEnt.Data)
	var diffData types.DiffData
	if err := decodeEntity(diffResultEnt, &diffData); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	t.Logf("4-step step 1: diff bytes=%d  added=%d changed=%d removed=%d unchanged=%d",
		diffBytes, len(diffData.Added), len(diffData.Changed), len(diffData.Removed), diffData.Unchanged)
	if len(diffData.Changed) == 0 && len(diffData.Added) == 0 {
		t.Fatal("step 1: diff returned empty added+changed — chain would idle")
	}

	// Step 2: collect_keys{fields: [added, changed]}. Inline projection.
	paths := collectKeysInline(diffData)
	t.Logf("4-step step 2: collect_keys → %d paths", len(paths))

	// Step 3: tree:extract(prefix, paths=$paths).
	extractParams := types.ExtractRequestData{Prefix: prefix, Paths: paths}
	extractParamEnt, _ := extractParams.ToEntity()
	extractResultEnt, err := executeOnRemote(ctx, bob, aliceID, "system/tree", "extract", extractParamEnt)
	if err != nil {
		t.Fatalf("step 3 tree:extract: %v", err)
	}
	extractBytes := len(extractResultEnt.Data)
	entities := countEnvelopeIncluded(t, extractResultEnt)
	t.Logf("4-step step 3: extract envelope bytes=%d  included=%d", extractBytes, entities)

	// Step 4: local tree:merge. Encode the extract result entity as the
	// source_envelope field per merge's "entity wrapping envelope" path.
	envEntRaw, err := cbor.Marshal(extractResultEnt)
	if err != nil {
		t.Fatalf("encode extract entity: %v", err)
	}
	mergeReq := types.MergeRequestData{
		Strategy:       "source-wins",
		SourcePrefix:   prefix,
		TargetPrefix:   prefix,
		SourceEnvelope: cbor.RawMessage(envEntRaw),
	}
	mergeParamEnt, _ := mergeReq.ToEntity()
	resp, err := bob.Executor().ExecuteWithParams("system/tree", "merge", mergeParamEnt)
	if err != nil {
		t.Fatalf("step 4 tree:merge: %v", err)
	}
	if resp == nil || resp.Status >= 400 {
		t.Fatalf("step 4: merge status=%d", statusOf(resp))
	}

	return pocMetrics{
		recipe:              "4-step",
		entitiesTransferred: entities,
		bytesTransferred:    diffBytes + extractBytes,
		rounds:              2,
		latency:             time.Since(start),
	}
}

// drive2StepRecipe executes the two chain steps via direct dispatch:
//
//  1. cross-peer tree:extract(prefix, since=$BOB_LOCAL_TRIE_ROOT)
//  2. local tree:merge(source_envelope=$step1_result)
//
// `since` is bob's current snapshot trie root for the followed prefix —
// alice walks her current trie, skips subtrees whose hash matches bob's
// `since` set, and bundles only the diff closure.
func drive2StepRecipe(t *testing.T, ctx context.Context, bob *entitysdk.AppPeer, aliceID, prefix string, sinceRoot hash.Hash) pocMetrics {
	t.Helper()
	start := time.Now()

	extractParams := types.ExtractRequestData{Prefix: prefix, Since: sinceRoot}
	extractParamEnt, _ := extractParams.ToEntity()
	extractResultEnt, err := executeOnRemote(ctx, bob, aliceID, "system/tree", "extract", extractParamEnt)
	if err != nil {
		t.Fatalf("step 1 tree:extract(since): %v", err)
	}
	bytes := len(extractResultEnt.Data)
	entities := countEnvelopeIncluded(t, extractResultEnt)
	t.Logf("2-step step 1: since-extract envelope bytes=%d  included=%d", bytes, entities)

	envEntRaw, _ := cbor.Marshal(extractResultEnt)
	mergeReq := types.MergeRequestData{
		Strategy:       "source-wins",
		SourcePrefix:   prefix,
		TargetPrefix:   prefix,
		SourceEnvelope: cbor.RawMessage(envEntRaw),
	}
	mergeParamEnt, _ := mergeReq.ToEntity()
	resp, err := bob.Executor().ExecuteWithParams("system/tree", "merge", mergeParamEnt)
	if err != nil {
		t.Fatalf("step 2 tree:merge: %v", err)
	}
	if resp == nil || resp.Status >= 400 {
		t.Fatalf("step 2: merge status=%d", statusOf(resp))
	}

	return pocMetrics{
		recipe:              "2-step",
		entitiesTransferred: entities,
		bytesTransferred:    bytes,
		rounds:              1,
		latency:             time.Since(start),
	}
}

// loadVersionTrieRoot returns the trie root hash referenced by a
// committed revision version entity. After Pull, bob's content store
// has alice's version entries; reading commitX.Version's Root gives the
// trie root at X, which is the canonical value to use as `since` on the
// 2-step recipe. In production this value would come from a
// subscription's `previous_hash` or the optional `tree:extract-since-local-head`
// companion op; this helper stands in for either source.
func loadVersionTrieRoot(t *testing.T, ap *entitysdk.AppPeer, versionHash hash.Hash) hash.Hash {
	t.Helper()
	ent, ok := ap.Store().GetByHash(versionHash)
	if !ok {
		t.Fatalf("version %s not in store", versionHash)
	}
	rev, err := types.RevisionEntryDataFromEntity(ent)
	if err != nil {
		t.Fatalf("decode version entry: %v", err)
	}
	return rev.Root
}

// driveFullExtract executes the current production cmdRevisionFollow shape:
//
//  1. cross-peer tree:extract(prefix)   — full subtree closure
//  2. local tree:merge(source_envelope) — apply
func driveFullExtract(t *testing.T, ctx context.Context, bob *entitysdk.AppPeer, aliceID, prefix string) pocMetrics {
	t.Helper()
	start := time.Now()

	extractParams := types.ExtractRequestData{Prefix: prefix}
	extractParamEnt, _ := extractParams.ToEntity()
	extractResultEnt, err := executeOnRemote(ctx, bob, aliceID, "system/tree", "extract", extractParamEnt)
	if err != nil {
		t.Fatalf("full extract: %v", err)
	}
	bytes := len(extractResultEnt.Data)
	entities := countEnvelopeIncluded(t, extractResultEnt)

	envEntRaw, _ := cbor.Marshal(extractResultEnt)
	mergeReq := types.MergeRequestData{
		Strategy:       "source-wins",
		SourcePrefix:   prefix,
		TargetPrefix:   prefix,
		SourceEnvelope: cbor.RawMessage(envEntRaw),
	}
	mergeParamEnt, _ := mergeReq.ToEntity()
	resp, err := bob.Executor().ExecuteWithParams("system/tree", "merge", mergeParamEnt)
	if err != nil {
		t.Fatalf("full merge: %v", err)
	}
	if resp == nil || resp.Status >= 400 {
		t.Fatalf("full merge: status=%d", statusOf(resp))
	}

	return pocMetrics{
		recipe:              "full-extract",
		entitiesTransferred: entities,
		bytesTransferred:    bytes,
		rounds:              1,
		latency:             time.Since(start),
	}
}

// collectKeysInline mirrors the collect_keys{fields:[added,changed]}
// transform op against a real system/tree/diff value. Same semantics as
// transform_ops.apply in the continuation; running it inline here keeps
// the POC focused on the bandwidth claim (pure transform, no I/O).
func collectKeysInline(diff types.DiffData) []string {
	out := make([]string, 0, len(diff.Added)+len(diff.Changed))
	for p := range diff.Added {
		out = append(out, p)
	}
	for p := range diff.Changed {
		out = append(out, p)
	}
	return out
}

func setupPeerPair(t *testing.T, ctx context.Context) (*entitysdk.AppPeer, *entitysdk.AppPeer) {
	t.Helper()
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
	return alice, bob
}

func statusOf(r *entitysdk.Response) int {
	if r == nil {
		return 0
	}
	return int(r.Status)
}
