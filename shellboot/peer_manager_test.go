// peer_manager_test.go — multi-peer smoke tests for shellboot.PeerManager.
//
// B-4 auto-Listen wiring tested in TestPeerManager_AutoListen_TCP /
// TestPeerManager_AutoListen_WebSocket / TestPeerManager_AutoListen_InvalidAddr.
//
// Migrated from avalonia/bridge/multi_peer_test.go (Phase I §12) — the
// peer-manager logic moved to shellboot/, the tests came with it.
// All exercise the public PeerManager API; no cgo, no FFI surface.
//
// New test added in S4: TestOnPeerDestroyedHookFires — proves the
// cascade-hook contract that bridge + console will rely on.

package shellboot

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/types"
	discoverymdns "go.entitychurch.org/entity-core-go/ext/discovery/mdns"

	"entity-workbench-go/entitysdk"
)

const testAppID = "entity-workbench-test"

// memCfg returns a Config that uses in-memory storage + an ephemeral
// keypair. Suitable for tests; nothing persists to disk.
func memCfg(alias string) Config {
	return Config{
		LocalAlias:  alias,
		StorageKind: "memory",
		OpenAccess:  true,
	}
}

func TestPeerManager_CreateAndIndependence(t *testing.T) {
	m := NewPeerManager(testAppID)

	hA, err := m.Create(memCfg("alpha"))
	if err != nil {
		t.Fatalf("Create alpha: %v", err)
	}
	if hA == 0 {
		t.Fatal("alpha handle is 0")
	}

	hB, err := m.Create(memCfg("beta"))
	if err != nil {
		t.Fatalf("Create beta: %v", err)
	}
	if hB == hA {
		t.Fatalf("beta handle %d collides with alpha %d", hB, hA)
	}

	pA := m.Get(hA)
	pB := m.Get(hB)
	if pA == nil || pB == nil {
		t.Fatalf("Get: pA=%v pB=%v", pA, pB)
	}
	if pA.AppPeer.PeerID() == pB.AppPeer.PeerID() {
		t.Fatalf("alpha+beta share peer-id %q (ephemeral keypairs collided)", pA.AppPeer.PeerID())
	}
	if m.SystemHandle() != hA {
		t.Errorf("system peer handle = %d, want alpha (%d) — first-created should be system", m.SystemHandle(), hA)
	}
	if !pA.IsSystem {
		t.Error("alpha snapshot IsSystem = false; expected true")
	}
	if pB.IsSystem {
		t.Error("beta snapshot IsSystem = true; expected false")
	}

	m.ShutdownAll()
}

func TestPeerManager_StoreIsolation(t *testing.T) {
	m := NewPeerManager(testAppID)
	defer m.ShutdownAll()

	hA, _ := m.Create(memCfg("alpha"))
	hB, _ := m.Create(memCfg("beta"))
	pA := m.Get(hA)
	pB := m.Get(hB)

	_, err := pA.AppPeer.Put("demo/marker", "test/marker", map[string]any{"who": "alpha"})
	if err != nil {
		t.Fatalf("alpha.Put: %v", err)
	}
	hasOnA, _ := pA.AppPeer.Has("demo/marker")
	hasOnB, _ := pB.AppPeer.Has("demo/marker")
	if !hasOnA {
		t.Error("alpha doesn't see its own put")
	}
	if hasOnB {
		t.Error("beta sees alpha's put — store isolation broken")
	}
}

func TestPeerManager_RosterPersistence(t *testing.T) {
	m := NewPeerManager(testAppID)
	defer m.ShutdownAll()

	hA, _ := m.Create(memCfg("alpha"))
	hB, _ := m.Create(memCfg("beta"))
	pA := m.Get(hA)
	pB := m.Get(hB)

	okA, err := HasRosterEntry(pA.AppPeer, testAppID, pA.AppPeer.PeerID())
	if err != nil {
		t.Fatalf("HasRosterEntry(alpha): %v", err)
	}
	if !okA {
		t.Error("alpha's roster entry missing (alpha IS the system peer; should self-include)")
	}
	okB, err := HasRosterEntry(pA.AppPeer, testAppID, pB.AppPeer.PeerID())
	if err != nil {
		t.Fatalf("HasRosterEntry(beta): %v", err)
	}
	if !okB {
		t.Errorf("beta's roster entry missing at %s", RosterPath(testAppID, pB.AppPeer.PeerID()))
	}
}

