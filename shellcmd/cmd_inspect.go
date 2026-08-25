package shellcmd

// `inspect` command — operator surface for the workbench-go inspect
// package. v1 ships snapshot-only sub-commands that read substrate
// state without installing peer-builder hooks.
//
// Grammar (per the grammar review — chain operations group
// under their own sub-verb tree so future additions like `chain
// dispatch`, `chain tail`, `chain watch` slot in cleanly):
//
//   inspect entity <path>           — decode + dump entity bound at path
//   inspect dump <hash>             — decode + dump entity by content hash
//   inspect find <substring>        — enumerate path bindings matching substring
//   inspect errors                  — alias for `inspect chain errors`
//   inspect chain <subverb>         — chain operations:
//     inspect chain ls [-status S]  — list discoverable chain_ids with status
//     inspect chain show <id>       — chain footprint snapshot
//     inspect chain errors          — chain-error-lost markers
//
// Backward-compat: `inspect chain <id>` (id not matching a subverb)
// dispatches to `inspect chain show <id>`.
//
// Streaming sub-commands (tap, content, binding, wire, subscription)
// require hooks installed at peer construction; deferred to v2 once
// the shell session model supports long-lived hook installation.
//
// Per GUIDE-INSPECTABILITY v1.1 §7.1: these commands surface
// SYMPTOMS (what's at a path, where errors landed, which chain hit
// what). They do NOT replace source reading or classical debuggers
// for handler-body bugs — they shorten the path to the right handler.

import (
	"fmt"
	"strings"
	"time"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/inspect"
)

// inspectResolveArgs resolves alias references in path-typed args of
// `inspect entity <path>` and `inspect find <substr>`. find's substr
// is path-shaped enough to benefit from @alias expansion when the
// operator wants to scope a search.
func inspectResolveArgs(sh *Shell, args []string) []string {
	if len(args) < 2 {
		return args
	}
	out := make([]string, len(args))
	copy(out, args)
	switch args[0] {
	case "entity", "ent", "find":
		out[1] = string(sh.Resolve(out[1]))
	}
	return out
}

// cmdInspect dispatches `inspect <subcommand>`.
func cmdInspect(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: inspect <entity|dump|find|errors|chain> [args]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "entity", "ent":
		return cmdInspectEntity(sh, rest)
	case "dump", "hash":
		return cmdInspectDump(sh, rest)
	case "find":
		return cmdInspectFind(sh, rest)
	case "errors", "err":
		return cmdInspectErrorsListing(sh, rest)
	case "chain":
		return cmdInspectChainDispatch(sh, rest)
	case "watch":
		return cmdInspectWatch(sh, rest)
	default:
		return Result{}, fmt.Errorf("unknown inspect subcommand: %s (expected entity|dump|find|errors|chain|watch)", sub)
	}
}

// cmdInspectWatch: `inspect watch [pattern] [-n N] [-timeout DUR]` —
// wait for binding events under pattern and surface them with inspect-
// flavored decoding (chain-error markers decode to reason + chain_id).
//
// Bounded shape (same as `tail`): wait for N events or timeout, then
// return. Streaming output (`-f` follow mode) is the §4.2 #4 work
// requiring always-on taps + a streaming Result variant; backlogged.
//
// Convenience: `inspect watch errors` is shorthand for
// `inspect watch system/runtime/chain-errors/` with marker decoding.
func cmdInspectWatch(sh *Shell, args []string) (Result, error) {
	if sh.Local == nil {
		return Result{}, fmt.Errorf("no local peer")
	}

	pattern := ""
	n := 10
	timeout := 30 * time.Second
	decodeErrors := false

	if len(args) > 0 && (args[0] == "errors" || args[0] == "err") {
		pattern = "system/runtime/chain-errors/*"
		decodeErrors = true
		args = args[1:]
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n":
			if i+1 >= len(args) {
				return Result{}, fmt.Errorf("inspect watch: -n requires a value")
			}
			if _, err := fmt.Sscanf(args[i+1], "%d", &n); err != nil || n < 1 {
				return Result{}, fmt.Errorf("inspect watch: -n must be positive, got %q", args[i+1])
			}
			i++
		case "-timeout":
			if i+1 >= len(args) {
				return Result{}, fmt.Errorf("inspect watch: -timeout requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return Result{}, fmt.Errorf("inspect watch: -timeout %q: %w", args[i+1], err)
			}
			timeout = d
			i++
		default:
			if pattern == "" {
				pattern = args[i]
			} else {
				return Result{}, fmt.Errorf("inspect watch: unknown arg %q", args[i])
			}
		}
	}

	if pattern == "" {
		// Default to local peer's full namespace.
		pattern = ""
	}

	target := pattern
	if pattern != "" && !strings.HasPrefix(pattern, "/") {
		// Local-peer-relative pattern.
		target = "/" + sh.Local.PeerID + "/" + pattern
	}
	peerID, p, err := splitTargetForSubscribe(sh, target)
	if err != nil {
		return Result{}, err
	}
	if p == "" {
		p = "*"
	}

	sub, err := sh.Local.Peer.SubscribeAt(peerID, p, entitysdk.SubscribeOpts{})
	if err != nil {
		return Result{}, fmt.Errorf("inspect watch: subscribe %s: %w", p, err)
	}
	defer sub.Close()

	out := make([]string, 0, n)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for len(out) < n {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				goto done
			}
			out = append(out, formatInspectWatchEvent(sh.Local.Peer, ev, decodeErrors))
		case <-deadline.C:
			goto done
		}
	}
