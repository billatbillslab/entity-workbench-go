// Package publish is the producer half of the CDN release corridor.
// Given a populated peer, walk a tree prefix and emit a sharded
// directory tree ready for upload to any HTTP static origin and
// consumable as a §6.5.3 http-poll transport profile.
//
// Aligned to EXTENSION-NETWORK v1.4 Amendment 5 — the
// HTTP read surface is now three route families addressable as plain
// static objects (no trailing slash, no redirects):
//
//	/content/{hex33(H)}                           — CONTENT_GET
//	/manifest                                     — MANIFEST_GET (the
//	                                                signed published-root)
//	/transport-profile                            — this publisher's own
//	                                                http-poll profile,
//	                                                out-of-band per §6.5.4
//	/peers{tree_listing_suffix}                   — all-peers root listing
//	/{peer_id}{tree_listing_suffix}               — peer-root listing
//	/{peer_id}/{path}{tree_leaf_suffix}           — entity binding
//	/{peer_id}/{path}{tree_listing_suffix}        — listing at path
//
// Defaults: tree_leaf_suffix=".bin", tree_listing_suffix=".list"
// (REQUIRED distinct). The two suffixes give a total bijection over
// any name — entity `foo`→`foo.bin`, listing `foo`→`foo.list`,
// entity `foo.bin`→`foo.bin.bin`, listing `foo.bin`→`foo.bin.list`.
//
// Pagination via `next_page` on `system/tree/listing` (V7 §3.9 +
// Amendment 5) is type-supported but not yet emitted — v0 publishes
// a single page per prefix. Once a real publish exceeds a sane page
// cap, chain emission lands (the head listing carries `next_page`;
// subsequent pages are content-addressed and namespace-bound per
// §6.4.2 — i.e. they fall out of the existing content/ shard).
//
// Closure walking over *bound* entities remains shallow: content-blob
// chunks are followed; everything else stops at the directly-bound
// entity. Capability chains, signature siblings, application-typed
// references, revision parents — all plug in through Opts.References.
//
// The signed-root closure is separate and is NOT shallow: because this
// publisher advertises `signed_pointer`, §6.5.3's publish-side
// obligation makes the transitive hash-linked closure of
// `published-root.root_hash` — the CHAMP trie root, every interior
// node, every leaf-bound hash — a MUST, and it is emitted in full
// (see signed_root.go). Interior trie nodes are hash-linked rather
// than path-bound, so nothing in the path walk above would reach them.
package publish

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/types"
)

// Amendment 5 default suffixes — re-exported from core/types so existing
// publish-side call sites (and tests) keep their package-local references
// working. The canonical pins live in types.DefaultTreeLeafSuffix /
// types.DefaultTreeListingSuffix.
// TransportProfileFile is the object name this publisher uses for the
// out-of-band transport profile (§6.5.4). Conventional, not normative
// — a consumer learns it out-of-band by definition; what IS normative
// is that it is not `manifest`.
const TransportProfileFile = "transport-profile"

const (
	DefaultTreeLeafSuffix    = types.DefaultTreeLeafSuffix
	DefaultTreeListingSuffix = types.DefaultTreeListingSuffix
)

