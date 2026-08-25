package entitysdk_test

// What the startup query-index rebuild costs, as a function of store size.
//
// Context: the three query indexes are in-memory and are fed only by the
// "query" sync hook, so against a persistent store they start empty and every
// peer open pays a full rebuild — `li.List("")` over every tree-bound entity,
// then a content-store Get plus a CBOR decode per entity to extract hash and
// path references. That is O(all entities) of real work on the open path, and
// it is the price of the 2026-08-23 correctness fix rather than a fix for the
// underlying shape (STATUS §0aa / AP39, backlog row PR-1).
//
// This benchmark exists to keep that price a measured number rather than an
// intuition, because the decision it feeds — build a SQLite-backed query index
// against the same *sql.DB, or keep rebuilding — turns entirely on how the
// curve behaves. Run it before arguing either way:
//
//	make test-sdk ARGS="-run XXX -bench BenchmarkQueryIndexRebuild -benchtime 1x -v"
//
// Benchmarks do not run under the normal sweep, so this costs the tree nothing.

import (
	"fmt"
	"testing"

	"entity-workbench-go/entitysdk"
)

// BenchmarkQueryIndexRebuild_OnOpen measures peer-open wall time against a
// pre-populated sqlite store. The reported ns/op is dominated by the rebuild:
// everything else on the open path is independent of store size, so the
// difference between the sizes is the rebuild curve.
func BenchmarkQueryIndexRebuild_OnOpen(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("entities=%d", n), func(b *testing.B) {
			dbPath := b.TempDir() + "/bench.db"
			b.Setenv("HOME", b.TempDir())
			if _, err := entitysdk.CreateIdentity("bench-rebuild"); err != nil {
				b.Fatalf("CreateIdentity: %v", err)
			}
			open := func() *entitysdk.AppPeer {
				ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
					Storage:  entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
					Identity: &entitysdk.IdentityBindingConfig{Name: "bench-rebuild"},
				})
				if err != nil {
					b.Fatalf("CreatePeer: %v", err)
				}
				return ap
			}

			// Seed once, outside the timer.
			seed := open()
			peerID := seed.PeerID()
			for i := 0; i < n; i++ {
				path := fmt.Sprintf("/%s/bench/e%06d", peerID, i)
				if _, err := seed.Put(path, "app/file", map[string]interface{}{
					"name": fmt.Sprintf("e%06d", i),
					"size": uint64(i),
				}); err != nil {
					b.Fatalf("put %d: %v", i, err)
				}
			}
			if err := seed.Close(); err != nil {
				b.Fatalf("close seed: %v", err)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ap := open()
				b.StopTimer()
				_ = ap.Close()
				b.StartTimer()
			}
		})
	}
}
