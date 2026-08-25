package shellcmd

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"sort"

	"entity-workbench-go/entitysdk"
)

// cmdIdentity dispatches the `identity <subcommand> [args...]` command
// surface. Per SHELL-DIRECTION.md §5.3 — identity-stack management
// lives at the shell level on top of entitysdk's identity helpers.
//
// V7-only mode subcommands:
//   - identity list           — enumerate keypairs under ~/.entity/identities/
//   - identity create <name>  — generate + persist a fresh keypair
//   - identity use <name>     — switch the local peer's identity (only when
//     no remote connections are active)
//
// Identity-aware mode subcommands:
//   - identity bootstrap [-members N] [-threshold K] [-name STRING]
//     — run the L0 identity ceremony on the
//     local peer; mints quorum + controller
//     cert; issues the local→controller cap
//
// Future (deferred): configure (post-bootstrap re-config),
// create-attestation, publish, revoke.
func cmdIdentity(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: identity <list|create|use|bootstrap> [args]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdIdentityList(sh, rest)
	case "create":
		return cmdIdentityCreate(sh, rest)
	case "use":
		return cmdIdentityUse(sh, rest)
	case "bootstrap":
		return cmdIdentityBootstrap(sh, rest)
	default:
		return Result{}, fmt.Errorf("unknown identity subcommand: %s (expected list|create|use|bootstrap)", sub)
	}
}

func cmdIdentityList(sh *Shell, _ []string) (Result, error) {
	ids, err := entitysdk.ListIdentities()
	if err != nil {
		return Result{}, fmt.Errorf("list identities: %w", err)
	}
	if len(ids) == 0 {
		dir, _ := entitysdk.IdentitiesDir()
		return MessageResult(fmt.Sprintf("no identities under %s", dir)), nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Name < ids[j].Name })

	activePeerID := sh.Local.PeerID
	lines := make([]string, 0, len(ids)+1)
	lines = append(lines, fmt.Sprintf("Identities (%d):", len(ids)))
	for _, id := range ids {
		marker := "  "
		if id.PeerID == activePeerID {
			marker = "* "
		}
		short := id.PeerID
		if len(short) > 12 {
			short = short[:12] + "..."
		}
		mode := id.Mode
		if mode == "" {
			mode = "v7-flat"
		}
		lines = append(lines, fmt.Sprintf("%s%-20s %-15s %s", marker, id.Name, mode, short))
	}
	return LinesResult(lines), nil
}

func cmdIdentityCreate(_ *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: identity create <name>")
	}
	name := args[0]
	id, err := entitysdk.CreateIdentity(name)
	if err != nil {
		return Result{}, fmt.Errorf("create identity: %w", err)
	}
	short := id.PeerID
	if len(short) > 12 {
		short = short[:12] + "..."
	}
	return MessageResult(fmt.Sprintf("created identity %q (peer-id %s)", name, short)), nil
}

// cmdIdentityUse switches the active identity. v1 constraint: only
// when there are no remote connections. The local peer is recreated
// with the new keypair, which rotates its peer-id; existing aliases
// are left intact (identity is process-scoped, not session-scoped).
//
// Restart with `entity-shell --identity NAME` is the alternative the
// user can fall back to whenever this constraint bites.
func cmdIdentityUse(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: identity use <name>")
	}
	name := args[0]

	// Refuse if any non-local connections exist.
	for alias, pc := range sh.Conns {
		if pc.Address != "" {
			return Result{}, fmt.Errorf(
				"cannot switch identity while connected to %q (%s); disconnect first or restart with --identity %s",
				alias, pc.Address, name)
		}
	}

	id, err := entitysdk.LoadIdentity(name)
	if err != nil {
		return Result{}, fmt.Errorf("load identity %q: %w", name, err)
	}

	// Recreate the local AppPeer with the new keypair. We replace the
	// shell's Local PeerConn entry (alias preserved).
	kp := id.Keypair
	newPeer, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		return Result{}, fmt.Errorf("recreate local peer: %w", err)
	}

	// Close the old peer after the new one is in hand so a partial
	// failure leaves the shell in a usable state.
	oldPeer := sh.Local.Peer
	sh.Local.Peer = newPeer
	sh.Local.PeerID = newPeer.PeerID()
	// peerMap rebuild: drop the old local peer-id entry, add the new.
	for pid, alias := range sh.peerMap {
		if alias == sh.Local.Alias {
			delete(sh.peerMap, pid)
			break
		}
	}
	sh.peerMap[sh.Local.PeerID] = sh.Local.Alias
	// Reset working directory if we were inside the old local peer's
	// tree — its peer-id segment is now stale.
	if sh.WD != "/" && sh.WD.PeerID() != sh.Local.PeerID {
		sh.SetWD("/")
	}
	if oldPeer != nil {
		_ = oldPeer.Close()
	}

	short := id.PeerID
	if len(short) > 12 {
		short = short[:12] + "..."
	}
	return MessageResult(fmt.Sprintf("now using identity %q (peer-id %s)", name, short)), nil
}

