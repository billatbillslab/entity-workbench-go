package shellcmd_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/tree"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// TestE2E_Bidirectional_BurstWrites_NoFS isolates whether the burst
// convergence bug is in the cross-peer merge cascade or somewhere in
// the filesystem/ingest stack. Identical convergence-checking shape
// to TestE2E_Bidirectional_BurstWrites but writes go via AppPeer.Put
// directly to archives/notes/ — no fsnotify, no localfiles, no
// workbench ingest handler. Just: two peers, follow chain in both
// directions, each peer writes 5 entities concurrently via Put,
// expect 10/10 entries to converge on both sides.
//
// If this test PASSES: the bug is somewhere in fsnotify / localfiles
// / ingest-from-notification, not in cross-peer merge.
//
// If this test FAILS the same way: the bug is in cross-peer merge
// itself, and the fastForward/checkout fixes were necessary but not
// sufficient.
func TestE2E_Bidirectional_BurstWrites_NoFS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const targetPrefix = "archives/notes/"
	const burst = 5

	a, b := bringUpNoFSPair(t, ctx, targetPrefix)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < burst; i++ {
			path := fmt.Sprintf("%sa-%d.md", targetPrefix, i)
			if _, err := a.ap.Put(path, "doc/markdown-file", map[string]interface{}{
				"path":    path,
				"title":   fmt.Sprintf("a %d", i),
				"content": fmt.Sprintf("# a %d\n", i),
			}); err != nil {
				t.Errorf("alice put a-%d: %v", i, err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < burst; i++ {
			path := fmt.Sprintf("%sb-%d.md", targetPrefix, i)
			if _, err := b.ap.Put(path, "doc/markdown-file", map[string]interface{}{
				"path":    path,
				"title":   fmt.Sprintf("b %d", i),
				"content": fmt.Sprintf("# b %d\n", i),
			}); err != nil {
				t.Errorf("bob put b-%d: %v", i, err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	wg.Wait()

	expectedPaths := make([]string, 0, burst*2)
	for i := 0; i < burst; i++ {
		expectedPaths = append(expectedPaths,
			fmt.Sprintf("%sa-%d.md", targetPrefix, i),
			fmt.Sprintf("%sb-%d.md", targetPrefix, i))
	}

	allConverged := func() bool {
		for _, p := range expectedPaths {
			if !a.ap.Store().Has(p) || !b.ap.Store().Has(p) {
				return false
			}
		}
		return true
	}
	headConverged := waitFor(20*time.Second, func() bool {
		aH, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bH, _ := b.ap.Revision().Status(ctx, targetPrefix)
		return !aH.Head.IsZero() && aH.Head == bH.Head && allConverged()
	})
	if !headConverged {
		aHead, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bHead, _ := b.ap.Revision().Status(ctx, targetPrefix)
		missing := make([]string, 0)
		for _, p := range expectedPaths {
			if !a.ap.Store().Has(p) {
				missing = append(missing, "alice missing "+p)
			}
			if !b.ap.Store().Has(p) {
				missing = append(missing, "bob missing "+p)
			}
		}
		t.Logf("alice has %d / %d expected entries; bob has %d / %d",
			len(a.ap.Store().List(targetPrefix)), len(expectedPaths),
			len(b.ap.Store().List(targetPrefix)), len(expectedPaths))
		if len(missing) > 0 {
			t.Logf("missing %d:", len(missing))
			for _, m := range missing {
				t.Logf("  %s", m)
			}
		}

		// **The two failures below are different bugs and the old
		// message could not tell them apart.**
		//
		// The wait condition is a CONJUNCTION (heads equal AND every
		// path bound), so a timeout says only "at least one of the two
		// was false." It used to report `heads_equal=%v` on one line,
		// which read as a paradox when it printed `true` — and the 2/4
		// failures on 2026-08-19 all printed `true`. It is not a
		// paradox, and it is not a dropped notification either:
		//
		//   - heads DIFFER  → the merge or the notification that drives
		//     it never landed. That is the delivery-path story, and the
		//     known subscription-saturation item is a candidate.
		//   - heads EQUAL, a path unbound → the head advanced and the
		//     CHECKOUT did not project the whole trie into the location
		//     index. Store.Has is `locationIndex.Has` (entitysdk/store.go)
		//     — a pure binding check — so agreeing on a revision head
		//     while disagreeing on the bound set means the revision
		//     merged and its projection did not follow. Nothing about
		//     delivery explains that; both peers already have the head.
		//
		// The DAG dump runs for the second case because that is the one
		// where the head is worth walking.
		if aHead.Head == bHead.Head {
			dumpRevisionDAG(t, "alice", a.ap, aHead.Head, targetPrefix)
			dumpRevisionDAG(t, "bob  ", b.ap, bHead.Head, targetPrefix)
			verdict := classifyConvergedHeadFailure(t, a.ap, b.ap, aHead.Head, missing)
			t.Fatalf("CONVERGENCE FAILED — HEADS AGREE (%s) BUT THE BOUND SET DOES NOT.\n%s",
				aHead.Head, verdict)
		}
		t.Fatalf("CONVERGENCE FAILED — HEADS DIFFER: alice=%s bob=%s.\n"+
			"One peer never merged the other's revision. This is the delivery/merge path; the "+
			"known subscription-saturation item is a candidate here and only here.",
			aHead.Head, bHead.Head)
	}

	if !allConverged() {
		missing := make([]string, 0)
		for _, p := range expectedPaths {
			if !a.ap.Store().Has(p) {
				missing = append(missing, "alice missing "+p)
			}
			if !b.ap.Store().Has(p) {
				missing = append(missing, "bob missing "+p)
			}
		}
		aHead, _ := a.ap.Revision().Status(ctx, targetPrefix)
		bHead, _ := b.ap.Revision().Status(ctx, targetPrefix)
		t.Logf("HEAD CONVERGED to %s but DATA INCOMPLETE", aHead.Head)
		t.Logf("alice has %d / %d expected entries; bob has %d / %d",
			len(a.ap.Store().List(targetPrefix)), len(expectedPaths),
			len(b.ap.Store().List(targetPrefix)), len(expectedPaths))
		t.Logf("missing %d:", len(missing))
		for _, m := range missing {
			t.Logf("  %s", m)
		}
		dumpRevisionDAG(t, "alice", a.ap, aHead.Head, targetPrefix)
		dumpRevisionDAG(t, "bob  ", b.ap, bHead.Head, targetPrefix)
		t.Fatalf("burst writes lost data during merge (no-FS path)")
	}
	t.Logf("burst OK: %d entries converged on both peers (no-FS path)", burst*2)
}

// bringUpNoFSPair stands up two peers configured for revision sync
// on targetPrefix, but with the localfiles extension DISABLED (no
// watcher, no ingest handler wired). Just the revision/converge
// plumbing.
func bringUpNoFSPair(t *testing.T, ctx context.Context, targetPrefix string) (*bidiPeer, *bidiPeer) {
	t.Helper()
	a := newNoFSPeer(t, "alice")
	b := newNoFSPeer(t, "bob")
	t.Cleanup(func() { _ = a.ap.Close() })
	t.Cleanup(func() { _ = b.ap.Close() })

	for _, p := range []*bidiPeer{a, b} {
		ready := make(chan struct{})
		errCh := make(chan error, 1)
		go func(ap *entitysdk.AppPeer) {
			errCh <- ap.ListenReady(ctx, ready)
		}(p.ap)
		select {
		case <-ready:
		case err := <-errCh:
			t.Fatalf("%s listen: %v", p.rootName, err)
		case <-time.After(2 * time.Second):
			t.Fatalf("%s listen timeout", p.rootName)
		}
	}
	if _, err := b.ap.Connect(ctx, a.ap.Addr().String()); err != nil {
		t.Fatalf("bob→alice connect: %v", err)
	}
	if _, err := a.ap.Connect(ctx, b.ap.Addr().String()); err != nil {
		t.Fatalf("alice→bob connect: %v", err)
	}

	autoTrue := true
	for _, p := range []*bidiPeer{a, b} {
		if _, err := p.ap.Revision().ConfigPut(ctx, "notes", types.RevisionConfigData{
			Prefix:      targetPrefix,
			AutoVersion: &autoTrue,
		}, nil); err != nil {
			t.Fatalf("%s auto-version config: %v", p.rootName, err)
		}
	}
	a.installFollow(t, b, targetPrefix)
	b.installFollow(t, a, targetPrefix)
	return a, b
}

func newNoFSPeer(t *testing.T, rootName string) *bidiPeer {
	t.Helper()
	logger := log.New(os.Stderr, "["+rootName+"] ", 0)
	cfg := entitysdk.PeerConfig{
		ListenAddr: "127.0.0.1:0",
		DebugLog:   logger,
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	}
	ap, err := entitysdk.CreatePeer(cfg)
	if err != nil {
		t.Fatalf("%s create: %v", rootName, err)
	}
	return &bidiPeer{
		ap:       ap,
		rootName: rootName,
		id:       ap.PeerID(),
	}
}

// classifyConvergedHeadFailure answers the one question that splits the
// heads-agree-but-data-differs failure into two different bugs, and it
// answers it by walking the trie rather than by arguing.
//
// **The fork.** Both peers hold head H and a path P is unbound on one of
// them. Either:
//
//	(A) H's trie COMMITS to P → the head was adopted without its
//	    bindings being projected. The version DAG is right and the live
//	    tree is behind it: an apply/projection gap.
//	(B) H's trie does NOT commit to P → the write was never captured, or
//	    was captured and then WIPED. The live tree is right on the peer
//	    that still has P and the DAG never learned about it: a
//	    capture/transcription gap.
//
// **(B) has a named prior and a live suspect.** `ext/revision/merge.go`'s
// fast-forward path carries a comment describing exactly this bug class
// — the "F10 diagnosis": an implementation that read the LIVE TREE to
// compute what to remove classified any in-flight write (present in the
// tree, not yet captured by auto-version) as removed and wiped it, so
// "the slower committer's writes vanished" under burst. That path was
// fixed to diff the COMMITTED trie instead. **`applyBindings`, which the
// three-way `performMerge` path calls, still removes every current
// binding under the prefix and re-sets only what the merged tries
// carry** (`ext/revision/merge.go::applyBindings`). A write in flight
// across that window is in neither trie.
//
// Two peers writing concurrently DIVERGE, so a bidirectional burst is
// exactly the shape that takes the three-way path rather than the
// fast-forward one.
//
// **This function is why that stays a hypothesis until it fires.** D19 /
// AP10: reading a code path establishes what the path does, not what the
// operation does — the two claims this repo routed on the strength of a
// correct source reading both missed a layer underneath. The verdict
// below is a measurement of the actual failing run, and it is what gets
// routed, not the argument above.
func classifyConvergedHeadFailure(
	t *testing.T, a, b *entitysdk.AppPeer, head hash.Hash, missing []string,
) string {
	t.Helper()
	if head.IsZero() {
		return "head is zero — nothing to walk."
	}
	// The version entity's Root is the trie; read it from whichever peer
	// has it (they agree on the head, so either will do).
	var root hash.Hash
	for _, ap := range []*entitysdk.AppPeer{a, b} {
		ent, ok := ap.RawContentStore().Get(head)
		if !ok {
			continue
		}
		ver, err := types.RevisionEntryDataFromEntity(ent)
		if err != nil {
			continue
		}
		root = ver.Root
		break
	}
	if root.IsZero() {
		return "VERDICT INCONCLUSIVE — the agreed head is not resolvable to a version/root on " +
			"either peer, which is its own finding: both advanced to a head neither can read."
	}

	committed := tree.CollectAllBindings(a.RawContentStore(), root, "")
	if len(committed) == 0 {
		committed = tree.CollectAllBindings(b.RawContentStore(), root, "")
	}

	var inTrie, notInTrie []string
	for _, m := range missing {
		// `missing` entries read "alice missing <path>" / "bob missing <path>".
		path := m
		if i := strings.LastIndex(m, " "); i >= 0 {
			path = m[i+1:]
		}
		found := false
		for k := range committed {
			if strings.HasSuffix(path, strings.TrimPrefix(k, "/")) || strings.HasSuffix(k, path) {
				found = true
				break
			}
		}
		if found {
			inTrie = append(inTrie, path)
		} else {
			notInTrie = append(notInTrie, path)
		}
	}

	t.Logf("agreed head %s → trie root %s, committing to %d bindings", head, root, len(committed))
	for k := range committed {
		t.Logf("  trie: %s", k)
	}

	switch {
	case len(inTrie) > 0 && len(notInTrie) == 0:
		return fmt.Sprintf("VERDICT (A) — APPLY/PROJECTION GAP. The agreed head's trie COMMITS to "+
			"%v, and those paths are not bound in the live tree. The version DAG is ahead of the "+
			"tree: a head was adopted without its bindings being projected. Route against the "+
			"merge apply path, NOT against delivery — both peers already have the head.", inTrie)
	case len(notInTrie) > 0 && len(inTrie) == 0:
		// **Split (B) again on one observable: does the peer that MADE
		// the write still hold it?** "Never captured" and "captured then
		// wiped" both leave the path out of the trie, and they are
		// different bugs in different files — so the verdict must not
		// name a suspect it has not separated.
		//
		//   B1 · the writer still holds it → the version DAG never
		//        learned about the write at all. Auto-version's capture
		//        side; nothing removed anything.
		//   B2 · nobody holds it → it was captured and then removed from
		//        the live tree. That is the F10 bug class, and
		//        ext/revision/merge.go::applyBindings is the live
		//        instance: it TreeRemoves every binding under the prefix
		//        and re-sets only what the merged tries carry, so a
		//        write in flight across that window is in neither. The
		//        FAST-FORWARD path was fixed for exactly this and the
		//        three-way path was not.
		var orphaned, wiped []string
		for _, path := range notInTrie {
			if a.Store().Has(path) || b.Store().Has(path) {
				orphaned = append(orphaned, path)
			} else {
				wiped = append(wiped, path)
			}
		}
		if len(wiped) > 0 {
			return fmt.Sprintf("VERDICT (B2) — CAPTURED THEN WIPED. %v are in neither peer's "+
				"live tree and in no version. Something removed them. Prime suspect: "+
				"ext/revision/merge.go::applyBindings — it TreeRemoves every binding under the "+
				"prefix and re-sets only what the merged tries carry, so a write in flight across "+
				"that window survives nowhere. The F10 bug class, fixed in fastForward and not in "+
				"the three-way path a bidirectional burst takes.", wiped)
		}
		return fmt.Sprintf("VERDICT (B1) — NEVER CAPTURED. %v are still held by the peer that "+
			"WROTE them and appear in NO version, so the counterpart has no way to learn of them: "+
			"the version DAG is the only channel and it never carried them. This is auto-version's "+
			"capture side, NOT a merge wipe — a wipe would have taken the writer's copy too, and "+
			"it did not. Terminal rather than transient: the head is settled, so nothing further "+
			"is emitted and no later merge can recover a write no version ever named.", orphaned)
	case len(inTrie) > 0 && len(notInTrie) > 0:
		return fmt.Sprintf("VERDICT MIXED — committed-but-unbound %v AND uncommitted-but-live %v. "+
			"Both gaps in one run; do not collapse them into one report.", inTrie, notInTrie)
	default:
		return "VERDICT INCONCLUSIVE — no missing path could be classified against the trie."
	}
}
