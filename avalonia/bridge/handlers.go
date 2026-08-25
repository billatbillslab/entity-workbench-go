package main

// Handler-browser panel bridge surface — the last console→Avalonia
// parity gap.
//
// `workbench.HandlerBrowserModel` has been renderer-neutral and complete
// since it was written: discovery, selection, spec formatting, execute,
// and an output log, all behind Render(). Only `console` ever drove it,
// which made the handler browser the one surface where the frozen
// renderer was ahead of the one doing the feature work. Nothing here
// reimplements any of that — this file is handle lifecycle, JSON
// marshalling, and wake fan-out, per GUIDE-AVALONIA-PANEL-PATTERNS P0-P6.
//
// The dispatch function is `shellcmd.Exec` against the hosted peer's
// local connection, which is the same call console's `dispatchExecute`
// makes. That is the DRY-the-integration rule: the shared thing is the
// dispatch seam, not the renderer.
//
// **This panel executes arbitrary handler operations against the local
// peer.** It is the most powerful surface in the app, and deliberately
// so — it is the tool you reach for when a verb does not exist yet, and
// it is what would have made this session's registry gap visible months
// ago. Execute is always explicit: nothing dispatches on selection
// change, only on ExecuteSelected/ExecuteCustom.

/*
#include <stdlib.h>
#include <stdint.h>

static inline void invoke_tree_wake_handlers(void* cb, int64_t handle) {
    if (cb != NULL) {
        ((void(*)(int64_t))cb)(handle);
    }
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/shellboot"
	"entity-workbench-go/shellcmd"
	wb "entity-workbench-go/workbench"
)

type handlersHandle struct {
	peerHandleID int64
	hp           *shellboot.HostedPeer
	model        *wb.HandlerBrowserModel

	wakeCh     chan struct{}
	doneCh     chan struct{}
	wakeDoneCh chan struct{}
	cancelEv   func()
}

var (
	handlersCounter int64
	handlersMu      sync.Mutex
	handlerPanels   = map[int64]*handlersHandle{}
)

// handlersRender is the JSON shape the C# panel binds to. It is a
// flattened projection of wb.HandlerBrowserOutput: the model's
// `Selected *HandlerInfo` pointer and `Specs` map do not survive a JSON
// boundary usefully, so the selected handler's operations are emitted
// with their spec strings already resolved.
type handlersRender struct {
	Handlers        []handlerRow `json:"handlers"`
	SelectedHandler int          `json:"selected_handler"`
	SelectedOp      int          `json:"selected_op"`
	SelectedPattern string       `json:"selected_pattern"`
	SelectedOpName  string       `json:"selected_op_name"`
	SpecLine        string       `json:"spec_line"`
	Operations      []opRow      `json:"operations"`
	Output          []outputRow  `json:"output"`
}

type handlerRow struct {
	Pattern string `json:"pattern"`
	Name    string `json:"name"`
	OpCount int    `json:"op_count"`
}

type opRow struct {
	Name       string `json:"name"`
	InputType  string `json:"input_type"`
	OutputType string `json:"output_type"`
}

type outputRow struct {
	Text string `json:"text"`
	Kind string `json:"kind"`
}

// HandlersOpen returns a handle scoped to a peer. The model owns a
// subscription on the `system/handler/` prefix and rediscovers only when
// an event arrives — never a full-tree scan per render, per the
// subscribe-by-prefix rule.
//
//export HandlersOpen
func HandlersOpen(peerHandle C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("HandlersOpen", &result)
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	hh := &handlersHandle{
		peerHandleID: hp.Handle,
		hp:           hp,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	// Same dispatch seam console uses (console/application.go
	// dispatchExecute) — shellcmd.Exec against the hosted peer's local
	// connection, so capability checks and remote routing behave
	// identically in both renderers.
	dispatch := func(path, operation string) (*entitysdk.Response, error) {
		if hh.hp == nil || hh.hp.Workspace == nil || hh.hp.Workspace.Local == nil {
			return nil, fmt.Errorf("no peer available")
		}
		return shellcmd.Exec(hh.hp.Workspace.Local, path, operation, nil, nil)
	}
	hh.model = wb.NewHandlerBrowserModel(hp.AppPeer.PeerContext(), dispatch)

	// The model subscribes to `system/handler/` to set its own dirty
	// flag, but it does not expose a callback — it is renderer-neutral
	// and owes a renderer nothing. So the panel takes its own
	// subscription on the same prefix purely to drive the wake. Two
	// subscriptions on one prefix is the honest cost of keeping the
	// model free of renderer plumbing, and it is still a prefix
	// subscription, never a scan.
	hh.cancelEv = hp.AppPeer.Store().OnPrefixChange("system/handler/", func(_ wb.ChangeEvent) {
		select {
		case hh.wakeCh <- struct{}{}:
		default:
		}
	})

	h := atomic.AddInt64(&handlersCounter, 1)
	handlersMu.Lock()
	handlerPanels[h] = hh
	handlersMu.Unlock()
	// Seed a wake so the panel paints the discovered set on mount rather
	// than staying blank until the first handler registration.
	select {
	case hh.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export HandlersRegisterWake
func HandlersRegisterWake(h C.int64_t, cb unsafe.Pointer) *C.char {
	handle := int64(h)
	hh, ok := lookupHandlers(handle)
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown handlers handle"}`)
	}
	hh.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(hh.wakeDoneCh)
		for {
			select {
			case <-hh.doneCh:
				return
			case <-hh.wakeCh:
				C.invoke_tree_wake_handlers(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

// HandlersRender snapshots the model. Refresh() is called first so a
// handler registered since the last paint is picked up; it is a no-op
// unless the prefix subscription flagged the model dirty.
//
//export HandlersRender
func HandlersRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("HandlersRender", &result)
	hh, ok := lookupHandlers(int64(h))
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown handlers handle"}`)
	}
	hh.model.Refresh()
	b, err := json.Marshal(buildHandlersRender(hh.model))
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

func buildHandlersRender(m *wb.HandlerBrowserModel) handlersRender {
	out := m.Render()
	rows := make([]handlerRow, 0, len(out.Handlers))
	for _, hi := range out.Handlers {
		rows = append(rows, handlerRow{
			Pattern: hi.Pattern,
			Name:    hi.Name,
			OpCount: len(hi.Operations),
		})
	}
	ops := []opRow{}
	if out.Selected != nil {
		for _, name := range out.Selected.Operations {
			spec := out.Selected.Specs[name]
			ops = append(ops, opRow{
				Name:       name,
				InputType:  spec.InputType,
				OutputType: spec.OutputType,
			})
		}
	}
	lines := make([]outputRow, 0, len(out.Output))
	for _, l := range out.Output {
		lines = append(lines, outputRow{Text: l.Text, Kind: kindName(l.Kind)})
	}
	r := handlersRender{
		Handlers:        rows,
		SelectedHandler: out.SelectedHandler,
		SelectedOp:      out.SelectedOp,
		SelectedOpName:  out.SelectedOpName,
		SpecLine:        out.SpecLine,
		Operations:      ops,
		Output:          lines,
	}
	if out.Selected != nil {
		r.SelectedPattern = out.Selected.Pattern
	}
	return r
}

// HandlersSelectHandler selects a handler by index and resets the
// operation selection (the model's own rule — an op index is only
// meaningful against the handler it came from).
//
//export HandlersSelectHandler
func HandlersSelectHandler(h C.int64_t, index C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("HandlersSelectHandler", &result)
	hh, ok := lookupHandlers(int64(h))
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown handlers handle"}`)
	}
	hh.model.SelectHandler(int(index))
	return C.CString(`{"ok":true}`)
}

//export HandlersSelectOperation
func HandlersSelectOperation(h C.int64_t, index C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("HandlersSelectOperation", &result)
	hh, ok := lookupHandlers(int64(h))
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown handlers handle"}`)
	}
	hh.model.SelectOperation(int(index))
	return C.CString(`{"ok":true}`)
}

// HandlersExecuteSelected runs the selected handler operation. Output
// lands in the model's log; the caller re-renders to read it.
//
//export HandlersExecuteSelected
func HandlersExecuteSelected(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("HandlersExecuteSelected", &result)
	hh, ok := lookupHandlers(int64(h))
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown handlers handle"}`)
	}
	hh.model.Execute()
	return C.CString(`{"ok":true}`)
}

// HandlersExecuteCustom runs an operation the discovery list does not
// offer — an explicit URI/op, optionally against a resource path.
//
// This is the escape hatch that makes the panel worth having: handler
// discovery reads `system/handler/*`, so an op that is dispatchable but
// undeclared, or a resource-qualified path, is unreachable from the
// list. The model refuses an empty URI or op and logs the refusal rather
// than dispatching a malformed request.
//
//export HandlersExecuteCustom
func HandlersExecuteCustom(h C.int64_t, cURI, cOp, cResource *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("HandlersExecuteCustom", &result)
	hh, ok := lookupHandlers(int64(h))
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown handlers handle"}`)
	}
	uri := strings.TrimSpace(C.GoString(cURI))
	op := strings.TrimSpace(C.GoString(cOp))
	resource := strings.TrimSpace(C.GoString(cResource))
	hh.model.ExecuteCustom(uri, op, resource)
	return C.CString(`{"ok":true}`)
}

//export HandlersClose
func HandlersClose(h C.int64_t) {
	handle := int64(h)
	handlersMu.Lock()
	hh, ok := handlerPanels[handle]
	if ok {
		delete(handlerPanels, handle)
	}
	handlersMu.Unlock()
	if !ok {
		return
	}
	closeHandlersHandle(hh)
}

func lookupHandlers(handle int64) (*handlersHandle, bool) {
	handlersMu.Lock()
	defer handlersMu.Unlock()
	hh, ok := handlerPanels[handle]
	return hh, ok
}

// closeHandlersHandle tears one handle down. The model's Close cancels
// its prefix subscription; the wake goroutine is joined so a callback
// cannot fire into a disposed managed object after the panel is gone —
// the lifetime rule the crash-hunt commits earned.
func closeHandlersHandle(hh *handlersHandle) {
	if hh.cancelEv != nil {
		hh.cancelEv()
	}
	if hh.model != nil {
		hh.model.Close()
	}
	close(hh.doneCh)
	if hh.wakeDoneCh != nil {
		<-hh.wakeDoneCh
	}
}

// cascadeHandlers tears down every handler-browser handle tagged with
// peer h. Registered as an OnPeerDestroyed hook in BridgeInit, matching
// every other panel: a destroyed peer must not leave a panel holding a
// model over a closed store.
func cascadeHandlers(h int64) {
	handlersMu.Lock()
	victims := []*handlersHandle{}
	for id, hh := range handlerPanels {
		if hh.peerHandleID == h {
			victims = append(victims, hh)
			delete(handlerPanels, id)
		}
	}
	handlersMu.Unlock()
	for _, hh := range victims {
		closeHandlersHandle(hh)
	}
}
