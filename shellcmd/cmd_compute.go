package shellcmd

// Compute shell verbs (Phase H.3 — Frontend 1).
//
// `compute show <path>` — pretty-print the compute IR rooted at path.
// First Frontend-1 slice: an inspector for any compute entity in the
// tree. No DSL needed; works on any IR (hand-built, S1-built, or
// stored by another tool).
//
// Architecture: per GUIDE-SHELL-FRAMING.md §8.1, the verb-op
// (FormatComputeIR) takes *entitysdk.AppPeer + typed inputs, never a
// *Shell. The cmd wrapper (cmdComputeShow) does arg parsing and
// shell-state extraction. The verb-op is reusable from tests, scripts,
// other shell commands, or future panels without going through the
// CLI parser — that's the seam payoff.

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// FormatComputeIR walks the compute expression rooted at rootPath and
// returns it as a slice of indented lines suitable for display.
// Non-compute entities at rootPath are rendered as a single line
// indicating their type (the formatter doesn't refuse to look at
// them — the user can `compute show` any path and learn whether what's
// there is compute or not).
//
// Walking is content-hash-driven via ap.Store().GetByHash so the
// rendering reflects the actual IR graph, not just the on-disk tree
// layout. Intermediate nodes that live at hash-derived paths under
// rootPath/_expr/ are reached via their child-hash references in
// parent nodes — the same machinery the evaluator uses.
//
// maxDepth caps the recursion to guard against pathological inputs.
// 0 means "unlimited" (the IR is content-addressed and acyclic in
// well-formed cases, but defensive).
func FormatComputeIR(ap *entitysdk.AppPeer, rootPath string, maxDepth int) ([]string, error) {
	if ap == nil {
		return nil, fmt.Errorf("FormatComputeIR: nil AppPeer")
	}
	if rootPath == "" {
		return nil, fmt.Errorf("FormatComputeIR: empty path")
	}
	root, found, err := ap.Get(rootPath)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", rootPath, err)
	}
	if !found {
		return nil, fmt.Errorf("no entity at %s", rootPath)
	}

	var out []string
	formatNode(ap, root, 0, maxDepth, &out)
	return out, nil
}

