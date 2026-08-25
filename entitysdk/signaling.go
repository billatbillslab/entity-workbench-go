package entitysdk

import (
	"context"

	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/signaling"
)

// The SIGNALING carrier — EXTENSION-SIGNALING v1.1, the mailbox two peers
// meet at before they can address each other.
//
// **This is the carrier, not discovery.** SIGNALING §1.2: *"the key
// introduces; it never authorizes"* — reaching a rendezvous key proves
// only that someone else derived the same key. Who they are is
// IDENTIFY's answer (EXTENSION-NETWORK) and whether they may do
// anything is DISCOVERY's (§2's grant decision). Nothing in this file
// surfaces a candidate or admits a peer, deliberately.
//
// The `rendezvous` DISCOVERY backend that sits on top is **not built
// yet, and is blocked upstream rather than unscoped**:
// PROPOSAL-DISCOVERY-RENDEZVOUS-BACKEND is RULED (2026-08-17) and its
// §6 fold has not landed — `EXTENSION-DISCOVERY` is still v1.0 with a
// `<"mdns" | "qr" | ...>` backend enum, no `rendezvous` token, and no
// §5.5. That subsection is where the mode split and the TOFU +
// successor requirements live, so building the backend now would mean
// implementing a normative surface from a proposal. Ask routed:
// `docs/architecture/reviews/DISCOVERY-RENDEZVOUS-FOLD-ASK-2026-08-19.md`.
//
// The carrier does not wait on any of that. It is landed spec, the
// kernel ships it (`ext/signaling`), and until now nothing in this
// repo consumed it.

// Rendezvous key modes — EXTENSION-SIGNALING §3.2. Exported because a
// caller picks one, and because the mode string is domain-separated
// INTO the key: two peers using different modes for the same label
// derive different keys and silently never meet.
const (
	// RendezvousModePair — exactly those two peers; both peer-ids are
	// inputs. **Not a discovery mode** (you already hold the
	// counterpart's identity); it is connect-to-a-known-peer.
	RendezvousModePair = "pair"
	// RendezvousModeTag — anyone who knows the tag. A public label,
	// discovery convenience, NOT access control (§3.2).
	RendezvousModeTag = "tag"
	// RendezvousModeSecret — anyone who knows the secret.
	RendezvousModeSecret = "secret"
	// RendezvousModeLobby — anyone on that service, right now.
	RendezvousModeLobby = "lobby"
)

// LobbyDefault is §2.2's named default lobby constant. Both arms must
// use the same one or they never meet, so a default with no name is a
// silent-never-meet bug; this is the kernel's constant, not ours.
const LobbyDefault = signaling.LobbyDefault

// SignalingClient wraps the `system/signaling` operations — offer,
// collect, advertise — behind typed Go methods (EXTENSION-SIGNALING
// §4.1, §4.5). **One client targets one node**, and that is load-bearing
// rather than an implementation detail: §3.4 makes same-provider a MUST,
// so two peers on different nodes derive the same key, deposit into
// different mailboxes, and never meet — with no error on either side.
// The node normally comes from pool selection, not a config constant.
type SignalingClient struct {
	ap      *AppPeer
	node    string
	nodeURI string
}

// Signaling returns a client targeting the signaling node hosted by
// nodePeerID. The local peer-id selects the bare handler path, so a
// peer that hosts its own node is reachable without a connection.
func (a *AppPeer) Signaling(nodePeerID string) *SignalingClient {
	return &SignalingClient{
		ap:      a,
		node:    nodePeerID,
		nodeURI: extPeerURI(a.PeerID(), nodePeerID, signaling.HandlerPattern),
	}
}

// NodePeerID returns the node this client deposits into and reads from.
// Worth logging next to any never-met diagnosis (§3.4).
func (sc *SignalingClient) NodePeerID() string { return sc.node }

