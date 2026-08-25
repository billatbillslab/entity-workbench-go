package shellcmd

import (
	"testing"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/crypto"
)

// TestShell_ResolveAliasForms verifies Shell.Resolve interprets
// alias-as-prefix, alias-as-first-absolute-segment, and bare/peer-id
// forms consistently. Regression for the bug where `/local/system`
// was treated as a literal peer-id.
func TestShell_ResolveAliasForms(t *testing.T) {
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
	want := "/" + ap.PeerID() + "/"

	tests := []struct {
		input string
		want  Path
	}{
		// Canonical @alias forms (current, per GUIDE-SHELL-FRAMING.md §3.4).
		{"@local", Path("/" + ap.PeerID() + "/")},
		{"@local/", Path(want)},
		{"@local/system/handler", Path(want + "system/handler")},
		{"/@local", Path("/" + ap.PeerID() + "/")},
		{"/@local/", Path(want)},
		{"/@local/system/handler", Path(want + "system/handler")},
		// Legacy alias-as-first-segment (peer-id form pass-through).
		{"/local/", Path(want)},
		{"/local/system/handler", Path(want + "system/handler")},
		{"/local", Path("/" + ap.PeerID())},
		// Deprecated alias: form — accepted for one release per
		// GUIDE-SHELL-FRAMING.md §3.4 deprecation window.
		{"local:", Path(want)},
		{"local:system/handler", Path(want + "system/handler")},
		// Root + bare relative.
		{"/", "/"},
		{"foo/bar", "/foo/bar"}, // bare path — no alias expansion possible
		// Entity name containing ":" — must NOT be parsed as an alias
		// prefix because it appears after a "/". Per §3.4 the `:`
		// reservation is grammatical, not character-level.
		{"/local/system/handler:register", Path(want + "system/handler:register")},
	}

	for _, tt := range tests {
		got := sh.Resolve(tt.input)
		if got != tt.want {
			t.Errorf("Resolve(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestShell_ResolveAliasUnknown ensures unknown aliases pass through
// rather than silently being treated as a peer-id.
func TestShell_ResolveAliasUnknown(t *testing.T) {
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

	// Both deprecated and canonical forms of an unknown alias should
	// pass through with the alias literal as the first segment, so the
	// downstream "no connection for path" is the user-visible error.
	if got := sh.Resolve("remote:foo/bar"); got != "/remote/foo/bar" {
		t.Errorf("unknown alias: passthrough: got %q, want /remote/foo/bar", got)
	}
	if got := sh.Resolve("@remote/foo/bar"); got != "/remote/foo/bar" {
		t.Errorf("unknown @alias: passthrough: got %q, want /remote/foo/bar", got)
	}
	if got := sh.Resolve("/@remote/foo/bar"); got != "/remote/foo/bar" {
		t.Errorf("unknown /@alias: passthrough: got %q, want /remote/foo/bar", got)
	}
}
