// peer_manager.go — renderer-agnostic multi-peer registry.
//
// PeerManager owns the in-process peer set for a workbench-go
// application. First peer added is the system peer (it owns the
// application-state namespace + the roster); subsequent peers are
// ordinary roster entries. Concurrency-safe; methods are callable
// from any goroutine.
//
// Per PHASE-I-MULTI-PEER-PLAN.md §12 — moved from avalonia/bridge/
// where it shouldn't have been, into shellboot/ where the shared
// peer-construction logic already lives. Avalonia + console both
// construct a PeerManager and consume the same surface; renderer-
// specific resource cleanup (cgo tree+watch handle maps for the
// bridge, tview model wrappers for console) hooks in via
// OnPeerDestroyed.
//
// Compare with godot core/app_state.gd (SIBLING-FRONTEND-SURVEY §3.1) —
// same shape: dict keyed by peer-id-equivalent, system peer slot,
// signals/hooks for add/remove.

package shellboot

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/shellcmd"
)

// HostedPeer is the public per-peer struct callers receive from
// PeerManager. The struct is a snapshot — callers should not mutate
// fields; for live state, call back through the manager.
//
// Fields:
//
//	Handle    — opaque int64 assigned by the manager at Create time.
//	            Stable for the peer's lifetime.
//	AppPeer   — the entitysdk peer (Store, PeerContext, Put/Get/...).
//	Workspace — the shellcmd workspace owning the local-peer alias
//	            + remote-peer connection pool.
//	Shell     — a pre-constructed shellcmd.Shell, WD set to the
//	            peer's canonical root path.
//	Config    — the shellboot.Config the peer was Create'd with.
//	AddedAt   — unix-millis timestamp of Create.
//	IsSystem  — true iff this peer is the manager's system peer at
//	            the snapshot moment (system handle can shift on
//	            Destroy + handover).
type HostedPeer struct {
	Handle    int64
	AppPeer   *entitysdk.AppPeer
	Workspace *shellcmd.ShellWorkspace
	Shell     *shellcmd.Shell
	Config    Config
	AddedAt   int64
	IsSystem  bool

	// ListenScheme is "tcp" or "ws" when Config.ListenAddr is set and
	// the listener bound successfully. Empty when the peer is
	// outbound-only or the listen attempt failed. Set at Create time;
	// stable for the peer's lifetime.
	ListenScheme string

	// AdvertisedURL is the dial address self-published as this peer's
	// transport profile once the listener bound, or empty when nothing
	// was advertised. AdvertiseErr carries why, when the reason is not
	// simply "no listener": it is NOT fatal to Create, because a peer
	// that listens but does not advertise is exactly the pre-2026-08-18
	// behaviour — reachable by anyone told out-of-band, invisible to
	// anyone who was not.
	AdvertisedURL string
	AdvertiseErr  error

	// listenCancel cancels the auto-Listen goroutine. nil for
	// outbound-only peers. Called during Destroy, before AppPeer.Close.
	listenCancel func()
}

// PeerManager is the per-process multi-peer registry. One per
// frontend; not a global. Concurrent-safe.
type PeerManager struct {
	appID string

	mu           sync.Mutex
	peers        map[int64]*HostedPeer
	counter      int64
	systemHandle int64

	// onDestroyHooks fire during Destroy AFTER the peer is removed
	// from the registry but BEFORE its AppPeer is closed. Renderers
	// use this to cascade-clean their own resources (bridge: trees +
	// watches keyed by peer-handle; console: tview model wrappers).
	//
	// Hooks fire WITHOUT the manager's mutex held — they can safely
	// call back into the manager.
	onDestroyHooks []func(handle int64)
}

// NewPeerManager constructs an empty manager for app-id appID
// (e.g. "entity-workbench"). The app-id namespaces every roster
// entry under the system peer's tree at app/{appID}/system/peers/.
func NewPeerManager(appID string) *PeerManager {
	return &PeerManager{
		appID: appID,
		peers: map[int64]*HostedPeer{},
	}
}

// AppID returns the namespace the manager writes roster entries
// under.
func (m *PeerManager) AppID() string { return m.appID }