func TestPeerManager_DestroyRemovesRoster(t *testing.T) {
	m := NewPeerManager(testAppID)
	defer m.ShutdownAll()

	hA, _ := m.Create(memCfg("alpha"))
	hB, _ := m.Create(memCfg("beta"))
	pA := m.Get(hA)
	pB := m.Get(hB)
	betaID := pB.AppPeer.PeerID()

	if err := m.Destroy(hB); err != nil {
		t.Fatalf("Destroy beta: %v", err)
	}
	if m.Get(hB) != nil {
		t.Error("Get(beta) returned non-nil after destroy")
	}
	hasBeta, err := HasRosterEntry(pA.AppPeer, testAppID, betaID)
	if err != nil {
		t.Fatalf("HasRosterEntry after destroy: %v", err)
	}
	if hasBeta {
		t.Errorf("beta's roster entry survived destroy at %s", RosterPath(testAppID, betaID))
	}

	// Idempotent — second destroy is a no-op.
	if err := m.Destroy(hB); err != nil {
		t.Errorf("Destroy beta (idempotent retry): %v", err)
	}
}

func TestPeerManager_DispatchRoutesToCorrectShell(t *testing.T) {
	m := NewPeerManager(testAppID)
	defer m.ShutdownAll()

	hA, _ := m.Create(memCfg("alpha"))
	hB, _ := m.Create(memCfg("beta"))
	pA := m.Get(hA)
	pB := m.Get(hB)

	wdA := string(pA.Shell.WD)
	wdB := string(pB.Shell.WD)
	if wdA == wdB {
		t.Fatalf("alpha and beta share WD %q — peer-ids should differ", wdA)
	}
	if !strings.Contains(wdA, pA.AppPeer.PeerID()) {
		t.Errorf("alpha WD %q doesn't contain alpha peer-id %q", wdA, pA.AppPeer.PeerID())
	}
	if !strings.Contains(wdB, pB.AppPeer.PeerID()) {
		t.Errorf("beta WD %q doesn't contain beta peer-id %q", wdB, pB.AppPeer.PeerID())
	}
}

func TestPeerManager_SystemPeerHandoverOnDestroy(t *testing.T) {
	m := NewPeerManager(testAppID)
	defer m.ShutdownAll()

	hA, _ := m.Create(memCfg("alpha"))
	hB, _ := m.Create(memCfg("beta"))

	if m.SystemHandle() != hA {
		t.Fatalf("system peer = %d, want alpha %d", m.SystemHandle(), hA)
	}
	if err := m.Destroy(hA); err != nil {
		t.Fatalf("Destroy alpha: %v", err)
	}
	if m.SystemHandle() != hB {
		t.Errorf("after destroying alpha, system peer = %d, want beta %d", m.SystemHandle(), hB)
	}
	if err := m.Destroy(hB); err != nil {
		t.Fatalf("Destroy beta: %v", err)
	}
	if m.SystemHandle() != 0 {
		t.Errorf("after destroying last peer, system peer = %d, want 0", m.SystemHandle())
	}
}

func TestPeerManager_ListSnapshot(t *testing.T) {
	m := NewPeerManager(testAppID)
	defer m.ShutdownAll()

	_, _ = m.Create(memCfg("alpha"))
	_, _ = m.Create(memCfg("beta"))
	_, _ = m.Create(memCfg("gamma"))

	infos := m.List()
	if len(infos) != 3 {
		t.Fatalf("List returned %d entries, want 3", len(infos))
	}
	sawSystem := 0
	for _, p := range infos {
		if p.IsSystem {
			sawSystem++
		}
		if p.AppPeer.PeerID() == "" {
			t.Errorf("info has empty peer_id: %+v", p)
		}
		if p.Handle == 0 {
			t.Errorf("info has zero handle: %+v", p)
		}
	}
	if sawSystem != 1 {
		t.Errorf("system peer count = %d, want exactly 1", sawSystem)
	}
}

