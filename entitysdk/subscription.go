package entitysdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// The subscription bridge (SDK-ALIGNMENT §7.3): AppPeer.Subscribe
// dispatches through system/subscription and surfaces dispatched
// notifications as a native Go channel. The bridge owns the inbox
// path + channel handler + delivery token lifecycle so callers never
// touch handler.Handler, capability tokens, or signatures.
//
// Layering: this IS the dispatched notification surface. Every event
// reaches the subscriber through a full EXECUTE dispatch — capability
// check on delivery, handler resolution, signed envelope — matching
// the cross-peer case. For same-process observation without
// capability enforcement, use Store.Watch (§7.1).
//
// Handler registration goes through AppPeer.RegisterHandler (§11.5),
// which writes the manifest, interface, and (when scoped) grant
// entities alongside the dispatch-index binding. Subscription.Close
// tears all three down via HandlerHandle.Close.

// Subscription is an active dispatched subscription. Events arrive on
// the channel returned by Events(). Close to unsubscribe and tear
// down the per-subscription inbox.
type Subscription struct {
	id         string
	subID      string // engine-assigned subscription ID
	pattern    string
	inboxPath  string
	remotePeer string // empty = local; non-empty = subscription stored on this peer
	events     chan ChangeEvent
	ap         *AppPeer
	ch         *channelInboxHandler
	handle     *HandlerHandle

	mu     sync.Mutex
	closed bool
}

// RemotePeer returns the peer-id whose namespace this subscription
// observes. Empty string for local subscriptions.
func (s *Subscription) RemotePeer() string { return s.remotePeer }

// RawSubscription represents an active subscription whose
// notifications dispatch to a caller-supplied delivery URI rather
// than the SDK's Go-channel inbox. Used to compose subscription +
// continuation chains where the deliveries land at an inbox path
// that already has a continuation entity bound.
//
// Close dispatches unsubscribe at the (possibly remote) subscription
// handler. The caller is responsible for tearing down the
// continuation entities that consume the deliveries.
type RawSubscription struct {
	ap         *AppPeer
	subID      string
	pattern    string
	deliverURI string
	remotePeer string
}

// ID returns the engine-assigned subscription ID.
func (rs *RawSubscription) ID() string { return rs.subID }

// Pattern returns the subscription pattern.
func (rs *RawSubscription) Pattern() string { return rs.pattern }

// DeliverURI returns the URI deliveries are dispatched to.
func (rs *RawSubscription) DeliverURI() string { return rs.deliverURI }

// RemotePeer returns the peer-id whose subscription registry holds
// this subscription. Empty string for local-only subscriptions.
func (rs *RawSubscription) RemotePeer() string { return rs.remotePeer }

// Close dispatches unsubscribe at the (possibly remote) subscription
// handler. Safe to call more than once.
func (rs *RawSubscription) Close() error {
	if rs == nil || rs.subID == "" {
		return nil
	}
	cancelReq := types.SubscriptionCancelData{SubscriptionID: rs.subID}
	cancelEntity, err := cancelReq.ToEntity()
	if err != nil {
		return err
	}
	subURI := "system/subscription"
	if rs.remotePeer != "" && rs.remotePeer != rs.ap.PeerID() {
		subURI = extPeerURI(rs.ap.PeerID(), rs.remotePeer, "system/subscription")
	}
	_, err = rs.ap.executor.ExecuteWithParams(subURI, "unsubscribe", cancelEntity)
	return err
}

