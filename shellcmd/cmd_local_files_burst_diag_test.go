package shellcmd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/types"
)

// TestE2E_BurstDiag_RevisionHistory is a diagnostic probe.
// Reproduces the bidirectional burst data-loss scenario and walks
// each peer's revision log to determine where files went missing.
//
// Two distinct hypotheses to discriminate between:
//
//	H1 — "Auto-versioner skipped some commits."
//	     File a-1 was ingested into alice's tree but no commit
//	     ever captured the tree state with a-1 present. Auto-
//	     versioner missed the tree change (e.g., debounce, race
//	     with the previous commit still in flight).
//
//	H2 — "Merge dropped already-committed entries."
//	     File a-1 WAS in some alice commit V_x. Later merges from
//	     bob's revisions produced a new committed state that
//	     doesn't include a-1. The merge's wipe-and-replace lost
//	     the binding.
//
// To distinguish: after the burst settles, for each missing file,
// walk alice's revision log and check whether ANY ancestor commit
// has the binding. If yes → H2 (merge dropped). If no → H1 (never
// committed).
//
// Run on demand; not part of the must-pass suite.
func TestE2E_BurstDiag_RevisionHistory(t *testing.T) {
	t.Skip("diagnostic — run on demand to investigate Finding 10's failure mode")

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	const targetPrefix = "archives/notes/"
	const burst = 5
	a, b := bringUpBidiPair(t, ctx, targetPrefix)
	time.Sleep(500 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < burst; i++ {
			_ = os.WriteFile(filepath.Join(a.fsDir, fmt.Sprintf("a-%d.md", i)),
				[]byte(fmt.Sprintf("# a %d\n", i)), 0600)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < burst; i++ {
			_ = os.WriteFile(filepath.Join(b.fsDir, fmt.Sprintf("b-%d.md", i)),
				[]byte(fmt.Sprintf("# b %d\n", i)), 0600)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	wg.Wait()

	// Let everything settle.
	time.Sleep(30 * time.Second)

	// Build the set of expected paths.
	expectedPaths := make([]string, 0, burst*2)
	for i := 0; i < burst; i++ {
		expectedPaths = append(expectedPaths,
			fmt.Sprintf("%sa-%d.md", targetPrefix, i),
			fmt.Sprintf("%sb-%d.md", targetPrefix, i))
	}

	for _, p := range []*bidiPeer{a, b} {
		t.Logf("=== %s — revision history walk ===", p.rootName)

		logResult, err := p.ap.Revision().Log(ctx, types.RevisionLogParamsData{Prefix: targetPrefix})
		if err != nil {
			t.Logf("  log error: %v", err)
			continue
		}
		t.Logf("  total revisions in log: %d", len(logResult.Versions))

		// Current tree state under the prefix.
		entries := p.ap.Store().List(targetPrefix)
		currentPaths := make(map[string]bool)
		for _, e := range entries {
			currentPaths[e.Path] = true
		}
		t.Logf("  current tree under %s: %d entries", targetPrefix, len(entries))

		// For each expected path, check: in the current tree?
		// in any ancestor commit?
		for _, expected := range expectedPaths {
			anywhere := false
			for _, k := range currentPaths {
				_ = k
			}
			// Current tree check (the location index list uses qualified
			// paths, so we need to check via store.Has on the relative
			// path).
			if p.ap.Store().Has(expected) {
				anywhere = true
			}
			// Note: checking historical commits would require the
			// revision-diff API to see what each commit's tree
			// state was. We approximate by checking the current
			// state only, since the merge's wipe-and-replace
			// behavior means historical bindings aren't in the
			// location index even if the entity is in the content
			// store.
			if !anywhere {
				t.Logf("  MISSING %s (not in current tree)", expected)
			}
		}
	}
}
