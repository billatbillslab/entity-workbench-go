package shellcmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// cmdRevision dispatches the `revision <subcommand> [args...]` command
// surface against the local peer's revision extension. Subcommand
// vocabulary mirrors the typed RevisionClient (see entitysdk/revision.go),
// surfaced at the CLI per SHELL-DIRECTION.md (shell-first feature
// development).
//
// Subcommands:
//
//	revision commit <prefix> [message]
//	revision log <prefix> [-limit N] [-full]
//	revision status <prefix>
//	revision show <prefix> <ref>           — resolve ref + show version metadata
//	revision diff <prefix> <base> <target>
//	revision find-ancestor <a> <b>
//	revision branch list|create|delete|switch <prefix> [args]
//	revision tag    list|create|delete         <prefix> [args]
//	revision checkout <prefix> <branch-or-version>
//	revision cherry-pick <prefix> <version>
//	revision revert      <prefix> <version>
//	revision merge       <prefix> <remote-version> [-strategy NAME] [-dry-run]
//	revision resolve     <prefix> <path> [<resolved-hash>]
//	revision config put|delete <name> [args]
//	revision sync <prefix> <remote-alias>      — one-shot pull
//	                                            (fetch + fetch-entities
//	                                            + merge from remote)
//
// `<ref>` accepted by `show` (and forms feeding revision/checkout
// hashes generally) is one of: a full hash (any of the formats
// parseHashHex understands), a branch name, a tag name, "HEAD", or
// a short hex prefix (≥4 chars) that uniquely matches one version
// in the log.
//
// Cross-peer ops (push / fetch / fetch-entities) are deferred — they
// land when the shell grows a stable cross-peer transfer story.
//
// Hashes throughout are 33-byte hex (system/hash content-hash form),
// matching the format emitted by `cat -diag` etc.
func cmdRevision(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf(
			"usage: revision <commit|log|status|diff|find-ancestor|branch|tag|checkout|cherry-pick|revert|merge|resolve|config|sync|follow|mirror|unfollow|push> [args]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "commit":
		return cmdRevisionCommit(sh, rest)
	case "log":
		return cmdRevisionLog(sh, rest)
	case "status":
		return cmdRevisionStatus(sh, rest)
	case "show":
		return cmdRevisionShow(sh, rest)
	case "diff":
		return cmdRevisionDiff(sh, rest)
	case "find-ancestor":
		return cmdRevisionFindAncestor(sh, rest)
	case "branch":
		return cmdRevisionBranch(sh, rest)
	case "tag":
		return cmdRevisionTag(sh, rest)
	case "checkout":
		return cmdRevisionCheckout(sh, rest)
	case "cherry-pick":
		return cmdRevisionCherryPick(sh, rest)
	case "revert":
		return cmdRevisionRevert(sh, rest)
	case "merge":
		return cmdRevisionMerge(sh, rest)
	case "resolve":
		return cmdRevisionResolve(sh, rest)
	case "config":
		return cmdRevisionConfig(sh, rest)
	case "sync":
		return cmdRevisionSync(sh, rest)
	case "follow":
		return cmdRevisionFollow(sh, rest)
	case "mirror":
		return cmdRevisionMirror(sh, rest)
	case "unfollow":
		return cmdRevisionUnfollow(sh, rest)
	case "push":
		return cmdRevisionPush(sh, rest)
	default:
		return Result{}, fmt.Errorf("unknown revision subcommand: %s", sub)
	}
}

// cmdRevisionSync wraps RevisionClient.Sync — one-shot fetch + merge
// from a connected remote into the local DAG. Pre-condition: remote
// is in the connection table (use `connect <alias> <addr>` first).
func cmdRevisionSync(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision sync <prefix> <remote-alias>")
	}
	prefix, alias := args[0], args[1]

	pc, ok := sh.Conns[alias]
	if !ok {
		return Result{}, fmt.Errorf("not connected: %s (use 'connect <alias> <addr>' first)", alias)
	}
	res, err := sh.Local.Peer.Revision().Pull(context.Background(), prefix, pc.PeerID)
	if err != nil {
		return Result{}, fmt.Errorf("revision sync: %w", err)
	}
	lines := []string{
		fmt.Sprintf("status:  %s", res.Status),
		fmt.Sprintf("version: %s", shortHashOrDash(res.Version)),
	}
	if res.MergedCount != nil {
		lines = append(lines, fmt.Sprintf("merged:  %d", *res.MergedCount))
	}
	if len(res.Conflicts) > 0 {
		lines = append(lines, fmt.Sprintf("conflicts: %d", len(res.Conflicts)))
		for _, p := range res.Conflicts {
			lines = append(lines, "  ! "+p)
		}
	}
	return LinesResult(lines), nil
}