// SubscribeRawAt subscribes to pattern on peerID's namespace and
// configures notifications to dispatch to the caller-supplied
// deliverURI + deliverOp. Unlike Subscribe/SubscribeAt, this does
// *not* register a Go-channel inbox handler — the caller is
// responsible for ensuring deliverURI has a handler (typically a
// continuation entity bound at the path).
//
// When peerID equals the local peer, the subscription is stored
// locally; otherwise the cross-peer dispatch path with Extras-
// carried deliver_token applies (see SubscribeAt).
//
// Returns a RawSubscription whose Close cancels the subscription at
// the (possibly remote) registry.
func (a *AppPeer) SubscribeRawAt(peerID, pattern, deliverURI, deliverOp string, opts SubscribeOpts) (*RawSubscription, error) {
	if a.subEngine == nil {
		return nil, NewError(500, "subscription_disabled",
			"enable PeerConfig.Extensions.Subscription to use Subscribe")
	}
	if pattern == "" {
		return nil, NewError(400, "invalid_pattern", "pattern is empty")
	}
	if peerID == "" {
		return nil, NewError(400, "invalid_peer_id", "peerID is empty")
	}
	if deliverURI == "" || deliverOp == "" {
		return nil, NewError(400, "invalid_delivery", "deliverURI + deliverOp are required")
	}

	// Mint a delivery token authorizing the subscription engine to
	// dispatch deliverOp at deliverURI. The cap's resource scope must
	// cover deliverURI; we use a per-call scope rather than a
	// wildcard so the token is least-privilege.
	capEntity, sigEntity, err := a.mintRawDeliveryToken(deliverURI, deliverOp)
	if err != nil {
		return nil, WrapError(500, "token_mint_failed", "mint delivery token", err)
	}
	cs := a.peer.Store()
	capHash, cerr := cs.Put(capEntity)
	if cerr != nil {
		return nil, WrapError(500, "store_failed", "store delivery token", cerr)
	}
	if _, serr := cs.Put(sigEntity); serr != nil {
		return nil, WrapError(500, "store_failed", "store delivery token sig", serr)
	}

	subscriptionHandlerURI := "system/subscription"
	if peerID != a.PeerID() {
		subscriptionHandlerURI = extPeerURI(a.PeerID(), peerID, "system/subscription")
	}

	subReq := types.SubscriptionRequestData{
		DeliverTo:      types.DeliverySpec{URI: deliverURI, Operation: deliverOp},
		DeliverToken:   capHash,
		IncludePayload: opts.IncludePayload,
		Limits:         opts.Limits,
	}
	if len(opts.Events) > 0 {
		subReq.Events = opts.Events
	}
	paramsEnt, perr := subReq.ToEntity()
	if perr != nil {
		return nil, WrapError(500, "request_build_failed", "build subscribe request", perr)
	}

	identity := a.peer.Identity()
	resp, derr := a.executor.ExecuteWithIncluded(
		subscriptionHandlerURI,
		"subscribe",
		paramsEnt,
		&types.ResourceTarget{Targets: []string{pattern}},
		map[hash.Hash]entity.Entity{
			capEntity.ContentHash: capEntity,
			sigEntity.ContentHash: sigEntity,
			identity.ContentHash:  identity,
		},
	)
	if derr != nil {
		return nil, derr
	}

	var subID string
	var result map[string]interface{}
	if err := ecf.Decode(resp.Data, &result); err == nil {
		if v, ok := result["subscription_id"].(string); ok {
			subID = v
		}
	}

	return &RawSubscription{
		ap:         a,
		subID:      subID,
		pattern:    pattern,
		deliverURI: deliverURI,
		remotePeer: peerID,
	}, nil
}

// mintRawDeliveryToken builds a delivery token scoped to a specific
// deliverURI + operation (least-privilege variant of
// mintSubscriberToken). Granter = local identity; grantee = local
// identity (the remote subscription engine carries it across the
// wire when dispatching the notification).
func (a *AppPeer) mintRawDeliveryToken(deliverURI, operation string) (entity.Entity, entity.Entity, error) {
	identity := a.peer.Identity()
	kp := a.peer.Keypair()

	now := uint64(time.Now().UnixMilli())
	expires := now + 24*3600*1000
	capData := types.CapabilityTokenData{
		Grants: []types.GrantEntry{
			{
				Handlers:   types.CapabilityScope{Include: []string{"*"}},
				Resources:  types.CapabilityScope{Include: []string{deliverURI}},
				Operations: types.CapabilityScope{Include: []string{operation}},
			},
		},
		Granter:   types.SingleSigGranter(identity.ContentHash),
		Grantee:   identity.ContentHash,
		CreatedAt: now,
		ExpiresAt: &expires,
	}
	capEntity, err := capData.ToEntity()
	if err != nil {
		return entity.Entity{}, entity.Entity{}, fmt.Errorf("cap entity: %w", err)
	}
	sig := kp.Sign(capEntity.ContentHash.Bytes())
	sigEntity, err := types.SignatureData{
		Target:    capEntity.ContentHash,
		Signer:    identity.ContentHash,
		Algorithm: "ed25519",
		Signature: sig,
	}.ToEntity()
	if err != nil {
		return entity.Entity{}, entity.Entity{}, fmt.Errorf("sig entity: %w", err)
	}
	return capEntity, sigEntity, nil
}

