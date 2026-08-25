package entitysdk

import (
	"fmt"

	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// resolve_chain.go — the SDK `resolve()` seam from
// PROPOSAL-UNIVERSAL-RESOLUTION.
//
// The proposal's load-bearing claim is that resolution is a *composition*
// of four already-landed protocol primitives (name→peer_id, peer_id→
// transport, tree-path→hash, hash→bytes), so the orchestration lives
// **client-side in the SDK** — never a kernel handler — because each
// cross-peer rung must present the *consumer's own* capability
// (GUIDE-RESOLUTION §5; confused-deputy avoidance).
//
// This file is that seam, composing the rungs that exist:
//
//   - name → peer_id      : ResolveName, dispatching system/registry:resolve
//                           (the rung that had NO SDK surface before this).
//   - peer_id → reach     : leans on AppPeer.Connect(addr) + the dispatcher's
//                           pooled-connection transport cache. The full
//                           Layer-B ladder (host-vouches, registry-transport
//                           fallback) is core-side and PARTIAL today
//                           (host-vouches is spec-only) — see the feedback doc.
//   - tree-path → hash    : AppPeer.Get (system/tree:get), local or remote.
//   - hash → bytes        : tree:get mode=entity / AppPeer.Content().Get,
//                           with substitute firing server-side transparently.
//
// browse_fetch (§3) = resolve(PeerId) ▸ resolve(TreePath) ▸ resolve(ContentHash);
// BrowseFetch below is that named composition.

const (
	registryHandlerURI  = "system/registry"
	localNameHandlerURI = "system/registry/local-name"
)

// Resolution rung labels — which link of the chain produced an Outcome
// (PROPOSAL-UNIVERSAL-RESOLUTION §3.2: "a dead-end at any rung is the
// honest answer for the whole chain, tagged by rung").
const (
	RungName      = "name"      // name → peer_id      (REGISTRY)
	RungTransport = "transport" // peer_id → reach     (NETWORK)
	RungPath      = "path"      // tree-path → hash    (TREE)
	RungContent   = "content"   // hash → bytes        (SUBSTITUTE)
)

// OutcomeKind is the typed dead-end taxonomy from
// PROPOSAL-UNIVERSAL-RESOLUTION §3.1, plus PinMismatch from
// PROPOSAL-NAME-GRAMMAR §3 (which the §3.1 table omits — flagged in the
// feedback doc). Per F1, each maps to one of two axes: *reachability*
// ("can I get to it") or *authorization* ("am I allowed").
type OutcomeKind string

const (
	// OutcomeNotFound — no such binding / path / content. Reachability.
	OutcomeNotFound OutcomeKind = "not_found"
	// OutcomePolicyRejected — binding existed but failed the receiver's
	// trust filter (REGISTRY §5). Reachability (trust).
	OutcomePolicyRejected OutcomeKind = "policy_rejected"
	// OutcomeUnreachable — peer known, no reachable transport (Layer B
	// exhausted). Reachability.
	OutcomeUnreachable OutcomeKind = "unreachable"
	// OutcomeDenied — reachable, but the owner's capability gate refused.
	// Authorization. The one outcome on the authz axis (F1).
	OutcomeDenied OutcomeKind = "denied"
	// OutcomePending — authoritative sync in flight (SUBSTITUTE 503
	// blob_pending_sync). Reachability (transient).
	OutcomePending OutcomeKind = "pending"
	// OutcomePinMismatch — name@peer_id resolved, but the result peer-id
	// != the pinned one (PROPOSAL-NAME-GRAMMAR §3 verification pin).
	OutcomePinMismatch OutcomeKind = "pin_mismatch"
)

// Outcome is a typed resolution dead-end. It implements error so it
// flows through the normal Go error path, but carries the *rung* that
// stopped and the *kind* of stop, so a UI can say "unreachable: peer
// found, no transport" vs "no such name" vs "content gone" without
// app-specific plumbing (the contract that makes a link "solve itself").
//
// It is a classifier layered over the SDK's existing Error{Status,Code}
// (errors.go), not a replacement: the wrapped *Error is preserved in Err
// so status-code inspection still works.
type Outcome struct {
	Kind    OutcomeKind
	Rung    string
	PeerID  string      // populated for Unreachable
	Missing []hash.Hash // populated for content NotFound / Pending
	Err     error       // the underlying SDK *Error, if any
}

func (o *Outcome) Error() string {
	if o.PeerID != "" {
		return fmt.Sprintf("resolve dead-end [%s/%s] peer=%s", o.Rung, o.Kind, o.PeerID)
	}
	return fmt.Sprintf("resolve dead-end [%s/%s]", o.Rung, o.Kind)
}

func (o *Outcome) Unwrap() error { return o.Err }

// AsOutcome extracts an *Outcome from an error chain, or nil.
func AsOutcome(err error) *Outcome {
	if o, ok := err.(*Outcome); ok {
		return o
	}
	return nil
}

// classifyError maps an SDK *Error from a given rung into a typed
// Outcome, or returns nil when the error is not a clean rung dead-end
// (the caller then propagates the raw error).
//
// **Confidentiality preservation (PROPOSAL-UNIVERSAL-RESOLUTION §8 / V7
// §5.5a):** this maps the status the peer *actually returned* — 404 →
// NotFound, 403 → Denied — and never synthesizes a more-informative
// Denied where the peer chose NotFound. The Denied-vs-NotFound call is
// the peer's to make; the SDK surfaces it faithfully.
func classifyError(rung string, err error) *Outcome {
	switch StatusOf(err) {
	case 404:
		return &Outcome{Kind: OutcomeNotFound, Rung: rung, Err: err}
	case 403:
		return &Outcome{Kind: OutcomeDenied, Rung: rung, Err: err}
	case 503:
		// SUBSTITUTE blob_pending_sync (§3.1): authoritative sync in
		// flight; retry-eligible.
		return &Outcome{Kind: OutcomePending, Rung: rung, Err: err}
	default:
		return nil
	}
}

// Resolution is the resolved `name → peer_id` binding (the §3.1 value
// branch for a Name ref). It carries identity *and* initial reach
// (transports) so a name lookup hands you both, not a peer_id you then
// reverse-look-up (MODEL §3.1).
type Resolution struct {
	Name        string
	PeerID      string
	Transports  []hash.Hash
	TrustAnchor string
	Status      string
}

// ResolveName walks the `name → peer_id` rung: it dispatches
// system/registry:resolve, which runs the meta-resolver over the
// resolver-config chain (REGISTRY §4.1: pinned → name_format_dispatch →
// chain in priority order → fail-closed). On a resolved binding it
// returns a *Resolution; on a chain-exhausted / not-found name it
// returns a typed *Outcome{NotFound, rung:name} (REGISTRY §4.1 fail-
// closed — never a silent empty).
//
// This is the SDK surface that did not exist before
// PROPOSAL-UNIVERSAL-RESOLUTION: the registry handler is wired into
// CreatePeer (default-on); call EnableLocalNameResolver once to activate
// the local-name backend in the chain.
func (a *AppPeer) ResolveName(name string) (*Resolution, error) {
	if name == "" {
		return nil, NewError(400, "invalid_name", "ResolveName requires a non-empty name")
	}
	req := types.ResolveRequestData{Name: name}
	reqEnt, err := req.ToEntity()
	if err != nil {
		return nil, WrapError(500, "encode_request", "encode resolve request", err)
	}
	resp, err := a.executor.ExecuteWithParams(registryHandlerURI, "resolve", reqEnt)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, NewError(500, "no_response", "system/registry:resolve returned no response")
	}
	if resp.Status >= 400 {
		if e := ErrorFromResponse(resp); e != nil {
			return nil, e
		}
		return nil, NewError(resp.Status, "resolve_failed",
			fmt.Sprintf("system/registry:resolve status %d", resp.Status))
	}
	result, err := types.ResolveResultDataFromEntity(resp.Entity())
	if err != nil {
		return nil, WrapError(500, "decode_failed", "decode resolve result", err)
	}
	switch result.Status {
	case types.ResolutionStatusResolved:
		return &Resolution{
			Name:        name,
			PeerID:      result.PeerID,
			Transports:  result.Transports,
			TrustAnchor: result.TrustAnchor,
			Status:      result.Status,
		}, nil
	default:
		// not_found / chain_exhausted — fail-closed at the name rung.
		return nil, &Outcome{
			Kind: OutcomeNotFound,
			Rung: RungName,
			Err:  NewError(404, "not_found", fmt.Sprintf("name %q: %s", name, result.Status)),
		}
	}
}

