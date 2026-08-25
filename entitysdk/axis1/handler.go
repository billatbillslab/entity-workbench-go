package axis1

import (
	"context"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/compute"
)

// Handler serves compute:eval on the Axis-1 engine.
//
// It exists for ONE reason: to make the whole descriptor-faithful tick
// measurable end to end. The engine benchmarks evaluate the step directly,
// which is an honest engine-to-engine ratio but is NOT the quantity POC §2
// reported (gens/s through executor → dispatch → handler → eval → decode →
// put). Amdahl says those differ a lot once eval stops dominating, and "a 71x
// eval speedup is not a 71x tick speedup" is a claim that needs measuring, not
// asserting.
//
// Registered at a NON-system pattern (see LifeTickPattern in the tests) so it
// sits beside system/compute rather than replacing it. Both handlers stay live
// in the same peer, so a single test can tick the same program through either
// engine over an otherwise byte-identical protocol path — the only difference
// downstream of ExecuteOnResource is which evaluator runs.
//
// Deliberately eval-only. install/uninstall are the reactive subgraph surface
// (engine.OnTreeChange, dependency indices, cascade limits) and are far outside
// the prototype fence; they answer 501 rather than pretending.
//
// This is a transcription of ext/compute/handler.go::handleEval @ core-go
// 769a888, with the evaluator swapped. Everything it re-derives —
// looksLikeExpressionPath, initBudget, wrapResult, makeEvalContext — is
// unexported there. Re-diff against that function when it moves; the ordering
// of its checks is part of the observable contract (which 4xx you get for which
// bad request).
type Handler struct {
	eng     *Engine
	pattern string
}

// NewHandler builds an Axis-1-backed eval handler bound to a pattern.
func NewHandler(eng *Engine, pattern string) *Handler {
	return &Handler{eng: eng, pattern: pattern}
}

func (h *Handler) Name() string { return "axis1-compute" }

