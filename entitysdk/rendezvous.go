package entitysdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	cbor "github.com/fxamacker/cbor/v2"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/signaling"
)

// The `rendezvous` DISCOVERY backend — candidate surfacing over the
// SIGNALING carrier.
//
// **BUILT AGAINST `PROPOSAL-DISCOVERY-RENDEZVOUS-BACKEND` (RULED
// 2026-08-17) at arch `05faaa5`, NOT against landed
// `EXTENSION-DISCOVERY` (v1.0).** The proposal's §6 fold has not landed:
// the backend enum is still `<"mdns" | "qr" | ...>` with no `rendezvous`
// token and no §5.5. Everything normative here traces to the proposal's
// §2, and this comment is the coupling — grep it when the fold lands and
// re-derive against the spec text. The known cost is stated up front: if
// the token's spelling moves, this file's constant and its tests move
// with it. The enum is open, so an undeclared token is not a conformance
// violation for a consumer.
//
// A RULED proposal is a decision, and implementing one ahead of the
// editorial fold is how a spec gets validated before it hardens. What
// the implementer owes instead of a stop is this citation and the
// feedback below.
//
// **The layering, which is the whole reason this is not in
// signaling.go.** SIGNALING owns the carrier: the mailbox, the key
// derivation, the coordination messages. DISCOVERY owns candidate
// surfacing and the admission decision. The two specs state one seam
// from opposite ends —
//
//	SIGNALING §1.2  "The key introduces; it never authorizes."
//	DISCOVERY §2    "Discovery is the initiator of the grant, never the authority."
//
// — so nothing in this file grants anything. It surfaces candidates and
// stops. Admission is the §2 decision, which is ordinary capability
// machinery and is the user's.
//
// **What building it taught, routed to arch rather than resolved here**
// (see `docs/architecture/reviews/`): the proposal's `identity_hint MUST
// be absent` was written against a carrier with no signed container, and
// core-go's has one (§6.3, `ext/signaling.ClassifyCollected`) — so a
// verified counterpart identity is available at the mailbox and this
// backend is required to throw it away. And "`endpoint_hint` = the
// bucket/key locator" does not distinguish two counterparts standing at
// one tag; it needs deposit granularity, which is what we emit.

// RendezvousBackendKind is the DISCOVERY backend token minted by
// `PROPOSAL-DISCOVERY-RENDEZVOUS-BACKEND` §2 and destined for
// `EXTENSION-DISCOVERY` §2.1's open enum.
//
// **This is not an AP20 locally-invented constant, and the difference is
// the referent.** AP20 is about a string a spec table names that exists
// in no other document — the tell being a doc comment explaining the
// absence. Here the string exists because a ruling minted it, by name,
// with the reasoning for choosing it over `signaling` (the enum's values
// name *how a candidate is obtained*, not the carrier the resulting
// connection uses; `signaling` would break that axis and the token would
// lie the day the same backend ran over a different carrier).
// `core/types` declares `DiscoveryBackendMDNS` and nothing else, which
// is upstream of core-go rather than a gap for us to fill differently.
const RendezvousBackendKind = "rendezvous"

// Rendezvous poll/re-offer defaults. Both are deliberately well under
// the node's §4.5 blob TTL (60s default): a deposit that is not
// refreshed is reaped by the node, and a peer that re-offers on the TTL
// boundary is invisible for whatever fraction of the window its clock is
// behind.
const (
	// DefaultRendezvousPollInterval is how often a live browse session
	// re-collects the bucket.
	DefaultRendezvousPollInterval = 5 * time.Second
	// DefaultRendezvousReofferInterval is how often an announcing peer
	// re-deposits its presence blob.
	DefaultRendezvousReofferInterval = 20 * time.Second
)

// RendezvousConfig configures one rendezvous — a node, a key mode, and
// the input that mode derives from.
type RendezvousConfig struct {
	// Node is the peer-id of the signaling node both arms use.
	//
	// **SIGNALING §3.4 makes same-provider a MUST, and this field is
	// where that MUST is kept or lost.** Two peers on different nodes
	// derive the same key, deposit into different mailboxes, and never
	// meet — with no error on either side and nothing in either log. It
	// is the failure mode this whole surface is most likely to hit, so
	// the node is a required field rather than something defaulted.
	Node string

	// Mode is one of RendezvousModeTag / RendezvousModeSecret /
	// RendezvousModeLobby.
	//
	// **`pair` is refused** — see NewRendezvousBackend.
	Mode string

	// Input is the mode's derivation input: the tag label, the shared
	// secret, or the lobby constant (LobbyDefault unless the node's
	// `advertise` published an override, which Advertise reads).
	Input string

	// Candidates is how a counterpart should reach us, carried in the
	// presence blob we deposit. Static by necessity: **no TURN
	// credential mechanism is specified anywhere in the corpus** (arch
	// Q18), and `data_relay` is `policy: open` only, so there is no
	// shape to synthesize a relay candidate from. An empty list is
	// legal — a peer may stand at a tag purely to watch.
	Candidates []types.NetworkCandidateData

	// PollInterval / ReofferInterval override the defaults above.
	PollInterval    time.Duration
	ReofferInterval time.Duration
}

