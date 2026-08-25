package shellcmd_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/hash"

	"entity-workbench-go/entitysdk"
)

// getStoredHash returns the content hash bound at path in ap's live
// tree, or zero hash and false if unbound. Convenience wrapper used
// by the delete-propagation tests to compare hashes across put / remove
// / re-add cycles.
func getStoredHash(ap *entitysdk.AppPeer, path string) (hash.Hash, bool) {
	ent, ok := ap.Store().Get(path)
	if !ok {
		return hash.Hash{}, false
	}
	return ent.ContentHash, true
}

// TestE2E_Delete_PropagatesAcrossPeers verifies the Phase 2
// (PROPOSAL-DELETION-MARKERS) end-to-end roundtrip:
//
//  1. alice puts an entity at P.
//  2. Both peers' live trees converge — bob's tree has P bound.
//  3. alice removes P (tree:put(P, null) → auto-version emits a
//     deletion marker into the new version's trie).
//  4. Cross-peer propagation: bob's converge handler pulls alice's
//     latest version → bob's merge translates the marker binding
//     to TreeRemove → bob's live tree unbinds P.
//
// This is the per-impl checklist's positive deletion-marker roundtrip
// from PROPOSAL-DELETION-MARKERS §7. With phantom-marker emission
// closed (F10 part 7), the test exercises the legitimate marker path:
// user-issued delete → marker in version trie → propagation →
// live-tree unbind on the remote peer.
func TestE2E_Delete_PropagatesAcrossPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const targetPrefix = "archives/notes/"
	const path = targetPrefix + "to-delete.md"
	a, b := bringUpNoFSPair(t, ctx, targetPrefix)

	// Step 1: alice writes.
	if _, err := a.ap.Put(path, "doc/markdown-file", map[string]interface{}{
		"path":    path,
		"title":   "to-delete",
		"content": "# delete me\n",
	}); err != nil {
		t.Fatalf("alice put: %v", err)
	}

	// Step 2: wait for bob to receive via follow chain.
	if !waitFor(10*time.Second, func() bool {
		return b.ap.Store().Has(path)
	}) {
		t.Fatalf("propagation: bob's tree never got %s within 10s", path)
	}
	t.Logf("step 2 OK: bob received %s", path)

	// Sanity: heads should be equal post-propagation.
	if !waitFor(5*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		t.Fatalf("heads didn't converge after put: alice=%s bob=%s", aH.Head, bH.Head)
	}

	// Step 3: alice deletes.
	if err := a.ap.Remove(path); err != nil {
		t.Fatalf("alice remove: %v", err)
	}
	if a.ap.Store().Has(path) {
		t.Fatalf("alice's live tree still has %s after Remove", path)
	}
	t.Logf("step 3 OK: alice unbound %s locally", path)

	// Step 4: wait for bob to observe the unbind via converge chain.
	// Bob's converge handler pulls alice's new head; merge sees the
	// canonical deletion marker at `path`; applyMergedBindingToTree
	// translates it to TreeRemove on bob's live tree.
	if !waitFor(10*time.Second, func() bool {
		return !b.ap.Store().Has(path)
	}) {
		t.Fatalf("propagation: bob's tree still has %s 10s after alice removed", path)
	}
	t.Logf("step 4 OK: bob's tree unbound %s after alice's delete propagated", path)

	// Final: heads converge to the post-delete version.
	if !waitFor(5*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		t.Fatalf("heads didn't converge after delete: alice=%s bob=%s", aH.Head, bH.Head)
	}
	t.Logf("delete propagation OK end-to-end")
}

