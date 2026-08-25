package shellcmd

import (
	"fmt"
	"strings"

	"entity-workbench-go/entitysdk"
)

// cmd_name.go — the `name` verb: EXTENSION-REGISTRY §11.2's "UI / CLI
// surface for local-name bind / unbind / list", which the spec lists as a
// SHOULD and which nothing in this repo provided until 2026-08-19.
//
// The gap this closes is not a missing feature so much as a missing door.
// Every piece behind it was already built and conformance-checked — the
// meta-resolver, the local-name backend, the resolver-config validator,
// the v1.13/1.14 adoption work, the hints round-trip pin — and none of it
// was reachable from a shipped binary: `entitysdk` had the calls, no verb
// dispatched to them, and shellboot never wired the handler they dispatch
// to. That is exactly where the CDN corridor's own end-to-end bug hid
// (AP21): a stack green at every unit boundary and broken at the one seam
// no test crossed. A verb is the cheapest way to make the seam crossable
// by hand.
//
// Subcommands:
//
//	name ls                                  — the local name book
//	name resolve <name> [-pin <peer-id>]     — walk the resolver chain
//	name bind <name> <@alias|peer-id> [-notes ...]
//	name unbind <name>
//	name config                              — show the resolver-config
//
// **`name` operates on THIS peer's name book, always.** Local-name
// bindings are peer-local by construction (§6.5: never published, never
// synced), so there is no path form and no remote variant to offer. A
// `@alias` in the bind target is resolved to a peer-id at the dispatcher
// tier of this file — it names WHO the binding points at, not whose book
// is being written.
func cmdName(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: name <ls|resolve|bind|unbind|config> [args]")
	}
	ap, err := nameResolutionPeer(sh)
	if err != nil {
		return Result{}, err
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return cmdNameLs(ap, rest)
	case "resolve":
		return cmdNameResolve(ap, rest)
	case "bind":
		return cmdNameBind(sh, ap, rest)
	case "unbind", "rm":
		return cmdNameUnbind(ap, rest)
	case "config":
		return cmdNameConfig(ap, rest)
	default:
		return Result{}, fmt.Errorf("unknown name subcommand: %s (want ls|resolve|bind|unbind|config)", sub)
	}
}

// nameResolutionPeer returns the local AppPeer, refusing early and in
// plain words when the registry substrate is not wired.
//
// Without this check every subcommand fails as a dispatch 404 naming
// `system/registry`, which a user reads as "that name does not exist" —
// the opposite diagnosis from "nothing was consulted". The two failures
// look identical at the prompt and send a user in opposite directions.
func nameResolutionPeer(sh *Shell) (*entitysdk.AppPeer, error) {
	if sh.Local == nil || sh.Local.Peer == nil {
		return nil, fmt.Errorf("name: no local peer")
	}
	ap := sh.Local.Peer
	if !ap.NameResolutionEnabled() {
		return nil, fmt.Errorf("name: name resolution is not enabled on this peer " +
			"(EXTENSION-REGISTRY substrate not wired). It is on by default; restart " +
			"without -disable-registry to use `name`")
	}
	return ap, nil
}

// cmdNameLs renders the local name book. Columns are NAME, TARGET, and
// NOTES; the binding hash is deliberately not a column — it is 66 chars
// and would push every other field off an 80-column terminal, and the one
// operation that needs it (update-transports) takes the name, not the
// hash. `name resolve` prints it for a single binding.
func cmdNameLs(ap *entitysdk.AppPeer, _ []string) (Result, error) {
	book, err := ap.ListLocalNames()
	if err != nil {
		return Result{}, fmt.Errorf("name ls: %w", err)
	}
	if len(book) == 0 {
		// Distinguished from an error on purpose: an empty book is the
		// state every peer starts in, and the message says what to do next.
		return MessageResult("(no local names bound — `name bind <name> <@alias|peer-id>`)"), nil
	}
	lines := []string{fmt.Sprintf("%-20s  %-16s  %s", "NAME", "TARGET", "NOTES")}
	for _, b := range book {
		notes := b.Notes
		// Only the EXCEPTIONAL pin state is shown. §6.4's
		// `default_pinned` is true, so every ordinary binding is pinned
		// and a "(pinned)" marker on every row is decoration that a
		// reader learns to skip — which is precisely how they would come
		// to miss the one row where it matters.
		if !b.Pinned {
			if notes != "" {
				notes += " "
			}
			notes += "(NOT pinned)"
		}
		lines = append(lines, fmt.Sprintf("%-20s  %-16s  %s",
			b.Name, shortID(b.TargetPeerID), notes))
	}
	return LinesResult(lines), nil
}