// RendezvousEndpointHint is this backend's `endpoint_hint` payload
// (DISCOVERY §2.1 types the field as opaque).
//
// **It is a locator for a DEPOSIT, not for a peer, and both halves of
// that matter.**
//
// *Not a peer:* a rendezvous candidate has no verified peer-id yet
// (§2.2), so there is no address to put here. What the admission path
// needs is enough to go back to the bucket and act on the specific blob
// this candidate was surfaced from.
//
// *A deposit rather than the bucket:* the proposal says `endpoint_hint`
// is "the bucket/key locator", which is not enough — two counterparts
// standing at one tag would produce two candidates identical in every
// field but `observed_at`, and a consumer could not tell it had found
// two peers rather than one peer twice. Deposit granularity is the
// smallest addition that fixes it. Routed to arch as feedback.
//
// **`Label` is present only for the public modes.** SIGNALING §3.2 types
// a tag as a "public label — discovery convenience, not access control",
// and a lobby constant is a published deployment name; both are safe to
// write into the tree. A `secret`-mode input is not: it is the whole
// access control, and the derived key is equivalent to it. So neither
// the secret nor the key appears here, and the local peer — which
// already holds the config it configured — re-derives what it needs.
// A candidate that carried the key would put a credential in an entity
// whose whole purpose is to be shown to a user for a decision.
type RendezvousEndpointHint struct {
	// Node is the signaling node peer-id the deposit was observed at.
	Node string `cbor:"node"`
	// Mode is the §3.2 key mode.
	Mode string `cbor:"mode"`
	// Label is the derivation input for `tag` / `lobby` only; absent
	// for `secret`.
	Label string `cbor:"label,omitempty"`
	// Deposit is the content hash of the observed blob, as hex.
	Deposit string `cbor:"deposit"`
}

// DecodeRendezvousEndpointHint decodes a rendezvous candidate's opaque
// `endpoint_hint`.
func DecodeRendezvousEndpointHint(raw []byte) (RendezvousEndpointHint, error) {
	var h RendezvousEndpointHint
	if err := cbor.Unmarshal(raw, &h); err != nil {
		return RendezvousEndpointHint{}, fmt.Errorf("decode rendezvous endpoint_hint: %w", err)
	}
	return h, nil
}

// RendezvousBackend implements `ext/discovery.Backend` over the
// SIGNALING carrier.
type RendezvousBackend struct {
	ap  *AppPeer
	cfg RendezvousConfig
	sc  *SignalingClient
	key []byte

	mu sync.Mutex
	// announcing tracks live announce sessions by profileRef, so
	// AnnounceStop can be idempotent on a profile that is not running
	// (§3 MUST) while still refusing one the backend does not recognize.
	announcing map[string]context.CancelFunc
	// browsing is the live §3.0 browse session's cancel, nil when no
	// Scan has started one.
	browsing context.CancelFunc
	// surfaced maps a deposit content-hash to the candidate entity hash
	// we emitted for it. Two jobs: emit each deposit ONCE (a candidate
	// entity embeds observed_at, so re-emitting on every poll would mint
	// a new entity per poll and the consumer would show one peer N
	// times — the exact defect the mDNS path works around downstream in
	// ReadDiscoveredCandidates), and give the reap path something to
	// name when a deposit ages out of the bucket.
	surfaced map[string]hash.Hash

	observe func(types.CandidateData)
	reapCb  func(hash.Hash)
}

