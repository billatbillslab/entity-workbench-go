package entitysdk_test

// The query index must survive a restart against a persistent store.
//
// The three query indexes core-go ships are in-memory only
// (MemoryTypeIndex / MemoryReverseHashIndex / MemoryPathLinkIndex) and are
// populated solely by the "query" sync hook — that is, only by writes made
// during the running process. Paired with a sqlite store and no backfill,
// the tree survives a restart and the index does not: `tree:list` shows an
// entity that `system/query` swears does not exist.
//
// That asymmetry is invisible to every single-process test in this package,
// which is why it shipped. It reached the user as three separate verbs going
// quiet — `find`, `grep` and `compute aggregate` all query — against a store
// whose contents `ls` was happily printing one line earlier.
//
// Tier: regression (real-session coverage — the bug only exists across the
// process boundary, so the assertion has to cross it too).

import (
	"testing"

	coretypes "go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// TestQueryIndex_SurvivesRestart_PersistentStore writes an entity through
// one peer, closes it, reopens the same sqlite store, and asserts the query
// handler still finds it. Without the Rebuild backfill in assembleAppPeer the
// second peer reports zero matches while Store().List reports one.
func TestQueryIndex_SurvivesRestart_PersistentStore(t *testing.T) {
	dbPath := t.TempDir() + "/query-restart.db"
	t.Setenv("HOME", t.TempDir())
	if _, err := entitysdk.CreateIdentity("query-restart"); err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	open := func(label string) *entitysdk.AppPeer {
		ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
			Storage:  entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
			Identity: &entitysdk.IdentityBindingConfig{Name: "query-restart"},
		})
		if err != nil {
			t.Fatalf("CreatePeer %s: %v", label, err)
		}
		return ap
	}

	// --- Phase 1: write, and confirm the index sees it in-process ---
	p1 := open("v1")
	path := "/" + p1.PeerID() + "/files/a"
	if _, err := p1.Put(path, "app/file", map[string]interface{}{
		"name": "a",
		"size": uint64(100),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	countMatches := func(ap *entitysdk.AppPeer, label string) int {
		limit := uint64(1000)
		res, err := ap.Executor().Query(coretypes.QueryExpressionData{
			PathPrefix: "files", Limit: &limit,
		})
		if err != nil {
			t.Fatalf("query %s: %v", label, err)
		}
		return len(res.Matches)
	}

	if got := countMatches(p1, "v1"); got != 1 {
		t.Fatalf("in-process query found %d matches, want 1 — the sync hook itself is broken", got)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close v1: %v", err)
	}

	// --- Phase 2: reopen the same store; the index must be rebuilt ---
	p2 := open("v2")
	defer p2.Close()

	// The tree half of the asymmetry: this passed even with the bug, and is
	// what made the query half read as "the entity is gone" rather than "the
	// index is empty".
	if _, ok := p2.Store().Get(path); !ok {
		t.Fatalf("tree lost %s across the restart — this test is measuring the wrong thing", path)
	}

	if got := countMatches(p2, "v2"); got != 1 {
		t.Errorf("after restart the query index found %d matches, want 1; "+
			"the tree still holds the entity, so `find`/`grep`/`compute aggregate` "+
			"are blind to everything written before this process started", got)
	}
}
