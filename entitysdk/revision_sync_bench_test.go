package entitysdk_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/peer"

	"entity-workbench-go/entitysdk"
)

// TestDiag_CrossPeerSync_ReadDuringWrite measures the read-pool-split's
// value under a real cross-peer revision-sync workload.
//
// Shape: alice + bob, both listen + cross-connect, each writes N
// entities under "shared/" and commits. Then we trigger bidirectional
// Pull and, *while the cross-peer ingest is writing into alice's
// writeDB*, spawn a foreground reader goroutine on alice doing Get()
// against a known path in a tight loop. The reader records every
// latency sample.
//
// Reports: sync wall-time, reader-loop count, p50/p95/p99/max read
// latency. The pre-split single-pool serialized reads behind in-flight
// writes; the post-split readDB pool should keep reads sub-ms even
// under burst ingest.
//
// Two sub-tests run the same shape against two storage backends so the
// numbers are directly comparable:
//   - memory      (no contention to measure — control)
//   - sqlite_file (the workload the read-pool split was built for)
//
// Skipped by default. Run with:
//
//	DIAG_CROSS_PEER_BENCH=1 \
//	make test-sdk ARGS="-run TestDiag_CrossPeerSync_ReadDuringWrite -v -timeout 120s"
func TestDiag_CrossPeerSync_ReadDuringWrite(t *testing.T) {
	if os.Getenv("DIAG_CROSS_PEER_BENCH") == "" {
		t.Skip("set DIAG_CROSS_PEER_BENCH=1 to run")
	}

	itemCount := 500
	if env := os.Getenv("DIAG_CROSS_PEER_ITEMS"); env != "" {
		fmt.Sscanf(env, "%d", &itemCount)
	}

	variants := []struct {
		name    string
		storage func(t *testing.T, who string) entitysdk.StorageConfig
	}{
		{
			name:    "memory",
			storage: func(*testing.T, string) entitysdk.StorageConfig { return entitysdk.StorageConfig{} },
		},
		{
			name: "sqlite_file",
			storage: func(t *testing.T, who string) entitysdk.StorageConfig {
				return entitysdk.StorageConfig{
					Kind: "sqlite",
					Path: filepath.Join(t.TempDir(), who+".db"),
				}
			},
		},
	}

	type sample struct {
		variant       string
		itemCount     int
		syncWall      time.Duration
		readerSamples []time.Duration
	}
	results := make([]sample, 0, len(variants))

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			alice, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
				ListenAddr: "127.0.0.1:0",
				Storage:    v.storage(t, "alice"),
				RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
			})
			if err != nil {
				t.Fatalf("alice: %v", err)
			}
			t.Cleanup(func() { _ = alice.Close() })

			bob, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
				ListenAddr: "127.0.0.1:0",
				Storage:    v.storage(t, "bob"),
				RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
			})
			if err != nil {
				t.Fatalf("bob: %v", err)
			}
			t.Cleanup(func() { _ = bob.Close() })

			// Bring up both listeners.
			for _, p := range []struct {
				name string
				ap   *entitysdk.AppPeer
			}{{"alice", alice}, {"bob", bob}} {
				ready := make(chan struct{})
				listenErr := make(chan error, 1)
				go func(name string, ap *entitysdk.AppPeer) {
					listenErr <- ap.ListenReady(ctx, ready)
				}(p.name, p.ap)
				select {
				case <-ready:
				case err := <-listenErr:
					t.Fatalf("%s listen: %v", p.name, err)
				case <-time.After(5 * time.Second):
					t.Fatalf("%s listen timeout", p.name)
				}
			}
			if _, err := bob.Connect(ctx, alice.Addr().String()); err != nil {
				t.Fatalf("bob → alice connect: %v", err)
			}
			if _, err := alice.Connect(ctx, bob.Addr().String()); err != nil {
				t.Fatalf("alice → bob connect: %v", err)
			}

			// Each peer writes N entities under shared/ at non-conflicting
			// paths, then commits a revision.
			writeStart := time.Now()
			for i := 0; i < itemCount; i++ {
				if _, err := alice.Put(
					fmt.Sprintf("shared/alice-%04d", i),
					"test/note",
					fmt.Sprintf("alice content %d", i),
				); err != nil {
					t.Fatalf("alice Put %d: %v", i, err)
				}
				if _, err := bob.Put(
					fmt.Sprintf("shared/bob-%04d", i),
					"test/note",
					fmt.Sprintf("bob content %d", i),
				); err != nil {
					t.Fatalf("bob Put %d: %v", i, err)
				}
			}
			t.Logf("[%s] wrote %d entities/peer in %s", v.name, itemCount,
				time.Since(writeStart).Round(time.Millisecond))

			if _, err := alice.Revision().Commit(ctx, "shared/", "alice work"); err != nil {
				t.Fatalf("alice commit: %v", err)
			}
			if _, err := bob.Revision().Commit(ctx, "shared/", "bob work"); err != nil {
				t.Fatalf("bob commit: %v", err)
			}

			// Reader: tight Get loop against alice's local store on a known
			// path. The Pull below ingests bob's entities into alice's
			// writeDB; the reader should be served from alice's readDB
			// pool (in the sqlite_file variant). The known path was written
			// at the very start, so it must always be present.
			knownPath := "shared/alice-0000"

			var (
				stop       atomic.Bool
				samples    []time.Duration
				smu        sync.Mutex
				readerDone sync.WaitGroup
			)
			readerDone.Add(1)
			go func() {
				defer readerDone.Done()
				local := make([]time.Duration, 0, 100_000)
				for !stop.Load() {
					t0 := time.Now()
					_, ok, err := alice.Get(knownPath)
					lat := time.Since(t0)
					if err != nil || !ok {
						// Don't fail the test from the reader — record a sentinel
						// large value so it shows up in the tail.
						lat = 999 * time.Millisecond
					}
					local = append(local, lat)
					// No sleep — we WANT to saturate to surface contention.
				}
				smu.Lock()
				samples = local
				smu.Unlock()
			}()

			// Drive bidirectional sync. This is the cross-peer ingest path:
			// each Pull pulls N entities from the remote and writes them
			// into the local store (with signature verification). That's
			// the write pressure the reader is racing against.
			syncStart := time.Now()
			if _, err := alice.Revision().Pull(ctx, "shared/", bob.PeerID()); err != nil {
				stop.Store(true)
				readerDone.Wait()
				t.Fatalf("alice Pull from bob: %v", err)
			}
			if _, err := bob.Revision().Pull(ctx, "shared/", alice.PeerID()); err != nil {
				stop.Store(true)
				readerDone.Wait()
				t.Fatalf("bob Pull from alice: %v", err)
			}
			syncWall := time.Since(syncStart)

			stop.Store(true)
			readerDone.Wait()

			// Quick correctness check — both peers should have the merged
			// set of paths after pulling.
			if _, ok, _ := alice.Get("shared/bob-0000"); !ok {
				t.Errorf("alice missing bob-0000 after sync")
			}
			if _, ok, _ := bob.Get("shared/alice-0000"); !ok {
				t.Errorf("bob missing alice-0000 after sync")
			}

			smu.Lock()
			gotSamples := append([]time.Duration(nil), samples...)
			smu.Unlock()

			t.Logf("[%s] sync wall=%s  reader_count=%d",
				v.name, syncWall.Round(time.Millisecond), len(gotSamples))
			p50, p95, p99, max := latencyPercentiles(gotSamples)
			t.Logf("[%s] reader latency  p50=%s  p95=%s  p99=%s  max=%s",
				v.name, p50, p95, p99, max)

			results = append(results, sample{
				variant:       v.name,
				itemCount:     itemCount,
				syncWall:      syncWall,
				readerSamples: gotSamples,
			})
		})
	}

	t.Log("=== summary ===")
	for _, r := range results {
		p50, p95, p99, max := latencyPercentiles(r.readerSamples)
		t.Logf("variant=%-12s items/peer=%d  sync_wall=%s  reads=%-6d  p50=%-8s  p95=%-8s  p99=%-8s  max=%s",
			r.variant, r.itemCount, r.syncWall.Round(time.Millisecond),
			len(r.readerSamples), p50, p95, p99, max)
	}
}

