package inspect_test

// End-to-end validation of EXTENSION-SUBSCRIPTION §4.7 marker emission
// observed from L2 (inspect) seat. Triggers the rate_limited path on a
// self-subscribing peer and confirms the marker appears at the v1.20
// path with the subscription_id substituted for step_index per §4.7.
//
// Validates core-go's bindLostMarker work (ext/subscription/
// chain_error_lost.go) end-to-end through entitysdk's peer abstraction.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	coretypes "go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/inspect"
)

// TestSubscription47_RateLimitedMarkerEmitted: single-peer self-loop
// with a tight rate_limit. Second emit in quick succession trips the
// limit; engine must bind a `lost` marker per §4.7 (reason=rate_limited)
// at the canonical path. Validates the END-TO-END flow from put →
// subscription match → rate-limit decision → marker bind → location
// index visible.
func TestSubscription47_RateLimitedMarkerEmitted(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	// rate_limit=60/min → ≥1s between deliveries.
	// Second event within 1s ⇒ rate-limited ⇒ §4.7 marker.
	rateLimit := uint64(60)
	sub, err := ap.SubscribeAt(ap.PeerID(), "rl-probe/*", entitysdk.SubscribeOpts{
		Limits: &coretypes.SubscriptionLimitsData{RateLimit: &rateLimit},
	})
	if err != nil {
		t.Fatalf("SubscribeAt: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// Fire two puts in rapid succession. First passes; second is
	// rate-limited.
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("rl-probe/note-%d", i)
		if _, err := ap.Put(path, "test/note", map[string]interface{}{"i": i}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond) // tight enough to trip rate limit
	}

	// Poll for marker — engine binds asynchronously after delivery
	// rejection. 2s budget.
	var markers []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries := inspect.FindChainErrors(ap)
		for _, e := range entries {
			if strings.Contains(e.Path, "/rate_limited/") {
				markers = append(markers, e.Path)
			}
		}
		if len(markers) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(markers) == 0 {
		all := inspect.FindChainErrors(ap)
		t.Fatalf("expected ≥1 rate_limited marker; found 0. all chain-error entries: %d", len(all))
	}

	// Verify path shape: .../system/runtime/chain-errors/lost/{chain_id}/{sub_id}/rate_limited/{hash}
	got := markers[0]
	t.Logf("marker path: %s", got)

	// Path parts after "system/runtime/chain-errors/lost/":
	idx := strings.Index(got, "system/runtime/chain-errors/lost/")
	if idx < 0 {
		t.Fatalf("marker path missing canonical prefix: %s", got)
	}
	tail := got[idx+len("system/runtime/chain-errors/lost/"):]
	parts := strings.Split(tail, "/")
	if len(parts) != 4 {
		t.Fatalf("expected 4 path parts after canonical prefix, got %d: %v", len(parts), parts)
	}

	chainID, subID, reason, markerHash := parts[0], parts[1], parts[2], parts[3]
	t.Logf("decoded: chain_id=%s sub_id=%s reason=%s marker_hash=%s...", chainID, subID, reason, markerHash[:16])

	if reason != "rate_limited" {
		t.Errorf("reason = %q want rate_limited", reason)
	}
	if subID == "" {
		t.Errorf("subscription_id empty — §4.7 substitution for step_index broken")
	}
	if chainID == "" {
		t.Errorf("chain_id empty — should be 'none' fallback per core-go impl")
	}
	if len(markerHash) < 60 {
		t.Errorf("marker_hash too short (%d) — invariant-pointer hex form expected", len(markerHash))
	}

	// Use ChainTrace to walk the marker and verify it decodes correctly.
	trace := inspect.TraceChain(ap, chainID)
	if len(trace.Errors) == 0 {
		t.Fatalf("ChainTrace found no errors for chain_id=%q", chainID)
	}
	found := false
	for _, m := range trace.Errors {
		if m.Reason == "rate_limited" {
			found = true
			if m.Body.Code == "" && m.Body.Status == 0 {
				t.Logf("marker body: code=%q status=%d (engine emits empty for suppression — expected)",
					m.Body.Code, m.Body.Status)
			}
		}
	}
	if !found {
		t.Errorf("ChainTrace did not decode a rate_limited marker")
	}
}