// ResolveNamePinned implements the name-grammar verification pin
// (PROPOSAL-NAME-GRAMMAR §3 / GUIDE-RESOLUTION §6.3): resolve `name`
// through the normal chain, then assert the result equals expectPeerID.
// On mismatch it returns *Outcome{PinMismatch} rather than silently
// accepting — name for reach, peer_id for trust.
func (a *AppPeer) ResolveNamePinned(name, expectPeerID string) (*Resolution, error) {
	res, err := a.ResolveName(name)
	if err != nil {
		return nil, err
	}
	if res.PeerID != expectPeerID {
		return nil, &Outcome{
			Kind: OutcomePinMismatch,
			Rung: RungName,
			Err: NewError(409, "pin_mismatch",
				fmt.Sprintf("name %q resolved to %s, pin expected %s", name, res.PeerID, expectPeerID)),
		}
	}
	return res, nil
}

// BindLocalName writes a local-name binding (name → target_peer_id +
// transports) via system/registry/local-name:bind (REGISTRY §6.5). The
// binding never leaves this peer; it makes `name` resolvable *for this
// receiver* through the local-name backend. Returns the binding's
// content hash. transports may be nil (resolution still yields the
// peer_id; reach then relies on a separately-established connection).
//
// The rest of the name book's upkeep — list, unbind, re-point the
// transports — lives in local_name.go. Options (WithNotes) are declared
// there too.
func (a *AppPeer) BindLocalName(name, targetPeerID string, transports []hash.Hash, opts ...BindOption) (hash.Hash, error) {
	req := types.LocalNameBindRequestData{
		Name:         name,
		TargetPeerID: targetPeerID,
		Transports:   transports,
	}
	for _, opt := range opts {
		opt(&req)
	}
	reqEnt, err := req.ToEntity()
	if err != nil {
		return hash.Hash{}, WrapError(500, "encode_request", "encode bind request", err)
	}
	resp, err := a.executor.ExecuteWithParams(localNameHandlerURI, "bind", reqEnt)
	if err != nil {
		return hash.Hash{}, err
	}
	if resp == nil || resp.Status >= 400 {
		if e := ErrorFromResponse(resp); e != nil {
			return hash.Hash{}, e
		}
		return hash.Hash{}, NewError(500, "bind_failed", "system/registry/local-name:bind failed")
	}
	res, err := types.LocalNameBindResultDataFromEntity(resp.Entity())
	if err != nil {
		return hash.Hash{}, WrapError(500, "decode_failed", "decode bind result", err)
	}
	return res.BindingHash, nil
}

