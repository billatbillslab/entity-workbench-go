// Package fetch is the consumer half of the CDN release corridor —
// EXTENSION-NETWORK §6.5.3's **Mode A2** consumer: a standalone verifying
// client, not a peer, no dispatch participation, no ingest.
//
// **It enters through the publisher's advertised layout and derives
// nothing.** Given an origin it reads the http-poll transport profile,
// and every URL after that — tree leaves, listings, content, the signed
// manifest — is built from what the publisher wrote there. The version
// before 2026-08-19 derived all three by convention and could not fetch
// one byte from our own publisher: it looked for `/{peer}/tree/{path}.bin`
// (we emit no `tree/` segment), read the leaf as a raw 33-byte hash (we
// emit the Amendment 6 `system/hash` pointer), and sharded content
// `2-flat` over digest-only hex (we emit `sharded-2-4` over wire hex).
// Four divergences, none of them caught, because no gate ever pointed
// this half at the other half's bytes.
//
// What's still deferred and tagged: signature verification of the signed
// root (the emitted directory carries the signature entity but not the
// publisher's identity entity, so a cold consumer cannot close the loop
// from the artifact set alone — measured, and routed); substitute-source
// chain traversal (one origin only); subscriber notification (this is a
// read-once tool); transitive closure (only the entity at the requested
// path, not its references).
package fetch

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// TypeHash is the entity type of an Amendment 6 tree-leaf pointer.
const TypeHash = "system/hash"

// Opts configures a Fetch call.
type Opts struct {
	// BaseURL is the origin the publisher's profile is read from.
	BaseURL string
	// PeerID is optional. When set it is CROSS-CHECKED against the
	// profile's `peer_id` and a mismatch is an error — a consumer that
	// silently preferred the caller's id would fetch one publisher's
	// bytes while reporting another's name.
	PeerID string
	// Path is the tree path, relative to the peer root, no suffix.
	Path   string
	Client *http.Client
	// Layout short-circuits profile discovery. Set it when the layout
	// came from somewhere better than the well-known object — a
	// registry binding, or a profile already in the consumer's tree.
	Layout *Layout
}

// Result bundles the resolved hash and decoded entity.
type Result struct {
	Hash     hash.Hash
	Entity   entity.Entity
	Layout   Layout
	TreeURL  string
	BlobURL  string
	TreeSize int
	BlobSize int
}

// Fetch resolves one tree path over the publisher's advertised layout.
//
//  1. GET {origin}/transport-profile        → the endpoint block (unless Opts.Layout)
//  2. GET {tree-base}/{path}{leaf-suffix}   → ECF(system/hash) pointer, cracked to H
//  3. GET {content-prefix}/{layout}/{hex(H)} → the ECF entity
//  4. decode, validate, and verify the content hash equals H
//
// Step 4 is the trust anchor: the tree binding says which bytes, the
// content hash proves they are those bytes. Nothing above it is trusted
// — not the origin, not the profile, not the path.
func Fetch(ctx context.Context, opts Opts) (Result, error) {
	if opts.BaseURL == "" && opts.Layout == nil {
		return Result{}, fmt.Errorf("fetch: BaseURL or Layout required")
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}

	layout := Layout{}
	if opts.Layout != nil {
		layout = *opts.Layout
	} else {
		l, err := LoadLayout(ctx, opts.BaseURL, client)
		if err != nil {
			return Result{}, err
		}
		layout = l
	}
	if opts.PeerID != "" && opts.PeerID != layout.PeerID {
		return Result{}, fmt.Errorf("fetch: caller asked for peer %s, the profile at this origin "+
			"is %s — the bytes and the name would not be the same publisher's",
			opts.PeerID, layout.PeerID)
	}

	treeURL := layout.TreeLeafURL(opts.Path)
	treeBytes, err := httpGet(ctx, client, treeURL)
	if err != nil {
		return Result{}, fmt.Errorf("fetch tree: %w", err)
	}
	h, err := crackPointer(treeBytes)
	if err != nil {
		return Result{}, fmt.Errorf("fetch: %s: %w", treeURL, err)
	}

	blobURL, err := layout.ContentURL(h)
	if err != nil {
		return Result{}, err
	}
	blobBytes, err := httpGet(ctx, client, blobURL)
	if err != nil {
		return Result{}, fmt.Errorf("fetch content: %w", err)
	}

	ent, err := decodeVerified(blobBytes, h)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Hash:     h,
		Entity:   ent,
		Layout:   layout,
		TreeURL:  treeURL,
		BlobURL:  blobURL,
		TreeSize: len(treeBytes),
		BlobSize: len(blobBytes),
	}, nil
}

// SignedRoot fetches and decodes the publisher's signed published-root
// through `manifest_url_prefix`.
//
// **What this returns is "verified as of published_at", never
// "verified"** (§6.5.3.1, D6/D7). The root states a moment; a quiet
// publisher and a withholding origin are indistinguishable from here,
// and no freshness field closes that. The caller decides what age it
// will accept.
func SignedRoot(ctx context.Context, layout Layout, client *http.Client) (types.PublishedRootData, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := layout.ManifestURL()
	if url == "" {
		return types.PublishedRootData{}, fmt.Errorf("fetch: publisher advertises no manifest_url_prefix, " +
			"so this origin has no signed entry point (§6.5.3 reserves the location; it is not derivable)")
	}
	raw, err := httpGet(ctx, client, url)
	if err != nil {
		return types.PublishedRootData{}, fmt.Errorf("fetch manifest: %w", err)
	}
	var ent entity.Entity
	if err := ecf.Decode(raw, &ent); err != nil {
		return types.PublishedRootData{}, fmt.Errorf("fetch: decode manifest: %w", err)
	}
	if ent.Type != types.TypePeerPublishedRoot {
		return types.PublishedRootData{}, fmt.Errorf("fetch: %s holds type %q, want %s",
			url, ent.Type, types.TypePeerPublishedRoot)
	}
	root, err := types.PublishedRootDataFromEntity(ent)
	if err != nil {
		return types.PublishedRootData{}, fmt.Errorf("fetch: decode published-root: %w", err)
	}
	if root.PeerID != layout.PeerID {
		return types.PublishedRootData{}, fmt.Errorf("fetch: signed root names peer %s, the profile at "+
			"this origin names %s", root.PeerID, layout.PeerID)
	}
	return root, nil
}