// --- per-prefix queries ---

func cmdRevisionCommit(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: revision commit <prefix> [message]")
	}
	prefix := args[0]
	message := strings.Join(args[1:], " ")

	res, err := sh.Local.Peer.Revision().Commit(context.Background(), prefix, message)
	if err != nil {
		return Result{}, fmt.Errorf("revision commit: %w", err)
	}
	return MessageResult(fmt.Sprintf("committed %s @ root %s",
		shortHash(res.Version), shortHash(res.Root))), nil
}

func cmdRevisionLog(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: revision log <prefix> [-limit N] [-full]")
	}
	prefix := args[0]
	params := types.RevisionLogParamsData{Prefix: prefix}
	full := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-limit":
			if i+1 >= len(args) {
				return Result{}, fmt.Errorf("-limit requires a value")
			}
			n, err := strconv.ParseUint(args[i+1], 10, 64)
			if err != nil {
				return Result{}, fmt.Errorf("-limit: %w", err)
			}
			params.Limit = &n
			i++
		case "-full":
			full = true
		default:
			return Result{}, fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	res, err := sh.Local.Peer.Revision().Log(context.Background(), params)
	if err != nil {
		return Result{}, fmt.Errorf("revision log: %w", err)
	}
	if len(res.Versions) == 0 {
		return MessageResult(fmt.Sprintf("(no versions at %s)", prefix)), nil
	}
	lines := make([]string, 0, len(res.Versions)+1)
	for i, v := range res.Versions {
		marker := " "
		if i == 0 {
			marker = "*"
		}
		hashStr := shortHash(v)
		if full {
			hashStr = v.String()
		}
		lines = append(lines, fmt.Sprintf("%s %s", marker, hashStr))
	}
	if res.HasMore {
		lines = append(lines, "(more — pass -limit to extend)")
	}
	return LinesResult(lines), nil
}

func cmdRevisionStatus(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: revision status <prefix>")
	}
	prefix := args[0]
	st, err := sh.Local.Peer.Revision().Status(context.Background(), prefix)
	if err != nil {
		return Result{}, fmt.Errorf("revision status: %w", err)
	}
	lines := []string{
		fmt.Sprintf("prefix:    %s", st.Prefix),
		fmt.Sprintf("head:      %s", shortHashOrDash(st.Head)),
		fmt.Sprintf("conflicts: %d", st.Conflicts),
		fmt.Sprintf("pending:   %d", st.Pending),
	}
	if len(st.Remotes) > 0 {
		lines = append(lines, "remotes:")
		for name, h := range st.Remotes {
			lines = append(lines, fmt.Sprintf("  %s = %s", name, shortHash(h)))
		}
	}
	if len(st.KeepBothPaths) > 0 {
		lines = append(lines, fmt.Sprintf("keep-both: %d path(s)", len(st.KeepBothPaths)))
	}
	return LinesResult(lines), nil
}

func cmdRevisionDiff(sh *Shell, args []string) (Result, error) {
	if len(args) < 3 {
		return Result{}, fmt.Errorf("usage: revision diff <prefix> <base-ref> <target-ref>")
	}
	prefix := args[0]
	rc := sh.Local.Peer.Revision()
	ctx := context.Background()
	base, _, err := resolveRevisionRef(ctx, rc, prefix, args[1])
	if err != nil {
		return Result{}, fmt.Errorf("base: %w", err)
	}
	target, _, err := resolveRevisionRef(ctx, rc, prefix, args[2])
	if err != nil {
		return Result{}, fmt.Errorf("target: %w", err)
	}
	diff, err := rc.Diff(ctx, prefix, base, target)
	if err != nil {
		return Result{}, fmt.Errorf("revision diff: %w", err)
	}
	lines := []string{
		fmt.Sprintf("base:      %s", shortHash(diff.Base)),
		fmt.Sprintf("target:    %s", shortHash(diff.Target)),
		fmt.Sprintf("added:     %d", len(diff.Added)),
		fmt.Sprintf("removed:   %d", len(diff.Removed)),
		fmt.Sprintf("changed:   %d", len(diff.Changed)),
		fmt.Sprintf("unchanged: %d (subtree count)", diff.Unchanged),
	}
	for p := range diff.Added {
		lines = append(lines, "  + "+p)
	}
	for p := range diff.Removed {
		lines = append(lines, "  - "+p)
	}
	for p := range diff.Changed {
		lines = append(lines, "  ~ "+p)
	}
	return LinesResult(lines), nil
}

