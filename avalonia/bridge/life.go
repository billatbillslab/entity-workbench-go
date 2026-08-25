package main

// Life game panel bridge surface — the display driver seam for the
// Conway's Life hostable compute program (pg.LifeGameModel). Adds
// LifeOpen / LifeRegisterWake / LifeStart / LifeStop / LifeRestart /
// LifeRender / LifeClose to the cgo envelope (D14).
//
// Mirrors snake.go exactly, minus the input port: Life is a closed
// program (state₀ + step is the whole thing), so there is no LifeInput.
// That absence is the point — the input-port machinery is a per-program
// property, not a runtime tax; the rest of the seam is unchanged.
//
// Same handle map + wake-goroutine shape, same recoverToErrorEnvelope
// discipline, same cascade-on-peer-destroy story. The model owns the
// tick loop (the compute-program runtime); this file is only the
// marshaling seam. The C# panel is the console-tier driver: it reads
// the output port (Render) and nothing else.

/*
#include <stdlib.h>
#include <stdint.h>

// invoke_tree_wake_life — local copy of main.go's helper (cgo compiles
// each file's preamble into its own translation unit).
static inline void invoke_tree_wake_life(void* cb, int64_t handle) {
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

	pg "entity-workbench-go/programs"
)

// lifeHandle bundles a LifeGameModel with the wake-coalescing goroutine
// channels. Tagged with peerHandleID for cascade.
type lifeHandle struct {
	peerHandleID int64
	model        *pg.LifeGameModel
	cancelChange func()

	wakeCh     chan struct{}
	doneCh     chan struct{}
	wakeDoneCh chan struct{}
}

var (
	lifeCounter int64
	lifeMu      sync.Mutex
	lives       = map[int64]*lifeHandle{}
)

// LifeOpen seeds a fresh Life program (state₀ + step expression) under a
// per-handle tree root on the bound peer and returns
// `{"ok":true,"handle":N}`. The board starts stopped; the panel calls
// LifeStart.
//
//export LifeOpen
func LifeOpen(peerHandle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("LifeOpen", &result)

	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}

	h := atomic.AddInt64(&lifeCounter, 1)
	root := fmt.Sprintf("app/life/ui-%d", h)
	model, err := pg.NewLifeGameModel(hp.AppPeer, root, 0)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}

	lh := &lifeHandle{
		peerHandleID: hp.Handle,
		model:        model,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	// Model OnChange → wakeCh non-blocking; drop-on-full is the P3
	// single-flight guard (the panel re-renders from the latest frame,
	// coalesced ticks are invisible — safe precisely because the output
	// port is a snapshot, not a stream).
	lh.cancelChange = model.OnChange(func() {
		select {
		case lh.wakeCh <- struct{}{}:
		default:
		}
	})

	lifeMu.Lock()
	lives[h] = lh
	lifeMu.Unlock()
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

func lifeGet(h int64) *lifeHandle {
	lifeMu.Lock()
	defer lifeMu.Unlock()
	return lives[h]
}

// LifeRegisterWake registers the C# wake callback; the bridge-owned
// goroutine drains wakeCh and invokes it. C# keeps the delegate
// GCHandle-pinned for the lifetime of Go's use (P6).
//
//export LifeRegisterWake
func LifeRegisterWake(h C.int64_t, cb unsafe.Pointer) (result *C.char) {
	defer recoverToErrorEnvelope("LifeRegisterWake", &result)
	lh := lifeGet(int64(h))
	if lh == nil {
		return C.CString(`{"ok":false,"error":"unknown life handle"}`)
	}
	handle := int64(h)
	lh.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(lh.wakeDoneCh)
		for {
			select {
			case <-lh.doneCh:
				return
			case <-lh.wakeCh:
				C.invoke_tree_wake_life(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

// LifeStart starts the clock-driven tick loop (idempotent).
//
//export LifeStart
func LifeStart(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("LifeStart", &result)
	lh := lifeGet(int64(h))
	if lh == nil {
		return C.CString(`{"ok":false,"error":"unknown life handle"}`)
	}
	lh.model.Start()
	return C.CString(`{"ok":true}`)
}

// LifeStop pauses the tick loop (idempotent).
//
//export LifeStop
func LifeStop(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("LifeStop", &result)
	lh := lifeGet(int64(h))
	if lh == nil {
		return C.CString(`{"ok":false,"error":"unknown life handle"}`)
	}
	lh.model.Stop()
	return C.CString(`{"ok":true}`)
}

// LifeRestart reseeds state₀ (fresh soup) and starts the loop.
//
//export LifeRestart
func LifeRestart(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("LifeRestart", &result)
	lh := lifeGet(int64(h))
	if lh == nil {
		return C.CString(`{"ok":false,"error":"unknown life handle"}`)
	}
	if err := lh.model.Restart(); err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(`{"ok":true}`)
}

// LifeRender returns the current LifeGameOutput frame as JSON.
//
//export LifeRender
func LifeRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("LifeRender", &result)
	lh := lifeGet(int64(h))
	if lh == nil {
		return C.CString(`{"ok":false,"error":"unknown life handle"}`)
	}
	out := lh.model.Render()
	b, err := json.Marshal(out)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

// LifeClose stops the loop and tears down the handle. Joins the wake
// goroutine before returning so C# can Free the pinned delegate
// immediately after (P6 release-order discipline).
//
//export LifeClose
func LifeClose(h C.int64_t) {
	handle := int64(h)
	lifeMu.Lock()
	lh, ok := lives[handle]
	if ok {
		delete(lives, handle)
	}
	lifeMu.Unlock()
	if !ok {
		return
	}
	teardownLife(lh)
}

func teardownLife(lh *lifeHandle) {
	if lh.cancelChange != nil {
		lh.cancelChange()
		lh.cancelChange = nil
	}
	lh.model.Close()
	close(lh.doneCh)
	if lh.wakeDoneCh != nil {
		<-lh.wakeDoneCh
	}
}

// cascadeLives tears down every life handle tagged with peer h.
// Registered in BridgeInit's OnPeerDestroyed chain.
func cascadeLives(h int64) {
	lifeMu.Lock()
	victims := []*lifeHandle{}
	for id, lh := range lives {
		if lh.peerHandleID == h {
			victims = append(victims, lh)
			delete(lives, id)
		}
	}
	lifeMu.Unlock()
	for _, lh := range victims {
		teardownLife(lh)
	}
}
