package shellcmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/fetch"
	"entity-workbench-go/workbench"
)

// cmd_browse.go — the operator's half of the journey: pin a name
// authority, see what it carries, go somewhere, come back.
//
// `site verify` (cmd_site.go) is the inspector: point it at an origin
// you already know and it tells you which link of the chain held. These
// verbs are the *browser*: you start holding one pinned peer-id and end
// up looking at a page, and the chain is printed beside the page rather
// than instead of it.
//
// **Three questions the shell could not answer before this file:**
//
//	registry ls        what names exist
//	open <name>        take me there
//	where              what am I looking at, and what verified it
//
// The split into `registry` and `browse` follows the nouns: a registry
// is a name authority you pin, and browsing is what you do afterwards.
// `open` is registered as a top-level alias for `browse open` because it
// is the verb you type twenty times a session and `site verify` set the
// precedent that the common operation gets the short spelling.

// browserOf returns the workspace's browser, creating it on first use.
//
// It lives on the workspace rather than the shell because the pin is a
// **trust decision**, and a trust decision that quietly differed between
// two shells in one workspace would be a very good way to confuse
// someone about which authority answered.
func browserOf(sh *Shell) *workbench.BrowseModel {
	if sh.Browser == nil {
		sh.Browser = workbench.NewBrowseModel(nil)
	}
	return sh.Browser
}

const registryUsage = `registry <pin|ls|show|resolve|unpin|issue|revoke> [args]

Consuming a registry:
  pin <origin> [-peer <id>] [-pin-* ...]   trust a name authority
  ls                                        the names it carries (walks the signed root)
  show                                      what is pinned, and how fresh it is
  resolve <name>                            one name, with every §6a.4 check reported
  unpin                                     forget it

BEING a registry (§6a.8 curated registration — operator tooling, no live protocol;
this peer's own key signs, so this peer's peer-id is what a consumer pins):
  issue <name> <target-peer-id> -target-origin <url> [-ttl 720h] [-note ...]
                                            mint + sign a binding into our tree
  revoke <binding-hash> [-reason ...]       publish a revocation targeting it

  Then "entity-publish -prefix system/" emits it as a static registry.
  The prefix is "system/", NOT "system/registry/": a binding's signature lives at
  system/signature/{hex} (V7 §5.2), outside every system/registry/ prefix, so the
  narrower publish emits bindings with no signatures over them — enumeration works
  and every resolve 404s. Measured; routed to arch.

The pin is the ONE thing you supply out of band. For an identity-form
peer-id the pin IS the key (§6a.5), so nothing is fetched to know who the
registry is — which is exactly why the host serving its bytes is trusted
for nothing.

Pinned layout, for a registry that serves no transport-profile (which is
conformant — NETWORK §6.5.4 puts profile distribution out-of-band in v1):
  -pin-tree <p> -pin-content <p> -pin-manifest <p> -pin-layout <name>
  -pin-leaf <suffix> -pin-listing <suffix>`

func cmdRegistry(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		return MessageResult(registryUsage), nil
	}
	switch args[0] {
	case "pin":
		return cmdRegistryPin(sh, args[1:])
	case "ls", "list":
		return cmdRegistryLs(sh)
	case "show":
		return cmdRegistryShow(sh)
	case "resolve":
		return cmdRegistryResolve(sh, args[1:])
	case "issue":
		return cmdRegistryIssue(sh, args[1:])
	case "revoke":
		return cmdRegistryRevoke(sh, args[1:])
	case "unpin":
		sh.Browser = nil
		return MessageResult("registry unpinned — names will not resolve until one is pinned again, " +
			"and a browser must not invent a name authority"), nil
	case "help", "-h", "--help":
		return MessageResult(registryUsage), nil
	default:
		return Result{}, fmt.Errorf("registry: unknown subcommand %q\n%s", args[0], registryUsage)
	}
}

