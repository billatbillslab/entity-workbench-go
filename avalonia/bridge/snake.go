package main

// Snake game panel bridge surface — the display/input driver seam for
// the Snake hostable compute program (pg.SnakeGameModel). Adds
// SnakeOpen / SnakeRegisterWake / SnakeInput / SnakeStart / SnakeStop /
// SnakeRestart / SnakeRender / SnakeClose to the cgo envelope (D14).
//
// Mirrors the Site surface (site.go) — same handle map + wake-goroutine
// shape, same recoverToErrorEnvelope discipline, same cascade-on-peer-
// destroy story. The model owns the tick loop (the compute-program
// runtime); this file is only the marshaling seam. The C# panel is the
// console-tier driver: it reads the output port (Render) and writes the
// input port (Input) — nothing else.

/*
#include <stdlib.h>
#include <stdint.h>

// invoke_tree_wake_snake — local copy of main.go's helper (cgo compiles
// each file's preamble into its own translation unit).
static inline void invoke_tree_wake_snake(void* cb, int64_t handle) {
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

// snakeHandle bundles a SnakeGameModel with the wake-coalescing
// goroutine channels. Tagged with peerHandleID for cascade.
type snakeHandle struct {
	peerHandleID int64
	model        *pg.SnakeGameModel
	cancelChange func()

	wakeCh     chan struct{}
	doneCh     chan struct{}
	wakeDoneCh chan struct{}
}

var (
	snakeCounter int64
	snakeMu      sync.Mutex
	snakes       = map[int64]*snakeHandle{}
)

// SnakeOpen seeds a fresh Snake program (state + input port + step
// expression) under a per-handle tree root on the bound peer and
// returns `{"ok":true,"handle":N}`. The game starts stopped; the panel
// calls SnakeStart.
//
//export SnakeOpen
func SnakeOpen(peerHandle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("SnakeOpen", &result)

	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}

	h := atomic.AddInt64(&snakeCounter, 1)
	root := fmt.Sprintf("app/snake/ui-%d", h)
	model, err := pg.NewSnakeGameModel(hp.AppPeer, root, 0)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}

	sh := &snakeHandle{
		peerHandleID: hp.Handle,
		model:        model,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	// Model OnChange → wakeCh non-blocking; drop-on-full is the P3
	// single-flight guard (the panel re-renders from the latest frame,
	// coalesced ticks are invisible).
	sh.cancelChange = model.OnChange(func() {
		select {
		case sh.wakeCh <- struct{}{}:
		default:
		}
	})

	snakeMu.Lock()
	snakes[h] = sh
	snakeMu.Unlock()
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

func snakeGet(h int64) *snakeHandle {
	snakeMu.Lock()
	defer snakeMu.Unlock()
	return snakes[h]
}

// SnakeRegisterWake registers the C# wake callback; the bridge-owned
// goroutine drains wakeCh and invokes it. C# keeps the delegate
// GCHandle-pinned for the lifetime of Go's use (P6).
//
//export SnakeRegisterWake
func SnakeRegisterWake(h C.int64_t, cb unsafe.Pointer) (result *C.char) {
	defer recoverToErrorEnvelope("SnakeRegisterWake", &result)
	sh := snakeGet(int64(h))
	if sh == nil {
		return C.CString(`{"ok":false,"error":"unknown snake handle"}`)
	}
	handle := int64(h)
	sh.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(sh.wakeDoneCh)
		for {
			select {
			case <-sh.doneCh:
				return
			case <-sh.wakeCh:
				C.invoke_tree_wake_snake(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

// SnakeInput writes a direction (0=up 1=right 2=down 3=left) to the
// program's snapshot input port. Last-write-wins; sampled at the next
// tick boundary.
//
//export SnakeInput
func SnakeInput(h C.int64_t, dir C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("SnakeInput", &result)
	sh := snakeGet(int64(h))
	if sh == nil {
		return C.CString(`{"ok":false,"error":"unknown snake handle"}`)
	}
	d := int64(dir)
	if d < 0 || d > 3 {
		return C.CString(`{"ok":false,"error":"direction out of range"}`)
	}
	if err := sh.model.Input(uint64(d)); err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(`{"ok":true}`)
}

// SnakeStart starts the clock-driven tick loop (idempotent).
//
//export SnakeStart
func SnakeStart(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("SnakeStart", &result)
	sh := snakeGet(int64(h))
	if sh == nil {
		return C.CString(`{"ok":false,"error":"unknown snake handle"}`)
	}
	sh.model.Start()
	return C.CString(`{"ok":true}`)
}

// SnakeStop pauses the tick loop (idempotent).
//
//export SnakeStop
func SnakeStop(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("SnakeStop", &result)
	sh := snakeGet(int64(h))
	if sh == nil {
		return C.CString(`{"ok":false,"error":"unknown snake handle"}`)
	}
	sh.model.Stop()
	return C.CString(`{"ok":true}`)
}

// SnakeRestart reseeds state₀ (fresh food sequence) and starts the loop.
//
//export SnakeRestart
func SnakeRestart(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("SnakeRestart", &result)
	sh := snakeGet(int64(h))
	if sh == nil {
		return C.CString(`{"ok":false,"error":"unknown snake handle"}`)
	}
	if err := sh.model.Restart(); err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(`{"ok":true}`)
}

// SnakeRender returns the current SnakeGameOutput frame as JSON.
//
//export SnakeRender
func SnakeRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("SnakeRender", &result)
	sh := snakeGet(int64(h))
	if sh == nil {
		return C.CString(`{"ok":false,"error":"unknown snake handle"}`)
	}
	out := sh.model.Render()
	b, err := json.Marshal(out)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

// SnakeClose stops the game loop and tears down the handle. Joins the
// wake goroutine before returning so C# can Free the pinned delegate
// immediately after (P6 release-order discipline).
//
//export SnakeClose
func SnakeClose(h C.int64_t) {
	handle := int64(h)
	snakeMu.Lock()
	sh, ok := snakes[handle]
	if ok {
		delete(snakes, handle)
	}
	snakeMu.Unlock()
	if !ok {
		return
	}
	teardownSnake(sh)
}

func teardownSnake(sh *snakeHandle) {
	if sh.cancelChange != nil {
		sh.cancelChange()
		sh.cancelChange = nil
	}
	sh.model.Close()
	close(sh.doneCh)
	if sh.wakeDoneCh != nil {
		<-sh.wakeDoneCh
	}
}

// cascadeSnakes tears down every snake handle tagged with peer h.
// Registered in BridgeInit's OnPeerDestroyed chain.
func cascadeSnakes(h int64) {
	snakeMu.Lock()
	victims := []*snakeHandle{}
	for id, sh := range snakes {
		if sh.peerHandleID == h {
			victims = append(victims, sh)
			delete(snakes, id)
		}
	}
	snakeMu.Unlock()
	for _, sh := range victims {
		teardownSnake(sh)
	}
}
