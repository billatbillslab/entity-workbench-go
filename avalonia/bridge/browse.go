package main

// Browser panel bridge surface — BrowseOpen / BrowseRegisterWake /
// BrowsePin / BrowseNames / BrowseGo / BrowseBack / BrowseForward /
// BrowseRender / BrowseClose on the cgo envelope (D14).
//
// **Same two departures as verify.go, for the same reasons, and a third
// that is new.**
//
//  1. *No peer handle.* A Mode A2 consumer is not a peer (§6.5.3). The
//     browser derives every key it verifies against from a peer-id and
//     dispatches nothing.
//  2. *Operation-triggered wake.* Every other panel wakes on tree events
//     at a rate we do not control; this one wakes when a navigation
//     finishes, because a navigation is something an operator started.
//
//  3. NEW — **the single-flight guard is per-operation, not per-panel.**
//     `verify.go` refuses a second run outright: two verifications would
//     interleave two origins' steps into one list. Here, enumerating the
//     registry and navigating are *different* operations against
//     *different* state (the name list vs. the page + chain), and a user
//     who clicks a name while the list is still loading is doing
//     something reasonable. So each has its own guard and neither
//     blocks the other. What is still refused is a second navigation
//     while one is in flight — that one has the verify.go failure mode
//     exactly: two chains, one list, read as one journey.

