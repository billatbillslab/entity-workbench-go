package entitysdk

// The consumer half of `system/peer/published-root` — the reader that
// N3 of REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17 says we did
// not have. It is step 3a of §5.1: the prerequisite that gates step 3
// (a target prefix on the sync surface), because arch's amendment
// makes the target's value the *publisher's declared prefix* rather
// than a caller-chosen string, and there was no way to read one.
//
// What a published-root is (PROPOSAL-PEER-MANIFEST-STATIC-HANDSHAKE §4,
// NORMATIVE-LOCKED): a standalone entity in which a peer signs the
// current root hash of a prefix of its own tree. A consumer that
// reaches the peer's data by an untrusted path verifies the root claim
// against the publisher's identity key, then walks TREE_GET from that
// signed root — every binding reachable from it is hash-chained, and
// every binding the host offers *outside* that chain is rejected. §1.1
// tree-binding fabrication is the threat the whole shape exists for.
//
// We were only ever on the writing side of this: `publish/publish.go`
// emits a `signed_pointer: "system/peer/published-root"` in the CDN
// corridor manifest, and nothing in the tree had ever read one. This
// file is also the first signature *verification* in our tree — every
// other `types.SignatureData` site in `entitysdk` mints one.
//
// Three verification decisions worth stating, since they are the ones
// a later reader will want to re-litigate:
//
//  1. **There is no unverified success.** core-go's HTTP-side reader
//     (`ext/httplive.Outbound.FetchPublishedRoot`) returns a
//     `Verified bool` because it may have no pinned identity to check
//     against. We are not in that position: V7 §1.5 identity-form
//     peer-ids *carry* the public key, so the key is derivable from
//     the peer-id we were asked about. Where it is not derivable
//     (SHA-256-form peer-id) the caller supplies it via
//     WithPublisherKey or gets an error. A `Verified: false` that a
//     caller forgets to check is how an unsigned root gets treated as
//     a signed one, so the field does not exist.
//  2. **The seq gate runs after signature verification, not before.**
//     The freshness floor is per-publisher state, and admitting an
//     unauthenticated seq into it lets anyone who can answer a
//     tree:get poison the floor to `MaxUint64` and lock out the real
//     publisher forever. Authenticate, then remember.
//  3. **We do not walk the root.** This returns the signed claim; the
//     hash-chain walk from `RootHash` is the consumer's next step and
//     belongs to whoever is doing the sync. Returning a verified
//     pointer is a smaller and more honest contract than half of a
//     traversal.