// Opts configures a Publish run.
//
// The hook fields (IncludePath, IncludeType, References) are the
// extension seams. They're sketch-level in v0 — wired through the
// pipeline, not exposed on the CLI, not used in defaults. Callers
// that need to scope a publish (only one type; only revisions; only
// signed entities; etc.) supply policy here.
type Opts struct {
	Peer      *entitysdk.AppPeer
	Prefix    string
	OutputDir string

	// OriginURL is the external HTTP origin the published directory
	// will be served from (e.g. "https://my-cdn.example.com"). Used
	// to populate the three Amendment-5 endpoint prefixes
	// (tree_url_prefix = origin root in co-located mode;
	// content_url_prefix = {origin}/content;
	// manifest_url_prefix = {origin}/manifest). If empty, the
	// manifest is still emitted but with empty prefix fields and a
	// warning — the operator must edit before upload, or re-publish
	// with -origin set.
	OriginURL string

	// IncludePath, if non-nil, gates which tree paths land in the
	// closure roots. Returns true to include. Called once per
	// LocationIndex entry, after the Prefix filter has run.
	IncludePath func(path string) bool

	// IncludeType, if non-nil, gates which entity types are emitted.
	// Returns true to include. Called once per entity encountered
	// during closure walking. An excluded entity's references are
	// NOT walked further — the type filter is a hard cut on the
	// emitted set, not just on what gets written.
	IncludeType func(typeName string) bool

	// References, if non-nil, returns additional content hashes to
	// walk for the given entity, beyond the built-in content-blob
	// chunk walker. This is where revision-tree, capability-chain,
	// and signature-sibling walkers plug in. Returned hashes that
	// don't resolve in the local store are skipped with a warning.
	References func(ent entity.Entity) []hash.Hash
}

// Result summarises what got emitted.
type Result struct {
	PeerID    string
	Prefix    string
	OutputDir string
	OriginURL string
	Paths     int
	Entities  int
	Listings  int
	Bytes     int64
	// Manifest is the http-poll transport profile. Named `Manifest`
	// from when it was the thing served at {manifest_url_prefix};
	// since 2026-08-18 that slot holds the signed root (§6.5.3.1) and
	// the profile ships beside the site (§6.5.4 / proposal D5).
	Manifest types.HTTPPollProfileData
	// SignedRoot is the signed `system/peer/published-root` this run
	// emitted — what the profile's `signed_pointer` advertisement now
	// actually resolves to.
	SignedRoot SignedRoot
}

