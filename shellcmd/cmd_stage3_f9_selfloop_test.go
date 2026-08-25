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

// TestStage3_F9_SelfLoop_SinglePeer is the smallest possible F9
// reproducer: ONE peer, ONE watcher, ONE subscription that loops back
// to itself. No cross-peer transport involved.
//
// Setup:
//   - alice runs a watcher on local/files/sync/* (her own prefix).
//   - alice subscribes to /alice/local/files/sync/* with delivery to
//     her OWN workbench/blob-resolve handler.
//   - alice writes ONE file to her FS.
//
// Predicted loop chain (the F9 hypothesis):
//  1. alice's os.WriteFile → fsnotify event → watcher debounce →
//     flush → tree:put at /alice/local/files/sync/file.md.
//  2. Tree change → alice's subscription (to her own prefix) fires
//     → notification delivered to blob-resolve.
//  3. blob-resolve runs → calls content.AtPeer + EnsureClosure
//     (no-op; blob already local) + hctx.Execute("local/files",
//     "write", ...) content-mode to the same tree path.
//  4. handleWrite executes — overwrites file (atomic + idempotent
//     content) + binds file entity to tree.
//  5. **handleWrite's disk write fires another fsnotify event.**
//     Watcher debounces → flush → tree:put. The reverseTracker
//     should suppress this, but reverseTracker.markWritten is
//     ONLY called from reverseWrite (reverse.go:185), NOT from
//     handleWrite (operations.go:114). So the tracker is empty
//     and doesn't suppress.
//  6. Tree change → subscription fires → GOTO 4. Loop.
//
// What this test measures:
//   - entity-count growth over a 5-second window after one user write
//   - tree binding count under sourcePrefix at end of window
//   - chain-error marker count
//
// Under healthy convergence, post-write entity growth should bound
// at a small constant (~5: 1 file + 1 blob + 1 chunk + a few system
// entries). Under runaway loop, growth scales with elapsed time.
//
// **Documented limitation:** self-subscription on the SAME peer-id
// might not fire delivery if the subscription engine treats
// local-peer-id specially. If the entity-count stays flat with no
// growth, the test setup didn't actually engage the loop and we need
// the two-peer hub-and-spoke variant.
func TestStage3_F9_SelfLoop_SinglePeer(t *testing.T) {
	aliceDir := t.TempDir()
	const rootName = "sync"
	const sourcePrefix = "local/files/" + rootName + "/"

	bobBR := workbench.NewBlobResolveHandler()
	bobBR.RegisterMount(sourcePrefix, sourcePrefix)

	alice, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
		Handlers: []entitysdk.HandlerRegistration{
			{Pattern: workbench.BlobResolvePattern, Handler: bobBR},
			{Pattern: workbench.ChainErrorsPattern, Handler: workbench.NewChainErrorsHandler()},
		},
	})
	if err != nil {
		t.Fatalf("CreatePeer alice: %v", err)
	}
	t.Cleanup(func() { _ = alice.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bringUpListener(t, ctx, alice, "alice")

	aliceID := alice.PeerID()
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

	deliverURI := fmt.Sprintf("entity://%s/%s", aliceID, workbench.BlobResolvePattern)
	if _, err := alice.SubscribeRawAt(aliceID, sourcePrefix+"*", deliverURI, "receive",
		entitysdk.SubscribeOpts{
			Events:         []string{"created", "updated"},
			IncludePayload: true,
		}); err != nil {
		t.Fatalf("alice self-subscribe: %v", err)
	}

	preWriteEntities := alice.EntityCount()
	preWriteBindings := len(listPrefix(alice, sourcePrefix))

	content := "single self-loop probe\n"
	if err := os.WriteFile(filepath.Join(aliceDir, "loop-probe.md"), []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 5-second observation window with 500ms samples.
	type sample struct {
		t        time.Duration
		entities int
		bindings int
		errors   int
	}
	samples := []sample{}
	startWait := time.Now()
	for time.Since(startWait) < 5*time.Second {
		samples = append(samples, sample{
			t:        time.Since(startWait).Round(100 * time.Millisecond),
			entities: alice.EntityCount() - preWriteEntities,
			bindings: len(listPrefix(alice, sourcePrefix)) - preWriteBindings,
			errors:   len(listPrefix(alice, "system/runtime/chain-errors/")),
		})
		time.Sleep(500 * time.Millisecond)
	}

	t.Logf("=== F9 self-loop probe — single peer, no cross-peer transport ===")
	t.Logf("  pre-write entities: %d, pre-write bindings under prefix: %d", preWriteEntities, preWriteBindings)
	for _, s := range samples {
		t.Logf("  %5s: Δentities=%d Δbindings=%d errors=%d", s.t, s.entities, s.bindings, s.errors)
	}

	finalEntities := alice.EntityCount() - preWriteEntities
	finalBindings := len(listPrefix(alice, sourcePrefix)) - preWriteBindings

	switch {
	case finalEntities == 0:
		t.Logf("VERDICT: no entity growth — self-subscription via local-peer-id likely doesn't fire delivery. Need two-peer hub-and-spoke variant to repro the original F9.")
	case finalEntities <= 10:
		t.Logf("VERDICT: bounded growth (%d entities, %d bindings) — F9 LOOP SUPPRESSION WORKING.", finalEntities, finalBindings)
		t.Logf("  F9 fix sites (closed):")
		t.Logf("    1. ext/localfiles/operations.go::handleWrite — calls reverseTracker.markWritten before disk write (core-go 8ad52bc)")
		t.Logf("    2. workbench/blob_resolve.go::Handle — idempotency short-circuit when local tree already has the same blob hash at target path")
		t.Logf("  Without either fix, this test produced ~2200 entities / 5s; with both, bounded at ~4.")
	default:
		t.Logf("VERDICT: %d entities, %d bindings from ONE user write — RUNAWAY LOOP. F9 has regressed.", finalEntities, finalBindings)
		t.Logf("Check: (1) ext/localfiles/operations.go::handleWrite still calls reverseTracker.markWritten")
		t.Logf("       (2) workbench/blob_resolve.go::Handle still has the tryGetLocalFileBlobHash idempotency check")
		t.Errorf("F9 regression: expected bounded growth, got %d entities", finalEntities)
	}
}