// SubscribeOpts controls optional subscription parameters. The zero
// value omits a Limits field on the subscription request — the
// server applies its own defaults per EXTENSION-SUBSCRIPTION §2.4
// (the spec leaves server defaults implementation-defined; impls
// observed to disagree). Pass an explicit Limits to express the
// subscriber's intent, but note the server MAY tighten further (it
// MUST NOT relax) — a permissive Limits sent to an impl with
// restrictive server defaults still caps at the server's value.
type SubscribeOpts struct {
	// Events filters which event types to deliver. Leave empty for
	// all three ("created", "updated", "deleted").
	Events []string

	// IncludePayload opts in to entity bundling in the delivery
	// envelope's Included map per EXTENSION-SUBSCRIPTION v3.14 §4.2.
	// When true, the engine attaches the changed entity at notif.Hash
	// to the delivery so the receiving handler can read it from
	// hctx.Included without a follow-up cross-peer GET. Essential for
	// Stage 3 cross-peer chains where the receiver needs the file
	// entity to decide what blob to fetch.
	IncludePayload bool

	// Limits sets the subscription's resource limits per
	// EXTENSION-SUBSCRIPTION §2.4. nil omits the limits field
	// entirely — server applies its own defaults. Non-nil expresses
	// the subscriber's intent; per §2.4 "If the subscriber requests
	// limits, the server MAY apply stricter limits but MUST NOT
	// apply more permissive ones." Set RateLimit to a high value
	// (e.g. 1_000_000) when probing substrate throughput on impls
	// that apply restrictive defaults (Python's hardcoded 60/min);
	// the server still caps at its own default, but the request
	// expresses workbench-side intent and is min'd with server
	// values rather than treated as opt-out.
	Limits *types.SubscriptionLimitsData
}

// Events returns the read-only event channel. It is closed after
// Close is called.
func (s *Subscription) Events() <-chan ChangeEvent { return s.events }

// Pattern returns the pattern this subscription was created with.
func (s *Subscription) Pattern() string { return s.pattern }

// ID returns the engine-assigned subscription ID.
func (s *Subscription) ID() string { return s.subID }

// Close dispatches unsubscribe, tears down the per-subscription
// handler registration (dispatch index + tree entries), and closes
// the event channel. Safe to call more than once.
func (s *Subscription) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Stop feeding the channel first so in-flight deliveries don't
	// panic-write on a closed channel. The engine may still be
	// processing an event for this subscription as we unsubscribe.
	s.ch.close()

	// Dispatch unsubscribe. Subscriber ownership check uses
	// hctx.AuthorHash, which matches because the SDK dispatches as
	// the local peer identity. For cross-peer subscriptions the
	// unsubscribe targets the remote subscription handler.
	if s.subID != "" {
		cancelReq := types.SubscriptionCancelData{SubscriptionID: s.subID}
		cancelEntity, err := cancelReq.ToEntity()
		if err == nil {
			subURI := "system/subscription"
			if s.remotePeer != "" && s.remotePeer != s.ap.PeerID() {
				subURI = extPeerURI(s.ap.PeerID(), s.remotePeer, "system/subscription")
			}
			_, _ = s.ap.executor.ExecuteWithParams(
				subURI, "unsubscribe", cancelEntity,
			)
		}
	}

	// Tear down the inbox handler registration. HandlerHandle.Close
	// unregisters the dispatch index entry (§11.5.2) then removes
	// the tree entries (interface + handler + grant-if-present) in
	// reverse write order.
	_ = s.handle.Close()

	// Remove the SDK's restoration-tracking sidecar so an explicitly
	// cancelled subscription doesn't auto-restore on next restart.
	// Only meaningful for cross-peer subscriptions (no sidecar exists
	// for local subs).
	if s.remotePeer != "" && s.remotePeer != s.ap.PeerID() {
		s.ap.removeSubscriptionTracking(s.id)
	}

	close(s.events)
	return nil
}