// Publish walks the peer's location index at Opts.Prefix and emits to
// Opts.OutputDir.
//
// Output layout (Amendment 5):
//
//	{out}/
//	├── manifest                            — signed-handshake wire entity
//	├── peers.list                          — all-peers root listing
//	├── content/{hex[0:2]}/{hex[2:4]}/{hex} — content shards (sharded-2-4)
//	└── {peer_id}/
//	    ├── {peer_id}.list                  — wait, actually:
//	└── {peer_id}.list                      — peer-root listing (sibling of {peer_id}/)
//	└── {peer_id}/
//	    ├── docs.list                       — listing at /docs
//	    ├── docs/
//	    │   ├── index.bin                   — entity at /docs/index
//	    │   ├── index.list                  — listing at /docs/index (if it has children)
//	    │   └── ...
//	    └── ...
//
// The all-peers root listing names every peer-id segment for which the
// publisher emitted bindings; the peer-root listing names the publisher's
// own top-level path segments; each interior path gets its own listing.
func Publish(ctx context.Context, opts Opts) (Result, error) {
	if opts.Peer == nil {
		return Result{}, fmt.Errorf("publish: Peer required")
	}
	if opts.OutputDir == "" {
		return Result{}, fmt.Errorf("publish: OutputDir required")
	}

	cs := opts.Peer.RawContentStore()
	li := opts.Peer.RawLocationIndex()
	peerID := opts.Peer.PeerID()

	entries := entitysdk.ListEntriesSorted(li, opts.Prefix)
	entries = applyPathFilter(entries, opts.IncludePath)
	fmt.Printf("publishing peer %s prefix %q — %d paths\n",
		shortPeerID(peerID), opts.Prefix, len(entries))

	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("publish: mkdir out: %w", err)
	}

	if opts.IncludePath != nil || opts.IncludeType != nil {
		// Refuse at the emitter rather than sign a root we then decline
		// to serve in full.
		//
		// A signed published-root commits to the trie of EVERY binding
		// under `prefix` (§3.3a: the consumer rebuilds absolute paths as
		// `prefix + relative_key`, over the whole key set the root
		// enumerates). A filter makes the emitted set a strict subset of
		// that, and the two ways out are both wrong:
		//
		//   - Serve the closure anyway and the filter leaks: §6.5.3's
		//     closure obligation would upload the content of every
		//     filtered-OUT entity under content_url_prefix, hash-addressed
		//     and fetchable. An operator who wrote IncludeType to keep a
		//     type off the CDN would have published its bytes.
		//   - Withhold the closure and the walk breaks silently:
		//     entity-browser-rust measured exactly this on the consumer
		//     side — withholding one interior node hid 1 of 24 names with
		//     no error anywhere ("a withheld trie node is silent",
		//     browser-rust 9a9c0f5 / STATUS-2026-08-18-a).
		//
		// So the honest publish of a subset is `-prefix` (which changes
		// what the root commits to) — not a filter over a wider one.
		return Result{}, fmt.Errorf(
			"publish: IncludePath/IncludeType cannot be combined with a signed published-root — "+
				"the root commits to every binding under prefix %q, so a filtered emit either leaks "+
				"the filtered-out content through the §6.5.3 closure or breaks a consumer's walk "+
				"silently; narrow -prefix instead", opts.Prefix)
	}

	// Mint the signed root BEFORE the closure walk, so the trie nodes
	// it commits to are in the content store when the closure is
	// collected. It is minted after `entries` is snapshotted so that
	// this run's own published-root / signature bindings are not inside
	// the root they authenticate — an entity cannot appear in its own
	// preimage.
	signed, err := mintSignedRoot(opts.Peer, opts.Prefix)
	if err != nil {
		return Result{}, err
	}

	closure, err := collectClosure(cs, entries, opts)
	if err != nil {
		return Result{}, err
	}
	addSignedRootClosure(cs, &signed, closure)
	fmt.Printf("closure: %d distinct entities\n", len(closure))

	bytes, err := emitContent(closure, opts.OutputDir)
	if err != nil {
		return Result{}, err
	}

	treeBytes, err := emitTree(opts.OutputDir, entries)
	if err != nil {
		return Result{}, err
	}
	bytes += treeBytes

	listings, listingBytes, err := emitListings(opts.OutputDir, entries)
	if err != nil {
		return Result{}, err
	}
	fmt.Printf("listings: %d emitted\n", listings)
	bytes += listingBytes

	rootBytes, err := emitSignedRoot(opts.OutputDir, signed)
	if err != nil {
		return Result{}, err
	}
	bytes += rootBytes

	profile, err := writeTransportProfile(opts.OutputDir, opts.OriginURL, peerID)
	if err != nil {
		return Result{}, err
	}
	return Result{
		PeerID:     peerID,
		Prefix:     opts.Prefix,
		OutputDir:  opts.OutputDir,
		OriginURL:  opts.OriginURL,
		Paths:      len(entries),
		Entities:   len(closure),
		Listings:   listings,
		Bytes:      bytes,
		Manifest:   profile,
		SignedRoot: signed,
	}, nil
}

func applyPathFilter(entries []store.LocationEntry, include func(string) bool) []store.LocationEntry {
	if include == nil {
		return entries
	}
	kept := entries[:0]
	for _, e := range entries {
		if include(e.Path) {
			kept = append(kept, e)
		}
	}
	return kept
}

// collectClosure walks each entry's hash through the local store,
// applying type filter and reference hooks. Built-in walker follows
// content-blob chunks; Opts.References adds whatever else the caller
// wants (revision parents, cap chains, signature siblings, …).
func collectClosure(cs store.ContentStore, entries []store.LocationEntry, opts Opts) (map[hash.Hash]entity.Entity, error) {
	closure := make(map[hash.Hash]entity.Entity, len(entries))
	for _, e := range entries {
		if err := addClosure(cs, e.Hash, closure, opts); err != nil {
			return nil, fmt.Errorf("publish: walk %s (%s): %w", e.Path, e.Hash, err)
		}
	}
	return closure, nil
}

