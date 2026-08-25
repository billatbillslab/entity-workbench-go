// Package publishedroot holds the gates a consumer applies to a
// `system/peer/published-root` — the checks that are decidable from the
// bytes themselves, independent of where those bytes came from.
//
// **It exists because we have two consumers of the same normative
// surface and only ever wrote one set of checks.** `entitysdk`'s
// `AppPeer.ReadPublishedRoot` reads a root out of the local store over a
// dispatched tree:get; `fetch`'s wire consumer reads one over HTTP from
// a CDN origin. The transport differs and *nothing else does*: the type
// check, the content-hash recompute, the `peer_id` self-declaration, the
// §3.3a `prefix` discipline and the two-hop signature verification are
// the same seven gates in the same order either way. A second copy of
// them in the wire consumer would be a second place for the ruling to
// land next time one moves — and the halves would agree until they did
// not, which is the failure this package's whole cohort keeps repeating.
//
// **Deliberately dependency-light.** Only `core/{crypto,entity,hash,types}`,
// so the standalone `entity-fetch` binary can link it without dragging in
// the peer, the store, or sqlite. That constraint is why it is a
// subpackage of `entitysdk` rather than a file inside it.
//
// Errors are [Fault], carrying an SDK-shaped status + code so
// `entitysdk` can map them onto `*entitysdk.Error` (preserving its error
// predicates) while `fetch` can render them directly.
package publishedroot

