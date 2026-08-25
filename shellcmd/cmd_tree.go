package shellcmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

// cmdPut implements `put <path> <type> <json-data>`. Everything after
// the type is the payload, joined back with spaces; it is parsed as
// JSON, and a literal string is stored only when the input was never
// trying to be JSON. The path follows shell resolution rules
// (peer-qualified or alias:relative).
func cmdPut(sh *Shell, args []string) (Result, error) {
	if len(args) < 3 {
		return Result{}, fmt.Errorf("usage: put <path> <type> <json-data>")
	}
	target := sh.Resolve(args[0])
	typeName := args[1]

	// **Everything after the type is the payload, not just the next
	// token.** `put p t {"a":1,"b":2}` tokenizes into one argument and
	// `put p t {"title":"My Site"}` into two, so reading args[2] alone
	// silently truncated any JSON containing a space — and the fallback
	// below then stored the fragment as a literal STRING. The entity
	// wrote fine, the hash came back, and the failure surfaced much
	// later in a consumer as `cannot unmarshal UTF-8 text string`,
	// which reads as the consumer's bug. Measured 2026-08-21, seeding a
	// site by hand.
	rawData := strings.Join(args[2:], " ")

	if target.IsRoot() {
		return Result{}, fmt.Errorf("cannot put at root (cd into a peer first)")
	}
	pc := sh.ConnForPath(target)
	if pc == nil {
		return Result{}, fmt.Errorf("no connection for path %s", target)
	}

	var data interface{}
	if err := json.Unmarshal([]byte(rawData), &data); err != nil {
		// A literal string is a legitimate payload, so the fallback
		// stays — but only for input that was never trying to be JSON.
		// Something opening with `{` or `[` and failing to parse is a
		// mistake, and storing it as a string turns that mistake into a
		// well-formed entity nobody can decode.
		if t := strings.TrimSpace(rawData); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			return Result{}, fmt.Errorf("put: data starts as JSON and does not parse: %w\n"+
				"  got: %s\n"+
				"  SplitArgs strips quotes as SHELL quoting, so an unquoted JSON object arrives\n"+
				"  with its string quotes gone. Single-quote the whole payload:\n"+
				"      put %s %s '%s'\n"+
				"  (storing it as a literal string would write an entity that decodes nowhere —\n"+
				"  the put would succeed, print a hash, and fail in a consumer much later)",
				err, rawData, args[0], typeName, rawData)
		}
		data = rawData
	}

	h, err := pc.Peer.Put(target.String(), typeName, data)
	if err != nil {
		return Result{}, fmt.Errorf("put: %w", err)
	}
	return MessageResult(fmt.Sprintf("put %s [%s] → %s", target, typeName, h.String())), nil
}

// cmdRm implements `rm <path>`.
func cmdRm(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: rm <path>")
	}
	target := sh.Resolve(args[0])
	if target.IsRoot() {
		return Result{}, fmt.Errorf("cannot rm root")
	}
	pc := sh.ConnForPath(target)
	if pc == nil {
		return Result{}, fmt.Errorf("no connection for path %s", target)
	}
	if err := pc.Peer.Remove(target.String()); err != nil {
		return Result{}, fmt.Errorf("rm: %w", err)
	}
	return MessageResult(fmt.Sprintf("removed %s", target)), nil
}

// cmdHas implements `has <path>`. Reports yes/no via a Result
// message; returns an error only on dispatch failure (404 is
// reported as no, not an error).
func cmdHas(sh *Shell, args []string) (Result, error) {
	if len(args) < 1 {
		return Result{}, fmt.Errorf("usage: has <path>")
	}
	target := sh.Resolve(args[0])
	if target.IsRoot() {
		return Result{}, fmt.Errorf("has requires a peer path")
	}
	pc := sh.ConnForPath(target)
	if pc == nil {
		return Result{}, fmt.Errorf("no connection for path %s", target)
	}
	ok, err := pc.Peer.Has(target.String())
	if err != nil {
		return Result{}, fmt.Errorf("has: %w", err)
	}
	if ok {
		return MessageResult(fmt.Sprintf("yes — %s exists", target)), nil
	}
	return MessageResult(fmt.Sprintf("no — %s does not exist", target)), nil
}

// cmdCp implements `cp <src> <dst>`. Reads the entity at src and
// writes it verbatim at dst (preserving content hash). src and dst
// can target different peers — cross-peer copy works because both
// dispatch through the local AppPeer's pool.
func cmdCp(sh *Shell, args []string) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("usage: cp <src> <dst>")
	}
	src := sh.Resolve(args[0])
	dst := sh.Resolve(args[1])

	if src.IsRoot() || dst.IsRoot() {
		return Result{}, fmt.Errorf("cp requires non-root paths on both sides")
	}

	srcPC := sh.ConnForPath(src)
	if srcPC == nil {
		return Result{}, fmt.Errorf("no connection for src %s", src)
	}
	dstPC := sh.ConnForPath(dst)
	if dstPC == nil {
		return Result{}, fmt.Errorf("no connection for dst %s", dst)
	}

	ent, ok, err := srcPC.Peer.Get(src.String())
	if err != nil {
		return Result{}, fmt.Errorf("get src: %w", err)
	}
	if !ok {
		return Result{}, fmt.Errorf("no entity at src %s", src)
	}

	h, err := dstPC.Peer.PutEntity(dst.String(), ent)
	if err != nil {
		return Result{}, fmt.Errorf("put dst: %w", err)
	}
	return MessageResult(fmt.Sprintf("copied %s → %s [%s] (%s)", src, dst, ent.Type, h.String())), nil
}