// crackPointer reads an Amendment 6 tree-leaf body: the bare 2-key
// `ECF({type:"system/hash", data:H})`, where H is the bound hash in its
// own wire form.
//
// Returning the dereferenced entity here instead is non-conformant, so
// the type check is a real check and not a formality: a one-hop
// publisher's leaf decodes perfectly well as *some* entity, and a
// consumer that accepted it would silently lose the dedup invariant the
// two-hop shape exists to preserve.
func crackPointer(b []byte) (hash.Hash, error) {
	var ent entity.Entity
	if err := ecf.Decode(b, &ent); err != nil {
		return hash.Hash{}, fmt.Errorf("decode tree-leaf pointer: %w", err)
	}
	if ent.Type != TypeHash {
		return hash.Hash{}, fmt.Errorf("tree leaf is type %q, want %q — a leaf MUST be the bound "+
			"hash pointer, not the dereferenced entity (EXTENSION-NETWORK Amendment 6)", ent.Type, TypeHash)
	}
	var h hash.Hash
	if err := ecf.Decode(ent.Data, &h); err != nil {
		return hash.Hash{}, fmt.Errorf("decode bound hash from pointer: %w", err)
	}
	return h, nil
}

// decodeVerified decodes a CONTENT_GET body and proves it is the bytes
// the tree bound.
//
// **The body is the BARE 2-key hashable form** — `{type, data}`, no
// `content_hash` — which is what both publishers in this cohort emit and
// what a content-addressed store holds: the hash is the URL, so carrying
// it inside the body again is a second copy of the same claim. So the
// check here is NOT `entity.Validate()`, which enforces the 3-key wire
// invariant and rejects every conformant content blob (it reads the
// absent field as a zero hash and reports a mismatch against it — the
// first thing this gate caught).
//
// What replaces it is stronger, not weaker: recompute the hash over
// (type, data) and require it to equal the hash the tree bound. A 3-key
// body is accepted too, but its self-claimed hash is checked rather than
// trusted — a body that disagrees with itself is refused even when it
// matches the binding.
func decodeVerified(b []byte, want hash.Hash) (entity.Entity, error) {
	var ent entity.Entity
	if err := ecf.Decode(b, &ent); err != nil {
		return entity.Entity{}, fmt.Errorf("fetch: decode entity: %w", err)
	}
	if ent.Type == "" {
		return entity.Entity{}, fmt.Errorf("fetch: content body carries no type")
	}
	got, err := hash.Compute(ent.Type, ent.Data)
	if err != nil {
		return entity.Entity{}, fmt.Errorf("fetch: recompute content hash: %w", err)
	}
	if got != want {
		return entity.Entity{}, fmt.Errorf("fetch: hash mismatch — the tree bound %s, these bytes are %s",
			want, got)
	}
	var zero hash.Hash
	if ent.ContentHash != zero && ent.ContentHash != want {
		return entity.Entity{}, fmt.Errorf("fetch: content body claims hash %s but is %s",
			ent.ContentHash, want)
	}
	ent.ContentHash = want
	return ent, nil
}

// contentURL builds `{prefix}/{layout-path}/{hex(H)}`.
//
// **The hex is of the FULL wire form, including the format-code byte**
// — 66 chars beginning `00` under ECFv1-SHA-256, never the 64-char
// digest-only hex. EXTENSION-NETWORK §6.5.3.1 makes that a MUST in as
// many words ("Hash hex includes the format-code byte … NOT the 64-char
// digest-only hex"), and it is what preserves URL ⇄ binding parity with
// the EXTENSION-CONTENT §6.4.2 tree key.
//
// We do NOT call core-go's `types.BuildContentURL`, which is otherwise
// exactly this function. It hexes `h.EffectiveDigest()` — the
// digest-only form the MUST excludes — so it builds a URL that finds
// nothing at either of this cohort's publishers, both of which emit
// wire hex (ours: publish/publish.go emitContent; browser-rust:
// content_site/http_poll.rs content_url, whose own test asserts the
// 66-char `00`-prefixed form). Routed to core-go rather than worked
// around silently; when it is fixed this function becomes a one-line
// delegation.
func contentURL(prefix, layout string, h hash.Hash) (string, error) {
	prefix = strings.TrimRight(prefix, "/")
	hx := hex.EncodeToString(h.Bytes())
	switch layout {
	case types.ContentLayoutFlat:
		return prefix + "/" + hx, nil
	case types.ContentLayoutSharded2Flat:
		return prefix + "/" + hx[0:2] + "/" + hx, nil
	case types.ContentLayoutSharded24, types.ContentLayoutSharded22:
		return prefix + "/" + hx[0:2] + "/" + hx[2:4] + "/" + hx, nil
	case "":
		return "", fmt.Errorf("fetch: publisher declares no content_layout")
	default:
		return "", fmt.Errorf("fetch: unknown content_layout %q — refused rather than guessed", layout)
	}
}

func httpGet(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
