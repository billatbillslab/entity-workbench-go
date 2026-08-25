package publish_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/publish"
)

// TestPublish_FirstForm covers the Amendment 5 happy path:
// manifest at {out}/manifest, content at content/{2}/{2}/{wire},
// tree bindings at {peer_id}/{path}.bin, AND the new listing objects:
// {peer_id}/{path}.list at every interior prefix, {peer_id}.list peer
// root, peers.list universal-tree-root.
func TestPublish_FirstForm(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	seeds := map[string]map[string]string{
		"docs/index":           {"title": "Welcome", "body": "first form"},
		"docs/intro":           {"title": "Intro", "body": "second"},
		"docs/chapter-1/start": {"title": "Chapter 1", "body": "third"},
	}
	hashes := make(map[string]hash.Hash, len(seeds))
	for p, data := range seeds {
		h, err := ap.Store().Put(p, "test/note", data)
		if err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
		hashes[p] = h
	}

	out := t.TempDir()
	const origin = "https://test-origin.example"
	res, err := publish.Publish(context.Background(), publish.Opts{
		Peer:      ap,
		Prefix:    "docs/",
		OutputDir: out,
		OriginURL: origin,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Paths != len(seeds) {
		t.Errorf("Paths = %d, want %d", res.Paths, len(seeds))
	}

	// Content round-trip for docs/index (sharded-2-4, full 33-byte wire hex).
	want := hashes["docs/index"]
	wireHex := hex.EncodeToString(want.Bytes())
	if len(wireHex) != 66 {
		t.Fatalf("expected 66-char wire hex, got %d (%q)", len(wireHex), wireHex)
	}
	raw, err := os.ReadFile(filepath.Join(out, "content", wireHex[0:2], wireHex[2:4], wireHex))
	if err != nil {
		t.Fatalf("read emitted content for docs/index: %v", err)
	}

	// Mode-A pre-image invariant: SHA-256(file_bytes) == h.EffectiveDigest().
	// (Under v7.69 multi-hash the Hash struct's Digest is [MaxDigestSize]byte,
	// so we compare slices via EffectiveDigest, not the raw arrays.)
	rehash := sha256.Sum256(raw)
	if !bytes.Equal(rehash[:], want.EffectiveDigest()) {
		t.Errorf("body re-hash mismatch for docs/index:\n  SHA-256(file)  = %x\n  hash.Digest    = %x\n(emit must be ecf.EncodeHashable, not ecf.Encode)",
			rehash[:], want.EffectiveDigest())
	}

	// Tree binding at {peer_id}/docs/index.bin — Amendment 6: the body
	// is the 2-key bare system/hash pointer ECF({type:"system/hash",
	// data: H}), NOT the dereferenced entity. (Returning the entity at
	// every path-keyed URL would multiply copies bound to the same hash,
	// defeating V7 §1.7 dedup on static CDNs.) Asserts:
	//   - body decodes as a 2-key ECF entity (no content_hash)
	//   - entity.type == "system/hash"
	//   - entity.data is the CBOR-bstr of the 33-byte bound hash
	//   - the bound hash matches the LocationIndex entry
	// The second-hop CONTENT_GET is implicit: the bound hash IS the
	// content-shard key already asserted above.
	treePath := filepath.Join(out, ap.PeerID(), "docs", "index.bin")
	treeBytes, err := os.ReadFile(treePath)
	if err != nil {
		t.Fatalf("read tree binding for docs/index: %v", err)
	}
	pointer := decodeHashPointer(t, treeBytes)
	if pointer != want {
		t.Errorf("emitted hash pointer = %s, want %s", pointer, want)
	}

	// --- Transport profile (Amendment 5 three-prefix endpoint + listing
	// suffix). It lives at {out}/transport-profile, NOT at
	// {out}/manifest: §6.5.3.1 reserves the manifest slot for the signed
	// published-root and §6.5.4 sends a static publisher's own profile
	// out-of-band. Serving the profile there was the 2026-08-18 defect. ---
	profileRaw, err := os.ReadFile(filepath.Join(out, publish.TransportProfileFile))
	if err != nil {
		t.Fatalf("read transport profile: %v", err)
	}
	var profileEnt entity.Entity
	if err := ecf.Decode(profileRaw, &profileEnt); err != nil {
		t.Fatalf("decode transport profile entity: %v", err)
	}
	if profileEnt.Type != types.TypePeerTransportHTTPPoll {
		t.Errorf("profile.type = %q, want %q", profileEnt.Type, types.TypePeerTransportHTTPPoll)
	}
	var md types.HTTPPollProfileData
	if err := cbor.Unmarshal(profileEnt.Data, &md); err != nil {
		t.Fatalf("decode transport profile data: %v", err)
	}
	if md.TransportType != "http-poll" {
		t.Errorf("profile.transport_type = %q, want http-poll", md.TransportType)
	}
	if md.PeerID != ap.PeerID() {
		t.Errorf("profile.peer_id = %q, want %q", md.PeerID, ap.PeerID())
	}
	if md.Endpoint.TreeURLPrefix != origin {
		t.Errorf("profile.endpoint.tree_url_prefix = %q, want %q", md.Endpoint.TreeURLPrefix, origin)
	}
	if md.Endpoint.ContentURLPrefix != origin+"/content" {
		t.Errorf("profile.endpoint.content_url_prefix = %q, want %q", md.Endpoint.ContentURLPrefix, origin+"/content")
	}
	if md.Endpoint.ManifestURLPrefix != origin+"/manifest" {
		t.Errorf("profile.endpoint.manifest_url_prefix = %q, want %q (Amendment 5 §6.5.3)", md.Endpoint.ManifestURLPrefix, origin+"/manifest")
	}
	if md.Endpoint.ContentLayout != types.ContentLayoutSharded24 {
		t.Errorf("profile.endpoint.content_layout = %q, want %q", md.Endpoint.ContentLayout, types.ContentLayoutSharded24)
	}
	if md.Endpoint.TreeLeafSuffix != publish.DefaultTreeLeafSuffix {
		t.Errorf("profile.endpoint.tree_leaf_suffix = %q, want %q", md.Endpoint.TreeLeafSuffix, publish.DefaultTreeLeafSuffix)
	}
	if md.Endpoint.TreeListingSuffix != publish.DefaultTreeListingSuffix {
		t.Errorf("profile.endpoint.tree_listing_suffix = %q, want %q (Amendment 5 §6.5.3)", md.Endpoint.TreeListingSuffix, publish.DefaultTreeListingSuffix)
	}
	if md.Endpoint.TreeLeafSuffix == md.Endpoint.TreeListingSuffix {
		t.Errorf("profile.endpoint: leaf and listing suffixes MUST differ (Amendment 5)")
	}
	if len(md.SupportedOps) != 3 ||
		md.SupportedOps[0] != types.OpTreeGet ||
		md.SupportedOps[1] != types.OpContentGet ||
		md.SupportedOps[2] != types.OpManifestGet {
		t.Errorf("profile.supported_ops = %v, want [%s %s %s]",
			md.SupportedOps, types.OpTreeGet, types.OpContentGet, types.OpManifestGet)
	}
	if md.Freshness != "static-immutable+signed-pointer" {
		t.Errorf("profile.freshness = %q, want static-immutable+signed-pointer", md.Freshness)
	}

	// --- Listings (Amendment 5 §6.5.3.1) ---

	// All-peers root listing: peers.list at the origin root, naming
	// this publisher's peer-id as the only peer-segment seen.
	allPeers := decodeListing(t, filepath.Join(out, "peers.list"))
	if allPeers.Path != "" {
		t.Errorf("peers.list path = %q, want \"\"", allPeers.Path)
	}
	if _, ok := allPeers.Entries[ap.PeerID()]; !ok {
		t.Errorf("peers.list: missing peer-id %q in entries (got %v)", ap.PeerID(), keysOf(allPeers.Entries))
	}
	if allPeers.Count != uint64(len(allPeers.Entries)) {
		t.Errorf("peers.list: count=%d but entries=%d", allPeers.Count, len(allPeers.Entries))
	}

	// Peer-root listing: {peer_id}.list, naming "docs" as a child with
	// has_children=true. listing.path carries the peer-id WITHOUT a
	// leading slash, matching core-go ext/httplive serveTreeListing
	// (poll.go:560 — `strings.TrimPrefix(prefix, "/")`).
	peerRoot := decodeListing(t, filepath.Join(out, ap.PeerID()+".list"))
	if peerRoot.Path != ap.PeerID() {
		t.Errorf("peer-root listing.path = %q, want %q (no leading slash; matches core-go ext/httplive convention)", peerRoot.Path, ap.PeerID())
	}
	docsEntry, ok := peerRoot.Entries["docs"]
	if !ok {
		t.Fatalf("peer-root listing missing 'docs' (got %v)", keysOf(peerRoot.Entries))
	}
	if hasChildren := asBool(docsEntry, "has_children"); !hasChildren {
		t.Errorf("peer-root listing: docs.has_children = false, want true")
	}

	// Listing at /{peer_id}/docs: should name index, intro (leaves), chapter-1 (parent).
	docsListing := decodeListing(t, filepath.Join(out, ap.PeerID(), "docs.list"))
	for _, name := range []string{"index", "intro", "chapter-1"} {
		if _, ok := docsListing.Entries[name]; !ok {
			t.Errorf("docs listing missing %q (got %v)", name, keysOf(docsListing.Entries))
		}
	}
	if !hasHash(docsListing.Entries["index"]) {
		t.Errorf("docs listing: index entry missing hash (leaf)")
	}
	if asBool(docsListing.Entries["chapter-1"], "has_children") != true {
		t.Errorf("docs listing: chapter-1.has_children = false, want true")
	}
	if asBool(docsListing.Entries["index"], "has_children") != false {
		t.Errorf("docs listing: index.has_children = true, want false (no children)")
	}

	// Listing at /{peer_id}/docs/chapter-1: should name 'start' as a leaf.
	chListing := decodeListing(t, filepath.Join(out, ap.PeerID(), "docs", "chapter-1.list"))
	if !hasHash(chListing.Entries["start"]) {
		t.Errorf("chapter-1 listing: start entry missing hash (leaf)")
	}

	// next_page is nil on every page (single-page renderer in v0).
	if allPeers.NextPage != nil || peerRoot.NextPage != nil || docsListing.NextPage != nil || chListing.NextPage != nil {
		t.Errorf("expected NextPage=nil on all single-page listings")
	}

	// Result count: peers + peer-root + docs + docs/chapter-1 = 4.
	if res.Listings != 4 {
		t.Errorf("res.Listings = %d, want 4", res.Listings)
	}
}

// TestPublish_FilterHooks pins the refusal: IncludePath / IncludeType
// are incompatible with a signed published-root and Publish says so
// rather than emitting something that is wrong in one of two ways.
//
// Tier: contract pin (TESTING-STRATEGY). The two failure modes it
// stands in for are the reason, and neither is hypothetical:
//
//   - Leak. The §6.5.3 closure obligation uploads every leaf-bound hash
//     the root commits to. Filtered-out entities are still under the
//     prefix, so their bytes would land under content_url_prefix,
//     hash-addressed and fetchable, while the operator believed they
//     were excluded.
//   - Silent short walk. Withholding them instead breaks the consumer's
//     enumeration with no error — entity-browser-rust measured one
//     withheld interior node hiding 1 of 24 names (9a9c0f5).
func TestPublish_FilterHooks(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	if _, err := ap.Store().Put("docs/keep", "type/keep", map[string]string{"k": "1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := ap.Store().Put("docs/drop-type", "type/drop", map[string]string{"k": "3"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, tc := range []struct {
		name string
		opts publish.Opts
	}{
		{"IncludePath", publish.Opts{IncludePath: func(string) bool { return true }}},
		{"IncludeType", publish.Opts{IncludeType: func(string) bool { return true }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			o := tc.opts
			o.Peer, o.Prefix, o.OutputDir = ap, "docs/", out
			if _, err := publish.Publish(context.Background(), o); err == nil {
				t.Fatalf("Publish with %s returned nil error; want a refusal", tc.name)
			} else if !strings.Contains(err.Error(), "signed published-root") {
				t.Errorf("refusal does not name the reason: %v", err)
			}
			// Nothing may be written before the refusal — a half-emitted
			// origin directory is worse than none.
			if entries, err := os.ReadDir(out); err != nil {
				t.Fatalf("ReadDir: %v", err)
			} else if len(entries) != 0 {
				t.Errorf("refused publish left %d entries in the output dir", len(entries))
			}
		})
	}
}

// decodeHashPointer reads a .bin file as the Amendment 6 system/hash
// pointer body — a 2-key bare ECF entity `{type: "system/hash", data: H}`
// — and returns the bound hash. Validates the 2-key shape, the type, and
// the 33-byte data length. Fatal on any divergence (Amendment 6 is
// normative; one-hop wire entities here are non-conformant per V7 §1.7
// dedup).
func decodeHashPointer(t *testing.T, body []byte) hash.Hash {
	t.Helper()
	// 2-key bare ECF — produced by ecf.EncodeHashable — has no
	// content_hash field. Decode directly as a CBOR map of string→raw.
	var fields map[string]cbor.RawMessage
	if err := cbor.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode .bin as CBOR map: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf(".bin body must be 2-key bare pointer (Amendment 6); got %d keys: %v", len(fields), keysOfRaw(fields))
	}
	var typeName string
	if err := cbor.Unmarshal(fields["type"], &typeName); err != nil {
		t.Fatalf("decode .bin type field: %v", err)
	}
	if typeName != "system/hash" {
		t.Fatalf(".bin pointer type = %q, want %q (Amendment 6)", typeName, "system/hash")
	}
	var data []byte
	if err := cbor.Unmarshal(fields["data"], &data); err != nil {
		t.Fatalf("decode .bin data field: %v", err)
	}
	if len(data) != 33 {
		t.Fatalf(".bin pointer data = %d bytes, want 33 (algorithm byte + 32-byte digest)", len(data))
	}
	h, err := hash.FromBytes(data)
	if err != nil {
		t.Fatalf("parse bound hash from pointer data: %v", err)
	}
	return h
}

func keysOfRaw(m map[string]cbor.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// decodeListing reads a .list file and decodes the system/tree/listing
// entity it carries. Fatal on any failure — listings are normative.
func decodeListing(t *testing.T, path string) types.ListingData {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read listing %s: %v", path, err)
	}
	var ent entity.Entity
	if err := ecf.Decode(raw, &ent); err != nil {
		t.Fatalf("decode listing entity %s: %v", path, err)
	}
	if ent.Type != types.TypeTreeListing {
		t.Fatalf("listing %s: entity.type = %q, want %q", path, ent.Type, types.TypeTreeListing)
	}
	ld, err := types.ListingDataFromEntity(ent)
	if err != nil {
		t.Fatalf("listing %s: ListingDataFromEntity: %v", path, err)
	}
	return ld
}

func asBool(entry interface{}, key string) bool {
	m, ok := entry.(map[interface{}]interface{})
	if ok {
		v, _ := m[key].(bool)
		return v
	}
	if m2, ok := entry.(map[string]interface{}); ok {
		v, _ := m2[key].(bool)
		return v
	}
	return false
}

func hasHash(entry interface{}) bool {
	if m, ok := entry.(map[interface{}]interface{}); ok {
		_, has := m["hash"]
		return has
	}
	if m, ok := entry.(map[string]interface{}); ok {
		_, has := m["hash"]
		return has
	}
	return false
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPublish_SignedRootVerifiesFromTheEmittedFiles is the acceptance
// test for the 2026-08-18 publisher-conformance fix, and it is written
// as a *consumer*: nothing below touches the publishing peer's store.
// It reads only the files a CDN would serve, which is the only vantage
// point from which the old defect was visible — our own
// AppPeer.ReadPublishedRoot rejected the previous {out}/manifest on its
// first gate because that file was the transport profile.
//
// Tier: end-to-end contract pin (TESTING-STRATEGY). It asserts the four
// things a Mode-A consumer does and nothing about how we do them:
//
//  1. MANIFEST_GET returns a 3-key `system/peer/published-root` wire
//     entity whose content_hash matches its own bytes (§6.5.3.1).
//  2. The §5.2 invariant pointer resolves through the ordinary two-hop
//     tree route to a `system/signature` over that content hash, and it
//     verifies against the key carried in the publisher's peer-id.
//  3. `prefix` is present and ends in "/" (§3.3a) — without it a
//     consumer cannot rebuild absolute paths.
//  4. The §6.5.3 publish-side closure is complete: walking the CHAMP
//     trie from root_hash over the emitted content/ shard reaches every
//     interior node and every leaf-bound hash with no 404. This is the
//     failure §6.5.3 names as the one prose review does not catch —
//     "every pointer resolves and a pinned consumer still gets nothing."
func TestPublish_SignedRootVerifiesFromTheEmittedFiles(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	for _, p := range []string{"docs/index", "docs/intro", "docs/chapter-1/start"} {
		if _, err := ap.Store().Put(p, "test/note", map[string]string{"body": p}); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	out := t.TempDir()
	res, err := publish.Publish(context.Background(), publish.Opts{
		Peer:      ap,
		Prefix:    "docs/",
		OutputDir: out,
		OriginURL: "https://test-origin.example",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	peerID := ap.PeerID()

	// (1) MANIFEST_GET — the wire entity, self-consistent.
	manifestRaw, err := os.ReadFile(filepath.Join(out, "manifest"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var rootEnt entity.Entity
	if err := ecf.Decode(manifestRaw, &rootEnt); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if rootEnt.Type != types.TypePeerPublishedRoot {
		t.Fatalf("manifest.type = %q, want %q — the manifest slot is reserved for the signed root (§6.5.3.1)",
			rootEnt.Type, types.TypePeerPublishedRoot)
	}
	if rootEnt.ContentHash.IsZero() {
		t.Fatal("manifest body carries no content_hash; §6.5.3.1 requires the 3-key wire form here")
	}
	computed, err := hash.ComputeFormat(rootEnt.ContentHash.Algorithm, rootEnt.Type, rootEnt.Data)
	if err != nil {
		t.Fatalf("recompute manifest content hash: %v", err)
	}
	if computed != rootEnt.ContentHash {
		t.Fatalf("manifest content_hash disagrees with its own bytes: served=%s computed=%s",
			rootEnt.ContentHash, computed)
	}

	rootData, err := types.PublishedRootDataFromEntity(rootEnt)
	if err != nil {
		t.Fatalf("decode published-root data: %v", err)
	}
	if rootData.PeerID != peerID {
		t.Errorf("published-root.peer_id = %q, want %q", rootData.PeerID, peerID)
	}

	// (3) §3.3a prefix discipline.
	if rootData.Prefix != "docs/" {
		t.Errorf("published-root.prefix = %q, want %q", rootData.Prefix, "docs/")
	}
	if !strings.HasSuffix(rootData.Prefix, "/") {
		t.Errorf("published-root.prefix %q must end in \"/\" (§3.3a) or absolute_prefix+relative_key concatenates wrong", rootData.Prefix)
	}
	if rootData.RootHash != res.SignedRoot.TrieRoot {
		t.Errorf("published-root.root_hash = %s, want the emitted trie root %s", rootData.RootHash, res.SignedRoot.TrieRoot)
	}

	// (2) The §5.2 invariant pointer, resolved the way a consumer does:
	// TREE_GET leaf → hash pointer → CONTENT_GET.
	sigRoute := filepath.Join(out, peerID,
		filepath.FromSlash(types.LocalSignaturePath(rootEnt.ContentHash))) + ".bin"
	sigPointerBody, err := os.ReadFile(sigRoute)
	if err != nil {
		t.Fatalf("read signature tree route %s: %v — a consumer resolving the §5.2 pointer gets a 404", sigRoute, err)
	}
	sigHash := decodeHashPointer(t, sigPointerBody)
	sigEnt := readContentEntity(t, out, sigHash)
	if sigEnt.Type != types.TypeSignature {
		t.Fatalf("signature entity type = %q, want %q", sigEnt.Type, types.TypeSignature)
	}
	sig, err := types.SignatureDataFromEntity(sigEnt)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if sig.Target != rootEnt.ContentHash {
		t.Fatalf("signature targets %s, not the published root %s", sig.Target, rootEnt.ContentHash)
	}
	pub, keyType, ok := crypto.DerivePeerFromPeerID(crypto.PeerID(peerID))
	if !ok {
		t.Fatal("publisher peer-id is not identity-form; the consumer has no key to verify against")
	}
	if !crypto.Verify(keyType, pub, rootEnt.ContentHash.Bytes(), sig.Signature) {
		t.Fatal("published-root signature does not verify against the publisher's own peer-id key")
	}

	// The published-root is also reachable by its advertised PATH, not
	// only by the manifest URL — `signed_pointer` names a path.
	prRoute := filepath.Join(out, peerID,
		filepath.FromSlash(types.PublishedRootStoragePath())) + ".bin"
	prPointerBody, err := os.ReadFile(prRoute)
	if err != nil {
		t.Fatalf("read published-root tree route %s: %v", prRoute, err)
	}
	if got := decodeHashPointer(t, prPointerBody); got != rootEnt.ContentHash {
		t.Errorf("signed_pointer path resolves to %s, want the manifest's %s", got, rootEnt.ContentHash)
	}

	// (4) The closure, walked over the emitted files only.
	seen := map[hash.Hash]bool{}
	var walk func(h hash.Hash)
	walk = func(h hash.Hash) {
		if seen[h] {
			return
		}
		seen[h] = true
		ent := readContentEntity(t, out, h)
		node, err := types.SnapshotNodeDataFromEntity(ent)
		if err != nil {
			// Not a trie node: a leaf-bound entity. Reaching it at all
			// is the assertion.
			return
		}
		for _, e := range node.Data {
			if e.IsLink() {
				walk(*e.Link)
				continue
			}
			for _, tuple := range e.Bucket {
				walk(tuple.ValueHash)
			}
		}
	}
	walk(rootData.RootHash)
	if len(seen) < 4 {
		t.Errorf("closure walk reached %d entities; three seeded notes plus at least one trie node were expected", len(seen))
	}
}

// readContentEntity performs a CONTENT_GET against the emitted origin:
// sharded-2-4 by the full wire hex, body re-hashed before it is used.
// Fatal on a miss, because a miss IS the §6.5.3 closure failure.
func readContentEntity(t *testing.T, outDir string, h hash.Hash) entity.Entity {
	t.Helper()
	hexWire := hex.EncodeToString(h.Bytes())
	path := filepath.Join(outDir, "content", hexWire[0:2], hexWire[2:4], hexWire)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("CONTENT_GET %s: %v — the signed-root closure is incomplete", hexWire, err)
	}
	rehash := sha256.Sum256(raw)
	if !bytes.Equal(rehash[:], h.EffectiveDigest()) {
		t.Fatalf("CONTENT_GET %s: body re-hash mismatch", hexWire)
	}
	var ent entity.Entity
	if err := ecf.Decode(raw, &ent); err != nil {
		t.Fatalf("decode content %s: %v", hexWire, err)
	}
	return ent
}

// TestPublish_SeqAdvancesAcrossRuns pins the §6.5.6 republish MUST —
// "seq MUST increase monotonically across republishes and predecessor
// MUST carry the prior published-root content hash once one exists" —
// across separate Publish calls over one store, which is what a batch
// publisher run twice actually is.
//
// Tier: regression pin. It exists because the obvious implementation
// fails it silently: ext/publishedroot.Publisher holds seq in process
// memory and does not seed it from the bound published-root, so three
// publishes of three DIFFERENT roots through it emit seq=1 /
// predecessor=nil every time. A consumer cannot tell a republish from a
// replay, and the predecessor chain never forms. Measured, then fixed
// by sourcing both from the store — see mintSignedRoot's note.
func TestPublish_SeqAdvancesAcrossRuns(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	var prevRootEntity hash.Hash
	for run := 1; run <= 3; run++ {
		// A different body each run, so the trie root genuinely moves —
		// a pin that republished the same root would pass on a publisher
		// that simply never changed anything.
		if _, err := ap.Store().Put("docs/a", "test/note", map[string]string{"body": strings.Repeat("x", run)}); err != nil {
			t.Fatalf("run %d seed: %v", run, err)
		}
		res, err := publish.Publish(context.Background(), publish.Opts{
			Peer:      ap,
			Prefix:    "docs/",
			OutputDir: t.TempDir(),
			OriginURL: "https://test-origin.example",
		})
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		d := res.SignedRoot.Data

		if d.Seq != uint64(run) {
			t.Fatalf("run %d: seq = %d, want %d — seq must increase across republishes (§6.5.6)", run, d.Seq, run)
		}
		if run == 1 {
			if d.Predecessor != nil {
				t.Errorf("run 1: predecessor = %s, want nil on the first published root", d.Predecessor)
			}
		} else {
			if d.Predecessor == nil {
				t.Fatalf("run %d: predecessor is nil; §6.5.6 requires the prior published-root hash once one exists", run)
			}
			if *d.Predecessor != prevRootEntity {
				t.Errorf("run %d: predecessor = %s, want the prior published-root %s", run, *d.Predecessor, prevRootEntity)
			}
		}
		prevRootEntity = res.SignedRoot.Root.ContentHash
	}
}

// TestPublish_IsByteStableWithAPinnedInstant pins the property the
// cross-impl fixture rests on: two runs of the same publish, from the
// same seed and the same Opts.At, emit byte-identical directories.
//
// Tier: integration (TESTING-STRATEGY) — it drives the real emit path
// end to end and asserts on the bytes on disk, because "byte-identical"
// is a claim about the bytes and nothing weaker substantiates it.
//
// This is a regression pin with a source. The fixture handed to
// entity-browser-rust was documented as byte-identical on their
// machine and was not: `published_at` lives INSIDE the published-root
// entity, so a fresh clock moved the root's content hash, the
// `system/signature/{root_hex}.bin` binding named after it, two content
// shards, and {out}/manifest. Only the trie root and the entities
// beneath it were ever stable — which excludes every artifact a
// consumer's reader enters through. The second half of the test is the
// half that would have caught it: it asserts the instant is genuinely
// load-bearing, so a future refactor that drops At on the floor fails
// here rather than in another implementation's test run.
func TestPublish_IsByteStableWithAPinnedInstant(t *testing.T) {
	seed := [32]byte{
		0x62, 0x79, 0x74, 0x65, 0x2d, 0x73, 0x74, 0x61,
		0x62, 0x6c, 0x65, 0x2d, 0x70, 0x75, 0x62, 0x6c,
		0x69, 0x73, 0x68, 0x2d, 0x70, 0x69, 0x6e, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
	}
	at := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)

	emit := func(t *testing.T, at time.Time) (string, hash.Hash) {
		t.Helper()
		kp := crypto.FromSeed(seed)
		ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
		if err != nil {
			t.Fatalf("CreatePeer: %v", err)
		}
		defer ap.Close()

		// Sorted, so the seeding order cannot be what makes the two
		// runs agree.
		pages := []struct{ path, body string }{
			{"docs/deep/one", "nested one level"},
			{"docs/deep/two/leaf", "nested two levels"},
			{"docs/index", "home"},
			{"docs/intro", "second page"},
		}
		for _, p := range pages {
			if _, err := ap.Store().Put(p.path, "test/note", map[string]string{"body": p.body}); err != nil {
				t.Fatalf("seed %s: %v", p.path, err)
			}
		}

		out := t.TempDir()
		res, err := publish.Publish(context.Background(), publish.Opts{
			Peer:      ap,
			Prefix:    "docs/",
			OutputDir: out,
			OriginURL: "https://go-arm.example",
			At:        at,
		})
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		return out, res.SignedRoot.Root.ContentHash
	}

	dirA, rootA := emit(t, at)
	dirB, rootB := emit(t, at)

	digestA := dirDigest(t, dirA)
	digestB := dirDigest(t, dirB)

	for rel, sum := range digestA {
		other, ok := digestB[rel]
		if !ok {
			t.Errorf("%s emitted by the first run and not the second", rel)
			continue
		}
		if other != sum {
			t.Errorf("%s differs between two identical runs: %s vs %s", rel, sum, other)
		}
	}
	for rel := range digestB {
		if _, ok := digestA[rel]; !ok {
			t.Errorf("%s emitted by the second run and not the first", rel)
		}
	}
	if rootA != rootB {
		t.Errorf("published-root entity hash moved between identical runs: %s vs %s", rootA, rootB)
	}

	// The instant is load-bearing, not decoration: move it by one
	// millisecond and the signed root's content address moves, which is
	// exactly why a fixture cannot be reproducible without pinning it.
	_, rootLater := emit(t, at.Add(time.Millisecond))
	if rootLater == rootA {
		t.Errorf("published-root entity hash %s is unchanged after moving Opts.At; "+
			"published_at is inside the entity (§3.3a) and must be part of its content address", rootA)
	}
}

// dirDigest maps every regular file under root to the sha256 of its
// bytes, keyed by path relative to root.
func dirDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
