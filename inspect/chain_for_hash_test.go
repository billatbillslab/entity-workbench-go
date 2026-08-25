package inspect_test

import (
	"testing"
	"time"

	coretypes "go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/inspect"
)

// TestChainForHash_MarkerEntity: a chain-error-lost marker entity's
// hash maps back to the chain_id encoded in its path.
func TestChainForHash_MarkerEntity(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	const chainID = "chain-for-hash-marker-001"
	body := coretypes.ChainErrorLostData{
		Code:      "not_found",
		Status:    404,
		TargetURI: "entity://peer/system/inbox/missing",
		Timestamp: uint64(time.Now().UnixMicro()),
		Reason:    "not_found",
		ChainID:   chainID,
		StepIndex: "2",
	}
	ent, err := body.ToEntity()
	if err != nil {
		t.Fatal(err)
	}

	markerPath := "system/runtime/chain-errors/lost/" + chainID + "/2/not_found/" + ent.ContentHash.String()
	if _, err := ap.PutEntity(markerPath, ent); err != nil {
		t.Fatalf("PutEntity: %v", err)
	}

	got, ok := inspect.ChainForHash(ap, ent.ContentHash)
	if !ok {
		t.Fatalf("ChainForHash(%s) = ok=false; want %q", ent.ContentHash, chainID)
	}
	if got != chainID {
		t.Errorf("ChainForHash = %q; want %q", got, chainID)
	}
}

// TestChainForHash_SuspendedContinuation: a suspended continuation
// entity's hash maps back to the chain_id in its body.
func TestChainForHash_SuspendedContinuation(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	const chainID = "chain-for-hash-suspended-001"
	body := coretypes.ContinuationSuspendedData{
		Target:      "system/continuation",
		Operation:   "advance",
		Reason:      "test-suspension",
		ChainID:     chainID,
		SuspendedAt: uint64(time.Now().UnixMicro()),
	}
	ent, err := body.ToEntity()
	if err != nil {
		t.Fatal(err)
	}

	path := "system/continuation/suspended/sus-cfh-1"
	if _, err := ap.PutEntity(path, ent); err != nil {
		t.Fatalf("PutEntity: %v", err)
	}

	got, ok := inspect.ChainForHash(ap, ent.ContentHash)
	if !ok {
		t.Fatalf("ChainForHash(%s) = ok=false; want %q", ent.ContentHash, chainID)
	}
	if got != chainID {
		t.Errorf("ChainForHash = %q; want %q", got, chainID)
	}
}

// TestChainForHash_NotFound: arbitrary entity hash with no marker/
// suspended-continuation attribution returns ok=false.
func TestChainForHash_NotFound(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	// Put a non-attributable entity at an arbitrary path.
	body := coretypes.ChainErrorLostData{
		Reason:    "irrelevant",
		ChainID:   "irrelevant-chain",
		StepIndex: "1",
		Timestamp: uint64(time.Now().UnixMicro()),
	}
	ent, err := body.ToEntity()
	if err != nil {
		t.Fatal(err)
	}
	// Bind at a non-marker path so chainIDFromMarkerPath rejects it.
	if _, err := ap.PutEntity("test/scratch/cfh-not-found", ent); err != nil {
		t.Fatalf("PutEntity: %v", err)
	}

	got, ok := inspect.ChainForHash(ap, ent.ContentHash)
	if ok {
		t.Errorf("ChainForHash(%s) = (%q, true); want false", ent.ContentHash, got)
	}
}

// TestBuildChainIndex_Composite: a marker + a suspended continuation,
// both attributable, both present in the batch index.
func TestBuildChainIndex_Composite(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	const markerChain = "idx-marker-chain"
	const suspendedChain = "idx-suspended-chain"

	mBody := coretypes.ChainErrorLostData{
		Reason:    "capability_denied",
		ChainID:   markerChain,
		StepIndex: "3",
		Timestamp: uint64(time.Now().UnixMicro()),
	}
	mEnt, err := mBody.ToEntity()
	if err != nil {
		t.Fatal(err)
	}
	markerPath := "system/runtime/chain-errors/lost/" + markerChain + "/3/capability_denied/" + mEnt.ContentHash.String()
	if _, err := ap.PutEntity(markerPath, mEnt); err != nil {
		t.Fatal(err)
	}

	sBody := coretypes.ContinuationSuspendedData{
		Target:      "system/continuation",
		Operation:   "advance",
		Reason:      "test",
		ChainID:     suspendedChain,
		SuspendedAt: uint64(time.Now().UnixMicro()),
	}
	sEnt, err := sBody.ToEntity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.PutEntity("system/continuation/suspended/sus-idx-1", sEnt); err != nil {
		t.Fatal(err)
	}

	idx := inspect.BuildChainIndex(ap)
	if idx[mEnt.ContentHash.String()] != markerChain {
		t.Errorf("marker entry: got %q want %q", idx[mEnt.ContentHash.String()], markerChain)
	}
	if idx[sEnt.ContentHash.String()] != suspendedChain {
		t.Errorf("suspended entry: got %q want %q", idx[sEnt.ContentHash.String()], suspendedChain)
	}
}
