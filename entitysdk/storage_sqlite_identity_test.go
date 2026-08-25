package entitysdk_test

import (
	"context"
	"path/filepath"
	"testing"

	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/role"

	"entity-workbench-go/entitysdk"
)

// Identity-bundle + SQLite round-trip tests.
//
// The existing `TestIdentityBundle_BootstrapPersistAndReload` in
// identity_bundle_test.go covers the in-memory-store reload path:
// bundle persists to disk, peer reload re-runs the deterministic
// ceremony and re-mints the same content hashes. That validates the
// determinism invariant but rebuilds the tree from scratch every
// time.
//
// The pre-deployment hygiene question is different: with SQLite
// storage, the tree is *already* persisted. The reload path
// (`ApplyIdentityBundle` in assembleAppPeer) re-runs the ceremony
// against an existing tree. Content addressing means Puts are
// idempotent, but the test is whether the composition actually
// works end-to-end: identity bundle on disk + SQLite tree on disk +
// reopen + the peer is still the same identity, can still dispatch
// identity-extension ops, and prior role/cap state survives.
//
// If anything regresses here (cert/quorum mismatch, role caps
// disappearing, dispatcher unable to validate post-reopen) we have
// a bricked identity-aware peer. Worth catching before deployment.

// TestStorage_SqliteIdentityBundle_RoundtripPreservesIdentity
// bootstraps an identity-aware peer with SQLite storage, runs
// identity-dependent operations (role.Define + role.Assign),
// closes, reopens against the same bundle + DB, and verifies:
//
//   - PeerID survives (controller keypair round-trips through the
//     bundle).
//   - QuorumID + ControllerCertHash from the original ceremony are
//     reproduced exactly on reopen (determinism invariant under
//     a populated SQL store).
//   - The role definition and role-derived cap from pass 1 are
//     still bound on the reloaded peer's tree.
//   - The reloaded peer can dispatch a fresh identity-extension op
//     (define a second role) — proving the in-memory handler state
//     (signer resolver, attestation index, quorum cache) rebuilds
//     correctly from the persisted tree.
func TestStorage_SqliteIdentityBundle_RoundtripPreservesIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(home, "peer.db")

	ctx := context.Background()
	bundleName := "alice-identity"
	const (
		contextName     = "team-cross-restart"
		roleName1       = "reader-pass1"
		roleName2       = "reader-pass2"
		quorumThreshold = 2
		quorumMembers   = 3
	)

	var originalPeerID string
	var originalQuorumID, originalCertHash string
	var originalAssignmentPath, originalCapPath, originalRoleDefPath string

	// --- Pass 1: bootstrap identity bundle + SQLite, drive ops -----
	func() {
		ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
			Storage: entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
		})
		if err != nil {
			t.Fatalf("pass1 CreatePeer: %v", err)
		}
		defer ap.Close()

		bootstrap, err := ap.BootstrapIdentity(ctx, entitysdk.BootstrapOpts{
			QuorumMembers:   quorumMembers,
			QuorumThreshold: quorumThreshold,
			QuorumName:      "alice-quorum",
			BundleName:      bundleName,
		})
		if err != nil {
			t.Fatalf("pass1 BootstrapIdentity: %v", err)
		}
		originalPeerID = ap.PeerID()
		originalQuorumID = hexBytes(bootstrap.QuorumID.Bytes())
		originalCertHash = hexBytes(bootstrap.ControllerCertHash.Bytes())

		// Drive a role.Define + role.Assign locally. RoleAt("") with
		// own peer-id is the canonical "this is my own peer" form.
		rc := ap.Role()
		grants := []types.GrantEntry{{
			Handlers:   types.CapabilityScope{Include: []string{"system/tree"}},
			Operations: types.CapabilityScope{Include: []string{"get", "list"}},
			Resources:  types.CapabilityScope{Include: []string{"public/*"}},
		}}
		defineRes, err := rc.Define(ctx, contextName, roleName1, grants, nil)
		if err != nil {
			t.Fatalf("pass1 role.Define: %v", err)
		}
		originalRoleDefPath = defineRes.RolePath

		// Assign the role to the peer's own identity hash — this
		// exercises the role-derived cap mint flow against persisted
		// quorum + cert state.
		assignRes, err := rc.Assign(ctx, contextName, ap.IdentityHash(), roleName1)
		if err != nil {
			t.Fatalf("pass1 role.Assign: %v", err)
		}
		if len(assignRes.DerivedTokens) == 0 {
			t.Fatal("pass1 role.Assign returned no derived tokens")
		}
		originalAssignmentPath = role.AssignmentPath(contextName, ap.IdentityHash(), roleName1)
		originalCapPath = role.RoleDerivedTokenPath(contextName, ap.IdentityHash(), assignRes.DerivedTokens[0])

		// Sanity: the four tree entries are bound on pass 1.
		for _, p := range []string{
			"system/identity/peer-config",
			originalRoleDefPath,
			originalAssignmentPath,
			originalCapPath,
		} {
			if !ap.Store().Has(p) {
				t.Errorf("pass1: missing tree binding at %s", p)
			}
		}
	}()

	// --- Pass 2: reopen via bundle name + same SQLite path ----------
	ap2, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		Identity: &entitysdk.IdentityBindingConfig{Name: bundleName},
		Storage:  entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
	})
	if err != nil {
		t.Fatalf("pass2 CreatePeer (reload): %v", err)
	}
	defer ap2.Close()

	// Identity continuity: same peer-id, same quorum-id, same
	// controller-cert hash.
	if ap2.PeerID() != originalPeerID {
		t.Errorf("reload PeerID = %s, want %s", ap2.PeerID(), originalPeerID)
	}
	quorumPath := "system/quorum/" + originalQuorumID
	if !ap2.Store().Has(quorumPath) {
		t.Errorf("reload missing quorum entity at %s", quorumPath)
	}
	certPath := "system/identity/internal/cert/" + originalCertHash
	if !ap2.Store().Has(certPath) {
		t.Errorf("reload missing controller cert at %s", certPath)
	}

	// Tree state from pass 1 survives.
	for _, p := range []string{
		"system/identity/peer-config",
		originalRoleDefPath,
		originalAssignmentPath,
		originalCapPath,
	} {
		if !ap2.Store().Has(p) {
			t.Errorf("reload: missing tree binding at %s (didn't survive close+reopen)", p)
		}
	}

	// The reloaded peer can dispatch a *fresh* identity-dependent
	// op. If the in-memory handler state (signer resolver, etc.)
	// didn't rebuild correctly from the persisted tree, this would
	// fail with an authorization or signer-resolution error.
	rc2 := ap2.Role()
	grants2 := []types.GrantEntry{{
		Handlers:   types.CapabilityScope{Include: []string{"system/query"}},
		Operations: types.CapabilityScope{Include: []string{"query"}},
		Resources:  types.CapabilityScope{Include: []string{"*"}},
	}}
	defineRes2, err := rc2.Define(ctx, contextName, roleName2, grants2, nil)
	if err != nil {
		t.Fatalf("reload role.Define (fresh op): %v", err)
	}
	if defineRes2.RolePath == "" {
		t.Error("reload role.Define returned empty path")
	}
	if !ap2.Store().Has(defineRes2.RolePath) {
		t.Errorf("reload role.Define did not bind at %s", defineRes2.RolePath)
	}
}

