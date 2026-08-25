// entity-fetch — the consumer half of the CDN release corridor, and
// EXTENSION-NETWORK §6.5.3's Mode A2 client: a verifying tool, not a
// peer.
//
// Given the URL of a published bundle it reads the publisher's http-poll
// transport profile and resolves a tree path over the layout advertised
// there — the two-hop content-addressed indirection, hash-verified. The
// peer-id and all three URL prefixes come from the profile; -peer-id is
// an optional cross-check, not an input the walk needs.
//
// One path per invocation, no transitive closure, no substitute-source
// chain. The signed root is fetched and reported when the publisher
// advertises one; its SIGNATURE is not verified — the emitted directory
// carries the signature entity but not the publisher's identity entity,
// so a cold consumer cannot close that loop from the artifact set alone.
// Reported as "signed root, unverified signature" rather than left to
// look like more than it is.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"go.entitychurch.org/entity-core-go/core/ecf"

	"entity-workbench-go/fetch"
)

const usage = `Usage:
  entity-fetch -base URL -path PATH [-peer-id ID] [-decode]

Flags:
  -base URL       Origin of the published bundle (e.g. http://localhost:8000).
                  Its transport-profile is read from {base}/transport-profile.
  -path PATH      Tree path to fetch (e.g. docs/foo.md)
  -peer-id ID     Optional cross-check against the profile's peer_id; a
                  mismatch is an error, never a substitution
  -decode         Best-effort decode the entity's data as JSON-ish for display
`

func main() {
	base := flag.String("base", "", "base URL")
	peerID := flag.String("peer-id", "", "publisher peer-id")
	path := flag.String("path", "", "tree path")
	decode := flag.Bool("decode", false, "best-effort decode for display")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if *base == "" || *path == "" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
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
		fmt.Printf("          (signed root as published; signature NOT verified — see package doc)\n")
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