// cmdIdentityBootstrap runs the L0 identity ceremony on the local
// peer per SDK-IDENTITY-INFRASTRUCTURE §4.1. After bootstrap the
// peer is identity-aware: peer-config is bound and the local→
// controller cap is issued. Subsequent identity ops dispatch
// through that cap.
//
// Usage:
//
//	identity bootstrap -name NAME [-members N] [-threshold K]
//	identity bootstrap [-members N] [-threshold K]   (in-memory only)
//
// When -name is set, the identity material persists to disk at
// ~/.entity/identities/{NAME}/ and can be re-loaded by
// `entity-shell --identity NAME` on subsequent runs. Omit -name for
// an in-memory ceremony that doesn't survive process restart.
//
// Flags:
//
//	-name STRING  — bundle name on disk (also the quorum label)
//	-members N    — quorum size (default 1)
//	-threshold K  — K-of-N (default = members; everyone signs)
func cmdIdentityBootstrap(sh *Shell, args []string) (Result, error) {
	fs := flag.NewFlagSet("identity bootstrap", flag.ContinueOnError)
	nameFlag := fs.String("name", "", "bundle name on disk (also quorum label); empty = in-memory only")
	members := fs.Int("members", 1, "number of quorum members")
	threshold := fs.Int("threshold", 0, "K in K-of-N (default = members)")
	if err := fs.Parse(args); err != nil {
		return Result{}, fmt.Errorf("identity bootstrap: %w", err)
	}
	name := *nameFlag
	bundleName := name
	res, err := sh.Local.Peer.BootstrapIdentity(context.Background(), entitysdk.BootstrapOpts{
		QuorumMembers:   *members,
		QuorumThreshold: *threshold,
		QuorumName:      name,
		BundleName:      bundleName,
	})
	if err != nil {
		return Result{}, fmt.Errorf("identity bootstrap: %w", err)
	}
	lines := []string{
		fmt.Sprintf("Bootstrapped identity-aware peer (quorum %d-of-%d):",
			effectiveThreshold(*threshold, *members), len(res.QuorumMembers)),
		fmt.Sprintf("  quorum-id:           %s", hex.EncodeToString(res.QuorumID.Bytes())[:16]+"..."),
		fmt.Sprintf("  controller-cert:     %s", hex.EncodeToString(res.ControllerCertHash.Bytes())[:16]+"..."),
		fmt.Sprintf("  peer-config-path:    %s", res.PeerConfigPath),
		fmt.Sprintf("  caps issued:         %d", len(res.LocalToControllerCaps)),
	}
	if res.BundleDir != "" {
		lines = append(lines,
			fmt.Sprintf("  bundle persisted to: %s", res.BundleDir),
			"",
			fmt.Sprintf("Re-load on next run with: entity-shell --identity %s", name),
		)
	} else {
		lines = append(lines,
			"",
			"In-memory only — identity will not survive process restart.",
			"Re-run with `identity bootstrap NAME` to persist to disk.",
		)
	}
	lines = append(lines,
		"",
		"NOTE: quorum constituent private keys live in the bundle dir —",
		"distribute to separate custody (paper / device / token) per",
		"SDK-IDENTITY-INFRASTRUCTURE §8.2 for production deployments.",
	)
	return LinesResult(lines), nil
}

func effectiveThreshold(req, members int) int {
	if req < 1 {
		return members
	}
	return req
}