// NewRendezvousBackend builds the backend for one rendezvous. It does
// not register itself — AppPeer.EnableRendezvousDiscovery does that.
//
// **`pair` mode is refused, and the refusal is normative rather than a
// simplification.** Of SIGNALING §3.2's four key modes, only `tag`,
// `secret` and `lobby` are discovery. A `pair` key takes both peer-ids
// as *inputs*, so you already hold the counterpart's identity — there is
// nothing to surface and nothing to admit that was not already known.
// That is connect-to-a-known-peer (DISCOVERY §5.1), and a backend that
// accepted it would emit a candidate whose whole content the caller
// supplied, which reads to a user as "a stranger appeared" when nobody
// did. Proposal §2.1.
func NewRendezvousBackend(ap *AppPeer, cfg RendezvousConfig) (*RendezvousBackend, error) {
	if ap == nil {
		return nil, NewError(400, "invalid_config", "rendezvous: nil peer")
	}
	if cfg.Node == "" {
		return nil, NewError(400, "invalid_config",
			"rendezvous: Node is required — EXTENSION-SIGNALING §3.4 makes same-provider a MUST, "+
				"and two peers on different nodes derive the same key into different mailboxes and "+
				"never meet, with no error on either side")
	}
	if cfg.Input == "" {
		return nil, NewError(400, "invalid_config",
			"rendezvous: Input is required (the tag label, the secret, or the lobby constant)")
	}

	var (
		key []byte
		err error
	)
	switch cfg.Mode {
	case RendezvousModeTag:
		key, err = TagKey(cfg.Input)
	case RendezvousModeSecret:
		key, err = SecretKey(cfg.Input)
	case RendezvousModeLobby:
		key, err = LobbyKey(cfg.Input)
	case RendezvousModePair:
		return nil, NewError(400, "mode_not_discovery",
			"rendezvous: `pair` is not a discovery mode — both peer-ids are INPUTS to a pair key, "+
				"so you already hold the counterpart's identity and there is no candidate to surface "+
				"(PROPOSAL-DISCOVERY-RENDEZVOUS-BACKEND §2.1). Connecting to a peer you can already "+
				"name is EXTENSION-DISCOVERY §5.1 — ordinary NETWORK dispatch, not discovery")
	case "":
		return nil, NewError(400, "invalid_config",
			"rendezvous: Mode is required (tag | secret | lobby)")
	default:
		return nil, NewError(400, "unknown_mode",
			fmt.Sprintf("rendezvous: unknown key mode %q; EXTENSION-SIGNALING §3.2 declares "+
				"pair | tag | secret | lobby, and the mode string is domain-separated INTO the key, "+
				"so an unrecognized one would derive a mailbox nobody else reaches", cfg.Mode))
	}
	if err != nil {
		return nil, WrapError(500, "key_derivation_failed", "rendezvous: derive key", err)
	}

	return &RendezvousBackend{
		ap:         ap,
		cfg:        cfg,
		sc:         ap.Signaling(cfg.Node),
		key:        key,
		announcing: map[string]context.CancelFunc{},
		surfaced:   map[string]hash.Hash{},
	}, nil
}

// Kind implements discovery.Backend.
func (rb *RendezvousBackend) Kind() string { return RendezvousBackendKind }

// RendezvousKey returns the derived §2.2 key. Exported for diagnosis:
// when two arms do not meet, the key bytes and the node peer-id are the
// two things worth comparing first, and neither is otherwise visible.
func (rb *RendezvousBackend) RendezvousKey() []byte { return rb.key }

// SetObserveCallback implements discovery.Backend. The substrate calls
// it once at RegisterBackend.
func (rb *RendezvousBackend) SetObserveCallback(
	observe func(types.CandidateData), reap func(hash.Hash),
) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.observe, rb.reapCb = observe, reap
}

// Scan implements discovery.Backend: collect the bucket, skip our own
// deposits, and surface one candidate per counterpart deposit.
//
// **The hybrid shape is DISCOVERY §3.0's, not a convenience.** The call
// returns an immediate snapshot AND starts (or refreshes) a browse
// session that keeps polling and pushes arrivals and departures through
// the observe/reap callbacks. A rendezvous bucket is a streaming surface
// in its native form — a counterpart arrives whenever they arrive — so
// modeling it as a one-shot would lose every arrival between calls.
//
// `filter` is accepted and ignored (§3.3 permits a backend to ignore
// it). It is not *rejected*: an unparseable filter MUST NOT come back as
// a silently empty result, and a backend that ignores filters has
// nothing to fail to parse.
func (rb *RendezvousBackend) Scan(
	ctx context.Context, filter map[string]cbor.RawMessage,
) ([]types.CandidateData, error) {
	_ = filter
	cands, err := rb.collect(ctx)
	if err != nil {
		return nil, err
	}
	rb.ensureBrowsing()
	return cands, nil
}