/*
#include <stdlib.h>
#include <stdint.h>

static inline void invoke_tree_wake_browse(void* cb, int64_t handle) {
    if (cb != NULL) {
        ((void(*)(int64_t))cb)(handle);
    }
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"unsafe"

	"go.entitychurch.org/entity-core-go/core/types"

	wb "entity-workbench-go/workbench"
)

type browseHandle struct {
	model *wb.BrowseModel

	// ops counts COMPLETED async operations on this handle. It is on the
	// render envelope for two reasons: a panel can ignore a wake that
	// arrived for an operation it has already drawn, and a test can wait
	// for a specific completion instead of guessing from a button's
	// enabled state — which is a race, because the goroutine may not
	// have entered the operation by the time the first Render lands.
	ops int64

	mu        sync.Mutex
	navving   bool
	listing   bool
	wakeCb    unsafe.Pointer
	cancelNav context.CancelFunc
}

var (
	browseCounter int64
	browseMu      sync.Mutex
	browsers      = map[int64]*browseHandle{}
)

// browsePin is the C#-facing pin payload. Any pin_* field set means the
// layout is PINNED and discovery is skipped — the same all-or-nothing
// rule verify.go uses, so an operator who has learned one has learned
// both.
type browsePin struct {
	Origin string `json:"origin"`
	PeerID string `json:"peer_id"`

	PinTree     string `json:"pin_tree"`
	PinContent  string `json:"pin_content"`
	PinManifest string `json:"pin_manifest"`
	PinLayout   string `json:"pin_layout"`
	PinLeaf     string `json:"pin_leaf"`
	PinListing  string `json:"pin_listing"`

	// TargetOrigin is where a resolved binding's origin-relative URLs
	// are rooted, and where a peer-id address is fetched from. Empty
	// means "the registry's own origin", which is right for every
	// single-host deployment.
	TargetOrigin string `json:"target_origin"`
}

func (p browsePin) pinned() bool {
	return p.PinTree != "" || p.PinContent != "" || p.PinManifest != "" || p.PinLayout != ""
}

// BrowseOpen creates a browser panel handle.
//
//export BrowseOpen
func BrowseOpen() (result *C.char) {
	defer recoverToErrorEnvelope("BrowseOpen", &result)

	bh := &browseHandle{model: wb.NewBrowseModel(nil)}
	h := atomic.AddInt64(&browseCounter, 1)
	browseMu.Lock()
	browsers[h] = bh
	browseMu.Unlock()

	b, _ := json.Marshal(map[string]any{"ok": true, "handle": h})
	return C.CString(string(b))
}

func lookupBrowse(h int64) *browseHandle {
	browseMu.Lock()
	defer browseMu.Unlock()
	return browsers[h]
}

// BrowseRegisterWake registers the completion callback.
//
//export BrowseRegisterWake
func BrowseRegisterWake(handle C.int64_t, cb unsafe.Pointer) (result *C.char) {
	defer recoverToErrorEnvelope("BrowseRegisterWake", &result)

	bh := lookupBrowse(int64(handle))
	if bh == nil {
		return C.CString(`{"ok":false,"error":"unknown browse handle"}`)
	}
	bh.mu.Lock()
	bh.wakeCb = cb
	bh.mu.Unlock()
	return C.CString(`{"ok":true}`)
}

// BrowsePin sets the trust root. Synchronous: pinning does one optional
// profile fetch and no verification, so there is nothing worth a wake.
//
//export BrowsePin
func BrowsePin(handle C.int64_t, cJSON *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("BrowsePin", &result)

	bh := lookupBrowse(int64(handle))
	if bh == nil {
		return C.CString(`{"ok":false,"error":"unknown browse handle"}`)
	}
	var p browsePin
	if cJSON != nil {
		if err := json.Unmarshal([]byte(C.GoString(cJSON)), &p); err != nil {
			b, _ := json.Marshal(map[string]any{"ok": false, "error": "bad pin: " + err.Error()})
			return C.CString(string(b))
		}
	}
	bh.model.SetTargetOrigin(p.TargetOrigin)

	var err error
	if p.pinned() {
		leaf, listing := p.PinLeaf, p.PinListing
		if leaf == "" {
			leaf = ".bin"
		}
		if listing == "" {
			listing = ".list"
		}
		err = bh.model.PinRegistry(context.Background(), p.Origin, p.PeerID, &types.TransportEndpoint{
			TreeURLPrefix:     p.PinTree,
			ContentURLPrefix:  p.PinContent,
			ManifestURLPrefix: p.PinManifest,
			ContentLayout:     p.PinLayout,
			TreeLeafSuffix:    leaf,
			TreeListingSuffix: listing,
		})
	} else {
		err = bh.model.PinRegistry(context.Background(), p.Origin, p.PeerID, nil)
	}
	if err != nil {
		b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
		return C.CString(string(b))
	}
	return C.CString(`{"ok":true}`)
}

// BrowseNames enumerates the pinned registry — the §6a.3a walk. Async;
// wakes on completion.
//
//export BrowseNames
func BrowseNames(handle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("BrowseNames", &result)

	bh := lookupBrowse(int64(handle))
	if bh == nil {
		return C.CString(`{"ok":false,"error":"unknown browse handle"}`)
	}
	bh.mu.Lock()
	if bh.listing {
		bh.mu.Unlock()
		return C.CString(`{"ok":false,"error":"the registry is already being enumerated"}`)
	}
	bh.listing = true
	bh.mu.Unlock()

	go func() {
		defer func() { _ = recover() }()
		_ = bh.model.RefreshNames(context.Background())
		atomic.AddInt64(&bh.ops, 1)
		bh.mu.Lock()
		bh.listing = false
		cb := bh.wakeCb
		bh.mu.Unlock()
		if cb != nil {
			C.invoke_tree_wake_browse(cb, C.int64_t(handle))
		}
	}()
	return C.CString(`{"ok":true}`)
}

// BrowseGo navigates. Async; wakes on completion.
//
// A second navigation while one is in flight is refused rather than
// queued or cancelled — two chains interleaved into one step list would
// be read as one journey, which is verify.go's failure mode exactly.
//
//export BrowseGo
func BrowseGo(handle C.int64_t, cAddr *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("BrowseGo", &result)

	// **Copy the C string HERE, not inside the goroutine.**
	//
	// `cAddr` is a buffer the .NET marshaller allocated for the duration
	// of the P/Invoke and frees the moment this function returns. The
	// goroutine below outlives that by construction — the whole point of
	// this call is that it returns immediately — so a `C.GoString(cAddr)`
	// down there reads freed memory. It does not crash: it reads as an
	// EMPTY STRING, and an empty address parses as "an address needs at
	// least a name or a peer-id", which reports as a user error in a
	// panel the user typed an address into.
	//
	// Found by the headless panel tests on 2026-08-21, after the first
	// version did exactly this. Nothing about the Go side is unsafe on
	// its own; the lifetime that matters belongs to the caller and ends
	// at the return.
	addr := C.GoString(cAddr)
	return browseNavigate(handle, func(m *wb.BrowseModel, ctx context.Context) {
		_ = m.Open(ctx, addr)
	})
}

// BrowseBack / BrowseForward move through history, re-running the chain.
//
//export BrowseBack
func BrowseBack(handle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("BrowseBack", &result)
	return browseNavigate(handle, func(m *wb.BrowseModel, ctx context.Context) { _ = m.Back(ctx) })
}

// BrowseForward is Back's mirror.
//
//export BrowseForward
func BrowseForward(handle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("BrowseForward", &result)
	return browseNavigate(handle, func(m *wb.BrowseModel, ctx context.Context) { _ = m.Forward(ctx) })
}

func browseNavigate(handle C.int64_t, run func(*wb.BrowseModel, context.Context)) *C.char {
	bh := lookupBrowse(int64(handle))
	if bh == nil {
		return C.CString(`{"ok":false,"error":"unknown browse handle"}`)
	}
	bh.mu.Lock()
	if bh.navving {
		bh.mu.Unlock()
		return C.CString(`{"ok":false,"error":"a navigation is already in flight"}`)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bh.navving, bh.cancelNav = true, cancel
	bh.mu.Unlock()

	go func() {
		defer func() { _ = recover() }()
		run(bh.model, ctx)
		atomic.AddInt64(&bh.ops, 1)

		bh.mu.Lock()
		bh.navving, bh.cancelNav = false, nil
		cb := bh.wakeCb
		bh.mu.Unlock()
		if cb != nil {
			C.invoke_tree_wake_browse(cb, C.int64_t(handle))
		}
	}()
	return C.CString(`{"ok":true}`)
}

// BrowseRender returns the current view as JSON. Safe at any time,
// including mid-navigation — the result carries `Running`.
//
//export BrowseRender
func BrowseRender(handle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("BrowseRender", &result)

	bh := lookupBrowse(int64(handle))
	if bh == nil {
		return C.CString(`{"ok":false,"error":"unknown browse handle"}`)
	}
	b, err := json.Marshal(map[string]any{
		"ok":   true,
		"view": bh.model.Render(),
		"ops":  atomic.LoadInt64(&bh.ops),
	})
	if err != nil {
		return C.CString(`{"ok":false,"error":"marshal view failed"}`)
	}
	return C.CString(string(b))
}

// BrowseClose releases the handle and cancels any navigation in flight.
//
//export BrowseClose
func BrowseClose(handle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("BrowseClose", &result)

	h := int64(handle)
	browseMu.Lock()
	bh := browsers[h]
	delete(browsers, h)
	browseMu.Unlock()
	if bh == nil {
		return C.CString(`{"ok":true}`) // idempotent
	}
	bh.mu.Lock()
	bh.wakeCb = nil
	cancel := bh.cancelNav
	bh.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return C.CString(`{"ok":true}`)
}