// Create boots a new peer per cfg and registers it. The first
// successful Create establishes the system peer (handle 1 if no
// prior Creates have happened).
//
// Returns the assigned handle. On Bootstrap failure returns
// (0, error). On roster-write failure returns (handle, error) —
// the peer is live in-memory; only the durable mirror failed.
// Callers that want strict success should treat (handle != 0, err)
// as a warning.
func (m *PeerManager) Create(cfg Config) (int64, error) {
	ap, ws, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		return 0, fmt.Errorf("shellboot.Bootstrap: %w", err)
	}

	sh := shellcmd.NewShellInWorkspace(ws)
	// Pre-cd to the local peer's canonical root so relative paths
	// work without a leading `cd @<alias>`. WD stays in canonical
	// peer-id form (`/{peerID}/`); alias substitution happens at
	// display time. Per feedback-shell-wd-canonical-form: never
	// store the alias-display form here.
	sh.SetWD(shellcmd.Path("/" + ws.Local.PeerID + "/"))

	h := atomic.AddInt64(&m.counter, 1)
	now := time.Now().UnixMilli()

	hp := &HostedPeer{
		Handle:    h,
		AppPeer:   ap,
		Workspace: ws,
		Shell:     sh,
		Config:    cfg,
		AddedAt:   now,
	}

	// Auto-Listen when ListenAddr is configured. Without this, every
	// frontend (avalonia/shell/console) is outbound-only by default —
	// the PeerConfig.ListenAddr is plumbed through to peer.WithListenAddr
	// at construction but nothing calls Listen unless we do it here.
	//
	// Scheme routing matches AppPeer.Connect: ws://+wss:// routes to
	// ListenWebSocketReady; bare host:port and tcp:// route to ListenReady.
	if cfg.ListenAddr != "" {
		listenCtx, cancel := context.WithCancel(context.Background())
		ready := make(chan struct{})
		listenErrCh := make(chan error, 1)
		scheme, bindAddr, wsPath, parseErr := parseListenAddr(cfg.ListenAddr)
		if parseErr != nil {
			cancel()
			_ = ap.Close()
			return 0, fmt.Errorf("shellboot: parse listen addr %q: %w", cfg.ListenAddr, parseErr)
		}
		go func() {
			if scheme == "ws" {
				listenErrCh <- ap.ListenWebSocketReady(listenCtx, bindAddr, wsPath, ready)
			} else {
				listenErrCh <- ap.ListenReady(listenCtx, ready)
			}
		}()
		// Wait until the listener binds or the goroutine errors out. A
		// 5s ceiling — well above a kernel ephemeral-port bind — turns
		// a hung listener into a surfaced error instead of a phantom
		// hang during Create.
		select {
		case <-ready:
			hp.ListenScheme = scheme
			hp.listenCancel = cancel
			// Self-publish now that the address is real. §6.5.1a D1 is a
			// SHOULD, and until this existed nothing in this tree ever
			// told another peer how to reach us — we published profiles
			// only for peers WE dialed. A browser peer cannot learn that
			// a Go peer accepts a WebSocket any other way.
			hp.AdvertisedURL, hp.AdvertiseErr = advertiseListener(ap, cfg.AdvertiseURL, scheme, bindAddr, wsPath)
		case err := <-listenErrCh:
			cancel()
			_ = ap.Close()
			return 0, fmt.Errorf("shellboot: listen on %q: %w", cfg.ListenAddr, err)
		case <-time.After(5 * time.Second):
			cancel()
			_ = ap.Close()
			return 0, fmt.Errorf("shellboot: listen on %q timed out after 5s", cfg.ListenAddr)
		}

		// Auto-Announce on the listener's scheme so LAN peers can find
		// this peer via mDNS. Failure here is non-fatal — the peer is
		// already live and reachable by direct address; we just lose
		// auto-discovery. Most likely cause: no multicast-capable
		// interface (CI runners, isolated network namespaces).
		if hp.ListenScheme != "" {
			announceCtx, announceCancel := context.WithTimeout(
				context.Background(), 2*time.Second)
			if err := ap.Announce(announceCtx, hp.ListenScheme); err != nil {
				// Drop the error — non-fatal. A future iteration could
				// surface this via a HostedPeer.AnnounceErr field for
				// the panel to display.
				_ = err
			}
			announceCancel()
		}
	}

	var systemPeerForRoster *entitysdk.AppPeer
	m.mu.Lock()
	m.peers[h] = hp
	firstPeer := m.systemHandle == 0
	if firstPeer {
		m.systemHandle = h
	}
	if sp, ok := m.peers[m.systemHandle]; ok {
		systemPeerForRoster = sp.AppPeer
	}
	m.mu.Unlock()

	// Write the roster entry under the system peer's tree. Done with
	// the manager mutex released — tree writes hit dispatch + could
	// otherwise deadlock if a hook fired during the write tried to
	// re-enter the manager.
	if systemPeerForRoster != nil {
		entry := RosterEntry{
			PeerID:      ap.PeerID(),
			Label:       ws.Local.Alias,
			AddedAt:     now,
			IsFavorite:  false,
			Identity:    cfg.Identity,
			StorageKind: cfg.StorageKind,
			StoragePath: cfg.StoragePath,
			ListenAddr:  cfg.ListenAddr,
		}
		if werr := WriteRosterEntry(systemPeerForRoster, m.appID, entry); werr != nil {
			return h, fmt.Errorf("peer created (handle %d) but roster write failed: %w", h, werr)
		}
	}
	return h, nil
}