func (h *Handler) Manifest() types.HandlerManifestData {
	return types.HandlerManifestData{
		Pattern: h.pattern,
		Name:    "axis1-compute",
		Operations: map[string]types.HandlerOperationSpec{
			"eval": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}
}

// RegisterTypes is a no-op: compute's types are registered by RegisterCoreTypes,
// and re-running ReflectType here would clobber the path-typed field overrides
// applied there — the same reason ext/compute's handler declines.
func (h *Handler) RegisterTypes(r *types.TypeRegistry) {}

func (h *Handler) Handle(ctx context.Context, req *handler.Request) (*handler.Response, error) {
	if req.Operation != "eval" {
		return handler.NewErrorResponse(400, "unknown_operation",
			"axis1-compute supports eval only, got: "+req.Operation)
	}
	return h.handleEval(ctx, req)
}

func (h *Handler) handleEval(ctx context.Context, req *handler.Request) (*handler.Response, error) {
	hctx := req.Context

	// V7 §3.2 path-as-resource: the expression path is EXECUTE.resource.targets[0].
	if hctx.Resource == nil || len(hctx.Resource.Targets) != 1 {
		return handler.NewErrorResponse(400, "ambiguous_resource",
			"eval requires exactly one resource target (the expression path)")
	}
	exprPath := hctx.Resource.Targets[0]
	if exprPath == hctx.HandlerPattern ||
		exprPath == "/"+string(hctx.LocalPeerID)+"/"+hctx.HandlerPattern {
		return handler.NewErrorResponse(400, "ambiguous_resource",
			"eval resource must target an expression path, not the handler pattern")
	}

	exprHash, ok := hctx.LocationIndex.Get(exprPath)
	if !ok {
		return handler.NewErrorResponse(404, compute.ErrNotFound, "No entity at path: "+exprPath)
	}
	expression, ok := hctx.Store.Get(exprHash)
	if !ok {
		return handler.NewErrorResponse(404, compute.ErrNotFound, "No entity at path: "+exprPath)
	}
	if !compute.IsComputeExpression(expression) {
		return handler.NewErrorResponse(400, compute.ErrInvalidExpression,
			"Entity at path is not a compute expression: "+expression.Type)
	}

	budget := initBudget(hctx)
	evalCtx := h.makeEvalContext(hctx)
	evalCtx.SubgraphRoot = exprPath

	result, err := h.eng.Evaluate(expression, nil, budget, evalCtx)
	if err != nil {
		if ce, ok := err.(*compute.ComputeError); ok {
			errEnt, entErr := ce.ToEntity()
			if entErr != nil {
				return handler.NewErrorResponse(500, "internal", "Failed to create error entity")
			}
			// F10: an evaluated compute/error is a VALUE (§1.5 error-as-value),
			// surfaced at 200 with the entity as the result. 4xx is reserved for
			// dispatch failures. Getting this wrong would change the status code
			// a budget_exhausted tick reports — which is exactly what the budget
			// cliff sweep reads.
			return &handler.Response{Status: 200, Result: errEnt}, nil
		}
		return handler.NewErrorResponse(500, "internal", err.Error())
	}

	// v3.19c Part A M3 boundary 1: materialize before crossing compute →
	// non-compute. This is THE boundary the contract binds (§13.5).
	result, err = materialize(result, hctx.Store)
	if err != nil {
		return handler.NewErrorResponse(500, "internal", "Failed to materialize result: "+err.Error())
	}

	resultEnt, err := wrapResult(result, expression.ContentHash)
	if err != nil {
		return handler.NewErrorResponse(500, "internal", "Failed to wrap result: "+err.Error())
	}
	return &handler.Response{Status: 200, Result: resultEnt}, nil
}

// makeEvalContext mirrors ext/compute/handler.go::makeEvalContext.
//
// For explicit eval, Capability and CallerCapability are the same thing — the
// external caller's grant both authorizes the eval and is the chain initiator
// (PROPOSAL-ENTITY-NATIVE-HANDLER-DISPATCH §6.1).
//
// RegisterDep is nil: dependency tracking exists for the reactive subgraph, and
// this handler is explicit-eval only. A host-clocked tick loop is the §5
// contract (never a reactive install on state), so nothing here should be
// registering deps.
func (h *Handler) makeEvalContext(hctx *handler.HandlerContext) *compute.EvalContext {
	return &compute.EvalContext{
		ContentStore:     hctx.Store,
		LocationIndex:    hctx.LocationIndex,
		LocalPeerID:      string(hctx.LocalPeerID),
		Capability:       hctx.CallerCapability,
		CallerCapability: hctx.CallerCapability,
		Author:           hctx.Author,
		Included:         hctx.Included,
		DispatchExecute: func(path, op string, resource *types.ResourceTarget, params entity.Entity, override *entity.Entity) (*handler.Response, error) {
			cap := hctx.CallerCapability
			if override != nil {
				cap = *override
			}
			// F4: a dispatched EXECUTE carries the apply's resource. With no
			// resource field we still pass an explicit empty ResourceTarget so
			// the dispatcher does NOT inherit the parent execute's resource —
			// inheritance would point back at the compute expression itself and
			// recurse.
			dispatchResource := resource
			if dispatchResource == nil {
				dispatchResource = &types.ResourceTarget{}
			}
			return hctx.Execute(context.Background(), path, op, params,
				handler.WithCapability(cap),
				handler.WithResource(dispatchResource),
			)
		},
	}
}

// initBudget mirrors ext/compute/handler.go::initBudget.
//
// Transcribed rather than approximated because the budget is what the cliff
// sweep measures. Note both clamps are LOWER-ONLY: Bounds.Budget applies only
// when < DefaultMaxOps, and a capability constraint only when < ops. That is
// the spec/impl divergence arch flagged — EXTENSION-COMPUTE §9.3 reads as
// though a grant's max_compute_operations SETS the limit (and could raise it),
// while core-go treats 100k as a ceiling nothing can lift. Reproduced here
// faithfully: this engine must not quietly acquire a raise surface the
// reference lacks, or the cliff comparison would be measuring our deviation
// instead of the cliff.
func initBudget(hctx *handler.HandlerContext) *compute.Budget {
	ops := compute.DefaultMaxOps
	depth := compute.DefaultMaxDepth

	if hctx.Bounds != nil && hctx.Bounds.Budget != nil && *hctx.Bounds.Budget < uint64(ops) {
		ops = int(*hctx.Bounds.Budget)
	}
	if !hctx.CallerCapability.ContentHash.IsZero() {
		capOps, capDepth := extractComputeConstraints(hctx.CallerCapability)
		if capOps > 0 && capOps < ops {
			ops = capOps
		}
		if capDepth > 0 && capDepth < depth {
			depth = capDepth
		}
	}
	return compute.NewBudget(ops, depth)
}

// extractComputeConstraints mirrors ext/compute/handler.go::extractComputeConstraints.
// The constraints field is an open type preserved as raw CBOR on the entity, so
// it decodes from entity data directly.
//
// DRIFT, KNOWN AND UNFIXED — do not read this as a current mirror. core-go
// replaced this reader on 2026-08-22 (`0e34e3e`, "§5.2 depth budget is a
// grant-level cap constraint"): EXTENSION-COMPUTE §5.2 sources the limits from
// the matching GRANT's constraints["system/compute"], and a capability token
// carries no top-level `constraints` field at all, so reading cap.Data
// ["constraints"] means no compute constraint has ever reached this evaluator.
// Their replacements are computeConstraintsOfGrant / computeConstraintsOfToken.
//
// Left as-is deliberately this session: Axis-1 is registered by tests only (no
// binary, bridge or panel reaches it — `make reachability`), so this changes
// nothing a user runs, and adopting it is engine-semantics work that belongs
// with the v3.26 contained-error adoption rather than ahead of a release. It is
// row 2's neighbour in the post-release backlog, tracked in STATUS §0b. What
// the comment must NOT do meanwhile is keep claiming a fidelity that lapsed —
// the transcription is the contract, and this one is now stale.
func extractComputeConstraints(cap entity.Entity) (ops, depth int) {
	var rawData map[string]interface{}
	if err := ecf.Decode(cap.Data, &rawData); err != nil {
		return 0, 0
	}
	constraints, ok := rawData["constraints"]
	if !ok {
		return 0, 0
	}
	constraintsMap := toStringMap(constraints)
	if constraintsMap == nil {
		return 0, 0
	}
	computeVal, ok := constraintsMap["system/compute"]
	if !ok {
		return 0, 0
	}
	computeMap := toStringMap(computeVal)
	if computeMap == nil {
		return 0, 0
	}
	if v, ok := computeMap["max_compute_operations"]; ok {
		ops = toIntValue(v)
	}
	if v, ok := computeMap["max_compute_depth"]; ok {
		depth = toIntValue(v)
	}
	return ops, depth
}

// toIntValue mirrors ext/compute/handler.go::toIntValue.
func toIntValue(v interface{}) int {
	switch n := v.(type) {
	case uint64:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// wrapResult mirrors ext/compute/handler.go::wrapResult — an entity result
// passes through bare; anything else is wrapped in a compute/result carrying
// the expression hash.
func wrapResult(result interface{}, expressionHash hash.Hash) (entity.Entity, error) {
	if ent, ok := result.(entity.Entity); ok {
		return ent, nil
	}
	d := types.ComputeResultData{
		Value:      result,
		Expression: expressionHash,
	}
	return d.ToEntity()
}
