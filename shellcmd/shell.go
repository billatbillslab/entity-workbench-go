package shellcmd

import (
	"strings"

	"entity-workbench-go/entitysdk"
)

// PeerConn represents one peer the shell can target. It bundles the
// AppPeer (always the local in-process peer that the shell owns) with
// per-connection metadata (alias, address). For the local peer,
// Address is empty.
//
// Remote peers are added via the local peer's Connect; the resulting
// entitysdk.Connection is held inside AppPeer's connection pool, so
// from the shell's perspective the AppPeer is the single dispatch
// surface — operations targeting a remote peer-id route through the
// pool automatically.
type PeerConn struct {
	Alias   string
	Address string // empty for the local peer
	PeerID  string
	Peer    *entitysdk.AppPeer
}

// Shell is the per-panel state of an active shell session. It pairs a
// per-session working directory with a shared ShellWorkspace that
// holds the AppPeer, alias table, identity, and workbench handler
// references.
//
// A Shell is not safe for concurrent use — it expects a single
// goroutine driving commands (REPL loop, panel input loop, or
// one-shot dispatch). Multi-panel deployments rely on each renderer
// having its own event loop; see ShellWorkspace's concurrency note.
type Shell struct {
	// ShellWorkspace is embedded so existing access patterns
	// (sh.Local, sh.Conns, sh.Identity, sh.NotificationIngest, etc.)
	// continue to read/write workspace-level state directly. New
	// callers should prefer the explicit accessor when they want to
	// document workspace-level intent.
	*ShellWorkspace

	// WD is the per-shell current working directory. Initial value: "/".
	// Production code should mutate via SetWD so subscribers see changes;
	// tests that intentionally bypass the hook may assign directly.
	WD Path

	// OnWDChanged, when non-nil, is invoked from SetWD with the previous
	// and new working directory. Conceptually a `navigate` action per
	// GUIDE-ENTITY-WORKBENCH-APP.md §5.3 — the shell is announcing it
	// is now attending to a new path. Embedding panels wire this to
	// publish the WD into the presentation-context selection slot
	// (PropContext default for navigate). The standalone REPL leaves
	// this nil.
	OnWDChanged func(prev, next Path)
}

// SetWD updates the working directory and fires OnWDChanged (if set).
// Fires on every call, including no-op `cd .` — subscribers that want
// to dedupe can compare prev and next.
func (sh *Shell) SetWD(next Path) {
	prev := sh.WD
	sh.WD = next
	if sh.OnWDChanged != nil {
		sh.OnWDChanged(prev, next)
	}
}

// NewShell creates a new shell session and a fresh workspace bound to
// the given AppPeer. This is the single-shell convenience for the
// standalone entity-shell binary and for tests; multi-shell callers
// (canvas, console) create one ShellWorkspace then call
// NewShellInWorkspace per panel.
func NewShell(local *entitysdk.AppPeer, localAlias, identity string) *Shell {
	ws := NewShellWorkspace(local, localAlias, identity)
	return NewShellInWorkspace(ws)
}

// NewShellInWorkspace creates a new shell session over an existing
// workspace. Each call produces a Shell with its own working directory
// and (downstream) its own command history + output; the workspace's
// Local/Conns/peerMap/Identity/handlers are shared.
func NewShellInWorkspace(ws *ShellWorkspace) *Shell {
	return &Shell{
		ShellWorkspace: ws,
		WD:             "/",
	}
}

// ConnForWD returns the PeerConn for the current working directory,
// or nil if at root.
func (sh *Shell) ConnForWD() *PeerConn {
	return sh.connForPath(sh.WD)
}

// ConnForPath returns the PeerConn for a path's peer-id, or nil if
// the path is at root or no connection matches the peer-id.
func (sh *Shell) ConnForPath(p Path) *PeerConn {
	return sh.connForPath(p)
}

func (sh *Shell) connForPath(p Path) *PeerConn {
	peerID := p.PeerID()
	if peerID == "" {
		return nil
	}
	alias, ok := sh.peerMap[peerID]
	if !ok {
		return nil
	}
	return sh.Conns[alias]
}

// Resolve interprets a user-typed path against the shell's working
// directory and alias table. It extends package-level Resolve with
// alias support, so users never have to type a peer-id literal.
//
// Per GUIDE-SHELL-FRAMING.md §3.4, the canonical alias form is
// "@alias" as a peer-id substitution sigil:
//
//   - "@alice"                → /{alice_peer_id}/
//   - "@alice/foo/bar"        → /{alice_peer_id}/foo/bar
//   - "/@alice/foo/bar"       → /{alice_peer_id}/foo/bar (canonical)
//   - "/alice/foo/bar"        → /{alice_peer_id}/foo/bar (legacy
//     absolute-with-alias-as-first-segment;
//     retained because @alias resolves to
//     the peer-id and we accept the
//     already-resolved form too)
//   - "alias:..." (deprecated)→ /{peer_id}/... — still accepted for
//     one release; users should migrate
//     to "@alias/..."
//   - everything else         → Resolve(input, sh.WD)
//
// Aliases that don't match a connection fall through, leaving the
// literal segment in place — typos surface downstream as "no
// connection for path …" rather than silently aliasing.
func (sh *Shell) Resolve(input string) Path {
	// Canonical "@alias" forms (current).
	if alias, rest, ok := splitAtSigil(input); ok {
		return Path(sh.aliasExpansion(alias, rest))
	}
	// Deprecated "alias:" form (accepted for one release; will be
	// removed in the next minor version).
	if alias, rest, ok := splitAliasPrefix(input); ok {
		return Path(sh.aliasExpansion(alias, rest))
	}
	resolved := Resolve(input, sh.WD)
	return sh.expandAliasInPath(resolved)
}