// collect performs one bucket read and returns the candidates it
// surfaced, firing observe for each newly seen deposit and reap for each
// deposit that has left the bucket.
func (rb *RendezvousBackend) collect(ctx context.Context) ([]types.CandidateData, error) {
	blobs, err := rb.sc.Collect(ctx, rb.key)
	if err != nil {
		return nil, err
	}

	self := rb.ap.PeerID()
	present := make(map[string]bool, len(blobs))
	var out []types.CandidateData

	for _, blob := range blobs {
		// A §6.3 container that fails to verify, and a blob this build
		// has never heard of, are BOTH skips rather than errors (§4.5,
		// §6.4). A shared bucket legitimately holds anything — another
		// implementation's future message type, a half-finished punch —
		// and a poll that failed on the first stranger would be a
		// mailbox any participant could break for everyone.
		msg, classifyErr := signaling.ClassifyCollected(blob, rb.key)
		if classifyErr != nil || msg.Kind != signaling.KindConnectRequest {
			continue
		}
		// Skip-own (§6.4). Without it a peer that stands at a tag
		// discovers ITSELF, and every step of that reports success —
		// the miserable class of bug where nothing errors and the
		// answer is wrong. The verified signer is preferred over the
		// claimed `initiator` because the latter is a wire field the
		// depositor chose.
		author := msg.Signer.PeerID
		if author == "" && msg.Request != nil {
			author = msg.Request.Initiator
		}
		if author == self {
			continue
		}

		depositHash := depositID(blob)
		present[depositHash] = true

		rb.mu.Lock()
		_, already := rb.surfaced[depositHash]
		rb.mu.Unlock()
		if already {
			continue
		}

		cd, err := rb.candidateFor(depositHash)
		if err != nil {
			return nil, err
		}
		ent, err := cd.ToEntity()
		if err != nil {
			return nil, WrapError(500, "encode_candidate", "rendezvous: materialize candidate", err)
		}

		rb.mu.Lock()
		rb.surfaced[depositHash] = ent.ContentHash
		observe := rb.observe
		rb.mu.Unlock()

		out = append(out, cd)
		if observe != nil {
			observe(cd)
		}
	}

	rb.reapDeparted(present)
	return out, nil
}

// reapDeparted removes candidates whose deposit is no longer in the
// bucket.
//
// **Rendezvous has a real departure signal and it is worth using.**
// DISCOVERY §3.0.1 rule 4 waives reaping for one-shot backends "with no
// TTL semantic" — that is QR, not this. A node reaps a blob at its §4.5
// TTL, so a counterpart that stopped re-offering falls out of the bucket
// within one TTL and their candidate is stale from that moment. Rule 3
// is the shape that applies: a re-invocation refreshes what is still
// observed, and what is missing ages out. Leaving them would show a user
// a peer who left as someone they can still meet.
func (rb *RendezvousBackend) reapDeparted(present map[string]bool) {
	rb.mu.Lock()
	var gone []hash.Hash
	for deposit, candHash := range rb.surfaced {
		if !present[deposit] {
			gone = append(gone, candHash)
			delete(rb.surfaced, deposit)
		}
	}
	reap := rb.reapCb
	rb.mu.Unlock()

	if reap == nil {
		return
	}
	for _, h := range gone {
		reap(h)
	}
}

// candidateFor builds the §2.2 `candidate_0` for one observed deposit.
//
// **`PeerID` and `IdentityHint` are both absent, and neither is a
// shortcut.**
//
//   - `PeerID` absent is §2.2 step 1: the peer-id is established by
//     IDENTIFY over the *admitted channel*, and a successor candidate
//     carries it (see PromoteRendezvousCandidate). We hold a
//     cryptographically verified signer at this point and still do not
//     use it, because reaching a key proves someone derived that key —
//     not that the channel admission later opens belongs to them.
//   - `IdentityHint` absent is TOFU (§2.2.1): the grant decision IS the
//     trust anchor. A backend MUST NOT synthesize an identity-claim for
//     a peer that merely stood at a tag (proposal §2.2).
//
// Both are emitted **absent, not explicit-null** — §2.1's wire-convention
// erratum is a MUST and is load-bearing for cross-impl byte equality,
// because ECF hashes null and absent to different content hashes. The Go
// encoding gets this from `omitempty` on `CandidateData`; a zero-value
// string and a nil pointer both omit.
func (rb *RendezvousBackend) candidateFor(depositHash string) (types.CandidateData, error) {
	hint := RendezvousEndpointHint{
		Node:    rb.cfg.Node,
		Mode:    rb.cfg.Mode,
		Deposit: depositHash,
	}
	if rb.cfg.Mode != RendezvousModeSecret {
		hint.Label = rb.cfg.Input
	}
	raw, err := cbor.Marshal(hint)
	if err != nil {
		return types.CandidateData{}, WrapError(500, "encode_hint",
			"rendezvous: encode endpoint_hint", err)
	}
	return types.CandidateData{
		Backend:      RendezvousBackendKind,
		ObservedAt:   uint64(time.Now().UnixMilli()),
		EndpointHint: raw,
	}, nil
}