done:
	if len(out) == 0 {
		return MessageResult(fmt.Sprintf("(no events on %s within %s)", p, timeout)), nil
	}
	return LinesResult(out), nil
}

// formatInspectWatchEvent formats a change event with optional marker-
// body decoding. For chain-error markers, surfaces reason + chain_id
// from the body so the operator doesn't have to chase the hash.
func formatInspectWatchEvent(ap *entitysdk.AppPeer, ev entitysdk.ChangeEvent, decodeErrors bool) string {
	base := fmt.Sprintf("%-6s  %s", ev.EventType, ev.Path)
	if !ev.NewHash.IsZero() {
		base += "  " + ev.NewHash.String()[:12]
	}
	if !decodeErrors || ev.NewHash.IsZero() {
		return base
	}
	if !strings.Contains(ev.Path, "system/runtime/chain-errors/") {
		return base
	}
	ent, ok := ap.Store().GetByHash(ev.NewHash)
	if !ok {
		return base
	}
	dec, ok := inspect.DecodeChainErrorMarker(ent)
	if !ok {
		return base
	}
	return base + fmt.Sprintf("  chain=%s reason=%s step=%s",
		dec.ChainID, dec.Reason, dec.StepIndex)
}

// cmdInspectChainDispatch handles `inspect chain <subverb>` with
// backward-compat for `inspect chain <id>` where id isn't a known
// subverb.
func cmdInspectChainDispatch(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: inspect chain <ls|show|errors|dispatch> [args]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return cmdInspectChainLs(sh, rest)
	case "show":
		return cmdInspectChain(sh, rest)
	case "errors", "err":
		return cmdInspectErrorsListing(sh, rest)
	case "dispatch", "track", "run":
		return cmdInspectChainDispatchTrack(sh, rest)
	case "for":
		return cmdInspectChainFor(sh, rest)
	default:
		// Backward-compat: treat first arg as a chain_id.
		return cmdInspectChain(sh, args)
	}
}

// cmdInspectChainFor: `inspect chain for <hash>` — reverse causality.
// Returns the chain_id that produced the entity with the given content
// hash, when discoverable from substrate-honest sources (chain-error
// markers + suspended continuations). For entities outside those two
// source paths, returns "not found" — fuller attribution waits on the
// DispatchEvent.ChainID core-go addition routed upstream.
func cmdInspectChainFor(sh *Shell, args []string) (Result, error) {
	if len(args) != 1 {
		return Result{}, fmt.Errorf("usage: inspect chain for <hash>")
	}
	if sh.Local == nil {
		return Result{}, fmt.Errorf("no local peer")
	}
	h, err := parseHashHex(args[0])
	if err != nil {
		return Result{}, fmt.Errorf("inspect chain for: invalid hash %q: %w", args[0], err)
	}
	chainID, ok := inspect.ChainForHash(sh.Local.Peer, h)
	if !ok {
		return MessageResult(fmt.Sprintf(
			"no chain attribution for %s (entity not a chain-error marker or suspended continuation)",
			args[0])), nil
	}
	return LinesResult([]string{
		fmt.Sprintf("chain:  %s", chainID),
		fmt.Sprintf("source: %s", args[0]),
	}), nil
}