// formatNode renders one entity at the given depth, then recurses
// into its child hashes. The walker is a value receiver so it stays
// independent of any global state — same verb-op-style discipline.
func formatNode(ap *entitysdk.AppPeer, ent entity.Entity, depth, maxDepth int, out *[]string) {
	indent := strings.Repeat("  ", depth)
	hashShort := shortComputeHash(ent.ContentHash)

	if maxDepth > 0 && depth > maxDepth {
		*out = append(*out, indent+"… (max depth reached at "+hashShort+")")
		return
	}

	switch ent.Type {
	case types.TypeComputeLiteral:
		var d types.ComputeLiteralData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error: "+err.Error()+">")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s value=%s  [%s]", indent, ent.Type, formatLiteralValue(d.Value), hashShort))

	case types.TypeComputeLookupScope:
		var d types.ComputeLookupScopeData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s name=%q  [%s]", indent, ent.Type, d.Name, hashShort))

	case types.TypeComputeLookupTree:
		var d types.ComputeLookupTreeData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		rel := ""
		if d.Relative {
			rel = " (relative)"
		}
		*out = append(*out, fmt.Sprintf("%s%s path=%q%s  [%s]", indent, ent.Type, d.Path, rel, hashShort))

	case types.TypeComputeLookupHash:
		var d types.ComputeLookupHashData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s hash=%s  [%s]", indent, ent.Type, shortComputeHash(d.Hash), hashShort))

	case types.TypeComputeField:
		var d types.ComputeFieldData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s name=%q  [%s]", indent, ent.Type, d.Name, hashShort))
		recurseChild(ap, d.Entity, depth+1, maxDepth, out)

	case types.TypeComputeIndex:
		var d types.ComputeIndexData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s  [%s]", indent, ent.Type, hashShort))
		recurseChild(ap, d.Array, depth+1, maxDepth, out)
		recurseChild(ap, d.Index, depth+1, maxDepth, out)

	case types.TypeComputeLength:
		var d types.ComputeLengthData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s  [%s]", indent, ent.Type, hashShort))
		recurseChild(ap, d.Array, depth+1, maxDepth, out)

	case types.TypeComputeNumericCast:
		var d types.ComputeNumericCastData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s to=%s  [%s]", indent, ent.Type, d.ToType, hashShort))
		recurseChild(ap, d.Value, depth+1, maxDepth, out)

	case types.TypeComputeArithmetic:
		var d types.ComputeArithmeticData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s op=%s  [%s]", indent, ent.Type, d.Op, hashShort))
		recurseChild(ap, d.Left, depth+1, maxDepth, out)
		recurseChild(ap, d.Right, depth+1, maxDepth, out)

	case types.TypeComputeCompare:
		var d types.ComputeCompareData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s op=%s  [%s]", indent, ent.Type, d.Op, hashShort))
		recurseChild(ap, d.Left, depth+1, maxDepth, out)
		recurseChild(ap, d.Right, depth+1, maxDepth, out)

	case types.TypeComputeLogic:
		var d types.ComputeLogicData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s op=%s  [%s]", indent, ent.Type, d.Op, hashShort))
		recurseChild(ap, d.Left, depth+1, maxDepth, out)
		if d.Right != nil {
			recurseChild(ap, *d.Right, depth+1, maxDepth, out)
		}

	case types.TypeComputeConstruct:
		var d types.ComputeConstructData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s entity_type=%s fields=%d  [%s]", indent, ent.Type, d.EntityType, len(d.Fields), hashShort))
		for _, fieldName := range sortedKeys(d.Fields) {
			*out = append(*out, fmt.Sprintf("%s  field %q:", indent, fieldName))
			recurseChild(ap, d.Fields[fieldName], depth+2, maxDepth, out)
		}

	case types.TypeComputeApply:
		var d types.ComputeApplyData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		head := fmt.Sprintf("%s%s path=%q op=%q args=%d", indent, ent.Type, d.Path, d.Operation, len(d.Args))
		if !d.Fn.IsZero() {
			head += " fn=" + shortComputeHash(d.Fn)
		}
		if !d.Capability.IsZero() {
			head += " capability=" + shortComputeHash(d.Capability)
		}
		if !d.Resource.IsZero() {
			head += " resource=" + shortComputeHash(d.Resource)
		}
		head += "  [" + hashShort + "]"
		*out = append(*out, head)
		for _, argName := range sortedKeys(d.Args) {
			*out = append(*out, fmt.Sprintf("%s  arg %q:", indent, argName))
			recurseChild(ap, d.Args[argName], depth+2, maxDepth, out)
		}

	case types.TypeComputeIf:
		var d types.ComputeIfData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s  [%s]", indent, ent.Type, hashShort))
		*out = append(*out, indent+"  condition:")
		recurseChild(ap, d.Condition, depth+2, maxDepth, out)
		*out = append(*out, indent+"  then:")
		recurseChild(ap, d.Then, depth+2, maxDepth, out)
		if d.Else != nil {
			*out = append(*out, indent+"  else:")
			recurseChild(ap, *d.Else, depth+2, maxDepth, out)
		}

	case types.TypeComputeLet:
		var d types.ComputeLetData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s bindings=%d  [%s]", indent, ent.Type, len(d.Bindings), hashShort))
		for _, b := range d.Bindings {
			*out = append(*out, fmt.Sprintf("%s  bind %q:", indent, b.Name))
			recurseChild(ap, b.Value, depth+2, maxDepth, out)
		}
		*out = append(*out, indent+"  body:")
		recurseChild(ap, d.Body, depth+2, maxDepth, out)

	case types.TypeComputeLambda:
		var d types.ComputeLambdaData
		if err := ecf.Decode(ent.Data, &d); err != nil {
			*out = append(*out, indent+ent.Type+" <decode error>")
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s params=%v  [%s]", indent, ent.Type, d.Params, hashShort))
		recurseChild(ap, d.Body, depth+1, maxDepth, out)

	default:
		// Not a compute IR node — render a one-liner so the user knows
		// what's there and that show stopped descending.
		*out = append(*out, fmt.Sprintf("%s%s  [%s]  (not a compute IR type — show does not recurse)", indent, ent.Type, hashShort))
	}
}

