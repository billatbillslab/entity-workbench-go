package shellcmd_test

import (
	"strings"
	"testing"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/shellcmd"
)

// cmd_name_test.go — the `name` verb driven the way a user drives it.
//
// Tier: integration (TESTING-STRATEGY.md). These go through
// Registry.Dispatch rather than calling the cmdName* functions directly,
// because the thing that was broken for the whole registry arc was never a
// function body — it was that no dispatch table entry reached one and no
// shipped peer carried the handler underneath. A test that calls the
// handler function directly is green in exactly the world this feature was
// missing from.

// nameShell builds a shell whose local peer carries the registry substrate,
// mirroring what shellboot now hands every frontend.
func nameShell(t *testing.T) *shellcmd.Shell {
	t.Helper()
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		Extensions: entitysdk.ExtensionsConfig{Registry: &entitysdk.RegistryConfig{}},
	})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	if _, err := ap.EnsureResolverConfig(); err != nil {
		t.Fatalf("EnsureResolverConfig: %v", err)
	}
	return shellcmd.NewShell(ap, "alice", "")
}

// TestName_BindLsResolveUnbind is the round trip a user actually performs:
// bind a name, see it in the book, resolve it, remove it, watch resolution
// fail closed.
func TestName_BindLsResolveUnbind(t *testing.T) {
	sh := nameShell(t)
	reg := shellcmd.Default()
	target := "z6MkSomeTargetPeerIdentifier"

	// An empty book reports as a message, not an error or a bare header.
	res, err := reg.Dispatch(sh, "name", []string{"ls"})
	if err != nil {
		t.Fatalf("name ls (empty): %v", err)
	}
	if res.Kind != shellcmd.KindMessage || !strings.Contains(res.Message, "no local names") {
		t.Errorf("empty book should say so plainly, got %v %q", res.Kind, res.Message)
	}

	if _, err := reg.Dispatch(sh, "name", []string{"bind", "lab", target, "-notes", "basement box"}); err != nil {
		t.Fatalf("name bind: %v", err)
	}

	res, err = reg.Dispatch(sh, "name", []string{"ls"})
	if err != nil {
		t.Fatalf("name ls: %v", err)
	}
	if res.Kind != shellcmd.KindLines || len(res.Lines) < 2 {
		t.Fatalf("name ls: want header + 1 row, got %v %v", res.Kind, res.Lines)
	}
	row := res.Lines[1]
	if !strings.Contains(row, "lab") {
		t.Errorf("name ls row missing the name: %q", row)
	}
	if !strings.Contains(row, "basement box") {
		t.Errorf("name ls row dropped -notes; the note is the only record of WHY a name points where it does: %q", row)
	}

	res, err = reg.Dispatch(sh, "name", []string{"resolve", "lab"})
	if err != nil {
		t.Fatalf("name resolve: %v", err)
	}
	joined := strings.Join(res.Lines, "\n")
	if !strings.Contains(joined, target) {
		t.Errorf("name resolve did not report the target peer-id:\n%s", joined)
	}
	// Reach is half of a resolution and must be reported even when empty,
	// or a zero-transport binding is indistinguishable from a field the
	// output does not carry.
	if !strings.Contains(joined, "transports") {
		t.Errorf("name resolve omitted the transports line:\n%s", joined)
	}

	if _, err := reg.Dispatch(sh, "name", []string{"unbind", "lab"}); err != nil {
		t.Fatalf("name unbind: %v", err)
	}
	if _, err := reg.Dispatch(sh, "name", []string{"resolve", "lab"}); err == nil {
		t.Fatal("name resolve succeeded after unbind")
	}
}

