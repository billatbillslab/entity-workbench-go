package inspect_test

import (
	"testing"
	"time"

	coretypes "go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/inspect"
)

// TestTraceChain_ChainErrorMarker: binds a synthetic chain-error-lost
// marker at the v1.20 path layout and verifies TraceChain enumerates
// it with the path-derived segments decoded.
func TestTraceChain_ChainErrorMarker(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	const chainID = "chain-trace-test-001"
	body := coretypes.ChainErrorLostData{
		Code:      "base_not_a_version",
		Status:    400,
		TargetURI: "entity://peer/system/inbox/follow/foo/bar/merge",
		Timestamp: uint64(time.Now().UnixMicro()),
		Reason:    "base_not_a_version",
		ChainID:   chainID,
		StepIndex: "5",
	}
	ent, err := body.ToEntity()
	if err != nil {
		t.Fatal(err)
	}

	markerPath := "system/runtime/chain-errors/lost/" + chainID + "/5/base_not_a_version/" + ent.ContentHash.String()
	if _, err := ap.PutEntity(markerPath, ent); err != nil {
		t.Fatalf("PutEntity: %v", err)
	}

	trace := inspect.TraceChain(ap, chainID)

	if trace.ChainID != chainID {
		t.Errorf("ChainID = %q want %q", trace.ChainID, chainID)
	}
	if len(trace.Errors) != 1 {
		t.Fatalf("Errors = %d want 1; summary: %s", len(trace.Errors), trace.Summary())
	}
	m := trace.Errors[0]
	if m.Kind != "lost" {
		t.Errorf("Kind = %q want lost", m.Kind)
	}
	if m.ChainID != chainID {
		t.Errorf("Marker ChainID = %q want %q", m.ChainID, chainID)
	}
	if m.StepIndex != "5" {
		t.Errorf("StepIndex = %q want 5", m.StepIndex)
	}
	if m.Reason != "base_not_a_version" {
		t.Errorf("Reason = %q want base_not_a_version", m.Reason)
	}
	if m.Body.Status != 400 {
		t.Errorf("Body.Status = %d want 400", m.Body.Status)
	}
	if m.Body.Code != "base_not_a_version" {
		t.Errorf("Body.Code = %q want base_not_a_version", m.Body.Code)
	}

	// Path-binding side: same path captured.
	if len(trace.PathBindings) < 1 {
		t.Errorf("PathBindings empty; expected at least the chain-error binding")
	}

	t.Logf("trace summary: %s", trace.Summary())
}

// TestTraceChain_NoMatch: chains that didn't fail leave no markers;
// trace returns an empty surface, not nil.
func TestTraceChain_NoMatch(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	trace := inspect.TraceChain(ap, "nonexistent-chain")
	if trace == nil {
		t.Fatal("trace is nil")
	}
	if len(trace.Errors) != 0 {
		t.Errorf("Errors = %d want 0", len(trace.Errors))
	}
	if len(trace.PathBindings) != 0 {
		t.Errorf("PathBindings = %d want 0", len(trace.PathBindings))
	}
}
