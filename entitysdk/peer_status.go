// The liveness read-model — the consumer half of EXTENSION-NETWORK
// §3.13 / §5.4's `system/peer/status`.
//
// What this replaces, and why it is not the same thing.
// AppPeer.ConnectedPeers() reports the local connection POOL: peers we
// currently hold a socket to. That is a snapshot of our own plumbing,
// not the peer's lifecycle. It cannot say "suspect", cannot say why a
// peer went away, cannot be subscribed to, and disagrees with the tree
// the moment a connection is evicted without a demotion (the §10.2
// dispatch-fallback and the RELAY terminal hop both evict WITHOUT
// demoting, deliberately). `entity-browser-rust` built and then deleted
// exactly this shape on their arm — their `connection_health` — and
// named the pool snapshot as the thing to replace.
//
// The tree entity is the sanctioned home for the whole cohort: the
// session entity dropped its own status/last_active fields precisely
// because they duplicated it. Reading it here means our liveness view
// is the same one a remote consumer, a continuation, or the reconnect
// graph sees.
//
// Three properties of the source worth carrying at the call site,
// because a reader that assumes otherwise is wrong in a way tests do
// not catch:
//
//  1. **It is transition-written, not a heartbeat** (§5.4.1 MUST). A
//     successful keepalive tick writes nothing. `LastSeen` is a
//     snapshot taken AT the transition — "last heard as of this
//     transition" — and is never cadence-refreshed. Treating a stale
//     LastSeen as evidence of a dead peer is a misread of the field.
//  2. **The freshness contract is `connected` plus the demotion
//     envelope**, not recency. §5.4.1 defers a cadence-visible
//     heartbeat surface deliberately.
//  3. **Absence is not "disconnected"** — it is "no transition has ever
//     been written for this peer," which is a different claim. Reported
//     as `found == false` rather than folded into a status string.
package entitysdk

import (
	"fmt"
	"sort"
	"strings"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/types"
)

// peerStatusPrefix is the bare (peer-relative) prefix every status
// entity lives under. The canonical path is
// /{local_peer}/system/peer/status/{remote_identity_hash_hex} — the
// hex segment being the same non-root path-position convention the
// transport profiles use, NOT the Base58 id.
const peerStatusPrefix = types.TypePeerStatus + "/"

// The §3.13 lifecycle enum, re-exported so app code reads liveness
// without importing core-go's types package. Three states, and that is
// the whole set: "reconnecting" is NOT one of them — it belongs to the
// derived system/network peer-summary, and a renderer that invents it
// here is describing a state the tree cannot hold.
const (
	PeerStatusConnected    = types.PeerStatusConnected
	PeerStatusSuspect      = types.PeerStatusSuspect
	PeerStatusDisconnected = types.PeerStatusDisconnected
)

// PeerLiveness is one remote peer's lifecycle state as the tree holds
// it. A renderer-neutral value: no entity, no hash, nothing that ties a
// consumer to the store layout.
type PeerLiveness struct {
	// PeerID is the remote peer's Base58 id, read from the entity's
	// own `peer_id` field rather than reversed out of the path
	// segment — the path carries a hash, which does not invert.
	PeerID string
	// Status is one of "connected" / "suspect" / "disconnected".
	// The §3.13 enum is three-state; "reconnecting" is NOT a value
	// here (it belongs to the derived system/network peer-summary).
	Status string
	// Reason is the OPTIONAL transition reason (keepalive-miss,
	// transport-error, auth-rejected, peer-shutdown, peer-idle,
	// peer-migration, local-release, retry-exhausted). An unrecognized
	// value is treated as generic per MUST-ignore-unknowns — do not
	// switch on it exhaustively.
	Reason string
	// LastError is opaque human/log detail. Never parse it.
	LastError string
	// ConnectedAt is ms-since-epoch of handshake completion, when known.
	ConnectedAt uint64
	// LastSeen is ms-since-epoch AT THE TRANSITION — see the package
	// note. Not a heartbeat.
	LastSeen uint64
	// FailingSince is ms-since-epoch of the transition out of
	// `connected` that opened the current failure episode, preserved
	// across escalation within the episode and cleared on recovery.
	// Zero = not currently failing. It is the only durable retry
	// state; attempt counts are derived, never stored.
	FailingSince uint64
}

// Connected reports whether the tree's last transition for this peer
// was to `connected`. Named as a question about the RECORD, not about
// the network — nothing here proves the peer is reachable right now,
// and §5.4.1 is explicit that the status entity is not a heartbeat.
func (l PeerLiveness) Connected() bool { return l.Status == types.PeerStatusConnected }

// Failing reports whether a failure episode is open — suspect, or
// disconnected, or a preserved FailingSince stamp.
func (l PeerLiveness) Failing() bool {
	return l.Status == types.PeerStatusSuspect ||
		l.Status == types.PeerStatusDisconnected ||
		l.FailingSince != 0
}

// PeerLivenessOf returns the recorded lifecycle state for one remote
// peer.
//
// found == false means no transition has ever been written for this
// peer — never contacted, or contacted before this store existed. It
// is deliberately not reported as "disconnected": a peer we have never
// heard of and a peer that told us goodbye are different facts, and
// collapsing them is how a UI shows a red dot for a stranger.
func (a *AppPeer) PeerLivenessOf(remotePeerID string) (PeerLiveness, bool, error) {
	return a.store.PeerLivenessOf(remotePeerID)
}

