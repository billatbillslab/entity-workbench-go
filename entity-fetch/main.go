// entity-fetch — the consumer half of the CDN release corridor, and
// EXTENSION-NETWORK §6.5.3's Mode A2 client: a verifying tool, not a
// peer.
//
// Two modes, and the difference between them is the whole point:
//
//   - **`-path`** resolves ONE tree path over the publisher's advertised
//     layout, two-hop and hash-verified. That proves the body is the
//     pre-image of the hash the leaf bound. It does **not** prove the
//     origin is not lying, because the origin supplied the pointer too.
//
//   - **`-verify`** runs the whole chain: manifest → **signature** →
//     **CHAMP trie walk from the signed root** → leaf. Only the walk can
//     tell a complete origin from one serving a correctly-signed root
//     whose closure it withholds — those are byte-identical at every
//     other step.
//
// `-verify` is also the shape a foreign publisher is consumed in: with
// `-pin-*` it takes a hand-supplied layout, for origins that serve no
// `transport-profile` object (§6.5.4 makes profile distribution
// out-of-band in v1, so that is conformant — `entity-core-go`'s
// federation origin is one).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/fetch"
)

const usage = `Usage:
  entity-fetch -base URL -path PATH [-peer-id ID] [-decode]
  entity-fetch -base URL -verify [-bodies] [-reconcile] [-absent KEY] [-json]
  entity-fetch -registry URL -registry-peer ID -names
  entity-fetch -registry URL -registry-peer ID -name NAME [-path PATH]

Resolve one path (-path), verify the whole published root (-verify), or enter
through a NAME (-registry + -name).

Flags:
  -base URL       Origin of the published bundle (e.g. http://localhost:8000).
                  Its transport-profile is read from {base}/transport-profile
                  unless the layout is pinned (-pin-*).
  -path PATH      Tree path to fetch (e.g. docs/foo.md)
  -peer-id ID     Cross-check against the profile's peer_id; REQUIRED when the
                  layout is pinned, because it is the key the signature
                  verifies against
  -decode         Best-effort decode the entity's data as JSON-ish for display

  -verify         Full chain: manifest -> signature -> trie walk -> enumerate
  -bodies         Also fetch and hash-verify every committed leaf
  -reconcile      Also check each committed key against the publisher's own
                  advertised tree-leaf URL (the two must agree)
  -absent KEY     Absent-key control; runs only after a non-empty enumeration
  -json           Machine-readable report

Registry mode (EXTENSION-REGISTRY §6a — a name, resolved against a pinned
name authority, then followed to the publisher it names):
  -registry URL         Origin serving the registry's tree
  -registry-peer ID     The registry's peer-id. THE PIN, and the only thing
                        supplied out of band. For an identity-form peer-id the
                        pin IS the key, so nothing is fetched to learn who the
                        registry is — which is why the host serving its bytes
                        is trusted for nothing.
  -names                Enumerate: WALK the registry's signed root and print
                        every name it commits to (§6a.3a). A served listing can
                        hide a name undetectably; a walk cannot hide one without
                        failing, and both are printed when they disagree.
  -name NAME            Resolve one name and report every §6a.4 check.
  -path PATH            With -name: follow through to the publisher the name
                        resolves to and fetch this path from ITS signed root.
  -target-origin URL    Where the resolved binding's origin-relative URLs are
                        rooted. Defaults to the registry's own origin, which is
                        right for a single-host deployment.

  The registry's own layout is pinned with the same -pin-* flags below; a
  registry that serves no transport-profile is CONFORMANT (NETWORK §6.5.4 makes
  profile distribution out-of-band in v1).

Pinned layout (for origins that serve no transport-profile):
  -pin-tree PREFIX      tree_url_prefix
  -pin-content PREFIX   content_url_prefix
  -pin-manifest PREFIX  manifest_url_prefix
  -pin-layout NAME      content_layout (flat | sharded-2-flat | sharded-2-4 | sharded-2-2)
  -pin-leaf SUFFIX      tree_leaf_suffix (default .bin)
  -pin-listing SUFFIX   tree_listing_suffix (default .list)
`

