package entitysdk

// S6 — InstallComputeSubgraph. The reactive counterpart of
// RegisterComputeHandler. Per Q1 (PROPOSAL-COMPUTE-RECURSION-AND-SUM-TYPES
// / SDK-EXTENSION-OPERATIONS
// E7) shipped now in workbench-go; appendix carries the shape cross-impl.
//
// Mirrors RegisterComputeHandler:
//
//   1. Build the expression — writes the IR tree at a stable
//      per-subgraph sub-path; root hash captured.
//   2. Dispatch system/compute:install (NOT tree:put on a subgraph
//      manifest — kernel-vs-handler principle SDK-OPERATIONS §11.2A:
//      the install op creates the subgraph entity + grant + registers
//      dependencies atomically).
//   3. Return a *SubgraphHandle wrapping the install result + a
//      typed close that dispatches system/compute:uninstall.
//
// What's reactive: when any tree path the expression LookupTree's
// gets mutated, the engine's OnTreeChange sync hook fires and
// reEvaluate writes the new result to result_path.

import (
	"context"
	"strconv"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/types"
)

// SubgraphHandle is the typed handle returned by InstallComputeSubgraph.
// Closing the handle dispatches system/compute:uninstall — removing the
// subgraph from the engine's reactive dependency index. Idempotent.
type SubgraphHandle struct {
	subgraphPath string
	resultPath   string
	ap           *AppPeer
	closed       bool

	// ImpureOperations is the audit result from install: which paths
	// were registered as read deps, which handlers are dispatched,
	// where writes go. Useful for diagnostics. Verbatim from the
	// install result entity (typically a map[interface{}]interface{}
	// with read_paths / handler_targets / write_paths keys).
	ImpureOperations interface{}
}

// ReadResult fetches the entity currently bound at ResultPath and
// returns its unwrapped value. The engine writes results as the SA-4
// compute/result envelope `{value, expression}`; this helper peels
// the envelope so callers get the inner value directly.
//
// Returns (nil, false, nil) if no entity is bound at result_path
// (the install registered deps but no mutation has triggered eval
// yet — the engine doesn't write result_path eagerly).
//
// Errors only on store/decode failures.
func (h *SubgraphHandle) ReadResult() (value interface{}, found bool, err error) {
	if h == nil || h.ap == nil {
		return nil, false, nil
	}
	ent, ok, err := h.ap.Get(h.resultPath)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	// SA-4 envelope: {value, expression}. Decode as a map, take .value.
	envelope, mapErr := decodeAsStringKeyMap(ent.Data, "SubgraphHandle.ReadResult")
	if mapErr == nil {
		if v, ok := envelope["value"]; ok {
			return v, true, nil
		}
	}
	// Fallback: bare value (some shapes / future versions may write bare).
	var bare interface{}
	if decErr := ecf.Decode(ent.Data, &bare); decErr != nil {
		return nil, true, decErr
	}
	return bare, true, nil
}

// SubgraphPath returns the path under system/compute/processes/ where
// the subgraph metadata entity is bound. Diagnostics + uninstall use.
func (h *SubgraphHandle) SubgraphPath() string { return h.subgraphPath }

// ResultPath returns where the engine writes the evaluated result.
// Callers subscribe / Get here to observe the latest value.
func (h *SubgraphHandle) ResultPath() string { return h.resultPath }

// Close dispatches system/compute:uninstall, removing the subgraph
// from the reactive index. Safe to call multiple times (idempotent).
// Returns an error only if the uninstall dispatch failed.
func (h *SubgraphHandle) Close() error {
	if h == nil || h.closed {
		return nil
	}
	h.closed = true
	if h.ap == nil {
		return nil
	}

	// system/compute:uninstall takes no params; resource = subgraph path.
	empty, err := PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return WrapError(500, "uninstall_encode_failed", "uninstall params encode", err)
	}
	_, err = h.ap.Executor().ExecuteOnResource(
		"system/compute", "uninstall", empty,
		&types.ResourceTarget{Targets: []string{h.subgraphPath}},
	)
	return err
}

// InstallComputeSubgraph installs a reactive compute subgraph. The
// expression DAG rooted at expr is materialized at a stable
// sub-path; install dispatches system/compute:install with that path
// as the resource target.
//
// On success, the engine has registered the expression's tree
// dependencies. When any registered dep is mutated via tree:put, the
// engine's sync hook fires and reEvaluate writes the new value to
// resultPath. Callers observe live updates via Get on resultPath or
// (better) system/subscription.
//
//	expr := c.Arithmetic("add", sizeOf(a), c.Arithmetic("add", sizeOf(b), sizeOf(c)))
//	h, err := ap.InstallComputeSubgraph(ctx, expr, "app/result/total")
//	defer h.Close()
//	// ... mutate inputs ...
//	// observe live value at h.ResultPath()
//
// Per the kernel-vs-handler principle (SDK-OPERATIONS §11.2A), this
// helper MUST dispatch system/compute:install (which audits + creates
// the grant entity + registers deps atomically) and never tree:put a
// subgraph manifest directly.
func (a *AppPeer) InstallComputeSubgraph(ctx context.Context, expr *Builder, resultPath string) (*SubgraphHandle, error) {
	if expr == nil {
		return nil, NewError(400, "invalid_install", "compute expression is nil")
	}
	if resultPath == "" {
		return nil, NewError(400, "invalid_install", "resultPath is empty")
	}

	// Stable expression path under app/compute/subgraphs/<resultPath-id>/_expr.
	// The expression tree gets materialized here; the install op then
	// audits via this path. We anchor under the result_path so multiple
	// subgraphs writing to different result paths don't collide.
	exprPath := "app/compute/subgraphs" + resultPathToID(resultPath) + "/_expr"

	if _, err := expr.Build(ctx, exprPath); err != nil {
		return nil, NewError(400, "compute_build_failed", err.Error())
	}

	// Dispatch system/compute:install with resource=exprPath, params=
	// ComputeInstallRequestData{ResultPath}.
	installReq, err := types.ComputeInstallRequestData{ResultPath: resultPath}.ToEntity()
	if err != nil {
		return nil, NewError(500, "install_encode_failed", err.Error())
	}
	resp, err := a.Executor().ExecuteOnResource(
		"system/compute", "install", installReq,
		&types.ResourceTarget{Targets: []string{exprPath}},
	)
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, NewError(resp.Status, "install_failed",
			"system/compute:install returned status "+strconv.FormatUint(uint64(resp.Status), 10)+" (type="+resp.Type+")")
	}

	var result types.ComputeInstallResultData
	if err := ecf.Decode(resp.Data, &result); err != nil {
		return nil, NewError(500, "install_decode_failed", "decode install result: "+err.Error())
	}

	return &SubgraphHandle{
		subgraphPath:     result.SubgraphPath,
		resultPath:       result.ResultPath,
		ap:               a,
		ImpureOperations: result.ImpureOperations,
	}, nil
}

// resultPathToID turns a result-path string into a stable id segment
// for the expression's tree-path. Replaces '/' with '_' so the
// expression sub-path doesn't get confused with the result hierarchy.
// Not a content hash — just a deterministic readable label.
func resultPathToID(resultPath string) string {
	out := make([]byte, 0, len(resultPath)+1)
	out = append(out, '/')
	for i := 0; i < len(resultPath); i++ {
		c := resultPath[i]
		if c == '/' {
			out = append(out, '_')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