// cmdInspectChainDispatchTrack: `inspect chain dispatch <handler>
// <op> [resource] [json-params]` — wraps exec with pre/post chain-
// artifact snapshots and reports new chains that emerged. Closes
// the two-window discovery loop in a single command.
//
// Settle window: 200ms by default. The continuation engine, inbox
// handler, and subscription engine all bind their chain artifacts
// async; 200ms is enough for fire-and-die chains to leave a trace
// but short enough for interactive use. Pass -wait DUR to override.
func cmdInspectChainDispatchTrack(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: inspect chain dispatch <handler> <op> [resource] [json-params] [-wait DUR]")
	}
	if sh.Local == nil {
		return Result{}, fmt.Errorf("no local peer")
	}

	// Pull off -wait if present (anywhere in args).
	wait := 200 * time.Millisecond
	execArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "-wait" && i+1 < len(args) {
			if d, err := time.ParseDuration(args[i+1]); err == nil {
				wait = d
			}
			i++
			continue
		}
		execArgs = append(execArgs, args[i])
	}

	// Pre-snapshot chain set.
	before := chainIDSet(sh.Local.Peer)
	beforeMarkers := markerCount(sh.Local.Peer)

	// Run the dispatch via the same path cmdExec uses.
	execRes, execErr := cmdExec(sh, execArgs)

	// Wait for async chain artifacts to settle.
	time.Sleep(wait)

	// Post-snapshot.
	after := inspect.ListChains(sh.Local.Peer)
	afterMarkers := markerCount(sh.Local.Peer)

	// Lines from the dispatch result first.
	lines := []string{}
	if execErr != nil {
		lines = append(lines, fmt.Sprintf("dispatch ERROR: %v", execErr))
	} else if execRes.Kind == KindDispatch && execRes.Dispatch != nil {
		lines = append(lines, fmt.Sprintf("dispatch: %d", execRes.Dispatch.Status))
		if execRes.Dispatch.Result.Type != "" {
			lines = append(lines, fmt.Sprintf("response: %s (len=%d)",
				execRes.Dispatch.Result.Type, len(execRes.Dispatch.Result.Data)))
		}
	} else {
		lines = append(lines, "dispatch: (no response detail)")
	}

	// New chains diff.
	newChains := make([]inspect.ChainSummary, 0)
	for _, c := range after {
		if !before[c.ChainID] {
			newChains = append(newChains, c)
		}
	}

	lines = append(lines, "")
	if len(newChains) == 0 && afterMarkers == beforeMarkers {
		lines = append(lines, "chain artifacts: NO new chain_ids appeared in "+wait.String())
		lines = append(lines, "  Note: chains using fallback chain_id='none' may overlap")
		lines = append(lines, "  with pre-existing markers and not show as new. Check")
		lines = append(lines, "  the markers count delta (before=%d after=%d).")
	} else {
		lines = append(lines, fmt.Sprintf("chain artifacts: %d new chain_id%s + %d new marker%s",
			len(newChains), plural(len(newChains)),
			afterMarkers-beforeMarkers, plural(afterMarkers-beforeMarkers)))
		for _, c := range newChains {
			reason := c.LastReason
			if reason == "" {
				reason = "—"
			}
			lines = append(lines, fmt.Sprintf("  %s  status=%s  markers=%d  suspended=%d  reason=%s",
				truncate(c.ChainID, 40), c.Status, c.MarkerCount, c.SuspendedCount, reason))
		}
	}

	return LinesResult(lines), nil
}

// chainIDSet returns the set of currently-discoverable chain_ids
// (the same set ListChains would enumerate).
func chainIDSet(peer *entitysdk.AppPeer) map[string]bool {
	chains := inspect.ListChains(peer)
	out := make(map[string]bool, len(chains))
	for _, c := range chains {
		out[c.ChainID] = true
	}
	return out
}

// markerCount returns the total chain-error marker count, used to
// detect new markers under chain_ids that already existed (the
// "none" fallback chain_id case).
func markerCount(peer *entitysdk.AppPeer) int {
	return len(inspect.FindChainErrors(peer))
}

// cmdInspectEntity: `inspect entity <path>` — decode the entity bound
// at path. Path is alias-resolved at the dispatcher tier.
func cmdInspectEntity(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: inspect entity <path>")
	}
	if sh.Local == nil {
		return Result{}, fmt.Errorf("no local peer")
	}
	path := args[0]
	d := inspect.DumpEntityAt(sh.Local.Peer, path)
	if d == nil {
		return MessageResult(fmt.Sprintf("no binding at %s", path)), nil
	}
	return LinesResult(formatEntityDump(d)), nil
}

