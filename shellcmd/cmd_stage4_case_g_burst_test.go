package shellcmd_test

// Stage 4 Case G — burst-write saturation under hub-and-spoke.
//
// Topology: 1 hub + 4 spokes (hub-and-spoke, NOT mesh). Single-publisher
// avoids the WB-28 2-peer reentrant-deadlock surface while still
// exercising high-cardinality fan-out. Hub writes K files in rapid
// succession (within one watcher debounce window so they coalesce
// into a small number of tree-batch notifications, then a longer phase
// where writes spread across multiple debounce windows to produce
// distinct notifications).
//
// What this stresses:
//
//  1. Hub-side subscription engine fan-out throughput. Each tree
//     change at the hub fires 4 outbound notifications. K files →
//     K notifications per spoke → 4K total outbound deliveries.
//
//  2. Spoke-side blob-resolve handler throughput. Each spoke processes
//     K incoming notifications + K cross-peer content fetches +
//     K local writes. Per-spoke serialized through the local
//     localfiles handler's mutex.
//
//  3. WB-21 saturation cliff approach. The known finding is ~2K
//     notifs/sec drops silently. We don't push to 2K (Case G is
//     hub-and-spoke at K=50 — manageable). What we measure is the
//     per-notification latency at moderate volume vs the lone-write
//     baseline (Stage 3 case 5 single-file).
//
//  4. Chunk dedup engagement. When K files share content (we use
//     varied-but-overlapping content so the FastCDC chunker can find
//     reuse), the content store should engage the dedup path on the
//     spoke side, reducing wire bytes for subsequent files.
//
// Not testing here (deferred):
//   - Push to true saturation (>2K/sec) — needs careful test design to
//     bypass watcher debounce coalescing entirely.
//   - Mesh burst (avoided due to WB-28 + WB-25 known 2-peer deadlock).
//   - Mixed file sizes with revision-tracked merge.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStage4_CaseG_BurstHubSpoke(t *testing.T) {
	const rootName = "sync"
	const sourcePrefix = "local/files/" + rootName + "/"
	const numPeers = 5 // 1 hub + 4 spokes
	const hubIdx = 0
	const numFiles = 50 // burst size

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	names := []string{"hub", "spoke1", "spoke2", "spoke3", "spoke4"}
	peers := stage4Setup(t, ctx, numPeers, names, rootName, sourcePrefix)
	stage4ConnectAllToAll(t, ctx, peers)
	stage4StartWatchers(t, ctx, peers, rootName, sourcePrefix)
	stage4WireHubSpoke(t, peers, hubIdx, rootName, sourcePrefix)

	hub := peers[hubIdx]

	// Burst phase: write K files as fast as possible. Within the
	// 2-second watcher debounce window. All K writes coalesce into a
	// single tree batch + a single fan-out wave.
	burstStart := time.Now()
	type fileSpec struct {
		filename string
		content  string
	}
	specs := make([]fileSpec, numFiles)
	for i := 0; i < numFiles; i++ {
		// Vary content: shared prefix (encourages chunk dedup) + per-file
		// suffix (forces distinct blob hashes).
		specs[i] = fileSpec{
			filename: fmt.Sprintf("burst-%03d.md", i),
			content:  "# Burst file\n\nShared body for FastCDC dedup probe.\n\n" + fmt.Sprintf("Per-file suffix %d\n", i),
		}
	}
	for _, s := range specs {
		path := filepath.Join(hub.fsRoot, s.filename)
		if err := os.WriteFile(path, []byte(s.content), 0600); err != nil {
			t.Fatalf("hub write %s: %v", s.filename, err)
		}
	}
	burstDuration := time.Since(burstStart)
	t.Logf("hub burst-wrote %d files in %s (%.0f files/sec)",
		numFiles, burstDuration, float64(numFiles)/burstDuration.Seconds())

	// Convergence: every spoke should have every file. Hub keeps its
	// K writes. Total: K × numPeers expected.
	convergenceStart := time.Now()
	deadline := time.Now().Add(120 * time.Second)
	converged := 0
	expected := numFiles * numPeers
	missing := []string{}
	for _, s := range specs {
		for i := range peers {
			p := peers[i]
			if !stage4AwaitFile(p, s.filename, deadline) {
				missing = append(missing, fmt.Sprintf("%s/%s", p.name, s.filename))
				continue
			}
			converged++
		}
	}
	convergenceDuration := time.Since(convergenceStart)
	t.Logf("converged %d/%d (peer × file) pairs in %s (%.2f ms/pair avg)",
		converged, expected, convergenceDuration,
		convergenceDuration.Seconds()*1000.0/float64(expected))

	if converged < expected {
		t.Errorf("missing %d/%d pairs: first 10 missing: %v",
			expected-converged, expected, missing[:minInt(10, len(missing))])
		stage4DumpDiagnostics(t, peers, sourcePrefix)
	}

	// Settle + bound check
	time.Sleep(5 * time.Second)
	stage4AssertEntityCountBound(t, peers, sourcePrefix, numFiles)
	stage4AssertNoChainErrors(t, peers)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
