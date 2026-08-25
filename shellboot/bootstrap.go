// Package shellboot is the shared startup sequence for the three
// frontends that drive a shell-backed workspace: the standalone
// entity-shell binary, the canvas GUI, and the console TUI. Each
// frontend defines its own flag surface, then calls Bootstrap to
// build the AppPeer + ShellWorkspace; from there the frontends
// diverge (REPL loop vs window event loop vs tview event loop).
//
// The point is that there is exactly one place where peer
// construction, identity binding, workbench-handler registration,
// and Phase E mount reload live. Frontends that skip the shared
// path will silently miss extensions; that's the divergence
// PHASE-G-SHELL-CENTRIC-UI-PLAN.md set out to fix.
package shellboot

import (
	"context"
	"fmt"

	"go.entitychurch.org/entity-core-go/core/peer"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/shellcmd"
	"entity-workbench-go/workbench"
)

// Config is the renderer-neutral knobs every frontend exposes
// through its own flag surface. Stage 1 covers the substrate; extra
// frontend-specific config (window title, layout, JSON output) stays
// on the frontend side.
//
// JSON tags match the field names the bridge + Avalonia frontend
// serialize (legacy "alias" name kept for compat with the existing
// avalonia/frontend/Program.cs BridgeConfig serializer).
// ExtraPeerOptions is JSON-ignored (not serializable).
type Config struct {
	// Identity is the optional identity name bound to the peer. Empty
	// means an ephemeral keypair. Non-empty resolves the on-disk
	// identity bundle via entitysdk.IdentityBindingConfig.
	Identity string `json:"identity"`

	// LocalAlias is the alias under which the in-process peer is
	// registered in the shell workspace. Empty means: derive from
	// Identity when set, otherwise fall back to "self". "local" is
	// reserved for the local/* extension namespace and is rejected
	// as a peer alias.
	LocalAlias string `json:"alias"`

	// StorageKind selects the backing store. "" or "memory" is the
	// in-process default; "sqlite" persists to disk.
	StorageKind string `json:"storage"`

	// StoragePath is the SQLite path when StorageKind == "sqlite".
	// Empty + Identity set → derived as
	// ~/.entity/peers/{Identity}/store.db (GUIDE-PERSISTENCE §1.1).
	// Use ":memory:" for an in-process SQL DB. Required for
	// StorageKind == "sqlite" when Identity is empty.
	StoragePath string `json:"storage_path"`

	// ListenAddr is the inbound TCP listener address (e.g.
	// "127.0.0.1:9100"). Empty means outbound-only.
	ListenAddr string `json:"listen"`

	// AdvertiseURL is the dial address published as this peer's
	// transport profile (EXTENSION-NETWORK §6.5.1a D1 self-publication)
	// once the listener binds. Empty means "derive it from ListenAddr",
	// which works whenever the bind host is concrete and is skipped
	// with HostedPeer.AdvertiseErr when it is a wildcard — a listener
	// binds 0.0.0.0, but a profile carries what a peer DIALS.
	//
	// Set it explicitly whenever the routable address differs from the
	// bound one: a LAN IP behind a 0.0.0.0 bind, a hostname, a
	// reverse-proxied wss:// URL.
	AdvertiseURL string `json:"advertise"`

	// OpenAccess, when true, grants every connecting peer wildcard
	// capabilities. Development use only — production peers should
	// configure scoped grants via the role extension. Required for
	// the prototype multi-peer flows in USAGE-PROTOTYPE-FILESYSTEM-SYNC.md.
	OpenAccess bool `json:"open_access"`

	// ExtraPeerOptions forwards raw peer options to entitysdk for
	// frontend-specific tuning (e.g. additional handlers, sync hooks).
	// Use sparingly; most knobs belong in Config above.
	ExtraPeerOptions []peer.Option `json:"-"`
}