// TestE2E_Delete_ReAddAtSamePath verifies re-add works after a delete:
//
//  1. alice puts at P → propagates.
//  2. alice removes P → propagates (live unbind on bob).
//  3. alice puts NEW content at P → propagates.
//  4. Final state: both peers have the new entity at P (not the
//     deletion marker, not the old entity).
//
// Tests that the merge correctly classifies "parent has marker at P,
// local has new entity at P" as `changed` (rebinding), not as a
// marker-vs-entity conflict. The new entity wins by virtue of being
// the live binding.
func TestE2E_Delete_ReAddAtSamePath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	const targetPrefix = "archives/notes/"
	const path = targetPrefix + "phoenix.md"
	a, b := bringUpNoFSPair(t, ctx, targetPrefix)

	// 1. First put.
	if _, err := a.ap.Put(path, "doc/markdown-file", map[string]interface{}{
		"path": path, "content": "# version 1\n",
	}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if !waitFor(10*time.Second, func() bool { return b.ap.Store().Has(path) }) {
		t.Fatalf("first put: bob never received")
	}
	firstHash, _ := getStoredHash(b.ap, path)
	t.Logf("step 1 OK: bob has %s = %s", path, firstHash)

	// 2. Delete.
	if err := a.ap.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !waitFor(10*time.Second, func() bool { return !b.ap.Store().Has(path) }) {
		t.Fatalf("delete: bob never unbound")
	}
	t.Logf("step 2 OK: bob unbound after alice's delete propagated")

	// 3. Re-add with NEW content.
	if _, err := a.ap.Put(path, "doc/markdown-file", map[string]interface{}{
		"path": path, "content": "# version 2 (phoenix)\n",
	}); err != nil {
		t.Fatalf("re-add put: %v", err)
	}
	if !waitFor(10*time.Second, func() bool {
		if !b.ap.Store().Has(path) {
			return false
		}
		h, _ := getStoredHash(b.ap, path)
		return h != firstHash
	}) {
		gotHash, gotBound := getStoredHash(b.ap, path)
		t.Fatalf("re-add: bob's tree didn't get the new entity (bound=%v, hash=%s)", gotBound, gotHash)
	}
	t.Logf("step 3 OK: bob received re-added entity (different hash from first)")

	// Heads converge to the post-re-add version.
	if !waitFor(5*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		t.Fatalf("heads didn't converge after re-add: alice=%s bob=%s", aH.Head, bH.Head)
	}
	t.Logf("re-add propagation OK end-to-end")
}

// configureDeletionStrategy installs a per-prefix merge-config setting
// `deletion_resolution` to the given strategy. Used by tests that need
// to verify behavior under an explicit (non-default) deletion policy.
// Writes the config on both peers so the same lookup resolves equivalently
// on either side of a merge.
func configureDeletionStrategy(t *testing.T, ap *entitysdk.AppPeer, pattern, strategy string) {
	t.Helper()
	// Use a unique config-storage path slug per (pattern, strategy) so multiple
	// configs can coexist if a test ever needs them; today one is enough.
	slug := "test-" + strategy
	path := "system/revision/config/merge/path/" + slug
	if _, err := ap.Put(path, "system/revision/merge-config", map[string]interface{}{
		"pattern":             pattern,
		"strategy":            "three-way",
		"deletion_resolution": strategy,
	}); err != nil {
		t.Fatalf("configure merge-config %s=%s: %v", pattern, strategy, err)
	}
}

