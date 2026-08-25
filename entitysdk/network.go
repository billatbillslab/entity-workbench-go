package entitysdk

import (
	"context"

	"go.entitychurch.org/entity-core-go/core/types"
)

// NetworkClient wraps the system/network operations — maintain-peer,
// release-peer, status, close — behind typed Go methods
// (EXTENSION-NETWORK §2, §3.1). One client targets one peer, local or
// remote, chosen at construction.
//
// The division of labour is worth holding in mind, because it decides
// which surface answers a given question:
//
//   - **core/peer owns the imperative half.** It writes `connected` at
//     handshake, `suspect` at the dispatch seam, `disconnected` on
//     keepalive miss — whether or not anything below is ever called.
//     Read that with AppPeer.PeerLivenessOf / PeerLivenessAll /
//     OnPeerLivenessChange.
//   - **this handler owns the reactive half.** MaintainPeer installs
//     the §4.1 continuation graph that watches those writes and dials
//     back. Nothing reconnects on its own until you ask for it.
//
// So MaintainPeer is not "connect" and does not replace AppPeer.Connect
// — it is the standing instruction to KEEP a relationship, across
// drops, with backoff and subscription restoration.
type NetworkClient struct {
	ap         *AppPeer
	target     string
	networkURI string
}

// Network returns a NetworkClient targeting the local peer.
func (a *AppPeer) Network() *NetworkClient {
	return &NetworkClient{
		ap:         a,
		target:     a.PeerID(),
		networkURI: "system/network",
	}
}

// NetworkAt returns a NetworkClient targeting a remote peer's network
// handler, dispatched through the local peer's connection pool.
func (a *AppPeer) NetworkAt(peerID string) *NetworkClient {
	return &NetworkClient{
		ap:         a,
		target:     peerID,
		networkURI: extPeerURI(a.PeerID(), peerID, "system/network"),
	}
}

// PeerID returns the peer-id this client targets.
func (nc *NetworkClient) PeerID() string { return nc.target }

// MaintainOpts are the optional knobs on MaintainPeer. The zero value
// is the §2.1 default: reconnect on, resubscribe on, default keepalive
// and backoff.
//
// Reconnect and Resubscribe are pointers because absent and explicitly
// false are different requests — the spec default is true, so a plain
// bool could not express "no, really, do not reconnect."
type MaintainOpts struct {
	// Address is the initial dial address ("host:port"). Optional when
	// the remote's transport profile is already resolvable — which is
	// what AdvertiseTransport on the other side is for.
	Address string
	// Reconnect enables auto-reconnect on disconnect. nil = default (on).
	Reconnect *bool
	// Resubscribe restores subscriptions after a reconnect. nil = default (on).
	Resubscribe *bool
	// Keepalive overrides the §2.3 keepalive defaults.
	Keepalive *types.KeepaliveConfigData
	// Backoff overrides the §2.2 reconnection backoff defaults.
	Backoff *types.BackoffConfigData
}

// MaintainPeer asks the network handler to keep a relationship with
// remotePeerID: dial it if needed, watch its lifecycle, and reconnect
// with backoff when it drops.
//
// Returns the §2.4 result — the session id, the lifecycle subscription
// ids, and the chain id the continuation graph runs under. Hold onto
// the chain id if you want to read chain-error markers for a reconnect
// that kept failing; it is the coordinate they are filed under.
func (nc *NetworkClient) MaintainPeer(ctx context.Context, remotePeerID string, opts MaintainOpts) (types.MaintainResultData, error) {
	if remotePeerID == "" {
		return types.MaintainResultData{}, NewError(400, "invalid_peer", "MaintainPeer: remotePeerID is empty")
	}
	req := types.MaintainRequestData{
		PeerID:      remotePeerID,
		Address:     opts.Address,
		Reconnect:   opts.Reconnect,
		Resubscribe: opts.Resubscribe,
		Keepalive:   opts.Keepalive,
		Backoff:     opts.Backoff,
	}
	paramEnt, err := encodeAsEntity(types.TypeNetworkMaintainRequest, req)
	if err != nil {
		return types.MaintainResultData{}, err
	}
	resultEnt, err := extDispatch(nc.ap, nc.networkURI, "maintain-peer", "", paramEnt)
	if err != nil {
		return types.MaintainResultData{}, err
	}
	res, err := types.MaintainResultDataFromEntity(resultEnt)
	if err != nil {
		return types.MaintainResultData{}, WrapError(500, "decode_result", "decode MaintainResult", err)
	}
	return res, nil
}

