package main

// Peer-liveness bridge surface. Wraps workbench's PeerLivenessModel —
// the tree's lifecycle record (`system/peer/status`, EXTENSION-NETWORK
// §3.13) for every peer a transition has ever been written for.
//
// Deliberately NOT the same set as PeerConnections, which reports the
// connection POOL. A peer that dialed US, or one released an hour ago,
// appears here and not there; and only this surface can say `suspect`
// or say WHY a peer went away. A UI showing one and labelling it the
// other is the mistake this surface exists to make avoidable.
//
// `last_seen` is not exposed. The status entity is transition-written
// (§5.4.1 MUST) — a successful keepalive writes nothing — so the field
// is a snapshot taken at the transition, and any UI rendering it as
// "last heard from" would invent a freshness contract the protocol
// declines to offer. `connected_at` and `failing_since` are the two
// stamps that mean what they look like.
//
// Handle lifecycle parity with the other panels (TreeOpen, PeerInfo,
// PeerConnections, DiscoveryOpen): Open → handle, RegisterWake →
// wake-fanout goroutine, Render → snapshot, Close → tear down.
//
// **Why a handle and not a one-shot call.** The export this replaces
// built a fresh model, seeded it, rendered once and dropped it — which
// re-pays the O(N) seed on every refresh AND, worse, gives the caller
// no wake. A demotion to `suspect` writes `system/peer/status` and
// nothing else, so a panel refreshing on connection events alone would
// never see the one transition the pool snapshot cannot express. The
// long-lived model is prefix-subscribed (never scan-and-filter) and
// costs O(1) per transition.

/*
#include <stdlib.h>
#include <stdint.h>

// Local copy of invoke_tree_wake — see peer_connections.go for the same pattern.
static inline void invoke_tree_wake_liveness(void* cb, int64_t handle) {
    if (cb != NULL) {
        ((void(*)(int64_t))cb)(handle);
    }
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"entity-workbench-go/shellboot"
	wb "entity-workbench-go/workbench"
)

type livenessHandle struct {
	peerHandleID int64
	hp           *shellboot.HostedPeer
	model        *wb.PeerLivenessModel

	wakeCh     chan struct{}
	doneCh     chan struct{}
	wakeDoneCh chan struct{}
	cancelEv   func()
}

var (
	livenessCounter int64
	livenessMu      sync.Mutex
	livenesses      = map[int64]*livenessHandle{}
)

//export LivenessOpen
func LivenessOpen(peerHandle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("LivenessOpen", &result)
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	ch := &livenessHandle{
		peerHandleID: hp.Handle,
		hp:           hp,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	// Model first, wake subscription second. The model seeds then
	// subscribes internally, so a transition landing between the two
	// registrations is already IN the model — and the seed wake below
	// makes the panel read it. The reverse order would fire a wake at
	// a nil model.
	ch.model = wb.NewPeerLivenessModel(hp.AppPeer.Store())
	ch.cancelEv = hp.AppPeer.Store().OnPeerLivenessChange(func(_ wb.PeerLiveness) {
		select {
		case ch.wakeCh <- struct{}{}:
		default:
		}
	})
	h := atomic.AddInt64(&livenessCounter, 1)
	livenessMu.Lock()
	livenesses[h] = ch
	livenessMu.Unlock()
	// Seed wake so the panel paints the transitions already on disk
	// instead of an empty list until the next one arrives.
	select {
	case ch.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export LivenessRegisterWake
func LivenessRegisterWake(h C.int64_t, cb unsafe.Pointer) *C.char {
	handle := int64(h)
	livenessMu.Lock()
	ch, ok := livenesses[handle]
	livenessMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown liveness handle"}`)
	}
	ch.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(ch.wakeDoneCh)
		for {
			select {
			case <-ch.doneCh:
				return
			case <-ch.wakeCh:
				C.invoke_tree_wake_liveness(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

// LivenessRender returns the current lifecycle view: one row per peer a
// transition has ever been recorded for, plus the three counts a status
// line wants without re-walking the rows.
//
//export LivenessRender
func LivenessRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("LivenessRender", &result)
	handle := int64(h)
	livenessMu.Lock()
	ch, ok := livenesses[handle]
	livenessMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown liveness handle"}`)
	}
	b, err := json.Marshal(map[string]any{
		"ok":     true,
		"result": buildLivenessResult(ch.model.Render()),
	})
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(string(b))
}

// buildLivenessResult is the renderer-neutral output → JSON mapping.
// It adds no state of its own: every field is the model's, and a field
// the model declines to expose (LastSeen — see the file header) is not
// smuggled back in here.
func buildLivenessResult(out wb.PeerLivenessOutput) map[string]any {
	rows := make([]map[string]any, 0, len(out.Peers))
	for _, l := range out.Peers {
		rows = append(rows, map[string]any{
			"peer_id":       l.PeerID,
			"status":        l.Status,
			"reason":        l.Reason,
			"last_error":    l.LastError,
			"connected_at":  l.ConnectedAt,
			"failing_since": l.FailingSince,
			"failing":       l.Failing(),
		})
	}
	result := map[string]any{
		"peers":        rows,
		"connected":    out.Connected,
		"suspect":      out.Suspect,
		"disconnected": out.Disconnected,
	}
	// A seed failure is surfaced, not swallowed: an empty list from a
	// failed read and an empty list from a peer that has talked to
	// nobody look identical in a UI, and only one of them is fine.
	if out.SeedError != nil {
		result["seed_error"] = out.SeedError.Error()
	}
	return result
}

//export LivenessClose
func LivenessClose(h C.int64_t) {
	handle := int64(h)
	livenessMu.Lock()
	ch, ok := livenesses[handle]
	if ok {
		delete(livenesses, handle)
	}
	livenessMu.Unlock()
	if !ok {
		return
	}
	closeLivenessHandle(ch)
}

// cascadeLivenesses tears down every liveness handle tagged with peer h.
// Registered as an OnPeerDestroyed hook in BridgeInit.
func cascadeLivenesses(h int64) {
	livenessMu.Lock()
	victims := []*livenessHandle{}
	for id, ch := range livenesses {
		if ch.peerHandleID == h {
			victims = append(victims, ch)
			delete(livenesses, id)
		}
	}
	livenessMu.Unlock()
	for _, ch := range victims {
		closeLivenessHandle(ch)
	}
}

// closeLivenessHandle cancels the subscriptions and waits for the wake
// goroutine to exit before returning — the C# callback it invokes is
// freed by the caller's Dispose right after, so returning early is a
// use-after-free window.
func closeLivenessHandle(ch *livenessHandle) {
	if ch.cancelEv != nil {
		ch.cancelEv()
		ch.cancelEv = nil
	}
	if ch.model != nil {
		ch.model.Close()
	}
	close(ch.doneCh)
	if ch.wakeDoneCh != nil {
		<-ch.wakeDoneCh
	}
}
