package entitysdk_test

import (
	"path/filepath"
	"testing"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/crypto"
)

// registry_bootstrap_cost_test.go — the measurement behind a default.
//
// `CreatePeer`'s registry extension is opt-in, and app.go's comment gave two
// reasons. The second was a cost claim about the SIBLING, not about us: "the
// local-name handler's default-grant caps are re-minted (not deduplicated) on
// every bootstrap, so wiring it default-on grows a peer's rebootstrap
// footprint linearly."
//
// That claim was the whole reason `entity-shell` had no name resolution:
// shellboot never set Extensions.Registry, so the peer carried no registry
// handler, so no `name` verb could have worked even once one existed. Before
// inheriting the default we measured it (D20 — a cost attributed to the
// substrate is a hypothesis until the operation has been run end to end).
//
// **Measured 2026-08-19, and the claim does not hold.** The per-restart
// marginal cost of the registry extension is ZERO on both counters. What the
// original observation almost certainly caught is the +2 entities/restart
// that a registry-LESS peer pays too — a baseline, not the registry's.
//
// Tier: substrate probe (TESTING-STRATEGY.md §4) — it measures the sibling's
// behavior through our configuration surface. It asserts the DIFFERENTIAL,
// never an absolute count: the absolute numbers move whenever core-go adds a
// handler, and pinning them would make this a tripwire for unrelated work.
//
// **The keypair must be pinned across opens, and this is the trap.** A
// zero-value PeerConfig generates a FRESH keypair, so a loop that reopens the
// same database without one is not measuring restarts at all — it is N
// different peers sharing a file, each bootstrapping its own namespace. The
// first version of this probe did exactly that and reported Δpaths=370 per
// "reopen" in BOTH arms, which reads as a catastrophic leak and is really
// just PathCount doing its job (Store.PathCount is LenPrefix(""), a COUNT(*)
// over the index, not over one namespace). The control is what caught it:
// the arm under test and the arm without it were equally "catastrophic",
// which is never what a real per-extension leak looks like.

// reopenSeries opens the same database as the same peer `n` times, returning
// the (paths, entities) counts each fresh peer sees. registryOn selects the
// arm. Each peer is closed before the next open, so this is a restart series,
// not concurrent handles.
func reopenSeries(t *testing.T, n int, registryOn bool) (paths, entities []int) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "peer.db")
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		cfg := entitysdk.PeerConfig{
			Keypair: &kp,
			Storage: entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
		}
		if registryOn {
			cfg.Extensions.Registry = &entitysdk.RegistryConfig{}
		}
		ap, err := entitysdk.CreatePeer(cfg)
		if err != nil {
			t.Fatalf("open %d (registry=%v): %v", i, registryOn, err)
		}
		paths = append(paths, ap.PathCount())
		entities = append(entities, ap.EntityCount())
		ap.Close()
	}
	return paths, entities
}

// steadyDelta returns the per-restart growth once the first open's
// population is done — the mean of the deltas from index 1 onward. It
// returns -1 when the series is not linear, so a caller cannot read a
// misleading average over a non-linear shape.
func steadyDelta(series []int) int {
	if len(series) < 3 {
		return -1
	}
	d := series[2] - series[1]
	for i := 3; i < len(series); i++ {
		if series[i]-series[i-1] != d {
			return -1
		}
	}
	return d
}

// TestRegistryExtension_AddsNoMarginalRestartCost is the assertion that
// decides shellboot's default: whatever a peer pays per restart, it must not
// pay MORE for carrying the registry. An absolute leak here would be core-go's
// and is out of our hands; a differential leak would be a real reason to keep
// name resolution off by default, which is what the old comment claimed.
func TestRegistryExtension_AddsNoMarginalRestartCost(t *testing.T) {
	const reopens = 5
	onPaths, onEntities := reopenSeries(t, reopens, true)
	offPaths, offEntities := reopenSeries(t, reopens, false)

	t.Logf("restart series over %d opens of one sqlite db, same keypair:", reopens)
	for i := 0; i < reopens; i++ {
		t.Logf("  open %d   ON: paths=%4d entities=%4d   OFF: paths=%4d entities=%4d",
			i, onPaths[i], onEntities[i], offPaths[i], offEntities[i])
	}

	onDP, offDP := steadyDelta(onPaths), steadyDelta(offPaths)
	onDE, offDE := steadyDelta(onEntities), steadyDelta(offEntities)
	t.Logf("steady per-restart growth — paths: ON %d / OFF %d   entities: ON %d / OFF %d",
		onDP, offDP, onDE, offDE)

	if onDP < 0 || offDP < 0 || onDE < 0 || offDE < 0 {
		t.Fatalf("a restart series was not linear (paths ON=%v OFF=%v, entities ON=%v OFF=%v); "+
			"the steady-state delta is not meaningful and this probe cannot rule on the default",
			onPaths, offPaths, onEntities, offEntities)
	}
	if onDP != offDP {
		t.Errorf("registry costs %d extra paths per restart (ON %d vs OFF %d)", onDP-offDP, onDP, offDP)
	}
	if onDE != offDE {
		t.Errorf("registry costs %d extra entities per restart (ON %d vs OFF %d)", onDE-offDE, onDE, offDE)
	}

	// The baseline is not ours and is recorded rather than asserted: a peer
	// carrying NO registry still accretes entities per restart. That belongs
	// to the same core-go ceremony family as the waived identity-rebootstrap
	// leak (storage_sqlite_identity_test.go) and is routed, not fixed here.
	if offDE > 0 {
		t.Logf("OBSERVATION (core-go, not registry): a registry-less peer accretes "+
			"%d entities per restart with no identity ceremony run; paths stay flat. "+
			"Same family as the waived identity-rebootstrap leak.", offDE)
	}
}

// TestRegistryExtension_OneTimeInstallCost prices what an operator pays for
// `name` to exist at all: one fresh database opened each way. The probe above
// says the cost does not recur; this one says how big the one-time cost is,
// and it is the number that goes in the commit message rather than an
// adjective.
func TestRegistryExtension_OneTimeInstallCost(t *testing.T) {
	onPaths, onEntities := reopenSeries(t, 1, true)
	offPaths, offEntities := reopenSeries(t, 1, false)
	t.Logf("first open — OFF: paths=%d entities=%d   ON: paths=%d entities=%d   (Δpaths=%+d Δentities=%+d)",
		offPaths[0], offEntities[0], onPaths[0], onEntities[0],
		onPaths[0]-offPaths[0], onEntities[0]-offEntities[0])
}