func cmdRegistryPin(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		return Result{}, fmt.Errorf("registry pin: an origin is required\n%s", registryUsage)
	}
	origin := args[0]
	rest := args[1:]

	var (
		peerID string
		ep     types.TransportEndpoint
		pinned bool
	)
	ep.TreeLeafSuffix, ep.TreeListingSuffix = ".bin", ".list"

	need := func(i int, flag string) (string, error) {
		if i+1 >= len(rest) {
			return "", fmt.Errorf("registry pin: %s needs a value", flag)
		}
		return rest[i+1], nil
	}
	for i := 0; i < len(rest); i++ {
		var err error
		switch rest[i] {
		case "-peer":
			peerID, err = need(i, "-peer")
			i++
		case "-pin-tree":
			ep.TreeURLPrefix, err = need(i, "-pin-tree")
			i, pinned = i+1, true
		case "-pin-content":
			ep.ContentURLPrefix, err = need(i, "-pin-content")
			i, pinned = i+1, true
		case "-pin-manifest":
			ep.ManifestURLPrefix, err = need(i, "-pin-manifest")
			i, pinned = i+1, true
		case "-pin-layout":
			ep.ContentLayout, err = need(i, "-pin-layout")
			i, pinned = i+1, true
		case "-pin-leaf":
			ep.TreeLeafSuffix, err = need(i, "-pin-leaf")
			i++
		case "-pin-listing":
			ep.TreeListingSuffix, err = need(i, "-pin-listing")
			i++
		default:
			return Result{}, fmt.Errorf("registry pin: unknown flag %q\n%s", rest[i], registryUsage)
		}
		if err != nil {
			return Result{}, err
		}
	}
	if pinned && peerID == "" {
		return Result{}, fmt.Errorf("registry pin: -peer is required with a pinned layout — " +
			"it is the key every signature is checked against, so there is no useful pin without it")
	}

	b := browserOf(sh)
	var perr error
	if pinned {
		perr = b.PinRegistry(context.Background(), origin, peerID, &ep)
	} else {
		perr = b.PinRegistry(context.Background(), origin, peerID, nil)
	}
	if perr != nil {
		sh.Browser = nil
		return Result{}, perr
	}
	out := b.Render()
	lines := []string{
		fmt.Sprintf("pinned   %s", out.Registry),
		fmt.Sprintf("origin   %s", out.RegistryOrigin),
	}
	if out.RegistryDiscovered {
		lines = append(lines, "layout   discovered from the origin's transport-profile")
	} else {
		lines = append(lines, "layout   PINNED by you — this origin serves no transport-profile, "+
			"so a wrong pin and a withholding origin look the same from here")
	}
	lines = append(lines, "", "Nothing has been verified yet: a pin is a key, not a claim about an "+
		"origin. Run `registry ls` to walk what it has signed.")
	return LinesResult(lines), nil
}

func cmdRegistryLs(sh *Shell) (Result, error) {
	b := browserOf(sh)
	if b.Registry() == "" {
		return Result{}, fmt.Errorf("registry ls: nothing pinned — `registry pin <origin> -peer <id>` first")
	}
	if err := b.RefreshNames(context.Background()); err != nil {
		return Result{}, err
	}
	out := b.Render()

	lines := []string{out.NamesAuthority, ""}
	for _, row := range out.Names {
		switch {
		case !row.Committed:
			lines = append(lines, fmt.Sprintf("  !!   %-32s advertised in the served listing, "+
				"NOT committed by the signed root", row.Name))
		case !row.Listed:
			lines = append(lines, fmt.Sprintf("  ok   %-32s %s  (committed but omitted from the "+
				"served menu)", row.Name, row.BindingHash))
		default:
			lines = append(lines, fmt.Sprintf("  ok   %-32s %s", row.Name, row.BindingHash))
		}
	}
	if out.NamesNote != "" {
		lines = append(lines, "", "note: "+wrapDetail(out.NamesNote))
	}
	lines = append(lines, "",
		fmt.Sprintf("registry root verified as of %s.", out.RegistryFresh),
		"A served listing can hide a name undetectably; a walk cannot hide one without failing.",
		"These rows are the walk (§6a.3a). Resolving any of them is a separate verification —",
		"`registry resolve <name>` shows it, `open <name>` does it and goes there.")
	return LinesResult(lines), nil
}

func cmdRegistryShow(sh *Shell) (Result, error) {
	b := browserOf(sh)
	if b.Registry() == "" {
		return MessageResult("no registry pinned"), nil
	}
	out := b.Render()
	lines := []string{
		fmt.Sprintf("registry %s", out.Registry),
		fmt.Sprintf("origin   %s", out.RegistryOrigin),
		fmt.Sprintf("layout   %s", pinnedOrDiscovered(out.RegistryDiscovered)),
	}
	if out.RegistryFresh != "" {
		lines = append(lines, fmt.Sprintf("root     verified as of %s", out.RegistryFresh))
	} else {
		lines = append(lines, "root     not walked yet (`registry ls`)")
	}
	if len(out.Names) > 0 {
		lines = append(lines, fmt.Sprintf("names    %d committed", len(out.Names)))
	}
	return LinesResult(lines), nil
}