// TagKey / SecretKey / LobbyKey / PairKey derive the §2.2 rendezvous key
// for a mode and its input.
//
// **These delegate to the kernel and MUST NOT be reimplemented here.**
// The derivation is a Layer-2 algorithm in this ecosystem's sense: both
// peers have to produce byte-identical key bytes or they derive
// different keys and *silently never meet*, which no same-impl test can
// see. It is also pinned to the SHA-256 floor regardless of the
// deriving peer's home hash format — a detail an independent
// reimplementation gets wrong by doing the obvious thing
// (`entity.NewEntity` hashes under the process-global default).
func TagKey(label string) ([]byte, error) { return signaling.TagKey(label) }

// SecretKey derives the §2.2 key for `secret` mode.
func SecretKey(secret string) ([]byte, error) { return signaling.SecretKey(secret) }

// LobbyKey derives the §2.2 key for `lobby` mode. Pass LobbyDefault
// unless the node's advertise published an override.
func LobbyKey(lobbyConstant string) ([]byte, error) { return signaling.LobbyKey(lobbyConstant) }

// PairKey derives the §2.2 key for `pair` mode from two peer-ids. The
// kernel canonicalizes the ordering, so either peer computes the same
// key from its own point of view.
func PairKey(peerA, peerB string) ([]byte, error) { return signaling.PairKey(peerA, peerB) }

// Offer deposits message at key (§4.1).
//
// Idempotent by content hash (§5 pin 1), so a retry after a timeout is
// safe and a duplicate is indistinguishable from the first. **Peers MUST
// be prepared to re-offer:** the node's TTL is binding (default 60s),
// and a blob that has been reaped is gone whether or not anyone
// collected it.
func (sc *SignalingClient) Offer(ctx context.Context, key, message []byte) error {
	if len(key) == 0 {
		return NewError(400, "invalid_key", "Offer: rendezvous key is empty")
	}
	paramEnt, err := encodeAsEntity(types.TypeSignalingOfferRequest,
		types.OfferRequestData{RendezvousKey: key, Message: message})
	if err != nil {
		return err
	}
	_, err = extDispatch(sc.ap, sc.nodeURI, signaling.OpOffer, "", paramEnt)
	return err
}

// Collect reads the bucket at key non-destructively (§4.1, §4.4),
// oldest-first.
//
// **An unknown key is an empty list and a 200, never a 404** (§4.4, §5
// pin 1) — a mailbox nobody has written to and a mailbox that does not
// exist are the same state, and distinguishing them would leak which
// keys are in use to anyone who can guess one. So an empty result means
// "nobody is standing here *yet*", never an error, and a caller polls.
//
// Collect returns the blobs themselves, not hashes.
func (sc *SignalingClient) Collect(ctx context.Context, key []byte) ([][]byte, error) {
	if len(key) == 0 {
		return nil, NewError(400, "invalid_key", "Collect: rendezvous key is empty")
	}
	paramEnt, err := encodeAsEntity(types.TypeSignalingCollectRequest,
		types.CollectRequestData{RendezvousKey: key})
	if err != nil {
		return nil, err
	}
	resultEnt, err := extDispatch(sc.ap, sc.nodeURI, signaling.OpCollect, "", paramEnt)
	if err != nil {
		return nil, err
	}
	res, err := types.CollectResultDataFromEntity(resultEnt)
	if err != nil {
		return nil, WrapError(500, "decode_result", "decode CollectResult", err)
	}
	return res.Messages, nil
}

// Advertise reads the node's endpoint, limits and reflection endpoints
// (§4.5).
//
// **Read these rather than assuming them.** A limit the client does not
// know is a cross-implementation reject boundary: offer a blob over the
// node's `max_blob_bytes` and the deposit fails at the node, which the
// other arm experiences as a peer that never showed up. The lobby
// override also lives here (`Limits.LobbyConstant`), and using the
// default against a node that overrode it is the same silent never-meet.
func (sc *SignalingClient) Advertise(ctx context.Context) (types.AdvertiseResultData, error) {
	resultEnt, err := extDispatch(sc.ap, sc.nodeURI, signaling.OpAdvertise, "", emptyParamsEntity())
	if err != nil {
		return types.AdvertiseResultData{}, err
	}
	res, err := types.AdvertiseResultDataFromEntity(resultEnt)
	if err != nil {
		return types.AdvertiseResultData{}, WrapError(500, "decode_result", "decode AdvertiseResult", err)
	}
	return res, nil
}
