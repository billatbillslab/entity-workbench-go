package entitysdk

import (
	"fmt"
	"time"

	"go.entitychurch.org/entity-core-go/core/capability"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// MintCrossPeerChainCapability mints a dispatch capability suitable as
// the `dispatch_capability` on a continuation step whose target is a
// REMOTE peer (EXTENSION-CONTINUATION §4.2 case 3 / §8.2 C-3 — the
// re-attenuation mint helper).
//
// The minted authority chain shape:
//
//	leaf (granter=local, grantee=local, parent=connCap)
//	  └─ connCap (granter=B, grantee=local, parent=nil)   ← B-rooted root
//
// where `B` is the remote peer identified by `remotePeerID` and `connCap`
// is the connection grant B conferred during the connect handshake (held
// on the pooled `*Connection`'s Session.Capability). For the workbench's
// typical case the installer (caller) and the dispatching host peer
// (EXECUTE author) are the same identity — the local peer — so the
// helper uses `localID` for both the in-chain leaf granter and the
// grantee.
//
// Why this shape (file:line, the load-bearing spec citations):
//   - **B-rooted, not installer-rooted.** Chain must root at B's
//     conferred authority so B's advance-time `VerifyChain` succeeds.
//     An installer-rooted chain is the local-sufficient form that
//     fails cross-peer; that was the v1.9 / pre-Amendment-4 collapse.
//   - **Installer in-chain.** The §3.1a / §3.2-step-4 install-time
//     in-chain check requires the writer (installer) to appear as a
//     granter ANYWHERE in the chain — not just at root. Mint as
//     re-attenuation: installer is the leaf granter, in-chain trivially.
//   - **Grantee = dispatching host peer (EXECUTE author).** §4.2 case 3
//     (iii). Self-wielding to the installer is the v1.9 gap B closes
//     with `grantee != author`. For workbench-go this is moot — both
//     are the local peer — but the helper's API forces the right
//     identity in both slots so callers don't drift.
//
// Persistence:
//   - The minted cap entity + its detached signature are written to the
//     local content store via `ContentStore.Put` (content-addressed).
//   - The signature is additionally bound at the V7 §3.5 v7.44 invariant
//     pointer path `/{localPeerID}/system/signature/{capHash}` so the
//     dispatch chain-bundle helper (`BundleCrossPeerChain`) can resolve
//     it the same way `envelope_ingest` resolves B's signature on the
//     connection grant. Without this, the bundle would be missing our
//     leaf's signature and B's `VerifyChain` would fail-closed on the
//     leaf link.
//
// Returns the leaf cap entity. Caller obtains the full chain + signature
// bundle via `BundleCrossPeerChain(leaf)` and bundles the result into
// the dispatched EXECUTE envelope's `included` per §4.3.
func (a *AppPeer) MintCrossPeerChainCapability(remotePeerID string, grants []types.GrantEntry, expiresAt *uint64) (entity.Entity, error) {
	if remotePeerID == "" {
		return entity.Entity{}, NewError(400, "invalid_peer_id",
			"MintCrossPeerChainCapability requires remotePeerID")
	}
	if len(grants) == 0 {
		return entity.Entity{}, NewError(400, "invalid_grants",
			"MintCrossPeerChainCapability requires at least one grant entry")
	}

	parentCap, ok := a.findConnectionCapability(remotePeerID)
	if !ok || parentCap == nil {
		return entity.Entity{}, NewError(404, "no_connection_grant",
			fmt.Sprintf("no connection-grant capability on file for remote peer %s — Connect first", remotePeerID))
	}
	parent := *parentCap

	// Core-go now runs envelope-signature ingest at the
	// end of `PerformConnect`, so the parent cap's signature + granter
	// identity are already in the local content store + invariant
	// pointer-path index by the time we get here. We additionally Put
	// the parent cap entity itself defensively — envelope ingest only
	// persists signatures + identity entities, and `CollectChainBundle`
	// needs the cap entity reachable from the store too.
	cs := a.peer.Store()
	if _, ok := cs.Get(parent.ContentHash); !ok {
		if _, err := cs.Put(parent); err != nil {
			return entity.Entity{}, WrapError(500, "persist_parent_cap",
				"persist connection-grant capability", err)
		}
	}

	localID := a.peer.Identity()
	localKP := a.peer.Keypair()

	// V7 v7.73 §PR-8: a cap's resource patterns canonicalize against ITS
	// OWN granter's peer_id. The leaf this helper mints has granter = local
	// peer, so a bare `*` resource canonicalizes to `/{localPeerID}/*` — the
	// local peer's namespace — not the remote peer's. Bare `*` is unambiguous
	// here (this helper's sole purpose is minting a cap for dispatch to
	// `remotePeerID`, so any caller passing bare `*` means "grant scope on the
	// remote peer's namespace"), so rewrite it to `/{remotePeerID}/*` —
	// semantic preservation of the pre-§PR-8 behavior the localPeerID
	// fallback at the receiver used to give. Callers wanting cross-peer
	// wildcard pass `/*/*` explicitly; callers wanting another absolute
	// scope pass it explicitly. Only the exact bare `*` is rewritten.
	grants = canonicalizeCrossPeerLeafResources(grants, remotePeerID)

	capEnt, sigEnt, err := capability.MintReattenuated(
		localKP,
		localID,
		localID.ContentHash, // grantee = dispatching host peer; same identity as installer locally
		parent,
		grants,
		uint64(time.Now().UnixMilli()),
		expiresAt,
	)
	if err != nil {
		return entity.Entity{}, WrapError(400, "mint_failed", "MintReattenuated", err)
	}

	if _, err := cs.Put(capEnt); err != nil {
		return entity.Entity{}, WrapError(500, "persist_cap",
			"persist re-attenuated capability", err)
	}
	if _, err := cs.Put(sigEnt); err != nil {
		return entity.Entity{}, WrapError(500, "persist_signature",
			"persist re-attenuated cap signature", err)
	}

	// Bind our own signature at the invariant pointer path so the
	// bundle helper finds it. Mirrors `envelope_ingest` for incoming
	// signatures. Under v7.65 Amendment 1 peer_id is no longer in the
	// PeerData payload — we use the local keypair's Base58 PeerID
	// directly (the canonical path-segment form per V7 §1.5).
	li := a.peer.LocationIndex()
	sigPath := types.InvariantSignaturePath(string(localKP.PeerID()), capEnt.ContentHash)
	if existing, present := li.Get(sigPath); present {
		// Idempotent: re-minting yields a different cap (CreatedAt differs)
		// so the path should not collide; if it does, the content store
		// already has this exact hash and we keep moving.
		if existing != sigEnt.ContentHash {
			return entity.Entity{}, NewError(500, "signature_path_conflict",
				fmt.Sprintf("invariant signature path %s already bound to a different hash", sigPath))
		}
	} else if err := li.Set(sigPath, sigEnt.ContentHash); err != nil {
		return entity.Entity{}, WrapError(500, "bind_signature_invariant",
			fmt.Sprintf("bind invariant signature path %s", sigPath), err)
	}

	return capEnt, nil
}

// canonicalizeCrossPeerLeafResources rewrites exact bare-`*` entries in
// each grant's Resources.Include to `/{remotePeerID}/*`. See the §PR-8
// rationale on the call site in MintCrossPeerChainCapability.
func canonicalizeCrossPeerLeafResources(grants []types.GrantEntry, remotePeerID string) []types.GrantEntry {
	target := "/" + remotePeerID + "/*"
	out := make([]types.GrantEntry, len(grants))
	for i, g := range grants {
		out[i] = g
		if len(g.Resources.Include) == 0 {
			continue
		}
		rewritten := make([]string, len(g.Resources.Include))
		for j, p := range g.Resources.Include {
			if p == "*" {
				rewritten[j] = target
			} else {
				rewritten[j] = p
			}
		}
		out[i].Resources = types.CapabilityScope{
			Include: rewritten,
			Exclude: g.Resources.Exclude,
		}
	}
	return out
}

// BundleCrossPeerChain assembles the full set of entities the remote
// verifier needs to validate `leafCap`'s authority chain end-to-end —
// every cap from leaf to root, plus each link's granter identity entity
// and the granter's detached signature (resolved from the V7 invariant
// pointer path).
//
// Per EXTENSION-CONTINUATION §4.3, this bundle MUST be in the dispatched
// EXECUTE envelope's `included`. The V7 §3.1/§3.2 general rule only
// carries the leaf cap (referenced from EXECUTE data); the transitive
// chain is referenced from *within* the cap entities and must be
// bundled explicitly.
//
// Over-inclusion is intentional and free: content-addressing dedups any
// entity the verifier already holds. The helper is best-effort per link
// — a link whose signature or identity is not locally resolvable is
// silently omitted; the verifier fails-closed if it actually needed it.
//
// Thin wrapper over `core/capability.CollectChainBundle`.
func (a *AppPeer) BundleCrossPeerChain(leafCap entity.Entity) (map[hash.Hash]entity.Entity, error) {
	return capability.CollectChainBundle(leafCap, a.peer.Store(), a.peer.LocationIndex())
}

// findConnectionCapability returns the B-conferred connection-grant capability
// for the named remote peer, if a live connection exists.
//
// ⚠️ It deliberately returns the CAPABILITY, not the `peer.Session` that carries
// it. `peer.Session` holds `requestSeq atomic.Int64`, which `Connection.Execute`
// increments on EVERY outgoing request — and core-go's own comment on the type
// says the increment must be atomic precisely "because concurrent Execute
// callers race on it before reaching the mutex" (core/peer/connection.go,
// `Session`). This helper used to `return *sess`, copying the session by value:
// a plain, non-atomic read of a counter another goroutine may be mid-`Add` on.
//
// That is a real race, not a lint nit — `go vet` reports it as "return copies
// lock value ... contains sync/atomic.noCopy", which is exactly what noCopy
// exists to say. Reading only the field the caller needs removes the copy and
// the race with it. Do not widen this back to returning a Session: if a future
// caller needs Envelope too, read that field here as well rather than copying
// the struct.
func (a *AppPeer) findConnectionCapability(remotePeerID string) (*entity.Entity, bool) {
	for _, c := range a.peer.Connections() {
		sess := c.Session()
		if sess == nil {
			continue
		}
		if string(sess.RemotePeerID) == remotePeerID {
			return sess.Capability, true
		}
	}
	return nil, false
}
