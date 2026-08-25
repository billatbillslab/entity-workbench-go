package entitysdk

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/entity"
	ecerrors "go.entitychurch.org/entity-core-go/core/errors"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/protocol"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/types"
)

// remoteExecuteFunc matches the signature of protocol.Dispatcher.RemoteExecute.
// When set on an Executor, it is invoked for handler URIs whose peer-id does
// not match the local peer — routing the EXECUTE through the dispatcher's
// remote path (which uses the local peer's pooled connections, dialing on
// demand if RegisterRemote has seeded an address).
//
// Unexported: this is internal wiring between assembleAppPeer and Executor;
// callers see *Response at the public boundary, never *handler.Response.
type remoteExecuteFunc func(ctx context.Context, uri, operation string, params entity.Entity, resource *types.ResourceTarget, async ...*protocol.AsyncDelivery) (*handler.Response, error)

// localExecuteFunc matches the signature of protocol.Dispatcher.DispatchLocalExecute.
// When set, it is invoked for handler URIs whose peer-id matches the local peer
// (or no peer-id), routing the EXECUTE through the dispatcher's V7 §6.6
// tree-walk path. This is the spec-correct local dispatch path — it reaches
// entity-native handlers (system/handler with expression_path), not just the
// in-memory registry's compiled-handler entries.
//
// Standalone Executors built without an AppPeer leave this nil and fall back
// to a registry-only path that handles compiled handlers but cannot resolve
// entity-native handlers stored in the tree.
type localExecuteFunc func(ctx context.Context, req protocol.LocalExecuteRequest) (*handler.Response, error)

// DispatchFunc is the signature for executing handler operations.
// Panels receive this as a callback — they don't need to know about
// handler.Registry or handler.Request.
type DispatchFunc func(path, operation string) (*Response, error)

// Executor provides protocol-level access to a peer.
//
// Applications use Executor methods instead of accessing store or
// location index directly. Writes go through handler dispatch
// (capability checks, emits, handler logic). Reads use efficient
// internal access since they don't change state.
//
// The internal fields are unexported — applications see only the
// method API, never the underlying store/index.
type Executor struct {
	registry      *handler.Registry
	store         store.ContentStore
	locationIndex store.LocationIndex
	peerID        crypto.PeerID
	authorHash    hash.Hash
	timeout       time.Duration
	log           *EventLog // optional — logs operations when set

	// remoteExecute, when non-nil, routes EXECUTEs whose handler URI names a
	// non-local peer-id through the dispatcher's remote path. Wired by
	// assembleAppPeer to peer.Dispatcher.RemoteExecute. Nil for executors
	// constructed without an enclosing AppPeer (tests / standalone use) —
	// such executors silently fall back to local dispatch and will return a
	// "not found" handler error if the URI references a remote peer-id.
	remoteExecute remoteExecuteFunc

	// localExecute, when non-nil, routes local EXECUTEs through the
	// dispatcher's V7 §6.6 tree-walk path (protocol.Dispatcher.DispatchLocalExecute).
	// Wired by assembleAppPeer. This is the spec-correct local dispatch path
	// that reaches entity-native handlers (system/handler entities with
	// expression_path), not just registry-resident compiled handlers. Nil
	// for standalone Executors built without an AppPeer; those fall back
	// to registry.Dispatch, which handles compiled handlers only.
	localExecute localExecuteFunc

	// callerCap is the peer-owner self-capability stamped onto every
	// local-dispatch HandlerContext as CallerCapability. Required for
	// handlers that enforce RL2 (system/role:define, :assign, etc.) —
	// without it, the handler returns 403 caller_capability_missing.
	// In open-grants Level 0 mode this is a wildcard self-grant.
	// Nil-valued (zero entity) is permitted: handlers that don't
	// enforce RL2 simply ignore it.
	callerCap entity.Entity
}

// NewExecutor creates an Executor for a peer.
func NewExecutor(registry *handler.Registry, s store.ContentStore, li store.LocationIndex, peerID crypto.PeerID) *Executor {
	return &Executor{
		registry:      registry,
		store:         s,
		locationIndex: li,
		peerID:        peerID,
	}
}

// PeerID returns the executor's bound peer id as a string. Renderers
// that build peer-bound models (e.g. wb.NewLocalTreeResolver) reach it
// via PeerContext.Executor().PeerID().
func (ex *Executor) PeerID() string {
	return string(ex.peerID)
}

// SetLog attaches an event log for operation tracing.
func (ex *Executor) SetLog(log *EventLog) {
	ex.log = log
}

// setRemoteExecute installs the callback used to dispatch EXECUTEs whose
// handler URI names a non-local peer-id. Wired by assembleAppPeer to
// peer.Dispatcher().RemoteExecute. Internal: the *handler.Response in the
// signature is the boundary type from core-go; the public Execute*
// surface translates to *Response before returning.
func (ex *Executor) setRemoteExecute(fn remoteExecuteFunc) {
	ex.remoteExecute = fn
}