// aliasExpansion looks up an alias and builds the resulting absolute
// path. If the alias is unknown, the alias literal is preserved as
// the first path segment — downstream surfaces a clean "no
// connection for path …" rather than silently producing a bad path.
// "self" is a built-in pronoun: always resolves to the in-process
// peer regardless of its primary alias.
func (sh *Shell) aliasExpansion(alias, rest string) string {
	segment := alias
	if alias == "self" && sh.Local != nil {
		segment = sh.Local.PeerID
	} else if pc, found := sh.Conns[alias]; found {
		segment = pc.PeerID
	}
	if rest == "" {
		return "/" + segment + "/"
	}
	return "/" + segment + "/" + rest
}

// DisplayWD returns the working directory in user-friendly form by
// reverse-resolving the peer-id against the alias table. If an alias
// is bound to the WD's peer-id, the path is rendered as
// "/@{alias}/{bare-path}" (the canonical absolute-with-alias form per
// GUIDE-SHELL-FRAMING.md §6.5). Otherwise, the resolved-form WD is
// returned as-is.
//
// WD storage is always the resolved form (see SetWD); reverse-resolution
// at display time keeps dispatch deterministic while presenting a
// readable form to the user. If the alias table changes after `cd`,
// the display reflects the current alias state rather than a stale
// snapshot — which is the intended behavior.
//
// When multiple aliases could map to the same peer-id, the workspace's
// peerMap holds only the most-recently-added binding; the display
// uses whatever the peerMap reports. (peer-rename/re-add semantics
// are governed by addConn/removeConn in workspace.go.)
func (sh *Shell) DisplayWD() Path {
	wd := sh.WD
	if wd.IsRoot() {
		return wd
	}
	peerID := wd.PeerID()
	if peerID == "" {
		return wd
	}
	// Local peer: render with its primary alias (typically "local") if set.
	var alias string
	if sh.Local != nil && peerID == sh.Local.PeerID && sh.Local.Alias != "" {
		alias = sh.Local.Alias
	} else {
		alias = sh.AliasFor(peerID)
	}
	if alias == "" {
		return wd
	}
	bare := wd.BarePath()
	trailingSlash := strings.HasSuffix(string(wd), "/")
	out := "/@" + alias
	if bare != "" {
		out += "/" + bare
	}
	if trailingSlash && !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return Path(out)
}

// expandAliasInPath swaps an alias name in the first path segment for
// the corresponding peer-id. Leaves non-alias paths untouched.
// "self" is a built-in pronoun: always expands to the in-process peer.
func (sh *Shell) expandAliasInPath(p Path) Path {
	if p.IsRoot() {
		return p
	}
	first := p.PeerID()
	var pc *PeerConn
	if first == "self" && sh.Local != nil {
		pc = sh.Local
	} else {
		var found bool
		pc, found = sh.Conns[first]
		if !found {
			return p
		}
	}
	bare := p.BarePath()
	trailingSlash := strings.HasSuffix(string(p), "/")
	out := "/" + pc.PeerID
	if bare != "" {
		out += "/" + bare
	}
	if trailingSlash && !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return Path(out)
}

// splitAtSigil recognizes the canonical @alias forms per
// GUIDE-SHELL-FRAMING.md §3.4. Returns (alias, rest, true) for:
//
//   - "@alice"           → ("alice", "",        true)
//   - "@alice/foo/bar"   → ("alice", "foo/bar", true)
//   - "/@alice"          → ("alice", "",        true)  (canonical absolute)
//   - "/@alice/foo/bar"  → ("alice", "foo/bar", true)
//
// The shortcut form "@alice/..." is recognized as equivalent to
// "/@alice/..." per §3.4's "input ergonomic" framing. Returns
// ("", "", false) when the input does not start with @ or /@, or
// when the alias name is empty.
//
// Note: this function only recognizes the structural shape; it does
// not validate the alias content. Validation happens at write time
// via NormalizeAlias (see alias.go). At read time we want unknown
// aliases to pass through rather than reject.
func splitAtSigil(input string) (alias, rest string, ok bool) {
	body := input
	switch {
	case strings.HasPrefix(input, "/@"):
		body = input[2:]
	case strings.HasPrefix(input, "@"):
		body = input[1:]
	default:
		return "", "", false
	}
	if body == "" {
		return "", "", false
	}
	if idx := strings.IndexByte(body, '/'); idx >= 0 {
		alias = body[:idx]
		rest = body[idx+1:]
	} else {
		alias = body
		rest = ""
	}
	if alias == "" {
		return "", "", false
	}
	return alias, rest, true
}

// splitAliasPrefix recognizes the deprecated "alias:..." form.
// Returns (alias, rest, true) when input has the form "alias:..."
// with alias non-empty and not containing a "/" or further ":".
// Returns ("", "", false) otherwise.
//
// Deprecated: per GUIDE-SHELL-FRAMING.md §3.4 the canonical form is
// "@alias/..." (see splitAtSigil). This parser is retained for one
// release to ease user migration; remove after the deprecation
// window closes. Note that ":" is reserved for the protocol's
// "<handler-path>:<op>" op-naming convention, and entity names MAY
// contain ":" (per §3.4 grammatical-not-character-level reservation),
// so this function intentionally only triggers on ":" before any "/"
// — entity paths like "/local/system/handler:register" pass through.
func splitAliasPrefix(input string) (alias, rest string, ok bool) {
	idx := strings.IndexByte(input, ':')
	if idx <= 0 {
		return "", "", false
	}
	alias = input[:idx]
	if strings.ContainsRune(alias, '/') {
		return "", "", false
	}
	rest = input[idx+1:]
	return alias, rest, true
}