import (
	"fmt"
	"strings"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// Fault is a gate failure, shaped like an SDK error without depending on
// one. Status/Code are the values `entitysdk` has published for these
// conditions since 2026-08-17 and are part of its surface.
type Fault struct {
	Status  uint
	Code    string
	Message string
	Cause   error
}

func (f *Fault) Error() string {
	base := fmt.Sprintf("published-root %d (%s)", f.Status, f.Code)
	if f.Message != "" {
		base += ": " + f.Message
	}
	return base
}

func (f *Fault) Unwrap() error { return f.Cause }

func newFault(status uint, code, message string) *Fault {
	return &Fault{Status: status, Code: code, Message: message}
}

func wrapFault(status uint, code, message string, cause error) *Fault {
	return &Fault{Status: status, Code: code, Message: message, Cause: cause}
}

// Check validates everything about a served published-root that can be
// decided from its own bytes, and returns the entity with its content
// hash corrected to the recomputed value.
//
// `where` names the place the bytes came from — a tree path for a store
// read, a URL for a wire read — and appears verbatim in every message,
// because "this root is malformed" is not actionable without it.
//
// The recompute is the load-bearing step: the claimed hash is the
// signature's target, so a host that could alter the payload and restate
// the hash would otherwise be verifying its own claim against itself.
func Check(peerID, where string, ent entity.Entity) (entity.Entity, types.PublishedRootData, error) {
	var zero types.PublishedRootData

	if ent.Type != types.TypePeerPublishedRoot {
		return ent, zero, newFault(500, "unexpected_result_type",
			fmt.Sprintf("%s holds type %q, want %s", where, ent.Type, types.TypePeerPublishedRoot))
	}

	// Published-roots are authored under the publisher's process-global
	// content_hash_format (v7.67 §2.3), so the claimed algorithm selects
	// the function.
	alg := ent.ContentHash.Algorithm
	if ent.ContentHash.IsZero() {
		alg = hash.AlgorithmSHA256
	}
	computed, err := hash.ComputeFormat(alg, ent.Type, ent.Data)
	if err != nil {
		return ent, zero, wrapFault(500, "hash_failed",
			"recompute published-root content hash", err)
	}
	if !ent.ContentHash.IsZero() && ent.ContentHash != computed {
		return ent, zero, newFault(500, "content_hash_mismatch",
			fmt.Sprintf("published-root content_hash disagrees with served bytes: served=%s computed=%s",
				ent.ContentHash, computed))
	}
	ent.ContentHash = computed

	data, err := types.PublishedRootDataFromEntity(ent)
	if err != nil {
		return ent, zero, wrapFault(500, "decode_failed",
			"decode published-root payload", err)
	}
	if data.PeerID != peerID {
		return ent, zero, newFault(500, "peer_id_mismatch",
			fmt.Sprintf("published-root at %s declares peer_id %s", where, data.PeerID))
	}
	if data.Prefix == "" {
		return ent, zero, newFault(500, "missing_prefix",
			"published-root omits `prefix`, REQUIRED per EXTENSION-TREE §3.3a — "+
				"without it the relative keys under root_hash cannot be rebuilt into absolute paths")
	}
	if !strings.HasSuffix(data.Prefix, "/") {
		return ent, zero, newFault(500, "invalid_prefix",
			fmt.Sprintf("published-root prefix %q does not end in \"/\" (EXTENSION-TREE §3.3a MUST); "+
				"`absolute_prefix + relative_key` would concatenate into a wrong path", data.Prefix))
	}
	return ent, data, nil
}

// DeriveKey returns the public key carried inside an identity-form
// peer-id (V7 §1.5).
//
// **This is why the chain has no key distribution step.** A SHA-256-form
// peer-id is a digest of the key rather than the key, so nothing is
// derivable and the caller must supply one out of band; that case is an
// error here rather than a silent unverified success.
func DeriveKey(peerID string) ([]byte, byte, error) {
	pub, keyType, ok := crypto.DerivePeerFromPeerID(crypto.PeerID(peerID))
	if !ok {
		return nil, 0, newFault(400, "unverifiable_peer_id",
			fmt.Sprintf("peer-id %s is not identity-form, so the publisher's public key is not "+
				"derivable from it; supply it out-of-band", peerID))
	}
	return pub, keyType, nil
}

// SignatureRelPath is the peer-relative tree path of the V7 §5.2/§975
// invariant pointer for target — `system/signature/{hex(target)}`.
//
// Callers derive it from the hash they RECOMPUTED, never from the one
// the host served, so a host cannot steer a consumer at a signature it
// prepared for different bytes. Both consumers depend on that ordering;
// stating it here is what keeps it from being re-derived wrongly.
func SignatureRelPath(target hash.Hash) string {
	return types.LocalSignaturePath(target)
}

// VerifySignature checks a resolved `system/signature` entity against the
// published-root's recomputed content hash and the publisher's key.
//
// `where` names where the signature entity was resolved from.
func VerifySignature(where string, root, sigEnt entity.Entity, pub []byte, keyType byte) (types.SignatureData, error) {
	return VerifySignatureOver("published-root", where, root, sigEnt, pub, keyType)
}

// VerifySignatureOver is [VerifySignature] over any signed entity.
//
// **The V7 §5.2 two-hop check does not vary by what is signed.** A
// published-root and a `system/registry/binding` are verified by the
// identical five steps — type, decode, target-matches-the-hash-we-
// recomputed, algorithm agrees with the key type, and the crypto — and
// the second consumer of those steps arrived the moment
// `fetch/registry.go` had to verify a registry's signature over a name
// binding (REGISTRY §6a.4 step 3).
//
// Generalizing here rather than copying there is the same call this
// package was created to make: two copies of one gate agree until they
// do not, and the divergence surfaces as one consumer accepting bytes
// the other refuses. `what` is a label for the diagnostics only — it
// changes no check.
func VerifySignatureOver(what, where string, signed, sigEnt entity.Entity, pub []byte, keyType byte) (types.SignatureData, error) {
	if sigEnt.Type != types.TypeSignature {
		return types.SignatureData{}, newFault(500, "unexpected_result_type",
			fmt.Sprintf("%s holds type %q, want %s", where, sigEnt.Type, types.TypeSignature))
	}
	sig, err := types.SignatureDataFromEntity(sigEnt)
	if err != nil {
		return types.SignatureData{}, wrapFault(500, "decode_failed",
			"decode "+what+" signature", err)
	}
	if sig.Target != signed.ContentHash {
		return types.SignatureData{}, newFault(403, "signature_target_mismatch",
			fmt.Sprintf("signature at %s targets %s, not %s", where, sig.Target, signed.ContentHash))
	}
	sigKeyType, ok := crypto.KeyTypeByte(sig.Algorithm)
	if !ok {
		return types.SignatureData{}, newFault(501, "unsupported_algorithm",
			fmt.Sprintf("signature algorithm %q is not a known key type", sig.Algorithm))
	}
	if sigKeyType != keyType {
		return types.SignatureData{}, newFault(403, "signature_algorithm_mismatch",
			fmt.Sprintf("signature algorithm %q does not match the %s signer's key type 0x%02x",
				sig.Algorithm, what, keyType))
	}
	if !crypto.Verify(sigKeyType, pub, signed.ContentHash.Bytes(), sig.Signature) {
		return types.SignatureData{}, newFault(403, "signature_invalid",
			what+" signature does not verify against the signer's key")
	}
	return sig, nil
}