// EnableLocalNameResolver writes the resolver-config that activates the
// local-name backend in the meta-resolver chain (REGISTRY §4). After
// this, ResolveName consults local-name bindings. Idempotent.
//
// Since 2026-08-18 it writes the **§4.1a default `name_format_dispatch`
// list** as well as the chain entry, via DefaultResolverConfig. It used
// to write the chain alone. That was harmless only by accident: with no
// dispatch list every name consults every backend in the chain, and the
// chain happened to be local-only — so the config was one remote
// backend away from disclosing every unscoped name a user types, and
// nothing in it said so. The default list makes the catch-all's
// local-only MUST explicit in the artifact rather than implicit in what
// the chain currently happens to contain.
//
// A distribution shipping pinned bindings or a peer-issued chain entry
// builds on DefaultResolverConfig and installs it with
// InstallResolverConfig, which enforces the same MUST.
func (a *AppPeer) EnableLocalNameResolver() error {
	return a.InstallResolverConfig(DefaultResolverConfig())
}

// BrowseFetch composes the lower rungs of the resolution chain
// (PROPOSAL-UNIVERSAL-RESOLUTION §3: browse_fetch = resolve(PeerId) ▸
// resolve(TreePath) ▸ resolve(ContentHash)). Given an already-reachable
// peer — Connect must have established the transport; the reach rung
// leans on Connect(addr) today, the full Layer-B ladder being core-side
// and partial — it resolves the tree path to a content entity (tree:get
// mode=entity does path→hash→content in one dispatch, with substitute
// firing server-side) and returns it, or a typed Outcome at whichever
// rung stops it.
//
// peerID == this peer's own id resolves locally (the self rung).
func (a *AppPeer) BrowseFetch(peerID, path string) (entity.Entity, error) {
	qualified := "/" + peerID + "/" + path
	ent, found, err := a.Get(qualified)
	if err != nil {
		if o := classifyError(RungPath, err); o != nil {
			return entity.Entity{}, o
		}
		// A remote dispatch that isn't a clean 404/403/503 means we
		// couldn't reach the peer (no pooled connection / dial failed):
		// the transport rung is exhausted (F1 reachability).
		return entity.Entity{}, &Outcome{
			Kind:   OutcomeUnreachable,
			Rung:   RungTransport,
			PeerID: peerID,
			Err:    err,
		}
	}
	if !found {
		return entity.Entity{}, &Outcome{Kind: OutcomeNotFound, Rung: RungPath}
	}
	return ent, nil
}