// Bootstrap builds the AppPeer + ShellWorkspace from a Config. The
// caller owns the peer's lifecycle; defer (*entitysdk.AppPeer).Close
// before exiting.
//
// The bootstrap responsibilities are deliberately concentrated here:
//
//  1. Derive the SQLite path from Identity when StorageKind=sqlite and
//     StoragePath is empty.
//  2. Create the storage directory if missing.
//  3. Resolve Identity into the peer-config identity binding.
//  4. Register the workbench handlers (notification-ingest,
//     chain-errors, revision-converge) so `mount` and `revision follow`
//     work.
//  5. Construct the AppPeer via entitysdk.CreatePeer.
//  6. Call localfiles.Engine.Load to re-start any persisted Phase E
//     mounts (restart-equivalence).
//  7. Wire the converge handler's AppPeer ref.
//  8. Construct the ShellWorkspace and stash the handler refs on it.
//
// Skipping any of these on the frontend side leaves a measurable
// feature gap (Phase E mounts don't reload, `revision follow` is
// broken, etc.). That's why all three frontends share this path.
func Bootstrap(ctx context.Context, cfg Config) (*entitysdk.AppPeer, *shellcmd.ShellWorkspace, error) {
	if cfg.LocalAlias == "" {
		if cfg.Identity != "" {
			cfg.LocalAlias = cfg.Identity
		} else {
			cfg.LocalAlias = "self"
		}
	}

	// SQLite path derivation: when -storage=sqlite and -storage-path
	// is empty, derive ~/.entity/peers/{Identity}/store.db per
	// GUIDE-PERSISTENCE §1.1. Identity is required for the derivation;
	// without it the caller must supply an explicit path.
	resolvedStoragePath := cfg.StoragePath
	if cfg.StorageKind == "sqlite" && resolvedStoragePath == "" {
		if cfg.Identity == "" {
			return nil, nil, fmt.Errorf("shellboot: storage=sqlite requires storage-path or identity (to derive ~/.entity/peers/{NAME}/store.db)")
		}
		p, err := entitysdk.DefaultPeerStoragePath(cfg.Identity)
		if err != nil {
			return nil, nil, fmt.Errorf("shellboot: resolve storage path: %w", err)
		}
		resolvedStoragePath = p
	}
	if err := entitysdk.EnsurePeerStorageDir(resolvedStoragePath); err != nil {
		return nil, nil, fmt.Errorf("shellboot: prepare storage dir: %w", err)
	}

	peerCfg := entitysdk.PeerConfig{
		Storage:    entitysdk.StorageConfig{Kind: cfg.StorageKind, Path: resolvedStoragePath},
		ListenAddr: cfg.ListenAddr,
	}
	if cfg.Identity != "" {
		peerCfg.Identity = &entitysdk.IdentityBindingConfig{Name: cfg.Identity}
	}
	if cfg.OpenAccess {
		peerCfg.RawOptions = append(peerCfg.RawOptions,
			peer.WithConnectionGrants(peer.OpenAccessGrants()))
	}
	if len(cfg.ExtraPeerOptions) > 0 {
		peerCfg.RawOptions = append(peerCfg.RawOptions, cfg.ExtraPeerOptions...)
	}

	// Wire workbench handlers needed for Phase E mounts + chain-errors
	// observability. These must be registered at peer-construction
	// time because PeerConfig.Handlers is consumed inside CreatePeer's
	// option list. Revision-follow convergence used to live here too
	// as `workbench.RevisionConvergeHandler`; it retired
	// once `revision:pull` (REVISION §4.4.8) landed in core-go — the
	// follow chain `subscribe head → revision:pull` now expresses the
	// same orchestration declaratively, with no workbench-internal
	// handler.
	ingestHandler := workbench.NewNotificationIngestHandler(nil)
	peerCfg.Handlers = append(peerCfg.Handlers,
		entitysdk.HandlerRegistration{
			Pattern: workbench.NotificationIngestPattern,
			Handler: ingestHandler,
		},
		entitysdk.HandlerRegistration{
			Pattern: workbench.ChainErrorsPattern,
			Handler: workbench.NewChainErrorsHandler(),
		},
	)

	ap, err := entitysdk.CreatePeer(peerCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("shellboot: create peer: %w", err)
	}

	// Restart-equivalence for Phase E mounts: walk the persisted
	// local-files config and re-start watchers. The localfiles
	// handler's own Load() handles this; we call it after
	// CreatePeer returns so the store/index/identity hash are
	// available.
	if lfh := ap.LocalFilesHandler(); lfh != nil {
		if err := lfh.Load(ctx, ap.RawContentStore(), ap.RawLocationIndex(), ap.IdentityHash()); err != nil {
			_ = ap.Close()
			return nil, nil, fmt.Errorf("shellboot: reload local-files mounts: %w", err)
		}
	}

	ws := shellcmd.NewShellWorkspace(ap, cfg.LocalAlias, cfg.Identity)
	ws.NotificationIngest = ingestHandler

	return ap, ws, nil
}