func cmdRevisionFindAncestor(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision find-ancestor <a-hash> <b-hash>")
	}
	a, err := parseHashHex(args[0])
	if err != nil {
		return Result{}, fmt.Errorf("a: %w", err)
	}
	b, err := parseHashHex(args[1])
	if err != nil {
		return Result{}, fmt.Errorf("b: %w", err)
	}
	anc, err := sh.Local.Peer.Revision().FindAncestor(context.Background(), a, b)
	if err != nil {
		return Result{}, fmt.Errorf("revision find-ancestor: %w", err)
	}
	if anc.IsZero() {
		return MessageResult("(no common ancestor)"), nil
	}
	return MessageResult(fmt.Sprintf("ancestor: %s", shortHash(anc))), nil
}

// --- show / ref resolution ---

// cmdRevisionShow resolves a ref (HEAD / branch name / tag name /
// hash / short hash prefix) to a version hash and prints the
// version's metadata: full hash, root, parents, plus any branch
// and tag pointers that name it.
func cmdRevisionShow(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision show <prefix> <HEAD|branch|tag|hash|short-prefix>")
	}
	prefix, ref := args[0], args[1]
	rc := sh.Local.Peer.Revision()
	ctx := context.Background()

	versionHash, source, err := resolveRevisionRef(ctx, rc, prefix, ref)
	if err != nil {
		return Result{}, fmt.Errorf("revision show: %w", err)
	}

	// Load the version entity via the local store (works for
	// versions on the local peer; cross-peer reads need the same
	// machinery wrapped server-side as a `revision show` op — not
	// in scope yet).
	ent, ok := sh.Local.Peer.Store().GetByHash(versionHash)
	if !ok {
		return Result{}, fmt.Errorf("revision show: version %s not in local store", versionHash)
	}
	ver, err := types.RevisionEntryDataFromEntity(ent)
	if err != nil {
		return Result{}, fmt.Errorf("revision show: decode version entry: %w", err)
	}

	// Find branch and tag pointers naming this version. Best-effort —
	// errors here are non-fatal (we still want the metadata).
	var branchHits, tagHits []string
	if branches, err := rc.BranchList(ctx, prefix); err == nil {
		for name, h := range branches.Branches {
			if h == versionHash {
				branchHits = append(branchHits, name)
			}
		}
	}
	if tags, err := rc.TagList(ctx, prefix); err == nil {
		for name, h := range tags.Tags {
			if h == versionHash {
				tagHits = append(tagHits, name)
			}
		}
	}

	lines := []string{
		fmt.Sprintf("ref:      %s (%s)", ref, source),
		fmt.Sprintf("version:  %s", versionHash.String()),
		fmt.Sprintf("root:     %s", ver.Root.String()),
	}
	if len(ver.Parents) == 0 {
		lines = append(lines, "parents:  (none — root version)")
	} else {
		for i, p := range ver.Parents {
			label := fmt.Sprintf("parent[%d]:", i)
			lines = append(lines, fmt.Sprintf("%-9s %s", label, p.String()))
		}
	}
	if len(branchHits) > 0 {
		lines = append(lines, "branches: "+strings.Join(branchHits, ", "))
	}
	if len(tagHits) > 0 {
		lines = append(lines, "tags:     "+strings.Join(tagHits, ", "))
	}
	return LinesResult(lines), nil
}