// recurseChild loads child entity by hash and renders it at the
// given depth. Children that the store can't find produce a "<missing
// hash X>" line — useful for debugging incomplete tree states.
func recurseChild(ap *entitysdk.AppPeer, h hash.Hash, depth, maxDepth int, out *[]string) {
	indent := strings.Repeat("  ", depth)
	ent, ok := ap.Store().GetByHash(h)
	if !ok {
		*out = append(*out, fmt.Sprintf("%s<missing %s>", indent, shortComputeHash(h)))
		return
	}
	formatNode(ap, ent, depth, maxDepth, out)
}

// shortComputeHash renders a 12-char hex prefix of the hash for
// compute-show output. Distinct from shortHash (cmd_role.go) only in
// the format we want here — full 12 chars with no algo prefix; the
// IR rendering already labels everything by type so the algo byte
// adds noise.
func shortComputeHash(h hash.Hash) string {
	if len(h.Digest) == 0 {
		return "(zero hash)"
	}
	full := hex.EncodeToString(h.Bytes())
	if len(full) >= 14 {
		// Skip the leading 2-char algorithm byte for compactness.
		full = full[2:]
	}
	if len(full) > 12 {
		return full[:12] + "…"
	}
	return full
}

// formatLiteralValue renders a literal value for the show output.
// Strings get quoted; numbers print as decimal; other types use Go's
// default formatter. Long values are truncated.
func formatLiteralValue(v interface{}) string {
	switch x := v.(type) {
	case string:
		s := fmt.Sprintf("%q", x)
		if len(s) > 60 {
			return s[:57] + "..."
		}
		return s
	case []byte:
		if len(x) > 24 {
			return fmt.Sprintf("[]byte(%d)...", len(x))
		}
		return fmt.Sprintf("[]byte(%v)", x)
	case nil:
		return "nil"
	default:
		s := fmt.Sprintf("%v", x)
		if len(s) > 60 {
			return s[:57] + "..."
		}
		return s
	}
}

// sortedKeys returns map keys in lex order. Used for deterministic
// render output (the IR is canonical-sorted by length-then-lex on
// encode, but for display lex-only is easier to read).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// cmdComputeShow implements `compute show <path>`. Resolves the path
// against the alias table + WD, looks up the right peer connection
// (local or remote — show works across @alias-prefixed paths the
// same way cat does), then renders via FormatComputeIR.
func cmdComputeShow(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: compute show <path>")
	}
	target := sh.Resolve(args[0])
	if target.IsRoot() {
		return Result{}, fmt.Errorf("cannot show root")
	}

	pc := sh.ConnForPath(target)
	if pc == nil {
		return Result{}, fmt.Errorf("no connection for path %s", target)
	}

	lines, err := FormatComputeIR(pc.Peer, target.String(), 32)
	if err != nil {
		return Result{}, err
	}
	return LinesResult(lines), nil
}

// cmdComputeRegister implements `compute register <pattern> <expr-path>`.
// Registers a compute-backed handler at <pattern> whose evaluation
// expression lives at <expr-path>. The expression must already be
// stored at <expr-path> (use Go-side S1/toolkit to author it, or
// import from another tool); this verb does NOT build expressions.
//
// Defaults: the registered handler exposes a single "compute"
// operation taking primitive/any in and out. For non-default
// operation names or input/output types, register via Go directly
// (the verb is for the common interactive case).
func cmdComputeRegister(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: compute register <pattern> <expr-path>")
	}
	pattern := args[0]
	exprPathArg := args[1]

	// Resolve the expression path against the alias table + WD.
	// Register the handler on the peer that owns the expression —
	// register-against-remote-expression isn't a thing today (R0
	// register is a local operation), but this keeps the verb
	// honest about which peer it's acting on.
	target := sh.Resolve(exprPathArg)
	pc := sh.ConnForPath(target)
	if pc == nil {
		return Result{}, fmt.Errorf("no connection for path %s", target)
	}

	spec := entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    pattern,
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}
	_, err := pc.Peer.RegisterComputeHandlerAtExpressionPath(
		shellContext(), spec, target.String())
	if err != nil {
		return Result{}, fmt.Errorf("register: %w", err)
	}
	return MessageResult(fmt.Sprintf("registered compute handler at %s (expression: %s)",
		pattern, target)), nil
}