func addClosure(cs store.ContentStore, h hash.Hash, closure map[hash.Hash]entity.Entity, opts Opts) error {
	if _, seen := closure[h]; seen {
		return nil
	}
	ent, ok := cs.Get(h)
	if !ok {
		fmt.Printf("  warn: missing entity %s\n", h)
		return nil
	}
	if opts.IncludeType != nil && !opts.IncludeType(ent.Type) {
		return nil
	}
	closure[h] = ent

	if ent.Type == "system/content/blob" {
		var blob types.ContentBlobData
		if err := ecf.Decode(ent.Data, &blob); err == nil {
			for _, chunkHash := range blob.Chunks {
				if err := addClosure(cs, chunkHash, closure, opts); err != nil {
					return err
				}
			}
		} else {
			fmt.Printf("  warn: decode blob %s: %v\n", h, err)
		}
	}

	if opts.References != nil {
		for _, refHash := range opts.References(ent) {
			if err := addClosure(cs, refHash, closure, opts); err != nil {
				return err
			}
		}
	}
	return nil
}

// emitContent writes each entity in the closure as the canonical
// hashable pre-image bytes — i.e. exactly what hash.Compute hashed —
// so a Mode-A consumer doing raw SHA-256 (or whatever algorithm the
// hash advertises) over the fetched file bytes recovers the URL hash
// digest byte-for-byte.
//
// Layout is sharded-2-4 (§6.5.3 enum), URL pattern
// /{hash[0:2]}/{hash[2:4]}/{hash} with `{hash}` consistently read
// as the 66-char wire-hex.
func emitContent(closure map[hash.Hash]entity.Entity, outDir string) (int64, error) {
	contentRoot := filepath.Join(outDir, "content")
	var total int64
	for h, ent := range closure {
		raw, err := ecf.EncodeHashable(ent.Type, ent.Data)
		if err != nil {
			return total, fmt.Errorf("publish: encode hashable %s: %w", h, err)
		}
		hexWire := hex.EncodeToString(h.Bytes())
		shardDir := filepath.Join(contentRoot, hexWire[0:2], hexWire[2:4])
		if err := os.MkdirAll(shardDir, 0o755); err != nil {
			return total, fmt.Errorf("publish: mkdir %s: %w", shardDir, err)
		}
		filePath := filepath.Join(shardDir, hexWire)
		if err := os.WriteFile(filePath, raw, 0o644); err != nil {
			return total, fmt.Errorf("publish: write %s: %w", filePath, err)
		}
		total += int64(len(raw))
	}
	return total, nil
}

// emitTree writes each (path → hash) binding at
// {out}/{peer_id}/{bare_path}.bin per Amendment 5 §6.5.3.1 + Amendment 6.
//
// Body shape — Amendment 6: a `system/hash` 2-key bare
// pointer `ECF({type:"system/hash", data: H})` where H is the 33-byte
// wire-form bound hash. NOT the dereferenced entity.
//
// Why the pointer, not the entity: returning the bound entity at every
// .bin URL would materialize a separate copy per path bound to the same
// hash, defeating V7 §1.7's content-store dedup invariant on every
// static CDN that can't recover dedup. The two-hop pattern
//
//	GET /{peer_id}/{path}.bin           → ECF({type:"system/hash", data: H})
//	GET /content/{hex33(H)}             → the entity body
//
// preserves dedup byte-for-byte. This is exactly `tree:get mode:"hash"`
// (V7 §1.7) projected over HTTP. Mirrors core-go ext/httplive
// serveTreeEntity (poll.go:506).
//
// 2-key, not 3-key: a path-addressed pointer has no useful self-hash;
// a 3-key body would carry two hashes — the bound H in data plus the
// pointer's own content_hash — forcing the consumer to disambiguate
// which one to second-hop. Same convention as CONTENT_GET's bare body.
//
// The .bin suffix avoids the leaf-vs-subtree-at-same-name filesystem
// collision (x.bin leaf vs x/y.bin subtree leaf vs x.list listing — all
// distinct files).
func emitTree(outDir string, entries []store.LocationEntry) (int64, error) {
	var total int64
	for _, e := range entries {
		peerID, bare, ok := splitAbsPath(e.Path)
		if !ok {
			fmt.Printf("  warn: skip un-namespaced path %q\n", e.Path)
			continue
		}
		if bare == "" {
			// Path is /{peer_id} with no remainder. A peer-id root is a
			// directory per V7 §1.4; emitting it as a leaf would be the
			// `{peer_id}.bin ⇒ 404` shape Amendment 5 §6.5.6 forbids.
			fmt.Printf("  warn: skip peer-root leaf binding %q\n", e.Path)
			continue
		}
		body, err := encodeHashPointer(e.Hash)
		if err != nil {
			return total, fmt.Errorf("publish: encode pointer for %s: %w", e.Path, err)
		}
		filePath := filepath.Join(outDir, peerID, filepath.FromSlash(bare)) + ".bin"
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return total, fmt.Errorf("publish: mkdir %s: %w", filepath.Dir(filePath), err)
		}
		if err := os.WriteFile(filePath, body, 0o644); err != nil {
			return total, fmt.Errorf("publish: write %s: %w", filePath, err)
		}
		total += int64(len(body))
	}
	return total, nil
}

