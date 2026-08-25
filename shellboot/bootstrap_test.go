package shellboot

import (
	"context"
	"os"
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

// TestBootstrap_SQLiteWithNoPathOrIdentityUsesTheDefault replaces
// TestBootstrap_SQLiteRequiresPathOrIdentity, which asserted the
// refusal this change deliberately removed. **The reversal is recorded
// rather than the old test quietly deleted**, because the old behaviour
// was defensible and the reason it changed is not obvious from the
// diff.
//
// The refusal existed to stop an "orphan store" — a database landing
// somewhere nobody chose. But the orphan it was guarding against is not
// what happened: the caller who supplied `-storage-path` and no identity
// sailed straight past it and got something worse, a store whose
// contents were invisible to the next run because the peer-id changed
// underneath it. The refusal covered the case where the path was
// unknown and missed the case where the *identity* was.
//
// Both are now answered by the same default: NAME defaults to
// `default`, the store lands at the GUIDE-PERSISTENCE §1.1 path derived
// from it, and the peer-id is stable across runs. Nothing is orphaned —
// the location is derived from a name an operator can see and reuse.
//
// Tier: contract pin.
func TestBootstrap_SQLiteWithNoPathOrIdentityUsesTheDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ctx := context.Background()
	ap, _, err := Bootstrap(ctx, Config{StorageKind: "sqlite"})
	if err != nil {
		t.Fatalf("Bootstrap with neither path nor identity: %v\n"+
			"This used to be refused to prevent an orphan store; it now derives "+
			"~/.entity/peers/%s/store.db, which is not orphaned — the location comes from a "+
			"name an operator can see and reuse", err, DefaultIdentityName)
	}
	defer ap.Close()

	want := filepath.Join(home, ".entity", "peers", DefaultIdentityName, "store.db")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("no store at the derived path %s (%v); the derivation is the whole reason the "+
			"refusal could be dropped", want, err)
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

// TestBootstrap_SQLiteWithoutIdentityIsStableAcrossRuns is the
// regression fence for the ephemeral-shell re-namespacing bug.
//
// **What was broken.** `entity-shell -storage sqlite` with no
// `-identity` generated a fresh keypair per invocation. The tree is
// peer-id-namespaced end to end, so every run wrote under a different
// namespace of the same database and nothing the previous run stored was
// visible to the next. It presented to a user as "persistence does not
// work", it affected **every** persistent surface rather than one
// feature, and the database accreted a full bootstrap per run on top.
//
// **Why no existing test saw it.** A peer-id only changes across a
// PROCESS, and every suite here builds its peers in-process and keeps
// them. The bug was found driving the shipped binary across separate
// invocations (D10 — headless/in-process green is necessary, not
// sufficient). This test is the cheap standing version of that: two
// Bootstraps, one HOME, no identity named, asserting the peer-id is the
// same both times.
//
// It asserts the PEER-ID rather than "a file exists", because the
// peer-id is the thing the namespace is keyed on — a fix that created an
// identity and then failed to bind it would pass a file check and leave
// the bug exactly where it was.
//
// Tier: real-session.
func TestBootstrap_SQLiteWithoutIdentityIsStableAcrossRuns(t *testing.T) {
	// Redirect the identity + peer directories into the test's own HOME
	// so this never reads or writes the developer's real ~/.entity.
	home := t.TempDir()
	t.Setenv("HOME", home)

	ctx := context.Background()
	cfg := Config{StorageKind: "sqlite", StoragePath: filepath.Join(t.TempDir(), "store.db")}

	ap1, _, err := Bootstrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Bootstrap (first run): %v", err)
	}
	first := ap1.PeerID()
	if err := ap1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ap2, _, err := Bootstrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Bootstrap (second run): %v", err)
	}
	defer ap2.Close()
	second := ap2.PeerID()

	if first != second {
		t.Fatalf("a persistent store came up under two different peer-ids across runs:\n"+
			"  run 1: %s\n  run 2: %s\n"+
			"The tree is peer-id-namespaced, so the second run cannot see anything the first "+
			"wrote — and the database accretes a fresh bootstrap every time. A persistent store "+
			"under a per-invocation keypair is not persistence.", first, second)
	}
	if first == "" {
		t.Fatal("peer-id is empty on both runs; the comparison above would pass vacuously")
	}

	// The identity it used is a real, nameable one — an operator who
	// never asked for an identity must still be able to see the one they
	// got, name it in a later -identity, and delete it.
	if _, err := entitysdk.LoadIdentity(DefaultIdentityName); err != nil {
		t.Errorf("the default identity is not loadable by name (%v); a keypair an operator "+
			"cannot see or name is one they cannot manage", err)
	}
}

// TestBootstrap_MemoryStorageStaysEphemeral is the control arm for the
// test above, and it is what keeps that fix from becoming "every peer
// gets a durable on-disk identity now".
//
// An in-memory peer keeps its per-invocation keypair, and that is
// coherent rather than an oversight: nothing survives the process
// either way, so there is no state for a stable id to be the key to.
// Creating an on-disk keypair for a peer that stores nothing would write
// to a user's home directory for a run that asked to leave no trace.
//
// Tier: contract pin.
func TestBootstrap_MemoryStorageStaysEphemeral(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ctx := context.Background()
	ap1, _, err := Bootstrap(ctx, Config{})
	if err != nil {
		t.Fatalf("Bootstrap (first): %v", err)
	}
	first := ap1.PeerID()
	_ = ap1.Close()

	ap2, _, err := Bootstrap(ctx, Config{})
	if err != nil {
		t.Fatalf("Bootstrap (second): %v", err)
	}
	defer ap2.Close()

	if first == ap2.PeerID() {
		t.Error("two in-memory peers came up under the SAME peer-id; the sqlite fix leaked into " +
			"the ephemeral path, which now writes a durable keypair for a run that stores nothing")
	}
	if _, err := os.Stat(filepath.Join(home, ".entity", "identities", DefaultIdentityName)); err == nil {
		t.Errorf("an in-memory bootstrap created %s in the operator's home; a peer that persists "+
			"nothing must not leave a keypair behind", DefaultIdentityName)
	}
}