// cmdInspectDump: `inspect dump <hash>` — decode the entity at hash.
func cmdInspectDump(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: inspect dump <hex-hash>")
	}
	if sh.Local == nil {
		return Result{}, fmt.Errorf("no local peer")
	}
	h, err := parseHashHex(args[0])
	if err != nil {
		return Result{}, fmt.Errorf("parse hash: %w", err)
	}
	d := inspect.DumpEntityByHash(sh.Local.Peer, h)
	if d == nil {
		return MessageResult(fmt.Sprintf("no entity at hash %s", args[0])), nil
	}
	return LinesResult(formatEntityDump(d)), nil
}

// cmdInspectFind: `inspect find <substring>` — substring-match all
// path bindings. Equivalent to inspect.FindUnder, presented as a
// path list.
func cmdInspectFind(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: inspect find <substring>")
	}
	if sh.Local == nil {
		return Result{}, fmt.Errorf("no local peer")
	}
	entries := inspect.FindUnder(sh.Local.Peer, args[0])
	if len(entries) == 0 {
		return MessageResult(fmt.Sprintf("no bindings match %q", args[0])), nil
	}
	lines := make([]string, 0, len(entries)+1)
	lines = append(lines, fmt.Sprintf("%-72s  %s", "PATH", "HASH"))
	for _, e := range entries {
		short := e.Hash.String()
		if len(short) > 16 {
			short = short[:16] + "..."
		}
		lines = append(lines, fmt.Sprintf("%-72s  %s", truncate(e.Path, 72), short))
	}
	return LinesResult(lines), nil
}

// cmdInspectErrorsListing: `inspect errors` / `inspect chain errors`
// — enumerate chain-error-lost markers. Decoded with the v1.20
// path-scheme breakdown.
func cmdInspectErrorsListing(sh *Shell, _ []string) (Result, error) {
	if sh.Local == nil {
		return Result{}, fmt.Errorf("no local peer")
	}
	entries := inspect.FindChainErrors(sh.Local.Peer)
	if len(entries) == 0 {
		return MessageResult("no chain-error markers"), nil
	}
	lines := []string{
		fmt.Sprintf("%d chain-error-lost markers", len(entries)),
		"",
		fmt.Sprintf("%-12s  %-32s  %-32s  %s", "KIND", "CHAIN_ID", "STEP_INDEX", "REASON"),
	}
	for _, e := range entries {
		kind, chainID, step, reason := decodeErrorPath(e.Path)
		lines = append(lines, fmt.Sprintf("%-12s  %-32s  %-32s  %s",
			kind, truncate(chainID, 32), truncate(step, 32), reason))
	}
	return LinesResult(lines), nil
}

// cmdInspectChainLs: `inspect chain ls [-status <status>]` —
// enumerate discoverable chain_ids with status inferred from
// artifact mix. Substrate-honest scope: shows failed (markers
// present) and suspended (suspended continuations present); does
// NOT show installed forward/join continuations because their
// bodies don't carry chain_id (use `continuation ls` for those).
func cmdInspectChainLs(sh *Shell, args []string) (Result, error) {
	if sh.Local == nil {
		return Result{}, fmt.Errorf("no local peer")
	}

	statusFilter := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "-status" && i+1 < len(args) {
			statusFilter = args[i+1]
			i++
		}
	}

	chains := inspect.ListChains(sh.Local.Peer)
	if statusFilter != "" {
		filtered := make([]inspect.ChainSummary, 0, len(chains))
		for _, c := range chains {
			if c.Status.String() == statusFilter {
				filtered = append(filtered, c)
			}
		}
		chains = filtered
	}

	if len(chains) == 0 {
		if statusFilter != "" {
			return MessageResult(fmt.Sprintf("no chains with status=%s", statusFilter)), nil
		}
		return MessageResult(
			"no chain artifacts in local store (no chain-error markers, no suspended continuations).\n" +
				"  Note: installed forward/join continuations are invisible to `chain ls` (their bodies don't carry chain_id).\n" +
				"  Use `continuation ls` for path-based enumeration of installed chains."), nil
	}

	lines := []string{
		fmt.Sprintf("%d chain%s discovered:", len(chains), plural(len(chains))),
		"",
		fmt.Sprintf("%-40s  %-10s  %-9s  %-9s  %s",
			"CHAIN_ID", "STATUS", "MARKERS", "SUSPEND'D", "LAST_REASON"),
	}
	for _, c := range chains {
		reason := c.LastReason
		if reason == "" {
			reason = "—"
		}
		lines = append(lines, fmt.Sprintf("%-40s  %-10s  %-9d  %-9d  %s",
			truncate(c.ChainID, 40), c.Status.String(),
			c.MarkerCount, c.SuspendedCount, reason))
	}
	return LinesResult(lines), nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// cmdInspectChain: `inspect chain <chain-id>` — walk every entity
