package entitysdk

// Test-only exports (the standard export_test.go idiom): reachable from the
// external entitysdk_test package, never part of the shipped API.
//
// This exists for the Axis-1 equivalence harness, which must drive the Stage-1
// and Axis-1 evaluators DIRECTLY — a differential test of two engines cannot go
// through system/compute:eval, because that path is hardwired to Stage-1 and
// would compare an engine against itself.
//
// Deliberately test-only. The SDK's rule is protocol-first — app code reaches
// entities through execute / tree get-put, never through the store or index
// (AGENTS.md). Shipping these as real accessors would invite exactly the direct
// store access that rule forbids, so they stay behind _test.go, where the only
// callers are engine-level probes that have a real reason to bypass the peer.

import (
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/ext/compute"
)

// TestContentStore exposes the peer's content store.
func (a *AppPeer) TestContentStore() store.ContentStore { return a.store.content }

// TestLocationIndex exposes the peer's location index.
func (a *AppPeer) TestLocationIndex() store.LocationIndex { return a.store.locationIndex }

// TestEvalContext builds an EvalContext equivalent to the one the compute
// handler constructs for an explicit eval, minus the capability ceiling.
//
// HasContentStoreAccess is true — the explicit-eval tier for a caller holding
// direct store access (§4.2 Tier 3). Capability is left zero, which makes
// evalLookupTree skip the permission check exactly as it does for the test and
// legacy callers Stage-1 already documents. Both engines receive the SAME
// context value, so any behavioral difference is the engine's, not the setup's.
func (a *AppPeer) TestEvalContext() *compute.EvalContext {
	return &compute.EvalContext{
		ContentStore:          a.store.content,
		LocationIndex:         a.store.locationIndex,
		LocalPeerID:           a.PeerID(),
		HasContentStoreAccess: true,
	}
}
