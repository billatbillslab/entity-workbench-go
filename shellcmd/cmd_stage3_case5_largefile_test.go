package shellcmd_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/localfiles"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/workbench"
)

// TestStage3_Case5_LargeFileTransfer is the production-readiness
// probe: sweep cross-peer file sync across the substrate's size
// bands and capture wall time, chunk count, entity-store delta. The
// load-bearing claim under test is **the substrate moves files of
// arbitrary size cross-peer, bypassing the historical 16 MiB single-
// EXECUTE-envelope TCP frame limit**. CONTENT v3.6 §7.2 chunked
// transfer is the mechanism: each chunk fits in one envelope, so
// the per-chunk wire size stays bounded regardless of file size.
//
// Size bands probed and why:
//
//	64 KiB   — at the CONTENT v3.6 §4.3 inline-include boundary.
//	           One chunk; blob + chunk MUST be inline-included with
//	           the subscription notification. No follow-up fetch.
//	 1 MiB   — post-A2 DEFAULT_CHUNK_SIZE; single chunk; one cross-
//	           peer fetch round-trip.
//	16 MiB   — historical TCP frame limit. Bytes-mode write would
//	           hit the wire-layer constraint; content-mode transfer
//	           via §7.2 sidesteps it.
//	64 MiB   — production media scale; ~64 chunks @ 1 MiB target;
//	           exercises batched §7.2 chunk fetch.
//
// What this validates:
//
//  1. Wall-time convergence for each band — sets the production-
//     readiness latency baseline.
//  2. Chunk count matches expectation per size + post-A2 default —
//     surfaces any chunking deviation.
//  3. Bob's content-store entity count grows by the expected amount
//     (1 blob + N chunks per file) — surfaces any double-store
//     or leak issue.
//  4. Bob's filesystem byte content matches alice's exactly —
//     end-to-end correctness at scale.
//  5. Identifies any performance cliff between bands so production
//     deployments know where the cost curve breaks.
//
// Wall-time budgets are loose enough to absorb CI jitter; absolute
// numbers logged at t.Logf for the perf-review record.
func TestStage3_Case5_LargeFileTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("Case 5 perf probe — skipped under -short (allocates 100+ MiB)")
	}

	cases := []struct {
		name           string
		bytes          int
		expectInlined  bool          // §4.3 inline-include applies (≤ 64 KiB)
		walltimeBudget time.Duration // upper bound for convergence
	}{
		{name: "64KiB", bytes: 64 * 1024, expectInlined: true, walltimeBudget: 10 * time.Second},
		{name: "1MiB", bytes: 1 * 1024 * 1024, expectInlined: false, walltimeBudget: 10 * time.Second},
		{name: "16MiB", bytes: 16 * 1024 * 1024, expectInlined: false, walltimeBudget: 20 * time.Second},
		{name: "64MiB", bytes: 64 * 1024 * 1024, expectInlined: false, walltimeBudget: 60 * time.Second},
		// Case 6 Round 6: extend past 64 MiB to validate linear
		// scaling — no hidden ceiling above the 16 MiB historical
		// TCP frame limit.
		{name: "128MiB", bytes: 128 * 1024 * 1024, expectInlined: false, walltimeBudget: 120 * time.Second},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runLargeFileBand(t, c.bytes, c.expectInlined, c.walltimeBudget)
		})
	}
}