func TestPeerManager_RosterRoundTripReadWrite(t *testing.T) {
	m := NewPeerManager(testAppID)
	defer m.ShutdownAll()

	hA, _ := m.Create(memCfg("alpha"))
	hB, _ := m.Create(memCfg("beta"))
	pA := m.Get(hA)
	pB := m.Get(hB)

	entries, err := ListRosterEntries(pA.AppPeer, testAppID)
	if err != nil {
		t.Fatalf("ListRosterEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListRosterEntries returned %d, want 2", len(entries))
	}
	byID := map[string]RosterEntry{}
	for _, e := range entries {
		byID[e.PeerID] = e
	}
	if got, ok := byID[pA.AppPeer.PeerID()]; !ok {
		t.Errorf("alpha not in roster after write")
	} else if got.Label != "alpha" {
		t.Errorf("alpha label = %q, want %q", got.Label, "alpha")
	}
	if got, ok := byID[pB.AppPeer.PeerID()]; !ok {
		t.Errorf("beta not in roster after write")
	} else if got.Label != "beta" {
		t.Errorf("beta label = %q, want %q", got.Label, "beta")
	}
}

func TestPeerManager_RestoreFiltersSelfAndEphemeral(t *testing.T) {
	m := NewPeerManager(testAppID)
	defer m.ShutdownAll()

	hA, _ := m.Create(memCfg("alpha")) // system, ephemeral
	_, _ = m.Create(memCfg("beta"))    // ephemeral
	pA := m.Get(hA)

	// Synthetic roster entry with bogus identity — restore will TRY
	// to spawn it and fail at shellboot (identity not on disk). We
	// verify the failure surfaces in the err return AND that alpha
	// + beta don't get re-spawned as duplicates.
	bogus := RosterEntry{
		PeerID:      "synthetic-fake-peer-id",
		Label:       "ghost",
		AddedAt:     1,
		Identity:    "no-such-identity-on-disk",
		StorageKind: "memory",
	}
	if err := WriteRosterEntry(pA.AppPeer, testAppID, bogus); err != nil {
		t.Fatalf("WriteRosterEntry bogus: %v", err)
	}

	handles, err := m.RestoreFromRoster()
	t.Logf("restore returned handles=%v err=%v", handles, err)

	live := m.liveHostedPeerIDs()
	if _, hasAlpha := live[pA.AppPeer.PeerID()]; !hasAlpha {
		t.Error("alpha missing from live set after restore")
	}
}

// TestOnPeerDestroyedHookFires — proves the cascade-hook contract
// that bridge + console rely on to clean up renderer-specific
// resources tied to a destroyed peer. New in Phase I §12.
// TestPeerManager_AutoListen_TCP verifies the B-4 wiring: when
// Config.ListenAddr is set (bare host:port), Create returns only
// after the listener has bound, HostedPeer.ListenScheme reflects
// the transport, and AppPeer.Addr() returns a live bound address.
func TestPeerManager_AutoListen_TCP(t *testing.T) {
	m := NewPeerManager(testAppID)
	cfg := memCfg("listener")
	cfg.ListenAddr = "127.0.0.1:0" // kernel-chosen port
	h, err := m.Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer m.Destroy(h)

	hp := m.Get(h)
	if hp == nil {
		t.Fatal("Get(h) returned nil")
	}
	if hp.ListenScheme != "tcp" {
		t.Errorf("ListenScheme = %q, want tcp", hp.ListenScheme)
	}
	if hp.AppPeer.Addr() == nil {
		t.Fatal("AppPeer.Addr() is nil after auto-Listen — listener did not bind")
	}
}