// cmdComputeAggregate implements `compute aggregate <prefix>` — a
// panel-level battle-test of the lowering toolkit. Queries entities
// under <prefix>, extracts the .size field from each (skipping
// entities without one), dispatches a compute handler that folds
// {count, total_bytes} over the list, and prints the result.
//
// Why a verb (and not a panel slot) for the v0 battle-test:
//   - Cheap to ship, no panel-state plumbing.
//   - End-to-end probe of every layer below the panel: query for
//     listing, S1+toolkit for IR, RegisterComputeHandler for setup,
//     dispatch+S5 unwrap for use.
//   - Surfaces S4/S6 ergonomic friction at a real call site —
//     before this verb existed there was no caller dispatching a
//     toolkit-built compute end-to-end outside test code.
//
// Idempotency: on first call per peer, builds + registers the
// handler at `app/compute/files-stats`. Subsequent calls reuse
// the existing registration (manifest presence at that path is the
// "already registered" signal — we skip the register dispatch).
//
// Scope discipline: hardcodes the "size" field name + the
// {count, total_bytes} output shape. A more general "aggregate"
// verb (configurable field, configurable reduce op) is a follow-on;
// the v0 target is "demonstrate compute usefully aggregates over
// real workbench entities."
func cmdComputeAggregate(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: compute aggregate <prefix>")
	}
	// Raw prefix — same convention as `find` / `grep`: PathPrefix is
	// stored as the bare path string, not peer-id qualified. sh.Resolve
	// is meant for entity lookup (where peer-id is part of the path);
	// query operates on the local store's unqualified prefix space.
	prefix := args[0]

	pc := sh.Local
	ctx := shellContext()

	bigLimit := uint64(100000)
	res, err := pc.Peer.Executor().Query(
		types.QueryExpressionData{PathPrefix: prefix, Limit: &bigLimit})
	if err != nil {
		return Result{}, fmt.Errorf("query: %w", err)
	}

	files := make([]interface{}, 0, len(res.Matches))
	skipped := 0
	for _, m := range res.Matches {
		ent, ok := pc.Peer.Store().GetByHash(m.Hash)
		if !ok {
			skipped++
			continue
		}
		size, has := extractNumericSize(ent.Data)
		if !has {
			skipped++
			continue
		}
		files = append(files, map[string]interface{}{
			"path": m.Path,
			"size": size,
		})
	}

	if len(files) == 0 {
		return MessageResult(fmt.Sprintf(
			"(no entities with a numeric .size field under %s; %d entities scanned, %d skipped)",
			prefix, len(res.Matches), skipped)), nil
	}

	pattern := "app/compute/files-stats"
	if err := ensureFilesStatsHandler(ctx, pc.Peer, pattern); err != nil {
		return Result{}, fmt.Errorf("register handler: %w", err)
	}

	params, _ := entitysdk.PrimitiveAny(map[string]interface{}{"files": files})
	resp, err := pc.Peer.Executor().ExecuteWithParams(pattern, "compute", params)
	if err != nil {
		return Result{}, fmt.Errorf("dispatch: %w", err)
	}
	if resp.Status != 200 {
		return Result{}, fmt.Errorf("compute returned status %d (type=%s)", resp.Status, resp.Type)
	}

	rec, err := entitysdk.UnwrapComputeResultAsMap(resp)
	if err != nil {
		return Result{}, fmt.Errorf("unwrap result: %w", err)
	}

	lines := []string{
		fmt.Sprintf("aggregate over %s (%d entities matched, %d had .size):",
			prefix, len(res.Matches), len(files)),
		fmt.Sprintf("  count       = %v", rec["count"]),
		fmt.Sprintf("  total_bytes = %v", rec["total_bytes"]),
	}
	if skipped > 0 {
		lines = append(lines,
			fmt.Sprintf("  (skipped %d entities with no numeric .size)", skipped))
	}
	return LinesResult(lines), nil
}