// ReleasePeer ends the maintenance relationship: the continuation graph
// comes down, the lifecycle subscriptions are removed, and no further
// reconnect is attempted. reason is "shutdown" (the default when
// empty), "idle", or "migration".
//
// This is the deliberate end of a relationship, which is why the
// resulting status write carries `local-release` — a terminal reason a
// consumer must not treat as something to back off and retry.
func (nc *NetworkClient) ReleasePeer(ctx context.Context, remotePeerID, reason string) error {
	if remotePeerID == "" {
		return NewError(400, "invalid_peer", "ReleasePeer: remotePeerID is empty")
	}
	req := types.ReleaseRequestData{PeerID: remotePeerID, Reason: reason}
	paramEnt, err := encodeAsEntity(types.TypeNetworkReleaseRequest, req)
	if err != nil {
		return err
	}
	if _, err := extDispatch(nc.ap, nc.networkURI, "release-peer", "", paramEnt); err != nil {
		return err
	}
	return nil
}

// Status returns the §2.7 status view.
//
// **`MaintainedPeers` is not the maintained set, despite the name.**
// §4.3's own pseudocode enumerates every entity under
// `system/peer/status/` — so the rows include peers that were released,
// and peers that were only ever dialed and never maintained at all. The
// discriminator is `SessionID`: populated for a live maintenance
// session, empty otherwise. Measured against a real handler, not
// inferred; the naming gap is routed to arch.
//
// Use MaintainedPeers below when the question is "what am I actually
// maintaining", and PeerLivenessAll when it is "what do I know about
// who is up". Reading the raw rows as the maintained set is how a UI
// shows a reconnect that nothing is driving.
//
// The §2.8 summary can carry a `reconnecting` status that the tree's
// three-state §3.13 enum does not — it is derived at this surface, not
// stored.
func (nc *NetworkClient) Status(ctx context.Context) (types.NetworkStatusData, error) {
	resultEnt, err := extDispatch(nc.ap, nc.networkURI, "status", "", emptyParamsEntity())
	if err != nil {
		return types.NetworkStatusData{}, err
	}
	st, err := types.NetworkStatusDataFromEntity(resultEnt)
	if err != nil {
		return types.NetworkStatusData{}, WrapError(500, "decode_result", "decode NetworkStatus", err)
	}
	return st, nil
}

// MaintainedPeers returns only the rows with a live maintenance
// session — the set the §2.7 field name promises and does not deliver.
//
// It is a filter rather than a second op because the discriminator is
// already on the wire: a row with no session_id is a peer nothing is
// reconnecting.
func (nc *NetworkClient) MaintainedPeers(ctx context.Context) ([]types.NetworkPeerSummaryData, error) {
	st, err := nc.Status(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]types.NetworkPeerSummaryData, 0, len(st.MaintainedPeers))
	for _, s := range st.MaintainedPeers {
		if s.SessionID != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// Close tears down the connection to remotePeerID without ending the
// maintenance relationship. reason is "shutdown", "idle", "error", or
// "migration" (§9.1) and rides through to the status write, where
// `idle` / `migration` tell the far side to preserve subscriptions and
// expect a resume.
func (nc *NetworkClient) Close(ctx context.Context, remotePeerID, reason string) error {
	if remotePeerID == "" {
		return NewError(400, "invalid_peer", "Close: remotePeerID is empty")
	}
	req := types.CloseRequestData{PeerID: remotePeerID, Reason: reason}
	paramEnt, err := encodeAsEntity(types.TypeNetworkCloseRequest, req)
	if err != nil {
		return err
	}
	if _, err := extDispatch(nc.ap, nc.networkURI, "close", "", paramEnt); err != nil {
		return err
	}
	return nil
}
