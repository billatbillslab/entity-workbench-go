// Package main is the Go c-shared library bridging the workbench
// stack (entitysdk + shellboot + shellcmd) into the Avalonia-based
// .NET desktop frontend.
//
// Build:  CGO_ENABLED=1 go build -buildmode=c-shared -o libbridge.so .
//
// The .NET side loads libbridge.so via P/Invoke; see
// ../frontend/Bridge.cs for the matching declarations.
//
// Lifecycle: the C# Program.cs calls BridgeInit on window open and
// BridgeShutdown on window close. All exported functions are safe to
// call concurrently from C# threads; internal state is protected by
// the mutexes on shellboot.PeerManager + the cgo-specific tree/watch
// handle maps in this file. Strings returned to C# are heap-allocated
// via C.CString and MUST be released by the caller with FreeString.
//
// Per PHASE-I-MULTI-PEER-PLAN.md §12 (Session 4): peer-manager + roster
// logic lives in shellboot.PeerManager (renderer-neutral). The bridge
// keeps only:
//   - cgo //export functions as JSON envelope wrappers
//   - tree-handle + watch-handle maps + goroutine fan-out (cgo-specific
//     because each entry holds C function pointers)
//   - the cascade-on-destroy hook that ties bridge-owned resources to
//     a peer's lifetime
//
// No multi-peer business logic in this file. Console adopts the same
// shellboot.PeerManager via a separate code path (PLAN §13).
package main