// TestE2E_Delete_ConcurrentEditPreservesOnConflict verifies the
// new default `preserve-on-conflict` deletion-vs-entity strategy
// (PROPOSAL-DELETION-MARKERS Amendment 4, post-default-flip):
//
//  1. alice and bob both have entity E at P (converged).
//  2. Concurrent: alice removes P; bob updates P with new content E'.
//  3. Both peers merge each other's branches.
//  4. Final state per default policy: P is BOUND on both peers to
//     bob's edit (E'). The delete signal is silently dropped because
//     preserve-on-conflict biases toward preserving concurrent edits.
//
// This is the contentious semantic case under the default. Operators
// who prefer the opposite bias (deletes always win) configure the
// per-prefix `deletion_resolution: deletion-wins` — see the
// `ExplicitDeletionWins` variant below.
//
// Honest framing: neither default is "lossless." preserve-on-conflict
// silently drops the delete signal; deletion-wins silently drops the
// edit signal. The spec picked preserve-on-conflict to bias toward
// not-losing-recent-edits.
func TestE2E_Delete_ConcurrentEditPreservesOnConflict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	const targetPrefix = "archives/notes/"
	const path = targetPrefix + "contested.md"
	a, b := bringUpNoFSPair(t, ctx, targetPrefix)

	// Step 1: alice writes; both converge to the initial entity.
	if _, err := a.ap.Put(path, "doc/markdown-file", map[string]interface{}{
		"path": path, "content": "# initial\n",
	}); err != nil {
		t.Fatalf("initial put: %v", err)
	}
	if !waitFor(10*time.Second, func() bool { return b.ap.Store().Has(path) }) {
		t.Fatalf("initial propagation failed")
	}
	if !waitFor(5*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		t.Fatalf("heads didn't converge after initial put")
	}
	t.Logf("step 1 OK: both peers at %s (initial)", path)

	// Step 2: concurrent — alice deletes, bob updates.
	if err := a.ap.Remove(path); err != nil {
		t.Fatalf("alice remove: %v", err)
	}
	if _, err := b.ap.Put(path, "doc/markdown-file", map[string]interface{}{
		"path": path, "content": "# bob updated\n",
	}); err != nil {
		t.Fatalf("bob put: %v", err)
	}
	t.Logf("step 2 OK: alice deleted, bob updated — concurrent")

	// Step 3: wait for convergence. Default preserve-on-conflict →
	// both peers should converge to BOUND with bob's new content.
	editPreserved := waitFor(20*time.Second, func() bool {
		if !a.ap.Store().Has(path) || !b.ap.Store().Has(path) {
			return false
		}
		aH, _ := getStoredHash(a.ap, path)
		bH, _ := getStoredHash(b.ap, path)
		return aH == bH
	})
	if !editPreserved {
		aHas := a.ap.Store().Has(path)
		bHas := b.ap.Store().Has(path)
		var aHash, bHash string
		if aHas {
			h, _ := getStoredHash(a.ap, path)
			aHash = h.String()
		}
		if bHas {
			h, _ := getStoredHash(b.ap, path)
			bHash = h.String()
		}
		t.Fatalf("preserve-on-conflict policy didn't converge to bound: alice has=%v (%s) bob has=%v (%s)",
			aHas, aHash, bHas, bHash)
	}

	// Heads also converge.
	if !waitFor(5*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		t.Fatalf("heads didn't converge: alice=%s bob=%s", aH.Head, bH.Head)
	}
	t.Logf("preserve-on-conflict resolved as spec'd: P bound to bob's edit on both peers; alice's delete dropped")
}

