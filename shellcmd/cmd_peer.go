package shellcmd

import (
	"fmt"
	"sort"
)

// cmdPeer dispatches `peer <subcommand>` — the peer-management surface
// per GUIDE-SHELL-FRAMING.md Tier E. Complements `connect`/`disconnect`
// (lifecycle) and `info` (single-peer details) with list + rename.
//
// Subcommands:
//
//	peer ls                   — compact list of all known peers
//	peer info [alias]         — detailed info (delegates to cmdInfo)
//	peer rename <old> <new>   — change the alias bound to a peer
func cmdPeer(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: peer <ls|status|info|rename> [args]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return cmdPeerLs(sh, rest)
	case "info", "show":
		return cmdInfo(sh, rest)
	case "status":
		return cmdPeerStatus(sh, rest)
	case "rename", "mv":
		return cmdPeerRename(sh, rest)
	default:
		return Result{}, fmt.Errorf("unknown peer subcommand: %s", sub)
	}
}

// cmdPeerLs lists known peers in compact "alias  peer-id  addr" form.
// Local peer always appears first; remote peers follow in alias order.
func cmdPeerLs(sh *Shell, _ []string) (Result, error) {
	if sh.Local == nil {
		return MessageResult("(no local peer)"), nil
	}
	lines := []string{
		fmt.Sprintf("%-16s  %-14s  %s", "ALIAS", "PEER-ID", "ADDRESS"),
	}
	lines = append(lines, formatPeerRow(sh.Local, true))

	aliases := make([]string, 0, len(sh.Conns))
	for a := range sh.Conns {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	for _, a := range aliases {
		lines = append(lines, formatPeerRow(sh.Conns[a], false))
	}
	return LinesResult(lines), nil
}

func formatPeerRow(pc *PeerConn, local bool) string {
	short := pc.PeerID
	if len(short) > 14 {
		short = short[:14]
	}
	alias := pc.Alias
	if local {
		alias += " *"
	}
	addr := pc.Address
	if addr == "" {
		addr = "(local)"
	}
	return fmt.Sprintf("%-16s  %-14s  %s", alias, short, addr)
}

// cmdPeerRename retags a known peer with a new alias. The local peer
// alias and any reserved built-in pronouns are protected.
func cmdPeerRename(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: peer rename <old> <new>")
	}
	oldAlias := args[0]
	newAlias, err := NormalizeAlias(args[1])
	if err != nil {
		return Result{}, fmt.Errorf("peer rename: %w", err)
	}
	if IsReservedAlias(newAlias) {
		return Result{}, fmt.Errorf("peer rename: %q is reserved", newAlias)
	}
	if _, exists := sh.Conns[newAlias]; exists {
		return Result{}, fmt.Errorf("peer rename: %q already in use", newAlias)
	}

	pc, err := PeerRename(sh, oldAlias, newAlias)
	if err != nil {
		return Result{}, err
	}
	return MessageResult(fmt.Sprintf("renamed %s → %s (peer-id %s)", oldAlias, newAlias, shortID(pc.PeerID))), nil
}

// PeerRename is the exported verb-op (GUIDE-SHELL-FRAMING.md §8.1).
// Retags a known connection from oldAlias to newAlias. The local peer
// rename is allowed but updates sh.Local.Alias in place.
//
// Returns the updated PeerConn so callers can confirm the binding.
// Errors on unknown old alias.
func PeerRename(sh *Shell, oldAlias, newAlias string) (*PeerConn, error) {
	if sh.Local != nil && sh.Local.Alias == oldAlias {
		sh.Local.Alias = newAlias
		sh.peerMap[sh.Local.PeerID] = newAlias
		return sh.Local, nil
	}
	pc, ok := sh.Conns[oldAlias]
	if !ok {
		return nil, fmt.Errorf("peer rename: unknown alias %q", oldAlias)
	}
	delete(sh.Conns, oldAlias)
	delete(sh.peerMap, pc.PeerID)
	pc.Alias = newAlias
	sh.Conns[newAlias] = pc
	sh.peerMap[pc.PeerID] = newAlias
	return pc, nil
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "..."
}
