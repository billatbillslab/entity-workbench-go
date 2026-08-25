package entitysdk_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

// TestPOC_DeletePropagation validates the HANDOFF §4 disposition:
// bare-removed paths preserve on the follower under the canonical
// chain shape (without marker discipline on the sender). The
// architecture team's disposition expects this — REVISION v3.1
// deletion markers are an opt-in sender mechanism for callers who
// need deletes to propagate.
//
// **POC finding** (worth raising): the manual Commit() path used by
// the workbench's revision client today does NOT emit deletion
// markers — emitDeletionMarkers runs only in
// `AutoVersioner.fire` (ext/revision/auto_version.go:260). The
// HANDOFF's "canonical default — deletion markers via REVISION v3.1"
// disposition implicitly assumes the AutoVersioner-driven path. The
// canonical guide rewrite should call this out so callers using
// manual commits understand the propagation semantic.
//
// What this test confirms (under the manual-Commit, no-markers path):
//
//  1. alice removes a leaf and updates another, then commits.
//  2. diff(X, Y) reports the removed leaf in `Removed` (NOT
//     `Changed → marker`).
//  3. The 4-step recipe's `collect_keys{fields:[added,changed]}`
//     does NOT include the bare-removed path → tree:extract(paths)
//     doesn't fetch it → bob's binding for it is preserved at the
//     original entity hash.
//  4. Same for the 2-step recipe: tree:extract(since) bundles only
//     the diff closure; bare-deleted paths aren't represented as
//     bindings to a marker, so the merge leaves bob's prior binding
//     in place.
//
// The verdict matches HANDOFF §4: "Bare-removed paths preserve on
// follower under this disposition. This is by design — bare deletion
// is not a deliberate-deletion signal." If a caller wants deletes to
// propagate, they bind the canonical deletion marker themselves; that
// path is a `changed` entry in the diff and rides through the same
// envelope mechanism.
func TestPOC_DeletePropagation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	alice, bob := setupPeerPair(t, ctx)
	const prefix = "deleteprop/"
	aliceID := alice.PeerID()

	// 5-leaf workspace → initial sync.
	const leaves = 5
	for i := 0; i < leaves; i++ {
		path := fmt.Sprintf("deleteprop/leaf-%02d", i)
		if _, err := alice.Put(path, "test/note", fmt.Sprintf("v1 %s", path)); err != nil {
			t.Fatalf("alice put: %v", err)
		}
	}
	commitX, err := alice.Revision().Commit(ctx, prefix, "initial")
	if err != nil {
		t.Fatalf("alice commit X: %v", err)
	}
	if _, err := bob.Revision().Pull(ctx, prefix, aliceID); err != nil {
		t.Fatalf("bob initial pull: %v", err)
	}

	// Alice updates one leaf and removes another, then commits.
	const updatedLeaf = "deleteprop/leaf-01"
	const removedLeaf = "deleteprop/leaf-03"
	if _, err := alice.Put(updatedLeaf, "test/note", "UPDATED v2"); err != nil {
		t.Fatalf("alice update: %v", err)
	}
	if err := alice.Remove(removedLeaf); err != nil {
		t.Fatalf("alice remove: %v", err)
	}
	commitY, err := alice.Revision().Commit(ctx, prefix, "update + delete")
	if err != nil {
		t.Fatalf("alice commit Y: %v", err)
	}
	if commitX.Version == commitY.Version {
		t.Fatal("commitY didn't advance head")
	}

	// --- Capture bob's binding for the removed path BEFORE the chain runs.
	// After Pull, bob's namespaced location index stores alice's bindings
	// under bob's namespace at the prefix-relative path (the prefix itself
	// is what bob synced, regardless of which peer authored it). The
	// effective lookup path is bob-namespaced; the source peer is alice.
	removedAbs := "/" + bob.PeerID() + "/" + removedLeaf
	bobPriorHash, ok := bobLocationLookup(t, bob, removedAbs)
	if !ok {
		// Diagnostic: list bob's bindings under the prefix to confirm
		// where the data actually landed.
		entries := bob.RawLocationIndex().List(prefix)
		t.Logf("bob's bindings under %q (%d total):", prefix, len(entries))
		for i, e := range entries {
			if i > 10 {
				t.Logf("  ... and %d more", len(entries)-10)
				break
			}
			t.Logf("  %s -> %s", e.Path, e.Hash)
		}
		t.Fatalf("bob has no prior binding for %q after initial pull", removedAbs)
	}
	t.Logf("bob's prior binding at %q: %s", removedAbs, bobPriorHash)

	// --- Step 1: revision:diff(X, Y). With manual Commit (no markers),
	// the removed leaf surfaces in `Removed`, NOT `Changed`.
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
	var diff types.DiffData
	if err := decodeEntity(diffResultEnt, &diff); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	t.Logf("diff X→Y: added=%d changed=%d removed=%d unchanged=%d",
		len(diff.Added), len(diff.Changed), len(diff.Removed), diff.Unchanged)
	relRemoved := "leaf-03"
	if _, ok := diff.Removed[relRemoved]; !ok {
		t.Fatalf("expected %q in Removed under manual Commit (no markers)", relRemoved)
	}
	if _, ok := diff.Changed[relRemoved]; ok {
		t.Fatalf("did not expect %q in Changed — manual Commit doesn't emit markers", relRemoved)
	}

	// --- Step 2: 4-step recipe applies. collect_keys{added,changed}
	// excludes the removed path, so tree:extract(paths) doesn't fetch
	// anything for it. After merge, bob's binding should be unchanged.
	paths := collectKeysInline(diff)
	extractParams := types.ExtractRequestData{Prefix: prefix, Paths: paths}
	extractParamEnt, _ := extractParams.ToEntity()
	extract4Ent, err := executeOnRemote(ctx, bob, aliceID, "system/tree", "extract", extractParamEnt)
	if err != nil {
		t.Fatalf("4-step extract: %v", err)
	}
	if err := merge4Step(t, bob, prefix, extract4Ent); err != nil {
		t.Fatalf("4-step merge: %v", err)
	}
	bobAfter4Hash, ok := bobLocationLookup(t, bob, removedAbs)
	if !ok {
		t.Fatalf("after 4-step merge: bob's binding at %q is gone (expected preservation)", removedAbs)
	}
	if bobAfter4Hash != bobPriorHash {
		t.Errorf("after 4-step merge: bob's binding at %q = %s, want preserved %s",
			removedAbs, bobAfter4Hash, bobPriorHash)
	} else {
		t.Logf("4-step recipe: bare-removed path PRESERVED on follower (expected per HANDOFF §4 disposition)")
	}

	// --- Step 3: 2-step recipe — same expectation. tree:extract(since)
	// bundles the diff closure; bare-deleted paths aren't bindings to
	// a marker, so merge leaves bob's prior binding alone.
	sinceRoot := loadVersionTrieRoot(t, bob, commitX.Version)
	extract2Params := types.ExtractRequestData{Prefix: prefix, Since: sinceRoot}
	extract2ParamEnt, _ := extract2Params.ToEntity()
	extract2Ent, err := executeOnRemote(ctx, bob, aliceID, "system/tree", "extract", extract2ParamEnt)
	if err != nil {
		t.Fatalf("2-step extract: %v", err)
	}
	if err := merge4Step(t, bob, prefix, extract2Ent); err != nil {
		t.Fatalf("2-step merge: %v", err)
	}
	bobAfter2Hash, ok := bobLocationLookup(t, bob, removedAbs)
	if !ok {
		t.Fatalf("after 2-step merge: bob's binding at %q is gone (expected preservation)", removedAbs)
	}
	if bobAfter2Hash != bobPriorHash {
		t.Errorf("after 2-step merge: bob's binding at %q = %s, want preserved %s",
			removedAbs, bobAfter2Hash, bobPriorHash)
	} else {
		t.Logf("2-step recipe: bare-removed path PRESERVED on follower (expected per HANDOFF §4 disposition)")
	}
}