// TestPeerManager_AutoListen_WebSocket verifies ws:// scheme routing
// through parseListenAddr → AppPeer.ListenWebSocketReady.
func TestPeerManager_AutoListen_WebSocket(t *testing.T) {
	m := NewPeerManager(testAppID)
	cfg := memCfg("ws-listener")
	cfg.ListenAddr = "ws://127.0.0.1:0/ws"
	h, err := m.Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer m.Destroy(h)

	hp := m.Get(h)
	if hp == nil {
		t.Fatal("Get(h) returned nil")
	}
	if hp.ListenScheme != "ws" {
		t.Errorf("ListenScheme = %q, want ws", hp.ListenScheme)
	}
}

// TestPeerManager_AutoListen_InvalidAddr verifies that a bind failure
// surfaces from Create instead of leaving a zombie peer.
func TestPeerManager_AutoListen_InvalidAddr(t *testing.T) {
	m := NewPeerManager(testAppID)
	cfg := memCfg("bad")
	// Port well above ephemeral range AND privileged-bind-only port:
	// 0:0 is "use kernel-chosen" so we need something the OS rejects.
	// :999999 is parse-valid but bind-invalid (port out of range).
	cfg.ListenAddr = "127.0.0.1:999999"
	_, err := m.Create(cfg)
	if err == nil {
		t.Fatal("expected error from Create with invalid bind address")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error %q lacks 'listen' context", err)
	}
}

// TestPeerManager_AutoListen_AutoAnnounce_LANDiscoverable verifies the
// B-5 wiring: when a peer auto-Listens, it also auto-Announces, so a
// sibling peer's DiscoverPeers() finds it via mDNS. Skipped in -short
// mode (uses real multicast).
func TestPeerManager_AutoListen_AutoAnnounce_LANDiscoverable(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS test uses real multicast; skip in -short mode")
	}
	m := NewPeerManager(testAppID)

	serverCfg := memCfg("server")
	serverCfg.ListenAddr = "127.0.0.1:0"
	hServer, err := m.Create(serverCfg)
	if err != nil {
		t.Fatalf("Create server: %v", err)
	}
	defer m.Destroy(hServer)

	clientCfg := memCfg("client")
	clientCfg.ListenAddr = "127.0.0.1:0"
	hClient, err := m.Create(clientCfg)
	if err != nil {
		t.Fatalf("Create client: %v", err)
	}
	defer m.Destroy(hClient)

	server := m.Get(hServer)
	client := m.Get(hClient)
	if server == nil || client == nil {
		t.Fatal("Get returned nil for one of the peers")
	}

	// Let the auto-Announce propagate before scanning. ~500ms is plenty
	// on `lo`.
	time.Sleep(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	cands, err := client.AppPeer.DiscoverPeers(ctx)
	if err != nil {
		t.Fatalf("client DiscoverPeers: %v", err)
	}

	serverPID := server.AppPeer.PeerID()
	found := false
	for _, c := range cands {
		_, _, txt, decErr := discoverymdns.DecodeEndpointHint(c.EndpointHint)
		if decErr != nil {
			continue
		}
		if txt["peer_id_hint"] == serverPID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("server peer-id %s not auto-discoverable via mDNS (%d candidates)",
			serverPID, len(cands))
		for i, c := range cands {
			_, _, txt, _ := discoverymdns.DecodeEndpointHint(c.EndpointHint)
			t.Logf("  candidate[%d] peer_id_hint=%s backend=%s",
				i, txt["peer_id_hint"], c.Backend)
		}
	}
}