// resolveRevisionRef attempts to resolve ref to a version hash, in
// the priority order: HEAD → exact branch → exact tag →
// full-format hash → short-prefix match against the log. Returns
// the resolved hash plus a one-word source label for the show
// command's display.
func resolveRevisionRef(ctx context.Context, rc *entitysdkRevisionClient, prefix, ref string) (hash.Hash, string, error) {
	// HEAD → status
	if ref == "HEAD" {
		st, err := rc.Status(ctx, prefix)
		if err != nil {
			return hash.Hash{}, "", err
		}
		if st.Head.IsZero() {
			return hash.Hash{}, "", fmt.Errorf("HEAD: prefix has no committed version")
		}
		return st.Head, "HEAD", nil
	}
	// branch?
	if branches, err := rc.BranchList(ctx, prefix); err == nil {
		if h, ok := branches.Branches[ref]; ok {
			return h, "branch", nil
		}
	}
	// tag?
	if tags, err := rc.TagList(ctx, prefix); err == nil {
		if h, ok := tags.Tags[ref]; ok {
			return h, "tag", nil
		}
	}
	// full-format hash?
	if h, err := parseHashHex(ref); err == nil {
		return h, "hash", nil
	}
	// short hex prefix → match against log
	probe := strings.ToLower(strings.TrimPrefix(ref, "ecf-sha256:"))
	if len(probe) >= 4 && isHexLower(probe) {
		log, err := rc.Log(ctx, types.RevisionLogParamsData{Prefix: prefix})
		if err != nil {
			return hash.Hash{}, "", fmt.Errorf("short-prefix match needs log: %w", err)
		}
		var matches []hash.Hash
		for _, v := range log.Versions {
			full := hex.EncodeToString(v.Bytes())
			// Accept matches against either the algorithm-prefixed (66-char)
			// or bare digest (64-char) form.
			if strings.HasPrefix(full, probe) || strings.HasPrefix(full[2:], probe) {
				matches = append(matches, v)
			}
		}
		switch len(matches) {
		case 0:
			return hash.Hash{}, "", fmt.Errorf("no version matches prefix %q", ref)
		case 1:
			return matches[0], "short", nil
		default:
			return hash.Hash{}, "", fmt.Errorf("ambiguous short prefix %q matches %d versions",
				ref, len(matches))
		}
	}
	return hash.Hash{}, "", fmt.Errorf("could not resolve ref %q (tried HEAD, branch, tag, hash, short-prefix)", ref)
}

// isHexLower reports whether s is a non-empty lowercase hex string.
func isHexLower(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// entitysdkRevisionClient is a local alias to keep the helper signature
// tight — same type as sh.Local.Peer.Revision() returns.
type entitysdkRevisionClient = entitysdk.RevisionClient

// --- branch ---

func cmdRevisionBranch(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision branch <list|create|delete|switch> <prefix> [args]")
	}
	action, prefix, rest := args[0], args[1], args[2:]
	rc := sh.Local.Peer.Revision()
	ctx := context.Background()

	switch action {
	case "list":
		res, err := rc.BranchList(ctx, prefix)
		if err != nil {
			return Result{}, fmt.Errorf("revision branch list: %w", err)
		}
		if len(res.Branches) == 0 {
			return MessageResult(fmt.Sprintf("(no branches at %s)", prefix)), nil
		}
		lines := make([]string, 0, len(res.Branches))
		for name, h := range res.Branches {
			marker := " "
			if name == res.Active {
				marker = "*"
			}
			lines = append(lines, fmt.Sprintf("%s %-20s %s", marker, name, shortHash(h)))
		}
		return LinesResult(lines), nil

	case "create":
		if len(rest) < 1 {
			return Result{}, fmt.Errorf("usage: revision branch create <prefix> <name> [<ref>]")
		}
		name := rest[0]
		var from hash.Hash
		if len(rest) >= 2 {
			h, _, err := resolveRevisionRef(ctx, rc, prefix, rest[1])
			if err != nil {
				return Result{}, fmt.Errorf("from: %w", err)
			}
			from = h
		}
		res, err := rc.BranchCreate(ctx, prefix, name, from)
		if err != nil {
			return Result{}, fmt.Errorf("revision branch create: %w", err)
		}
		return MessageResult(fmt.Sprintf("created branch %q @ %s", name, shortHashOrDash(res.Version))), nil

	case "delete":
		if len(rest) < 1 {
			return Result{}, fmt.Errorf("usage: revision branch delete <prefix> <name>")
		}
		if _, err := rc.BranchDelete(ctx, prefix, rest[0]); err != nil {
			return Result{}, fmt.Errorf("revision branch delete: %w", err)
		}
		return MessageResult(fmt.Sprintf("deleted branch %q", rest[0])), nil

	case "switch":
		if len(rest) < 1 {
			return Result{}, fmt.Errorf("usage: revision branch switch <prefix> <name>")
		}
		if _, err := rc.BranchSwitch(ctx, prefix, rest[0]); err != nil {
			return Result{}, fmt.Errorf("revision branch switch: %w", err)
		}
		return MessageResult(fmt.Sprintf("switched to branch %q", rest[0])), nil

	default:
		return Result{}, fmt.Errorf("unknown branch action: %s", action)
	}
}