// merge4Step is a shared merge driver that takes an extract result
// entity and applies it via source-wins.
func merge4Step(t *testing.T, bob *entitysdk.AppPeer, prefix string, extractEnt entity.Entity) error {
	t.Helper()
	envEntRaw, _ := cbor.Marshal(extractEnt)
	mergeReq := types.MergeRequestData{
		Strategy:       "source-wins",
		SourcePrefix:   prefix,
		TargetPrefix:   prefix,
		SourceEnvelope: cbor.RawMessage(envEntRaw),
	}
	mergeParamEnt, _ := mergeReq.ToEntity()
	resp, err := bob.Executor().ExecuteWithParams("system/tree", "merge", mergeParamEnt)
	if err != nil {
		return err
	}
	if resp == nil || resp.Status >= 400 {
		return fmt.Errorf("merge status=%d", statusOf(resp))
	}
	return nil
}

// envelopeIncludesHashEnt decodes envEnt as system/envelope and reports
// whether any entry in its Included map has the given content hash.
func envelopeIncludesHashEnt(t *testing.T, envEnt entity.Entity, h hash.Hash) bool {
	t.Helper()
	var env entity.Envelope
	if err := cbor.Unmarshal(envEnt.Data, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	_, ok := env.Included[h]
	return ok
}

// bobLocationLookup reads bob's location index hash for an absolute
// path directly. Bypasses the capability check (tree:get from bob's
// executor would 403 on alice's prefix); the POC needs to inspect bob's
// actual bound state, which is internal observation.
func bobLocationLookup(t *testing.T, bob *entitysdk.AppPeer, absPath string) (hash.Hash, bool) {
	t.Helper()
	return bob.RawLocationIndex().Get(absPath)
}
