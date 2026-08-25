package entitysdk_test

import (
	"context"
	"testing"

	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// TestMintChainCapability_RejectsEmptyGrants is the structural
// guard: minting a cap with zero grants would produce a cap that
// authorizes nothing — and worse, would mask intent ("I forgot to
// specify grants" looks the same as "I explicitly granted nothing").
// Reject at the API boundary.
func TestMintChainCapability_RejectsEmptyGrants(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	if _, err := ap.MintChainCapability(nil); err == nil {
		t.Error("expected error on nil grants")
	}
	if _, err := ap.MintChainCapability([]types.GrantEntry{}); err == nil {
		t.Error("expected error on empty grants")
	}
}

// TestMintChainCapability_ProducesValidCap covers the happy path:
// supply a scoped grant set, get back a content-addressed cap
// entity whose data round-trips and whose grantee is the local
// peer's identity hash.
func TestMintChainCapability_ProducesValidCap(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	grants := []types.GrantEntry{
		{
			Handlers:   types.CapabilityScope{Include: []string{"local/files"}},
			Operations: types.CapabilityScope{Include: []string{"read"}},
			Resources:  types.CapabilityScope{Include: []string{"local/files/notes/*"}},
		},
		{
			Handlers:   types.CapabilityScope{Include: []string{"system/tree"}},
			Operations: types.CapabilityScope{Include: []string{"put"}},
			Resources:  types.CapabilityScope{Include: []string{"archives/notes/*"}},
		},
	}

	capEnt, err := ap.MintChainCapability(grants)
	if err != nil {
		t.Fatalf("MintChainCapability: %v", err)
	}
	if capEnt.ContentHash.IsZero() {
		t.Fatal("minted cap has zero content hash")
	}
	if capEnt.Type != "system/capability/token" {
		t.Errorf("cap type = %s, want system/capability/token", capEnt.Type)
	}

	tok, err := types.CapabilityTokenDataFromEntity(capEnt)
	if err != nil {
		t.Fatalf("decode cap: %v", err)
	}
	if len(tok.Grants) != 2 {
		t.Errorf("grants len = %d, want 2", len(tok.Grants))
	}
	if tok.Grantee != ap.IdentityHash() {
		t.Errorf("grantee = %s, want local identity %s", tok.Grantee, ap.IdentityHash())
	}
	// Self-cap: granter and grantee match.
	if !tok.Granter.IsSingle() {
		t.Error("expected single-sig granter, got multi-sig")
	}
	granterHash, ok := tok.Granter.SingleHash()
	if !ok {
		t.Fatal("granter.SingleHash returned !ok for a single-sig granter")
	}
	if granterHash != ap.IdentityHash() {
		t.Errorf("granter = %s, want local identity %s", granterHash, ap.IdentityHash())
	}
}

// TestMintChainCapability_AcceptedByContinuationInstall is the
// load-bearing integration check: a scoped cap minted by this
// helper passes the R1 creator-authorization check at continuation
// install (EXTENSION-CONTINUATION §3.2 step 4). If this fails the
// helper is producing caps the system can't actually use.
func TestMintChainCapability_AcceptedByContinuationInstall(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	grants := []types.GrantEntry{{
		Handlers:   types.CapabilityScope{Include: []string{"system/tree"}},
		Operations: types.CapabilityScope{Include: []string{"put", "get"}},
		Resources:  types.CapabilityScope{Include: []string{"scratch/*"}},
	}}
	capEnt, err := ap.MintChainCapability(grants)
	if err != nil {
		t.Fatalf("MintChainCapability: %v", err)
	}

	// Build a minimal continuation that targets system/tree:put
	// (within the cap's grant scope) and install it.
	contData := types.ContinuationData{
		Target:             "system/tree",
		Operation:          "put",
		DispatchCapability: capEnt.ContentHash,
	}
	contEnt, err := contData.ToEntity()
	if err != nil {
		t.Fatalf("ContinuationData.ToEntity: %v", err)
	}

	installPath := "system/inbox/test-scoped-cap"
	if _, err := ap.Continuation().Install(context.Background(), installPath, contEnt); err != nil {
		t.Fatalf("continuation install rejected scoped cap: %v", err)
	}
	// Cleanup.
	_ = ap.Continuation().Abandon(context.Background(), installPath)
}