// ensureFilesStatsHandler registers the files-stats compute handler
// at pattern, idempotently. Uses IsHandlerRegistered (S6b) instead of
// the prior probe of internal expression-path layout.
func ensureFilesStatsHandler(ctx context.Context, peer *entitysdk.AppPeer, pattern string) error {
	if peer.IsHandlerRegistered(pattern) {
		return nil
	}

	c := peer.Compute()
	files := c.Field(c.LookupScope("params"), "files")
	totalBytes := entitysdk.LowerFold(c, files, c.Literal(uint64(0)),
		func(acc, elem *entitysdk.Builder) *entitysdk.Builder {
			return c.Arithmetic("add", acc, c.Field(elem, "size"))
		})
	expr := entitysdk.LowerRecord(c, "primitive/any", map[string]interface{}{
		"count":       c.Length(files),
		"total_bytes": totalBytes,
	})

	_, err := peer.RegisterComputeHandler(ctx, entitysdk.HandlerSpec{
		Pattern: pattern,
		Name:    "files-stats",
		Operations: map[string]types.HandlerOperationSpec{
			"compute": {InputType: "primitive/any", OutputType: "primitive/any"},
		},
	}, expr)
	return err
}

// extractNumericSize pulls a numeric "size" field out of CBOR entity
// data. Returns (value, true) if present and numeric; (0, false)
// otherwise. Accepts uint64, int64, and the smaller numeric kinds
// CBOR may decode to depending on value range.
//
// FLOATS ARE THE COMMON CASE, not an exotic one. `put` decodes its
// payload with json.Unmarshal into an interface{}, and encoding/json
// turns every JSON number into a float64; CBOR core-deterministic
// encoding does not fold an integral float back to an integer, so the
// round trip returns float64. The effect before this case existed:
// `put files/a app/file '{"size":100}'` followed by `compute aggregate
// files` reported "3 entities scanned, 3 skipped" — the verb whose
// stated job is to aggregate over real workbench entities could not
// read one written by the shell's own put. Measured 2026-08-23.
//
// A non-integral size is refused rather than truncated (AP33: a
// tolerant fallback that turns malformed input into a well-formed
// answer is a bug). It is also what the fold requires — the
// accumulator is a uint64 literal, so a fractional byte count has
// nowhere to go but a silently wrong total.
func extractNumericSize(data []byte) (uint64, bool) {
	var decoded map[string]interface{}
	if err := ecf.Decode(data, &decoded); err != nil {
		return 0, false
	}
	v, ok := decoded["size"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case uint64:
		return n, true
	case int64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case uint:
		return uint64(n), true
	case int:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case float64:
		return integralSize(n)
	case float32:
		return integralSize(float64(n))
	default:
		return 0, false
	}
}

// integralSize accepts a float only where it names an exact,
// representable, non-negative whole number. NaN and ±Inf fail the
// f == math.Trunc(f) test; the upper bound keeps a float too large for
// uint64 from wrapping to a nonsense total.
func integralSize(f float64) (uint64, bool) {
	if f < 0 || f != math.Trunc(f) || f > math.MaxInt64 {
		return 0, false
	}
	return uint64(f), true
}

// cmdCompute is the sub-op dispatcher for `compute`. Future verbs
// (build with DSL) plug in as additional sub-ops here. Following
// the per-verb-parser pattern (cmd_revision.go, cmd_history.go etc.)
// rather than per-sub-op flat-register, so the help text and arg
// shape stay coherent.
func cmdCompute(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: compute <subop> [args] (subops: show, register, aggregate)")
	}
	subop := args[0]
	rest := args[1:]
	switch subop {
	case "show":
		return cmdComputeShow(sh, rest)
	case "register":
		return cmdComputeRegister(sh, rest)
	case "aggregate":
		return cmdComputeAggregate(sh, rest)
	default:
		return Result{}, fmt.Errorf("compute: unknown subop %q (subops: show, register, aggregate)", subop)
	}
}

// shellContext returns the context for shell-initiated dispatches.
// Today this is just context.Background; if we ever want per-verb
// cancellation (e.g., a long-running compute install), this is the
// single place to thread a real context through.
func shellContext() context.Context {
	return context.Background()
}