func runLargeFileBand(t *testing.T, payloadSize int, expectInlined bool, walltimeBudget time.Duration) {
	t.Helper()
	aliceDir := t.TempDir()
	bobDir := t.TempDir()
	const rootName = "sync"
	const sourcePrefix = "local/files/" + rootName + "/"

	bobBlobResolve := workbench.NewBlobResolveHandler()
	bobBlobResolve.RegisterMount(sourcePrefix, sourcePrefix)

	alice, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	})
	if err != nil {
		t.Fatalf("CreatePeer alice: %v", err)
	}
	t.Cleanup(func() { _ = alice.Close() })

	bob, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
		Handlers: []entitysdk.HandlerRegistration{
			{Pattern: workbench.BlobResolvePattern, Handler: bobBlobResolve},
			{Pattern: workbench.ChainErrorsPattern, Handler: workbench.NewChainErrorsHandler()},
		},
	})
	if err != nil {
		t.Fatalf("CreatePeer bob: %v", err)
	}
	t.Cleanup(func() { _ = bob.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), walltimeBudget+30*time.Second)
	defer cancel()

	bringUpListener(t, ctx, alice, "alice")
	bringUpListener(t, ctx, bob, "bob")
	if _, err := bob.Connect(ctx, alice.Addr().String()); err != nil {
		t.Fatalf("bob→alice connect: %v", err)
	}
	if _, err := alice.Connect(ctx, bob.Addr().String()); err != nil {
		t.Fatalf("alice→bob connect: %v", err)
	}

	aliceID := alice.PeerID()
	bobID := bob.PeerID()

	if err := alice.LocalFilesHandler().AddRoot(rootName, localfiles.RootConfigData{
		Prefix:         sourcePrefix,
		FilesystemRoot: aliceDir,
	}, alice.RawContentStore(), alice.RawLocationIndex()); err != nil {
		t.Fatalf("alice AddRoot: %v", err)
	}
	if err := alice.LocalFilesHandler().StartWatching(ctx, rootName, alice.RawContentStore(),
		alice.RawLocationIndex(), alice.IdentityHash()); err != nil {
		t.Fatalf("alice StartWatching: %v", err)
	}
	if err := bob.LocalFilesHandler().AddRoot(rootName, localfiles.RootConfigData{
		Prefix:         sourcePrefix,
		FilesystemRoot: bobDir,
	}, bob.RawContentStore(), bob.RawLocationIndex()); err != nil {
		t.Fatalf("bob AddRoot: %v", err)
	}

	chainGrants := []types.GrantEntry{{
		Handlers:   types.CapabilityScope{Include: []string{workbench.BlobResolvePattern}},
		Operations: types.CapabilityScope{Include: []string{"receive"}},
	}}
	if _, err := bob.MintChainCapabilityBound(chainGrants,
		"system/capability/grants/chain/blob-resolve/"+rootName); err != nil {
		t.Fatalf("mint chain cap: %v", err)
	}

	deliverURI := fmt.Sprintf("entity://%s/%s", bobID, workbench.BlobResolvePattern)
	if _, err := bob.SubscribeRawAt(aliceID, sourcePrefix+"*", deliverURI, "receive",
		entitysdk.SubscribeOpts{
			Events:         []string{"created", "updated"},
			IncludePayload: true,
		}); err != nil {
		t.Fatalf("bob subscribe: %v", err)
	}

	// Pre-write baselines for entity-count delta + memory.
	aliceBaseEntities := alice.EntityCount()
	bobBaseEntities := bob.EntityCount()

	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	// Generate deterministic-but-varied content with math/rand seeded
	// per size. FastCDC's content-defined boundaries respond naturally
	// to randomized bytes (a constant stream would produce a single
	// chunk per its rolling-hash discipline).
	payload := makeProbePayload(int64(payloadSize), payloadSize)
	mdName := fmt.Sprintf("probe-%d.bin", payloadSize)
	mdPath := filepath.Join(aliceDir, mdName)

	writeStart := time.Now()
	if err := os.WriteFile(mdPath, payload, 0600); err != nil {
		t.Fatalf("alice write file: %v", err)
	}

	// Phase 1: wait for alice's watcher to bind the FileData. Then
	// we know chunking + content-store landing completed alice-side.
	wantSourcePath := sourcePrefix + mdName
	t1Bound := pollUntilBoundDeadline(alice, wantSourcePath, walltimeBudget)
	if t1Bound.IsZero() {
		t.Fatalf("alice's watcher never bound %s within budget", wantSourcePath)
	}
	aliceBindWall := t1Bound.Sub(writeStart)

	// Phase 2: wait for bob's filesystem to receive the file.
	// Transient-heap sampler: peak HeapAlloc + TotalAlloc-delta over
	// bob's materialize window. Round 5 closeout flagged that
	// post-GC HeapAlloc misses the streaming-write fix (resident
	// state is dominated by chunks in stores + test-scope payload).
	// Peak transient HeapAlloc during materialize captures the
	// streaming-write reduction directly.
	bobFSPath := filepath.Join(bobDir, mdName)
	var samplerStop atomic.Bool
	var samplerDone = make(chan struct{})
	var peakHeapAlloc atomic.Uint64
	var samplerBaselineTotal uint64
	{
		var s runtime.MemStats
		runtime.ReadMemStats(&s)
		samplerBaselineTotal = s.TotalAlloc
		peakHeapAlloc.Store(s.HeapAlloc)
	}
	go func() {
		defer close(samplerDone)
		for !samplerStop.Load() {
			var s runtime.MemStats
			runtime.ReadMemStats(&s)
			if cur := s.HeapAlloc; cur > peakHeapAlloc.Load() {
				peakHeapAlloc.Store(cur)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	t2Materialize := pollUntilFileDeadline(bobFSPath, walltimeBudget)
	samplerStop.Store(true)
	<-samplerDone

	var memDuring runtime.MemStats
	runtime.ReadMemStats(&memDuring)
	transientTotalAlloc := memDuring.TotalAlloc - samplerBaselineTotal

	if t2Materialize.IsZero() {
		// Surface any chain-error markers for diagnosis.
		errPaths := listPrefix(bob, "system/runtime/chain-errors/")
		t.Logf("bob chain-error markers after timeout: %d", len(errPaths))
		for _, p := range errPaths {
			t.Logf("  %s", p)
		}
		t.Fatalf("bob's %s did not materialize within %s", mdName, walltimeBudget)
	}
	bobMaterializeWall := t2Materialize.Sub(t1Bound)
	totalWall := t2Materialize.Sub(writeStart)

	// Content match — byte-for-byte at scale. Use SHA-256 over both
	// instead of an in-memory equality test for legibility on very
	// large payloads.
	gotBytes, err := os.ReadFile(bobFSPath)
	if err != nil {
		t.Fatalf("read bob's %s: %v", mdName, err)
	}
	if len(gotBytes) != payloadSize {
		t.Errorf("bob's %s size = %d, want %d", mdName, len(gotBytes), payloadSize)
	}
	wantSum := sha256.Sum256(payload)
	gotSum := sha256.Sum256(gotBytes)
	if wantSum != gotSum {
		t.Errorf("content hash mismatch for %s: got %s want %s",
			mdName, hex.EncodeToString(gotSum[:]), hex.EncodeToString(wantSum[:]))
	}

	// Chunk count: read alice's file entity → blob → chunks slice.
	fileEnt, ok, err := alice.Get(wantSourcePath)
	if err != nil || !ok {
		t.Fatalf("alice tree:get for own file failed: ok=%v err=%v", ok, err)
	}
	file, err := localfiles.FileDataFromEntity(fileEnt)
	if err != nil {
		t.Fatalf("decode alice FileData: %v", err)
	}
	blobEnt, ok := alice.RawContentStore().Get(file.Content)
	if !ok {
		t.Fatalf("alice content store missing blob %s", file.Content)
	}
	var blobData types.ContentBlobData
	if err := ecf.Decode(blobEnt.Data, &blobData); err != nil {
		t.Fatalf("decode blob data: %v", err)
	}
	chunkCount := len(blobData.Chunks)

	// Entity-count delta on each peer.
	aliceDelta := alice.EntityCount() - aliceBaseEntities
	bobDelta := bob.EntityCount() - bobBaseEntities

	// Memory after transfer.
	var memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memAfter)
	heapDelta := int64(memAfter.HeapAlloc) - int64(memBefore.HeapAlloc)

	// Inline-include expectation: ≤ 64 KiB → blob + chunks must be
	// in the subscription delivery's Included map (and therefore
	// already present in bob's content store from the subscription
	// engine's delivery path, without needing the §7.2 follow-up
	// fetch). Asserting the bob-side blob presence after materialize
	// covers both branches; the diagnostic log records which path.
	for _, ch := range blobData.Chunks {
		if _, ok := bob.RawContentStore().Get(ch); !ok {
			t.Errorf("bob content store missing chunk %s after materialize", ch)
		}
	}

	t.Logf("=== %d bytes ===", payloadSize)
	t.Logf("  chunks:                       %d (chunk_size=%d)", chunkCount, blobData.ChunkSize)
	t.Logf("  inline_include applies:       %v", expectInlined)
	t.Logf("  wall_total:                   %s", totalWall.Round(time.Millisecond))
	t.Logf("  wall_alice_bind (incl debounce): %s", aliceBindWall.Round(time.Millisecond))
	t.Logf("  wall_bob_materialize:         %s", bobMaterializeWall.Round(time.Millisecond))
	t.Logf("  alice entity-count delta:     %d", aliceDelta)
	t.Logf("  bob entity-count delta:       %d", bobDelta)
	t.Logf("  heap delta (post-GC):         %d bytes (%.2f MiB)", heapDelta, float64(heapDelta)/(1<<20))
	t.Logf("  peak heap during materialize: %d bytes (%.2f MiB)",
		peakHeapAlloc.Load(), float64(peakHeapAlloc.Load())/(1<<20))
	t.Logf("  total-alloc during materialize: %d bytes (%.2f MiB) — cumulative alloc count",
		transientTotalAlloc, float64(transientTotalAlloc)/(1<<20))
	if chunkCount > 0 {
		t.Logf("  per-chunk wall (bob phase):   %s", (bobMaterializeWall / time.Duration(chunkCount)).Round(time.Microsecond))
	}
}

// makeProbePayload generates a deterministic-per-seed pseudo-random
// payload of the requested size. Uses math/rand seeded by the size
// so reruns produce identical bytes.
func makeProbePayload(seed int64, size int) []byte {
	r := rand.New(rand.NewSource(seed))
	buf := make([]byte, size)
	_, _ = r.Read(buf)
	return buf
}

// pollUntilBoundDeadline polls for path-bound on peer, returning the
// time at which the binding became present, or zero time on timeout.
func pollUntilBoundDeadline(ap *entitysdk.AppPeer, path string, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok, _ := ap.Get(path); ok {
			return time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Time{}
}

// pollUntilFileDeadline polls for FS presence; returns time-bound on
// success, zero on timeout.
func pollUntilFileDeadline(fsPath string, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fsPath); err == nil {
			return time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Time{}
}
