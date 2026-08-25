package main

// GENERIC HOST BRIDGE — the cgo seam for pg.Host (mount any compute program
// from its descriptor).
//
// This file replaces what life.go + snake.go + asteroids.go do, with ONE seam
// instead of three. That is the whole point of the rung: those three files are
// near-identical — same handle map, same wake goroutine, same Open/Start/Stop/
// Restart/Render/Close — differing only in which program-specific model they
// name. The descriptor removes the reason for the difference.
//
// ─── What is per-program here, and why that is correct ─────────────────────
//
// Exactly one thing: `authors`, the name → Author dispatch. Authoring IS
// per-program (it is how a program gets written); the falsifiable claim is that
// the HOST has no per-program code, and pg.Host is what carries it. Once
// ProgramAuthor has run, nothing below this line knows what a Life is.
//
// The split is visible in the API on purpose:
//
//	ProgramAuthor(peer, name) → descriptorPath     ; per-program, runs once
//	ProgramMount(peer, descriptorPath) → handle    ; generic, runs always
//
// A front-end that already has a descriptor (e.g. one fetched from another peer
// by hash — phase 2) calls ProgramMount alone and never touches ProgramAuthor.
// That is the transfer story in one function signature.
//
// ─── Render marshals BY SHAPE ──────────────────────────────────────────────
//
// The host hands out PortValue{Shape, Scene, Data} with Data still CBOR — it
// does not decode program bytes. This seam decodes by SHAPE (never by program)
// and emits JSON for C#. That is what a driver is: shape-aware, program-blind.
// Add a shape → add a case here + a view in C#; touch nothing else.