// --- tag ---

func cmdRevisionTag(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision tag <list|create|delete> <prefix> [args]")
	}
	action, prefix, rest := args[0], args[1], args[2:]
	rc := sh.Local.Peer.Revision()
	ctx := context.Background()

	switch action {
	case "list":
		res, err := rc.TagList(ctx, prefix)
		if err != nil {
			return Result{}, fmt.Errorf("revision tag list: %w", err)
		}
		if len(res.Tags) == 0 {
			return MessageResult(fmt.Sprintf("(no tags at %s)", prefix)), nil
		}
		lines := make([]string, 0, len(res.Tags))
		for name, h := range res.Tags {
			lines = append(lines, fmt.Sprintf("  %-20s %s", name, shortHash(h)))
		}
		return LinesResult(lines), nil

	case "create":
		if len(rest) < 2 {
			return Result{}, fmt.Errorf("usage: revision tag create <prefix> <name> <ref>")
		}
		name := rest[0]
		version, _, err := resolveRevisionRef(ctx, rc, prefix, rest[1])
		if err != nil {
			return Result{}, fmt.Errorf("version: %w", err)
		}
		if _, err := rc.TagCreate(ctx, prefix, name, version); err != nil {
			return Result{}, fmt.Errorf("revision tag create: %w", err)
		}
		return MessageResult(fmt.Sprintf("tagged %s as %q", shortHash(version), name)), nil

	case "delete":
		if len(rest) < 1 {
			return Result{}, fmt.Errorf("usage: revision tag delete <prefix> <name>")
		}
		if _, err := rc.TagDelete(ctx, prefix, rest[0]); err != nil {
			return Result{}, fmt.Errorf("revision tag delete: %w", err)
		}
		return MessageResult(fmt.Sprintf("deleted tag %q", rest[0])), nil

	default:
		return Result{}, fmt.Errorf("unknown tag action: %s", action)
	}
}

// --- working state mutations ---

func cmdRevisionCheckout(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision checkout <prefix> <ref>")
	}
	prefix, target := args[0], args[1]
	rc := sh.Local.Peer.Revision()
	ctx := context.Background()

	params := types.RevisionCheckoutParamsData{Prefix: prefix}
	// If target names an existing branch, follow it (the branch
	// pointer can advance as new commits land on it). Otherwise
	// resolve to a version and check out detached. This mirrors
	// the spec's two checkout modes.
	if branches, err := rc.BranchList(ctx, prefix); err == nil {
		if _, ok := branches.Branches[target]; ok {
			params.Branch = target
		}
	}
	if params.Branch == "" {
		h, _, err := resolveRevisionRef(ctx, rc, prefix, target)
		if err != nil {
			return Result{}, fmt.Errorf("revision checkout: %w", err)
		}
		params.Version = h
	}

	res, err := rc.Checkout(ctx, params)
	if err != nil {
		return Result{}, fmt.Errorf("revision checkout: %w", err)
	}
	msg := fmt.Sprintf("checked out %s", shortHash(res.Version))
	if res.Branch != "" {
		msg += fmt.Sprintf(" (branch %q)", res.Branch)
	}
	if len(res.CascadeWarnings) > 0 {
		msg += fmt.Sprintf(" — %d cascade warning(s)", len(res.CascadeWarnings))
	}
	return MessageResult(msg), nil
}