// setLocalExecute installs the callback used to dispatch EXECUTEs whose
// handler URI names the local peer (or no peer-id), routing through the
// dispatcher's V7 §6.6 tree-walk path. Wired by assembleAppPeer to
// peer.Dispatcher().DispatchLocalExecute.
func (ex *Executor) setLocalExecute(fn localExecuteFunc) {
	ex.localExecute = fn
}

// SetAuthorHash sets the local peer's identity entity content hash. The
// executor stamps it onto every dispatched HandlerContext as AuthorHash so
// handlers performing creator-authorization checks (e.g. subscription's R1
// chain-root walk) see the SDK as the request author. Without this, those
// handlers see a zero hash and reject the request 403.
func (ex *Executor) SetAuthorHash(h hash.Hash) {
	ex.authorHash = h
}

// SetCallerCapability sets the peer-owner self-capability the executor
// stamps onto every local-dispatch HandlerContext as CallerCapability.
// Required by handlers enforcing RL2 (system/role:define, :assign, etc.).
// In open-grants Level 0 mode this is a wildcard self-cap minted at
// AppPeer construction.
func (ex *Executor) SetCallerCapability(cap entity.Entity) {
	ex.callerCap = cap
}

// Execute dispatches a handler operation through the protocol.
// Handlers fire, capability grants are checked, emits propagate.
func (ex *Executor) Execute(path, operation string) (*Response, error) {
	return ex.executeDispatch(path, operation, entity.Entity{}, nil, nil, entity.Entity{})
}

// ExecuteWithParams dispatches a handler operation with parameters.
// The params entity is passed to the handler as Request.Params.
func (ex *Executor) ExecuteWithParams(path, operation string, params entity.Entity) (*Response, error) {
	return ex.executeDispatch(path, operation, params, nil, nil, entity.Entity{})
}

// ExecuteOnResource dispatches a handler operation that targets a
// specific resource path (like system/tree's get/put, where the path
// being accessed lives in Resource.Targets[0] rather than in the
// handler-target path). Used internally by the L1 tree wrappers.
func (ex *Executor) ExecuteOnResource(handlerPath, operation string, params entity.Entity, resource *types.ResourceTarget) (*Response, error) {
	return ex.executeDispatch(handlerPath, operation, params, resource, nil, entity.Entity{})
}

// ExecuteWithIncluded dispatches an operation whose handler needs
// additional entities (capability tokens, signatures, identity) in the
// HandlerContext.Included map — the shape subscribe uses to present its
// delivery token. Used internally by the subscription bridge and any
// future extension wrapper that carries an Included chain.
func (ex *Executor) ExecuteWithIncluded(handlerPath, operation string, params entity.Entity, resource *types.ResourceTarget, included map[hash.Hash]entity.Entity) (*Response, error) {
	return ex.executeDispatch(handlerPath, operation, params, resource, included, entity.Entity{})
}

// executeAs dispatches under an explicit caller capability instead of
// the executor's standing owner self-cap.
//
// It exists because the owner self-cap cannot express authority over
// another peer's namespace. Its resources are `["*"]`, and under §PR-8
// (V7 §5.5) a bare `*` in a capability RESOURCE is peer-LOCAL — it
// canonicalizes to `/{me}/*`. That is correct and deliberate: it is
// what stops a self-issued grant from claiming the world. But a peer
// writing into its own cached copy of someone else's namespace
// (`/{them}/…`, V7 §1.4 layer 1: "full write access to its entire
// local tree") is asking for exactly the authority that phrasing
// forbids expressing, so the caller has to mint a cap that names the
// namespace and hand it in for that one dispatch.
//
// Per-call rather than a setter: ex.callerCap is process-wide state on
// a shared executor, and swapping it around a dispatch would leak the
// broader authority to anything running concurrently.
func (ex *Executor) executeAs(callerCap entity.Entity, handlerPath, operation string, params entity.Entity, resource *types.ResourceTarget) (*Response, error) {
	return ex.executeDispatch(handlerPath, operation, params, resource, nil, callerCap)
}