/*
#include <stdlib.h>
#include <stdint.h>

// Invoke a C# watch-event callback function pointer registered via
// WatchSubscribe. Called from the watch-fanout goroutine; the C# side
// MUST copy the json string before returning since Go frees it the
// moment this call returns.
static inline void invoke_watch(void* cb, int64_t handle, const char* json) {
    if (cb != NULL) {
        ((void(*)(int64_t, const char*))cb)(handle, json);
    }
}

// Invoke a C# tree-wake callback (no payload — caller pulls the latest
// snapshot via TreeRender). Wake-storm collapsing is the caller's job;
// the goroutine fans out at most one wake per dirty-transition window.
static inline void invoke_tree_wake(void* cb, int64_t handle) {
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
	"entity-workbench-go/shellpanel"
	wb "entity-workbench-go/workbench"
)

const (
	// appID is the entity-workbench-go app's namespace under each
	// peer's tree (matches the cross-impl path convention; see
	// godot CLAUDE.md:333–342 + egui app_paths.rs).
	appID = "entity-workbench"

	errNotInit = `{"ok":false,"error":"bridge not initialized"}`
	errBadPeer = `{"ok":false,"error":"unknown peer handle"}`
)

// recoverToErrorEnvelope translates any panic inside a cgo-exported
// function into an `{"ok":false,"error":"panic in <fn>: <msg>"}`
// envelope on the named return value `result`.
//
// Critical: a panic on a cgo-locked thread aborts the entire host
// process. Without this catch every store-touching panic (e.g. the
// NamespacedIndex.canonicalize "not a valid peer ID" panic) drops
// the user into a SIGSEGV with no recoverable trace and no chance
// for C# to translate the error into a user-visible message.
//
// Apply this as the first `defer` in every //export function that
// can reach Store / PeerContext / shell dispatch:
//
//	func XYZ(args) (result *C.char) {
//	    defer recoverToErrorEnvelope("XYZ", &result)
//	    ...
//	}
func recoverToErrorEnvelope(fn string, result **C.char) {
	r := recover()
	if r == nil {
		return
	}
	msg := fmt.Sprintf(`{"ok":false,"error":"panic in %s: %v"}`, fn, r)
	*result = C.CString(msg)
}

// Globals: the peer manager (owned + cleaned by BridgeInit/Shutdown),
// the shellcmd registry (process-wide; same commands serve every peer),
// the default peer handle (single-peer convenience surface), and the
// cgo-specific tree+watch handle maps.

var (
	manager  *shellboot.PeerManager
	registry *shellcmd.Registry

	defaultPeerHandle int64 // populated by BridgeInit; what BridgeDefaultPeer returns

	watchCounter int64
	watchMu      sync.Mutex
	watches      = map[int64]*watchHandle{}

	treeCounter int64
	treeMu      sync.Mutex
	trees       = map[int64]*treeHandle{}

	peerInfoCounter int64
	peerInfoMu      sync.Mutex
	peerInfos       = map[int64]*peerInfoHandle{}

	logCounter int64
	logMu      sync.Mutex
	logs       = map[int64]*logHandle{}

	mdViewCounter int64
	mdViewMu      sync.Mutex
	mdViews       = map[int64]*mdViewHandle{}

	mdFilesCounter int64
	mdFilesMu      sync.Mutex
	mdFiles        = map[int64]*mdFilesHandle{}

	queryCounter int64
	queryMu      sync.Mutex
	queries      = map[int64]*queryHandle{}
)

// watchHandle pairs a StoreWatch with the peer it belongs to so the
// cascade hook can find watches owned by a destroyed peer.
type watchHandle struct {
	peerHandleID int64
	w            *entitysdk.StoreWatch
}

// treeHandle bundles a TreeBrowserModel with the goroutine + channels
// driving its wake-coalescing. Tagged with peerHandleID for cascade.
//
// wakeDoneCh is closed by the RegisterWake goroutine on exit. Close
// and cascade paths MUST wait on it after closing doneCh — otherwise
// the goroutine can be mid-`C.invoke_tree_wake` (calling into a C#
// delegate's marshaled stub) when the C# side proceeds to null its
// delegate field, the .NET GC reclaims the stub, and the next
// invocation jumps into freed memory. Root cause of segfaults
// observed on panel swap / tab close.
type treeHandle struct {
	peerHandleID int64
	model        *wb.TreeBrowserModel
	wakeCh       chan struct{}
	doneCh       chan struct{}
	wakeDoneCh   chan struct{}
	cancelEv     func()
}

// peerInfoHandle bundles a PeerInfoModel with wake-coalescing
// machinery, same shape as treeHandle. The model has its own
// onPrefixChange subscription; we wire its dirty-transition to a
// wake channel the C# side polls via PeerInfoRender.
type peerInfoHandle struct {
	peerHandleID int64
	model        *wb.PeerInfoModel
	wakeCh       chan struct{}
	doneCh       chan struct{}
	wakeDoneCh   chan struct{}
	cancelEv     func()
}

// logHandle bundles a LogFilterModel with EventLog wake fanout.
// Same shape as treeHandle / peerInfoHandle. cancelEv is the
// EventLog.OnAppend cancel function.
type logHandle struct {
	peerHandleID int64
	model        *wb.LogFilterModel
	wakeCh       chan struct{}
	doneCh       chan struct{}
	wakeDoneCh   chan struct{}
	cancelEv     func()
}

// queryHandle wraps a QueryModel. No wake source — queries are pull-
// only (results don't change until the next Execute). Lightest of all
// panel handles; no goroutine, no subscription.
type queryHandle struct {
	peerHandleID int64
	model        *wb.QueryModel
}

// mdFilesHandle wraps a MarkdownFilesModel. The model maintains its
// own prefix subscription internally; we add a second subscription
// on the peer's "" prefix purely to drive the wake channel — the
// model's Render() is a no-op when nothing changed, so spurious
// wakes are cheap.
type mdFilesHandle struct {
	peerHandleID int64
	model        *wb.MarkdownFilesModel
	wakeCh       chan struct{}
	doneCh       chan struct{}
	wakeDoneCh   chan struct{}
	cancelEv     func()
}

// mdViewHandle wraps a MarkdownViewModel. Two wake sources:
//   - C# calls MarkdownViewLoadPath to bind a new path → wake.
//   - The bound path's entity is mutated → wake (per-path Store.Watch).
//
// pathWatchCancel cancels the per-path watch; rebound when path changes.
type mdViewHandle struct {
	peerHandleID int64
	store        *entitysdk.Store
	model        *wb.MarkdownViewModel
	wakeCh       chan struct{}
	doneCh       chan struct{}
	wakeDoneCh   chan struct{}

	mu              sync.Mutex
	pathWatchCancel func()
}

// ---- Bridge init / shutdown -----------------------------------------------

// BridgeInit boots a default peer for the C# caller (single-peer
// convenience). Equivalent to PeerCreate(cfgJson) + remembering the
// returned handle as the implicit default.
//
// Idempotent: subsequent calls are no-ops once a default peer exists.
// To boot additional peers, call PeerCreate.
//
// Returns NULL on success; on failure returns an error string the
// caller must release with FreeString.
//
//export BridgeInit
func BridgeInit(cConfig *C.char) *C.char {
	if atomic.LoadInt64(&defaultPeerHandle) != 0 {
		return nil // already booted
	}
	// First call also initializes process-wide state: the manager
	// and the shellcmd command registry. Both stay alive until
	// BridgeShutdown.
	if manager == nil {
		manager = shellboot.NewPeerManager(appID)
		registry = shellcmd.Default()
		// Cascade hook — tear down bridge-owned trees + watches
		// belonging to a destroyed peer BEFORE the AppPeer closes.
		manager.OnPeerDestroyed(func(h int64) {
			cascadeTrees(h)
			cascadeWatches(h)
			cascadePeerInfos(h)
			cascadeDiscoveries(h)
			cascadeLogs(h)
			cascadeMdViews(h)
			cascadeMdFiles(h)
			cascadeQueries(h)
			cascadeSites(h)
			cascadeShells(h)
			cascadeConnections(h)
			cascadeLivenesses(h)
			cascadePrograms(h)
			cascadeHandlers(h)
		})
	}

	cfg := shellboot.Config{}
	if cConfig != nil {
		if blob := C.GoString(cConfig); blob != "" {
			if err := json.Unmarshal([]byte(blob), &cfg); err != nil {
				return C.CString(fmt.Sprintf("config: %s", err.Error()))
			}
		}
	}
	h, err := manager.Create(cfg)
	if err != nil && h == 0 {
		return C.CString(err.Error())
	}
	// Non-zero handle + non-nil err = peer up, roster write failed;
	// treat as warning (matches the single-peer-convenience contract
	// that returning NULL means "good enough to proceed").
	atomic.StoreInt64(&defaultPeerHandle, h)
	return nil
}

// BridgeDefaultPeer returns the default peer handle established by
// BridgeInit, or 0 if BridgeInit hasn't run. C# calls this once
// after init to learn the handle to thread through per-peer ops.
//
//export BridgeDefaultPeer
func BridgeDefaultPeer() C.int64_t {
	return C.int64_t(atomic.LoadInt64(&defaultPeerHandle))
}

// BridgeShutdown tears down every peer, cascading through registered
// hooks. Idempotent.
//
//export BridgeShutdown
func BridgeShutdown() {
	if manager != nil {
		manager.ShutdownAll()
	}
	atomic.StoreInt64(&defaultPeerHandle, 0)
}

// BridgeRestorePeers reads the system peer's roster and respawns
// every non-ephemeral peer that isn't already hosted. Envelope:
//
//	{"ok":true,"restored":[N,M,...]}             (handle ids)
//	{"ok":true,"restored":[N],"warning":"..."}   (partial — first error)
//	{"ok":false,"error":"..."}                   (no system peer / list failure)
//
//export BridgeRestorePeers
func BridgeRestorePeers() *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	handles, err := manager.RestoreFromRoster()
	if err != nil && len(handles) == 0 {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	payload := map[string]any{
		"ok":       true,
		"restored": handles,
	}
	if err != nil {
		payload["warning"] = err.Error()
	}
	b, jerr := json.Marshal(payload)
	if jerr != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, jerr.Error()))
	}
	return C.CString(string(b))
}

// Hello returns a liveness greeting. Used by the boot spike + smoke tests.
//
//export Hello
func Hello() *C.char {
	return C.CString("Hello from Go — bridge alive, multi-peer ready.")
}

// FreeString releases a C-string previously returned by any bridge
// function. Safe to call with NULL.
//
//export FreeString
func FreeString(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

// ---- Peer manager (envelope wrappers around shellboot.PeerManager) -------

//export PeerCreate
func PeerCreate(cConfig *C.char) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	cfg := shellboot.Config{}
	if cConfig != nil {
		if blob := C.GoString(cConfig); blob != "" {
			if err := json.Unmarshal([]byte(blob), &cfg); err != nil {
				return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, "config: "+err.Error()))
			}
		}
	}
	h, err := manager.Create(cfg)
	if err != nil && h == 0 {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d,"warning":%q}`, h, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export PeerDestroy
func PeerDestroy(h C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	if err := manager.Destroy(int64(h)); err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	if atomic.LoadInt64(&defaultPeerHandle) == int64(h) {
		atomic.StoreInt64(&defaultPeerHandle, 0)
	}
	return C.CString(`{"ok":true}`)
}

// peerListDTO mirrors the legacy bridge response shape so the C# side
// PeerInfoDto continues to deserialize unchanged.
type peerListDTO struct {
	Handle      int64  `json:"handle"`
	PeerID      string `json:"peer_id"`
	Alias       string `json:"alias"`
	Identity    string `json:"identity"`
	StorageKind string `json:"storage_kind"`
	ListenAddr  string `json:"listen,omitempty"`
	IsSystem    bool   `json:"is_system"`
	Connections int    `json:"connections"`
	AddedAt     int64  `json:"added_at"`
}

//export PeerList
func PeerList() *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	live := manager.List()
	dtos := make([]peerListDTO, 0, len(live))
	for _, hp := range live {
		// shell.Conns includes the local peer keyed by its own alias,
		// so remote count is len-1.
		remote := len(hp.Shell.Conns) - 1
		if remote < 0 {
			remote = 0
		}
		dtos = append(dtos, peerListDTO{
			Handle:      hp.Handle,
			PeerID:      hp.AppPeer.PeerID(),
			Alias:       hp.Workspace.Local.Alias,
			Identity:    hp.Config.Identity,
			StorageKind: hp.Config.StorageKind,
			ListenAddr:  hp.Config.ListenAddr,
			IsSystem:    hp.IsSystem,
			Connections: remote,
			AddedAt:     hp.AddedAt,
		})
	}
	b, err := json.Marshal(map[string]any{
		"ok":    true,
		"peers": dtos,
	})
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(string(b))
}

//export PeerConfig
func PeerConfig(h C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(h))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	b, err := json.Marshal(map[string]any{
		"ok":     true,
		"config": hp.Config,
	})
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(string(b))
}

// The tree's lifecycle record — `system/peer/status` — lives in
// liveness.go (LivenessOpen / RegisterWake / Render / Close). It used
// to be a one-shot `PeerLiveness(h)` export here; that shape re-paid
// the model's seed on every call and, having no wake, could not tell a
// panel about the one transition the connection pool cannot express
// (a demotion to `suspect` writes the status entity and nothing else).
// See liveness.go's header.

//export PeerListenAddr
func PeerListenAddr(h C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(h))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	payload := map[string]any{
		"ok": true,
	}
	// `scheme` is the authority on whether we are listening, not
	// `Addr()`. core-go sets p.listener only on ListenReady, so
	// Peer.Addr() is nil for a WebSocket listener that is up and
	// serving — this reported `listening: false` to the UI for every
	// ws peer until 2026-08-18. Fall back to the self-advertised dial
	// URL, which is what a peer would use anyway.
	scheme := hp.ListenScheme
	if scheme == "" {
		payload["result"] = map[string]any{
			"listening":  false,
			"scheme":     "",
			"addr":       "",
			"advertised": "",
		}
	} else {
		shown := hp.AdvertisedURL
		if addr := hp.AppPeer.Addr(); addr != nil {
			shown = addr.String()
		}
		payload["result"] = map[string]any{
			"listening":  true,
			"scheme":     scheme,
			"addr":       shown,
			"advertised": hp.AdvertisedURL,
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(string(b))
}

// ---- Per-peer ops --------------------------------------------------------

//export DispatchLine
func DispatchLine(peerHandle C.int64_t, cLine *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("DispatchLine", &result)
	if manager == nil || registry == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	line := strings.TrimSpace(C.GoString(cLine))
	out := []wb.OutputLine{{Text: promptFor(hp.Shell) + line, Kind: wb.KindPath}}

	if line == "" {
		return encodeDispatchResp(hp.Shell, out)
	}
	args := shellcmd.SplitArgs(line)
	if len(args) == 0 {
		return encodeDispatchResp(hp.Shell, out)
	}
	cmd := args[0]
	switch cmd {
	case "clear":
		out = []wb.OutputLine{{Text: "(clear)", Kind: wb.KindNull}}
	case "quit", "exit":
		out = append(out, wb.OutputLine{
			Text: "(panel shell does not exit; close the window)",
			Kind: wb.KindNull,
		})
	default:
		res, err := registry.Dispatch(hp.Shell, cmd, args[1:])
		if err != nil {
			out = append(out, shellpanel.RenderError(err))
		} else {
			out = append(out, shellpanel.RenderResult(res)...)
		}
	}
	return encodeDispatchResp(hp.Shell, out)
}

func encodeDispatchResp(sh *shellcmd.Shell, lines []wb.OutputLine) *C.char {
	type lineDto struct {
		Text string `json:"text"`
		Kind string `json:"kind"`
	}
	dtos := make([]lineDto, len(lines))
	for i, l := range lines {
		dtos[i] = lineDto{Text: l.Text, Kind: kindName(l.Kind)}
	}
	payload := map[string]any{
		"ok":     true,
		"lines":  dtos,
		"prompt": promptFor(sh),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(string(b))
}

//export Complete
func Complete(peerHandle C.int64_t, cLine *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("Complete", &result)
	if manager == nil || registry == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	line := C.GoString(cLine)

	tokenStart := len(line)
	for i := len(line) - 1; i >= 0; i-- {
		if line[i] == ' ' || line[i] == '\t' {
			tokenStart = i + 1
			break
		}
		if i == 0 {
			tokenStart = 0
		}
	}
	prefix := line[tokenStart:]

	leading := strings.TrimSpace(line[:tokenStart])
	candidates := []string{}
	if leading == "" {
		for _, c := range registry.Commands() {
			if strings.HasPrefix(c.Name, prefix) {
				candidates = append(candidates, c.Name)
			}
		}
	} else {
		candidates = pathCandidates(hp, prefix)
	}

	payload := map[string]any{
		"ok":         true,
		"candidates": candidates,
		"tokenStart": tokenStart,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(string(b))
}

//export EntityGet
func EntityGet(peerHandle C.int64_t, cPath *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("EntityGet", &result)
	if manager == nil || registry == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	path := strings.TrimSpace(C.GoString(cPath))
	if path == "" {
		return C.CString(`{"ok":true,"lines":[]}`)
	}
	res, err := registry.Dispatch(hp.Shell, "cat", []string{path})
	type lineDto struct {
		Text string `json:"text"`
		Kind string `json:"kind"`
	}
	var dtos []lineDto
	if err != nil {
		l := shellpanel.RenderError(err)
		dtos = append(dtos, lineDto{Text: l.Text, Kind: kindName(l.Kind)})
	} else {
		for _, l := range shellpanel.RenderResult(res) {
			dtos = append(dtos, lineDto{Text: l.Text, Kind: kindName(l.Kind)})
		}
	}
	b, jerr := json.Marshal(map[string]any{"ok": true, "lines": dtos})
	if jerr != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, jerr.Error()))
	}
	return C.CString(string(b))
}

//export PeerSummary
func PeerSummary(peerHandle C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	if hp.Shell == nil || hp.Shell.Local == nil {
		return C.CString(errNotInit)
	}
	remote := len(hp.Shell.Conns) - 1
	if remote < 0 {
		remote = 0
	}
	payload := map[string]any{
		"ok":          true,
		"alias":       hp.Shell.Local.Alias,
		"peer_id":     hp.Shell.Local.PeerID,
		"identity":    hp.Shell.Identity,
		"connections": remote,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(string(b))
}

//export ShellPrompt
func ShellPrompt(peerHandle C.int64_t) *C.char {
	if manager == nil {
		return C.CString("")
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString("")
	}
	return C.CString(promptFor(hp.Shell))
}

func promptFor(sh *shellcmd.Shell) string {
	wd := sh.WD
	peerID := wd.PeerID()
	alias := ""
	if peerID != "" {
		alias = sh.AliasFor(peerID)
	}
	if alias != "" {
		bare := wd.BarePath()
		if bare == "" {
			return "entity:" + alias + ":/ > "
		}
		return "entity:" + alias + ":/" + bare + " > "
	}
	return "entity:" + string(wd) + " > "
}

func pathCandidates(hp *shellboot.HostedPeer, prefix string) []string {
	if hp == nil || hp.Shell == nil || hp.AppPeer == nil {
		return nil
	}
	if strings.HasPrefix(prefix, "/") || strings.HasPrefix(prefix, "@") {
		return nil
	}
	const maxCandidates = 50

	canonical := hp.Shell.Resolve(prefix)
	entries := hp.AppPeer.Store().List(string(canonical))

	wdCanonical := string(hp.Shell.WD)
	if !strings.HasSuffix(wdCanonical, "/") {
		wdCanonical += "/"
	}

	out := make([]string, 0, len(entries))
	seen := map[string]struct{}{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, wdCanonical) {
			continue
		}
		display := e.Path[len(wdCanonical):]
		if _, ok := seen[display]; ok {
			continue
		}
		seen[display] = struct{}{}
		out = append(out, display)
		if len(out) >= maxCandidates {
			break
		}
	}
	return out
}

func kindName(k wb.ValueKind) string {
	switch k {
	case wb.KindPath:
		return "path"
	case wb.KindHash:
		return "hash"
	case wb.KindKey:
		return "key"
	case wb.KindString:
		return "string"
	case wb.KindNumber:
		return "number"
	case wb.KindError:
		return "error"
	case wb.KindNull:
		return "null"
	default:
		return "unknown"
	}
}

// ---- Watches -------------------------------------------------------------

//export WatchSubscribe
func WatchSubscribe(peerHandle C.int64_t, cPattern *C.char, cb unsafe.Pointer) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	w, err := hp.AppPeer.Store().Watch(C.GoString(cPattern))
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	h := atomic.AddInt64(&watchCounter, 1)
	watchMu.Lock()
	watches[h] = &watchHandle{peerHandleID: hp.Handle, w: w}
	watchMu.Unlock()

	go func(handle int64) {
		for evt := range w.Events() {
			payload := map[string]any{
				"type": string(evt.EventType),
				"path": evt.Path,
				"hash": evt.NewHash.String(),
			}
			b, _ := json.Marshal(payload)
			cs := C.CString(string(b))
			C.invoke_watch(cb, C.int64_t(handle), cs)
			C.free(unsafe.Pointer(cs))
		}
	}(h)
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export WatchUnsubscribe
func WatchUnsubscribe(h C.int64_t) {
	handle := int64(h)
	watchMu.Lock()
	wh, ok := watches[handle]
	if ok {
		delete(watches, handle)
	}
	watchMu.Unlock()
	if ok {
		wh.w.Close()
	}
}

// cascadeWatches tears down every watch handle tagged with peer h.
// Registered as an OnPeerDestroyed hook in BridgeInit.
func cascadeWatches(h int64) {
	watchMu.Lock()
	victims := []*watchHandle{}
	for id, wh := range watches {
		if wh.peerHandleID == h {
			victims = append(victims, wh)
			delete(watches, id)
		}
	}
	watchMu.Unlock()
	for _, wh := range victims {
		wh.w.Close()
	}
}

// ---- Tree browser --------------------------------------------------------

//export TreeOpen
func TreeOpen(peerHandle C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	model := wb.NewTreeBrowserModel(hp.AppPeer.PeerContext())
	th := &treeHandle{
		peerHandleID: hp.Handle,
		model:        model,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	th.cancelEv = hp.AppPeer.Store().OnPrefixChange("", func(_ wb.ChangeEvent) {
		select {
		case th.wakeCh <- struct{}{}:
		default:
		}
	})
	h := atomic.AddInt64(&treeCounter, 1)
	treeMu.Lock()
	trees[h] = th
	treeMu.Unlock()
	select {
	case th.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export TreeRegisterWake
func TreeRegisterWake(h C.int64_t, cb unsafe.Pointer) *C.char {
	handle := int64(h)
	treeMu.Lock()
	th, ok := trees[handle]
	treeMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown tree handle"}`)
	}
	th.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(th.wakeDoneCh)
		for {
			select {
			case <-th.doneCh:
				return
			case <-th.wakeCh:
				C.invoke_tree_wake(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

//export TreeRender
func TreeRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("TreeRender", &result)
	handle := int64(h)
	treeMu.Lock()
	th, ok := trees[handle]
	treeMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown tree handle"}`)
	}
	th.model.Refresh()
	out := th.model.Render()
	b, err := json.Marshal(out)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

//export TreeToggleExpand
func TreeToggleExpand(h C.int64_t, index C.int) (result *C.char) {
	defer recoverToErrorEnvelope("TreeToggleExpand", &result)
	handle := int64(h)
	treeMu.Lock()
	th, ok := trees[handle]
	treeMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown tree handle"}`)
	}
	th.model.ToggleExpand(int(index))
	select {
	case th.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(`{"ok":true}`)
}

//export TreeSetSearch
func TreeSetSearch(h C.int64_t, cText *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("TreeSetSearch", &result)
	handle := int64(h)
	treeMu.Lock()
	th, ok := trees[handle]
	treeMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown tree handle"}`)
	}
	text := ""
	if cText != nil {
		text = C.GoString(cText)
	}
	if text == "" {
		th.model.ClearSearch()
	} else {
		th.model.SetSearch(text)
	}
	select {
	case th.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(`{"ok":true}`)
}

//export TreeClose
func TreeClose(h C.int64_t) {
	handle := int64(h)
	treeMu.Lock()
	th, ok := trees[handle]
	if ok {
		delete(trees, handle)
	}
	treeMu.Unlock()
	if !ok {
		return
	}
	if th.cancelEv != nil {
		th.cancelEv()
	}
	close(th.doneCh)
	// Wait for the wake goroutine to exit before returning, so the
	// C# caller can safely drop its delegate reference without
	// risking a use-after-free via C.invoke_tree_wake.
	if th.wakeDoneCh != nil {
		<-th.wakeDoneCh
	}
	th.model.Close()
}

// cascadeTrees tears down every tree handle tagged with peer h.
// Registered as an OnPeerDestroyed hook in BridgeInit.
func cascadeTrees(h int64) {
	treeMu.Lock()
	victims := []*treeHandle{}
	for id, th := range trees {
		if th.peerHandleID == h {
			victims = append(victims, th)
			delete(trees, id)
		}
	}
	treeMu.Unlock()
	for _, th := range victims {
		if th.cancelEv != nil {
			th.cancelEv()
		}
		close(th.doneCh)
		if th.wakeDoneCh != nil {
			<-th.wakeDoneCh
		}
		th.model.Close()
	}
}

// ---- PeerInfo panel ------------------------------------------------------
//
// Mirrors the tree-handle shape: Open returns a handle, RegisterWake
// spawns the wake-fanout goroutine, Render pulls the snapshot, Close
// tears down. Cascade fires on peer destroy.

//export PeerInfoOpen
func PeerInfoOpen(peerHandle C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	model := wb.NewPeerInfoModel(hp.AppPeer.PeerContext())
	ph := &peerInfoHandle{
		peerHandleID: hp.Handle,
		model:        model,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	// PeerInfoModel already subscribes via NewPeerInfoModel; we add
	// our own subscription purely to drive the wake channel. The
	// model maintains its own state independently.
	ph.cancelEv = hp.AppPeer.Store().OnPrefixChange("", func(_ wb.ChangeEvent) {
		select {
		case ph.wakeCh <- struct{}{}:
		default:
		}
	})
	h := atomic.AddInt64(&peerInfoCounter, 1)
	peerInfoMu.Lock()
	peerInfos[h] = ph
	peerInfoMu.Unlock()
	// Seed wake so the C# side gets an initial render.
	select {
	case ph.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export PeerInfoRegisterWake
func PeerInfoRegisterWake(h C.int64_t, cb unsafe.Pointer) *C.char {
	handle := int64(h)
	peerInfoMu.Lock()
	ph, ok := peerInfos[handle]
	peerInfoMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown peer-info handle"}`)
	}
	ph.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(ph.wakeDoneCh)
		for {
			select {
			case <-ph.doneCh:
				return
			case <-ph.wakeCh:
				C.invoke_tree_wake(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

//export PeerInfoRender
func PeerInfoRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("PeerInfoRender", &result)
	handle := int64(h)
	peerInfoMu.Lock()
	ph, ok := peerInfos[handle]
	peerInfoMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown peer-info handle"}`)
	}
	out := ph.model.Render()
	b, err := json.Marshal(out)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

//export PeerInfoClose
func PeerInfoClose(h C.int64_t) {
	handle := int64(h)
	peerInfoMu.Lock()
	ph, ok := peerInfos[handle]
	if ok {
		delete(peerInfos, handle)
	}
	peerInfoMu.Unlock()
	if !ok {
		return
	}
	if ph.cancelEv != nil {
		ph.cancelEv()
	}
	close(ph.doneCh)
	if ph.wakeDoneCh != nil {
		<-ph.wakeDoneCh
	}
	ph.model.Close()
}

// cascadePeerInfos tears down every peer-info handle tagged with peer h.
func cascadePeerInfos(h int64) {
	peerInfoMu.Lock()
	victims := []*peerInfoHandle{}
	for id, ph := range peerInfos {
		if ph.peerHandleID == h {
			victims = append(victims, ph)
			delete(peerInfos, id)
		}
	}
	peerInfoMu.Unlock()
	for _, ph := range victims {
		if ph.cancelEv != nil {
			ph.cancelEv()
		}
		close(ph.doneCh)
		if ph.wakeDoneCh != nil {
			<-ph.wakeDoneCh
		}
		ph.model.Close()
	}
}

// ---- Log viewer panel ----------------------------------------------------
//
// Same Open/RegisterWake/Render/Close lifecycle as tree + peer-info.
// LogCycleDisplayLevel / LogCycleCollectionLevel let the C# side
// drive the filter without owning the model state.

//export LogOpen
func LogOpen(peerHandle C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	model := wb.NewLogFilterModel(hp.AppPeer.EventLog())
	lh := &logHandle{
		peerHandleID: hp.Handle,
		model:        model,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	lh.cancelEv = hp.AppPeer.EventLog().OnAppend(func() {
		select {
		case lh.wakeCh <- struct{}{}:
		default:
		}
	})
	h := atomic.AddInt64(&logCounter, 1)
	logMu.Lock()
	logs[h] = lh
	logMu.Unlock()
	// Seed an initial render so the panel paints existing entries
	// without waiting for a fresh log call.
	select {
	case lh.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export LogRegisterWake
func LogRegisterWake(h C.int64_t, cb unsafe.Pointer) *C.char {
	handle := int64(h)
	logMu.Lock()
	lh, ok := logs[handle]
	logMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown log handle"}`)
	}
	lh.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(lh.wakeDoneCh)
		for {
			select {
			case <-lh.doneCh:
				return
			case <-lh.wakeCh:
				C.invoke_tree_wake(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

//export LogRender
func LogRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("LogRender", &result)
	handle := int64(h)
	logMu.Lock()
	lh, ok := logs[handle]
	logMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown log handle"}`)
	}
	out := lh.model.Render()
	b, err := json.Marshal(out)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

//export LogCycleDisplayLevel
func LogCycleDisplayLevel(h C.int64_t) *C.char {
	handle := int64(h)
	logMu.Lock()
	lh, ok := logs[handle]
	logMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown log handle"}`)
	}
	lh.model.CycleDisplayLevel()
	// Wake so the C# side picks up the new title + filtered set.
	select {
	case lh.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(`{"ok":true}`)
}

//export LogCycleCollectionLevel
func LogCycleCollectionLevel(h C.int64_t) *C.char {
	handle := int64(h)
	logMu.Lock()
	lh, ok := logs[handle]
	logMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown log handle"}`)
	}
	lh.model.CycleCollectionLevel()
	select {
	case lh.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(`{"ok":true}`)
}

//export LogClose
func LogClose(h C.int64_t) {
	handle := int64(h)
	logMu.Lock()
	lh, ok := logs[handle]
	if ok {
		delete(logs, handle)
	}
	logMu.Unlock()
	if !ok {
		return
	}
	if lh.cancelEv != nil {
		lh.cancelEv()
	}
	close(lh.doneCh)
	if lh.wakeDoneCh != nil {
		<-lh.wakeDoneCh
	}
}

// cascadeLogs tears down every log handle tagged with peer h.
func cascadeLogs(h int64) {
	logMu.Lock()
	victims := []*logHandle{}
	for id, lh := range logs {
		if lh.peerHandleID == h {
			victims = append(victims, lh)
			delete(logs, id)
		}
	}
	logMu.Unlock()
	for _, lh := range victims {
		if lh.cancelEv != nil {
			lh.cancelEv()
		}
		close(lh.doneCh)
		if lh.wakeDoneCh != nil {
			<-lh.wakeDoneCh
		}
	}
}

// ---- Markdown view panel -------------------------------------------------
//
// Read-mode only in v1 — selection-driven, displays a doc/markdown-file
// entity's content. Edit/save UI deferred (the bridge model supports
// it; just no C# UI to drive EnterEdit/UpdateTitle/Save yet).

//export MarkdownViewOpen
func MarkdownViewOpen(peerHandle C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	mh := &mdViewHandle{
		peerHandleID: hp.Handle,
		store:        hp.AppPeer.Store(),
		model:        wb.NewMarkdownViewModel(hp.AppPeer.PeerContext()),
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	h := atomic.AddInt64(&mdViewCounter, 1)
	mdViewMu.Lock()
	mdViews[h] = mh
	mdViewMu.Unlock()
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export MarkdownViewRegisterWake
func MarkdownViewRegisterWake(h C.int64_t, cb unsafe.Pointer) *C.char {
	handle := int64(h)
	mdViewMu.Lock()
	mh, ok := mdViews[handle]
	mdViewMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown markdown-view handle"}`)
	}
	mh.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(mh.wakeDoneCh)
		for {
			select {
			case <-mh.doneCh:
				return
			case <-mh.wakeCh:
				C.invoke_tree_wake(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

//export MarkdownViewLoadPath
func MarkdownViewLoadPath(h C.int64_t, cPath *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("MarkdownViewLoadPath", &result)
	handle := int64(h)
	mdViewMu.Lock()
	mh, ok := mdViews[handle]
	mdViewMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown markdown-view handle"}`)
	}
	path := ""
	if cPath != nil {
		path = C.GoString(cPath)
	}
	mh.model.LoadFromPath(path)

	// Rebind the per-path watch so we wake when this entity is mutated.
	mh.mu.Lock()
	if mh.pathWatchCancel != nil {
		mh.pathWatchCancel()
		mh.pathWatchCancel = nil
	}
	if path != "" {
		w, err := mh.store.Watch(path)
		if err == nil {
			done := make(chan struct{})
			go func() {
				defer close(done)
				for range w.Events() {
					select {
					case mh.wakeCh <- struct{}{}:
					default:
					}
				}
			}()
			mh.pathWatchCancel = func() { w.Close(); <-done }
		}
	}
	mh.mu.Unlock()

	// Fire wake so the panel re-renders with the new path immediately.
	select {
	case mh.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(`{"ok":true}`)
}

//export MarkdownViewRender
func MarkdownViewRender(h C.int64_t) (result *C.char) {
	// recoverPanic: model.Render walks Store.Resolve which calls
	// NamespacedIndex.canonicalize. canonicalize *panics* on
	// malformed paths (e.g. a path without a valid peer-id first
	// segment). A panic on a cgo-locked thread aborts the host
	// process — exactly the SIGSEGV class observed in real use.
	// Translate any panic into an error envelope so the C# side
	// sees a normal {ok:false,...} reply instead of the entire
	// app dying.
	defer recoverToErrorEnvelope("MarkdownViewRender", &result)
	handle := int64(h)
	mdViewMu.Lock()
	mh, ok := mdViews[handle]
	mdViewMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown markdown-view handle"}`)
	}
	out := mh.model.Render()
	b, err := json.Marshal(out)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

//export MarkdownViewClose
func MarkdownViewClose(h C.int64_t) {
	handle := int64(h)
	mdViewMu.Lock()
	mh, ok := mdViews[handle]
	if ok {
		delete(mdViews, handle)
	}
	mdViewMu.Unlock()
	if !ok {
		return
	}
	mh.mu.Lock()
	if mh.pathWatchCancel != nil {
		mh.pathWatchCancel()
		mh.pathWatchCancel = nil
	}
	mh.mu.Unlock()
	close(mh.doneCh)
	if mh.wakeDoneCh != nil {
		<-mh.wakeDoneCh
	}
}

// cascadeMdViews tears down every markdown-view handle tagged with peer h.
func cascadeMdViews(h int64) {
	mdViewMu.Lock()
	victims := []*mdViewHandle{}
	for id, mh := range mdViews {
		if mh.peerHandleID == h {
			victims = append(victims, mh)
			delete(mdViews, id)
		}
	}
	mdViewMu.Unlock()
	for _, mh := range victims {
		mh.mu.Lock()
		if mh.pathWatchCancel != nil {
			mh.pathWatchCancel()
			mh.pathWatchCancel = nil
		}
		mh.mu.Unlock()
		close(mh.doneCh)
		if mh.wakeDoneCh != nil {
			<-mh.wakeDoneCh
		}
	}
}

// ---- Markdown files panel -------------------------------------------------
//
// Tree-shaped browser filtered to doc/markdown-file entities under
// wb.MarkdownFilesPrefix ("docs/"). Same open/wake/render/close/
// toggleExpand surface as the main tree. Selection broadcast happens
// C# side via IPanelHost.PublishSelectedPath.

//export MarkdownFilesOpen
func MarkdownFilesOpen(peerHandle C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	model := wb.NewMarkdownFilesModel(hp.AppPeer.Store(), wb.MarkdownFilesPrefix)
	mh := &mdFilesHandle{
		peerHandleID: hp.Handle,
		model:        model,
		wakeCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	mh.cancelEv = hp.AppPeer.Store().OnPrefixChange("", func(_ wb.ChangeEvent) {
		select {
		case mh.wakeCh <- struct{}{}:
		default:
		}
	})
	h := atomic.AddInt64(&mdFilesCounter, 1)
	mdFilesMu.Lock()
	mdFiles[h] = mh
	mdFilesMu.Unlock()
	// Seed wake so the initial render fires.
	select {
	case mh.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export MarkdownFilesRegisterWake
func MarkdownFilesRegisterWake(h C.int64_t, cb unsafe.Pointer) *C.char {
	handle := int64(h)
	mdFilesMu.Lock()
	mh, ok := mdFiles[handle]
	mdFilesMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown markdown-files handle"}`)
	}
	mh.wakeDoneCh = make(chan struct{})
	go func() {
		defer close(mh.wakeDoneCh)
		for {
			select {
			case <-mh.doneCh:
				return
			case <-mh.wakeCh:
				C.invoke_tree_wake(cb, C.int64_t(handle))
			}
		}
	}()
	return C.CString(`{"ok":true}`)
}

//export MarkdownFilesRender
func MarkdownFilesRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("MarkdownFilesRender", &result)
	handle := int64(h)
	mdFilesMu.Lock()
	mh, ok := mdFiles[handle]
	mdFilesMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown markdown-files handle"}`)
	}
	mh.model.Refresh()
	out := mh.model.Render()
	b, err := json.Marshal(out)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

//export MarkdownFilesToggleExpand
func MarkdownFilesToggleExpand(h C.int64_t, index C.int) (result *C.char) {
	defer recoverToErrorEnvelope("MarkdownFilesToggleExpand", &result)
	handle := int64(h)
	mdFilesMu.Lock()
	mh, ok := mdFiles[handle]
	mdFilesMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown markdown-files handle"}`)
	}
	mh.model.ToggleExpand(int(index))
	select {
	case mh.wakeCh <- struct{}{}:
	default:
	}
	return C.CString(`{"ok":true}`)
}

//export MarkdownFilesClose
func MarkdownFilesClose(h C.int64_t) {
	handle := int64(h)
	mdFilesMu.Lock()
	mh, ok := mdFiles[handle]
	if ok {
		delete(mdFiles, handle)
	}
	mdFilesMu.Unlock()
	if !ok {
		return
	}
	if mh.cancelEv != nil {
		mh.cancelEv()
	}
	close(mh.doneCh)
	if mh.wakeDoneCh != nil {
		<-mh.wakeDoneCh
	}
	mh.model.Close()
}

// cascadeMdFiles tears down every markdown-files handle tagged with peer h.
func cascadeMdFiles(h int64) {
	mdFilesMu.Lock()
	victims := []*mdFilesHandle{}
	for id, mh := range mdFiles {
		if mh.peerHandleID == h {
			victims = append(victims, mh)
			delete(mdFiles, id)
		}
	}
	mdFilesMu.Unlock()
	for _, mh := range victims {
		if mh.cancelEv != nil {
			mh.cancelEv()
		}
		close(mh.doneCh)
		if mh.wakeDoneCh != nil {
			<-mh.wakeDoneCh
		}
		mh.model.Close()
	}
}

// ---- Query browser panel -------------------------------------------------
//
// Pull-only — no wake source. C# panel sets type + path filters,
// calls Execute, then renders the result list. SelectNext/Prev/
// NextPage drive list navigation. No registerWake.

//export QueryOpen
func QueryOpen(peerHandle C.int64_t) *C.char {
	if manager == nil {
		return C.CString(errNotInit)
	}
	hp := manager.Get(int64(peerHandle))
	if hp == nil {
		return C.CString(errBadPeer)
	}
	qh := &queryHandle{
		peerHandleID: hp.Handle,
		model:        wb.NewQueryModel(hp.AppPeer.PeerContext()),
	}
	h := atomic.AddInt64(&queryCounter, 1)
	queryMu.Lock()
	queries[h] = qh
	queryMu.Unlock()
	return C.CString(fmt.Sprintf(`{"ok":true,"handle":%d}`, h))
}

//export QuerySetFilters
func QuerySetFilters(h C.int64_t, cType *C.char, cPath *C.char) (result *C.char) {
	defer recoverToErrorEnvelope("QuerySetFilters", &result)
	handle := int64(h)
	queryMu.Lock()
	qh, ok := queries[handle]
	queryMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown query handle"}`)
	}
	if cType != nil {
		qh.model.TypeFilter = C.GoString(cType)
	}
	if cPath != nil {
		qh.model.PathPrefix = C.GoString(cPath)
	}
	return C.CString(`{"ok":true}`)
}

//export QueryExecute
func QueryExecute(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("QueryExecute", &result)
	handle := int64(h)
	queryMu.Lock()
	qh, ok := queries[handle]
	queryMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown query handle"}`)
	}
	qh.model.Execute()
	return C.CString(`{"ok":true}`)
}

//export QuerySelectNext
func QuerySelectNext(h C.int64_t) *C.char {
	handle := int64(h)
	queryMu.Lock()
	qh, ok := queries[handle]
	queryMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown query handle"}`)
	}
	qh.model.SelectNext()
	return C.CString(`{"ok":true}`)
}

//export QuerySelectPrev
func QuerySelectPrev(h C.int64_t) *C.char {
	handle := int64(h)
	queryMu.Lock()
	qh, ok := queries[handle]
	queryMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown query handle"}`)
	}
	qh.model.SelectPrev()
	return C.CString(`{"ok":true}`)
}

//export QueryNextPage
func QueryNextPage(h C.int64_t) *C.char {
	handle := int64(h)
	queryMu.Lock()
	qh, ok := queries[handle]
	queryMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown query handle"}`)
	}
	qh.model.NextPage()
	return C.CString(`{"ok":true}`)
}

//export QueryRender
func QueryRender(h C.int64_t) (result *C.char) {
	defer recoverToErrorEnvelope("QueryRender", &result)
	handle := int64(h)
	queryMu.Lock()
	qh, ok := queries[handle]
	queryMu.Unlock()
	if !ok {
		return C.CString(`{"ok":false,"error":"unknown query handle"}`)
	}
	// Flatten to a JSON-friendly shape: omit the hash field (binary,
	// not useful for the UI) and pin only what the panel renders.
	out := qh.model.Render()
	type matchOut struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	type renderOut struct {
		TypeFilter  string     `json:"type_filter"`
		PathPrefix  string     `json:"path_prefix"`
		Matches     []matchOut `json:"matches"`
		Total       uint64     `json:"total"`
		HasMore     bool       `json:"has_more"`
		Selected    int        `json:"selected"`
		Status      string     `json:"status"`
		HasExecuted bool       `json:"has_executed"`
	}
	r := renderOut{
		TypeFilter:  out.TypeFilter,
		PathPrefix:  out.PathPrefix,
		Total:       out.Total,
		HasMore:     out.HasMore,
		Selected:    out.Selected,
		Status:      out.Status,
		HasExecuted: out.HasExecuted,
		Matches:     make([]matchOut, len(out.Matches)),
	}
	for i, m := range out.Matches {
		r.Matches[i] = matchOut{Path: m.Path, Type: m.Type}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return C.CString(fmt.Sprintf(`{"ok":true,"result":%s}`, string(b)))
}

//export QueryClose
func QueryClose(h C.int64_t) {
	handle := int64(h)
	queryMu.Lock()
	delete(queries, handle)
	queryMu.Unlock()
}

// cascadeQueries tears down every query handle tagged with peer h.
func cascadeQueries(h int64) {
	queryMu.Lock()
	for id, qh := range queries {
		if qh.peerHandleID == h {
			delete(queries, id)
		}
	}
	queryMu.Unlock()
}

func main() {}