// encodeHashPointer renders the Amendment 6 system/hash pointer body:
// `ECF({type:"system/hash", data: <CBOR-bstr of 33-byte wire form>})`.
// EncodeHashable produces the 2-key bare form (no content_hash); see
// emitTree for the dedup-preservation rationale.
func encodeHashPointer(h hash.Hash) ([]byte, error) {
	dataCbor, err := ecf.Encode(h.Bytes())
	if err != nil {
		return nil, fmt.Errorf("encode pointer data: %w", err)
	}
	body, err := ecf.EncodeHashable("system/hash", cbor.RawMessage(dataCbor))
	if err != nil {
		return nil, fmt.Errorf("encode pointer: %w", err)
	}
	return body, nil
}

// emitListings walks the entries and writes a system/tree/listing entity
// at every prefix in the tree per Amendment 5 §6.5.3.1:
//
//   - {out}/peers.list                      — universal-tree-root
//     (Amendment 5 §6.5.6: this is the normal universal-tree-root view,
//     NOT a multi-tenant feature; lists every peer-id segment present.)
//   - {out}/{peer_id}.list                  — peer-root listing
//   - {out}/{peer_id}/{stem}.list           — listing at every interior
//     prefix that has at least one child binding
//
// Each listing entity is a canonical system/tree/listing (V7 §3.9):
// `{path, entries: {name → {hash?, has_children}}, count, offset, next_page?}`.
// v0 emits a single page per prefix (no chain pagination); when a
// real publish needs pagination, the chain pages would be content-
// addressed and bound into the served namespace per §2A β.
//
// Scope: every entry in the input is in-scope by construction (the
// caller already applied Prefix + IncludePath + IncludeType), so the
// listing's `count` is the filtered total per TREE §1176.
func emitListings(outDir string, entries []store.LocationEntry) (int, int64, error) {
	// Group every entry under each of its ancestor prefixes. The set of
	// ancestor prefixes per entry path /{peer_id}/a/b/c is:
	//   /                — universal root (peers.list)
	//   /{peer_id}       — peer root
	//   /{peer_id}/a     — listing at a
	//   /{peer_id}/a/b   — listing at a/b
	// Each prefix gets the direct-child it sees at the next segment.
	type childAgg struct {
		hash        *hash.Hash
		hasChildren bool
	}
	// listings[prefix][childName] = aggregated info
	listings := make(map[string]map[string]*childAgg)

	// Track presence-only sets we still need to render:
	//   - the universal root always gets one entry per peer-id seen
	//   - every prefix we visit always gets its own listing object (even
	//     when it's empty in-scope — but for our publisher the prefix only
	//     exists because something lives under it, so empty doesn't occur)
	for _, e := range entries {
		segments := splitSegments(e.Path)
		if len(segments) == 0 {
			continue
		}
		// segments[0] = peer-id; remaining = within-peer path.
		// For each ancestor prefix, record one child.
		// Universal root has child = peer-id; rest follow.
		for i := 0; i <= len(segments)-1; i++ {
			// prefix = "/" + segments[0..i-1] joined
			prefix := joinPrefix(segments[:i])
			childName := segments[i]
			bucket := listings[prefix]
			if bucket == nil {
				bucket = make(map[string]*childAgg)
				listings[prefix] = bucket
			}
			agg, ok := bucket[childName]
			if !ok {
				agg = &childAgg{}
				bucket[childName] = agg
			}
			isLastSegment := i == len(segments)-1
			if isLastSegment {
				h := e.Hash
				agg.hash = &h
			} else {
				agg.hasChildren = true
			}
		}
	}

	// Render each listing. Sort prefix keys for determinism / byte-stable
	// output (matches ECF map canonical-by-key ordering inside the entity).
	prefixes := make([]string, 0, len(listings))
	for p := range listings {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)

	var total int64
	for _, prefix := range prefixes {
		bucket := listings[prefix]
		// Materialize entries in sorted child order.
		names := make([]string, 0, len(bucket))
		for n := range bucket {
			names = append(names, n)
		}
		sort.Strings(names)
		entriesMap := make(map[string]interface{}, len(names))
		for _, name := range names {
			a := bucket[name]
			entry := map[string]interface{}{
				"has_children": a.hasChildren,
			}
			if a.hash != nil {
				entry["hash"] = *a.hash
			}
			entriesMap[name] = entry
		}

		// listing.path is rendered without a leading slash, matching the
		// core-go ext/httplive serveTreeListing convention (poll.go:560 —
		// `strings.TrimPrefix(prefix, "/")`). Universal-root stays "".
		listingPath := strings.TrimPrefix(prefix, "/")
		ld := types.ListingData{
			Path:    listingPath,
			Entries: entriesMap,
			Count:   uint64(len(entriesMap)),
			Offset:  0,
			// NextPage: nil — single-page renderer for v0.
		}
		ent, err := ld.ToEntity()
		if err != nil {
			return 0, total, fmt.Errorf("publish: build listing %q: %w", prefix, err)
		}
		raw, err := ecf.Encode(ent)
		if err != nil {
			return 0, total, fmt.Errorf("publish: encode listing %q: %w", prefix, err)
		}
		filePath := listingFilePath(outDir, prefix)
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return 0, total, fmt.Errorf("publish: mkdir %s: %w", filepath.Dir(filePath), err)
		}
		if err := os.WriteFile(filePath, raw, 0o644); err != nil {
			return 0, total, fmt.Errorf("publish: write %s: %w", filePath, err)
		}
		total += int64(len(raw))
	}
	return len(prefixes), total, nil
}