// cmdNameResolve walks the resolver chain for a name and reports where it
// got to. The typed *Outcome is the whole point of the surface, so a
// failure prints its rung and kind rather than a bare error string — "no
// such name" and "found it, cannot reach it" are different problems and a
// user should not have to guess which one they have.
func cmdNameResolve(ap *entitysdk.AppPeer, args []string) (Result, error) {
	name, flags, err := splitNameFlags(args)
	if err != nil {
		return Result{}, fmt.Errorf("usage: name resolve <name> [-pin <peer-id>]: %w", err)
	}
	if name == "" {
		return Result{}, fmt.Errorf("usage: name resolve <name> [-pin <peer-id>]")
	}

	var res *entitysdk.Resolution
	if pin := flags["pin"]; pin != "" {
		res, err = ap.ResolveNamePinned(name, pin)
	} else {
		res, err = ap.ResolveName(name)
	}
	if err != nil {
		if o := entitysdk.AsOutcome(err); o != nil {
			return Result{}, fmt.Errorf("name resolve %q: stopped at the %s rung (%s): %w",
				name, o.Rung, o.Kind, o.Err)
		}
		return Result{}, fmt.Errorf("name resolve %q: %w", name, err)
	}

	lines := []string{
		fmt.Sprintf("%-14s %s", "name", res.Name),
		fmt.Sprintf("%-14s %s", "peer-id", res.PeerID),
		fmt.Sprintf("%-14s %s", "status", res.Status),
	}
	if res.TrustAnchor != "" {
		lines = append(lines, fmt.Sprintf("%-14s %s", "trust-anchor", res.TrustAnchor))
	}
	// Transports are the reach half of a resolution (MODEL §3.1: a name
	// lookup hands you identity AND initial reach). Report the count even
	// when it is zero — zero is the common case for a bind that named no
	// transports, and silence there reads as "this field does not exist".
	lines = append(lines, fmt.Sprintf("%-14s %d", "transports", len(res.Transports)))
	for _, h := range res.Transports {
		lines = append(lines, fmt.Sprintf("%-14s %s", "", h.String()))
	}
	return LinesResult(lines), nil
}

// cmdNameBind writes a local-name binding.
//
// The target accepts `@alias` (or a bare known alias) as well as a literal
// peer-id. `@alias` is the pinned substitution sigil, and this is the
// ergonomic that makes the verb usable at all: peer-ids are 40+ opaque
// characters, and a user who has just run `connect lab 10.0.0.4:9100`
// should be able to say `name bind lab-box @lab` without going to find
// one.
//
// Transports are NOT settable here yet. A binding's transports are hash
// references to transport-profile entities, and there is no verb that
// hands a user one; offering a flag that can only take a hash nobody can
// obtain would be a worse surface than not offering it. The SDK carries
// UpdateLocalNameTransports for when the profile-hash story lands.
func cmdNameBind(sh *Shell, ap *entitysdk.AppPeer, args []string) (Result, error) {
	rest, flags, err := splitFlags(args, "notes")
	if err != nil {
		return Result{}, fmt.Errorf("usage: name bind <name> <@alias|peer-id> [-notes \"...\"]: %w", err)
	}
	if len(rest) < 2 {
		return Result{}, fmt.Errorf("usage: name bind <name> <@alias|peer-id> [-notes \"...\"]")
	}
	name := rest[0]
	target, targetVia := resolvePeerRef(sh, rest[1])

	var opts []entitysdk.BindOption
	if n := flags["notes"]; n != "" {
		opts = append(opts, entitysdk.WithNotes(n))
	}
	if _, err := ap.BindLocalName(name, target, nil, opts...); err != nil {
		return Result{}, fmt.Errorf("name bind %q: %w", name, err)
	}
	return MessageResult(fmt.Sprintf("bound %s → %s%s", name, shortID(target), targetVia)), nil
}

