package shell

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/shellcmd"
	"go.entitychurch.org/entity-core-go/core/crypto"
)

// newTestApp builds an App over a fresh local AppPeer and seeds a few
// known paths so completion has something to enumerate.
func newTestApp(t *testing.T) *App {
	t.Helper()
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(Config{
		LocalAlias: "local",
		PeerConfig: entitysdk.PeerConfig{Keypair: &kp},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })

	// Seed scratch entries so completion has predictable rows.
	if _, err := app.local.Store().Put("scratch/alpha", "test/v", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := app.local.Store().Put("scratch/beta", "test/v", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := app.local.Store().Put("scratch/sub/leaf", "test/v", 3); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestSplitPathToken(t *testing.T) {
	tests := []struct {
		in   string
		dir  string
		leaf string
	}{
		{"", "", ""},
		{"foo", "", "foo"},
		{"foo/", "foo/", ""},
		{"foo/bar", "foo/", "bar"},
		{"local:", "local:", ""},
		{"local:foo", "local:", "foo"},
		{"local:foo/bar", "local:foo/", "bar"},
		{"/local/", "/local/", ""},
		{"/local/sys", "/local/", "sys"},
	}
	for _, tt := range tests {
		dir, leaf := splitPathToken(tt.in)
		if dir != tt.dir || leaf != tt.leaf {
			t.Errorf("splitPathToken(%q) = (%q, %q); want (%q, %q)",
				tt.in, dir, leaf, tt.dir, tt.leaf)
		}
	}
}

func TestSplitVerb(t *testing.T) {
	tests := []struct {
		in       string
		verb     string
		argStart int
		hasArg   bool
	}{
		{"", "", 0, false},
		{"ls", "ls", 0, false},
		{"ls ", "ls", 3, true},
		{"ls foo", "ls", 3, true},
		{"ls   foo", "ls", 5, true},
	}
	for _, tt := range tests {
		v, a, h := splitVerb(tt.in)
		if v != tt.verb || a != tt.argStart || h != tt.hasArg {
			t.Errorf("splitVerb(%q) = (%q, %d, %v); want (%q, %d, %v)",
				tt.in, v, a, h, tt.verb, tt.argStart, tt.hasArg)
		}
	}
}

func TestCompleter_Verbs(t *testing.T) {
	app := newTestApp(t)

	got := app.completer("c")
	want := []string{"cat", "cd", "compute", "connect", "continuation", "count", "cp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("verb completion: got %v, want %v", got, want)
	}

	got = app.completer("disc")
	want = []string{"disconnect"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("verb completion: got %v, want %v", got, want)
	}
}

func TestCompleter_PathAtRoot(t *testing.T) {
	app := newTestApp(t)
	// WD is "/", so completing `ls ` should suggest the canonical
	// `@local/` form per GUIDE-SHELL-FRAMING.md §3.4. Legacy
	// `local:` and `/local/` forms still parse, but completion
	// teaches the canonical sigil form.
	got := app.completer("ls ")
	if !contains(got, "ls @local/") {
		t.Errorf("expected `ls @local/` in candidates; got %v", got)
	}

	// Absolute-form: `ls /loc<TAB>` → `ls /@local/` (canonical).
	got = app.completer("ls /loc")
	if !contains(got, "ls /@local/") {
		t.Errorf("expected `ls /@local/` in candidates; got %v", got)
	}

	// Canonical-absolute typing: `ls /@lo<TAB>` → `ls /@local/`.
	got = app.completer("ls /@lo")
	if !contains(got, "ls /@local/") {
		t.Errorf("expected `ls /@local/` in candidates; got %v", got)
	}
}

func TestCompleter_AtAliasShortcut(t *testing.T) {
	app := newTestApp(t)
	// `ls @lo<TAB>` → `ls @local/` (shortcut form per §3.4).
	got := app.completer("ls @lo")
	if !contains(got, "ls @local/") {
		t.Errorf("expected `ls @local/` in candidates; got %v", got)
	}

	// Inside the peer via shortcut: `ls @local/scrat<TAB>` →
	// `ls @local/scratch/`.
	got = app.completer("ls @local/scrat")
	if !contains(got, "ls @local/scratch/") {
		t.Errorf("expected `ls @local/scratch/` in candidates; got %v", got)
	}
}

func TestCompleter_PathInsidePeer(t *testing.T) {
	app := newTestApp(t)
	// `ls local:scrat<TAB>` should expand to `ls local:scratch/`.
	got := app.completer("ls local:scrat")
	if !contains(got, "ls local:scratch/") {
		t.Errorf("expected `ls local:scratch/` in candidates; got %v", got)
	}

	// Listing the directory: candidates are children with trailing
	// slash for those that have sub-children.
	got = app.completer("ls local:scratch/")
	wantAny := []string{"ls local:scratch/alpha", "ls local:scratch/beta", "ls local:scratch/sub/"}
	for _, w := range wantAny {
		if !contains(got, w) {
			t.Errorf("expected %q in candidates; got %v", w, got)
		}
	}

	// Filter by leaf prefix.
	got = app.completer("cat local:scratch/al")
	if !contains(got, "cat local:scratch/alpha") {
		t.Errorf("expected `cat local:scratch/alpha`; got %v", got)
	}
	for _, c := range got {
		if strings.Contains(c, "beta") {
			t.Errorf("did not expect `beta` candidates; got %v", got)
		}
	}
}

func TestCompleter_AbsoluteAliasForm(t *testing.T) {
	app := newTestApp(t)
	got := app.completer("ls /local/scrat")
	if !contains(got, "ls /local/scratch/") {
		t.Errorf("expected `ls /local/scratch/` in candidates; got %v", got)
	}
}

func TestCompleter_RelativeAfterCD(t *testing.T) {
	app := newTestApp(t)
	// Move WD into the local peer.
	app.sh.WD = shellcmd.Path("/" + app.local.PeerID() + "/")

	// Now `ls scrat<TAB>` (relative) should complete from WD.
	got := app.completer("ls scrat")
	if !contains(got, "ls scratch/") {
		t.Errorf("expected `ls scratch/`; got %v", got)
	}

	// And `ls scratch/al<TAB>` should complete to `scratch/alpha`.
	got = app.completer("ls scratch/al")
	if !contains(got, "ls scratch/alpha") {
		t.Errorf("expected `ls scratch/alpha`; got %v", got)
	}
}

func contains(haystack []string, needle string) bool {
	idx := sort.SearchStrings(haystack, needle)
	return idx < len(haystack) && haystack[idx] == needle
}
