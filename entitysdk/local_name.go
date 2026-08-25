package entitysdk

import (
	"fmt"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
)

// local_name.go — the management half of the local-name backend
// (EXTENSION-REGISTRY §6.5).
//
// `BindLocalName` lives in resolve_chain.go because it is a rung of the
// resolution chain: it is how a name comes to resolve at all. The three
// operations here are not rungs — they are the name book's upkeep, and
// they exist because a binding a user cannot list, correct, or remove is
// a binding they will not trust enough to make.
//
// The kernel has declared all four ops since the local-name handler
// landed (`ext/registry/localname.Handler.Manifest`: bind, unbind, list,
// update-transports). Only bind had an SDK surface, which is the shape
// D20 warns about from the other side — the absence was in `entitysdk/`,
// never in the substrate, and pricing the work off our own tree would
// have budgeted for a handler that was already written.
//
// All four are peer-local by construction: §6.5's bindings never leave
// the peer that wrote them, so these dispatch to our own handler and
// carry no remote-reachability failure mode. They still go through the
// executor rather than the store — protocol-first, so the handler's
// capability gate and supersedes-chain bookkeeping run.

// LocalNameBinding is one row of the local name book, as `:list` reports
// it (§6.5). Hash is the binding entity's content hash — the value
// `UpdateLocalNameTransports` supersedes and the one a pin is taken
// against, so it is surfaced rather than hidden behind the name.
type LocalNameBinding struct {
	Name         string
	TargetPeerID string
	Hash         hash.Hash
	Notes        string
	Pinned       bool
}

// BindOption customizes a local-name bind. Variadic rather than extra
// parameters because `BindLocalName`'s three-arg form is the common case
// and is already load-bearing in the resolution tests; a fourth
// positional arg would churn every caller to pass nil.
type BindOption func(*types.LocalNameBindRequestData)

// WithNotes attaches an operator note to a binding. §6.5 carries the
// field and the kernel round-trips it into `:list`; it is the only place
// a user can record *why* a name points where it does, which is the
// question a stale name book cannot otherwise answer.
func WithNotes(notes string) BindOption {
	return func(r *types.LocalNameBindRequestData) {
		if notes != "" {
			r.Notes = &notes
		}
	}
}

// ListLocalNames returns every local-name binding this peer holds
// (§6.5 `:list`). The result is the peer's own name book — never
// another peer's, and never a merged view: local-name bindings are not
// published and not synced.
//
// An empty book is (nil, nil), not an error. "No names bound yet" is the
// state every peer starts in, and a caller rendering a name list needs
// to tell it apart from a failure.
func (a *AppPeer) ListLocalNames() ([]LocalNameBinding, error) {
	reqEnt, err := types.LocalNameListRequestData{}.ToEntity()
	if err != nil {
		return nil, WrapError(500, "encode_request", "encode local-name list request", err)
	}
	resp, err := a.executor.ExecuteWithParams(localNameHandlerURI, "list", reqEnt)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Status >= 400 {
		if e := ErrorFromResponse(resp); e != nil {
			return nil, e
		}
		return nil, NewError(500, "list_failed", "system/registry/local-name:list failed")
	}
	res, err := types.LocalNameListResultDataFromEntity(resp.Entity())
	if err != nil {
		return nil, WrapError(500, "decode_failed", "decode local-name list result", err)
	}
	out := make([]LocalNameBinding, 0, len(res.Entries))
	for _, e := range res.Entries {
		b := LocalNameBinding{
			Name:         e.Name,
			TargetPeerID: e.TargetPeerID,
			Hash:         e.Hash,
			Pinned:       e.Pinned,
		}
		if e.Notes != nil {
			b.Notes = *e.Notes
		}
		out = append(out, b)
	}
	return out, nil
}

// UnbindLocalName removes a binding (§6.5 `:unbind`). After it returns,
// `ResolveName(name)` fails closed at the name rung exactly as it did
// before the name was ever bound.
//
// **Unbinding a name that was never bound reports SUCCESS** — measured,
// not assumed. The kernel's `Unbind` is a `TreeRemove` of the pointer
// path (`ext/registry/localname.Handler.Unbind`), and `TreeRemove` on an
// absent path is a no-op, so the operation is idempotent. §6.5 does not
// rule either way, and idempotent-delete is a defensible reading; what
// it costs is that a user who typos a name during cleanup is told the
// name is gone rather than that it was never there.
//
// We do NOT paper over that with a local existence pre-check. Reading
// the book before the delete would invent a semantic the protocol does
// not declare, would differ from every other impl's answer to the same
// dispatch, and would race. The ambiguity is routed to arch instead; the
// `name` verb spends one extra `:list` to give the user the better
// message, which is a UI choice at the UI tier and does not change what
// the SDK reports. Pinned by
// TestLocalName_UnbindUnknownNameIsIdempotent.
func (a *AppPeer) UnbindLocalName(name string) error {
	if name == "" {
		return NewError(400, "invalid_name", "UnbindLocalName requires a non-empty name")
	}
	reqEnt, err := types.LocalNameUnbindRequestData{Name: name}.ToEntity()
	if err != nil {
		return WrapError(500, "encode_request", "encode local-name unbind request", err)
	}
	resp, err := a.executor.ExecuteWithParams(localNameHandlerURI, "unbind", reqEnt)
	if err != nil {
		return err
	}
	if resp == nil || resp.Status >= 400 {
		if e := ErrorFromResponse(resp); e != nil {
			return e
		}
		return NewError(500, "unbind_failed",
			fmt.Sprintf("system/registry/local-name:unbind %q failed", name))
	}
	return nil
}

// UpdateLocalNameTransports re-points a binding's transports without
// disturbing the name→peer_id association (§6.5 `:update-transports`).
// The kernel issues a NEW binding carrying `supersedes = <old hash>`
// rather than mutating in place, and returns the successor's hash.
//
// This is the operation a moved peer needs: the identity is unchanged —
// re-binding from scratch would be the wrong verb, since it discards the
// supersedes chain that lets a holder of the old hash see what replaced
// it.
func (a *AppPeer) UpdateLocalNameTransports(name string, transports []hash.Hash) (hash.Hash, error) {
	if name == "" {
		return hash.Hash{}, NewError(400, "invalid_name", "UpdateLocalNameTransports requires a non-empty name")
	}
	reqEnt, err := types.LocalNameUpdateTransportsRequestData{
		Name:       name,
		Transports: transports,
	}.ToEntity()
	if err != nil {
		return hash.Hash{}, WrapError(500, "encode_request", "encode local-name update-transports request", err)
	}
	resp, err := a.executor.ExecuteWithParams(localNameHandlerURI, "update-transports", reqEnt)
	if err != nil {
		return hash.Hash{}, err
	}
	if resp == nil || resp.Status >= 400 {
		if e := ErrorFromResponse(resp); e != nil {
			return hash.Hash{}, e
		}
		return hash.Hash{}, NewError(500, "update_transports_failed",
			fmt.Sprintf("system/registry/local-name:update-transports %q failed", name))
	}
	res, err := types.LocalNameBindResultDataFromEntity(resp.Entity())
	if err != nil {
		return hash.Hash{}, WrapError(500, "decode_failed", "decode update-transports result", err)
	}
	return res.BindingHash, nil
}
