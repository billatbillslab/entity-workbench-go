package shellboot

import (
	"context"
	"path/filepath"
	"testing"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

// TestBootstrap_Memory verifies the default (ephemeral, in-memory)
// bootstrap path produces a usable AppPeer + ShellWorkspace, and that
// the workbench handler refs are wired on the workspace.
func TestBootstrap_Memory(t *testing.T) {
	ctx := context.Background()
	ap, ws, err := Bootstrap(ctx, Config{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer ap.Close()

	if ap.PeerID() == "" {
		t.Fatalf("AppPeer has empty PeerID")
	}
	if ws.Local == nil || ws.Local.Peer != ap {
		t.Fatalf("workspace Local does not point at the returned AppPeer")
	}
	if ws.Local.Alias != "self" {
		t.Fatalf("default alias should be %q, got %q", "self", ws.Local.Alias)
	}
	if ws.NotificationIngest == nil {
		t.Fatalf("NotificationIngest not wired on workspace")
	}
}

// TestBootstrap_SQLiteInMemory verifies that StorageKind=sqlite with
// ":memory:" runs the SQL backend through the SDK without touching
// disk. Exercises the SQL path in CI without temp-dir setup.
func TestBootstrap_SQLiteInMemory(t *testing.T) {
	ctx := context.Background()
	ap, ws, err := Bootstrap(ctx, Config{
		StorageKind: "sqlite",
		StoragePath: ":memory:",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer ap.Close()
	if ws == nil {
		t.Fatalf("nil workspace")
	}
}

// TestBootstrap_SQLiteRequiresPathOrIdentity verifies the safety
// check: -storage=sqlite with neither an explicit path nor an
// identity name to derive from is rejected before it can land an
// orphan store.
func TestBootstrap_SQLiteRequiresPathOrIdentity(t *testing.T) {
	ctx := context.Background()
	_, _, err := Bootstrap(ctx, Config{StorageKind: "sqlite"})
	if err == nil {
		t.Fatalf("expected error when storage=sqlite with no path or identity")
	}
}

// TestBootstrap_AliasFromIdentity verifies the LocalAlias fallback
// chain: explicit alias takes precedence, else identity name, else
// "self".
func TestBootstrap_AliasFromIdentity(t *testing.T) {
	ctx := context.Background()

	// Build a sqlite-backed peer in a temp dir so we can pass a real
	// path without an identity binding (which would try to load from
	// ~/.entity).
	tmpDir := t.TempDir()
	storagePath := filepath.Join(tmpDir, "store.db")

	ap, ws, err := Bootstrap(ctx, Config{
		LocalAlias:  "myname",
		StorageKind: "sqlite",
		StoragePath: storagePath,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer ap.Close()

	if ws.Local.Alias != "myname" {
		t.Fatalf("explicit LocalAlias should win, got %q", ws.Local.Alias)
	}
}

// TestBootstrap_ANameDisclosingConfigDoesNotStopTheBoot crosses the seam
// between the SDK's EnsureResolverConfig and the shipped startup
// sequence: a peer whose stored resolver-config trips EXTENSION-REGISTRY
// §4.1 step 2 **starts**, keeps the operator's bytes, and carries the
// condition as a diagnostic.
//
// It is a real-session test — two Bootstraps over one SQLite file under
// one keypair, so the second boot reads what the first one left — rather
// than a unit call, because the defect it fences was ONLY at this seam.
// EnsureResolverConfig was correct in isolation and shellboot wrapped its
// error into a fatal `return nil, nil, err`; every unit test on either
// side was green while the shipped binary refused to start. That is
// AP21's shape, and D22 is the rule: a contract between two components is
// only tested by a test that crosses it.
//
// The spec: §4.1 [MUST, v1.17] — "at load: surface it, never normalize
// it, never refuse to start". Refusing to boot deletes the operator
// override the same paragraph grants, exactly as normalizing would.
//
// Tier: real-session.
func TestBootstrap_ANameDisclosingConfigDoesNotStopTheBoot(t *testing.T) {
	ctx := context.Background()
	storagePath := filepath.Join(t.TempDir(), "store.db")

	// One keypair across both boots. Without it each Bootstrap generates a
	// fresh one and writes under a different peer-id in the same file, so
	// the second boot would read an empty namespace and the test would
	// pass without ever loading the config it is about (the ephemeral-shell
	// re-namespacing issue, STATUS "Open bugs").
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatalf("crypto.Generate: %v", err)
	}
	cfg := Config{
		LocalAlias:       "op",
		StorageKind:      "sqlite",
		StoragePath:      storagePath,
		ExtraPeerOptions: []peer.Option{peer.WithIdentity(kp)},
	}

	// Boot 1 — clean. Then the operator deliberately installs a config
	// that discloses names, via the direct tree write §4.3 keeps open as
	// the out-of-band seed path (InstallResolverConfig refuses it, which
	// is the write-side MUST and is not what is under test here).
	ap, _, err := Bootstrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Bootstrap (clean): %v", err)
	}
	violating := types.ResolverConfigData{
		ResolverChain: []types.ResolverChainEntry{{BackendKind: types.BackendKindDNSTXT, Priority: 0}},
		NameFormatDispatch: []types.DispatchEntry{
			{Pattern: entitysdk.CatchAllPattern, BackendKinds: []string{types.BackendKindDNSTXT}},
		},
	}
	ent, err := violating.ToEntity()
	if err != nil {
		t.Fatalf("ToEntity: %v", err)
	}
	if _, err := ap.PutEntity(types.ResolverConfigStoragePath, ent); err != nil {
		t.Fatalf("seed the violating config: %v", err)
	}
	if err := ap.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Boot 2 — this is the assertion. Before 2026-08-19 it returned
	// "resolver-config refused at load" and the binary exited.
	ap2, _, err := Bootstrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Bootstrap refused to start on a name-disclosing stored config: %v\n"+
			"EXTENSION-REGISTRY §4.1 [MUST, v1.17] says surface it and run; a peer that will not "+
			"boot on a config its operator deliberately wrote has revoked the override the spec "+
			"grants them in the same paragraph", err)
	}
	defer ap2.Close()

	if diag := ap2.ResolverConfigDiagnostic(); diag == nil {
		t.Error("the peer booted with no diagnostic recorded; 'surface it' is the other half of the " +
			"MUST, and a boot that is silent about a disclosing config is the failure mode")
	}

	// And it is running under the operator's config, not a repaired copy.
	// A boot that quietly reinstalled the default would satisfy the
	// assertions above and be the normalization 1.17 forbids outright.
	got, found, _ := ap2.ResolverConfig()
	if !found {
		t.Fatal("the stored config vanished across the boot")
	}
	if len(got.NameFormatDispatch) != 1 ||
		got.NameFormatDispatch[0].BackendKinds[0] != types.BackendKindDNSTXT {
		t.Errorf("the boot rewrote the operator's config (%+v); a resolver MUST NOT rewrite stored "+
			"configuration as a side effect of reading it", got)
	}
}
