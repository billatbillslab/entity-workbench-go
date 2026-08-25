// Package shell hosts the standalone entity-shell binary's runtime
// glue: REPL loop, one-shot dispatch, and result formatting. The
// command vocabulary itself lives in the shellcmd package, and the
// peer + workspace bootstrap lives in shellboot — both shared with
// the canvas + console frontends so all three drive an aligned
// substrate (see PHASE-G-SHELL-CENTRIC-UI-PLAN.md).
package shell

import (
	"context"
	"fmt"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/shellboot"
	"entity-workbench-go/shellcmd"
)

// Config controls how a shell App is constructed. Identity / storage
// / listen / open-access flags map to shellboot.Config; JSON is the
// only entity-shell-only knob (REPL output is always text).
type Config struct {
	Identity    string
	LocalAlias  string
	JSON        bool
	StorageKind string
	StoragePath string
	ListenAddr  string
	OpenAccess  bool

	// DisableRegistry turns off the name-resolution substrate the `name`
	// verb dispatches to. Default (false) means ON — see
	// shellboot.Config.DisableRegistry for the measurement behind that.
	DisableRegistry bool

	// PeerConfig is forwarded raw to shellboot for tests that need a
	// pre-built keypair or extra peer options. Most main() callers
	// leave it zero and let shellboot derive everything from the
	// fields above.
	PeerConfig entitysdk.PeerConfig
}

// App is a configured shell instance: a local AppPeer plus the
// shellcmd.Shell that drives commands.
type App struct {
	cfg   Config
	local *entitysdk.AppPeer
	sh    *shellcmd.Shell
	reg   *shellcmd.Registry
}

// New bootstraps a local AppPeer and a fresh Shell session ready for
// either REPL or one-shot dispatch. Delegates the peer construction
// path to shellboot so all three frontends (entity-shell, canvas,
// console) share an identical substrate.
func New(cfg Config) (*App, error) {
	bootCfg := shellboot.Config{
		Identity:    cfg.Identity,
		LocalAlias:  cfg.LocalAlias,
		StorageKind: cfg.StorageKind,
		StoragePath: cfg.StoragePath,
		ListenAddr:  cfg.ListenAddr,
		OpenAccess:  cfg.OpenAccess,

		DisableRegistry: cfg.DisableRegistry,
	}
	// Honor PeerConfig.RawOptions for tests that pre-build options;
	// the rest of PeerConfig is reconstructed by shellboot from the
	// flag-shaped Config above.
	if len(cfg.PeerConfig.RawOptions) > 0 {
		bootCfg.ExtraPeerOptions = cfg.PeerConfig.RawOptions
	}
	// Test pre-built keypair / identity binding path: skip shellboot's
	// flag-driven setup and use entitysdk.CreatePeer directly. This
	// keeps the existing test surface working without complicating the
	// shellboot API.
	if cfg.PeerConfig.Keypair != nil || cfg.PeerConfig.Identity != nil {
		return newFromPeerConfig(cfg)
	}

	peer, ws, err := shellboot.Bootstrap(context.Background(), bootCfg)
	if err != nil {
		return nil, err
	}
	drainTreeEvents(peer)
	// Shared shell↔workspace integration. The standalone REPL has no
	// presentation context to publish into, so OnWDChanged stays nil;
	// alias persistence is the same as the canvas/console wiring.
	state := entitysdk.NewWorkspaceState(peer.Store())
	ws.PersistAliases(state)
	sh := shellcmd.NewShellInWorkspace(ws)
	return &App{
		cfg:   cfg,
		local: peer,
		sh:    sh,
		reg:   shellcmd.Default(),
	}, nil
}

// drainTreeEvents starts a no-op goroutine that pulls events off
// peer.TreeEvents() and discards them. peer.New always wires
// TreeEvents into the fanout, and after the core-go switch to
// blocking-fanout (no silent drops), any unread sink stalls the entire
// write path after the channel buffer (256) fills.
//
// canvas and console drain TreeEvents themselves to drive UI refresh.
// The standalone REPL has no UI to refresh, so a no-op drainer keeps
// the channel flowing.
//
// When peer is closed, TreeEvents() closes, the range loop exits, and
// the goroutine returns.
func drainTreeEvents(peer *entitysdk.AppPeer) {
	go func() {
		for range peer.TreeEvents() {
		}
	}()
}

// newFromPeerConfig is the test path: construct the peer directly
// from a pre-built PeerConfig (with a Keypair or IdentityBindingConfig
// the test owns) and wrap it in a workspace + Shell.
func newFromPeerConfig(cfg Config) (*App, error) {
	peerCfg := cfg.PeerConfig
	// Keep this path's substrate identical to the shellboot one. It is a
	// test-only door, and a test-only door that quietly lacks an extension
	// the shipped path has is how a feature passes its suite and fails in
	// the binary — the caller pre-builds a keypair, not a different product.
	// An explicit Registry setting on the forwarded PeerConfig wins.
	if !cfg.DisableRegistry && peerCfg.Extensions.Registry == nil {
		peerCfg.Extensions.Registry = &entitysdk.RegistryConfig{}
	}
	peer, err := entitysdk.CreatePeer(peerCfg)
	if err != nil {
		return nil, fmt.Errorf("create local peer: %w", err)
	}
	if !cfg.DisableRegistry {
		if _, err := peer.EnsureResolverConfig(); err != nil {
			_ = peer.Close()
			return nil, fmt.Errorf("resolver-config refused at load: %w", err)
		}
	}
	drainTreeEvents(peer)
	alias := cfg.LocalAlias
	if alias == "" {
		if cfg.Identity != "" {
			alias = cfg.Identity
		} else {
			alias = "self"
		}
	}
	ws := shellcmd.NewShellWorkspace(peer, alias, cfg.Identity)
	state := entitysdk.NewWorkspaceState(peer.Store())
	ws.PersistAliases(state)
	sh := shellcmd.NewShellInWorkspace(ws)
	return &App{
		cfg:   cfg,
		local: peer,
		sh:    sh,
		reg:   shellcmd.Default(),
	}, nil
}

// Close releases the local peer's resources.
func (a *App) Close() error {
	if a.local == nil {
		return nil
	}
	return a.local.Close()
}

// Shell returns the underlying shellcmd.Shell. Useful for tests.
func (a *App) Shell() *shellcmd.Shell { return a.sh }
