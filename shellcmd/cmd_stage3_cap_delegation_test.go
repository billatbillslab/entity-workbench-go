package shellcmd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/localfiles"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/workbench"
)

// TestStage3_CapDelegation_Positive replays case 1.5's subscription-
// driven cross-peer file sync, but with ALICE'S connection grants
// restricted to exactly what bob's blob-resolve handler needs to do
// its work — instead of `peer.OpenAccessGrants()` wildcard. Closes the
// cap-delegation empirical gap flagged in the v2 review: case 1, 1.5,
// 5 all use OpenAccessGrants, so the actual cross-peer cap-check path
// (alice verifies bob's cap-chain) is never exercised.
//
// The grant set alice extends to incoming connections (= cap she
// delegates to remote callers) is the minimum set blob-resolve needs:
//   - system/subscription:* on any resource — for bob to subscribe
//     to alice's prefix (the trigger)
//   - system/content:get on system/content — for bob's blob-resolve
//     to drive content.EnsureClosure during materialization
//   - local/files:read on any resource — required by the subscription
//     engine to enumerate matching tree paths when the subscription
//     pattern's namespace overlaps the local/files namespace
//   - system/tree:get on any resource — required by the subscription
//     engine's pattern-match traversal
//
// If the chain composes correctly with this grant set, the cross-peer
// cap path is genuinely exercised (not bypassed via wildcard). If the
// chain stalls or errors with one of these grants missing, that
// pinpoints which op needs cap-coverage in production.
//
// Resources are bare "*" only (peer-scoped), never also "/*/*"
// (cross-peer). Since core-go's §PR-8 canonicalization + §3
// advertisement discipline (connect.go's AssembleInboundGrants), a
// peer can only ever advertise coverage over ITS OWN namespace — "*"
// canonicalizes to "/{aliceID}/*", never "/*/*". A grant entry listing
// "/*/*" alongside "*" has one Include member the advertised scope can
// never cover, and coverage requires EVERY Include member to match, so
// the whole entry (not just the uncoverable member) gets dropped at
// connect time — silently revoking the same-peer authority "*" alone
// would have granted. All of these operations target alice's own
// namespace, so "*" alone is both correct and sufficient.
func TestStage3_CapDelegation_Positive(t *testing.T) {
	scopedGrants := []types.GrantEntry{
		{
			Handlers:   types.CapabilityScope{Include: []string{"system/subscription"}},
			Operations: types.CapabilityScope{Include: []string{"*"}},
			Resources:  types.CapabilityScope{Include: []string{"*"}},
		},
		{
			Handlers:   types.CapabilityScope{Include: []string{"system/content"}},
			Operations: types.CapabilityScope{Include: []string{"get"}},
			Resources:  types.CapabilityScope{Include: []string{"system/content"}},
		},
		{
			Handlers:   types.CapabilityScope{Include: []string{"local/files"}},
			Operations: types.CapabilityScope{Include: []string{"read"}},
			Resources:  types.CapabilityScope{Include: []string{"*"}},
		},
		{
			Handlers:   types.CapabilityScope{Include: []string{"system/tree"}},
			Operations: types.CapabilityScope{Include: []string{"get"}},
			Resources:  types.CapabilityScope{Include: []string{"*"}},
		},
	}

	if err := runCapDelegatedSyncCase(t, scopedGrants); err != nil {
		t.Fatalf("scoped-grants case 1.5 should succeed with the minimum grant set: %v", err)
	}
}

// TestStage3_CapDelegation_Negative_NoContentGrant complements the
// positive variant: omit `system/content:get` from alice's connection
// grants, expect the chain to fail at materialize (bob's blob-resolve
// calls EnsureClosure which dispatches system/content:get cross-peer
// to alice; alice's cap-check should reject with 403 because nothing
// in her grant set authorizes content:get for bob).
//
// This is the negative validation that proves the positive test
// actually exercises the cap-check path. If both positive and
// negative pass alice's check, the path is being bypassed somewhere.
func TestStage3_CapDelegation_Negative_NoContentGrant(t *testing.T) {
	scopedGrants := []types.GrantEntry{
		{
			Handlers:   types.CapabilityScope{Include: []string{"system/subscription"}},
			Operations: types.CapabilityScope{Include: []string{"*"}},
			Resources:  types.CapabilityScope{Include: []string{"*"}},
		},
		// system/content:get DELIBERATELY ABSENT.
		{
			Handlers:   types.CapabilityScope{Include: []string{"local/files"}},
			Operations: types.CapabilityScope{Include: []string{"read"}},
			Resources:  types.CapabilityScope{Include: []string{"*"}},
		},
		{
			Handlers:   types.CapabilityScope{Include: []string{"system/tree"}},
			Operations: types.CapabilityScope{Include: []string{"get"}},
			Resources:  types.CapabilityScope{Include: []string{"*"}},
		},
	}
	err := runCapDelegatedSyncCase(t, scopedGrants)
	if err == nil {
		t.Fatal("expected materialization to FAIL without system/content:get grant on alice's side; got success — cap-check is being bypassed")
	}
	t.Logf("negative case correctly stalled: %v", err)
}

