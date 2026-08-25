package publish

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/tree"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/publishedroot"

	"entity-workbench-go/entitysdk"
)

// SignedRoot is what the `signed_pointer` advertisement obliges this
// publisher to actually serve: the signed `system/peer/published-root`
// entity, the `system/signature` that authenticates it, and the trie
// root the two of them commit to.
//
// Before 2026-08-18 the corridor advertised
// `signed_pointer: "system/peer/published-root"` and emitted no
// signature at all — the artifact at `{manifest_url_prefix}` was the
// http-poll transport profile, which EXTENSION-NETWORK §6.5.3.1 pins
// as non-conformant in as many words ("Serving any other entity here
// — in particular a `system/peer/transport/*` profile — is
// non-conformant, however plausible it looks"). Our own consumer-side
// reader (entitysdk.AppPeer.ReadPublishedRoot) rejected our own
// publisher's output on its first gate, which is how the defect
// surfaced without a spec argument.
type SignedRoot struct {
	// Root is the `system/peer/published-root` wire entity — the body
	// MANIFEST_GET serves, 3-key ECF including its content_hash.
	Root entity.Entity
	// Signature is the `system/signature` entity bound at the
	// ENTITY-CORE-PROTOCOL §5.2 invariant pointer for Root.
	Signature entity.Entity
	// Data is Root decoded, so callers (and the CLI summary) can read
	// seq / prefix / root_hash without re-decoding.
	Data types.PublishedRootData
	// TrieRoot is the CHAMP trie root the published root commits to.
	TrieRoot hash.Hash
	// ClosureSize is how many distinct hashes the §6.5.3 publish-side
	// closure obligation contributed to the emitted content set (trie
	// root + interior nodes + leaf-bound hashes).
	ClosureSize int
}

