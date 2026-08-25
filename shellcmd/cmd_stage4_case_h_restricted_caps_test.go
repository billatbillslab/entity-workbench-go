package shellcmd_test

// Stage 4 Case H — 3-peer symmetric mesh with restricted connection caps.
//
// Topology: same as Case A (3-peer symmetric mesh, every peer subscribes
// to every other). Differs in the cap-discipline dimension: every peer's
// incoming-connection grants are restricted to the minimum set the
// canonical Stage 3 substrate needs to function (per the Stage 3
// cap-delegation positive case), instead of OpenAccessGrants.
//
// What this stresses:
//
//  1. Symmetric scoped grants must compose under mesh. In the 2-peer
//     cap-delegation positive test, alice extended restricted grants
//     and bob was the consumer (bob mints, bob subscribes, bob's
//     blob-resolve fetches from alice). In a mesh, EVERY peer plays
//     both roles. The same scoped grant set must work on all peers
//     for both inbound subscribe-and-fetch directions.
//
//  2. The grant set is symmetric — each peer is granting OTHER peers
//     the right to subscribe, content-fetch, and tree-read. There's no
//     asymmetric "I'm the source" vs "I'm the sink" cap configuration
//     in the mesh case.
//
//  3. This is the third datapoint after Stage 3 cap-delegation positive
//     (2-peer, asymmetric roles) and Stage 3 cap-delegation negative
//     (2-peer, missing grant fails as expected). Mesh adds the
//     "everyone plays every role simultaneously" dimension and verifies
//     the grant set doesn't develop new requirements at higher N.
//
// What this does NOT yet test:
//   - Per-peer asymmetric grants (some peers more privileged than others)
//   - Cascade/transitive cap delegation (whether bob can sub-delegate
//     to carol; spec-level question, parked)
//   - Negative variants (which grant is required by which step) —
//     already covered by 2-peer Stage 3 cap-delegation negative test.
//
// **Findings surfaced by this test:**
//
//   1. The Stage 3 cap-delegation positive test's "minimum grant set" is
//      INCOMPLETE for symmetric mesh. It lacks workbench/blob-resolve:receive.
//      In 2-peer asymmetric setup (alice scoped + bob wildcard), the
//      missing grant was masked because the chain receiver (bob) had
//      wildcard. In mesh, every peer is a chain receiver, so the grant
//      gap manifests. Routed as Finding 3 to docs/cap-discipline-guide.
//
//   2. Cap-rejected inbound chain dispatch is SILENT — no chain-error
//      marker is bound on the receiver when the engine rejects the
//      delivery on cap-check. Initial run had 0 chain-error markers
//      across all 3 peers despite 0 of 6 cross-peer deliveries succeeding.
//      Routed as observability finding to core-go subscription team.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/types"
)

func TestStage4_CaseH_RestrictedCapsMesh3(t *testing.T) {
	const rootName = "sync"
	const sourcePrefix = "local/files/" + rootName + "/"

	// Minimum grant set for symmetric mesh: subscribe + content:get +
	// local-files:read + tree:get on appropriate scopes. Mirrors the
	// Stage 3 cap-delegation positive test's grant set, applied
	// symmetrically to every mesh peer. Resources are bare "*"
	// (peer-scoped) only — see cmd_stage3_cap_delegation_test.go's
	// TestStage3_CapDelegation_Positive doc comment for why "/*/*"
	// alongside it silently drops the whole grant entry at connect
	// time under core-go's §PR-8 advertisement discipline.
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
		// NEW for mesh: each peer must authorize incoming
		// workbench/blob-resolve:receive dispatches from the
		// subscription engines of other peers. In the 2-peer Stage 3
		// cap-delegation positive test, bob (the receiver) had wildcard
		// grants and this grant wasn't needed in alice's restricted
		// set. In mesh, every peer is a receiver; every peer must
		// grant inbound blob-resolve.
		{
			Handlers:   types.CapabilityScope{Include: []string{"workbench/blob-resolve"}},
			Operations: types.CapabilityScope{Include: []string{"receive"}},
			Resources:  types.CapabilityScope{Include: []string{"*"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	peers := stage4SetupWithGrants(t, ctx, 3,
		[]string{"alice", "bob", "carol"},
		rootName, sourcePrefix, scopedGrants)
	stage4ConnectAllToAll(t, ctx, peers)
	stage4StartWatchers(t, ctx, peers, rootName, sourcePrefix)
	stage4WireMesh(t, peers, rootName, sourcePrefix)

	type fileSpec struct {
		filename string
		content  string
		author   int
	}
	specs := []fileSpec{
		{"alice-restricted.md", "# Alice\n\nUnder restricted caps; should propagate.\n", 0},
		{"bob-restricted.md", "# Bob\n\nUnder restricted caps; should propagate.\n", 1},
		{"carol-restricted.md", "# Carol\n\nUnder restricted caps; should propagate.\n", 2},
	}

	for _, s := range specs {
		p := peers[s.author]
		path := filepath.Join(p.fsRoot, s.filename)
		if err := os.WriteFile(path, []byte(s.content), 0600); err != nil {
			t.Fatalf("%s write %s: %v", p.name, s.filename, err)
		}
	}

	startConvergence := time.Now()
	deadline := time.Now().Add(45 * time.Second)
	convergedCount := 0
	expectedCount := len(specs) * len(peers)

	for _, s := range specs {
		for i := range peers {
			p := peers[i]
			label := fmt.Sprintf("%s has %s", p.name, s.filename)
			if !stage4AwaitFile(p, s.filename, deadline) {
				t.Errorf("%s — did not materialize within deadline", label)
				continue
			}
			target := filepath.Join(p.fsRoot, s.filename)
			got, err := os.ReadFile(target)
			if err != nil {
				t.Errorf("%s read: %v", label, err)
				continue
			}
			if string(got) != s.content {
				t.Errorf("%s content mismatch", label)
				continue
			}
			convergedCount++
		}
	}
	t.Logf("restricted-cap mesh converged %d/%d (peer × file) pairs in %s",
		convergedCount, expectedCount, time.Since(startConvergence))

	if convergedCount < expectedCount {
		// Surface chain-error markers to pinpoint which grant is
		// missing in mesh-context (vs the 2-peer Stage 3 case).
		stage4DumpDiagnostics(t, peers, sourcePrefix)
	}

	time.Sleep(3 * time.Second)
	stage4AssertEntityCountBound(t, peers, sourcePrefix, len(peers))
	stage4AssertNoChainErrors(t, peers)
}