// TestName_UnbindUnknownSaysSo is the message the extra `:list` buys. The
// kernel's unbind is idempotent, so without this the user is told a name
// they mistyped was removed — and walks away believing the real binding is
// gone.
func TestName_UnbindUnknownSaysSo(t *testing.T) {
	sh := nameShell(t)
	res, err := shellcmd.Default().Dispatch(sh, "name", []string{"unbind", "never-bound"})
	if err != nil {
		t.Fatalf("name unbind (unknown): %v", err)
	}
	if !strings.Contains(res.Message, "was not bound") {
		t.Errorf("unbinding an unbound name reported %q; it must not read as a successful removal", res.Message)
	}
}

// TestName_ResolveReportsTheRung pins the typed-Outcome surfacing. A name
// that resolves nowhere must say WHERE it stopped — "the name rung" — and
// not just fail. This is the difference between "no such name" and "found
// it, cannot reach it", which send a user in opposite directions.
func TestName_ResolveReportsTheRung(t *testing.T) {
	sh := nameShell(t)
	_, err := shellcmd.Default().Dispatch(sh, "name", []string{"resolve", "nobody-bound-this"})
	if err == nil {
		t.Fatal("resolving an unbound name succeeded")
	}
	if !strings.Contains(err.Error(), "name rung") {
		t.Errorf("resolve failure did not name the rung it stopped at: %v", err)
	}
}

// TestName_BindAcceptsAliasSigil covers the ergonomic that makes the verb
// usable: after `connect`, a user names the peer they just connected to by
// its alias rather than by 40-odd opaque characters.
func TestName_BindAcceptsAliasSigil(t *testing.T) {
	sh := nameShell(t)
	reg := shellcmd.Default()
	selfID := sh.Local.PeerID

	if _, err := reg.Dispatch(sh, "name", []string{"bind", "me", "@alice"}); err != nil {
		t.Fatalf("name bind @alias: %v", err)
	}
	res, err := reg.Dispatch(sh, "name", []string{"resolve", "me"})
	if err != nil {
		t.Fatalf("name resolve: %v", err)
	}
	if !strings.Contains(strings.Join(res.Lines, "\n"), selfID) {
		t.Errorf("@alice did not resolve to the local peer-id %s:\n%s", selfID, strings.Join(res.Lines, "\n"))
	}
}

// TestName_ConfigShowsTheDefaultDispatch checks that `name config` renders
// the §4.1a list a distribution ships — the surface an operator uses to
// answer "which backends can see the names I type".
func TestName_ConfigShowsTheDefaultDispatch(t *testing.T) {
	sh := nameShell(t)
	res, err := shellcmd.Default().Dispatch(sh, "name", []string{"config"})
	if err != nil {
		t.Fatalf("name config: %v", err)
	}
	out := strings.Join(res.Lines, "\n")
	for _, want := range []string{"resolver_chain", "local-name", "name_format_dispatch", "did:web:*"} {
		if !strings.Contains(out, want) {
			t.Errorf("name config output missing %q:\n%s", want, out)
		}
	}
}

// TestName_RefusesPlainlyWhenSubstrateIsOff is the message that stands
// between a user and a dispatch 404. A peer without the registry extension
// must say the feature is off — not 404 in a way that reads as "no such
// name", which is the opposite diagnosis.
func TestName_RefusesPlainlyWhenSubstrateIsOff(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	sh := shellcmd.NewShell(ap, "alice", "")

	_, err = shellcmd.Default().Dispatch(sh, "name", []string{"ls"})
	if err == nil {
		t.Fatal("name ls succeeded on a peer with no registry substrate")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("substrate-off error should say the feature is off, got: %v", err)
	}
}

// TestName_UnknownFlagIsRefused: a mistyped flag must not become a
// positional. Binding a name literally called "-notse" would otherwise
// succeed and be very confusing to find later.
func TestName_UnknownFlagIsRefused(t *testing.T) {
	sh := nameShell(t)
	_, err := shellcmd.Default().Dispatch(sh, "name", []string{"bind", "x", "peer", "-notse", "oops"})
	if err == nil {
		t.Fatal("an unknown flag was accepted")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("want an unknown-flag error, got: %v", err)
	}
}
