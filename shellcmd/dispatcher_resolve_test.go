package shellcmd

import (
	"reflect"
	"testing"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/crypto"
)

// TestDispatcher_ResolvesPathArgs confirms Registry.Dispatch applies
// the Command.ResolveArgs callback before invoking Handler. Per
// GUIDE-SHELL-FRAMING.md §8.1: alias resolution happens at the
// dispatcher tier; verb-parsers receive already-resolved paths.
func TestDispatcher_ResolvesPathArgs(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	sh := NewShell(ap, "local", "")

	var got []string
	r := NewRegistry()
	r.Register(Command{
		Name: "probe",
		Handler: func(_ *Shell, args []string) (Result, error) {
			got = append([]string(nil), args...)
			return Result{}, nil
		},
		ResolveArgs: PathArgs(0),
	})

	if _, err := r.Dispatch(sh, "probe", []string{"@local/foo"}); err != nil {
		t.Fatal(err)
	}
	wantPrefix := "/" + ap.PeerID() + "/"
	if len(got) != 1 || got[0] != wantPrefix+"foo" {
		t.Errorf("dispatcher did not pre-resolve args[0]: got %v, want [%s%s]", got, wantPrefix, "foo")
	}
}

// TestDispatcher_ResolveArgs_MultiplePositions confirms PathArgs
// handles multi-position declarations (cp <src> <dst> shape).
func TestDispatcher_ResolveArgs_MultiplePositions(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	sh := NewShell(ap, "local", "")

	var got []string
	r := NewRegistry()
	r.Register(Command{
		Name: "probe",
		Handler: func(_ *Shell, args []string) (Result, error) {
			got = append([]string(nil), args...)
			return Result{}, nil
		},
		ResolveArgs: PathArgs(0, 1),
	})

	if _, err := r.Dispatch(sh, "probe", []string{"@local/a", "@local/b"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/" + ap.PeerID() + "/a",
		"/" + ap.PeerID() + "/b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestDispatcher_ResolveArgs_OutOfRangeSafe confirms PathArgs
// skips positions that don't exist (optional args the user didn't
// supply).
func TestDispatcher_ResolveArgs_OutOfRangeSafe(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	sh := NewShell(ap, "local", "")

	r := NewRegistry()
	r.Register(Command{
		Name: "probe",
		Handler: func(_ *Shell, args []string) (Result, error) {
			return Result{}, nil
		},
		ResolveArgs: PathArgs(0, 1, 5), // 5 is out of range
	})

	// Passing one arg; PathArgs(0,1,5) should not panic on missing positions.
	if _, err := r.Dispatch(sh, "probe", []string{"@local"}); err != nil {
		t.Fatal(err)
	}
}

// TestDispatcher_NilResolveArgs_PassesThrough confirms commands
// without ResolveArgs receive raw args. Flag-interleaved verbs (ls,
// cat, tree) rely on this — their handlers do their own resolution
// because the dispatcher can't tell which arg is the path without
// parsing the flags.
func TestDispatcher_NilResolveArgs_PassesThrough(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	sh := NewShell(ap, "local", "")

	var got []string
	r := NewRegistry()
	r.Register(Command{
		Name: "probe",
		Handler: func(_ *Shell, args []string) (Result, error) {
			got = append([]string(nil), args...)
			return Result{}, nil
		},
		// ResolveArgs: nil
	})

	input := []string{"@local/foo", "-flag", "value"}
	if _, err := r.Dispatch(sh, "probe", input); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, input) {
		t.Errorf("nil ResolveArgs should pass through: got %v, want %v", got, input)
	}
}

// TestDispatcher_ResolveArgs_IdempotentWithHandlerResolve confirms
// that handlers that ALSO call sh.Resolve still work — the
// dispatcher-tier resolution feeds an already-resolved path into
// the handler, and sh.Resolve on an already-resolved path is a
// no-op. This is the property that makes flag-interleaved verbs
// (which still do handler-side resolution) compose safely with
// dispatcher-tier resolution.
func TestDispatcher_ResolveArgs_IdempotentWithHandlerResolve(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	sh := NewShell(ap, "local", "")

	r := NewRegistry()
	r.Register(Command{
		Name: "probe",
		Handler: func(sh *Shell, args []string) (Result, error) {
			// Simulate a handler that calls sh.Resolve defensively.
			target := sh.Resolve(args[0])
			expected := Path("/" + ap.PeerID() + "/foo")
			if target != expected {
				t.Errorf("handler-side sh.Resolve(dispatcher-resolved) = %q, want %q", target, expected)
			}
			return Result{}, nil
		},
		ResolveArgs: PathArgs(0),
	})

	if _, err := r.Dispatch(sh, "probe", []string{"@local/foo"}); err != nil {
		t.Fatal(err)
	}
}