// listingFilePath maps a prefix path to the on-disk .list object.
//
//	prefix == ""              → {out}/peers.list   (Amendment 5 reserved-word literal)
//	prefix == "/{peer_id}"    → {out}/{peer_id}.list
//	prefix == "/{peer_id}/a"  → {out}/{peer_id}/a.list
func listingFilePath(outDir, prefix string) string {
	if prefix == "" {
		return filepath.Join(outDir, "peers"+DefaultTreeListingSuffix)
	}
	rel := strings.TrimPrefix(prefix, "/")
	return filepath.Join(outDir, filepath.FromSlash(rel)) + DefaultTreeListingSuffix
}

// splitSegments splits an absolute path into its slash-separated
// segments, dropping the leading slash. "/a/b/c" → ["a","b","c"].
func splitSegments(abs string) []string {
	raw := strings.TrimPrefix(abs, "/")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "/")
}

// joinPrefix renders the slash-prefixed absolute path for a segment
// list. [] → ""; ["a","b"] → "/a/b".
func joinPrefix(segments []string) string {
	if len(segments) == 0 {
		return ""
	}
	return "/" + strings.Join(segments, "/")
}

// splitAbsPath splits "/{peer_id}/{rest}" into (peer_id, rest).
// Returns ok=false for paths that don't have at least the peer-id
// segment.
func splitAbsPath(abs string) (peerID, bare string, ok bool) {
	raw := strings.TrimPrefix(abs, "/")
	if raw == "" {
		return "", "", false
	}
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		return raw[:i], raw[i+1:], true
	}
	return raw, "", true
}