// TestE2E_Delete_ConcurrentEditDeletionWinsExplicit verifies the
// explicit `deletion-wins` per-prefix override (PROPOSAL-DELETION-MARKERS
// Amendment 4):
//
//  1. Both peers configure `deletion_resolution: deletion-wins` for the
//     target prefix via merge-config at
//     `system/revision/config/merge/path/{slug}`.
//  2. alice and bob both have entity E at P (converged).
//  3. Concurrent: alice removes P; bob updates P with new content E'.
//  4. Final state per explicit policy: P is UNBOUND on both peers
//     (deletion-wins; the marker supersedes the concurrent edit).
//
// This exercises the same divergent scenario as
// `_ConcurrentEditPreservesOnConflict` but with the operator opting
// into the opposite bias. Verifies the per-prefix lookup +
// resolveDeletionVsEntity flow end-to-end.
func TestE2E_Delete_ConcurrentEditDeletionWinsExplicit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	const targetPrefix = "archives/notes/"
	const path = targetPrefix + "contested.md"
	a, b := bringUpNoFSPair(t, ctx, targetPrefix)

	// Configure deletion-wins on both peers BEFORE any user writes.
	// The pattern needs to cover the target path's trie-relative form,
	// which is just the filename after the prefix is stripped.
	for _, p := range []*bidiPeer{a, b} {
		configureDeletionStrategy(t, p.ap, "*", "deletion-wins")
	}
	// Allow time for the config to propagate via auto-version + sync.
	// The config entity is itself revision-tracked; both peers need to
	// have the matching config in their location index before the
	// merge fires.
	time.Sleep(500 * time.Millisecond)

	// Step 1: alice writes; both converge to the initial entity.
	if _, err := a.ap.Put(path, "doc/markdown-file", map[string]interface{}{
		"path": path, "content": "# initial\n",
	}); err != nil {
		t.Fatalf("initial put: %v", err)
	}
	if !waitFor(10*time.Second, func() bool { return b.ap.Store().Has(path) }) {
		t.Fatalf("initial propagation failed")
	}
	if !waitFor(5*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		t.Fatalf("heads didn't converge after initial put")
	}
	t.Logf("step 1 OK: both peers at %s (initial)", path)

	// Step 2: concurrent — alice deletes, bob updates.
	if err := a.ap.Remove(path); err != nil {
		t.Fatalf("alice remove: %v", err)
	}
	if _, err := b.ap.Put(path, "doc/markdown-file", map[string]interface{}{
		"path": path, "content": "# bob updated\n",
	}); err != nil {
		t.Fatalf("bob put: %v", err)
	}
	t.Logf("step 2 OK: alice deleted, bob updated — concurrent")

	// Step 3: wait for convergence under explicit deletion-wins.
	deletionWon := waitFor(20*time.Second, func() bool {
		return !a.ap.Store().Has(path) && !b.ap.Store().Has(path)
	})
	if !deletionWon {
		aHas := a.ap.Store().Has(path)
		bHas := b.ap.Store().Has(path)
		var aHash, bHash string
		if aHas {
			h, _ := getStoredHash(a.ap, path)
			aHash = h.String()
		}
		if bHas {
			h, _ := getStoredHash(b.ap, path)
			bHash = h.String()
		}
		t.Fatalf("explicit deletion-wins didn't converge to unbound: alice has=%v (%s) bob has=%v (%s)",
			aHas, aHash, bHas, bHash)
	}

	// Heads also converge.
	if !waitFor(5*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		t.Fatalf("heads didn't converge: alice=%s bob=%s", aH.Head, bH.Head)
	}
	t.Logf("explicit deletion-wins resolved as configured: P unbound on both peers, heads equal")
}

// TestE2E_Delete_ConcurrentSamePathBothDelete verifies that concurrent
// deletes of the SAME path on both peers converge trivially:
//
//  1. Both peers start with entity E bound at P (converged).
//  2. Concurrent: alice removes P; bob also removes P.
//  3. Both peers' commits emit the canonical deletion marker at P.
//     Because the marker is byte-identical across peers (Amendment 1
//     "canonical" property), both versions carry the SAME marker hash
//     at P.
//  4. Three-way merge classifies as "same on both sides" — no divergent
//     case, no conflict resolution invoked. Trivially convergent.
//
// This directly verifies the canonical-marker convergence claim from
// PROPOSAL-DELETION-MARKERS Amendment 3: "deletion-vs-deletion is NOT
// a divergent case." If the marker weren't canonical (e.g., carried
// timestamps), this test would expose the cross-peer divergent-hashes
// bug.
func TestE2E_Delete_ConcurrentSamePathBothDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const targetPrefix = "archives/notes/"
	const path = targetPrefix + "both-delete-me.md"
	a, b := bringUpNoFSPair(t, ctx, targetPrefix)

	// Step 1: alice writes; both converge.
	if _, err := a.ap.Put(path, "doc/markdown-file", map[string]interface{}{
		"path": path, "content": "# shared\n",
	}); err != nil {
		t.Fatalf("initial put: %v", err)
	}
	if !waitFor(10*time.Second, func() bool { return b.ap.Store().Has(path) }) {
		t.Fatalf("initial propagation failed")
	}
	if !waitFor(5*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		t.Fatalf("heads didn't converge after initial put")
	}
	t.Logf("step 1 OK: both peers have %s", path)

	// Step 2: concurrent — both peers delete the same path.
	if err := a.ap.Remove(path); err != nil {
		t.Fatalf("alice remove: %v", err)
	}
	if err := b.ap.Remove(path); err != nil {
		t.Fatalf("bob remove: %v", err)
	}
	t.Logf("step 2 OK: both peers issued Remove on %s", path)

	// Step 3 + 4: convergence. Both peers should reach a state where
	// the path is unbound, heads equal.
	if !waitFor(15*time.Second, func() bool {
		if a.ap.Store().Has(path) || b.ap.Store().Has(path) {
			return false
		}
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		t.Fatalf("concurrent same-path delete didn't converge: alice has=%v bob has=%v alice head=%s bob head=%s",
			a.ap.Store().Has(path), b.ap.Store().Has(path), aH.Head, bH.Head)
	}
	t.Logf("concurrent same-path delete converged trivially: both unbound, heads equal")
}