func cmdRevisionCherryPick(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision cherry-pick <prefix> <ref> [<parent-ref>]")
	}
	prefix := args[0]
	rc := sh.Local.Peer.Revision()
	ctx := context.Background()
	version, _, err := resolveRevisionRef(ctx, rc, prefix, args[1])
	if err != nil {
		return Result{}, fmt.Errorf("version: %w", err)
	}
	var parent hash.Hash
	if len(args) >= 3 {
		parent, _, err = resolveRevisionRef(ctx, rc, prefix, args[2])
		if err != nil {
			return Result{}, fmt.Errorf("parent: %w", err)
		}
	}
	res, err := rc.CherryPick(ctx, prefix, version, parent)
	if err != nil {
		return Result{}, fmt.Errorf("revision cherry-pick: %w", err)
	}
	msg := fmt.Sprintf("cherry-pick %s → %s [%s]",
		shortHash(res.Source), shortHash(res.Version), res.Status)
	if len(res.Conflicts) > 0 {
		msg += fmt.Sprintf(" (%d conflict(s) — use 'revision resolve')", len(res.Conflicts))
	}
	return MessageResult(msg), nil
}

func cmdRevisionRevert(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision revert <prefix> <ref> [<parent-ref>]")
	}
	prefix := args[0]
	rc := sh.Local.Peer.Revision()
	ctx := context.Background()
	version, _, err := resolveRevisionRef(ctx, rc, prefix, args[1])
	if err != nil {
		return Result{}, fmt.Errorf("version: %w", err)
	}
	var parent hash.Hash
	if len(args) >= 3 {
		parent, _, err = resolveRevisionRef(ctx, rc, prefix, args[2])
		if err != nil {
			return Result{}, fmt.Errorf("parent: %w", err)
		}
	}
	res, err := rc.Revert(ctx, prefix, version, parent)
	if err != nil {
		return Result{}, fmt.Errorf("revision revert: %w", err)
	}
	msg := fmt.Sprintf("reverted %s → %s [%s]",
		shortHash(res.Reverted), shortHash(res.Version), res.Status)
	if len(res.Conflicts) > 0 {
		msg += fmt.Sprintf(" (%d conflict(s) — use 'revision resolve')", len(res.Conflicts))
	}
	return MessageResult(msg), nil
}

func cmdRevisionMerge(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision merge <prefix> <remote-version-hash> [-strategy NAME] [-dry-run]")
	}
	prefix := args[0]
	remoteVer, err := parseHashHex(args[1])
	if err != nil {
		return Result{}, fmt.Errorf("remote-version: %w", err)
	}
	params := types.RevisionMergeParamsData{Prefix: prefix, RemoteVersion: remoteVer}
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "-strategy":
			if i+1 >= len(args) {
				return Result{}, fmt.Errorf("-strategy requires a value")
			}
			params.Strategy = args[i+1]
			i++
		case "-dry-run":
			b := true
			params.DryRun = &b
		default:
			return Result{}, fmt.Errorf("unknown flag: %s", args[i])
		}
	}
	res, err := sh.Local.Peer.Revision().Merge(context.Background(), params)
	if err != nil {
		return Result{}, fmt.Errorf("revision merge: %w", err)
	}
	lines := []string{fmt.Sprintf("status:  %s", res.Status)}
	if !res.Version.IsZero() {
		lines = append(lines, fmt.Sprintf("version: %s", shortHash(res.Version)))
	}
	if res.MergedCount != nil {
		lines = append(lines, fmt.Sprintf("merged:  %d", *res.MergedCount))
	}
	if res.DeletedCount != nil {
		lines = append(lines, fmt.Sprintf("deleted: %d", *res.DeletedCount))
	}
	if len(res.Conflicts) > 0 {
		lines = append(lines, fmt.Sprintf("conflicts: %d", len(res.Conflicts)))
		for _, p := range res.Conflicts {
			lines = append(lines, "  ! "+p)
		}
		lines = append(lines, "use 'revision resolve <prefix> <path> [<hash>]' to fix.")
	}
	if len(res.CascadeWarnings) > 0 {
		lines = append(lines, fmt.Sprintf("cascade warnings: %d", len(res.CascadeWarnings)))
	}
	return LinesResult(lines), nil
}

