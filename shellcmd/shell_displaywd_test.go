package shellcmd

import (
	"testing"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/crypto"
)

// TestShell_DisplayWD_LocalAlias confirms WD on the local peer
// reverse-resolves to "/@<local-alias>/...". Per
// GUIDE-SHELL-FRAMING.md §6.5 — WD stores resolved form; display
// reverse-resolves against current alias table.
func TestShell_DisplayWD_LocalAlias(t *testing.T) {
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
	sh.WD = Path("/" + ap.PeerID() + "/foo/bar")

	got := sh.DisplayWD()
	want := Path("/@local/foo/bar")
	if got != want {
		t.Errorf("DisplayWD() = %q, want %q", got, want)
	}
}

// TestShell_DisplayWD_PeerRoot confirms the peer-root display
// preserves the trailing slash.
func TestShell_DisplayWD_PeerRoot(t *testing.T) {
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
	sh.WD = Path("/" + ap.PeerID() + "/")

	got := sh.DisplayWD()
	want := Path("/@local/")
	if got != want {
		t.Errorf("DisplayWD() = %q, want %q", got, want)
	}
}

// TestShell_DisplayWD_UnmappedPeer confirms a WD against an
// unmapped peer-id displays as-is (resolved form), per §6.5
// fallback behavior.
func TestShell_DisplayWD_UnmappedPeer(t *testing.T) {
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
	// Point WD at a peer-id with no alias binding.
	sh.WD = Path("/unmapped_peer_xyz/some/path")

	got := sh.DisplayWD()
	want := Path("/unmapped_peer_xyz/some/path")
	if got != want {
		t.Errorf("DisplayWD() with unmapped peer = %q, want resolved-form passthrough %q", got, want)
	}
}

// TestShell_DisplayWD_RootStaysRoot confirms "/" displays as "/".
func TestShell_DisplayWD_RootStaysRoot(t *testing.T) {
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
	sh.WD = Path("/")

	got := sh.DisplayWD()
	if got != Path("/") {
		t.Errorf("DisplayWD() at root = %q, want %q", got, "/")
	}
}

// TestShell_DisplayWD_ReflectsAliasTableChanges confirms reverse-
// resolution is done at display time, not snapshotted at cd. Per
// §6.5: "the display reflects the current alias state, not a stale
// snapshot."
func TestShell_DisplayWD_ReflectsAliasTableChanges(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	sh := NewShell(ap, "alpha", "")
	sh.WD = Path("/" + ap.PeerID() + "/x")

	if got := sh.DisplayWD(); got != Path("/@alpha/x") {
		t.Errorf("first display = %q, want /@alpha/x", got)
	}

	// Mutate the local alias. (In production this isn't a routine
	// operation, but the property under test — that DisplayWD reads
	// the alias table afresh each call — is what §6.5 pins.)
	sh.Local.Alias = "beta"

	if got := sh.DisplayWD(); got != Path("/@beta/x") {
		t.Errorf("after alias change, display = %q, want /@beta/x", got)
	}
}