func cmdRegistryResolve(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		return Result{}, fmt.Errorf("registry resolve: a name is required")
	}
	b := browserOf(sh)
	if b.Registry() == "" {
		return Result{}, fmt.Errorf("registry resolve: nothing pinned")
	}
	// Navigating is what actually runs §6a.4 end to end; resolve reports
	// the same chain without keeping the page. Deliberately the same code
	// path — a "resolve" that checked less than "open" would be a
	// diagnostic that disagrees with the thing it diagnoses.
	err := b.Open(context.Background(), args[0])
	lines := renderChain(b.Render())
	if err != nil {
		return LinesResult(lines), nil
	}
	return LinesResult(lines), nil
}

// cmdRegistryIssue is §6a.8: this peer signs a name → peer-id binding.
//
// The target's transports are **read from the target**, never composed
// here: `-target-origin` is fetched and its advertised `transport-profile`
// is what the binding carries. A registry asserting a reach it read is
// asserting something it checked; one asserting a reach it built from a
// convention is asserting something it invented, and the consumer cannot
// tell the two apart.
func cmdRegistryIssue(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("registry issue: need <name> <target-peer-id>\n%s", registryUsage)
	}
	if sh.Local == nil || sh.Local.Peer == nil {
		return Result{}, fmt.Errorf("registry issue: no local peer — issuing signs with THIS peer's key")
	}
	name, target := args[0], args[1]

	var (
		targetOrigin string
		ttl          = 30 * 24 * time.Hour
	)
	rest := args[2:]
	for i := 0; i < len(rest); i++ {
		need := func(flag string) (string, error) {
			if i+1 >= len(rest) {
				return "", fmt.Errorf("registry issue: %s needs a value", flag)
			}
			return rest[i+1], nil
		}
		var err error
		switch rest[i] {
		case "-target-origin":
			targetOrigin, err = need("-target-origin")
			i++
		case "-ttl":
			var raw string
			if raw, err = need("-ttl"); err == nil {
				ttl, err = time.ParseDuration(raw)
			}
			i++
		default:
			return Result{}, fmt.Errorf("registry issue: unknown flag %q\n%s", rest[i], registryUsage)
		}
		if err != nil {
			return Result{}, err
		}
	}
	if targetOrigin == "" {
		return Result{}, fmt.Errorf("registry issue: -target-origin is required. §6a.3 makes " +
			"`transports` a MUST on a peer-issued binding, and the honest source is the target's " +
			"own advertised transport-profile — a reach this registry read rather than composed")
	}

	layout, err := fetch.LoadLayout(context.Background(), targetOrigin, nil)
	if err != nil {
		return Result{}, fmt.Errorf("registry issue: reading %s's advertised transport-profile: %w",
			targetOrigin, err)
	}
	if layout.PeerID != target {
		return Result{}, fmt.Errorf("registry issue: %s advertises peer %s, you named %s — "+
			"refusing to sign a binding whose target and whose reach disagree",
			targetOrigin, layout.PeerID, target)
	}

	issued, err := sh.Local.Peer.IssueBinding(entitysdk.IssueOpts{
		Name:         name,
		TargetPeerID: target,
		Transports: []types.HTTPPollProfileData{{
			PeerID:        layout.PeerID,
			TransportType: "http-poll",
			Endpoint:      layout.Endpoint,
			SupportedOps:  []string{types.OpTreeGet, types.OpContentGet, types.OpManifestGet},
			Freshness:     layout.Freshness,
			SignedPointer: layout.SignedPointer,
		}},
		TTL: ttl,
	})
	if err != nil {
		return Result{}, err
	}
	return LinesResult([]string{
		fmt.Sprintf("issued   %s → %s", issued.Name, target),
		fmt.Sprintf("binding  %s", issued.BindingHash),
		fmt.Sprintf("signed   by this peer (%s) — the peer-id a consumer pins", sh.Local.Peer.PeerID()),
		fmt.Sprintf("valid    %s → %s",
			issued.IssuedAt.Format(time.RFC3339), issued.ExpiresAt.Format(time.RFC3339)),
		fmt.Sprintf("reach    read from %s, carried %s", targetOrigin, issued.TransportShape),
		"",
		"bound at:",
		"  " + issued.BodyPath,
		"  " + issued.ByNamePath,
		"  " + issued.SigPath,
		"",
		"Nothing is published yet — this wrote three entities into THIS peer's tree.",
		"`entity-publish -prefix system/` emits them as a static registry (§7.4).",
	}), nil
}

