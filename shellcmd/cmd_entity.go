package shellcmd

import (
	"fmt"
	"strings"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/types"

	"github.com/fxamacker/cbor/v2"
)

// Exec runs a handler operation against the given peer connection
// and returns the raw SDK Response. This is the shared op behind the
// `exec` shell verb (cmdExec) and any panel that drives a handler
// dispatch — both call this so the qualification + params encoding
// logic lives in one place.
//
// Handler qualification: a bare pattern (no `entity://` prefix) is
// qualified as entity://{pc.PeerID}/{handler} so the executor routes
// local vs remote uniformly. An already-qualified URI passes through
// unchanged, which is what callers want for cross-peer dispatch.
//
// params nil is treated as empty; resource non-nil routes through
// ExecuteOnResource, otherwise ExecuteWithParams.
func Exec(pc *PeerConn, handler, op string, resource *types.ResourceTarget, params map[string]interface{}) (*entitysdk.Response, error) {
	if pc == nil {
		return nil, fmt.Errorf("no connection")
	}
	if !strings.HasPrefix(handler, "entity://") {
		handler = "entity://" + pc.PeerID + "/" + handler
	}
	if params == nil {
		params = map[string]interface{}{}
	}
	paramsRaw, err := ecf.Encode(params)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	paramsEntity, err := entity.NewEntity("primitive/any", cbor.RawMessage(paramsRaw))
	if err != nil {
		return nil, fmt.Errorf("create params: %w", err)
	}
	if resource != nil {
		return pc.Peer.Executor().ExecuteOnResource(handler, op, paramsEntity, resource)
	}
	return pc.Peer.Executor().ExecuteWithParams(handler, op, paramsEntity)
}

// cmdCat implements `cat <path> [-diag]`.
func cmdCat(sh *Shell, args []string) (Result, error) {
	if len(args) == 0 {
		return Result{}, fmt.Errorf("usage: cat <path> [-diag]")
	}

	diag := false
	pathArg := ""
	for _, a := range args {
		if a == "-diag" {
			diag = true
		} else {
			pathArg = a
		}
	}
	if pathArg == "" {
		return Result{}, fmt.Errorf("usage: cat <path> [-diag]")
	}

	target := sh.Resolve(pathArg)
	if target.IsRoot() {
		return Result{}, fmt.Errorf("cannot cat root")
	}

	pc := sh.ConnForPath(target)
	if pc == nil {
		return Result{}, fmt.Errorf("no connection for path %s", target)
	}

	// Pass the peer-qualified path; AppPeer.Get routes local/remote.
	ent, ok, err := pc.Peer.Get(target.String())
	if err != nil {
		return Result{}, fmt.Errorf("get %s: %w", target, err)
	}
	if !ok {
		return Result{}, fmt.Errorf("no entity at %s", target)
	}

	// Directories aren't real entities — they're an emergent property
	// of the path index. But system/tree:get on a path with a trailing
	// slash returns a synthesized `system/tree/listing` entity describing
	// the children. From the user's perspective this is "I cat'd a
	// directory and got told it's a directory" — render it as a listing
	// rather than a raw entity dump. (`-diag` still shows the entity
	// form on demand for users who want the underlying envelope.)
	if !diag && ent.Type == "system/tree/listing" {
		rows, lerr := listAt(pc.Peer, target)
		if lerr == nil {
			if len(rows) == 0 {
				return MessageResult(fmt.Sprintf("(empty directory: %s)", target)), nil
			}
			return Result{Kind: KindListing, Listing: rows}, nil
		}
	}

	payload := &EntityPayload{Entity: ent, Diag: diag}
	if !diag {
		var decoded interface{}
		_ = ecf.Decode(ent.Data, &decoded)
		payload.Decoded = decoded
	}
	return Result{Kind: KindEntity, Entity: payload}, nil
}

// cmdExec implements `exec <handler> <op> [resource] [json-params]`.
// Thin parser around Exec: pulls the handler/op/resource/params out of
// the CLI args and calls the shared op. Panels skip this layer and
// call Exec directly with structured inputs.
func cmdExec(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: exec <handler> <op> [resource] [json-params]")
	}

	handler := args[0]
	operation := args[1]

	var resource *types.ResourceTarget
	var jsonParams string
	for _, a := range args[2:] {
		if strings.HasPrefix(a, "{") {
			jsonParams = a
		} else if resource == nil {
			resource = &types.ResourceTarget{Targets: []string{a}}
		}
	}

	pc := sh.ConnForWD()
	if pc == nil {
		return Result{}, fmt.Errorf("no connection (cd into a peer first)")
	}

	var params map[string]interface{}
	if jsonParams != "" {
		if err := ecf.Decode([]byte(jsonParams), &params); err != nil {
			params = map[string]interface{}{}
		}
	}

	resp, err := Exec(pc, handler, operation, resource, params)
	if err != nil {
		return Result{}, fmt.Errorf("execute: %w", err)
	}

	dispatch := &DispatchResp{
		Status:   int(resp.Status),
		Result:   resp.Entity(),
		Included: len(resp.Included),
	}
	var decoded interface{}
	if err := ecf.Decode(resp.Data, &decoded); err == nil {
		dispatch.Decoded = decoded
	}
	return Result{Kind: KindDispatch, Dispatch: dispatch}, nil
}
