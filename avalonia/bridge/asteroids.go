package main

// Asteroids game panel bridge surface — the display/input driver seam for the
// Asteroids hostable compute program (pg.AsteroidsGameModel). Adds
// AsteroidsOpen / AsteroidsRegisterWake / AsteroidsInput / AsteroidsStart /
// AsteroidsStop / AsteroidsRestart / AsteroidsRender / AsteroidsClose to the
// cgo envelope (D14).
//
// Mirrors snake.go — same handle map + wake-goroutine shape, same
// recoverToErrorEnvelope discipline, same cascade-on-peer-destroy story. The
// model owns the tick loop (the compute-program runtime); this file is only the
// marshaling seam.
//
// Two differences from Snake, and both are the point of this panel:
//
//   - INPUT is a held-key SET (a bitmask), not a single direction. Snake's port
//     is one last-write-wins value, which structurally cannot express "thrust +
//     rotate + fire at once". A bitmask keeps it a SNAPSHOT port — the panel
//     hands over the currently-held set each time it changes, and the step
//     samples it at the tick boundary. No new port kind was needed.
//   - RENDER returns a DISPLAY LIST (world-space quads + kind tags), not game
//     state. The C# side draws polylines and knows nothing about asteroids.
//     If this file ever needs to marshal a position or a radius, the
//     display-list port claim (COMPUTE-ASTEROIDS-PORT-TAXONOMY-2026-07-16 §4)
//     was wrong.