import (
	"context"
	"fmt"
	"strings"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// PublishedRoot is a verified view of one peer's published-root.
//
// A value of this type only exists if the signature over Entity's
// content hash checked out against the publisher's key — see the
// package note above on why there is no `Verified` field.
type PublishedRoot struct {
	// Entity is the wire entity as served, with ContentHash set to
	// the *recomputed* hash rather than the one the host claimed.
	Entity entity.Entity

	// Data is the decoded payload. Prefix and RootHash are the two
	// fields the sync surface needs: they are respectively the
	// absolute prefix the publisher's keys are relative to
	// (EXTENSION-TREE §3.3a) and the tree root it committed to.
	Data types.PublishedRootData

	// Signature is the system/signature entity that authenticated
	// Data, resolved from the V7 §5.2/§975 invariant pointer.
	// Retained so callers can record what they verified against.
	Signature types.SignatureData
}

// publishedRootOpts collects ReadPublishedRoot's optional inputs.
type publishedRootOpts struct {
	seqFloor    *uint64
	publisherPK []byte
	publisherKT byte
	havePK      bool
}

// PublishedRootOption configures a ReadPublishedRoot call.
type PublishedRootOption func(*publishedRootOpts)

// WithSeqFloor supplies a caller-held freshness floor, rejecting any
// published-root whose seq is below it.
//
// Use this when the caller persists the floor. The AppPeer's own
// floor is in-memory and process-lifetime only (see
// PublishedRootSeqFloor), which means a restart forgets it — and a
// forgotten floor is exactly the window a rollback attack wants. An
// application that cares about rollback across restarts stores the
// last-accepted seq itself and passes it here; the effective floor is
// the higher of the two.
func WithSeqFloor(seq uint64) PublishedRootOption {
	return func(o *publishedRootOpts) { o.seqFloor = &seq }
}

// WithPublisherKey supplies the publisher's public key out-of-band.
//
// Required only for SHA-256-form peer-ids (V7 §1.5 hash_type 0x01),
// where the peer-id is a digest of the key rather than the key, so
// nothing can be derived from it. For identity-form peer-ids — what
// `crypto.Generate` produces and what every peer in this repo uses —
// the key comes from the peer-id and this option is unnecessary.
func WithPublisherKey(publicKey []byte, keyType byte) PublishedRootOption {
	return func(o *publishedRootOpts) {
		o.publisherPK = publicKey
		o.publisherKT = keyType
		o.havePK = true
	}
}

// ReadPublishedRoot fetches, verifies, and returns peerID's current
// published-root.
//
// The read is a dispatched tree:get at the canonical storage path
// (`types.PublishedRootStoragePath`) inside peerID's own namespace, so
// it works unchanged against a remote peer over a connection or
// against a local mirror of that namespace already in our tree. For a
// remote read the caller must have connected first (AppPeer.Connect)
// and the connection's grants must cover the publisher's `system/`
// paths.
//
// What is checked, in order:
//
//   - peerID is a well-formed V7 §1.5 peer-id;
//   - the served entity is `system/peer/published-root`;
//   - its content hash recomputes from the bytes served (publisher or
//     transit corruption);
//   - the payload's `peer_id` is the peer we asked about — a peer may
//     hold other peers' roots, but its *own* storage path may only
//     hold its own;
//   - `prefix` is present and ends in "/" (EXTENSION-TREE §3.3a
//     REQUIRED; the field's absence is what made three conformant
//     impls publish mutually unreadable keys, so we refuse rather than
//     default it);
//   - the signature at `system/signature/{hex(content_hash)}` verifies
//     against the publisher's key;
//   - `seq` is not below the freshness floor.
//
// ctx is accepted for symmetry with the rest of the remote surface and
// with ReconcileSinceLastSeen, its intended caller. It is not yet
// honored: the dispatched tree:get underneath is synchronous and has
// no cancellation seam. Stated rather than hidden.
func (a *AppPeer) ReadPublishedRoot(ctx context.Context, peerID string, opts ...PublishedRootOption) (PublishedRoot, error) {
	_ = ctx

	o := &publishedRootOpts{}
	for _, fn := range opts {
		fn(o)
	}

	if peerID == "" {
		return PublishedRoot{}, NewError(400, "invalid_peer", "peerID is empty")
	}
	if err := crypto.PeerID(peerID).Validate(); err != nil {
		return PublishedRoot{}, WrapError(400, "invalid_peer",
			fmt.Sprintf("peerID %q is not a valid V7 §1.5 peer-id", peerID), err)
	}

	pub, keyType, err := publisherKey(peerID, o)
	if err != nil {
		return PublishedRoot{}, err
	}

	path := "/" + peerID + "/" + types.PublishedRootStoragePath()
	ent, found, err := a.Get(path)
	if err != nil {
		return PublishedRoot{}, WrapError(StatusOf(err), "published_root_get_failed",
			"tree:get "+path, err)
	}
	if !found {
		return PublishedRoot{}, NewError(404, "no_published_root",
			"no published-root bound at "+path)
	}
	ent, data, err := checkPublishedRootEntity(peerID, path, ent)
	if err != nil {
		return PublishedRoot{}, err
	}

	sig, err := a.verifyPublishedRootSignature(peerID, ent, pub, keyType)
	if err != nil {
		return PublishedRoot{}, err
	}

	// Freshness last — see decision 2 in the package note: the floor
	// is only ever advanced by a payload we have authenticated.
	if err := a.gatePublishedRootSeq(peerID, data.Seq, o.seqFloor); err != nil {
		return PublishedRoot{}, err
	}

	return PublishedRoot{Entity: ent, Data: data, Signature: sig}, nil
}

// checkPublishedRootEntity validates everything about a served
// published-root that can be decided from its own bytes, and returns
// the entity with its content hash corrected to the recomputed value.
//
// Split out from ReadPublishedRoot so it is reachable without a
// hostile peer: the corruption branch below cannot be provoked through
// a real store, which computes hashes rather than accepting claimed
// ones, and an unreachable check is an unverified one.
func checkPublishedRootEntity(peerID, path string, ent entity.Entity) (entity.Entity, types.PublishedRootData, error) {
	var zero types.PublishedRootData

	if ent.Type != types.TypePeerPublishedRoot {
		return ent, zero, NewError(500, "unexpected_result_type",
			fmt.Sprintf("%s holds type %q, want %s", path, ent.Type, types.TypePeerPublishedRoot))
	}

	// Recompute from the bytes served. The claimed hash is the
	// signature's target, so a host that could alter the payload and
	// restate the hash would otherwise be verifying its own claim
	// against itself. Published-roots are authored under the
	// publisher's process-global content_hash_format (v7.67 §2.3), so
	// the claimed algorithm selects the function.
	alg := ent.ContentHash.Algorithm
	if ent.ContentHash.IsZero() {
		alg = hash.AlgorithmSHA256
	}
	computed, err := hash.ComputeFormat(alg, ent.Type, ent.Data)
	if err != nil {
		return ent, zero, WrapError(500, "hash_failed",
			"recompute published-root content hash", err)
	}
	if !ent.ContentHash.IsZero() && ent.ContentHash != computed {
		return ent, zero, NewError(500, "content_hash_mismatch",
			fmt.Sprintf("published-root content_hash disagrees with served bytes: served=%s computed=%s",
				ent.ContentHash, computed))
	}
	ent.ContentHash = computed

	data, err := types.PublishedRootDataFromEntity(ent)
	if err != nil {
		return ent, zero, WrapError(500, "decode_failed",
			"decode published-root payload", err)
	}
	if data.PeerID != peerID {
		return ent, zero, NewError(500, "peer_id_mismatch",
			fmt.Sprintf("published-root at %s declares peer_id %s", path, data.PeerID))
	}
	if data.Prefix == "" {
		return ent, zero, NewError(500, "missing_prefix",
			"published-root omits `prefix`, REQUIRED per EXTENSION-TREE §3.3a — "+
				"without it the relative keys under root_hash cannot be rebuilt into absolute paths")
	}
	if !strings.HasSuffix(data.Prefix, "/") {
		return ent, zero, NewError(500, "invalid_prefix",
			fmt.Sprintf("published-root prefix %q does not end in \"/\" (EXTENSION-TREE §3.3a MUST); "+
				"`absolute_prefix + relative_key` would concatenate into a wrong path", data.Prefix))
	}
	return ent, data, nil
}

// publisherKey resolves the key the published-root's signature must
// verify against: the caller-supplied one when given, otherwise the
// one carried inside an identity-form peer-id.
func publisherKey(peerID string, o *publishedRootOpts) ([]byte, byte, error) {
	if o.havePK {
		return o.publisherPK, o.publisherKT, nil
	}
	pub, keyType, ok := crypto.DerivePeerFromPeerID(crypto.PeerID(peerID))
	if !ok {
		return nil, 0, NewError(400, "unverifiable_peer_id",
			fmt.Sprintf("peer-id %s is not identity-form, so the publisher's public key is not "+
				"derivable from it; supply it out-of-band with WithPublisherKey", peerID))
	}
	return pub, keyType, nil
}

// verifyPublishedRootSignature resolves the V7 §5.2/§975 invariant
// pointer `/{peer}/system/signature/{hex(target)}` and checks the
// signature over the published-root's content hash.
//
// The pointer path is derived from the hash we recomputed, not from
// the one the host served, so a host cannot steer us at a signature it
// prepared for different bytes.
func (a *AppPeer) verifyPublishedRootSignature(peerID string, ent entity.Entity, pub []byte, keyType byte) (types.SignatureData, error) {
	sigPath := "/" + peerID + "/" + types.LocalSignaturePath(ent.ContentHash)
	sigEnt, found, err := a.Get(sigPath)
	if err != nil {
		return types.SignatureData{}, WrapError(StatusOf(err), "signature_get_failed",
			"tree:get "+sigPath, err)
	}
	if !found {
		return types.SignatureData{}, NewError(404, "missing_signature",
			"no signature bound at "+sigPath+" — the published-root is unverifiable")
	}
	if sigEnt.Type != types.TypeSignature {
		return types.SignatureData{}, NewError(500, "unexpected_result_type",
			fmt.Sprintf("%s holds type %q, want %s", sigPath, sigEnt.Type, types.TypeSignature))
	}
	sig, err := types.SignatureDataFromEntity(sigEnt)
	if err != nil {
		return types.SignatureData{}, WrapError(500, "decode_failed",
			"decode published-root signature", err)
	}
	if sig.Target != ent.ContentHash {
		return types.SignatureData{}, NewError(403, "signature_target_mismatch",
			fmt.Sprintf("signature at %s targets %s, not %s", sigPath, sig.Target, ent.ContentHash))
	}
	sigKeyType, ok := crypto.KeyTypeByte(sig.Algorithm)
	if !ok {
		return types.SignatureData{}, NewError(501, "unsupported_algorithm",
			fmt.Sprintf("signature algorithm %q is not a known key type", sig.Algorithm))
	}
	if sigKeyType != keyType {
		return types.SignatureData{}, NewError(403, "signature_algorithm_mismatch",
			fmt.Sprintf("signature algorithm %q does not match the publisher key type 0x%02x",
				sig.Algorithm, keyType))
	}
	if !crypto.Verify(sigKeyType, pub, ent.ContentHash.Bytes(), sig.Signature) {
		return types.SignatureData{}, NewError(403, "signature_invalid",
			"published-root signature does not verify against the publisher's key")
	}
	return sig, nil
}

// gatePublishedRootSeq enforces the per-publisher monotonic freshness
// floor (published-root §4 point 2, the snapshot-manifest §3-RES.4
// discipline) and advances it on success.
//
// The effective floor is the higher of the AppPeer's in-memory floor
// and any caller-supplied one.
func (a *AppPeer) gatePublishedRootSeq(peerID string, seq uint64, callerFloor *uint64) error {
	a.prSeqMu.Lock()
	defer a.prSeqMu.Unlock()

	floor, have := a.prSeqFloor[peerID]
	if callerFloor != nil && (!have || *callerFloor > floor) {
		floor, have = *callerFloor, true
	}
	if have && seq < floor {
		return NewError(409, "stale_published_root",
			fmt.Sprintf("published-root seq %d is below the freshness floor %d for %s "+
				"(rollback rejected per §3-RES.4)", seq, floor, peerID))
	}
	if a.prSeqFloor == nil {
		a.prSeqFloor = make(map[string]uint64)
	}
	if !have || seq > floor {
		a.prSeqFloor[peerID] = seq
	} else {
		a.prSeqFloor[peerID] = floor
	}
	return nil
}

// PublishedRootSeqFloor reports the highest published-root seq this
// AppPeer has accepted for peerID, and whether it has accepted any.
//
// The floor is in-memory and lives for the process only. That is a
// real limit, not an oversight to read past: rollback protection
// across a restart requires the application to persist this value and
// hand it back via WithSeqFloor. Persisting it here would make a read
// call write to the tree, which is the wrong shape for an SDK reader.
func (a *AppPeer) PublishedRootSeqFloor(peerID string) (uint64, bool) {
	a.prSeqMu.Lock()
	defer a.prSeqMu.Unlock()
	seq, ok := a.prSeqFloor[peerID]
	return seq, ok
}
