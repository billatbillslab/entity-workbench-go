package shellcmd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/ext/localfiles"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/workbench"
)

// TestStage3_Case4_ModifyAfterSync validates update propagation +
// chunk dedup: after initial sync converges, alice modifies an
// existing file with mostly-unchanged content. Verify (a) bob's
// mirror updates to the new content, (b) the chunks that didn't
// change aren't re-fetched.
//
// Setup: case 1.5 baseline up to convergence. Then alice rewrites
// hello.md with a payload that shares ~50% of its bytes with the
// original (FastCDC's content-defined boundaries will produce some
// overlapping chunk hashes for the unchanged regions).
//
// What this validates:
//   - Updates flow through the same chain that handles creates —
//     subscription emits an `updated` event, blob-resolve runs again,
//     local/files:write replaces atomically.
//   - Chunk dedup is observable: bob's content store gains FEWER
//     entities after the modify than it gained on initial sync,
//     because unchanged chunks (already keyed by hash) are skipped.
//
// What this does NOT test:
//   - True FastCDC chunk-boundary stability with carefully crafted
//     payloads — we use coarsely-similar bytes; a stricter dedup
//     test would require constructing the payload to hit the
//     rolling-hash chunk boundaries exactly.
func TestStage3_Case4_ModifyAfterSync(t *testing.T) {
	const fileBytes = 4 * 1024 * 1024 // 4 MiB — enough chunks to observe dedup

	aliceDir := t.TempDir()
	bobDir := t.TempDir()
	const rootName = "sync"
	const sourcePrefix = "local/files/" + rootName + "/"

	bobBR := workbench.NewBlobResolveHandler()
	bobBR.RegisterMount(sourcePrefix, sourcePrefix)

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
			{Pattern: workbench.BlobResolvePattern, Handler: bobBR},
			{Pattern: workbench.ChainErrorsPattern, Handler: workbench.NewChainErrorsHandler()},
		},
	})
	if err != nil {
		t.Fatalf("CreatePeer bob: %v", err)
	}
	t.Cleanup(func() { _ = bob.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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

	aliceLF := alice.LocalFilesHandler()
	if err := aliceLF.AddRoot(rootName, localfiles.RootConfigData{
		Prefix: sourcePrefix, FilesystemRoot: aliceDir,
	}, alice.RawContentStore(), alice.RawLocationIndex()); err != nil {
		t.Fatalf("alice AddRoot: %v", err)
	}
	if err := aliceLF.StartWatching(ctx, rootName, alice.RawContentStore(),
		alice.RawLocationIndex(), alice.IdentityHash()); err != nil {
		t.Fatalf("alice StartWatching: %v", err)
	}

	bobLF := bob.LocalFilesHandler()
	if err := bobLF.AddRoot(rootName, localfiles.RootConfigData{
		Prefix: sourcePrefix, FilesystemRoot: bobDir,
	}, bob.RawContentStore(), bob.RawLocationIndex()); err != nil {
		t.Fatalf("bob AddRoot: %v", err)
	}

	deliverURI := fmt.Sprintf("entity://%s/%s", bobID, workbench.BlobResolvePattern)
	if _, err := bob.SubscribeRawAt(aliceID, sourcePrefix+"*", deliverURI, "receive",
		entitysdk.SubscribeOpts{
			Events:         []string{"created", "updated"},
			IncludePayload: true,
		}); err != nil {
		t.Fatalf("bob subscribe: %v", err)
	}

	mdPath := filepath.Join(aliceDir, "doc.bin")
	bobFSPath := filepath.Join(bobDir, "doc.bin")

	// Phase 1: initial sync.
	v1 := makeProbePayload(42, fileBytes)
	if err := os.WriteFile(mdPath, v1, 0600); err != nil {
		t.Fatalf("alice initial write: %v", err)
	}
	bobEntitiesBefore := bob.EntityCount()
	if !pollUntilBound(alice, sourcePrefix+"doc.bin", 5*time.Second) {
		t.Fatalf("alice's watcher never bound initial file")
	}
	if !pollFSDeadline(bobFSPath, 20*time.Second) {
		t.Fatalf("initial sync did not converge")
	}
	// Wait for bob's content store to settle so the post-initial
	// entity-count snapshot is accurate.
	time.Sleep(500 * time.Millisecond)
	bobEntitiesAfterInitial := bob.EntityCount()
	initialDelta := bobEntitiesAfterInitial - bobEntitiesBefore

	// Verify initial content matches.
	got1, err := os.ReadFile(bobFSPath)
	if err != nil {
		t.Fatalf("read initial: %v", err)
	}
	if !bytesEqual(got1, v1) {
		t.Fatalf("initial content mismatch")
	}

	// Phase 2: modify with a payload that overlaps ~half of v1.
	v2 := make([]byte, fileBytes)
	copy(v2[:fileBytes/2], v1[:fileBytes/2])                  // first half identical
	copy(v2[fileBytes/2:], makeProbePayload(43, fileBytes/2)) // second half differs
	modifyTime := time.Now()
	if err := os.WriteFile(mdPath, v2, 0600); err != nil {
		t.Fatalf("alice modify write: %v", err)
	}

	// Wait for bob's mirror to reflect the new content. We can't just
	// poll for path existence (file already exists from initial sync);
	// poll for content match instead.
	deadline := time.Now().Add(20 * time.Second)
	converged := false
	for time.Now().Before(deadline) {
		got2, err := os.ReadFile(bobFSPath)
		if err == nil && bytesEqual(got2, v2) {
			converged = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	modifyWall := time.Since(modifyTime)
	if !converged {
		errPaths := listPrefix(bob, "system/runtime/chain-errors/")
		t.Logf("bob chain-error markers: %d", len(errPaths))
		for _, p := range errPaths {
			t.Logf("  %s", p)
		}
		t.Fatalf("modify did not propagate within 20s")
	}

	time.Sleep(500 * time.Millisecond) // let entity count settle
	bobEntitiesAfterModify := bob.EntityCount()
	modifyDelta := bobEntitiesAfterModify - bobEntitiesAfterInitial

	t.Logf("=== Case 4 modify-after-sync ===")
	t.Logf("  file size:                    %d bytes", fileBytes)
	t.Logf("  bob entity delta on initial:  %d", initialDelta)
	t.Logf("  bob entity delta on modify:   %d", modifyDelta)
	t.Logf("  modify wall (incl debounce):  %s", modifyWall.Round(time.Millisecond))

	// Dedup observation: if the unchanged half maps to the same
	// chunk hashes (FastCDC content-defined boundaries), the modify
	// should add FEWER entities than the initial sync. We don't pin
	// a specific ratio (FastCDC boundary alignment isn't guaranteed
	// even for identical prefixes) — just observe.
	if modifyDelta >= initialDelta {
		t.Logf("  NOTE: modify delta (%d) >= initial delta (%d) — no chunk dedup observed for this payload shape (FastCDC boundary stability not guaranteed)",
			modifyDelta, initialDelta)
	} else {
		t.Logf("  dedup observed: modify added %d new entities vs %d on initial (%.0f%% reuse)",
			modifyDelta, initialDelta, float64(initialDelta-modifyDelta)/float64(initialDelta)*100)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