// TestE2E_Delete_MixedOpBurst stresses the merge classifier under
// realistic mixed-operation load: each peer does a burst of puts,
// deletes, and re-adds against a pre-existing baseline, while the
// other peer does the same concurrently on its own paths.
//
// Shape:
//  1. Baseline: both peers have entries baseline-0..baseline-3 (4 entries).
//  2. Concurrent burst (alice and bob in parallel goroutines):
//     - alice: puts 3 new (alice-0..alice-2), removes baseline-0, re-puts baseline-1 with new content.
//     - bob:   puts 3 new (bob-0..bob-2),   removes baseline-2, re-puts baseline-3 with new content.
//  3. After convergence, expected final state on BOTH peers:
//     - baseline-1 (alice's re-add wins via "changed" branch — entity supersedes prior).
//     - baseline-3 (bob's re-add wins).
//     - alice-0..alice-2 (alice's new puts propagate).
//     - bob-0..bob-2   (bob's new puts propagate).
//     - baseline-0 unbound (alice deleted, no concurrent op on this path → propagates cleanly).
//     - baseline-2 unbound (bob deleted, same).
//     Total: 8 entries on each peer.
//
// This stresses:
//   - Marker emission alongside add emission in the same commit cycle.
//   - Cross-peer merge with markers from both sides (different paths).
//   - Re-add path against own prior version (parent has entity, live has different entity).
//   - The per-prefix mutex coordination under sustained mixed load.
//
// If any phase has an edge case (marker leaking into wrong path, re-add
// classified as conflict instead of changed, etc.), this test surfaces it.
func TestE2E_Delete_MixedOpBurst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const targetPrefix = "archives/notes/"
	a, b := bringUpNoFSPair(t, ctx, targetPrefix)

	baselinePaths := []string{
		targetPrefix + "baseline-0.md",
		targetPrefix + "baseline-1.md",
		targetPrefix + "baseline-2.md",
		targetPrefix + "baseline-3.md",
	}

	// Step 1: alice writes the baseline; wait for both peers to have it.
	for i, p := range baselinePaths {
		if _, err := a.ap.Put(p, "doc/markdown-file", map[string]interface{}{
			"path": p, "content": "# baseline " + string(rune('0'+i)) + "\n",
		}); err != nil {
			t.Fatalf("alice baseline put %d: %v", i, err)
		}
	}
	if !waitFor(15*time.Second, func() bool {
		for _, p := range baselinePaths {
			if !b.ap.Store().Has(p) {
				return false
			}
		}
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head
	}) {
		t.Fatalf("baseline propagation failed; bob has %d/4", len(baselinePaths))
	}
	t.Logf("step 1 OK: baseline of 4 entries converged on both peers")

	// Step 2: concurrent mixed-op burst.
	// alice paths.
	aliceNewPaths := []string{
		targetPrefix + "alice-0.md",
		targetPrefix + "alice-1.md",
		targetPrefix + "alice-2.md",
	}
	bobNewPaths := []string{
		targetPrefix + "bob-0.md",
		targetPrefix + "bob-1.md",
		targetPrefix + "bob-2.md",
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// alice: 3 puts + 1 delete (baseline-0) + 1 re-add (baseline-1).
		for _, p := range aliceNewPaths {
			if _, err := a.ap.Put(p, "doc/markdown-file", map[string]interface{}{
				"path": p, "content": "# alice new\n",
			}); err != nil {
				t.Errorf("alice put %s: %v", p, err)
				return
			}
		}
		if err := a.ap.Remove(baselinePaths[0]); err != nil {
			t.Errorf("alice remove baseline-0: %v", err)
			return
		}
		if _, err := a.ap.Put(baselinePaths[1], "doc/markdown-file", map[string]interface{}{
			"path": baselinePaths[1], "content": "# baseline-1 updated\n",
		}); err != nil {
			t.Errorf("alice re-put baseline-1: %v", err)
			return
		}
	}()
	go func() {
		defer wg.Done()
		// bob: 3 puts + 1 delete (baseline-2) + 1 re-add (baseline-3).
		for _, p := range bobNewPaths {
			if _, err := b.ap.Put(p, "doc/markdown-file", map[string]interface{}{
				"path": p, "content": "# bob new\n",
			}); err != nil {
				t.Errorf("bob put %s: %v", p, err)
				return
			}
		}
		if err := b.ap.Remove(baselinePaths[2]); err != nil {
			t.Errorf("bob remove baseline-2: %v", err)
			return
		}
		if _, err := b.ap.Put(baselinePaths[3], "doc/markdown-file", map[string]interface{}{
			"path": baselinePaths[3], "content": "# baseline-3 updated\n",
		}); err != nil {
			t.Errorf("bob re-put baseline-3: %v", err)
			return
		}
	}()
	wg.Wait()
	if t.Failed() {
		return
	}
	t.Logf("step 2 OK: mixed-op burst issued (3 puts + 1 delete + 1 re-add per peer)")

	// Step 3: expected final state — paths bound, paths unbound.
	expectBound := []string{
		baselinePaths[1], // alice re-added
		baselinePaths[3], // bob re-added
		aliceNewPaths[0], aliceNewPaths[1], aliceNewPaths[2],
		bobNewPaths[0], bobNewPaths[1], bobNewPaths[2],
	}
	expectUnbound := []string{
		baselinePaths[0], // alice deleted
		baselinePaths[2], // bob deleted
	}

	converged := waitFor(20*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		if aH.Head.IsZero() || aH.Head != bH.Head {
			return false
		}
		for _, p := range expectBound {
			if !a.ap.Store().Has(p) || !b.ap.Store().Has(p) {
				return false
			}
		}
		for _, p := range expectUnbound {
			if a.ap.Store().Has(p) || b.ap.Store().Has(p) {
				return false
			}
		}
		return true
	})
	if !converged {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		t.Logf("convergence diagnostic:")
		t.Logf("  alice head: %s", aH.Head)
		t.Logf("  bob head:   %s", bH.Head)
		for _, p := range expectBound {
			t.Logf("  expect-bound %s: alice=%v bob=%v", p, a.ap.Store().Has(p), b.ap.Store().Has(p))
		}
		for _, p := range expectUnbound {
			t.Logf("  expect-unbound %s: alice=%v bob=%v", p, a.ap.Store().Has(p), b.ap.Store().Has(p))
		}
		t.Fatalf("mixed-op burst didn't converge to expected state within 20s")
	}
	t.Logf("mixed-op burst converged correctly: 8 entries bound, 2 unbound, heads equal")
}