// cmdNameUnbind removes a binding.
//
// It costs one extra `:list` to tell "removed" from "was never bound",
// and the reason is worth the round trip: the kernel's unbind is a
// TreeRemove and is therefore idempotent, so a user who typos a name
// during cleanup is otherwise told it is gone and walks away believing
// the real binding was removed. The check is advisory and racy by nature
// — it improves a message, it does not gate the operation — so a failure
// to list is not fatal here.
func cmdNameUnbind(ap *entitysdk.AppPeer, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: name unbind <name>")
	}
	name := args[0]

	existed := false
	if book, err := ap.ListLocalNames(); err == nil {
		for _, b := range book {
			if b.Name == name {
				existed = true
				break
			}
		}
	}
	if err := ap.UnbindLocalName(name); err != nil {
		return Result{}, fmt.Errorf("name unbind %q: %w", name, err)
	}
	if !existed {
		return MessageResult(fmt.Sprintf("%s was not bound (nothing removed)", name)), nil
	}
	return MessageResult(fmt.Sprintf("unbound %s", name)), nil
}

// cmdNameConfig shows the installed resolver-config: the dispatch rules
// that decide which backends a name shape is eligible for, and the chain
// that is consulted in priority order.
//
// **A config that fails validation is still printed**, with the refusal
// alongside it. That mirrors AppPeer.ResolverConfig's own contract: the
// entity is the operator's and is already in their tree, so what a
// load-time refusal denies is USE, not SIGHT — and an operator told only
// "your config is rejected" cannot see what to repair.
func cmdNameConfig(ap *entitysdk.AppPeer, _ []string) (Result, error) {
	cfg, found, err := ap.ResolverConfig()
	if !found && err == nil {
		return MessageResult("(no resolver-config installed — no name will resolve)"), nil
	}
	lines := []string{}
	if err != nil {
		lines = append(lines,
			"REFUSED AT LOAD — this config is shown for repair, not in use:",
			"  "+err.Error(),
			"")
	}
	lines = append(lines, "resolver_chain (consulted in ascending priority):")
	if len(cfg.ResolverChain) == 0 {
		lines = append(lines, "  (empty — every name reports chain_exhausted)")
	}
	for _, e := range cfg.ResolverChain {
		lines = append(lines, fmt.Sprintf("  [%d] %s", e.Priority, e.BackendKind))
	}
	lines = append(lines, "", "name_format_dispatch (a FILTER; it expresses no precedence):")
	if len(cfg.NameFormatDispatch) == 0 {
		lines = append(lines, "  (empty — the filter is DISABLED and every kind is eligible for every name)")
	}
	for _, d := range cfg.NameFormatDispatch {
		lines = append(lines, fmt.Sprintf("  %-12s → %s", d.Pattern, strings.Join(d.BackendKinds, ", ")))
	}
	return LinesResult(lines), nil
}

// resolvePeerRef maps a `@alias` / bare-alias / literal-peer-id argument
// to a peer-id, returning the id plus a human suffix naming how it was
// reached (empty when the argument was already an id).
//
// Unknown aliases pass through as literals rather than erroring. A
// peer-id is an opaque string and this function cannot tell a typo'd
// alias from an id for a peer we have never connected to — and binding a
// name to a peer you have not met is a legitimate, even expected, use of
// a name book. Refusing would block the case the feature is for.
func resolvePeerRef(sh *Shell, ref string) (peerID, via string) {
	bare := strings.TrimPrefix(ref, "@")
	if sh.Local != nil && sh.Local.Alias == bare {
		return sh.Local.PeerID, " (@" + bare + ", this peer)"
	}
	if pc, ok := sh.Conns[bare]; ok {
		return pc.PeerID, " (@" + bare + ")"
	}
	return ref, ""
}

// splitFlags separates `-flag value` pairs from positional arguments.
// Only the named flags are recognized; an unknown `-flag` is an error
// rather than a positional, because silently binding a name called
// "-notse" is worse than a usage message.
func splitFlags(args []string, known ...string) (rest []string, flags map[string]string, err error) {
	allowed := make(map[string]bool, len(known))
	for _, k := range known {
		allowed[k] = true
	}
	flags = make(map[string]string, len(known))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		k := strings.TrimLeft(a, "-")
		if !allowed[k] {
			return nil, nil, fmt.Errorf("unknown flag %q", a)
		}
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("flag %q needs a value", a)
		}
		flags[k] = args[i+1]
		i++
	}
	return rest, flags, nil
}

// splitNameFlags is splitFlags specialized to the single-positional
// `-pin` shape used by `name resolve`.
func splitNameFlags(args []string) (name string, flags map[string]string, err error) {
	rest, flags, err := splitFlags(args, "pin")
	if err != nil {
		return "", nil, err
	}
	if len(rest) > 0 {
		name = rest[0]
	}
	return name, flags, nil
}