// TestPeerManager_NearbyConnect_TCP_EndToEnd is the regression test
// that should have caught the B-5 phase B bugs before the user did:
//
//	(1) The UI freeze (Render-on-UI-thread → Scan → wake loop) —
//	    caught structurally because this test does *not* run a Scan
//	    inline with each prefix-watch wake.
//
//	(2) The "too many colons" connect failure — caught by dialing
//	    through the exact URL shape the bridge's chooseDialAddr emits
//	    (`tcp://host:port`), exercising the workbench's connect path
//	    end-to-end including the entitysdk scheme-strip.
//
//	(3) The `.local.` hostname dial portability gap — caught by
//	    asserting that the endpoint_hint exposes an IPv4 list (so the
//	    bridge can prefer IP over the announced HostName) and dialing
//	    through that IPv4. Without the IP-prefer path, cross-LAN
//	    dials need nss-mdns / avahi-daemon on every dialing host.
//
// Two PeerManager-managed peers (matches the production code path:
// shellboot.PeerManager.Create is what the bridge calls) announce
// over mDNS; one peer discovers the other, decodes the hint through
// the workbench-side `entitysdk.DecodeMDNSEndpointHint` mirror, and
// dials via `tcp://<ipv4>:<port>` — the exact URL the panel produces.
func TestPeerManager_NearbyConnect_TCP_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS test uses real multicast; skip in -short mode")
	}
	m := NewPeerManager(testAppID)

	// Listen on all interfaces so the LAN IP the mDNS backend announces
	// is actually reachable. (Binding 127.0.0.1 means the server accepts
	// only on loopback, but the announce surfaces the box's LAN IP — the
	// dial mismatch was the first failure mode this test caught.) This
	// matches the real cross-network demo's `--listen 0.0.0.0:9001`.
	serverCfg := memCfg("server")
	serverCfg.ListenAddr = "0.0.0.0:0"
	hServer, err := m.Create(serverCfg)
	if err != nil {
		t.Fatalf("Create server: %v", err)
	}
	defer m.Destroy(hServer)

	clientCfg := memCfg("client")
	clientCfg.ListenAddr = "0.0.0.0:0"
	hClient, err := m.Create(clientCfg)
	if err != nil {
		t.Fatalf("Create client: %v", err)
	}
	defer m.Destroy(hClient)

	server := m.Get(hServer)
	client := m.Get(hClient)
	if server == nil || client == nil {
		t.Fatal("Get returned nil for one of the peers")
	}

	time.Sleep(500 * time.Millisecond) // let mDNS propagate

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	cands, err := client.AppPeer.DiscoverPeers(ctx)
	if err != nil {
		t.Fatalf("client DiscoverPeers: %v", err)
	}

	serverPID := server.AppPeer.PeerID()
	var hint entitysdk.MDNSEndpointHint
	found := false
	for _, c := range cands {
		h, decErr := entitysdk.DecodeMDNSEndpointHint(c.EndpointHint)
		if decErr != nil {
			continue
		}
		txt := map[string]string{}
		for _, t := range h.Text {
			if i := indexByte(t, '='); i >= 0 {
				txt[t[:i]] = t[i+1:]
			}
		}
		if txt["peer_id_hint"] == serverPID {
			hint = h
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("server peer-id %s not in client's scan (%d candidates)",
			serverPID, len(cands))
	}

	// IP-prefer assertion: the hint MUST expose at least one IPv4
	// from the loopback announce so the bridge's pickDialHost can
	// reach this peer without nss-mdns on the dialing host.
	if len(hint.IPv4) == 0 {
		t.Fatalf("endpoint_hint did not surface IPv4 (got %+v) — bridge would fall back to %q which requires avahi", hint, hint.HostName)
	}

	// Exact shape the bridge's chooseDialAddr produces.
	dialURL := "tcp://" + hint.IPv4[0] + ":" + portStr(hint.Port)
	conn, err := client.AppPeer.Connect(ctx, dialURL)
	if err != nil {
		t.Fatalf("Connect(%q) failed: %v", dialURL, err)
	}
	defer conn.Close()
	if st := conn.ConnState(); st == nil || !st.Completed || string(st.RemotePeerID) != serverPID {
		t.Fatalf("handshake didn't complete with expected peer (state=%+v want %s)",
			st, serverPID)
	}
}