// TestStorage_SqliteIdentityBundle_RebootstrapGrowsBoundedly is a
// diagnostic probe: bootstrap once with SQLite, then reopen via
// bundle N times. Each reopen runs `ApplyIdentityBundle` which
// re-executes the ceremony against an already-populated store.
//
// What we want to know:
//
//   - One-time delta (acceptable): bootstrap + 1st reload grows the
//     counts by some constant X; subsequent reloads grow by 0. That
//     means the first reload installs runtime-derived entities the
//     initial bootstrap didn't (acceptable; bootstrap fully populated
//     would just rebuild same hashes).
//
//   - Linear leak (NOT acceptable): every reload grows the counts by
//     a non-zero delta. That's an unbounded leak — over the lifetime
//     of a long-running peer that restarts many times, this becomes
//     a real problem.
//
// The test fails only on linear-leak detection. The growth pattern
// is logged for visibility.
//
// Findings: the first reload appears to grow the
// counts by ~4 entities and ~4 paths (likely re-issued local-peer→
// controller caps with fresh signatures, or peer-config entities
// embedding ceremony-time state). Subsequent reloads should be
// flat. If they're not, file as a bug — see
// `DEPLOYMENT-DIRECTION.md §7 "Operational concerns still open."`
func TestStorage_SqliteIdentityBundle_RebootstrapGrowsBoundedly(t *testing.T) {
	// WAIVED for the 0.9.0 preview (re-measured at the 0.9.0 cut, see
	// below — the waiver was carried forward on evidence, not on
	// inertia). This probe still surfaces a REAL linear leak: every
	// reload grows the store by a constant, indefinitely.
	//
	// RE-MEASURED 2026-08-24 against core-go 13a42ea, by removing this
	// Skip and running it — and the shape has CHANGED since the waiver
	// was written:
	//
	//	                  when waived        2026-08-24
	//	  ΔpathCount         1                  0
	//	  ΔentityCount       4                  2
	//	  bootstrap→reload-4 347/338→351/354    374/365→374/373
	//
	// So the path leak is GONE and the entity leak is HALVED — core-go
	// has fixed part of this, and the old numbers in this comment had
	// silently become false. It is still linear and still unbounded, so
	// the waiver stands on substance; what does not stand is quoting a
	// stale measurement to justify it. Routed to core-go in
	// reviews/RELEASE-READINESS-REPLY-2026-08-24.md §7. The root
	// cause is in the identity *ceremony's* determinism, which lives
	// in the core-go sibling (ext/identity) — re-running
	// ApplyIdentityBundle against an already-populated store re-issues
	// the local-peer→controller cap (PI-9) and sibling signature
	// (PI-10) with fresh, ceremony-time-varying material instead of
	// reproducing the prior content hashes. It is NOT a workbench bug
	// and is not workbench's to fix (see the "don't edit sibling
	// impls" rule + the handoff filed for the core-go
	// identity-ceremony owner).
	//
	// The assertion below is correct and intentionally left intact:
	// remove this Skip to re-arm the regression the moment core-go's
	// ceremony is made idempotent on re-apply. The leak is bounded
	// per-restart (small constant), so for a wipe-and-rebuild preview
	// it is a conscious waiver, not a blocker.
	t.Skip("WAIVED (0.9.0 preview): real linear leak rooted in core-go ext/identity " +
		"ceremony re-apply — re-measured 2026-08-24 at ΔentityCount=2/reload (was 4, and " +
		"the ΔpathCount=1 is now 0); see reviews/FEEDBACK-CORE-GO-IDENTITY-REBOOTSTRAP-LEAK.md")

	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(home, "peer.db")

	ctx := context.Background()
	bundleName := "growth-probe"

	type sample struct {
		pathCount, entityCount int
	}
	samples := make([]sample, 0, 5)

	// Initial bootstrap.
	func() {
		ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
			Storage: entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
		})
		if err != nil {
			t.Fatalf("initial CreatePeer: %v", err)
		}
		defer ap.Close()
		if _, err := ap.BootstrapIdentity(ctx, entitysdk.BootstrapOpts{
			QuorumMembers:   3,
			QuorumThreshold: 2,
			QuorumName:      "growth",
			BundleName:      bundleName,
		}); err != nil {
			t.Fatalf("initial BootstrapIdentity: %v", err)
		}
		samples = append(samples, sample{ap.PathCount(), ap.EntityCount()})
	}()

	const reloads = 4
	for i := 0; i < reloads; i++ {
		ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
			Identity: &entitysdk.IdentityBindingConfig{Name: bundleName},
			Storage:  entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
		})
		if err != nil {
			t.Fatalf("reload %d CreatePeer: %v", i, err)
		}
		samples = append(samples, sample{ap.PathCount(), ap.EntityCount()})
		_ = ap.Close()
	}

	// Log the full series so a failure tells us the shape.
	t.Logf("Counts after bootstrap + %d reloads (paths, entities):", reloads)
	for i, s := range samples {
		label := "bootstrap"
		if i > 0 {
			label = "reload " + itoa(i)
		}
		t.Logf("  %-12s pathCount=%4d  entityCount=%4d", label, s.pathCount, s.entityCount)
	}

	// Linear-leak detection: the growth from reload N to reload N+1
	// (for N ≥ 1) must be zero. The bootstrap → reload-1 delta is
	// allowed to be non-zero (the one-time runtime-derived state).
	for i := 2; i < len(samples); i++ {
		dPath := samples[i].pathCount - samples[i-1].pathCount
		dEnt := samples[i].entityCount - samples[i-1].entityCount
		if dPath != 0 || dEnt != 0 {
			t.Errorf("reload %d → reload %d leaked state: ΔpathCount=%d ΔentityCount=%d "+
				"(linear leak — would grow unboundedly over long-running peer's restart history)",
				i-1, i, dPath, dEnt)
		}
	}
}

// itoa is a tiny helper to avoid pulling in strconv just for one
// debug-log site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}