// mintSignedRoot builds the trie over prefix, signs a published-root
// over its root hash, and binds both at their canonical paths.
//
// The trie is built directly with tree.BuildTrieForPrefix rather than
// through a RootTracker sync hook, because publishing is a batch
// operation over an already-populated store: there is no event stream
// to ride, and a full build is exactly what RootTracker.Load() does at
// startup for a freshly-enabled prefix.
//
// BuildTrie writes every node it constructs into the content store, so
// the closure the §6.5.3 obligation names is resolvable locally the
// moment this returns.
//
// WHY THIS DOES NOT USE ext/publishedroot.Publisher, which is the
// engine for exactly this entity and which our own tests drive
// elsewhere: that Publisher keeps `lastSeq` and `lastHash` in process
// memory and never seeds them from the published-root already bound in
// the store. A batch publisher is a fresh process every run, so seq
// restarts at 1 and predecessor stays nil forever. Measured, not
// inferred — three consecutive publishes of three DIFFERENT roots
// through the Publisher emitted seq=1 / predecessor=nil every time.
// §6.5.6 makes both a MUST: "seq MUST increase monotonically across
// republishes and predecessor MUST carry the prior published-root
// content hash once one exists."
//
// So the seq/predecessor sourcing is ours and everything else is
// core-go's: types.PublishedRootData, types.SignatureData, the keypair,
// the §5.2 invariant pointer path, the publisher self-tag, and the
// binding order. Routed to core-go as an ask for a seeding option
// (2026-08-18); when it lands this collapses back to two calls.
func mintSignedRoot(ap *entitysdk.AppPeer, prefix string, at time.Time) (SignedRoot, error) {
	cs := ap.RawContentStore()
	li := ap.RawLocationIndex()
	peerID := ap.PeerID()

	trieRoot, err := tree.BuildTrieForPrefix(cs, li, crypto.PeerID(peerID), prefix)
	if err != nil {
		return SignedRoot{}, fmt.Errorf("publish: build trie for prefix %q: %w", prefix, err)
	}
	if trieRoot.IsZero() {
		return SignedRoot{}, fmt.Errorf("publish: prefix %q has no bindings, so there is no root to sign", prefix)
	}

	prevSeq, prevHash := priorPublishedRoot(cs, li)

	data := types.PublishedRootData{
		PeerID:   peerID,
		RootHash: trieRoot,
		// §3.3a: REQUIRED and MUST end with "/" — a consumer rebuilds
		// absolute paths as `prefix + relative_key`, and "" would
		// concatenate into a wrong path rather than fail.
		Prefix:      publishedPrefix(prefix),
		Seq:         prevSeq + 1,
		PublishedAt: uint64(at.UnixMilli()),
		Predecessor: prevHash,
	}
	rootEnt, err := data.ToEntity()
	if err != nil {
		return SignedRoot{}, fmt.Errorf("publish: encode published-root: %w", err)
	}

	kp := ap.RawPeer().Keypair()
	sigData := types.SignatureData{
		Target:    rootEnt.ContentHash,
		Signer:    ap.RawPeer().Identity().ContentHash,
		Algorithm: crypto.KeyTypeString(kp.KeyType),
		Signature: kp.Sign(rootEnt.ContentHash.Bytes()),
	}
	sigEnt, err := sigData.ToEntity()
	if err != nil {
		return SignedRoot{}, fmt.Errorf("publish: encode published-root signature: %w", err)
	}
	if _, err := cs.Put(rootEnt); err != nil {
		return SignedRoot{}, fmt.Errorf("publish: store published-root: %w", err)
	}
	if _, err := cs.Put(sigEnt); err != nil {
		return SignedRoot{}, fmt.Errorf("publish: store published-root signature: %w", err)
	}

	// ORDER IS LOAD-BEARING and is core-go's, kept verbatim: bind the
	// SIGNATURE first, the published-root second. The published-root
	// binding is what makes a new head visible, and a consumer that
	// observes the head before the signature is bound caches a head it
	// cannot verify. Same discipline as SUBSTITUTE §7.3 — bind what is
	// referenced before the reference that makes it reachable.
	//
	// The self-tag is core-go's too: a RootTracker in this process must
	// skip our writes, or binding the published-root under a tracked
	// prefix advances the tracked root, which republishes, forever.
	ctx := &store.MutationContext{
		AuthorHash:     ap.RawPeer().Identity().ContentHash,
		HandlerPattern: publishedroot.PublisherHandlerPattern,
		Operation:      "publish",
	}
	if err := bindTagged(li, ctx, types.LocalSignaturePath(rootEnt.ContentHash), sigEnt.ContentHash); err != nil {
		return SignedRoot{}, err
	}
	if err := bindTagged(li, ctx, types.PublishedRootStoragePath(), rootEnt.ContentHash); err != nil {
		return SignedRoot{}, err
	}

	return SignedRoot{
		Root:      rootEnt,
		Signature: sigEnt,
		Data:      data,
		TrieRoot:  trieRoot,
	}, nil
}

// priorPublishedRoot reads the published-root this peer already has
// bound, returning its seq and content hash. Both zero-valued when
// there is none (the first publish) or when what is bound does not
// decode — a corrupt predecessor is not a reason to refuse to publish,
// but it IS a reason not to claim a chain we cannot substantiate.
func priorPublishedRoot(cs store.ContentStore, li store.LocationIndex) (uint64, *hash.Hash) {
	h, ok := li.Get(types.PublishedRootStoragePath())
	if !ok {
		return 0, nil
	}
	ent, ok := cs.Get(h)
	if !ok {
		fmt.Printf("  warn: prior published-root %s is bound but not stored; seq restarts\n", h)
		return 0, nil
	}
	prev, err := types.PublishedRootDataFromEntity(ent)
	if err != nil {
		fmt.Printf("  warn: prior published-root %s does not decode (%v); seq restarts\n", h, err)
		return 0, nil
	}
	prevHash := ent.ContentHash
	return prev.Seq, &prevHash
}

// publishedPrefix normalizes to the §3.3a form: "/" is the universal
// tree, everything else ends in "/". Mirrors core-go's unexported
// publishedroot.publishedPrefix.
func publishedPrefix(prefix string) string {
	if prefix == "" {
		return "/"
	}
	if strings.HasSuffix(prefix, "/") {
		return prefix
	}
	return prefix + "/"
}