func cmdRevisionResolve(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision resolve <prefix> <path> [<resolved-hash>]")
	}
	prefix, path := args[0], args[1]
	var resolved *hash.Hash
	if len(args) >= 3 {
		h, err := parseHashHex(args[2])
		if err != nil {
			return Result{}, fmt.Errorf("resolved-hash: %w", err)
		}
		resolved = &h
	}
	res, err := sh.Local.Peer.Revision().Resolve(context.Background(), prefix, path, resolved)
	if err != nil {
		return Result{}, fmt.Errorf("revision resolve: %w", err)
	}
	bound := "dropped"
	if res.Resolved != nil {
		bound = "→ " + shortHash(*res.Resolved)
	}
	return MessageResult(fmt.Sprintf("resolved %s %s (remaining: %d)",
		res.Path, bound, res.RemainingConflicts)), nil
}

// --- config ---

func cmdRevisionConfig(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: revision config <put|delete> <name> [args]")
	}
	action, name, rest := args[0], args[1], args[2:]
	rc := sh.Local.Peer.Revision()
	ctx := context.Background()

	switch action {
	case "put":
		if len(rest) < 1 {
			return Result{}, fmt.Errorf("usage: revision config put <name> <prefix> [-auto] [-exclude PAT]... [-merge-order ORDER]")
		}
		cfg := types.RevisionConfigData{Prefix: rest[0]}
		for i := 1; i < len(rest); i++ {
			switch rest[i] {
			case "-auto":
				b := true
				cfg.AutoVersion = &b
			case "-exclude":
				if i+1 >= len(rest) {
					return Result{}, fmt.Errorf("-exclude requires a value")
				}
				cfg.Exclude = append(cfg.Exclude, rest[i+1])
				i++
			case "-merge-order":
				if i+1 >= len(rest) {
					return Result{}, fmt.Errorf("-merge-order requires a value")
				}
				cfg.MergeOrder = rest[i+1]
				i++
			default:
				return Result{}, fmt.Errorf("unknown flag: %s", rest[i])
			}
		}
		res, err := rc.ConfigPut(ctx, name, cfg, nil)
		if err != nil {
			return Result{}, fmt.Errorf("revision config put: %w", err)
		}
		return MessageResult(fmt.Sprintf("wrote config %q at %s (hash %s)",
			name, res.ConfigPath, shortHash(res.ConfigHash))), nil

	case "delete":
		if _, err := rc.ConfigDelete(ctx, name, nil); err != nil {
			return Result{}, fmt.Errorf("revision config delete: %w", err)
		}
		return MessageResult(fmt.Sprintf("deleted config %q", name)), nil

	default:
		return Result{}, fmt.Errorf("unknown config action: %s", action)
	}
}

// parseHashHex parses a content-hash from one of three accepted
// shell-friendly forms:
//
//   - "ecf-sha256:HEX64"  — the form Hash.String() emits and `cat
//     -diag` prints (32-byte digest, algorithm implied by the prefix).
//   - "00HEX64"            — algorithm-byte + digest as raw 33-byte
//     hex; matches the form `revision log` and friends emit (modulo
//     truncation to short form).
//   - "HEX64"              — bare 32-byte digest hex; algorithm
//     defaults to sha256.
//
// Trailing "..." (the short-form ellipsis) is rejected — callers
// are expected to feed the full hex.
func parseHashHex(s string) (hash.Hash, error) {
	if strings.HasSuffix(s, "...") {
		return hash.Hash{}, fmt.Errorf("hash is truncated (ends with '...'); use the full form (cat -diag <path>)")
	}
	if strings.HasPrefix(s, "ecf-sha256:") {
		s = s[len("ecf-sha256:"):]
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("not valid hex: %w", err)
	}
	// Bare 32-byte digest — prepend the sha256 algorithm byte.
	if len(b) == 32 {
		b = append([]byte{0x00}, b...)
	}
	h, err := hash.FromBytes(b)
	if err != nil {
		return hash.Hash{}, err
	}
	return h, nil
}

// shortHashOrDash returns "—" for the zero hash, otherwise the short
// hex form. Used for header rows where missing values are common
// (e.g. the head field of a never-committed prefix).
func shortHashOrDash(h hash.Hash) string {
	if h.IsZero() {
		return "—"
	}
	return shortHash(h)
}