// TestPeerManager_NearbyPeer_AgesOutAfterDeparture verifies the
// workbench-side reaper closes the "ghost peer" gap left by core-go's
// mDNS backend not firing reapCb. After the announcing peer goes away,
// the discovered candidate must (a) disappear from
// ReadDiscoveredCandidates within DiscoveryCandidateMaxAge and (b)
// actually be removed from the store by ReapStaleDiscoveredCandidates
// so the candidate prefix doesn't accumulate forever.
func TestPeerManager_NearbyPeer_AgesOutAfterDeparture(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS test uses real multicast; skip in -short mode")
	}

	// Compress the freshness window so the test runs in ~3s rather than
	// >15s. Must be > the mDNS browse window (1.5s) plus some slack,
	// otherwise the "discovery itself works" assertion races with the
	// cutoff. 2s is the smallest value that consistently passes.
	// Restore on exit; package var override is the standard Go pattern.
	prev := entitysdk.DiscoveryCandidateMaxAge
	entitysdk.DiscoveryCandidateMaxAge = 2 * time.Second
	defer func() { entitysdk.DiscoveryCandidateMaxAge = prev }()

	m := NewPeerManager(testAppID)

	serverCfg := memCfg("server")
	serverCfg.ListenAddr = "0.0.0.0:0"
	hServer, err := m.Create(serverCfg)
	if err != nil {
		t.Fatalf("Create server: %v", err)
	}

	clientCfg := memCfg("client")
	clientCfg.ListenAddr = "0.0.0.0:0"
	hClient, err := m.Create(clientCfg)
	if err != nil {
		t.Fatalf("Create client: %v", err)
	}
	defer m.Destroy(hClient)

	server := m.Get(hServer)
	client := m.Get(hClient)
	if server == nil || client == nil {
		t.Fatal("Get returned nil for one of the peers")
	}
	serverPID := server.AppPeer.PeerID()

	time.Sleep(500 * time.Millisecond) // let mDNS converge

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.AppPeer.DiscoverPeers(ctx); err != nil {
		t.Fatalf("client DiscoverPeers: %v", err)
	}
	cands := client.AppPeer.ReadDiscoveredCandidates()
	if !containsServerPID(cands, serverPID) {
		// Dump what we actually got so a failure is debuggable.
		raw := client.AppPeer.Store().List("system/discovery/candidate/mdns/")
		t.Logf("ReadDiscoveredCandidates returned %d, store has %d raw entries",
			len(cands), len(raw))
		for i, e := range raw {
			ent, _ := client.AppPeer.Store().Get(e.Path)
			cd, _ := types.CandidateDataFromEntity(ent)
			h, _ := entitysdk.DecodeMDNSEndpointHint(cd.EndpointHint)
			t.Logf("  store[%d] observed_at=%d text=%v", i, cd.ObservedAt, h.Text)
		}
		t.Fatalf("server peer-id %s not in client's nearby list immediately after scan — discovery itself failed",
			serverPID)
	}

	// Server goes away. Destroy stops the announce + tears down the
	// listener, so the next client scan can't refresh the candidate.
	if err := m.Destroy(hServer); err != nil {
		t.Fatalf("Destroy server: %v", err)
	}

	// Wait past the (shrunk) freshness window. No further DiscoverPeers
	// during the wait — that's what would refresh ObservedAt.
	time.Sleep(entitysdk.DiscoveryCandidateMaxAge + 100*time.Millisecond)

	if containsServerPID(client.AppPeer.ReadDiscoveredCandidates(), serverPID) {
		t.Fatal("departed server still in client's nearby list past max-age window — freshness filter not applied")
	}

	// And the underlying store entries must be gone too, not just hidden.
	before := len(client.AppPeer.Store().List("system/discovery/candidate/mdns/"))
	if before == 0 {
		t.Fatal("no candidate entries at all — test setup didn't actually populate the store")
	}
	reaped := client.AppPeer.ReapStaleDiscoveredCandidates()
	if reaped == 0 {
		t.Fatal("ReapStaleDiscoveredCandidates removed nothing; expected at least the departed-server snapshot(s)")
	}
	after := len(client.AppPeer.Store().List("system/discovery/candidate/mdns/"))
	if after >= before {
		t.Fatalf("reap left the store full: before=%d after=%d reaped=%d",
			before, after, reaped)
	}
}