// Watch is the SDK-OPERATIONS §6.1 simple form: subscribe to all
// event types on pattern with a zero-config SubscribeOpts, returning
// the same Subscription handle. Use Subscribe directly when you need
// event-type filtering or future limits knobs.
//
// Both routes land in the same dispatched bridge (§7.3) — events
// arrive through full EXECUTE dispatch, not raw-sink fanout.
func (a *AppPeer) Watch(pattern string) (*Subscription, error) {
	return a.Subscribe(pattern, SubscribeOpts{})
}

// Subscribe registers for dispatched change notifications matching
// pattern on the local peer's namespace. Returns a Subscription whose
// Events() channel receives each matched event.
//
// Pattern forms per SDK-OPERATIONS §6.1:
//
//   - exact:  "knowledge/articles/intro"
//   - prefix: "knowledge/articles/*"
//
// Peer-relative patterns resolve to the local peer namespace.
//
// Requires PeerConfig.Extensions.Subscription to be enabled; returns
// a 500 subscription_disabled Error otherwise.
//
// For subscriptions on a remote peer's namespace, use SubscribeAt.
func (a *AppPeer) Subscribe(pattern string, opts SubscribeOpts) (*Subscription, error) {
	return a.SubscribeAt(a.PeerID(), pattern, opts)
}