/*
#include <stdlib.h>
#include <stdint.h>

// invoke_tree_wake_program — local copy of main.go's helper (cgo compiles each
// file's preamble into its own translation unit).
static inline void invoke_tree_wake_program(void* cb, int64_t handle) {
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

	"entity-workbench-go/entitysdk"
	pg "entity-workbench-go/programs"
)

// programAuthorDispatch is the ONLY per-program branch in this file, and it is
// the authoring half — correct by definition (authoring is how a program gets
// written). Everything below it is generic: once this returns a descriptor path,
// nothing in this file knows what a Life is.
//
// A fixed seed per program so a panel restart reproduces the same run; the
// legacy models pass 0 (clock-derived), which is why two Life panels never
// showed the same board.
func programAuthorDispatch(name string, ap *entitysdk.AppPeer, root string) (string, error) {
	switch name {
	case "life":
		return pg.AuthorLife(ap, root, 0x5eed1)
	case "snake":
		return pg.AuthorSnake(ap, root, 0x5eed2)
	case "asteroids":
		return pg.AuthorAsteroids(ap, root, 0x5eed3)
	case "life-big":
		// The sharded floor, end to end to a screen: 64×64 arith Life — ~16× past
		// the ~24×24 single-eval budget cliff, so it CANNOT mount unsharded. It
		// mounts here only because the host runs the static-k host-managed shard
		// family (k=8, program-owned gather stitch). Same ProgramPanel, same shape
		// driver (text) — the panel never learns it is sharded; the descriptor's
		// shard block is the host's concern alone.
		return pg.AuthorLifeSharded(ap, root, 0x5eed4, 64, 64, 8)
	default:
		return "", fmt.Errorf("unknown program %q (have: life, snake, asteroids, life-big)", name)
	}
}

// programHandle bundles a mounted Host with the wake-coalescing goroutine
// channels. Tagged with peerHandleID for cascade.
type programHandle struct {
	peerHandleID int64
	host         *pg.Host
	cancelChange func()

	wakeCh     chan struct{}
	doneCh     chan struct{}
	wakeDoneCh chan struct{}
}

var (
	programCounter int64
	programMu      sync.Mutex
	programs       = map[int64]*programHandle{}
)

// ProgramAuthor writes a program into the bound peer's tree and returns
// `{"ok":true,"descriptor":"<path>"}`. Per-program, runs once per root.
//
//export ProgramAuthor
func ProgramAuthor(peerHandle C.int64_t, name *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("ProgramAuthor", &result)

	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	prog := C.GoString(name)

	// A per-handle root so two panels on one peer don't share live state.
	h := atomic.AddInt64(&programCounter, 1)
	root := fmt.Sprintf("app/%s/ui-%d", prog, h)

	descPath, err := programAuthorDispatch(prog, hp.AppPeer, root)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"descriptor":%q}`, descPath))
}

// ProgramMount mounts a descriptor and returns `{"ok":true,"handle":N}`. This
// is the generic entry point — it knows nothing about what it is mounting.
// The program starts stopped; the panel calls ProgramStart.
//
//export ProgramMount
func ProgramMount(peerHandle C.int64_t, descriptorPath *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("ProgramMount", &result)

	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}

	host, err := pg.Mount(hp.AppPeer, C.GoString(descriptorPath))
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}

	h := atomic.AddInt64(&programCounter, 1)
	ph := &programHandle{
		peerHandleID: hp.Handle,
		host:         host,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	// Host OnChange → wakeCh non-blocking; drop-on-full is the P3 single-flight
	// guard. Safe precisely because every output port is a SNAPSHOT: the panel
	// re-renders from the latest frame and coalesced ticks are invisible. A
	// stream port would need a different rule — the two-kind split (proposal §3)
	// seen from the renderer side.
	ph.cancelChange = host.OnChange(func() {
		select {
		case ph.wakeCh <- struct{}{}:
		default:
		}
	})

	programMu.Lock()
	programs[h] = ph
	programMu.Unlock()
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

func programGet(h int64) *programHandle {
	programMu.Lock()
	defer programMu.Unlock()
	return programs[h]
}

// ProgramRegisterWake pins a C# callback invoked (coalesced) on every host
// change. Mirrors life.go's wake discipline exactly.
//
//export ProgramRegisterWake
func ProgramRegisterWake(h C.int64_t, cb unsafe.Pointer) (result *C.char) {
	defer recoverToErrorEnvelope("ProgramRegisterWake", &result)
	ph := programGet(int64(h))
	if ph == nil {
		return C.CString(`{"ok":false,"error":"unknown program handle"}`)
	}
	programMu.Lock()
	if ph.wakeDoneCh != nil {
		programMu.Unlock()
		return C.CString(`{"ok":false,"error":"wake already registered"}`)
	}
	done := make(chan struct{})
	ph.wakeDoneCh = done
	programMu.Unlock()

	handle := int64(h)
	go func() {
		defer close(done)
		for {
			select {
			case <-ph.doneCh:
				return
			case <-ph.wakeCh:
				C.invoke_tree_wake_program(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

// ProgramStart clocks the tick.
//
//export ProgramStart
func ProgramStart(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("ProgramStart", &result)
	ph := programGet(int64(h))
	if ph == nil {
		return C.CString(`{"ok":false,"error":"unknown program handle"}`)
	}
	ph.host.Start()
	return C.CString(`{"ok":true}`)
}

// ProgramStop halts the tick.
//
//export ProgramStop
func ProgramStop(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("ProgramStop", &result)
	ph := programGet(int64(h))
	if ph == nil {
		return C.CString(`{"ok":false,"error":"unknown program handle"}`)
	}
	ph.host.Stop()
	return C.CString(`{"ok":true}`)
}

// ProgramRestart reseeds from the descriptor's initial_state and leaves the
// program stopped. Run-state is the host's surface, outside the program.
//
//export ProgramRestart
func ProgramRestart(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("ProgramRestart", &result)
	ph := programGet(int64(h))
	if ph == nil {
		return C.CString(`{"ok":false,"error":"unknown program handle"}`)
	}
	if err := ph.host.Restart(); err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(`{"ok":true}`)
}

// ProgramInputKeys writes a held-key bitmask to a `key-set` input port.
//
//export ProgramInputKeys
func ProgramInputKeys(h C.int64_t, portName *C.char, keys C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("ProgramInputKeys", &result)
	ph := programGet(int64(h))
	if ph == nil {
		return C.CString(`{"ok":false,"error":"unknown program handle"}`)
	}
	typ, data, err := pg.EncodeKeySet(uint64(keys))
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	if err := ph.host.Input(C.GoString(portName), typ, data); err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(`{"ok":true}`)
}

// ProgramInputDirection writes a latched direction to a `direction` input port.
//
//export ProgramInputDirection
func ProgramInputDirection(h C.int64_t, portName *C.char, dir C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("ProgramInputDirection", &result)
	ph := programGet(int64(h))
	if ph == nil {
		return C.CString(`{"ok":false,"error":"unknown program handle"}`)
	}
	typ, data, err := pg.EncodeDirection(uint64(dir))
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	if err := ph.host.Input(C.GoString(portName), typ, data); err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(`{"ok":true}`)
}

// portDTO is one materialized output port, marshaled BY SHAPE.
//
// Exactly one of Text / DisplayList is populated, per Shape. A new shape adds a
// field here and a view in C#; the host, the descriptor and every existing
// program are untouched. That is the registry-extended-by-a-rule property, seen
// from the driver side.
type portDTO struct {
	Name  string                 `json:"name"`
	Shape string                 `json:"shape"`
	Scene map[string]interface{} `json:"scene,omitempty"`

	Text        *pg.TextFrame   `json:"text,omitempty"`
	DisplayList *pg.DisplayList `json:"displayList,omitempty"`
}

// inputDTO is a declared input port. The frame carries these so a driver can
// bind keys WITHOUT knowing the program: it binds on (shape, scene.keymap).
//
// For `key-set`, scene.keymap maps bit → semantic action ("thrust", "fire").
// The driver holds a generic action → physical-key table, so it learns that bit
// 2 is thrust from the descriptor and never learns that thrust belongs to a
// ship. That is what makes the input driver blind rather than merely small.
type inputDTO struct {
	Name  string                 `json:"name"`
	Shape string                 `json:"shape"`
	Scene map[string]interface{} `json:"scene,omitempty"`
}

type frameDTO struct {
	Ports   map[string]portDTO `json:"ports"`
	Inputs  []inputDTO         `json:"inputs,omitempty"`
	Status  int                `json:"status"`
	Ticks   uint64             `json:"ticks"`
	Running bool               `json:"running"`
	Err     string             `json:"err,omitempty"`
}

// ProgramRender returns the current frame: every output port, decoded by shape.
//
//export ProgramRender
func ProgramRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("ProgramRender", &result)
	ph := programGet(int64(h))
	if ph == nil {
		return C.CString(`{"ok":false,"error":"unknown program handle"}`)
	}
	frame := ph.host.Render()

	dto := frameDTO{
		Ports:   make(map[string]portDTO, len(frame.Ports)),
		Status:  frame.Status,
		Ticks:   frame.Ticks,
		Running: frame.Running,
		Err:     frame.Err,
	}
	for _, ip := range ph.host.Descriptor().InputPorts {
		dto.Inputs = append(dto.Inputs, inputDTO{Name: ip.Name, Shape: ip.Shape, Scene: ip.Scene})
	}
	for name, pv := range frame.Ports {
		p := portDTO{Name: pv.Name, Shape: pv.Shape, Scene: pv.Scene}
		switch pv.Shape {
		case pg.ShapeText:
			tf, err := pg.DecodeTextFrame(pv)
			if err != nil {
				return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
			}
			p.Text = &tf
		case pg.ShapeDisplayList:
			dl, err := pg.DecodeDisplayList(pv)
			if err != nil {
				return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
			}
			p.DisplayList = &dl
		default:
			// Unreachable: admission refuses an unsupported shape at Mount, so
			// a shape we cannot marshal never reaches a frame. Say so loudly
			// rather than half-render — the admission contract, at the seam.
			return C.CString(fmt.Sprintf(
				`{"ok":false,"error":"port %q has shape %q with no driver at the bridge seam"}`,
				name, pv.Shape))
		}
		dto.Ports[name] = p
	}

	b, err := json.Marshal(dto)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

// ProgramClose stops the program and tears down the handle. Joins the wake
// goroutine before returning so C# can Free the pinned delegate immediately
// after (P6 release-order discipline).
//
//export ProgramClose
func ProgramClose(h C.int64_t) {
	handle := int64(h)
	programMu.Lock()
	ph, ok := programs[handle]
	if ok {
		delete(programs, handle)
	}
	programMu.Unlock()
	if !ok {
		return
	}
	teardownProgram(ph)
}

func teardownProgram(ph *programHandle) {
	if ph.cancelChange != nil {
		ph.cancelChange()
	}
	ph.host.Close()
	close(ph.doneCh)
	if ph.wakeDoneCh != nil {
		<-ph.wakeDoneCh
	}
}

// cascadePrograms tears down every program mounted on a destroyed peer. Mirrors
// cascadeLives / cascadeAsteroids.
func cascadePrograms(peerHandle int64) {
	programMu.Lock()
	var doomed []*programHandle
	for h, ph := range programs {
		if ph.peerHandleID == peerHandle {
			doomed = append(doomed, ph)
			delete(programs, h)
		}
	}
	programMu.Unlock()
	for _, ph := range doomed {
		teardownProgram(ph)
	}
}
