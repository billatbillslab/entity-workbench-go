package shellcmd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/ecf"
)

// cmdLs implements `ls [path]`. At root (no peer in the path) it
// lists connected peers. Inside a peer it dispatches a listing via
// the system/tree handler — local for the shell's own peer, remote
// (over the local peer's pooled connection) for a connected peer.
// The local-vs-remote split is handled by entitysdk based on the
// peer-id in the dispatched URI.
func cmdLs(sh *Shell, args []string) (Result, error) {
	target := sh.WD
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			target = sh.Resolve(a)
			break
		}
	}

	if target.IsRoot() {
		return listConnections(sh), nil
	}

	pc := sh.ConnForPath(target)
	if pc == nil {
		return Result{}, fmt.Errorf("no connection for path %s", target)
	}

	rows, err := listAt(pc.Peer, target)
	if err != nil {
		return Result{}, err
	}
	return Result{Kind: KindListing, Listing: rows}, nil
}

func listConnections(sh *Shell) Result {
	if len(sh.Conns) == 0 {
		return MessageResult("(no connections)")
	}
	aliases := make([]string, 0, len(sh.Conns))
	for a := range sh.Conns {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	rows := make([]ListingRow, 0, len(aliases))
	for _, a := range aliases {
		pc := sh.Conns[a]
		detail := pc.Address
		if detail == "" {
			detail = "(self)"
		}
		rows = append(rows, ListingRow{
			Name:   a,
			Kind:   "connection",
			Detail: detail,
		})
	}
	return Result{Kind: KindListing, Listing: rows}
}

func listAt(peer *entitysdk.AppPeer, target Path) ([]ListingRow, error) {
	// Pass the peer-qualified path to entitysdk; AppPeer.List routes
	// local or remote based on the peer-id in the path.
	prefix := target.String()
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	entries, err := peer.List(prefix)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}
	rows := make([]ListingRow, 0, len(entries))
	for _, e := range entries {
		kind := classifyEntry(e)
		rows = append(rows, ListingRow{
			Name:        e.Name,
			Path:        e.Path,
			Kind:        kind,
			HasChildren: e.HasChildren,
			Hash:        e.ContentHash,
		})
	}
	return rows, nil
}

func classifyEntry(e entitysdk.Entry) string {
	hasEntity := !e.ContentHash.IsZero()
	switch {
	case e.HasChildren && hasEntity:
		return "dir+entity"
	case e.HasChildren:
		return "dir"
	case hasEntity:
		return "entity"
	default:
		return ""
	}
}

// cmdCd implements `cd <path>`. Supports `cd alias:` shorthand to
// jump to a peer's root.
func cmdCd(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		sh.SetWD("/")
		return Result{}, nil
	}

	input := args[0]

	if strings.HasSuffix(input, ":") {
		alias := strings.TrimSuffix(input, ":")
		// "self" is a built-in pronoun for the in-process peer; it
		// always resolves regardless of the peer's primary alias.
		if alias == "self" && sh.Local != nil {
			sh.SetWD(Path("/" + sh.Local.PeerID + "/"))
			return Result{}, nil
		}
		pc, ok := sh.Conns[alias]
		if !ok {
			return Result{}, fmt.Errorf("not connected: %s", alias)
		}
		sh.SetWD(Path("/" + pc.PeerID + "/"))
		return Result{}, nil
	}

	target := sh.Resolve(input)
	if pc := sh.ConnForPath(target); pc == nil && !target.IsRoot() {
		return Result{}, fmt.Errorf("no connection for path %s", target)
	}
	sh.SetWD(target)
	return Result{}, nil
}

// cmdPwd implements `pwd`. Result.Path carries the WD in
// reverse-resolved (alias-substituted) form per
// GUIDE-SHELL-FRAMING.md §6.5; storage stays resolved (peer-id form)
// in sh.WD for dispatch determinism.
func cmdPwd(sh *Shell, _ []string) (Result, error) {
	return PathResult(sh.DisplayWD()), nil
}

// cmdTree implements `tree [path] [-depth N] [-v]`.
func cmdTree(sh *Shell, args []string) (Result, error) {
	target := sh.WD
	maxDepth := 3
	verbose := false

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-depth" && i+1 < len(args):
			d, err := strconv.Atoi(args[i+1])
			if err != nil {
				return Result{}, fmt.Errorf("invalid depth: %s", args[i+1])
			}
			maxDepth = d
			i++
		case args[i] == "-v" || args[i] == "-verbose":
			verbose = true
		case !strings.HasPrefix(args[i], "-"):
			target = sh.Resolve(args[i])
		}
	}

	if target.IsRoot() {
		return Result{}, fmt.Errorf("tree requires a peer path (cd into a peer first)")
	}

	pc := sh.ConnForPath(target)
	if pc == nil {
		return Result{}, fmt.Errorf("no connection for path %s", target)
	}

	var rows []TreeRow
	if err := walkTree(pc.Peer, target, 0, maxDepth, verbose, &rows); err != nil {
		return Result{}, err
	}
	return Result{Kind: KindTree, Tree: rows}, nil
}

func walkTree(peer *entitysdk.AppPeer, target Path, depth, maxDepth int, verbose bool, rows *[]TreeRow) error {
	if depth >= maxDepth {
		return nil
	}
	// Use peer-qualified paths so remote peers route through the SDK.
	listPath := target.String()
	if listPath != "" && !strings.HasSuffix(listPath, "/") {
		listPath += "/"
	}
	entries, err := peer.List(listPath)
	if err != nil {
		return fmt.Errorf("list %s: %w", listPath, err)
	}
	for _, e := range entries {
		kind := classifyEntry(e)
		row := TreeRow{
			Path:        e.Path,
			Name:        e.Name,
			Depth:       depth,
			Kind:        kind,
			HasChildren: e.HasChildren,
			Hash:        e.ContentHash,
		}
		if verbose && !e.ContentHash.IsZero() {
			// e.Path is already peer-qualified because we passed a
			// peer-qualified listPath into List; AppPeer.Get routes
			// local/remote based on the peer-id.
			ent, ok, gerr := peer.Get(e.Path)
			if gerr == nil && ok {
				row.Entity = &ent
				var decoded interface{}
				_ = ecf.Decode(ent.Data, &decoded)
				row.Decoded = decoded
			}
		}
		*rows = append(*rows, row)
		if e.HasChildren {
			child := Path(listPath + e.Name + "/")
			if err := walkTree(peer, child, depth+1, maxDepth, verbose, rows); err != nil {
				return err
			}
		}
	}
	return nil
}