// SubscribeAt subscribes to pattern on the peerID peer's namespace.
// When peerID equals the local peer, behaves identically to Subscribe.
// Otherwise the subscription entity is stored on the remote peer and
// its subscription engine dispatches notifications back to a local
// inbox handler at entity://{localID}/system/inbox/sdk-sub-{id}.
//
// The delivery token is rooted at the local peer (the inbox owner)
// and authorizes the "receive" operation on the inbox path; the
// remote subscription engine carries the token when dispatching back.
//
// Cross-peer subscribe requires:
//   - A connection to peerID (established via AppPeer.Connect).
//   - The remote to accept the local peer's inbound dispatches.
//     With OpenAccessGrants on the remote, no further setup is needed.
//
// Returns the same Subscription type as Subscribe; Close tears down
// the inbox handler locally and dispatches unsubscribe at the remote.
func (a *AppPeer) SubscribeAt(peerID, pattern string, opts SubscribeOpts) (*Subscription, error) {
	if a.subEngine == nil {
		return nil, NewError(500, "subscription_disabled",
			"enable PeerConfig.Extensions.Subscription to use Subscribe")
	}
	if pattern == "" {
		return nil, NewError(400, "invalid_pattern", "pattern is empty")
	}
	if peerID == "" {
		return nil, NewError(400, "invalid_peer_id", "peerID is empty")
	}

	id, err := randomID()
	if err != nil {
		return nil, WrapError(500, "rand_failed", "generate subscription id", err)
	}
	inboxPath := "system/inbox/sdk-sub-" + id
	events := make(chan ChangeEvent, 64)
	ch := &channelInboxHandler{out: events}

	// Inbox handler ALWAYS lives on the local peer — the deliveries
	// come back to us regardless of where the subscription entity is
	// stored. RegisterHandler writes interface + handler + dispatch
	// index entries atomically with compensation on partial failure.
	handle, err := a.RegisterHandler(HandlerSpec{
		Pattern: inboxPath,
		Name:    "sdk-inbox-channel",
		Operations: map[string]types.HandlerOperationSpec{
			"receive": {InputType: "primitive/any"},
		},
	}, ch.Handle)
	if err != nil {
		return nil, err
	}

	// Compute the delivery URI and identify the grantee BEFORE minting
	// the token: the token's resources scope must match the request's
	// deliver_uri string exactly under cross-peer subscribe, and the
	// grantee must be the remote peer's identity (the deliverer). For
	// local subscriptions both collapse onto self.
	//
	// Why the resources field must be the qualified URI under cross-peer:
	// the remote's subscription engine (Rust per
	// extensions/subscription/src/lib.rs:791 validate_delivery_token_scope)
	// checks token.resources.include against the request's deliver_uri
	// by literal/wildcard/subtree match. A bare inbox path does not
	// match an entity:// URI; the token reads as scope-insufficient.
	deliverURI := inboxPath
	subscriptionHandlerURI := "system/subscription"
	granteeHash := a.peer.Identity().ContentHash // local default: self
	var parentCapHash *hash.Hash                  // unused locally; set for cross-peer forward-compat
	if peerID != a.PeerID() {
		deliverURI = fmt.Sprintf("entity://%s/%s", a.PeerID(), inboxPath)
		subscriptionHandlerURI = extPeerURI(a.PeerID(), peerID, "system/subscription")
		remoteHash, parentHash, lookupErr := a.remoteConnectionCapInfo(peerID)
		if lookupErr != nil {
			_ = handle.Close()
			return nil, WrapError(500, "connection_lookup_failed",
				"resolve remote identity hash for cross-peer subscribe", lookupErr)
		}
		// Graceful degrade when the connection has no cap available
		// (in-process flows that bypass AUTHENTICATE): keep self as
		// grantee. Cross-impl flows go through AUTHENTICATE and the cap
		// is populated, so this path is for Go-to-Go in-process only.
		if !remoteHash.IsZero() {
			granteeHash = remoteHash
		}
		parentCapHash = parentHash
	}

	// Delivery token: granter=local peer (we authorize routing into our
	// own inbox); grantee=the deliverer (us for local, the remote peer
	// for cross-peer). Cross-peer also sets Parent=connectionCapHash
	// to make the delegation chain explicit per validate-peer's recipe
	// (forward-compat for SB1 chain-walk + Python verifier).
	capEntity, sigEntity, err := a.mintSubscriberToken(deliverURI, granteeHash, parentCapHash)
	if err != nil {
		_ = handle.Close()
		return nil, WrapError(500, "token_mint_failed", "mint delivery token", err)
	}

	cs := a.peer.Store()
	capHash, cerr := cs.Put(capEntity)
	if cerr != nil {
		_ = handle.Close()
		return nil, WrapError(500, "store_failed", "store delivery token", cerr)
	}
	if _, serr := cs.Put(sigEntity); serr != nil {
		_ = handle.Close()
		return nil, WrapError(500, "store_failed", "store delivery token sig", serr)
	}

	subReq := types.SubscriptionRequestData{
		DeliverTo:      types.DeliverySpec{URI: deliverURI, Operation: "receive"},
		DeliverToken:   capHash,
		IncludePayload: opts.IncludePayload,
		Limits:         opts.Limits,
	}
	if len(opts.Events) > 0 {
		subReq.Events = opts.Events
	}
	paramsEnt, perr := subReq.ToEntity()
	if perr != nil {
		_ = handle.Close()
		return nil, WrapError(500, "request_build_failed", "build subscribe request", perr)
	}

	identity := a.peer.Identity()
	resp, derr := a.executor.ExecuteWithIncluded(
		subscriptionHandlerURI,
		"subscribe",
		paramsEnt,
		&types.ResourceTarget{Targets: []string{pattern}},
		map[hash.Hash]entity.Entity{
			capEntity.ContentHash: capEntity,
			sigEntity.ContentHash: sigEntity,
			identity.ContentHash:  identity,
		},
	)
	if derr != nil {
		_ = handle.Close()
		return nil, derr
	}

	var subID string
	var result map[string]interface{}
	if err := ecf.Decode(resp.Data, &result); err == nil {
		if v, ok := result["subscription_id"].(string); ok {
			subID = v
		}
	}

	// Persist restoration-tracking sidecar for cross-peer subs so
	// RestorePriorSubscriptions can re-issue after process restart
	// per EXTENSION-SUBSCRIPTION v3.15 §5.7. Failures are non-fatal:
	// the subscription itself is alive; restoration just won't fire
	// for this entry. Tracking is a no-op for local subscriptions.
	if peerID != a.PeerID() {
		// Tracking is best-effort; failure to persist the sidecar does
		// not invalidate the live subscription. RestorePriorSubscriptions
		// just won't see this entry on next restart.
		_ = a.writeSubscriptionTracking(id, peerID, pattern, opts)
	}

	return &Subscription{
		id:        id,
		subID:     subID,
		pattern:   pattern,
		inboxPath: inboxPath,
		events:    events,
		ap:        a,
		ch:        ch,
		handle:    handle,
		remotePeer: peerID,
	}, nil
}