// PeerLivenessOf is the Store-level (L0) form. The read-model lives
// here rather than only on AppPeer because a status entity is an
// ordinary local binding in our own namespace — no dispatch, no
// capability, nothing an AppPeer adds — and the workbench models that
// render it are constructed with a *Store.
func (s *Store) PeerLivenessOf(remotePeerID string) (PeerLiveness, bool, error) {
	if remotePeerID == "" {
		return PeerLiveness{}, false, NewError(400, "invalid_peer", "PeerLivenessOf: remotePeerID is empty")
	}
	idHash, err := types.ComputePeerIdentityHashFromPeerID(crypto.PeerID(remotePeerID))
	if err != nil {
		return PeerLiveness{}, false, WrapError(400, "invalid_peer",
			fmt.Sprintf("PeerLivenessOf: %q is not a valid peer-id", remotePeerID), err)
	}
	// L0 read of our OWN namespace. Not AppPeer.Get: a status entity
	// under /{us}/… would be answered locally either way, but the
	// dispatched form invites the AP11 mistake the moment someone
	// generalizes this to /{them}/… — where it would silently ask the
	// remote peer for its opinion of itself.
	ent, ok := s.Get(peerStatusPrefix + types.PeerIdentityHashHex(idHash))
	if !ok {
		return PeerLiveness{}, false, nil
	}
	live, err := decodePeerLiveness(ent)
	if err != nil {
		return PeerLiveness{}, false, err
	}
	return live, true, nil
}

// PeerLivenessAll returns every recorded peer lifecycle state, sorted
// by peer-id for a stable render order.
//
// This is a prefix list over a namespace whose size is the number of
// peers we have ever transitioned — not a scan of the tree — so it is
// the one place a full read is the right shape. Consumers that want to
// stay current subscribe with OnPeerLivenessChange rather than polling
// this.
func (a *AppPeer) PeerLivenessAll() ([]PeerLiveness, error) {
	return a.store.PeerLivenessAll()
}

// PeerLivenessAll is the Store-level (L0) form.
func (s *Store) PeerLivenessAll() ([]PeerLiveness, error) {
	entries := s.List(peerStatusPrefix)
	out := make([]PeerLiveness, 0, len(entries))
	for _, e := range entries {
		ent, ok := s.GetByHash(e.Hash)
		if !ok {
			continue
		}
		live, err := decodePeerLiveness(ent)
		if err != nil {
			// One malformed entity must not blind the whole view.
			// Skipping is right; silence is not, so it carries the
			// path.
			s.debugf("peer-liveness: skipping %s: %v", e.Path, err)
			continue
		}
		out = append(out, live)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out, nil
}

// OnPeerLivenessChange subscribes to lifecycle transitions and calls fn
// once per transition, plus once per already-recorded peer at
// subscription time so a consumer starts from a complete picture rather
// than an empty one that fills in.
//
// Prefix-subscribed, never scan-and-filter: the status namespace is
// watched directly, so the cost is per-transition rather than
// O(tree) per render. Cancel stops delivery.
//
// The callback runs on the store's watch goroutine. Do not block it —
// hand off to a channel, which is what the renderer bridge does.
func (a *AppPeer) OnPeerLivenessChange(fn func(PeerLiveness)) (cancel func()) {
	return a.store.OnPeerLivenessChange(fn)
}

// OnPeerLivenessChange is the Store-level (L0) form.
func (s *Store) OnPeerLivenessChange(fn func(PeerLiveness)) (cancel func()) {
	return s.OnPrefixChange(peerStatusPrefix, func(ev ChangeEvent) {
		// A removal carries a zero hash and nothing to decode. Status
		// entities are rebound rather than removed in the lifecycle
		// paths, so this is the unusual case (an operator deleting
		// state, a GC of a superseded entity) and there is no
		// transition to report.
		if ev.EventType == ChangeRemove || ev.NewHash.IsZero() {
			return
		}
		ent, ok := s.GetByHash(ev.NewHash)
		if !ok {
			s.debugf("peer-liveness: change at %s names %s, which is not in the content store", ev.Path, ev.NewHash)
			return
		}
		live, err := decodePeerLiveness(ent)
		if err != nil {
			s.debugf("peer-liveness: undecodable change at %s: %v", ev.Path, err)
			return
		}
		fn(live)
	})
}

// decodePeerLiveness converts a stored entity into the read-model
// value, rejecting anything that is not a status entity rather than
// returning a zero-valued one — a PeerLiveness with an empty Status is
// indistinguishable from "connected: no" at every call site.
func decodePeerLiveness(ent entity.Entity) (PeerLiveness, error) {
	if ent.Type != types.TypePeerStatus {
		return PeerLiveness{}, NewError(500, "unexpected_result_type",
			fmt.Sprintf("peer-liveness: entity type %q, want %s", ent.Type, types.TypePeerStatus))
	}
	d, err := types.PeerStatusDataFromEntity(ent)
	if err != nil {
		return PeerLiveness{}, WrapError(500, "decode_failed", "decode peer status", err)
	}
	if strings.TrimSpace(d.Status) == "" {
		return PeerLiveness{}, NewError(500, "invalid_status",
			"peer-liveness: entity carries no status")
	}
	return PeerLiveness{
		PeerID:       d.PeerID,
		Status:       d.Status,
		Reason:       d.Reason,
		LastError:    d.LastError,
		ConnectedAt:  d.ConnectedAt,
		LastSeen:     d.LastSeen,
		FailingSince: d.FailingSince,
	}, nil
}

// debugf routes read-model diagnostics to the store's event log when
// one is wired. Skipping a malformed entry is the right behaviour —
// one bad entity must not blind the whole liveness view — but doing it
// silently is not, so the path always gets said out loud somewhere.
func (s *Store) debugf(format string, args ...interface{}) {
	if s.log != nil {
		s.log.Debugf(format, args...)
	}
}