// associated with a chain_id: chain-error markers, continuations,
// path bindings. Per v1.1 §9 #8 chain-participation invariants, the
// completion contract is what makes "neither succeeded nor recorded
// a marker" detectable as anomalous.
func cmdInspectChain(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: inspect chain <chain-id>")
	}
	if sh.Local == nil {
		return Result{}, fmt.Errorf("no local peer")
	}
	chainID := args[0]
	trace := inspect.TraceChain(sh.Local.Peer, chainID)

	lines := []string{trace.Summary(), ""}

	if len(trace.Errors) > 0 {
		lines = append(lines, fmt.Sprintf("--- %d chain-error markers ---", len(trace.Errors)))
		for _, m := range trace.Errors {
			lines = append(lines,
				fmt.Sprintf("  [%s] step=%s reason=%s status=%d code=%s",
					m.Kind, m.StepIndex, m.Reason, m.Body.Status, m.Body.Code),
				fmt.Sprintf("    failed_uri: %s", m.Body.TargetURI),
				fmt.Sprintf("    marker:     %s", m.Hash),
			)
		}
		lines = append(lines, "")
	}

	if len(trace.Continuations) > 0 {
		lines = append(lines, fmt.Sprintf("--- %d continuation entries ---", len(trace.Continuations)))
		for _, c := range trace.Continuations {
			lines = append(lines, fmt.Sprintf("  %s → %s:%s", c.Path, c.Body.Target, c.Body.Operation))
		}
		lines = append(lines, "")
	}

	if len(trace.PathBindings) > 0 {
		lines = append(lines, fmt.Sprintf("--- %d path bindings carrying chain_id ---", len(trace.PathBindings)))
		for i, b := range trace.PathBindings {
			if i >= 20 {
				lines = append(lines, fmt.Sprintf("  ... (%d more)", len(trace.PathBindings)-20))
				break
			}
			lines = append(lines, fmt.Sprintf("  %s", b.Path))
		}
		lines = append(lines, "")
	}

	if len(trace.Errors) == 0 && len(trace.Continuations) == 0 && len(trace.PathBindings) == 0 {
		// Per v1.1 §9 #8 honesty caveat: "neither success nor failure
		// recorded" is the anomalous case once extensions declare
		// completion contracts.
		lines = append(lines,
			"NO ARTIFACTS FOUND for chain_id="+chainID,
			"",
			"  Per v1.1 §9 #8: this is anomalous IF the chain was dispatched.",
			"  May indicate silent failure (dispatcher-level drop), or that",
			"  the chain_id was never used / has expired from the local store.",
		)
	}

	return LinesResult(lines), nil
}

// formatEntityDump renders an EntityDump as indented lines.
func formatEntityDump(d *inspect.EntityDump) []string {
	lines := []string{
		fmt.Sprintf("path:  %s", d.Path),
		fmt.Sprintf("type:  %s", d.Type),
		fmt.Sprintf("hash:  %s", d.Hash),
		fmt.Sprintf("len:   %d", d.Len),
		"data:",
	}
	rendered := inspect.RenderCBOR(d.Data, 0)
	for _, line := range strings.Split(rendered, "\n") {
		lines = append(lines, "  "+line)
	}
	return lines
}

// decodeErrorPath extracts the four canonical chain-error path
// segments from a runtime/chain-errors path. Tolerant of namespace
// prefix.
func decodeErrorPath(path string) (kind, chainID, step, reason string) {
	idx := strings.Index(path, "system/runtime/chain-errors/")
	if idx < 0 {
		return "", "", "", ""
	}
	tail := path[idx+len("system/runtime/chain-errors/"):]
	parts := strings.Split(tail, "/")
	if len(parts) >= 4 {
		return parts[0], parts[1], parts[2], parts[3]
	}
	return "", "", "", ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