// mintSubscriberToken builds a capability token + signature authorizing
// "receive" on the per-subscription delivery URI. Caller computes the
// deliverURI shape (bare inbox path for local; entity:// URI for
// cross-peer) and the grantee (self for local; remote peer's identity
// hash for cross-peer). parentCapHash is the connection cap's hash for
// cross-peer forward-compat (SB1 chain-walk); nil for local.
//
// Handlers.Include is "system/inbox" (handler name, not a path pattern)
// matching validate-peer's recipe and what both Rust and Python
// verifiers expect.
func (a *AppPeer) mintSubscriberToken(deliverURI string, granteeHash hash.Hash, parentCapHash *hash.Hash) (entity.Entity, entity.Entity, error) {
	identity := a.peer.Identity()
	kp := a.peer.Keypair()

	now := uint64(time.Now().UnixMilli())
	expires := now + 24*3600*1000 // 24h is a generous default
	capData := types.CapabilityTokenData{
		Grants: []types.GrantEntry{
			{
				Handlers:   types.CapabilityScope{Include: []string{"system/inbox"}},
				Resources:  types.CapabilityScope{Include: []string{deliverURI}},
				Operations: types.CapabilityScope{Include: []string{"receive"}},
			},
		},
		Granter:   types.SingleSigGranter(identity.ContentHash),
		Grantee:   granteeHash,
		Parent:    parentCapHash,
		CreatedAt: now,
		ExpiresAt: &expires,
	}
	capEntity, err := capData.ToEntity()
	if err != nil {
		return entity.Entity{}, entity.Entity{}, fmt.Errorf("cap entity: %w", err)
	}

	sig := kp.Sign(capEntity.ContentHash.Bytes())
	sigEntity, err := types.SignatureData{
		Target:    capEntity.ContentHash,
		Signer:    identity.ContentHash,
		Algorithm: "ed25519",
		Signature: sig,
	}.ToEntity()
	if err != nil {
		return entity.Entity{}, entity.Entity{}, fmt.Errorf("sig entity: %w", err)
	}

	return capEntity, sigEntity, nil
}