// ensureBrowsing starts the §3.0 watchable session if it is not running.
func (rb *RendezvousBackend) ensureBrowsing() {
	rb.mu.Lock()
	if rb.browsing != nil {
		rb.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rb.browsing = cancel
	interval := rb.cfg.PollInterval
	rb.mu.Unlock()

	if interval <= 0 {
		interval = DefaultRendezvousPollInterval
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// A failed poll is not fatal to the session: the node
				// may be briefly unreachable, and a browse that tore
				// itself down on one error would turn a blip into a
				// permanent loss of discovery with nothing to retry.
				_, _ = rb.collect(ctx)
			}
		}
	}()
}

// Announce implements discovery.Backend: deposit a presence blob at the
// rendezvous key and keep it alive.
//
// **The blob is a `system/signaling/connect-request` sealed in a §6.3
// container**, not a shape of ours. The carrier already defines what a
// peer standing at a key says — who it is, how to reach it, and a nonce
// to correlate the answer — and a discovery backend that minted a second
// presence type would put two encodings of one fact on a shared bucket
// that other implementations also read.
//
// **A deposit expires and re-offering is mandatory.** The node's §4.5
// TTL is binding (60s default) and a reaped blob is gone whether or not
// anyone collected it, so this starts a loop rather than writing once.
// Offer is idempotent by content hash (§5 pin 1), which is what makes a
// re-offer safe and a duplicate indistinguishable from the original.
func (rb *RendezvousBackend) Announce(ctx context.Context, profileRef string) error {
	if profileRef == "" {
		return NewError(400, "unknown_profile_ref",
			"rendezvous: announce needs a profile_ref naming the session")
	}
	rb.mu.Lock()
	if _, live := rb.announcing[profileRef]; live {
		rb.mu.Unlock()
		return nil // idempotent on an already-running profile
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	rb.announcing[profileRef] = cancel
	interval := rb.cfg.ReofferInterval
	rb.mu.Unlock()

	// Deposit once synchronously so a caller that returns from Announce
	// and immediately asks a counterpart to scan is not racing the first
	// tick. An error here rolls the session back rather than leaving a
	// registered profile that never deposited.
	if err := rb.offerPresence(ctx); err != nil {
		rb.mu.Lock()
		delete(rb.announcing, profileRef)
		rb.mu.Unlock()
		cancel()
		return err
	}

	if interval <= 0 {
		interval = DefaultRendezvousReofferInterval
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				_ = rb.offerPresence(loopCtx)
			}
		}
	}()
	return nil
}

// AnnounceStop implements discovery.Backend.
//
// **Idempotent on a profile that is not running** — DISCOVERY §3.3 makes
// that a MUST, and it is separate from the unknown-profile case: a
// profile this backend does not recognize is `400`, a recognized one
// that is not currently announced is a success. The two rules were
// written to be confusable and the spec says so.
//
// Stopping does NOT retract the deposit already in the bucket; nothing
// in the carrier can. The blob ages out at the node's TTL, which is why
// a counterpart may still surface us for up to one TTL after we stop.
// That is the carrier's contract, not a leak, and a caller telling a
// user "you are no longer visible" would be lying for that window.
func (rb *RendezvousBackend) AnnounceStop(_ context.Context, profileRef string) error {
	if profileRef == "" {
		return NewError(400, "unknown_profile_ref",
			"rendezvous: announce-stop needs a profile_ref")
	}
	rb.mu.Lock()
	cancel, live := rb.announcing[profileRef]
	delete(rb.announcing, profileRef)
	rb.mu.Unlock()
	if live {
		cancel()
	}
	return nil
}