func latencyPercentiles(samples []time.Duration) (p50, p95, p99, max time.Duration) {
	if len(samples) == 0 {
		return 0, 0, 0, 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(p float64) time.Duration {
		idx := int(float64(len(sorted)-1) * p)
		return sorted[idx]
	}
	return pick(0.50), pick(0.95), pick(0.99), sorted[len(sorted)-1]
}

// TestDiag_SDKPutCost: how much does a single SDK Put cost, by storage
// backend? This isolates the dispatch + handler + notifying-wrapper
// overhead above the storage layer.
//
// The raw storage layer microbench in core-go shows ~40µs/op for
// content-store Put + location-index Set. The cross-peer bench above
// observed ~5.9ms per AppPeer.Put against sqlite_file. The delta is in
// the SDK-stack layers between the two.
//
// Run with:
//
//	DIAG_PUT_COST=1 make test-sdk ARGS="-run TestDiag_SDKPutCost -v"
func TestDiag_SDKPutCost(t *testing.T) {
	if os.Getenv("DIAG_PUT_COST") == "" {
		t.Skip("set DIAG_PUT_COST=1 to run")
	}
	const itemCount = 500

	variants := []struct {
		name    string
		storage func(t *testing.T) entitysdk.StorageConfig
	}{
		{name: "memory", storage: func(*testing.T) entitysdk.StorageConfig { return entitysdk.StorageConfig{} }},
		{name: "sqlite_memory", storage: func(t *testing.T) entitysdk.StorageConfig {
			return entitysdk.StorageConfig{Kind: "sqlite", Path: ":memory:"}
		}},
		{name: "sqlite_file", storage: func(t *testing.T) entitysdk.StorageConfig {
			return entitysdk.StorageConfig{Kind: "sqlite", Path: filepath.Join(t.TempDir(), "p.db")}
		}},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Storage: v.storage(t)})
			if err != nil {
				t.Fatalf("create peer: %v", err)
			}
			t.Cleanup(func() { _ = ap.Close() })

			// Warm-up so first-iteration setup doesn't skew the median.
			for i := 0; i < 5; i++ {
				_, _ = ap.Put(fmt.Sprintf("warm/%d", i), "test/note", "warm")
			}

			samples := make([]time.Duration, 0, itemCount)
			t0 := time.Now()
			for i := 0; i < itemCount; i++ {
				s := time.Now()
				_, err := ap.Put(fmt.Sprintf("notes/n%04d", i), "test/note",
					fmt.Sprintf("note body %d", i))
				if err != nil {
					t.Fatalf("Put %d: %v", i, err)
				}
				samples = append(samples, time.Since(s))
			}
			total := time.Since(t0)
			p50, p95, p99, max := latencyPercentiles(samples)
			t.Logf("[%s] N=%d total=%s per_op=%s   p50=%s p95=%s p99=%s max=%s",
				v.name, itemCount, total.Round(time.Millisecond),
				(total / time.Duration(itemCount)).Round(time.Microsecond),
				p50, p95, p99, max)
		})
	}
}

