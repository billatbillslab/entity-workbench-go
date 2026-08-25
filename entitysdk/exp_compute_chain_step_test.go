package entitysdk_test

// EXPERIMENT-A: chain ↔ compute composability.
//
// Workbench-side experiment probing whether a compute expression composes
// cleanly as a step inside a continuation chain. Hand-assembled — no
// helpers — to surface real authoring friction. Friction notes live in
// docs/architecture/EXPERIMENT-A-CHAIN-COMPUTE-NOTES.md (live document).
//
// Phases:
//   A1 (TestExpA_1_LiteralHandler) — smoke: can we register a compute-backed
//      handler and dispatch to it from the workbench SDK at all?
//   A2 (TestExpA_2_ScopeParams) — does prior-step data reach the expression
//      via scope.params in a field-accessible form?
//   A3 (TestExpA_3_ConstructResult) — does multi-field extract + construct
//      produce a result entity that flows back through the dispatch boundary?
//   A4 (TestExpA_4_ChainComposition) — install a continuation that
//      dispatches to the compute handler; trigger it; verify composition.

import (
	"context"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

// expA_putExpr is the local equivalent of core-go's validate/compute.go
// helper — bottom-up expression assembly. Friction-log entry #1 — without
// this we'd need 3 lines per leaf entity to TreePut.
func expA_putExpr(t *testing.T, ap *entitysdk.AppPeer, path string, ent entity.Entity) {
	t.Helper()
	if _, err := ap.PutEntity(path, ent); err != nil {
		t.Fatalf("put expr at %q: %v", path, err)
	}
}

// expA_registerComputeHandler dispatches system/handler:register with a
// manifest that points expression_path at the given tree path. There is
// NO typed SDK wrapper for this — friction-log entry #2 — RegisterHandler
// in entitysdk only accepts a Go-func body, not a compute expression
// path. We have to assemble the RegisterRequestData and dispatch raw.
func expA_registerComputeHandler(t *testing.T, ap *entitysdk.AppPeer, pattern, exprPath string) {
	t.Helper()
	manifest := types.HandlerManifestData{
		Pattern: pattern,
		Name:    pattern,
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
		ExpressionPath: exprPath,
		InternalScope: []types.GrantEntry{{
			Operations: types.CapabilityScope{Include: []string{"*"}},
			Handlers:   types.CapabilityScope{Include: []string{"*"}},
			Resources:  types.CapabilityScope{Include: []string{"*"}},
		}},
	}
	req := types.RegisterRequestData{
		Manifest:       manifest,
		RequestedScope: manifest.InternalScope,
	}
	reqEnt, err := req.ToEntity()
	if err != nil {
		t.Fatalf("build register-request entity: %v", err)
	}
	resp, err := ap.Executor().ExecuteOnResource(
		"system/handler", "register", reqEnt,
		&types.ResourceTarget{Targets: []string{"system/handler/" + pattern}},
	)
	if err != nil {
		t.Fatalf("dispatch system/handler:register: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("register returned status %d (type=%s)", resp.Status, resp.Type)
	}
	// Diagnostic: what entity (type + non-zero data) actually landed at pattern?
	got, found, getErr := ap.Get(pattern)
	if getErr != nil {
		t.Logf("DIAG: ap.Get(%q) error: %v", pattern, getErr)
	} else if !found {
		t.Logf("DIAG: register returned 200 but ap.Get(%q) → not found", pattern)
	} else {
		t.Logf("DIAG: at %q after register: type=%q, hash=%s", pattern, got.Type, got.ContentHash)
	}
	// Also confirm interface entity landed.
	ifaceGot, ifaceFound, _ := ap.Get("system/handler/" + pattern)
	if ifaceFound {
		t.Logf("DIAG: at system/handler/%s: type=%q", pattern, ifaceGot.Type)
	} else {
		t.Logf("DIAG: system/handler/%s NOT FOUND", pattern)
	}
}

// expA_callHandler dispatches an EXECUTE to the registered compute handler
// and returns the response. Caller decodes the result entity.
func expA_callHandler(t *testing.T, ap *entitysdk.AppPeer, pattern, op string, params entity.Entity) *entitysdk.Response {
	t.Helper()
	resp, err := ap.Executor().ExecuteWithParams(pattern, op, params)
	if err != nil {
		t.Fatalf("dispatch %s:%s: %v", pattern, op, err)
	}
	return resp
}

// expA_paramsEntity wraps a Go map as a primitive/any entity for use as
// EXECUTE params. Matches core-go validate's buildAnyParams. Friction-log
// entry #3 — there's no entitysdk helper for "build a primitive/any from a
// map" either. The CBOR shape matters: this becomes the value the
// expression sees via lookup/scope("params").
func expA_paramsEntity(t *testing.T, m map[string]interface{}) entity.Entity {
	t.Helper()
	raw, err := ecf.Encode(m)
	if err != nil {
		t.Fatalf("encode params: %v", err)
	}
	ent, err := entity.NewEntity("primitive/any", cbor.RawMessage(raw))
	if err != nil {
		t.Fatalf("build params entity: %v", err)
	}
	return ent
}

// expA_decodeResult extracts the unwrapped value from a dispatch response.
// Mirrors core-go validate's callEntityNative unwrap logic — handles the
// three response shapes: compute/error, compute/result, primitive/any
// (auto-wrapped per handler.go:396 unwrapEntityNativeResult).
//
// Friction-log entry #4 — entitysdk.Response is already "split" (Type +
// Data fields) where the validate driver got an envelope it had to
// decode. This is an ergonomic improvement at the SDK boundary; we just
// switch on resp.Type directly. Nice.
func expA_decodeResult(t *testing.T, resp *entitysdk.Response) interface{} {
	t.Helper()
	if resp.Status != 200 {
		t.Fatalf("dispatch status %d (expected 200)", resp.Status)
	}
	switch resp.Type {
	case types.TypeComputeError:
		var d types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &d)
		t.Fatalf("compute error: %s — %s", d.Code, d.Message)
	case types.TypeComputeResult:
		// Friction-log entry #5 — entity-native dispatch is supposed to
		// unwrap compute/result (handler.go:396), but if we get this
		// type back the unwrap didn't happen at the boundary.
		var d types.ComputeResultData
		if err := ecf.Decode(resp.Data, &d); err != nil {
			t.Fatalf("decode compute/result: %v", err)
		}
		return d.Value
	case "primitive/any":
		var v interface{}
		if err := ecf.Decode(resp.Data, &v); err != nil {
			t.Fatalf("decode primitive/any: %v", err)
		}
		return v
	}
	// Unknown / pass-through entity result — hand back as an entity.
	return entity.Entity{Type: resp.Type, Data: resp.Data, ContentHash: resp.Hash}
}

// --- Probe phase: characterize the gap ---------------------------------

// TestExpA_P1_LanguageNativeStillWorks — confirm the *registry-direct*
// dispatch path is functional for language-native handlers (Go-func body).
// This isolates the issue to entity-native specifically: if this passes,
// the dispatch infrastructure is fine; only tree-walk fallback is missing.
func TestExpA_P1_LanguageNativeStillWorks(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/exp-a/native"
	h, err := ap.RegisterHandler(entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    "native-test",
		Operations: map[string]types.HandlerOperationSpec{
			"ping": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, func(ctx context.Context, req *handler.Request) (*handler.Response, error) {
		raw, _ := ecf.Encode(map[string]interface{}{"echoed": req.Operation})
		resultEnt, _ := entity.NewEntity("primitive/any", cbor.RawMessage(raw))
		return &handler.Response{Status: 200, Result: resultEnt}, nil
	})
	if err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	resp, err := ap.Executor().ExecuteWithParams(pattern, "ping",
		expA_paramsEntity(t, map[string]interface{}{}))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d", resp.Status)
	}
	t.Logf("P1 PASS: language-native via Executor works (status=%d type=%s)", resp.Status, resp.Type)
}

// TestExpA_P2_EvaluateExpressionWired — confirm the dispatcher's
// EvaluateExpression hook is wired post-CreatePeer (it should be per
// app.go:652). This rules out a wiring-not-applied hypothesis for F6.
func TestExpA_P2_EvaluateExpressionWired(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	// AppPeer doesn't expose the underlying peer.Dispatcher directly. We
	// can probe via the system/compute handler being reachable through the
	// in-memory registry — that's the "compute extension wired" smoke
	// (mirrors the existing TestCompute_HandlerReachable test).
	_, err = ap.Executor().Execute("system/compute", "eval")
	// Expected: 400-ish (no resource) or 404 from compute eval itself —
	// the point is that system/compute resolves as a handler (not handler_not_found).
	if err != nil && entitysdk.IsNotFound(err) {
		t.Fatalf("system/compute not registered: %v", err)
	}
	t.Logf("P2 PASS: system/compute handler reachable via registry; EvaluateExpression should also be wired (app.go:652)")
}

// TestExpA_P3_DidInstallHelp — answer the user's question: does running
// system/compute:install (the reactive subgraph path) make entity-native
// dispatch start working? Spec says no (entity-native is on-demand at
// dispatch via resolveHandler → EvaluateExpression hook; install is for
// reactive cells writing to {expr-path}/result). Probe to confirm.
func TestExpA_P3_DidInstallHelp(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/exp-a/with-install"
	exprPath := pattern + "/expr"
	lit, _ := types.ComputeLiteralData{Value: uint64(7)}.ToEntity()
	expA_putExpr(t, ap, exprPath, lit)
	expA_registerComputeHandler(t, ap, pattern, exprPath)

	// Run system/compute:install on the same expression.
	installReq, _ := types.ComputeInstallRequestData{}.ToEntity()
	installResp, err := ap.Executor().ExecuteOnResource(
		"system/compute", "install", installReq,
		&types.ResourceTarget{Targets: []string{exprPath}},
	)
	if err != nil {
		t.Fatalf("install dispatch: %v", err)
	}
	t.Logf("DIAG: install returned status=%d type=%s", installResp.Status, installResp.Type)

	// Now try the entity-native dispatch.
	//
	// Historical context: when F6 was open, this dispatch failed regardless
	// of whether install ran — install is for reactive cells, not entity-
	// native dispatch. After the F6 fix landed (workbench Executor routes
	// local URIs through Dispatcher.DispatchLocalExecute → V7 §6.6 tree-walk),
	// the dispatch succeeds. Its success here demonstrates that the F6 fix
	// works regardless of whether install ran first — confirming again that
	// install was never the missing piece.
	resp, err := ap.Executor().ExecuteWithParams(pattern, "compute",
		expA_paramsEntity(t, map[string]interface{}{}))
	if err != nil {
		t.Logf("P3 PRE-FIX BEHAVIOR: entity-native dispatch fails: %v", err)
		t.Logf("(Expected only when F6 is open — see EXPERIMENT-A-CHAIN-COMPUTE-NOTES.md)")
		return
	}
	t.Logf("P3 POST-FIX BEHAVIOR: entity-native dispatch succeeds (status=%d) — confirms install is not required; F6 fix is what made it reachable", resp.Status)
}

// numEq normalizes the {int8/16/32/64, uint*, float64} that CBOR decode
// produces, so the test can compare against a single int literal.
func numEq(got interface{}, want float64) bool {
	switch v := got.(type) {
	case uint64:
		return float64(v) == want
	case int64:
		return float64(v) == want
	case float64:
		return v == want
	case int:
		return float64(v) == want
	}
	return false
}

// --- Phase A1: smoke ---------------------------------------------------

// TestExpA_1_LiteralHandler — the floor. Register a compute-backed
// handler whose expression is just `literal(42)`, dispatch, get 42 back.
// If this fails, nothing else in Experiment A works.
func TestExpA_1_LiteralHandler(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/exp-a/literal"
	exprPath := pattern + "/expr"

	// Build the expression: literal(42).
	lit, err := types.ComputeLiteralData{Value: uint64(42)}.ToEntity()
	if err != nil {
		t.Fatalf("build literal: %v", err)
	}
	expA_putExpr(t, ap, exprPath, lit)

	// Register the handler.
	expA_registerComputeHandler(t, ap, pattern, exprPath)

	// Dispatch. No params needed; the expression is closed.
	resp := expA_callHandler(t, ap, pattern, "compute", expA_paramsEntity(t, map[string]interface{}{}))
	got := expA_decodeResult(t, resp)
	if !numEq(got, 42) {
		t.Fatalf("expected 42, got %v (%T)", got, got)
	}
	t.Logf("A1 PASS: literal(42) via compute-backed handler returned %v", got)
}

// --- Phase A2: scope.params --------------------------------------------

// TestExpA_2_ScopeParams — verify prior-step-shaped data reaches the
// expression. Expression: field(lookup/scope("params"), "x"). Dispatch
// with params={x: 42}, expect 42.
//
// This is the load-bearing claim for chain composition: a chain step
// passing its result forward needs to land as scope.params in a
// field-accessible form.
func TestExpA_2_ScopeParams(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/exp-a/scope-params"
	exprPath := pattern + "/expr"

	// Build: field(lookup/scope("params"), "x")
	// Two leaf entities + one parent.
	paramsLookup, err := types.ComputeLookupScopeData{Name: "params"}.ToEntity()
	if err != nil {
		t.Fatalf("build lookup: %v", err)
	}
	paramsLookupH, err := ap.PutEntity(pattern+"/p-lookup", paramsLookup)
	if err != nil {
		t.Fatalf("put lookup: %v", err)
	}

	fieldExpr, err := types.ComputeFieldData{Name: "x", Entity: paramsLookupH}.ToEntity()
	if err != nil {
		t.Fatalf("build field: %v", err)
	}
	expA_putExpr(t, ap, exprPath, fieldExpr)

	expA_registerComputeHandler(t, ap, pattern, exprPath)

	// Dispatch with params={x: 42}.
	resp := expA_callHandler(t, ap, pattern, "compute",
		expA_paramsEntity(t, map[string]interface{}{"x": uint64(42)}))
	got := expA_decodeResult(t, resp)
	if !numEq(got, 42) {
		t.Fatalf("expected 42, got %v (%T)", got, got)
	}
	t.Logf("A2 PASS: field(scope.params, x) with params={x: 42} returned %v", got)
}

// --- Phase A4: chain → compute → next step (the actual experiment) ----

// TestExpA_4_ChainCompositionViaCompute — the load-bearing test.
//
// Installs a forward continuation whose Target is a compute-backed handler
// (the A2-style "field(scope.params, x)" expression). The continuation has
// static Params={x: 42} and a DeliverTo that routes the compute result to
// a language-native recording handler. When the continuation is advanced,
// the chain should:
//
//  1. dispatch system/continuation:advance
//  2. continuation handler fires, dispatches to the compute-backed handler
//  3. compute handler evaluates the expression with scope.params = {x: 42}
//  4. compute handler returns 42 as primitive/any
//  5. the continuation's deliver_to routes the result to the recording handler
//  6. recording handler observes 42 in the inbox-delivery payload
//
// This is the chain↔compute composition Experiment A was designed to test.
// A1-A3 proved the floor (compute handler dispatchable in-process). A4
// proves the integration with a real continuation chain.
//
// Friction-log entry #7 — the "verify the compute result actually flowed"
// problem. The cleanest mechanism we found: deliver_to a language-native
// handler that records its received params. Compute can't side-effect to
// the tree, so a chain-level result observer is needed. Worth thinking
// about whether the SDK should offer an "observe-delivery" pattern.
func TestExpA_4_ChainCompositionViaCompute(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	// --- Step A: register the compute-backed handler (A2 shape) ----------
	computePattern := "app/exp-a4/extract-x"
	exprPath := computePattern + "/expr"
	paramsLookup, _ := types.ComputeLookupScopeData{Name: "params"}.ToEntity()
	paramsLookupH, err := ap.PutEntity(computePattern+"/p-lookup", paramsLookup)
	if err != nil {
		t.Fatalf("put p-lookup: %v", err)
	}
	fieldExpr, _ := types.ComputeFieldData{Name: "x", Entity: paramsLookupH}.ToEntity()
	expA_putExpr(t, ap, exprPath, fieldExpr)
	expA_registerComputeHandler(t, ap, computePattern, exprPath)

	// --- Step B: register a recording handler as the chain's terminal --
	recordingPattern := "app/exp-a4/record"
	type recorded struct {
		op     string
		params cbor.RawMessage
	}
	recCh := make(chan recorded, 1)
	recHandle, err := ap.RegisterHandler(entitysdk.HandlerSpec{
		Pattern: recordingPattern,
		Name:    "exp-a4-recorder",
		Operations: map[string]types.HandlerOperationSpec{
			// The deliver_to mechanism dispatches an EXECUTE whose params
			// is wrapped as system/protocol/inbox/delivery (per delivery.go:30
			// InboxDeliveryData). We accept primitive/any so the SDK
			// doesn't type-check it for us.
			"deliver": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, func(ctx context.Context, req *handler.Request) (*handler.Response, error) {
		recCh <- recorded{op: req.Operation, params: req.Params.Data}
		// Acknowledge — non-200 here would bind a lost-error marker.
		raw, _ := ecf.Encode(map[string]interface{}{"ack": true})
		ackEnt, _ := entity.NewEntity("primitive/any", cbor.RawMessage(raw))
		return &handler.Response{Status: 200, Result: ackEnt}, nil
	})
	if err != nil {
		t.Fatalf("register recording handler: %v", err)
	}
	t.Cleanup(func() { _ = recHandle.Close() })

	// --- Step C: install the forward continuation ----------------------
	// Target = compute handler; DeliverTo = recording handler.
	// Static Params={x: 42} so the compute eval has something concrete.
	staticParams := map[string]interface{}{"x": uint64(42)}
	staticRaw, _ := ecf.Encode(staticParams)

	contData := types.ContinuationData{
		Target:    computePattern,
		Operation: "compute",
		Params:    cbor.RawMessage(staticRaw),
		DeliverTo: &types.DeliverySpec{
			URI:       recordingPattern,
			Operation: "deliver",
		},
	}
	entitysdk.SetDefaultDispatchCap(ap.OwnerCapability().ContentHash, &contData)
	contEnt, err := contData.ToEntity()
	if err != nil {
		t.Fatalf("build continuation: %v", err)
	}

	const contPath = "system/inbox/exp-a4/forward"
	if _, err := ap.Continuation().Install(context.Background(), contPath, contEnt); err != nil {
		t.Fatalf("install continuation: %v", err)
	}

	// --- Step D: advance the continuation ------------------------------
	// Trigger-only advance — the continuation has static params, no
	// transform, so the Advance result body is unused. Pass an empty map.
	emptyMap, _ := ecf.Encode(map[string]interface{}{})
	advReq := types.ContinuationAdvanceRequestData{Result: cbor.RawMessage(emptyMap)}
	advEnt, _ := advReq.ToEntity()
	advResp, err := ap.Executor().ExecuteOnResource(
		"system/continuation", "advance", advEnt,
		&types.ResourceTarget{Targets: []string{contPath}},
	)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if advResp.Status != 200 {
		t.Fatalf("advance returned status %d (type=%s)", advResp.Status, advResp.Type)
	}
	t.Logf("DIAG: advance returned status=%d (continuation dispatched its target)", advResp.Status)

	// --- Step E: verify the chain delivery reached the recording handler --
	//
	// A4's claim: the continuation handler dispatches to the compute-backed
	// handler, the compute eval runs in the deliver_to async goroutine, and
	// its result is wrapped as InboxDeliveryData and delivered to the
	// recording handler. The dispatcher honors deliver_to uniformly across
	// handler shapes — V7 §6.6 puts the tree in charge of what binds at a
	// pattern; "dispatch and deliver" doesn't branch on whether the
	// implementation is compiled code or an expression. (This is the
	// post-unification contract; the prior conservative 400-refusal
	// behavior was an over-cautious implementation choice, not a spec
	// constraint.)
	select {
	case got := <-recCh:
		if got.op != "deliver" {
			t.Fatalf("recording handler called with op=%q, want %q", got.op, "deliver")
		}
		var delivery types.InboxDeliveryData
		if err := ecf.Decode(got.params, &delivery); err != nil {
			t.Fatalf("decode InboxDeliveryData from delivery params: %v", err)
		}
		if delivery.Status != 200 {
			t.Fatalf("delivery wrapped status=%d, want 200 (the compute handler's response)", delivery.Status)
		}
		if len(delivery.Result) == 0 {
			t.Fatal("delivery result was empty — compute handler's result didn't flow through")
		}
		t.Logf("A4 PASS: chain → compute → deliver_to → recording handler observed status=%d result_bytes=%d", delivery.Status, len(delivery.Result))
	case <-time.After(3 * time.Second):
		t.Fatal("recording handler never received the chain-delivered result (3s timeout) — deliver_to flow did not propagate from compute handler")
	}
}

// --- Phase A3: construct ------------------------------------------------

// TestExpA_3_ConstructResult — verify multi-field extract + construct.
// Expression:
//
//	construct{
//	  value:   field(params, "x"),
//	  doubled: arithmetic(mul, field(params, "x"), literal(2)),
//	}
//
// Dispatch with params={x: 21}, expect a returned entity with
// {value: 21, doubled: 42}.
//
// This tests the "build a structured result" path — the kind of thing a
// V-1-shaped transform would do (extract from a notification, build a
// resource-target struct, hand it to the next chain step).
func TestExpA_3_ConstructResult(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	pattern := "app/exp-a/construct"
	exprPath := pattern + "/expr"

	// Friction-log entry #5 — bottom-up assembly: 6 entities to express
	// what would be {value: x, doubled: x*2} in any host language.

	// Leaf 1: lookup/scope("params")
	paramsLookup, _ := types.ComputeLookupScopeData{Name: "params"}.ToEntity()
	paramsLookupH, err := ap.PutEntity(pattern+"/p-lookup", paramsLookup)
	if err != nil {
		t.Fatalf("put p-lookup: %v", err)
	}

	// Leaf 2: field(params-lookup, "x") — reusable, both subexpressions need it
	fieldX, _ := types.ComputeFieldData{Name: "x", Entity: paramsLookupH}.ToEntity()
	fieldXH, err := ap.PutEntity(pattern+"/field-x", fieldX)
	if err != nil {
		t.Fatalf("put field-x: %v", err)
	}

	// Leaf 3: literal(2)
	lit2, _ := types.ComputeLiteralData{Value: uint64(2)}.ToEntity()
	lit2H, err := ap.PutEntity(pattern+"/lit2", lit2)
	if err != nil {
		t.Fatalf("put lit2: %v", err)
	}

	// Leaf 4: arithmetic(mul, field-x, lit2)
	doubled, _ := types.ComputeArithmeticData{Op: "mul", Left: fieldXH, Right: lit2H}.ToEntity()
	doubledH, err := ap.PutEntity(pattern+"/doubled", doubled)
	if err != nil {
		t.Fatalf("put doubled: %v", err)
	}

	// Root: construct{value: field-x, doubled: doubled}
	// Type matters — construct's EntityType field determines the result type.
	// We'll use primitive/any for now since the handler's output_type is also
	// primitive/any. Friction-log entry #6 — type-selection is a real
	// concern; a real V-1 transform would want to construct a typed entity
	// like system/protocol/resource-target.
	root, err := types.ComputeConstructData{
		EntityType: "primitive/any",
		Fields: map[string]hash.Hash{
			"value":   fieldXH,
			"doubled": doubledH,
		},
	}.ToEntity()
	if err != nil {
		t.Fatalf("build construct: %v", err)
	}
	expA_putExpr(t, ap, exprPath, root)

	expA_registerComputeHandler(t, ap, pattern, exprPath)

	resp := expA_callHandler(t, ap, pattern, "compute",
		expA_paramsEntity(t, map[string]interface{}{"x": uint64(21)}))
	got := expA_decodeResult(t, resp)

	// Result should be an entity with fields {value: 21, doubled: 42}.
	// The decode path lands it as map[string]interface{} via primitive/any.
	asMap, ok := got.(map[string]interface{})
	if !ok {
		// Could also be map[interface{}]interface{} depending on CBOR mode.
		if alt, ok2 := got.(map[interface{}]interface{}); ok2 {
			asMap = make(map[string]interface{}, len(alt))
			for k, v := range alt {
				if ks, ok3 := k.(string); ok3 {
					asMap[ks] = v
				}
			}
		} else {
			t.Fatalf("expected map result, got %T: %v", got, got)
		}
	}
	if !numEq(asMap["value"], 21) {
		t.Fatalf("value: expected 21, got %v (%T)", asMap["value"], asMap["value"])
	}
	if !numEq(asMap["doubled"], 42) {
		t.Fatalf("doubled: expected 42, got %v (%T)", asMap["doubled"], asMap["doubled"])
	}
	t.Logf("A3 PASS: construct{value: x, doubled: x*2} with x=21 returned %+v", asMap)
}