// Destroy tears down peer h. Cascade order:
//  1. Remove from in-memory registry; demote system-peer if it was h.
//  2. Promote a replacement system peer if any survivors remain.
//  3. Fire OnPeerDestroyed hooks (renderer cascades own resources).
//  4. Remove the roster entry from the (possibly-new) system peer's tree.
//  5. Close the AppPeer.
//
// Idempotent — unknown handles return nil.
func (m *PeerManager) Destroy(h int64) error {
	m.mu.Lock()
	hp, ok := m.peers[h]
	if ok {
		delete(m.peers, h)
	}
	wasSystem := h == m.systemHandle
	if wasSystem {
		m.systemHandle = 0
		// Map iter order is random; at single-survivor scope this is
		// fine. Multi-survivor handover policy can be made
		// deterministic if a use case demands it.
		for id := range m.peers {
			m.systemHandle = id
			break
		}
	}
	hooks := append([]func(int64){}, m.onDestroyHooks...)
	var newSystemPeer *entitysdk.AppPeer
	if sp, ok := m.peers[m.systemHandle]; ok {
		newSystemPeer = sp.AppPeer
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}

	// Fire cascade hooks BEFORE closing the AppPeer — renderers
	// touching the peer's store during cleanup need it live.
	for _, cb := range hooks {
		cb(h)
	}

	// Cancel the auto-Listen goroutine if one is running. AppPeer.Close
	// also tears down the listener, but cancelling first lets the
	// listen goroutine return cleanly (avoids the "use of closed
	// network connection" log spam).
	if hp.listenCancel != nil {
		hp.listenCancel()
	}

	// Remove the roster entry. If the destroyed peer WAS the system
	// peer and there's no replacement, skip (the tree we'd write to
	// is going away anyway). If there IS a replacement, the old roster
	// entries currently live on the dying peer's tree — that's a known
	// limitation (the new system peer doesn't inherit them in-memory
	// here; RestoreFromRoster on next boot reconstructs them).
	if !wasSystem && newSystemPeer != nil {
		_ = RemoveRosterEntry(newSystemPeer, m.appID, hp.AppPeer.PeerID())
	}

	if hp.AppPeer != nil {
		_ = hp.AppPeer.Close()
	}
	return nil
}

// Get returns a snapshot of peer h, or nil if h is 0 / unknown.
// The returned pointer is a fresh allocation; mutating it doesn't
// affect the manager's state. AppPeer / Workspace / Shell pointers
// inside the struct point to the live underlying objects — those
// remain valid until Destroy(h) is called.
func (m *PeerManager) Get(h int64) *HostedPeer {
	if h == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hp, ok := m.peers[h]
	if !ok {
		return nil
	}
	out := *hp
	out.IsSystem = hp.Handle == m.systemHandle
	return &out
}

// List returns a snapshot of every live peer. Order is map-iteration
// (unstable); callers sort if they want a presentation order.
func (m *PeerManager) List() []*HostedPeer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*HostedPeer, 0, len(m.peers))
	for _, hp := range m.peers {
		copy := *hp
		copy.IsSystem = hp.Handle == m.systemHandle
		out = append(out, &copy)
	}
	return out
}

// SystemPeer returns the current system peer (snapshot) or nil.
func (m *PeerManager) SystemPeer() *HostedPeer {
	return m.Get(m.SystemHandle())
}

// SystemHandle returns the current system-peer handle (0 if empty).
func (m *PeerManager) SystemHandle() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.systemHandle
}

// ShutdownAll destroys every peer. Call before process exit.
func (m *PeerManager) ShutdownAll() {
	m.mu.Lock()
	handles := make([]int64, 0, len(m.peers))
	for id := range m.peers {
		handles = append(handles, id)
	}
	m.mu.Unlock()
	for _, h := range handles {
		_ = m.Destroy(h)
	}
}

// OnPeerDestroyed appends a cleanup hook fired during Destroy after
// the peer is unregistered + system-handover happens but BEFORE its
// AppPeer is closed. Hooks fire serially in registration order. The
// manager's mutex is NOT held during hook execution.
//
// Renderer-specific resource registries (cgo tree+watch maps in the
// bridge; tview model wrappers in console) register here so a
// PeerDestroy call cleanly tears down everything attached to the peer.
func (m *PeerManager) OnPeerDestroyed(cb func(handle int64)) {
	if cb == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onDestroyHooks = append(m.onDestroyHooks, cb)
}