// remoteConnectionCapInfo looks up the open connection to peerID and
// returns (remoteIdentityHash, connectionCapHash, error). Used by
// SubscribeAt to build a delivery token whose grantee + parent match
// validate-peer's cross-impl recipe (the shape Rust + Python verifiers
// accept).
//
// Connection cap storage is asymmetric in core-go: outbound (dialer)
// connections expose the cap via `Session().Capability`; inbound
// (listener) connections expose it via `ConnState().GrantedCapability`.
// Check both so this works regardless of which side called Subscribe.
//
// If no cap is found on the connection (in-process test paths that
// bypass the AUTHENTICATE handler), return zero hashes — the caller
// degrades to a self-issued token without a parent. This is OK for
// Go-to-Go in-process flows where the local peer is the trust root;
// cross-impl flows go through real AUTHENTICATE and the cap is there.
func (a *AppPeer) remoteConnectionCapInfo(peerID string) (hash.Hash, *hash.Hash, error) {
	for _, c := range a.peer.Connections() {
		cs := c.ConnState()
		if cs == nil || string(cs.RemotePeerID) != peerID {
			continue
		}
		var capEnt entity.Entity
		if sess := c.Session(); sess != nil && sess.Capability != nil {
			capEnt = *sess.Capability
		} else if cs.GrantedCapability != nil {
			capEnt = *cs.GrantedCapability
		} else {
			// Connection found but no cap available — graceful degrade.
			return hash.Hash{}, nil, nil
		}
		capData, err := types.CapabilityTokenDataFromEntity(capEnt)
		if err != nil {
			return hash.Hash{}, nil, fmt.Errorf("decode connection cap: %w", err)
		}
		remoteHash, single := capData.Granter.SingleHash()
		if !single {
			return hash.Hash{}, nil, fmt.Errorf("connection cap granter is not single-sig")
		}
		parentHash := capEnt.ContentHash
		return remoteHash, &parentHash, nil
	}
	return hash.Hash{}, nil, fmt.Errorf("no open connection to %s", peerID)
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// channelInboxHandler is the per-subscription inbox handler body.
// AppPeer.Subscribe passes its Handle method value to RegisterHandler;
// the struct exists to own the channel, mutex, and closed flag shared
// between Handle and the teardown path.
//
// Backpressure: the send blocks when the buffer is full. A slow
// consumer stalls deliveries for this subscription only (not other
// subscriptions on the peer, and not the subscription engine for
// other subscribers). Spec §6.1 forbids silent drops; rate-limiting
// is the caller's responsibility via SubscriptionLimits (not wired
// through SubscribeOpts yet).
type channelInboxHandler struct {
	out chan ChangeEvent

	mu     sync.Mutex
	closed bool
}

// Handle is the HandlerFunc body bound to the inbox path by
// RegisterHandler. Each dispatched notification turns into one
// ChangeEvent on out.
func (h *channelInboxHandler) Handle(ctx context.Context, req *handler.Request) (*handler.Response, error) {
	if req.Operation != "receive" {
		return handler.NewErrorResponse(400, "unknown_operation",
			"sdk-inbox-channel supports only receive")
	}

	// The engine delivers a system/subscription/notification payload
	// as Params. Decode it and translate into a ChangeEvent.
	var notif types.SubscriptionNotificationData
	if len(req.Params.Data) > 0 {
		if err := ecf.Decode(req.Params.Data, &notif); err != nil {
			return handler.NewErrorResponse(400, "invalid_notification",
				"could not decode notification: "+err.Error())
		}
	}

	et := ChangePut
	if notif.Event == "deleted" {
		et = ChangeRemove
	}
	evt := ChangeEvent{
		EventType: et,
		Path:      stripEntityURI(notif.URI),
		NewHash:   notif.Hash,
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return handler.NewErrorResponse(410, "closed",
			"subscription channel closed")
	}
	out := h.out
	h.mu.Unlock()

	select {
	case out <- evt:
	case <-ctx.Done():
		return handler.NewErrorResponse(500, "canceled", "dispatch canceled")
	}

	// Body mirrors core-go's stock inbox handler (ext/inbox/handler.go):
	// entity.NewEntity rejects nil/empty data with
	// "invalid entity: data is empty", so the receive-result entity
	// must carry a non-empty CBOR payload. Without this the handler
	// returns 500/internal_error on every dispatch — the dispatching
	// peer logs every notification as "delivery failed" (F-CIMP-2),
	// even though the ChangeEvent was already handed to the consumer
	// above via `out <- evt`.
	resultRaw, _ := ecf.Encode(map[string]interface{}{"accepted": true})
	result, err := entity.NewEntity("system/inbox/receive-result", resultRaw)
	if err != nil {
		return handler.NewErrorResponse(500, "internal_error", err.Error())
	}
	return &handler.Response{Status: 200, Result: result}, nil
}

func (h *channelInboxHandler) close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
}

// stripEntityURI normalizes notification URIs by removing the
// entity:// scheme + peer-id prefix, returning a bare path. We leave
// absolute-form URIs in place (they came in as such; caller can tell
// from the leading '/').
func stripEntityURI(uri string) string {
	const scheme = "entity://"
	if !strings.HasPrefix(uri, scheme) {
		return uri
	}
	rest := uri[len(scheme):]
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return "/" + rest[:idx] + "/" + rest[idx+1:]
	}
	return rest
}