// offerPresence seals and deposits one presence blob.
func (rb *RendezvousBackend) offerPresence(ctx context.Context) error {
	nonce, err := signaling.GenerateNonce()
	if err != nil {
		return WrapError(500, "nonce_failed", "rendezvous: generate nonce", err)
	}
	req := types.ConnectRequestData{
		Candidates: rb.cfg.Candidates,
		Initiator:  rb.ap.PeerID(),
		Nonce:      nonce,
	}
	ent, err := encodeAsEntity(types.TypeSignalingConnectRequest, req)
	if err != nil {
		return err
	}
	sealed, err := signaling.SealBlob(ent, rb.ap.peer.Keypair(), rb.key)
	if err != nil {
		return WrapError(500, "seal_failed", "rendezvous: seal presence blob", err)
	}
	blob, err := signaling.SealedToBlob(sealed)
	if err != nil {
		return WrapError(500, "encode_blob", "rendezvous: encode sealed blob", err)
	}
	return rb.sc.Offer(ctx, rb.key, blob)
}

// Close stops the browse session and every announce loop. Called by
// AppPeer.Close for a registered backend.
func (rb *RendezvousBackend) Close() {
	rb.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(rb.announcing)+1)
	for ref, cancel := range rb.announcing {
		cancels = append(cancels, cancel)
		delete(rb.announcing, ref)
	}
	if rb.browsing != nil {
		cancels = append(cancels, rb.browsing)
		rb.browsing = nil
	}
	rb.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// EnableRendezvousDiscovery builds the backend and registers it on the
// peer's discovery substrate.
//
// **The substrate has to already be wired**, and on this peer that means
// either a ListenAddr or an explicit `Extensions.Discovery`. The second
// exists because of this backend: mDNS needs a bound listener to
// announce a port, so the substrate was made listener-conditional — but
// a rendezvous is exactly the case where a peer has NO reachable
// listener and is standing at a mailbox to be introduced. Gating
// discovery on a listener would have excluded the peers the backend is
// for.
func (a *AppPeer) EnableRendezvousDiscovery(cfg RendezvousConfig) (*RendezvousBackend, error) {
	if a.discoveryHandler == nil {
		return nil, NewError(400, "discovery_disabled",
			"rendezvous: the discovery substrate is not wired on this peer — construct it with "+
				"Extensions.Discovery (or a ListenAddr). A rendezvous peer often has no listener, "+
				"which is why the explicit toggle exists")
	}
	rb, err := NewRendezvousBackend(a, cfg)
	if err != nil {
		return nil, err
	}
	a.discoveryHandler.RegisterBackend(rb)
	a.rdvMu.Lock()
	a.rendezvous = rb
	a.rdvMu.Unlock()
	return rb, nil
}

// Rendezvous returns the registered rendezvous backend, or nil.
func (a *AppPeer) Rendezvous() *RendezvousBackend {
	a.rdvMu.Lock()
	defer a.rdvMu.Unlock()
	return a.rendezvous
}

// ReadCandidates returns the candidate entities the substrate has bound
// under one backend's watchable prefix (DISCOVERY §3.0).
//
// **This is a store read, not a dispatch, and the distinction is AP11.**
// The prefix is peer-local — our own observations — so reading it
// through the store is reading what we hold. A dispatched read of a
// peer-qualified path would route to that peer and return THEIR tree.
//
// Unlike ReadDiscoveredCandidates (the mDNS reader), nothing here
// deduplicates or ages entries: the rendezvous backend emits one
// candidate per deposit and reaps on departure, so the prefix is already
// the live set. The mDNS path needs that machinery because its backend
// writes a fresh entity every scan and does not fire its reap callback.
func (a *AppPeer) ReadCandidates(backend string) []types.CandidateData {
	if a.store == nil {
		return nil
	}
	entries := a.store.List(types.CandidatePrefix(backend))
	out := make([]types.CandidateData, 0, len(entries))
	for _, e := range entries {
		ent, ok := a.store.Get(e.Path)
		if !ok || ent.Type != types.TypeDiscoveryCandidate {
			continue
		}
		cd, err := types.CandidateDataFromEntity(ent)
		if err != nil {
			continue
		}
		out = append(out, cd)
	}
	return out
}

// depositID names one blob in a bucket.
//
// **A LOCAL identifier, deliberately not a protocol hash.** The tree
// binds entities by their ECF content hash; a bucket blob is a sealed
// §6.3 container whose bytes are opaque to us, and computing an
// entity-shaped hash over it would invent a content-address for
// something the protocol does not address that way. All this has to do
// is be stable across polls and distinct between deposits, which is
// exactly what a digest of the bytes gives — and it stays inside this
// file, reaching the wire only as an opaque string inside
// `endpoint_hint`.
func depositID(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}
