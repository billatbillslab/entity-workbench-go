package entitysdk

import (
	"fmt"
	"time"

	cbor "github.com/fxamacker/cbor/v2"
	"golang.org/x/text/unicode/norm"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// registry_issue.go — **being** a registry: §6a.8 curated registration.
//
// This is the other end of `registry_pin.go`. That file consumes a name
// authority; this one makes this peer into one. §6a.8 is explicit that
// curated registration is **operator tooling and no live protocol** —
// *"a curated/static registry's operator decides what it signs"* — so
// there is no handler here and no request type. It is: build the body,
// sign it with this peer's key, bind three paths.
//
//	system/registry/binding/{hex(binding_hash)}   the body
//	system/registry/binding/by-name/{nfc(name)}   the name → hash index
//	system/signature/{hex(binding_hash)}          the §5.2 signature
//
// Publishing `-prefix system/registry/` afterwards emits a **static
// registry** — §7.4's *"the registry is itself a coral reef"*, literally,
// and byte-compatible with what `fetch.Registry` consumes. The loop
// closes: this peer can mint a name, publish it, and another peer holding
// only its peer-id can browse and resolve it.
//
// # The `transports` shape is a decision, and it is the operator's
//
// EXTENSION-REGISTRY §3 declares `transports: [<endpoint per NETWORK
// §6.5>]`, and the cohort reads that two incompatible ways —
// `entity-core-rust` as an inline endpoint object, `entity-core-go` as a
// bare `system/hash`. **Neither is a misreading and the ruling is open**
// (`reviews/REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21.md`).
//
// An emitter cannot abstain — the field holds one shape — so this one
// writes **inline** and says so on every result
// ([IssuedBinding.TransportShape]), because inline is the shape the only
// live federation uses and because it is **self-contained**, which a
// name→reach statement has to be.
//
// The by-hash shape is **refused rather than emitted**, and the reason is
// not preference: a bare hash names an entity a consumer must fetch, and
// §3 does not say *where a registry serves it*. Choosing a path would be
// inventing a convention no consumer could be expected to know (AP20).
// The refusal carries that sentence, so an operator who wants the other
// shape learns what is actually missing.

// IssueOpts describes one binding to mint.
type IssueOpts struct {
	// Name is the user-facing name. NFC-normalized and §6.3
	// name-path-safety checked here; the normalized form is what the
	// body carries and what the by-name path embeds, because §6a.4's
	// association check compares the two.
	Name string

	// TargetPeerID is the Base58 peer-id this name resolves to (§3: an
	// identity, never a content-hash).
	TargetPeerID string

	// Transports is how the target is reached. **Required**: §6a.3 makes
	// a non-empty `transports` a MUST on a peer-issued binding, because
	// §4.1.2's "transport resolution finds reachable endpoints" has no
	// static counterpart — a consumer that resolves a peer-id and stops
	// has learned a fact it cannot act on.
	//
	// The natural source is the target's own advertised
	// `transport-profile`: a registry asserting a reach it read from the
	// target is asserting something it checked, and one asserting a
	// reach it composed is asserting something it invented.
	Transports []types.HTTPPollProfileData

	// TransportsByHash asks for the bare-`system/hash` reading of §3
	// instead of the inline one. **It is currently REFUSED**, with the
	// reason on the error: §3 does not say where a registry serves the
	// entity such a hash names, so emitting one means inventing a
	// serving convention (AP20). The flag exists so the refusal has a
	// place to be stated rather than being an unexplained absence, and
	// so the day it is ruled there is one line to change.
	TransportsByHash bool

	// TTL is how long the binding is valid. **Zero is refused**, not
	// defaulted to null: §6a.3 makes a finite ttl a MUST on this kind
	// because the expiry is the only bound on a revocation a hostile
	// origin withholds, and a null one makes the binding permanently
	// unrevokable.
	TTL time.Duration

	// At pins `issued_at`. Zero means now. Present for the reason
	// `publish.Opts.At` is (AP18): a fresh clock per run makes an
	// emission unfreezable, because the body hash — and therefore the
	// signature path and the by-name target — moves on every issue.
	At time.Time

	// Supersedes names the binding this one replaces (§3, the
	// ATTESTATION supersedes chain). Nil for a first issue.
	Supersedes *hash.Hash
}

// IssuedBinding is what was minted and where it was bound.
type IssuedBinding struct {
	Name        string
	BindingHash hash.Hash
	Signature   hash.Hash
	IssuedAt    time.Time
	ExpiresAt   time.Time

	BodyPath   string
	ByNamePath string
	SigPath    string

	// TransportShape is which reading of §3 this binding was written
	// under — always "inline" today. Surfaced, not buried: it decides
	// which implementations can read the result, and it will stop being
	// a constant the day the divergence rules.
	TransportShape string
	// ProfileHashes is reserved for the by-hash shape and is empty today.
	ProfileHashes []hash.Hash
}

// IssueBinding mints and binds one peer-issued name binding, signed by
// this peer.
//
// The peer doing the issuing **is** the registry: its peer-id is the
// `backend_id` a consumer pins, and the key that signs here is the key
// that pin derives. There is no separate registry identity and there
// should not be — §6a.2 makes the registry peer-id the trust root.
func (a *AppPeer) IssueBinding(opts IssueOpts) (IssuedBinding, error) {
	normalized, err := normalizeRegistryName(opts.Name)
	if err != nil {
		return IssuedBinding{}, err
	}
	if opts.TargetPeerID == "" {
		return IssuedBinding{}, NewError(400, "missing_target",
			"a binding needs a target peer-id — the whole content of the assertion")
	}
	if _, _, ok := crypto.DerivePeerFromPeerID(crypto.PeerID(opts.TargetPeerID)); !ok {
		// Not fatal to the protocol, but a target whose key is not
		// derivable cannot have its content signature checked by a
		// consumer holding only this binding, which makes the name a
		// dead end at the next hop.
		return IssuedBinding{}, NewError(400, "unverifiable_target",
			fmt.Sprintf("target %s is not identity-multihash form, so a consumer following this "+
				"name could not verify anything it publishes", opts.TargetPeerID))
	}
	if len(opts.Transports) == 0 {
		return IssuedBinding{}, NewError(400, "missing_transports",
			"§6a.3 makes a non-empty `transports` a MUST on a peer-issued binding: a statically-"+
				"published target has no profile to discover (NETWORK §6.5.4), so a consumer that "+
				"resolves the peer-id and stops has learned a fact it cannot act on")
	}
	if opts.TTL <= 0 {
		return IssuedBinding{}, NewError(400, "missing_ttl",
			"§6a.3 makes a finite `ttl` a MUST on a peer-issued binding. It is not a default we "+
				"are declining to pick: the expiry is the ONLY bound on a revocation a hostile "+
				"origin withholds, so a null ttl does not weaken revocation, it removes it")
	}

	at := opts.At
	if at.IsZero() {
		at = time.Now()
	}
	ttlMillis := uint64(opts.TTL / time.Millisecond)

	body := types.BindingData{
		Name:         normalized,
		Kind:         types.BackendKindPeerIssued,
		TargetPeerID: opts.TargetPeerID,
		IssuedAt:     uint64(at.UnixMilli()),
		TTL:          &ttlMillis,
		Supersedes:   opts.Supersedes,
	}

	if opts.TransportsByHash {
		// **Refused, and the refusal is the finding.**
		//
		// The by-hash reading of §3 is coherent right up to the question
		// it does not answer: a bare hash names a transport-profile
		// entity the consumer must then fetch, and **nothing in §3 or
		// §6a says the registry serves it, or at what tree path.** Our
		// own consumer looks for it in the registry's content store,
		// which is a guess; a different consumer will guess differently,
		// and the two disagree as a 404 — indistinguishable at the
		// consumer from a withholding origin.
		//
		// Emitting it anyway would mean inventing a serving path, which
		// is AP20's shape exactly: a constant whose referent exists in
		// no document. So this emitter does not cast that vote. The
		// question is §2 of
		// `reviews/REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21.md`,
		// and one added sentence in §3 unblocks it.
		return IssuedBinding{}, NewError(501, "transports_by_hash_unspecified",
			"the by-hash `transports` shape is not emitted here: §3 does not say where a registry "+
				"serves the transport-profile entity a binding names, so any path we chose would "+
				"be a convention no consumer could be expected to know. Routed as "+
				"REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21 §2; issue inline until it rules")
	}
	shape := "inline"
	var profileHashes []hash.Hash

	bodyEnt, err := encodeBinding(body, opts.Transports, false)
	if err != nil {
		return IssuedBinding{}, err
	}

	kp := a.RawPeer().Keypair()
	sigEnt, err := types.SignatureData{
		Target:    bodyEnt.ContentHash,
		Signer:    a.RawPeer().Identity().ContentHash,
		Algorithm: crypto.KeyTypeString(kp.KeyType),
		Signature: kp.Sign(bodyEnt.ContentHash.Bytes()),
	}.ToEntity()
	if err != nil {
		return IssuedBinding{}, WrapError(500, "encode_signature", "encode binding signature", err)
	}

	out := IssuedBinding{
		Name:        normalized,
		BindingHash: bodyEnt.ContentHash,
		Signature:   sigEnt.ContentHash,
		IssuedAt:    at.UTC(),
		ExpiresAt:   at.Add(opts.TTL).UTC(),

		BodyPath:   a.registryPath(types.BindingStoragePath(bodyEnt.ContentHash)),
		ByNamePath: a.registryPath(types.PeerIssuedByNamePath(normalized)),
		SigPath:    a.registryPath(types.LocalSignaturePath(bodyEnt.ContentHash)),

		TransportShape: shape,
		ProfileHashes:  profileHashes,
	}

	// **Order matters and it is the publishing order, not a preference.**
	// Body and signature first, the by-name pointer last: a consumer that
	// reads the index between the two writes would find a binding whose
	// signature is not yet served, which is indistinguishable at the
	// consumer from a registry serving an unsigned binding. Writing the
	// index last makes the intermediate state "this name does not
	// resolve yet", which is honest.
	if _, err := a.PutEntity(out.BodyPath, bodyEnt); err != nil {
		return IssuedBinding{}, err
	}
	if _, err := a.PutEntity(out.SigPath, sigEnt); err != nil {
		return IssuedBinding{}, err
	}
	if _, err := a.PutEntity(out.ByNamePath, bodyEnt); err != nil {
		return IssuedBinding{}, err
	}
	return out, nil
}

// encodeBinding builds the body entity, splicing inline transports in
// when that is the chosen shape.
//
// `types.BindingData` types `Transports` as `[]hash.Hash`, so the inline
// shape cannot go through it — the struct cannot represent the other
// reading of its own spec sentence. Rather than fork the type we encode
// the map form directly, which keeps every other field's encoding
// exactly core-go's.
func encodeBinding(body types.BindingData, profiles []types.HTTPPollProfileData, byHash bool) (entity.Entity, error) {
	if byHash {
		ent, err := body.ToEntity()
		if err != nil {
			return entity.Entity{}, WrapError(500, "encode_binding", "encode registry binding", err)
		}
		return ent, nil
	}

	// The inline shape. Field names and order are §3's; only
	// `transports` differs from what BindingData would emit.
	m := map[string]any{
		"name":           body.Name,
		"kind":           body.Kind,
		"target_peer_id": body.TargetPeerID,
		"transports":     profiles,
		"issued_at":      body.IssuedAt,
	}
	if body.TTL != nil {
		m["ttl"] = *body.TTL
	}
	if body.Supersedes != nil {
		m["supersedes"] = *body.Supersedes
	}
	// **`ecf.Encode`, not `cbor.Marshal`.** ECF pins a canonical
	// encoding — deterministic map-key ordering above all — and the
	// content hash is computed over these exact bytes. A default
	// `cbor.Marshal` would produce a self-consistent entity whose hash
	// no other implementation reproduces for the same logical body,
	// which is a reproducibility break that nothing local would notice:
	// our own verification passes, because it hashes what we wrote.
	raw, err := ecf.Encode(m)
	if err != nil {
		return entity.Entity{}, WrapError(500, "encode_binding", "encode inline-transport binding", err)
	}
	ent, err := entity.NewEntity(types.TypeRegistryBinding, cbor.RawMessage(raw))
	if err != nil {
		return entity.Entity{}, WrapError(500, "encode_binding", "build registry binding entity", err)
	}
	return ent, nil
}

// RevokeBinding publishes a §6a.6 revocation targeting a binding.
//
// **Publishing a revocation is not the same as revoking**, and the gap
// is the reason §6a.3 requires a TTL. A revocation is an artifact the
// origin can withhold indefinitely, and a withheld revocation is
// byte-identical at a consumer to one that was never issued (§6a.1a). So
// this bounds nothing on its own: what bounds a compromised binding is
// its expiry, and this shortens the window for consumers whose origin is
// honest.
func (a *AppPeer) RevokeBinding(bindingHash hash.Hash, reason string, at time.Time) (hash.Hash, error) {
	if at.IsZero() {
		at = time.Now()
	}
	rev := types.RevocationData{
		Revokes:   bindingHash,
		RevokedAt: uint64(at.UnixMilli()),
	}
	if reason != "" {
		rev.Reason = &reason
	}
	revEnt, err := rev.ToEntity()
	if err != nil {
		return hash.Hash{}, WrapError(500, "encode_revocation", "encode revocation", err)
	}
	kp := a.RawPeer().Keypair()
	sigEnt, err := types.SignatureData{
		Target:    revEnt.ContentHash,
		Signer:    a.RawPeer().Identity().ContentHash,
		Algorithm: crypto.KeyTypeString(kp.KeyType),
		Signature: kp.Sign(revEnt.ContentHash.Bytes()),
	}.ToEntity()
	if err != nil {
		return hash.Hash{}, WrapError(500, "encode_signature", "encode revocation signature", err)
	}

	// The signature first, then the index — same reason as the issue
	// path, mirrored: a revocation whose signature is not yet served is
	// one a conformant consumer MUST ignore (§6a.6 verifies before it
	// believes), so the intermediate state must not be "revoked".
	if _, err := a.PutEntity(a.registryPath(types.LocalSignaturePath(revEnt.ContentHash)), sigEnt); err != nil {
		return hash.Hash{}, err
	}
	idx := a.registryPath(types.PeerIssuedRevocationByTargetPath(bindingHash))
	if _, err := a.PutEntity(idx, revEnt); err != nil {
		return hash.Hash{}, err
	}
	return revEnt.ContentHash, nil
}

// registryPath qualifies a peer-relative registry path with this peer's
// namespace, which is the form every store-side surface here expects.
func (a *AppPeer) registryPath(rel string) string {
	return "/" + a.PeerID() + "/" + rel
}

// normalizeRegistryName applies NFC + §6.3 name-path safety at ISSUE
// time, which is where the spec puts it.
//
// Doing it here rather than only at resolve is what makes the §6a.4
// association check meaningful: the body carries the normalized name and
// the by-name path embeds the same string, so the pairing a consumer
// compares is the pairing that was signed.
func normalizeRegistryName(name string) (string, error) {
	if name == "" {
		return "", NewError(400, "invalid_name", "a binding needs a name")
	}
	if !norm.NFC.IsNormalString(name) {
		name = norm.NFC.String(name)
	}
	for _, r := range name {
		if r == '/' {
			return "", NewError(400, "invalid_name",
				fmt.Sprintf("name %q contains '/', forbidden by REGISTRY §6.3 — a name is one path segment", name))
		}
		if r <= 0x0020 || r == 0x007F {
			return "", NewError(400, "invalid_name",
				fmt.Sprintf("name %q contains control character U+%04X", name, r))
		}
	}
	return name, nil
}
