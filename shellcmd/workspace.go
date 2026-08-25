package shellcmd

import (
	"entity-workbench-go/entitysdk"
	"entity-workbench-go/workbench"
)

// ShellWorkspace is the workspace-level state shared by all Shell
// sessions in a single process. One process holds one AppPeer (per
// active identity), one alias table, and one set of workbench handler
// references; per-panel state (working directory, command history,
// output scrollback) stays on Shell.
//
// In single-shell deployments (the standalone entity-shell binary) the
// workspace has exactly one Shell. In multi-shell deployments
// (canvas, console) every shell panel is a Shell over the same
// workspace, so connecting a remote peer in one panel is immediately
// visible from another.
//
// v1 stance on concurrency: every Shell is driven by a single
// event-loop on its renderer (raylib frame loop, tview event loop,
// the REPL's read-eval-print loop). Commands therefore execute
// serially even when multiple shells live in one process, and the
// workspace needs no mutex. Future concurrent mutation paths (e.g.
// background subscription writers calling addConn) will need locking;
// see Phase G plan §"Concurrency."
type ShellWorkspace struct {
	// Local is the in-process peer the workspace owns. All shells
	// dispatch through this peer; remote peers are reached via the
	// peer's connection pool.
	Local *PeerConn

	// Conns is alias -> PeerConn. Includes the in-process peer keyed
	// by its configured alias (caller-supplied; falls back to "self"
	// when empty — "local" is reserved for the local/* extension
	// namespace).
	Conns map[string]*PeerConn

	// peerMap is peerID -> alias for reverse lookup when resolving
	// paths to connections.
	peerMap map[string]string

	// Identity is the active identity name. Empty means ephemeral.
	// One identity per workspace in v1; multi-identity is deferred
	// (SHELL-DIRECTION §4.8).
	Identity string

	// NotificationIngest is the workbench notification-ingest handler
	// used by the `mount` verb. Bound at workspace construction by
	// shellboot; nil when the workbench handlers aren't wired.
	NotificationIngest *workbench.NotificationIngestHandler

	// Browser is the workspace's consume-side browser: the pinned name
	// authority, the history, and the trust chain for whatever is on
	// screen (`registry` / `browse` / `open`). Nil until first use.
	//
	// It lives on the WORKSPACE, not the shell, because the registry pin
	// is a trust decision: two shells in one workspace resolving the same
	// name through different authorities is a very good way to confuse
	// someone about which one answered.
	//
	// It holds no peer. A Mode A2 consumer reading a static origin is not
	// a peer and does not become one by being useful — the browser
	// verifies signatures against keys it derives from peer-ids and never
	// dispatches anything.
	Browser *workbench.BrowseModel

	// mountSubs holds the source-prefix subscription per mounted root
	// (keyed by root name) so `unmount` can cancel it. Without this,
	// unmount left the subscription live and the subscription engine
	// kept delivering into a torn-down ingest mapping. Not guarded by
	// a mutex — consistent with Conns/peerMap above; the workspace is
	// not concurrently mutated across shells today.
	mountSubs map[string]*entitysdk.RawSubscription

	// OnConnAdded / OnConnRemoved, when non-nil, are invoked after
	// addConn / removeConn (remote connections only — the local peer
	// is added directly during NewShellWorkspace and does not fire
	// these). Embedding renderers wire these to persist the alias
	// binding under the workspace; shellcmd stays unaware of where
	// (or whether) the binding is stored.
	OnConnAdded   func(pc *PeerConn)
	OnConnRemoved func(alias string)
}

// NewShellWorkspace creates a workspace bound to the given in-process
// AppPeer. The peer is registered under localAlias; when empty it
// falls back to "self" (the term "local" is reserved for the local/*
// extension namespace). Identity is the identity name used when
// establishing new remote connections; empty means ephemeral.
func NewShellWorkspace(local *entitysdk.AppPeer, localAlias, identity string) *ShellWorkspace {
	if localAlias == "" {
		localAlias = "self"
	}
	localID := local.PeerID()
	pc := &PeerConn{
		Alias:  localAlias,
		Peer:   local,
		PeerID: localID,
	}
	return &ShellWorkspace{
		Local:     pc,
		Conns:     map[string]*PeerConn{localAlias: pc},
		peerMap:   map[string]string{localID: localAlias},
		Identity:  identity,
		mountSubs: map[string]*entitysdk.RawSubscription{},
	}
}

// registerMountSub records the source-prefix subscription for a mounted
// root so unmount can later cancel it. Overwrites any prior entry
// (remount of the same root name).
func (ws *ShellWorkspace) registerMountSub(rootName string, sub *entitysdk.RawSubscription) {
	if ws.mountSubs == nil {
		ws.mountSubs = map[string]*entitysdk.RawSubscription{}
	}
	ws.mountSubs[rootName] = sub
}

// closeMountSub cancels and forgets the subscription for a mounted
// root. Idempotent — a root with no recorded subscription is a no-op.
func (ws *ShellWorkspace) closeMountSub(rootName string) error {
	sub, ok := ws.mountSubs[rootName]
	if !ok || sub == nil {
		return nil
	}
	delete(ws.mountSubs, rootName)
	return sub.Close()
}

// AliasFor returns the alias bound to a peer-id, or empty string if
// no connection matches.
func (ws *ShellWorkspace) AliasFor(peerID string) string {
	return ws.peerMap[peerID]
}

// addConn registers a connection under the given alias and peer-id.
// Used by the connect command. Fires OnConnAdded (if set) so embedders
// can persist the binding.
func (ws *ShellWorkspace) addConn(pc *PeerConn) {
	ws.Conns[pc.Alias] = pc
	ws.peerMap[pc.PeerID] = pc.Alias
	if ws.OnConnAdded != nil {
		ws.OnConnAdded(pc)
	}
}

// removeConn unregisters a connection. The caller is responsible for
// closing the underlying connection if it's a remote peer. Fires
// OnConnRemoved (if set) so embedders can drop the persisted binding.
func (ws *ShellWorkspace) removeConn(alias string) *PeerConn {
	pc, ok := ws.Conns[alias]
	if !ok {
		return nil
	}
	delete(ws.Conns, alias)
	delete(ws.peerMap, pc.PeerID)
	if ws.OnConnRemoved != nil {
		ws.OnConnRemoved(alias)
	}
	return pc
}