// writeTransportProfile builds and writes the http-poll profile entity
// at {out}/transport-profile using core-go's types.HTTPPollProfileData
// (which carries the Amendment-5 endpoint extension since core-go
// 11f512f).
//
// It is NOT written to {out}/manifest. That slot is reserved for the
// signed `system/peer/published-root` (§6.5.3.1 MUST), and §6.5.4 says
// where a static publisher's own profile goes instead: out-of-band —
// "a well-known URL, a deployment descriptor, a pinned config". A
// sibling object at the origin root is the cheapest form of that, and
// it cannot collide with the §6.5.6 demux, whose reserved first-segment
// literals are exactly {content, manifest, peers}.
//
// The three Amendment-5 prefixes are derived from OriginURL in
// co-located mode (PROPOSAL §3 Edit A, Option B):
//
//	tree_url_prefix     = {origin}             (origin root; peer-id parse
//	                                            is the tree signal — no
//	                                            tree/ reserved word)
//	content_url_prefix  = {origin}/content
//	manifest_url_prefix = {origin}/manifest
//
// If OriginURL is empty, all three are empty and the operator must
// edit before upload.
func writeTransportProfile(outDir, originURL, peerID string) (types.HTTPPollProfileData, error) {
	if originURL == "" {
		fmt.Println("  warn: -origin not set; manifest emitted with empty URL prefixes (operator must edit before upload)")
	}
	var contentPrefix, manifestPrefix string
	if originURL != "" {
		contentPrefix = originURL + "/content"
		manifestPrefix = originURL + "/manifest"
	}

	md := types.HTTPPollProfileData{
		PeerID:        peerID,
		TransportType: "http-poll",
		Endpoint: types.TransportEndpoint{
			TreeURLPrefix:     originURL,
			ContentURLPrefix:  contentPrefix,
			ManifestURLPrefix: manifestPrefix,
			ContentLayout:     types.ContentLayoutSharded24,
			TreeLeafSuffix:    types.DefaultTreeLeafSuffix,
			TreeListingSuffix: types.DefaultTreeListingSuffix,
		},
		SupportedOps:   []string{types.OpTreeGet, types.OpContentGet, types.OpManifestGet},
		Freshness:      "static-immutable+signed-pointer",
		NonceRequired:  false,
		CapFlow:        "egress",
		PollIntervalMs: 60000,
		SignedPointer:  "system/peer/published-root",
		AdvertisedAt:   uint64(time.Now().UnixMilli()),
	}

	// Validate before emit — §6.5.3 / §6.5.3.1 distinct-suffix invariant
	// belongs at the construction site, not silently into the wire.
	if err := md.Endpoint.Validate(); err != nil {
		return types.HTTPPollProfileData{}, fmt.Errorf("publish: endpoint invalid: %w", err)
	}

	raw, err := cbor.Marshal(md)
	if err != nil {
		return types.HTTPPollProfileData{}, fmt.Errorf("publish: encode manifest data: %w", err)
	}
	ent, err := entity.NewEntity(types.TypePeerTransportHTTPPoll, cbor.RawMessage(raw))
	if err != nil {
		return types.HTTPPollProfileData{}, fmt.Errorf("publish: build manifest entity: %w", err)
	}
	wire, err := ecf.Encode(ent)
	if err != nil {
		return types.HTTPPollProfileData{}, fmt.Errorf("publish: encode manifest entity: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, TransportProfileFile), wire, 0o644); err != nil {
		return types.HTTPPollProfileData{}, err
	}
	return md, nil
}

func shortPeerID(p string) string {
	if len(p) <= 12 {
		return p
	}
	return p[:12] + "…"
}