/*
#include <stdlib.h>
#include <stdint.h>

// invoke_tree_wake_asteroids — local copy of main.go's helper (cgo compiles
// each file's preamble into its own translation unit).
static inline void invoke_tree_wake_asteroids(void* cb, int64_t handle) {
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

// asteroidsHandle bundles an AsteroidsGameModel with the wake-coalescing
// goroutine channels. Tagged with peerHandleID for cascade.
type asteroidsHandle struct {
	peerHandleID int64
	model        *pg.AsteroidsGameModel
	cancelChange func()

	wakeCh     chan struct{}
	doneCh     chan struct{}
	wakeDoneCh chan struct{}
}

var (
	asteroidsCounter int64
	asteroidsMu      sync.Mutex
	asteroidsGames   = map[int64]*asteroidsHandle{}
)

// AsteroidsOpen seeds a fresh Asteroids program (state + input port + step +
// display expressions) under a per-handle tree root on the bound peer and
// returns `{"ok":true,"handle":N}`. The game starts stopped; the panel calls
// AsteroidsStart.
//
//export AsteroidsOpen
func AsteroidsOpen(peerHandle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("AsteroidsOpen", &result)

	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}

	h := atomic.AddInt64(&asteroidsCounter, 1)
	root := fmt.Sprintf("app/asteroids/ui-%d", h)
	model, err := pg.NewAsteroidsGameModel(hp.AppPeer, root, 0)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}

	ah := &asteroidsHandle{
		peerHandleID: hp.Handle,
		model:        model,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	// Model OnChange → wakeCh non-blocking; drop-on-full is the P3 single-flight
	// guard (the panel re-renders from the latest frame, coalesced ticks are
	// invisible).
	ah.cancelChange = model.OnChange(func() {
		select {
		case ah.wakeCh <- struct{}{}:
		default:
		}
	})

	asteroidsMu.Lock()
	asteroidsGames[h] = ah
	asteroidsMu.Unlock()
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

func asteroidsGet(h int64) *asteroidsHandle {
	asteroidsMu.Lock()
	defer asteroidsMu.Unlock()
	return asteroidsGames[h]
}

// AsteroidsRegisterWake registers the C# wake callback; the bridge-owned
// goroutine drains wakeCh and invokes it. C# keeps the delegate GCHandle-pinned
// for the lifetime of Go's use (P6).
//
//export AsteroidsRegisterWake
func AsteroidsRegisterWake(h C.int64_t, cb unsafe.Pointer) (result *C.char) {
	defer recoverToErrorEnvelope("AsteroidsRegisterWake", &result)
	ah := asteroidsGet(int64(h))
	if ah == nil {
		return C.CString(`{"ok":false,"error":"unknown asteroids handle"}`)
	}
	handle := int64(h)
	ah.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(ah.wakeDoneCh)
		for {
			select {
			case <-ah.doneCh:
				return
			case <-ah.wakeCh:
				C.invoke_tree_wake_asteroids(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

// AsteroidsInput writes the currently-held key SET to the program's snapshot
// input port as a bitmask (bit 0 = left, 1 = right, 2 = thrust, 3 = fire).
// Last-write-wins; sampled at the next tick boundary.
//
// The panel calls this on every keydown/keyup with the CURRENT set — it does not
// synthesize per-tick events. Held state is a snapshot, not a stream.
//
//export AsteroidsInput
func AsteroidsInput(h C.int64_t, keys C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("AsteroidsInput", &result)
	ah := asteroidsGet(int64(h))
	if ah == nil {
		return C.CString(`{"ok":false,"error":"unknown asteroids handle"}`)
	}
	k := int64(keys)
	if k < 0 || k > 15 {
		return C.CString(`{"ok":false,"error":"key bitmask out of range"}`)
	}
	if err := ah.model.Input(uint64(k)); err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(`{"ok":true}`)
}

// AsteroidsStart starts the clock-driven tick loop (idempotent).
//
//export AsteroidsStart
func AsteroidsStart(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("AsteroidsStart", &result)
	ah := asteroidsGet(int64(h))
	if ah == nil {
		return C.CString(`{"ok":false,"error":"unknown asteroids handle"}`)
	}
	ah.model.Start()
	return C.CString(`{"ok":true}`)
}

// AsteroidsStop pauses the tick loop (idempotent).
//
//export AsteroidsStop
func AsteroidsStop(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("AsteroidsStop", &result)
	ah := asteroidsGet(int64(h))
	if ah == nil {
		return C.CString(`{"ok":false,"error":"unknown asteroids handle"}`)
	}
	ah.model.Stop()
	return C.CString(`{"ok":true}`)
}

// AsteroidsRestart reseeds state₀ (fresh asteroid layout) and starts the loop.
//
//export AsteroidsRestart
func AsteroidsRestart(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("AsteroidsRestart", &result)
	ah := asteroidsGet(int64(h))
	if ah == nil {
		return C.CString(`{"ok":false,"error":"unknown asteroids handle"}`)
	}
	if err := ah.model.Restart(); err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(`{"ok":true}`)
}

// AsteroidsRender returns the current AsteroidsGameOutput frame as JSON — a
// display list: live quads (4 world-space vertices + a kind tag) and the
// scoreboard scalars. No actor state crosses this seam.
//
//export AsteroidsRender
func AsteroidsRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("AsteroidsRender", &result)
	ah := asteroidsGet(int64(h))
	if ah == nil {
		return C.CString(`{"ok":false,"error":"unknown asteroids handle"}`)
	}
	out := ah.model.Render()
	b, err := json.Marshal(out)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

// AsteroidsClose stops the game loop and tears down the handle. Joins the wake
// goroutine before returning so C# can Free the pinned delegate immediately
// after (P6 release-order discipline).
//
//export AsteroidsClose
func AsteroidsClose(h C.int64_t) {
	handle := int64(h)
	asteroidsMu.Lock()
	ah, ok := asteroidsGames[handle]
	if ok {
		delete(asteroidsGames, handle)
	}
	asteroidsMu.Unlock()
	if !ok {
		return
	}
	teardownAsteroids(ah)
}

func teardownAsteroids(ah *asteroidsHandle) {
	if ah.cancelChange != nil {
		ah.cancelChange()
		ah.cancelChange = nil
	}
	ah.model.Close()
	close(ah.doneCh)
	if ah.wakeDoneCh != nil {
		<-ah.wakeDoneCh
	}
}

// cascadeAsteroids tears down every asteroids handle tagged with peer h.
// Registered in BridgeInit's OnPeerDestroyed chain.
func cascadeAsteroids(h int64) {
	asteroidsMu.Lock()
	victims := []*asteroidsHandle{}
	for id, ah := range asteroidsGames {
		if ah.peerHandleID == h {
			victims = append(victims, ah)
			delete(asteroidsGames, id)
		}
	}
	asteroidsMu.Unlock()
	for _, ah := range victims {
		teardownAsteroids(ah)
	}
}