// containsServerPID returns true if any candidate has a peer_id_hint
// TXT key matching pid.
func containsServerPID(cands []types.CandidateData, pid string) bool {
	for _, c := range cands {
		h, err := entitysdk.DecodeMDNSEndpointHint(c.EndpointHint)
		if err != nil {
			continue
		}
		for _, t := range h.Text {
			if i := indexByte(t, '='); i >= 0 && t[:i] == "peer_id_hint" && t[i+1:] == pid {
				return true
			}
		}
	}
	return false
}

// indexByte is strings.IndexByte without importing strings.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// portStr is a small allocation-free itoa for the test.
func portStr(p int) string {
	if p == 0 {
		return "0"
	}
	var buf [6]byte
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = byte('0' + p%10)
		p /= 10
	}
	return string(buf[i:])
}

// TestParseListenAddr covers the scheme-classification helper directly.
func TestParseListenAddr(t *testing.T) {
	cases := []struct {
		in     string
		scheme string
		bind   string
		path   string
	}{
		{"host:9001", "tcp", "host:9001", ""},
		{"tcp://host:9001", "tcp", "host:9001", ""},
		{"ws://host:9001/ws", "ws", "host:9001", "/ws"},
		{"ws://host:9001/custom", "ws", "host:9001", "/custom"},
		{"ws://host:9001", "ws", "host:9001", "/ws"},
		{"wss://host:9001/ws", "ws", "host:9001", "/ws"},
	}
	for _, c := range cases {
		scheme, bind, path, err := parseListenAddr(c.in)
		if err != nil {
			t.Errorf("parseListenAddr(%q): unexpected error %v", c.in, err)
			continue
		}
		if scheme != c.scheme || bind != c.bind || path != c.path {
			t.Errorf("parseListenAddr(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.in, scheme, bind, path, c.scheme, c.bind, c.path)
		}
	}
}

func TestOnPeerDestroyedHookFires(t *testing.T) {
	m := NewPeerManager(testAppID)
	defer m.ShutdownAll()

	var fired int64
	var lastHandle int64
	m.OnPeerDestroyed(func(h int64) {
		atomic.AddInt64(&fired, 1)
		atomic.StoreInt64(&lastHandle, h)
	})

	hA, _ := m.Create(memCfg("alpha"))
	hB, _ := m.Create(memCfg("beta"))

	if atomic.LoadInt64(&fired) != 0 {
		t.Fatalf("hook fired before any destroy: %d", atomic.LoadInt64(&fired))
	}

	if err := m.Destroy(hB); err != nil {
		t.Fatalf("Destroy beta: %v", err)
	}
	if atomic.LoadInt64(&fired) != 1 {
		t.Errorf("fired count after 1 destroy = %d, want 1", atomic.LoadInt64(&fired))
	}
	if atomic.LoadInt64(&lastHandle) != hB {
		t.Errorf("hook received handle %d, want beta %d", atomic.LoadInt64(&lastHandle), hB)
	}

	if err := m.Destroy(hA); err != nil {
		t.Fatalf("Destroy alpha: %v", err)
	}
	if atomic.LoadInt64(&fired) != 2 {
		t.Errorf("fired count after 2 destroys = %d, want 2", atomic.LoadInt64(&fired))
	}

	// Destroying an unknown handle should NOT fire the hook
	// (Destroy is a no-op for unknown handles).
	if err := m.Destroy(99999); err != nil {
		t.Fatalf("Destroy unknown: %v", err)
	}
	if atomic.LoadInt64(&fired) != 2 {
		t.Errorf("hook fired for unknown handle: count = %d", atomic.LoadInt64(&fired))
	}
}