func (ex *Executor) executeDispatch(path, operation string, params entity.Entity, resource *types.ResourceTarget, included map[hash.Hash]entity.Entity, callerCap entity.Entity) (*Response, error) {
	effectiveCap := ex.callerCap
	if !callerCap.ContentHash.IsZero() {
		effectiveCap = callerCap
	}
	timeout := ex.timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if ex.log != nil {
		ex.log.Verbosef("exec %s %s", path, operation)
	}

	var raw *handler.Response
	var err error
	if ex.isRemote(path) {
		if ex.remoteExecute == nil {
			return nil, NewError(501, "remote_unavailable",
				"executor has no remote dispatch wired (URI "+path+" targets a non-local peer)")
		}
		// Pack any caller-supplied Included entities as AsyncDelivery.Extras
		// so they cross the wire and land in the receiving handler's
		// hctx.Included. Required for cross-peer subscribe (deliver_token)
		// and any handler reading params-referenced entities from Included.
		var async []*protocol.AsyncDelivery
		if len(included) > 0 {
			async = []*protocol.AsyncDelivery{{Extras: included}}
		}
		raw, err = ex.remoteExecute(ctx, path, operation, params, resource, async...)
	} else if ex.localExecute != nil {
		// V7 §6.6 spec-correct path: route through the dispatcher's
		// tree-walk so entity-native handlers (system/handler entities
		// with expression_path) resolve identically to over-the-wire
		// dispatch. The dispatcher builds the HandlerContext (including
		// hctx.Execute for handler-internal sub-dispatch), so this branch
		// no longer needs to construct one here. Wired by assembleAppPeer.
		raw, err = ex.localExecute(ctx, protocol.LocalExecuteRequest{
			URI:              path,
			Operation:        operation,
			Params:           params,
			Resource:         resource,
			CallerCapability: effectiveCap,
			Author:           ex.peerID,
			AuthorHash:       ex.authorHash,
			Included:         included,
		})
	} else {
		// Standalone fallback for Executors built without an AppPeer
		// (resolve_test.go-style construction). Registry-only — entity-
		// native handlers are unreachable on this path because the tree
		// is not consulted. Documented limitation.
		hctx := &handler.HandlerContext{
			Store:            ex.store,
			LocationIndex:    ex.locationIndex,
			LocalPeerID:      ex.peerID,
			Author:           ex.peerID,
			AuthorHash:       ex.authorHash,
			CallerCapability: effectiveCap,
			Resource:         resource,
			Included:         included,
		}
		hctx.Execute = func(subCtx context.Context, uri, op string, p entity.Entity, opts ...handler.ExecuteOption) (*handler.Response, error) {
			o := handler.ApplyOpts(opts)
			subResource := resource
			if o.Resource != nil {
				subResource = o.Resource
			}
			if ex.isRemote(uri) {
				if ex.remoteExecute == nil {
					return handler.NewErrorResponse(501, "remote_unavailable",
						"executor has no remote dispatch wired (URI "+uri+" targets a non-local peer)")
				}
				return ex.remoteExecute(subCtx, uri, op, p, subResource)
			}
			subReq := &handler.Request{
				Path:      entity.ExtractHandlerPath(uri),
				Operation: op,
				Params:    p,
				Context: &handler.HandlerContext{
					Store:            ex.store,
					LocationIndex:    ex.locationIndex,
					LocalPeerID:      ex.peerID,
					Author:           ex.peerID,
					AuthorHash:       ex.authorHash,
					CallerCapability: effectiveCap,
					Resource:         subResource,
				},
			}
			return ex.registry.Dispatch(subCtx, subReq)
		}
		req := &handler.Request{
			Path:      entity.ExtractHandlerPath(path),
			Operation: operation,
			Params:    params,
			Context:   hctx,
		}
		raw, err = ex.registry.Dispatch(ctx, req)
	}

	if err != nil {
		if ex.log != nil {
			ex.log.Verbosef("exec %s %s → error: %s", path, operation, err)
		}
		// Map registry not-found errors to 404 so callers can use
		// IsNotFound consistently. Other dispatch failures stay 500.
		if errors.Is(err, ecerrors.ErrNotFound) {
			return nil, WrapError(404, "handler_not_found",
				fmt.Sprintf("no handler for %s", path), err)
		}
		return nil, WrapError(500, "dispatch_failed",
			fmt.Sprintf("execute %s %s: %s", path, operation, err), err)
	}

	resp := responseFromHandler(raw)
	if ex.log != nil {
		ex.log.Verbosef("exec %s %s → %d %s", path, operation, resp.Status, resp.Type)
		if ex.log.Level() >= LogDebug && len(resp.Data) > 0 {
			ex.logEntitySummary("  result", resp.Entity())
		}
	}
	if resp.Status >= 400 {
		return nil, ErrorFromResponse(resp)
	}
	return resp, nil
}

// isRemote reports whether the handler URI names a peer-id different from
// this executor's local peer. Bare paths and local URIs return false.
func (ex *Executor) isRemote(uri string) bool {
	parsed, err := entity.ParseURI(uri)
	if err != nil {
		return false
	}
	return parsed.PeerID != "" && parsed.PeerID != string(ex.peerID)
}

// EntityCount returns the number of entities in the content store.
func (ex *Executor) EntityCount() int {
	return ex.store.Len()
}

// logEntitySummary logs a decoded summary of an entity's data.
func (ex *Executor) logEntitySummary(prefix string, ent entity.Entity) {
	if ex.log == nil {
		return
	}
	decoded, ok := DecodeEntityData(ent.Data)
	if !ok {
		ex.log.Debugf("%s [%s] %d bytes (decode failed)", prefix, ent.Type, len(ent.Data))
		return
	}
	lines := FormatCBOR(decoded)
	if len(lines) == 0 {
		ex.log.Debugf("%s [%s] (empty)", prefix, ent.Type)
		return
	}
	// Show first few fields inline
	summary := RenderPlainText(lines)
	if len(summary) > 200 {
		summary = summary[:197] + "..."
	}
	// Trim trailing newline for clean log output
	for len(summary) > 0 && summary[len(summary)-1] == '\n' {
		summary = summary[:len(summary)-1]
	}
	ex.log.Debugf("%s [%s] %s", prefix, ent.Type, summary)
}