func main() {
	base := flag.String("base", "", "base URL")
	peerID := flag.String("peer-id", "", "publisher peer-id")
	path := flag.String("path", "", "tree path")
	decode := flag.Bool("decode", false, "best-effort decode for display")

	verify := flag.Bool("verify", false, "verify the signed root end to end")
	bodies := flag.Bool("bodies", false, "fetch and verify every committed leaf")
	reconcile := flag.Bool("reconcile", false, "reconcile the trie against the advertised leaf URLs")
	absent := flag.String("absent", "", "absent-key control probe")
	asJSON := flag.Bool("json", false, "machine-readable report")

	pinTree := flag.String("pin-tree", "", "pinned tree_url_prefix")
	pinContent := flag.String("pin-content", "", "pinned content_url_prefix")
	pinManifest := flag.String("pin-manifest", "", "pinned manifest_url_prefix")
	pinLayout := flag.String("pin-layout", "", "pinned content_layout")
	pinLeaf := flag.String("pin-leaf", ".bin", "pinned tree_leaf_suffix")
	pinListing := flag.String("pin-listing", ".list", "pinned tree_listing_suffix")

	registryURL := flag.String("registry", "", "registry origin")
	registryPeer := flag.String("registry-peer", "", "registry peer-id (the pin)")
	names := flag.Bool("names", false, "enumerate the registry by walking its signed root")
	name := flag.String("name", "", "resolve one name")
	targetOrigin := flag.String("target-origin", "", "where a resolved binding's relative URLs are rooted")

	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	pinned := *pinTree != "" || *pinContent != "" || *pinManifest != "" || *pinLayout != ""
	endpoint := types.TransportEndpoint{
		TreeURLPrefix:     *pinTree,
		ContentURLPrefix:  *pinContent,
		ManifestURLPrefix: *pinManifest,
		ContentLayout:     *pinLayout,
		TreeLeafSuffix:    *pinLeaf,
		TreeListingSuffix: *pinListing,
	}

	if *registryURL != "" {
		if *registryPeer == "" {
			fmt.Fprintln(os.Stderr, "entity-fetch: -registry needs -registry-peer — the pin is the "+
				"key every signature is checked against, so there is no useful registry without it")
			os.Exit(2)
		}
		if !*names && *name == "" {
			fmt.Fprintln(os.Stderr, "entity-fetch: -registry needs -names or -name")
			os.Exit(2)
		}
		if err := runRegistry(registryArgs{
			origin: *registryURL, peerID: *registryPeer, pinned: pinned, endpoint: endpoint,
			enumerate: *names, name: *name, path: *path, targetOrigin: *targetOrigin,
			json: *asJSON,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "entity-fetch: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *base == "" || (*path == "" && !*verify) {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if *verify {
		if err := runVerify(verifyArgs{
			base: *base, peerID: *peerID, pinned: pinned,
			endpoint: endpoint,
			opts:     fetch.ConsumeOpts{Bodies: *bodies, Reconcile: *reconcile, AbsentProbe: *absent},
			json:     *asJSON,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "entity-fetch: %v\n", err)
			os.Exit(1)
		}
		return
	}

	res, err := fetch.Fetch(context.Background(), fetch.Opts{
		BaseURL: *base,
		PeerID:  *peerID,
		Path:    *path,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "entity-fetch: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("peer:     %s (from the publisher's profile)\n", res.Layout.PeerID)
	fmt.Printf("path:     %s\n", *path)
	fmt.Printf("tree-url: %s (%d bytes)\n", res.TreeURL, res.TreeSize)
	fmt.Printf("hash:     %s\n", res.Hash)
	fmt.Printf("blob-url: %s (%d bytes)\n", res.BlobURL, res.BlobSize)
	fmt.Printf("type:     %s\n", res.Entity.Type)
	fmt.Printf("verified: content hash matches the tree binding\n")

	if root, err := fetch.SignedRoot(context.Background(), res.Layout, nil); err == nil {
		fmt.Printf("root:     seq=%d published_at=%d %s\n", root.Seq, root.PublishedAt, root.RootHash)
		fmt.Printf("          (as published — this mode does not verify the signature or walk the\n")
		fmt.Printf("           root; -verify does both)\n")
	} else {
		fmt.Printf("root:     none reachable (%v)\n", err)
	}

	if *decode {
		var v any
		if err := ecf.Decode(res.Entity.Data, &v); err != nil {
			fmt.Printf("decode:   <failed: %v>\n", err)
			return
		}
		b, err := json.MarshalIndent(stringifyKeys(v), "", "  ")
		if err != nil {
			fmt.Printf("decode:   <marshal failed: %v>\n", err)
			return
		}
		fmt.Printf("decode:\n%s\n", b)
	}
}

type verifyArgs struct {
	base     string
	peerID   string
	pinned   bool
	endpoint types.TransportEndpoint
	opts     fetch.ConsumeOpts
	json     bool
}

func runVerify(a verifyArgs) error {
	ctx := context.Background()

	var layout fetch.Layout
	var err error
	if a.pinned {
		if a.peerID == "" {
			return errors.New("-peer-id is required with a pinned layout: it is the key the " +
				"signature verifies against, and nothing else supplies it")
		}
		layout, err = fetch.PinnedLayout(a.base, a.peerID, a.endpoint)
	} else {
		layout, err = fetch.LoadLayout(ctx, a.base, nil)
		if err == nil && a.peerID != "" && a.peerID != layout.PeerID {
			return fmt.Errorf("the profile at this origin is peer %s, you asked for %s — the bytes "+
				"and the name would not be the same publisher's", layout.PeerID, a.peerID)
		}
	}
	if err != nil {
		return err
	}

	rep, err := fetch.NewConsumer(layout, nil).Consume(ctx, a.opts)
	if err != nil {
		// A walk failure is a verdict about the ORIGIN, not a tool
		// error, so it is reported in the report's own shape when JSON
		// was asked for.
		if a.json {
			emitJSON(verdict{
				Origin: a.base, PeerID: layout.PeerID,
				Verified: false, Error: err.Error(),
				Incomplete: errors.Is(err, fetch.ErrIncompleteWalk),
			})
		}
		return err
	}

	if a.json {
		emitJSON(reportVerdict(a.base, rep))
		if len(rep.Failures()) > 0 {
			return fmt.Errorf("%d of %d committed keys failed verification",
				len(rep.Failures()), len(rep.Keys))
		}
		return nil
	}

	printReport(a.base, rep, a.opts)
	if fails := rep.Failures(); len(fails) > 0 {
		return fmt.Errorf("%d of %d committed keys failed verification", len(fails), len(rep.Keys))
	}
	return nil
}

func printReport(origin string, rep fetch.Report, opts fetch.ConsumeOpts) {
	fmt.Printf("origin:     %s\n", origin)
	fmt.Printf("peer:       %s\n", rep.Root.Data.PeerID)
	fmt.Printf("manifest:   %s\n", rep.Root.ManifestURL)
	fmt.Printf("signature:  %s\n", rep.Root.SignatureURL)
	fmt.Printf("            VERIFIED (%s, against the key the peer-id carries)\n",
		rep.Root.Signature.Algorithm)
	fmt.Printf("root:       %s  seq=%d  published_at=%d  prefix=%q\n",
		rep.Root.Data.RootHash, rep.Root.Data.Seq, rep.Root.Data.PublishedAt, rep.Root.Data.Prefix)
	fmt.Printf("walk:       %d CHAMP nodes resolved, %d keys committed\n",
		rep.Walk.Nodes(), len(rep.Walk.Bindings))

	ok, bad := 0, 0
	for _, k := range rep.Keys {
		if k.Err != nil {
			bad++
			continue
		}
		ok++
	}
	if opts.Bodies || opts.Reconcile {
		fmt.Printf("keys:       %d verified, %d failed\n", ok, bad)
	}
	for _, k := range rep.Keys {
		switch {
		case k.Err != nil:
			fmt.Printf("  ✗ %-48s %v\n", k.Key, k.Err)
		case k.Reconciled:
			fmt.Printf("  ✓ %-48s %s (trie == advertised leaf)\n", k.Key, short(k.TrieHash.String()))
		case opts.Bodies:
			fmt.Printf("  ✓ %-48s %s %s (%d bytes)\n", k.Key, short(k.TrieHash.String()), k.Type, k.Bytes)
		default:
			fmt.Printf("    %-48s %s\n", k.Key, short(k.TrieHash.String()))
		}
	}
	if rep.AbsentProbe != "" {
		if rep.AbsentCorrect {
			fmt.Printf("absent:     %q reported ABSENT (distinct from unreachable)\n", rep.AbsentProbe)
		} else {
			fmt.Printf("absent:     control did NOT fire: %v\n", rep.AbsentErr)
		}
	}
	fmt.Printf("\nverified as of published_at=%d — never simply \"verified\" (§6.5.3.1): a quiet\n",
		rep.Root.Data.PublishedAt)
	fmt.Printf("publisher and a withholding origin are indistinguishable from here.\n")
}

type verdict struct {
	Origin      string        `json:"origin"`
	PeerID      string        `json:"peer_id"`
	Verified    bool          `json:"verified"`
	Error       string        `json:"error,omitempty"`
	Incomplete  bool          `json:"incomplete_walk,omitempty"`
	RootHash    string        `json:"root_hash,omitempty"`
	Seq         uint64        `json:"seq,omitempty"`
	PublishedAt uint64        `json:"published_at,omitempty"`
	Prefix      string        `json:"prefix,omitempty"`
	Nodes       int           `json:"champ_nodes,omitempty"`
	Keys        []keyVerdict  `json:"keys,omitempty"`
	Absent      *absentResult `json:"absent_control,omitempty"`
}

type keyVerdict struct {
	Key        string `json:"key"`
	Hash       string `json:"hash"`
	Type       string `json:"type,omitempty"`
	Bytes      int    `json:"bytes,omitempty"`
	Reconciled bool   `json:"reconciled,omitempty"`
	Error      string `json:"error,omitempty"`
}

type absentResult struct {
	Probe   string `json:"probe"`
	Correct bool   `json:"correct"`
	Error   string `json:"error,omitempty"`
}

func reportVerdict(origin string, rep fetch.Report) verdict {
	v := verdict{
		Origin:      origin,
		PeerID:      rep.Root.Data.PeerID,
		Verified:    true,
		RootHash:    rep.Root.Data.RootHash.String(),
		Seq:         rep.Root.Data.Seq,
		PublishedAt: rep.Root.Data.PublishedAt,
		Prefix:      rep.Root.Data.Prefix,
		Nodes:       rep.Walk.Nodes(),
	}
	for _, k := range rep.Keys {
		kv := keyVerdict{Key: k.Key, Hash: k.TrieHash.String(), Type: k.Type,
			Bytes: k.Bytes, Reconciled: k.Reconciled}
		if k.Err != nil {
			kv.Error = k.Err.Error()
		}
		v.Keys = append(v.Keys, kv)
	}
	if rep.AbsentProbe != "" {
		ar := &absentResult{Probe: rep.AbsentProbe, Correct: rep.AbsentCorrect}
		if rep.AbsentErr != nil {
			ar.Error = rep.AbsentErr.Error()
		}
		v.Absent = ar
	}
	return v
}

func emitJSON(v verdict) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "entity-fetch: marshal report: %v\n", err)
		return
	}
	fmt.Println(string(b))
}

func short(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

// stringifyKeys converts CBOR-decoded map[interface{}]interface{}
// values into map[string]any so encoding/json can marshal them. We
// don't need a faithful CBOR-to-JSON because this is display-only —
// callers that need byte fidelity use the entity bytes directly.
func stringifyKeys(v any) any {
	switch t := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprintf("%v", k)] = stringifyKeys(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stringifyKeys(val)
		}
		return out
	default:
		return v
	}
}
