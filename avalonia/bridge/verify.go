package main

// Publisher-verification panel bridge surface — VerifyOpen /
// VerifyRegisterWake / VerifyConfigure / VerifyStart / VerifyRender /
// VerifyClose on the cgo envelope (D14).
//
// **This is the one panel surface with no peer handle, and that is the
// design, not an omission.** EXTENSION-NETWORK §6.5.3's Mode A2
// consumer is *not a peer*: no dispatch participation, no ingest, no
// store. Requiring a peer to verify a published origin would have
// implied a relationship the corridor deliberately does not need — the
// whole point of the CDN corridor is that a stranger with a URL can
// check a publisher's work.
//
// Wake semantics differ from every other panel here too. The others
// wake on *tree events* — something changed under us. This one wakes
// exactly once per run, on completion, because a verification is an
// operation an operator started rather than a stream they subscribed
// to. `VerifyStart` returns immediately; the network work happens on a
// goroutine, so the UI thread is never inside an HTTP call.

/*
#include <stdlib.h>
#include <stdint.h>

// invoke_tree_wake — local copy of main.go's helper (cgo compiles each
// file's preamble as its own translation unit, so main.go's static
// inline is not visible here).
static inline void invoke_tree_wake_verify(void* cb, int64_t handle) {
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

	"entity-workbench-go/fetch"
	wb "entity-workbench-go/workbench"
)

type verifyHandle struct {
	model *wb.ConsumeModel

	mu      sync.Mutex
	running bool
	wakeCb  unsafe.Pointer
	cancel  context.CancelFunc
}

var (
	verifyCounter int64
	verifyMu      sync.Mutex
	verifies      = map[int64]*verifyHandle{}
)

// verifyConfig is the C#-facing configuration payload. Pin fields are
// all-or-nothing: any of them set means the layout is PINNED and
// discovery is skipped.
type verifyConfig struct {
	Origin    string `json:"origin"`
	PeerID    string `json:"peer_id"`
	Bodies    bool   `json:"bodies"`
	Reconcile bool   `json:"reconcile"`
	Absent    string `json:"absent"`

	PinTree     string `json:"pin_tree"`
	PinContent  string `json:"pin_content"`
	PinManifest string `json:"pin_manifest"`
	PinLayout   string `json:"pin_layout"`
	PinLeaf     string `json:"pin_leaf"`
	PinListing  string `json:"pin_listing"`
}

func (c verifyConfig) pinned() bool {
	return c.PinTree != "" || c.PinContent != "" || c.PinManifest != "" || c.PinLayout != ""
}

// VerifyOpen creates a verification panel handle.
//
//export VerifyOpen
func VerifyOpen() (result *C.char) {
	defer recoverToErrorEnvelope("VerifyOpen", &result)

	vh := &verifyHandle{model: wb.NewConsumeModel(nil)}
	h := atomic.AddInt64(&verifyCounter, 1)
	verifyMu.Lock()
	verifies[h] = vh
	verifyMu.Unlock()

	b, _ := json.Marshal(map[string]any{"ok": true, "handle": h})
	return C.CString(string(b))
}

func lookupVerify(h int64) *verifyHandle {
	verifyMu.Lock()
	defer verifyMu.Unlock()
	return verifies[h]
}

// VerifyRegisterWake registers the completion callback.
//
//export VerifyRegisterWake
func VerifyRegisterWake(handle C.int64_t, cb unsafe.Pointer) (result *C.char) {
	defer recoverToErrorEnvelope("VerifyRegisterWake", &result)

	vh := lookupVerify(int64(handle))
	if vh == nil {
		return C.CString(`{"ok":false,"error":"unknown verify handle"}`)
	}
	vh.mu.Lock()
	vh.wakeCb = cb
	vh.mu.Unlock()
	return C.CString(`{"ok":true}`)
}

// VerifyConfigure sets the origin and run options from a JSON payload.
//
//export VerifyConfigure
func VerifyConfigure(handle C.int64_t, cJSON *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("VerifyConfigure", &result)

	vh := lookupVerify(int64(handle))
	if vh == nil {
		return C.CString(`{"ok":false,"error":"unknown verify handle"}`)
	}
	var cfg verifyConfig
	if cJSON != nil {
		if err := json.Unmarshal([]byte(C.GoString(cJSON)), &cfg); err != nil {
			b, _ := json.Marshal(map[string]any{"ok": false, "error": "bad config: " + err.Error()})
			return C.CString(string(b))
		}
	}
	vh.model.SetOrigin(cfg.Origin)
	vh.model.SetPeerID(cfg.PeerID)
	vh.model.SetOpts(fetch.ConsumeOpts{
		Bodies: cfg.Bodies, Reconcile: cfg.Reconcile, AbsentProbe: cfg.Absent,
	})
	if cfg.pinned() {
		leaf, listing := cfg.PinLeaf, cfg.PinListing
		if leaf == "" {
			leaf = ".bin"
		}
		if listing == "" {
			listing = ".list"
		}
		vh.model.Pin(&types.TransportEndpoint{
			TreeURLPrefix:     cfg.PinTree,
			ContentURLPrefix:  cfg.PinContent,
			ManifestURLPrefix: cfg.PinManifest,
			ContentLayout:     cfg.PinLayout,
			TreeLeafSuffix:    leaf,
			TreeListingSuffix: listing,
		})
	} else {
		vh.model.Pin(nil)
	}
	return C.CString(`{"ok":true}`)
}

// VerifyStart kicks a run and returns immediately. A second start while
// one is in flight is refused rather than queued — two runs would
// interleave two origins' steps into one list, and the operator would
// read the result as one verification.
//
//export VerifyStart
func VerifyStart(handle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("VerifyStart", &result)

	vh := lookupVerify(int64(handle))
	if vh == nil {
		return C.CString(`{"ok":false,"error":"unknown verify handle"}`)
	}
	vh.mu.Lock()
	if vh.running {
		vh.mu.Unlock()
		return C.CString(`{"ok":false,"error":"a verification is already running"}`)
	}
	ctx, cancel := context.WithCancel(context.Background())
	vh.running, vh.cancel = true, cancel
	vh.mu.Unlock()

	go func() {
		defer func() {
			// A panic on the network path must not take the UI down
			// with it; the model keeps whatever steps completed.
			_ = recover()
		}()
		vh.model.Verify(ctx)

		vh.mu.Lock()
		vh.running, vh.cancel = false, nil
		cb := vh.wakeCb
		vh.mu.Unlock()
		if cb != nil {
			C.invoke_tree_wake_verify(cb, C.int64_t(handle))
		}
	}()
	return C.CString(`{"ok":true}`)
}

// VerifyRender returns the current view as JSON. Safe at any time,
// including mid-run — the result carries `running`.
//
//export VerifyRender
func VerifyRender(handle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("VerifyRender", &result)

	vh := lookupVerify(int64(handle))
	if vh == nil {
		return C.CString(`{"ok":false,"error":"unknown verify handle"}`)
	}
	out := vh.model.Render()

	// FreshnessNote is computed rather than stored, and it is the one
	// string on this panel that must not be summarized away — it is
	// what keeps a green result from reading as "verified" full stop.
	payload := struct {
		wb.ConsumeOutput
		FreshnessNote string `json:"freshness_note"`
	}{ConsumeOutput: out, FreshnessNote: out.FreshnessNote()}

	b, err := json.Marshal(map[string]any{"ok": true, "report": payload})
	if err != nil {
		return C.CString(`{"ok":false,"error":"marshal report failed"}`)
	}
	return C.CString(string(b))
}

// VerifyClose releases the handle and cancels any run in flight.
//
//export VerifyClose
func VerifyClose(handle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("VerifyClose", &result)

	h := int64(handle)
	verifyMu.Lock()
	vh := verifies[h]
	delete(verifies, h)
	verifyMu.Unlock()
	if vh == nil {
		return C.CString(`{"ok":true}`) // idempotent
	}
	vh.mu.Lock()
	vh.wakeCb = nil
	cancel := vh.cancel
	vh.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return C.CString(`{"ok":true}`)
}