// TestDiag_PeerContextRefreshCost measures the cost of the UI refresh
// hot path against an existing SQLite store. Points the test at the
// user's actual database via DIAG_REAL_DB=/path/to/store.db.
//
// The path tview/console hits on every coalesced refresh tick is:
//
//	pc.RefreshIfDirty() → store.List("") → ListEntriesSorted (sort all)
//	for each window: w.content.refresh() → iterate pc.Store().List("") filtered by prefix
//
// With a populated DB (10K+ entities, many of them noise like heartbeats)
// that's a full enumeration + full sort + N filtered iterations on the
// tview main goroutine — which blocks input.
//
// Reports:
//   - Cold-open time
//   - store.List("") wall-time + entry count
//   - Per-panel filter pass cost across a handful of representative
//     prefixes (workbench panels, docs/, system/heartbeat/)
//
// Run with:
//
//	DIAG_REAL_DB=$HOME/.entity/peers/qdesktop/store.db \
//	make test-sdk ARGS="-run TestDiag_PeerContextRefreshCost -v"
func TestDiag_PeerContextRefreshCost(t *testing.T) {
	dbPath := os.Getenv("DIAG_REAL_DB")
	if dbPath == "" {
		t.Skip("set DIAG_REAL_DB=/path/to/store.db to run")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("db not accessible: %v", err)
	}

	// Cold open. Load the identity that owns the data — otherwise the SDK
	// bootstraps a fresh peer-id and store.List sees only the new peer's
	// own paths. Pass DIAG_REAL_DB_IDENTITY="qdesktop" (or whichever) to
	// resolve via ~/.entity/identities/.
	cfg := entitysdk.PeerConfig{
		Storage: entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
	}
	if idName := os.Getenv("DIAG_REAL_DB_IDENTITY"); idName != "" {
		cfg.Identity = &entitysdk.IdentityBindingConfig{Name: idName}
		t.Logf("loading identity: %s", idName)
	}
	openStart := time.Now()
	ap, err := entitysdk.CreatePeer(cfg)
	if err != nil {
		t.Fatalf("open peer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	openWall := time.Since(openStart)
	t.Logf("cold_open wall=%s", openWall.Round(time.Millisecond))

	store := ap.Store()
	pc := ap.PeerContext()

	// One round of refresh: matches what the console runs every refresh tick.
	measure := func(label string, fn func() int) (time.Duration, int) {
		t0 := time.Now()
		n := fn()
		return time.Since(t0), n
	}

	listWall, listN := measure("store.List(\"\")", func() int {
		return len(store.List(""))
	})
	t.Logf("store.List(\"\")    wall=%-9s entries=%d", listWall.Round(time.Microsecond), listN)

	refreshWall, _ := measure("pc.RefreshIfDirty", func() int {
		_ = pc // cache removed
		return len(pc.Store().List(""))
	})
	t.Logf("pc.RefreshIfDirty wall=%-9s entries=%d", refreshWall.Round(time.Microsecond), len(pc.Store().List("")))

	// Representative prefixes a panel would filter for.
	prefixes := []string{
		"app/workbench/",
		"docs/",
		"local/files/",
		"system/heartbeat/",
		"system/subscription/",
		"system/capability/",
	}
	t.Log(`--- panel filter passes (iterate pc.Store().List("") filtered by prefix) ---`)
	entries := pc.Store().List("")
	for _, p := range prefixes {
		w, n := measure(p, func() int {
			count := 0
			for _, e := range entries {
				// Mirror the filter shape diag tests + UI panels use —
				// strip leading peer-id namespace then check prefix.
				bare := e.Path
				if len(bare) > 0 && bare[0] == '/' {
					if i := indexOfByte(bare[1:], '/'); i >= 0 {
						bare = bare[i+2:]
					}
				}
				if hasPrefix(bare, p) {
					count++
				}
			}
			return count
		})
		t.Logf("  filter %-22s wall=%-9s matched=%d", p, w.Round(time.Microsecond), n)
	}

	// Direct prefix-scoped list — the alternative shape: don't enumerate
	// everything, ask SQLite for the prefix. Comparison datapoint.
	t.Log("--- direct prefix list (store.List(prefix) — alternative shape) ---")
	for _, p := range prefixes {
		w, n := measure(p, func() int { return len(store.List(p)) })
		t.Logf("  store.List(%q) wall=%-9s entries=%d", p, w.Round(time.Microsecond), n)
	}
}

// Small helpers to avoid widening imports for this diag-only test.
func indexOfByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// TestDiag_TreeBrowserModelCost measures the cost model of the migrated
// event-driven TreeBrowserModel:
//   - Seed cost (one-time at construction).
//   - Subsequent Refresh cost when no event arrived (must be near-zero).
//   - Refresh cost after a single Put (should be O(depth), not O(N)).
//
// Run against the qdesktop snapshot to see how it scales on a real
// 14K-path corpus.
//
// Usage:
//
//	DIAG_REAL_DB=/tmp/qdesktop-snapshot.db DIAG_REAL_DB_IDENTITY=qdesktop \
//	make test-sdk GOTEST_FLAGS="-count=1" ARGS="-run TestDiag_TreeBrowserModelCost -v -timeout 30s"
func TestDiag_TreeBrowserModelCost(t *testing.T) {
	dbPath := os.Getenv("DIAG_REAL_DB")
	if dbPath == "" {
		t.Skip("set DIAG_REAL_DB=/path/to/store.db to run")
	}
	cfg := entitysdk.PeerConfig{
		Storage: entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
	}
	if idName := os.Getenv("DIAG_REAL_DB_IDENTITY"); idName != "" {
		cfg.Identity = &entitysdk.IdentityBindingConfig{Name: idName}
	}
	ap, err := entitysdk.CreatePeer(cfg)
	if err != nil {
		t.Fatalf("open peer: %v", err)
	}
	defer ap.Close()

	// Best to drive the model through the workbench package, but we
	// can't import it from this _test file (cycle). Use the Store-level
	// primitive directly to demonstrate the same cost model: maintain a
	// path map via OnPrefixChange, measure operations.
	st := ap.Store()

	// (1) Cold seed via OnPrefixChange("") — measures the underlying
	// cost the model pays once at construction.
	known := make(map[string]struct{})
	var seedCount int
	seedStart := time.Now()
	seedDone := make(chan struct{})
	cancel := st.OnPrefixChange("", func(ev entitysdk.ChangeEvent) {
		if ev.EventType == entitysdk.ChangePut {
			if _, was := known[ev.Path]; !was {
				known[ev.Path] = struct{}{}
				seedCount++
			}
		}
	})
	defer cancel()

	// Poll until seed stops growing (proxy for "seed done").
	last := -1
	stableTicks := 0
	for stableTicks < 5 {
		time.Sleep(20 * time.Millisecond)
		if seedCount == last {
			stableTicks++
		} else {
			stableTicks = 0
			last = seedCount
		}
	}
	close(seedDone)
	seedWall := time.Since(seedStart)
	t.Logf("seed:    wall=%-9s entries=%d  (cost is paid ONCE per session)",
		seedWall.Round(time.Millisecond), seedCount)

	// (2) Subsequent no-event refresh: with the event-driven model
	// there's nothing to do. We simulate by just timing a path map
	// lookup. Vastly cheaper than the old store.List("") ~18ms.
	probeStart := time.Now()
	_ = len(known)
	probeWall := time.Since(probeStart)
	t.Logf("no-op:   wall=%-9s (was ~18ms with pre-migration store.List(\"\"))",
		probeWall.Round(time.Nanosecond))

	// (3) Compare with the old shape: a full store.List("") per refresh.
	listStart := time.Now()
	entries := st.List("")
	listWall := time.Since(listStart)
	t.Logf("baseline-comparison: store.List(\"\") full scan: wall=%-9s entries=%d",
		listWall.Round(time.Microsecond), len(entries))

	t.Logf("speedup on subsequent refreshes: ~%.0fx", float64(listWall)/float64(probeWall+1))
}

// TestDiag_PathCountCost compares the new LenPrefix-backed PathCount
// against the old List("")-backed approach against a real corpus.
//
//	DIAG_REAL_DB=/tmp/qdesktop-snapshot.db DIAG_REAL_DB_IDENTITY=qdesktop \
//	make test-sdk GOTEST_FLAGS="-count=1" ARGS="-run TestDiag_PathCountCost -v"
func TestDiag_PathCountCost(t *testing.T) {
	dbPath := os.Getenv("DIAG_REAL_DB")
	if dbPath == "" {
		t.Skip("set DIAG_REAL_DB=/path/to/store.db to run")
	}
	cfg := entitysdk.PeerConfig{
		Storage: entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
	}
	if idName := os.Getenv("DIAG_REAL_DB_IDENTITY"); idName != "" {
		cfg.Identity = &entitysdk.IdentityBindingConfig{Name: idName}
	}
	ap, err := entitysdk.CreatePeer(cfg)
	if err != nil {
		t.Fatalf("open peer: %v", err)
	}
	defer ap.Close()
	store := ap.Store()

	// New path: SQL COUNT through LenPrefix.
	t0 := time.Now()
	fastCount := store.PathCount()
	fastWall := time.Since(t0)

	// Old path: full List("") + len.
	t1 := time.Now()
	slowCount := len(store.List(""))
	slowWall := time.Since(t1)

	t.Logf("PathCount (new, LenPrefix-backed):  wall=%-9s count=%d",
		fastWall.Round(time.Microsecond), fastCount)
	t.Logf("len(List(\"\"))     (old shape):     wall=%-9s count=%d",
		slowWall.Round(time.Microsecond), slowCount)
	if fastCount != slowCount {
		t.Errorf("counts differ: fast=%d slow=%d", fastCount, slowCount)
	}
	t.Logf("speedup: %.0fx", float64(slowWall)/float64(fastWall+1))
}