// runCapDelegatedSyncCase runs the case 1.5 subscription-driven
// cross-peer file sync end-to-end with alice's connection grants set
// to the supplied scopedGrants (instead of OpenAccessGrants). Returns
// nil on materialization-converged-content-matched; returns an error
// (test-relayable, not t.Fatal'd) if any step stalls or mismatches.
//
// Wall-time budget for materialize is 15 s (matches case 1.5).
func runCapDelegatedSyncCase(t *testing.T, aliceGrants []types.GrantEntry) error {
	t.Helper()
	aliceDir := t.TempDir()
	bobDir := t.TempDir()
	const rootName = "sync"
	const sourcePrefix = "local/files/" + rootName + "/"

	bobBlobResolve := workbench.NewBlobResolveHandler()
	bobBlobResolve.RegisterMount(sourcePrefix, sourcePrefix)

	alice, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(aliceGrants)},
	})
	if err != nil {
		return fmt.Errorf("CreatePeer alice: %w", err)
	}
	t.Cleanup(func() { _ = alice.Close() })

	// Bob keeps wildcard grants — only alice's incoming-connection
	// grants are the cap surface under test. (Bob's outgoing
	// authority is what alice's grants authorize.)
	bob, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
		Handlers: []entitysdk.HandlerRegistration{
			{Pattern: workbench.BlobResolvePattern, Handler: bobBlobResolve},
			{Pattern: workbench.ChainErrorsPattern, Handler: workbench.NewChainErrorsHandler()},
		},
	})
	if err != nil {
		return fmt.Errorf("CreatePeer bob: %w", err)
	}
	t.Cleanup(func() { _ = bob.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bringUpListener(t, ctx, alice, "alice")
	bringUpListener(t, ctx, bob, "bob")
	if _, err := bob.Connect(ctx, alice.Addr().String()); err != nil {
		return fmt.Errorf("bob→alice connect: %w", err)
	}
	if _, err := alice.Connect(ctx, bob.Addr().String()); err != nil {
		return fmt.Errorf("alice→bob connect: %w", err)
	}

	aliceID := alice.PeerID()
	bobID := bob.PeerID()

	// Alice mounts + watches.
	aliceLF := alice.LocalFilesHandler()
	if aliceLF == nil {
		return fmt.Errorf("alice local/files handler not wired")
	}
	if err := aliceLF.AddRoot(rootName, localfiles.RootConfigData{
		Prefix:         sourcePrefix,
		FilesystemRoot: aliceDir,
	}, alice.RawContentStore(), alice.RawLocationIndex()); err != nil {
		return fmt.Errorf("alice AddRoot: %w", err)
	}
	if err := aliceLF.StartWatching(ctx, rootName, alice.RawContentStore(),
		alice.RawLocationIndex(), alice.IdentityHash()); err != nil {
		return fmt.Errorf("alice StartWatching: %w", err)
	}

	// Bob mounts (no watcher — sink role, same as case 1.5).
	bobLF := bob.LocalFilesHandler()
	if bobLF == nil {
		return fmt.Errorf("bob local/files handler not wired")
	}
	if err := bobLF.AddRoot(rootName, localfiles.RootConfigData{
		Prefix:         sourcePrefix,
		FilesystemRoot: bobDir,
	}, bob.RawContentStore(), bob.RawLocationIndex()); err != nil {
		return fmt.Errorf("bob AddRoot: %w", err)
	}

	// Mint bob's local chain cap for the inbound subscription delivery.
	chainGrants := []types.GrantEntry{{
		Handlers:   types.CapabilityScope{Include: []string{workbench.BlobResolvePattern}},
		Operations: types.CapabilityScope{Include: []string{"receive"}},
	}}
	if _, err := bob.MintChainCapabilityBound(chainGrants,
		"system/capability/grants/chain/blob-resolve/"+rootName); err != nil {
		return fmt.Errorf("mint blob-resolve chain cap: %w", err)
	}

	// Subscribe with include_payload (case 1.5 shape).
	deliverURI := fmt.Sprintf("entity://%s/%s", bobID, workbench.BlobResolvePattern)
	if _, err := bob.SubscribeRawAt(aliceID, sourcePrefix+"*", deliverURI, "receive",
		entitysdk.SubscribeOpts{
			Events:         []string{"created", "updated"},
			IncludePayload: true,
		}); err != nil {
		return fmt.Errorf("bob subscribe to alice: %w", err)
	}

	// Alice writes the file. Standard case 1.5 flow from here.
	mdContent := "# Cap delegation variant\n\nUnder restricted alice grants.\n"
	mdPath := filepath.Join(aliceDir, "hello.md")
	if err := os.WriteFile(mdPath, []byte(mdContent), 0600); err != nil {
		return fmt.Errorf("alice write file: %w", err)
	}

	wantSourcePath := sourcePrefix + "hello.md"
	if !pollUntilBound(alice, wantSourcePath, 5*time.Second) {
		return fmt.Errorf("alice's watcher never bound %s", wantSourcePath)
	}

	bobFSPath := filepath.Join(bobDir, "hello.md")
	deadline := time.Now().Add(15 * time.Second)
	var landed bool
	for time.Now().Before(deadline) {
		if _, err := os.Stat(bobFSPath); err == nil {
			landed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !landed {
		// Surface any chain-error markers — these tell us whether the
		// cap-check tripped and where.
		errPaths := listPrefix(bob, "system/runtime/chain-errors/")
		var diag string
		for _, p := range errPaths {
			diag += "  " + p + "\n"
		}
		if diag != "" {
			return fmt.Errorf("bob's hello.md did not materialize within 15s; chain-error markers:\n%s", diag)
		}
		return fmt.Errorf("bob's hello.md did not materialize within 15s; no chain-error markers (silent failure)")
	}

	gotBytes, err := os.ReadFile(bobFSPath)
	if err != nil {
		return fmt.Errorf("read bob's hello.md: %w", err)
	}
	if string(gotBytes) != mdContent {
		return fmt.Errorf("bob's hello.md content mismatch: got %q want %q", string(gotBytes), mdContent)
	}
	t.Logf("cap-delegation positive: materialization converged with restricted alice grants (%d bytes)", len(gotBytes))
	return nil
}