func cmdRegistryRevoke(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		return Result{}, fmt.Errorf("registry revoke: need <binding-hash>")
	}
	if sh.Local == nil || sh.Local.Peer == nil {
		return Result{}, fmt.Errorf("registry revoke: no local peer")
	}
	// The hash is accepted in the WIRE hex form (66 chars, format byte
	// included) — the same string `registry issue` printed and the same
	// one a content URL is built from (EXTENSION-SUBSTITUTE 1.3, ruled
	// 2026-08-21). The `ecf-sha256:` display prefix is stripped if a
	// user pasted that instead, because they will.
	raw := strings.TrimPrefix(args[0], "ecf-sha256:")
	h, err := hash.ParseHex(raw)
	if err != nil {
		return Result{}, fmt.Errorf("registry revoke: %q is not a content hash: %w", args[0], err)
	}
	reason := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "-reason" && i+1 < len(args) {
			reason = args[i+1]
			i++
		}
	}
	revHash, err := sh.Local.Peer.RevokeBinding(h, reason, time.Time{})
	if err != nil {
		return Result{}, err
	}
	return LinesResult([]string{
		fmt.Sprintf("revoked  %s", h),
		fmt.Sprintf("entity   %s", revHash),
		"",
		"Publishing a revocation is not the same as revoking. An origin can withhold it",
		"indefinitely, and a withheld revocation is byte-identical at a consumer to one that",
		"was never issued (§6a.1a). What actually bounds a compromised binding is its TTL;",
		"this shortens the window for consumers whose origin is honest.",
	}), nil
}

const browseUsage = `browse <open|back|forward|where|sites> [args]

  open <address>   go there. address is <name>[/<site>[/<page>]] or a peer-id,
                   optionally spelled entity://name/site/page
  back / forward   history, across publishers
  where            what is on screen and the chain that put it there
  sites            every site the current target's SIGNED ROOT commits to

`

func cmdBrowse(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		return MessageResult(browseUsage), nil
	}
	switch args[0] {
	case "open", "go":
		return cmdBrowseOpen(sh, args[1:])
	case "back":
		return cmdBrowseStep(sh, -1)
	case "forward":
		return cmdBrowseStep(sh, +1)
	case "where":
		return cmdBrowseWhere(sh)
	case "sites":
		return cmdBrowseSites(sh)
	case "help", "-h", "--help":
		return MessageResult(browseUsage), nil
	default:
		return Result{}, fmt.Errorf("browse: unknown subcommand %q\n%s", args[0], browseUsage)
	}
}

func cmdBrowseOpen(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		return Result{}, fmt.Errorf("open: an address is required\n%s", browseUsage)
	}
	b := browserOf(sh)
	for i := 1; i < len(args); i++ {
		if args[i] == "-origin" && i+1 < len(args) {
			// A peer-id address has no registry telling us where to
			// fetch from, and nothing in a peer-id says where its bytes
			// live (NETWORK §6.5.4). This is how an operator supplies it.
			b.SetTargetOrigin(args[i+1])
			i++
		}
	}
	err := b.Open(context.Background(), args[0])
	out := b.Render()
	lines := renderPage(out)
	if err != nil {
		return LinesResult(lines), nil
	}
	return LinesResult(lines), nil
}

func cmdBrowseStep(sh *Shell, delta int) (Result, error) {
	b := browserOf(sh)
	var err error
	if delta < 0 {
		err = b.Back(context.Background())
	} else {
		err = b.Forward(context.Background())
	}
	if err != nil {
		return Result{}, err
	}
	out := b.Render()
	if out.Address == "" {
		return MessageResult("nowhere to go"), nil
	}
	return LinesResult(renderPage(out)), nil
}

func cmdBrowseWhere(sh *Shell) (Result, error) {
	b := browserOf(sh)
	out := b.Render()
	if out.Address == "" {
		return MessageResult("not browsing anything yet — `open <name>`"), nil
	}
	lines := []string{
		fmt.Sprintf("address  %s", out.Address),
		fmt.Sprintf("host     %s", out.Host),
	}
	if out.Registry != "" {
		lines = append(lines, fmt.Sprintf("via      registry %s (%s)", out.Registry,
			pinnedOrDiscovered(out.RegistryDiscovered)))
	} else {
		lines = append(lines, "via      nothing — you supplied a peer-id, so no name authority vouched for this target")
	}
	lines = append(lines, "")
	lines = append(lines, renderChain(out)...)
	return LinesResult(lines), nil
}