// RestoreFromRoster reads the system peer's roster and respawns
// every non-ephemeral peer that isn't already hosted. Filters:
//   - the system peer itself (already up; that's our reader)
//   - ephemeral peers (Identity == "" — keypair can't be reloaded)
//   - already-hosted peer-ids (idempotent retry)
//
// Returns the handles of the newly-spawned peers plus any error
// encountered during enumeration or per-entry spawn. A per-entry
// failure does NOT abort the rest — successful spawns appear in
// handles; the err carries the first failure with a count.
func (m *PeerManager) RestoreFromRoster() ([]int64, error) {
	sp := m.SystemPeer()
	if sp == nil {
		return nil, fmt.Errorf("RestoreFromRoster: no system peer (call Create first)")
	}
	entries, err := ListRosterEntries(sp.AppPeer, m.appID)
	if err != nil {
		return nil, fmt.Errorf("RestoreFromRoster: %w", err)
	}
	systemPeerID := sp.AppPeer.PeerID()
	alreadyHosted := m.liveHostedPeerIDs()

	handles := make([]int64, 0, len(entries))
	failures := 0
	var firstErr error
	for _, e := range entries {
		if e.PeerID == systemPeerID {
			continue
		}
		if e.Identity == "" {
			continue
		}
		if _, dup := alreadyHosted[e.PeerID]; dup {
			continue
		}
		cfg := Config{
			Identity:    e.Identity,
			LocalAlias:  e.Label,
			StorageKind: e.StorageKind,
			StoragePath: e.StoragePath,
			ListenAddr:  e.ListenAddr,
			// OpenAccess isn't persisted — defaults to false on
			// restore. Re-enable through a future config UI if needed.
		}
		h, cerr := m.Create(cfg)
		if cerr != nil && h == 0 {
			failures++
			if firstErr == nil {
				firstErr = cerr
			}
			continue
		}
		handles = append(handles, h)
	}
	if failures > 0 {
		return handles, fmt.Errorf("RestoreFromRoster: %d of %d entries failed (first: %w)",
			failures, len(entries), firstErr)
	}
	return handles, nil
}

// parseListenAddr classifies a Config.ListenAddr into a transport
// scheme + a bind address suitable for the matching listener call.
// Three accepted shapes:
//
//   - "host:port"            → ("tcp", "host:port", "")
//   - "tcp://host:port"      → ("tcp", "host:port", "")
//   - "ws://host:port[/path]" → ("ws", "host:port", "/path"; default "/ws")
//   - "wss://host:port[/path]" → ("ws", "host:port", "/path"; default "/ws")
//
// wss:// downgrades to ws-with-no-TLS for now — production wss is
// expected to terminate TLS at a reverse proxy per
// peer.ListenWebSocketReady's doc.
func parseListenAddr(raw string) (scheme, bindAddr, wsPath string, err error) {
	switch {
	case strings.HasPrefix(raw, "ws://"), strings.HasPrefix(raw, "wss://"):
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", "", "", perr
		}
		if u.Host == "" {
			return "", "", "", fmt.Errorf("missing host:port in %q", raw)
		}
		path := u.Path
		if path == "" {
			path = "/ws"
		}
		return "ws", u.Host, path, nil
	case strings.HasPrefix(raw, "tcp://"):
		return "tcp", strings.TrimPrefix(raw, "tcp://"), "", nil
	default:
		return "tcp", raw, "", nil
	}
}

// liveHostedPeerIDs returns the set of peer-ids currently in the
// in-memory registry. Internal — RestoreFromRoster uses it for dedup.
func (m *PeerManager) liveHostedPeerIDs() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]struct{}, len(m.peers))
	for _, hp := range m.peers {
		out[hp.AppPeer.PeerID()] = struct{}{}
	}
	return out
}

// advertiseListener self-publishes the peer's transport profile for the
// listener that just bound, returning the URL actually advertised.
//
// Explicit AdvertiseURL wins. Otherwise the dial URL is derived from
// the bind address, which is correct exactly when the bind host is
// concrete — `-listen 127.0.0.1:9100` is a real address a peer on the
// same machine dials, and that is the first browser↔Go loop we expect
// to run. A wildcard bind has no derivation and returns an error
// instead of publishing a profile nobody can use; the caller surfaces
// it rather than failing Create.
func advertiseListener(ap *entitysdk.AppPeer, advertiseURL, scheme, bindAddr, wsPath string) (string, error) {
	dialURL := advertiseURL
	if dialURL == "" {
		switch scheme {
		case "ws":
			dialURL = "ws://" + bindAddr + wsPath
		default:
			dialURL = "tcp://" + bindAddr
		}
	}
	if err := ap.AdvertiseTransport(dialURL); err != nil {
		return "", fmt.Errorf("shellboot: advertise %q: %w", dialURL, err)
	}
	return dialURL, nil
}
