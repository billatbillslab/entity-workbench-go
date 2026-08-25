package shellcmd

import (
	"context"
	"fmt"
	"strings"

	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/fetch"
	"entity-workbench-go/workbench"
)

// cmdSite is the operator surface over the CDN corridor's consume stack:
// point it at a published origin and it walks the whole verification
// chain and reports which link held.
//
// **It renders the chain, not the site.** `entity-browser-rust`'s Site
// Browser renders the pages — the reader's surface. This is the
// inspector's: an operator asking *is this origin serving what it
// signed* gets six verdicts and what each one proves, because
// "verified" is not one fact and a single green tick teaches that it is.
//
// The one step that cannot be faked is the trie walk. Every other step
// in this chain is satisfiable by an origin serving a correctly-signed
// root that commits to nothing — the shape `entity-core-go` shipped for
// a week (fixed at their `dabd076`), which every per-leaf consumer in
// this cohort reported green against.
func cmdSite(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		return MessageResult(siteUsage), nil
	}
	switch args[0] {
	case "verify":
		return cmdSiteVerify(sh, args[1:])
	case "help", "-h", "--help":
		return MessageResult(siteUsage), nil
	default:
		return Result{}, fmt.Errorf("site: unknown subcommand %q\n%s", args[0], siteUsage)
	}
}

const siteUsage = `site verify <origin> [flags]  — walk a published origin's whole verification chain

  -peer <id>        expected publisher; a cross-check with a discovered layout,
                    REQUIRED with a pinned one (it is the key the signature
                    verifies against)
  -absent <key>     absent-key control probe (runs only after enumeration)
  -no-bodies        skip fetching every committed leaf
  -no-reconcile     skip checking the trie against the advertised leaf URLs
  -keys             list every committed key

Pinned layout, for origins that serve no transport-profile (conformant —
§6.5.4 makes profile distribution out-of-band in v1):
  -pin-tree <p> -pin-content <p> -pin-manifest <p> -pin-layout <name>
  -pin-leaf <suffix> -pin-listing <suffix>`

func cmdSiteVerify(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		return Result{}, fmt.Errorf("site verify: an origin is required\n%s", siteUsage)
	}
	origin := args[0]
	rest := args[1:]

	var (
		peerID   string
		absent   string
		listKeys bool
		ep       types.TransportEndpoint
		pinned   bool
	)
	opts := fetch.ConsumeOpts{Bodies: true, Reconcile: true}
	ep.TreeLeafSuffix, ep.TreeListingSuffix = ".bin", ".list"

	need := func(i int, flag string) (string, error) {
		if i+1 >= len(rest) {
			return "", fmt.Errorf("site verify: %s needs a value", flag)
		}
		return rest[i+1], nil
	}
	for i := 0; i < len(rest); i++ {
		var err error
		switch rest[i] {
		case "-peer":
			peerID, err = need(i, "-peer")
			i++
		case "-absent":
			absent, err = need(i, "-absent")
			i++
		case "-no-bodies":
			opts.Bodies = false
		case "-no-reconcile":
			opts.Reconcile = false
		case "-keys":
			listKeys = true
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
			return Result{}, fmt.Errorf("site verify: unknown flag %q\n%s", rest[i], siteUsage)
		}
		if err != nil {
			return Result{}, err
		}
	}
	opts.AbsentProbe = absent

	m := workbench.NewConsumeModel(nil)
	m.SetOrigin(origin)
	m.SetPeerID(peerID)
	m.SetOpts(opts)
	if pinned {
		m.Pin(&ep)
	}
	out := m.Verify(context.Background())

	return LinesResult(renderConsume(out, listKeys)), nil
}

// renderConsume turns the model's output into the operator's view.
//
// The `proves` line under every green step is the point of the layout,
// not decoration: a chain rendered as six ticks says "verified" six
// times, and five of those six are true of an origin that is lying.
func renderConsume(out workbench.ConsumeOutput, listKeys bool) []string {
	lines := []string{
		fmt.Sprintf("origin   %s", out.Origin),
		fmt.Sprintf("peer     %s", dashIfEmpty(out.PeerID)),
	}
	if out.Discovered {
		lines = append(lines, "layout   discovered from the origin's transport-profile")
	} else {
		lines = append(lines, "layout   PINNED by you — this origin serves no transport-profile, so a "+
			"wrong pin and a withholding origin look the same")
	}
	lines = append(lines, "")

	for _, s := range out.Steps {
		mark := map[workbench.ConsumeStatus]string{
			workbench.StepOK:      "  ok  ",
			workbench.StepFailed:  " FAIL ",
			workbench.StepSkipped: " skip ",
			workbench.StepPending: "  ..  ",
		}[s.Status]
		lines = append(lines, fmt.Sprintf("[%s] %-20s %s", mark, s.Name, s.Detail))
		if s.Err != "" {
			lines = append(lines, "         "+wrapDetail(s.Err))
		}
		if s.Status == workbench.StepOK && s.Proves != "" {
			lines = append(lines, "         proves: "+wrapDetail(s.Proves))
		}
	}

	if out.RootHash != "" {
		lines = append(lines, "",
			fmt.Sprintf("root     %s", out.RootHash),
			fmt.Sprintf("prefix   %q  →  %s (EXTENSION-TREE §3.3 absolute form)",
				out.Prefix, dashIfEmpty(out.AbsolutePrefix)),
			fmt.Sprintf("scope    seq %d · %d CHAMP nodes · %d committed keys",
				out.Seq, out.Nodes, out.KeysTotal))
	}

	if listKeys {
		lines = append(lines, "")
		for _, k := range out.Keys {
			switch {
			case k.Err != "":
				lines = append(lines, fmt.Sprintf("  FAIL %-44s %s", k.Key, k.Err))
			case k.Reconciled:
				lines = append(lines, fmt.Sprintf("  ok   %-44s %s (trie == advertised leaf)",
					k.Key, shortSiteHash(k.Hash)))
			default:
				lines = append(lines, fmt.Sprintf("  ok   %-44s %s %s", k.Key, shortSiteHash(k.Hash), k.Type))
			}
		}
	}

	lines = append(lines, "")
	switch {
	case out.Incomplete:
		lines = append(lines,
			"VERDICT: INCOMPLETE WALK — this origin does not serve the closure its own signed",
			"root commits to. That is a statement about the PUBLISHER, not about reachability:",
			"a per-leaf fetch of any single page would have succeeded.")
	case out.Err != "":
		lines = append(lines, "VERDICT: FAILED — "+wrapDetail(out.Err))
	case out.Verified:
		lines = append(lines, "VERDICT: "+out.FreshnessNote())
	default:
		lines = append(lines, fmt.Sprintf("VERDICT: %d of %d committed keys failed verification",
			out.KeysFailed, out.KeysTotal))
	}
	if out.HasRun {
		lines = append(lines, fmt.Sprintf("(%s)", out.Duration))
	}
	return lines
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func shortSiteHash(s string) string {
	if len(s) > 26 {
		return s[:26] + "…"
	}
	return s
}

// wrapDetail folds a long explanation onto continuation lines so a
// terminal at 100 columns does not swallow the second half of a reason.
func wrapDetail(s string) string {
	const width = 84
	if len(s) <= width {
		return s
	}
	var b strings.Builder
	col := 0
	for _, w := range strings.Fields(s) {
		if col > 0 && col+1+len(w) > width {
			b.WriteString("\n         ")
			col = 0
		} else if col > 0 {
			b.WriteString(" ")
			col++
		}
		b.WriteString(w)
		col += len(w)
	}
	return b.String()
}