// bindTagged writes one location-index binding carrying the publisher
// self-tag, falling back to an untagged Set on an index that does not
// implement ContextualWriter.
func bindTagged(li store.LocationIndex, ctx *store.MutationContext, path string, target hash.Hash) error {
	if cw, ok := li.(store.ContextualWriter); ok {
		if _, err := cw.SetWithContext(path, target, ctx); err != nil {
			return fmt.Errorf("publish: bind %s: %w", path, err)
		}
		return nil
	}
	if err := li.Set(path, target); err != nil {
		return fmt.Errorf("publish: bind %s: %w", path, err)
	}
	return nil
}

// addSignedRootClosure adds everything EXTENSION-NETWORK §6.5.3's
// publish-side closure obligation names to the emitted content set:
// the transitive hash-linked closure of `published-root.root_hash`
// (trie root, interior nodes, leaf-bound content hashes), the
// published-root entity itself, and its signature.
//
// The obligation is a MUST, so this deliberately does NOT run entries
// through Opts.IncludeType: a caller-supplied type filter is a choice
// about which of *its own* bindings to publish, and letting it drop an
// interior CHAMP node would leave the advertisement true and the walk
// broken — the failure mode §6.5.3 calls out by name ("every pointer
// resolves and a pinned consumer still gets nothing").
func addSignedRootClosure(cs store.ContentStore, sr *SignedRoot, closure map[hash.Hash]entity.Entity) {
	reachable := tree.CollectNodeClosure(cs, sr.TrieRoot)
	added := 0
	for h := range reachable {
		if _, seen := closure[h]; seen {
			continue
		}
		ent, ok := cs.Get(h)
		if !ok {
			// CollectNodeClosure is best-effort over a locally complete
			// trie; a miss here means the store does not hold something
			// the root commits to, which a consumer would see as a 404
			// mid-walk. Say so rather than emitting silently.
			fmt.Printf("  warn: signed-root closure: %s is unreachable locally\n", h)
			continue
		}
		closure[h] = ent
		added++
	}
	sr.ClosureSize = len(reachable)

	if _, seen := closure[sr.Root.ContentHash]; !seen {
		closure[sr.Root.ContentHash] = sr.Root
		added++
	}
	if _, seen := closure[sr.Signature.ContentHash]; !seen {
		closure[sr.Signature.ContentHash] = sr.Signature
		added++
	}
	fmt.Printf("signed root: seq=%d prefix=%q root=%s (+%d entities for the closure)\n",
		sr.Data.Seq, sr.Data.Prefix, sr.TrieRoot, added)
}

// emitSignedRoot writes the two objects a consumer needs to verify the
// origin:
//
//	{out}/manifest
//	    MANIFEST_GET. The published-root as a 3-key wire entity
//	    ECF({type, data, content_hash}) per §6.5.3.1 — the signature's
//	    target is that content_hash, so unlike every other route on
//	    this surface the body is NOT the bare hashable form.
//
//	{out}/{peer_id}/system/signature/{hex}.bin
//	    The §5.2 invariant pointer as an ordinary TREE_GET leaf, so a
//	    static consumer resolves the signature by the same path a live
//	    one does (hash pointer → CONTENT_GET, two hops).
//
// The published-root binding at
// {peer_id}/system/peer/published-root/{peer_id}.bin is emitted too:
// `signed_pointer` names a *path*, and a consumer that reaches this
// origin holding that path rather than the manifest URL must find it.
func emitSignedRoot(outDir string, sr SignedRoot) (int64, error) {
	var total int64

	wire, err := ecf.Encode(sr.Root)
	if err != nil {
		return 0, fmt.Errorf("publish: encode published-root wire entity: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest"), wire, 0o644); err != nil {
		return 0, fmt.Errorf("publish: write manifest: %w", err)
	}
	total += int64(len(wire))

	peerID := sr.Data.PeerID
	routes := map[string]hash.Hash{
		types.LocalSignaturePath(sr.Root.ContentHash): sr.Signature.ContentHash,
		types.PublishedRootStoragePath():              sr.Root.ContentHash,
	}
	for bare, target := range routes {
		body, err := encodeHashPointer(target)
		if err != nil {
			return total, fmt.Errorf("publish: encode pointer for %s: %w", bare, err)
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