func cmdBrowseSites(sh *Shell) (Result, error) {
	out := browserOf(sh).Render()
	if out.Address == "" {
		return MessageResult("not browsing anything yet — `open <name>`"), nil
	}
	if len(out.Sites) == 0 {
		return MessageResult("the target's signed root commits to no sites/ subtree"), nil
	}
	lines := []string{
		fmt.Sprintf("%d site(s), from the walk of %s's signed root:", len(out.Sites), out.Host),
		"",
	}
	for _, s := range out.Sites {
		mark := "  "
		if s == out.Site {
			mark = "* "
		}
		lines = append(lines, "  "+mark+s)
	}
	lines = append(lines, "",
		"This list is the key set the publisher signed, not a directory listing they served.")
	return LinesResult(lines), nil
}

// renderPage prints the page and, under it, the chain.
//
// **The order is the argument.** The page is what the user asked for and
// goes first; the chain goes underneath, always, never behind a flag —
// an inspector you have to opt into is an inspector nobody runs. It is
// short enough (one line a step) that it reads as provenance rather than
// as an interruption.
func renderPage(out workbench.BrowseOutput) []string {
	var lines []string
	if out.Err != "" {
		lines = append(lines, "REFUSED: "+wrapDetail(out.Err), "")
		lines = append(lines, renderChain(out)...)
		return lines
	}

	if out.Content.SiteTitle != "" {
		lines = append(lines, out.Content.SiteTitle)
		lines = append(lines, strings.Repeat("=", len(out.Content.SiteTitle)))
	}
	if len(out.Content.Breadcrumbs) > 0 {
		var crumbs []string
		for _, c := range out.Content.Breadcrumbs {
			crumbs = append(crumbs, c.Label)
		}
		lines = append(lines, strings.Join(crumbs, " / "))
	}
	lines = append(lines, "")
	lines = append(lines, strings.Split(out.Content.BodyMarkdown, "\n")...)

	if len(out.Content.Nav) > 0 {
		var nav []string
		for _, n := range out.Content.Nav {
			nav = append(nav, n.Label)
		}
		lines = append(lines, "", "nav: "+strings.Join(nav, " · "))
	}
	if out.SiteDefaulted {
		lines = append(lines, "",
			fmt.Sprintf("(%s publishes %d sites and the address named none, so this is %q — "+
				"first in byte order, not a front door anyone declared. `browse sites` for the rest.)",
				out.Host, len(out.Sites), out.Site))
	}
	lines = append(lines, "", strings.Repeat("─", 72))
	lines = append(lines, renderChain(out)...)
	return lines
}

// renderChain is the trust rail, in a terminal.
//
// One line per step with its verdict and detail; `proves` only on
// request would defeat the purpose, so it is folded in under any step
// that is not plainly ok. The freshness line is last and never says the
// bare word "verified".
func renderChain(out workbench.BrowseOutput) []string {
	if len(out.Steps) == 0 {
		return []string{"(no chain: nothing has been navigated)"}
	}
	lines := make([]string, 0, len(out.Steps)+3)
	for _, s := range out.Steps {
		mark := map[workbench.ConsumeStatus]string{
			workbench.StepOK:      "  ok  ",
			workbench.StepFailed:  " FAIL ",
			workbench.StepSkipped: " skip ",
			workbench.StepPending: "  ..  ",
		}[s.Status]
		lines = append(lines, fmt.Sprintf("[%s] %-18s %s", mark, s.Name, s.Detail))
		if s.Err != "" {
			lines = append(lines, "         "+wrapDetail(s.Err))
		}
		if s.Status != workbench.StepOK && s.Proves != "" {
			lines = append(lines, "         "+wrapDetail(s.Proves))
		}
	}
	if out.Freshness != "" {
		lines = append(lines, "", wrapDetail(out.Freshness))
	}
	lines = append(lines, "", "`browse where` repeats this chain with what each step proves.")
	return lines
}

func pinnedOrDiscovered(discovered bool) string {
	if discovered {
		return "discovered from the origin's transport-profile"
	}
	return "pinned by hand"
}
