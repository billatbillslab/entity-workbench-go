package shellcmd_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/localfiles"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/workbench"
)

// TestDiag_RealCorpus_BulkIngest mounts the real entity-systems
// directory (or whatever path is in DIAG_REAL_CORPUS env var) with
// sqlite_file storage and a debug log captured to /tmp/diag-mount.log.
// Run manually when investigating ingest losses at scale. Skipped by
// default since it depends on host state.
//
// Usage:
//
//	DIAG_REAL_CORPUS=$HOME/projects/entity-systems \
//	DIAG_INCLUDE="*.md" \
//	DIAG_EXCLUDE="target/*,node_modules/*,.git/*" \
//	make test-shellcmd ARGS="-run TestDiag_RealCorpus_BulkIngest -v -timeout 300s"
//
// DIAG_INCLUDE / DIAG_EXCLUDE are comma-separated globs forwarded to
// the localfiles RootConfigData. Pin DIAG_INCLUDE="*.md" to reproduce
// the earlier 1092-file baseline (whole corpus minus build
// artifacts).
//
// Inspect /tmp/diag-mount.log afterwards for "tree event dropped" lines
// and any error messages from the watcher / ingest handler.
func TestDiag_RealCorpus_BulkIngest(t *testing.T) {
	corpus := os.Getenv("DIAG_REAL_CORPUS")
	if corpus == "" {
		t.Skip("set DIAG_REAL_CORPUS=/path/to/dir to run")
	}
	if _, err := os.Stat(corpus); err != nil {
		t.Skipf("corpus dir not accessible: %v", err)
	}
	splitCSV := func(s string) []string {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		out := parts[:0]
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	include := splitCSV(os.Getenv("DIAG_INCLUDE"))
	exclude := splitCSV(os.Getenv("DIAG_EXCLUDE"))

	logPath := "/tmp/diag-mount.log"
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	defer logFile.Close()
	debugLog := log.New(logFile, "", log.LstdFlags|log.Lmicroseconds)
	t.Logf("debug log: %s", logPath)

	kp, err := crypto.Generate()
	if err != nil {
		t.Fatalf("crypto.Generate: %v", err)
	}

	ingestHandler := workbench.NewNotificationIngestHandler(nil)
	dbPath := filepath.Join(t.TempDir(), "peer.db")
	t.Logf("storage: sqlite at %s", dbPath)

	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		Keypair:  &kp,
		Storage:  entitysdk.StorageConfig{Kind: "sqlite", Path: dbPath},
		DebugLog: debugLog,
		Handlers: []entitysdk.HandlerRegistration{
			{Pattern: workbench.NotificationIngestPattern, Handler: ingestHandler},
			{Pattern: workbench.ChainErrorsPattern, Handler: workbench.NewChainErrorsHandler()},
		},
	})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	const (
		rootName     = "corpus"
		sourcePrefix = "local/files/corpus/"
		targetPrefix = "docs/"
	)
	ingestHandler.RegisterMount(sourcePrefix, targetPrefix)
	if len(include) > 0 || len(exclude) > 0 {
		t.Logf("filters: include=%v exclude=%v", include, exclude)
	}
	wallStart := time.Now()
	if err := installMountProductionOrderFiltered(t, ap, corpus, rootName, sourcePrefix, targetPrefix, include, exclude); err != nil {
		t.Fatalf("installMount: %v", err)
	}

	// Settle wait: scale with corpus size. 1000+ files = ~60s.
	settleWait := 180 * time.Second
	if env := os.Getenv("DIAG_SETTLE_SECONDS"); env != "" {
		if n, err := time.ParseDuration(env + "s"); err == nil {
			settleWait = n
		}
	}

	// Poll until counts stabilize OR deadline.
	deadline := time.Now().Add(settleWait)
	var lastSource, lastTarget int
	var stableFor time.Duration
	const stableNeeded = 3 * time.Second
	pollInterval := 1 * time.Second
	tick := 0
	for time.Now().Before(deadline) {
		s := countPrefix(ap, sourcePrefix)
		tg := countPrefix(ap, targetPrefix)
		if tick%5 == 0 {
			t.Logf("t=%ds  source=%d  target=%d", tick, s, tg)
		}
		if s == lastSource && tg == lastTarget && s > 0 {
			stableFor += pollInterval
			if stableFor >= stableNeeded {
				break
			}
		} else {
			stableFor = 0
			lastSource = s
			lastTarget = tg
		}
		time.Sleep(pollInterval)
		tick++
	}

	srcPaths := listPrefix(ap, sourcePrefix)
	tgtPaths := listPrefix(ap, targetPrefix)
	wallElapsed := time.Since(wallStart)
	t.Logf("FINAL: source=%d  target=%d  total_entities=%d  wall=%s",
		len(srcPaths), len(tgtPaths), ap.EntityCount(), wallElapsed.Round(100*time.Millisecond))

	chainErrPaths := listPrefix(ap, "system/runtime/chain-errors/")
	t.Logf("chain-errors: %d", len(chainErrPaths))
	for i, p := range chainErrPaths {
		if i >= 5 {
			t.Logf("  ... %d more chain-errors", len(chainErrPaths)-5)
			break
		}
		t.Logf("  %s", p)
	}

	// What's missing? Translate source paths → expected target paths.
	expected := make(map[string]bool, len(srcPaths))
	for _, sp := range srcPaths {
		bare := stripPeer(sp)
		if !strings.HasPrefix(bare, sourcePrefix) {
			continue
		}
		rel := strings.TrimPrefix(bare, sourcePrefix)
		expected[targetPrefix+rel] = true
	}
	for _, tp := range tgtPaths {
		delete(expected, stripPeer(tp))
	}
	missing := make([]string, 0, len(expected))
	for k := range expected {
		missing = append(missing, k)
	}
	sort.Strings(missing)
	t.Logf("MISSING at target: %d", len(missing))
	for i, m := range missing {
		if i >= 20 {
			t.Logf("  ... %d more", len(missing)-20)
			break
		}
		t.Logf("  %s", m)
	}
}

// TestDiag_SqliteVsMemory_BulkIngest is a diagnostic test for the
// reported SQLite-vs-memory divergence on `mount ... -include "*.md"`:
// many files appear at local/files/{root}/... but only a handful show
// up at the target prefix as doc/markdown-file entities. With memory
// storage the same workflow ingests every file.
//
// The test runs the same Q2 mount setup across three storage variants:
//   - memory               (the baseline that works)
//   - sqlite (file-backed) (the variant that breaks in the field)
//   - sqlite (:memory:)    (isolates "is it SQL semantics or file I/O?")
//
// For each variant, write N markdown files to a temp dir, wait for the
// chain to settle, then count entities at both the source mirror prefix
// and the target ingest prefix. Logs the full path lists so we can see
// *which* files made it vs were dropped.
//
// This is intentionally NOT a t.Errorf-driven assertion test. It's a
// diagnostic that exposes the gap. Once we can see the shape of what's
// missing, the fix gets a targeted regression test.
func TestDiag_SqliteVsMemory_BulkIngest(t *testing.T) {
	if testing.Short() {
		t.Skip("diagnostic test; skipped under -short")
	}

	const (
		fileCount    = 30
		settleWait   = 5 * time.Second
		rootName     = "diag"
		sourcePrefix = "local/files/diag/"
		targetPrefix = "docs/"
	)

	type variant struct {
		name    string
		storage func(t *testing.T) entitysdk.StorageConfig
	}
	variants := []variant{
		{
			name:    "memory",
			storage: func(*testing.T) entitysdk.StorageConfig { return entitysdk.StorageConfig{} },
		},
		{
			name: "sqlite_memory",
			storage: func(*testing.T) entitysdk.StorageConfig {
				return entitysdk.StorageConfig{Kind: "sqlite", Path: ":memory:"}
			},
		},
		{
			name: "sqlite_file",
			storage: func(t *testing.T) entitysdk.StorageConfig {
				return entitysdk.StorageConfig{
					Kind: "sqlite",
					Path: filepath.Join(t.TempDir(), "peer.db"),
				}
			},
		},
	}

	type result struct {
		variant     string
		sourceCount int
		targetCount int
		sourcePaths []string
		targetPaths []string
		missing     []string // present at source but not at target
	}

	results := make([]result, 0, len(variants))

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			fsDir := t.TempDir()

			// Stable identity per variant so SQLite-file storage doesn't
			// re-bootstrap with a different peer-id on every run.
			kp, err := crypto.Generate()
			if err != nil {
				t.Fatalf("crypto.Generate: %v", err)
			}

			ingestHandler := workbench.NewNotificationIngestHandler(nil)
			// To debug: enable DebugLog. Goes to stderr because the
			// watcher goroutine outlives test completion; logging to
			// t.Log post-test would panic. The SQLITE_BUSY messages
			// from sqlite_file's watcher writes are visible there.
			//   debugLog := log.New(os.Stderr, "["+v.name+"] ", 0)
			ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
				Keypair: &kp,
				Storage: v.storage(t),
				Handlers: []entitysdk.HandlerRegistration{
					{Pattern: workbench.NotificationIngestPattern, Handler: ingestHandler},
					{Pattern: workbench.ChainErrorsPattern, Handler: workbench.NewChainErrorsHandler()},
				},
			})
			if err != nil {
				t.Fatalf("CreatePeer(%s): %v", v.name, err)
			}
			t.Cleanup(func() { _ = ap.Close() })

			// Write N markdown files FIRST, then mount. Mirrors the
			// user's flow: `mount existing-directory/` against a tree
			// that already has content. The watcher's initial-scan path
			// is what gets exercised, not the fsnotify event path. (A
			// second sub-test below covers the event-path scenario.)
			//
			// Names include the index for diff'ing across variants;
			// spread across two subdirs to rule out flat-vs-nested.
			for i := 0; i < fileCount; i++ {
				var rel string
				if i%2 == 0 {
					rel = fmt.Sprintf("a/note-%03d.md", i)
				} else {
					rel = fmt.Sprintf("b/note-%03d.md", i)
				}
				body := fmt.Sprintf("# Note %03d\n\nContent for file %d.\n", i, i)
				p := filepath.Join(fsDir, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatalf("MkdirAll(%s): %v", p, err)
				}
				if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
					t.Fatalf("WriteFile(%s): %v", p, err)
				}
			}

			// Now mount the populated directory. The watcher does an
			// initial scan + binds FileData entities for every file
			// already present; the subscription then drives the ingest
			// chain. Mount order mirrors production (`cmd_local_files.go`):
			// AddRoot → SubscribeRawAt → StartWatching. Subscribing
			// BEFORE StartWatching is essential because the watcher's
			// initial scan emits tree events synchronously.
			ingestHandler.RegisterMount(sourcePrefix, targetPrefix)
			if err := installMountProductionOrder(t, ap, fsDir, rootName, sourcePrefix, targetPrefix); err != nil {
				t.Fatalf("installMount(%s): %v", v.name, err)
			}

			// Wait for the chain to settle. fsnotify events fire in
			// bursts and the chain takes a few ms per file to advance.
			// Poll until counts stabilize, capped by settleWait.
			deadline := time.Now().Add(settleWait)
			var (
				lastSource int
				lastTarget int
				stableFor  time.Duration
			)
			const stableNeeded = 500 * time.Millisecond
			pollInterval := 50 * time.Millisecond
			for time.Now().Before(deadline) {
				s := countPrefix(ap, sourcePrefix)
				tg := countPrefix(ap, targetPrefix)
				if s == lastSource && tg == lastTarget {
					stableFor += pollInterval
					if stableFor >= stableNeeded && s > 0 {
						break
					}
				} else {
					stableFor = 0
					lastSource = s
					lastTarget = tg
				}
				time.Sleep(pollInterval)
			}

			srcPaths := listPrefix(ap, sourcePrefix)
			tgtPaths := listPrefix(ap, targetPrefix)

			// Compute the set of source files that should have produced
			// target entities but didn't. Translate source paths to
			// expected target paths by swapping the prefix.
			expectedTargets := make(map[string]bool, len(srcPaths))
			for _, sp := range srcPaths {
				// sp looks like "/<peer>/local/files/diag/a/note-000.md".
				// Strip the peer namespace + sourcePrefix, prepend target.
				bare := stripPeer(sp)
				if !strings.HasPrefix(bare, sourcePrefix) {
					continue
				}
				rel := strings.TrimPrefix(bare, sourcePrefix)
				expectedTargets[targetPrefix+rel] = true
			}
			for _, tp := range tgtPaths {
				bare := stripPeer(tp)
				delete(expectedTargets, bare)
			}
			missing := make([]string, 0, len(expectedTargets))
			for k := range expectedTargets {
				missing = append(missing, k)
			}
			sort.Strings(missing)

			t.Logf("[%s] wrote=%d  source=%d  target=%d  missing=%d  total_entities=%d",
				v.name, fileCount, len(srcPaths), len(tgtPaths), len(missing), ap.EntityCount())

			// Surface diagnostic prefixes that often carry the story:
			// chain-error deposits and notification-ingest activity.
			diagPrefixes := []string{
				"system/runtime/chain-errors/",
				"system/subscription/",
				"system/inbox/",
				"system/continuation/",
			}
			for _, dp := range diagPrefixes {
				dps := listPrefix(ap, dp)
				if len(dps) > 0 {
					t.Logf("[%s] %s = %d entries", v.name, dp, len(dps))
					for _, p := range dps {
						if len(dps) <= 5 {
							t.Logf("[%s]   %s", v.name, p)
						}
					}
				}
			}

			// Dump the subscription record to confirm Pattern shape +
			// peer-id qualification. The engine matches qualified paths
			// against this pattern; a mismatch is a likely culprit.
			subPaths := listPrefix(ap, "system/subscription/")
			for _, sp := range subPaths {
				ent, ok, err := ap.Get(stripPeer(sp))
				if err != nil || !ok {
					t.Logf("[%s] subscription read failed: %v (ok=%v)", v.name, err, ok)
					continue
				}
				sub, err := types.SubscriptionDataFromEntity(ent)
				if err != nil {
					t.Logf("[%s] subscription decode failed: %v", v.name, err)
					continue
				}
				// First couple of events for cross-reference.
				firstEvent := ""
				if len(srcPaths) > 0 {
					firstEvent = srcPaths[0]
				}
				t.Logf("[%s] SUB pattern=%q events=%v deliverURI=%q op=%q",
					v.name, sub.Pattern, sub.Events, sub.DeliverURI, sub.DeliverOperation)
				t.Logf("[%s] EXAMPLE event path=%q", v.name, firstEvent)
				t.Logf("[%s] sub.SubscriptionID=%q", v.name, sub.SubscriptionID)
			}
			if len(missing) > 0 && len(missing) <= 20 {
				for _, m := range missing {
					t.Logf("[%s] MISSING target: %s", v.name, m)
				}
			} else if len(missing) > 20 {
				t.Logf("[%s] MISSING target (first 20 of %d):", v.name, len(missing))
				for _, m := range missing[:20] {
					t.Logf("[%s]   %s", v.name, m)
				}
			}

			results = append(results, result{
				variant:     v.name,
				sourceCount: len(srcPaths),
				targetCount: len(tgtPaths),
				sourcePaths: srcPaths,
				targetPaths: tgtPaths,
				missing:     missing,
			})
		})
	}

	// Cross-variant summary. The diagnostic question: does memory ingest
	// every file but sqlite drop most?
	t.Log("=== summary ===")
	for _, r := range results {
		t.Logf("variant=%-14s  source=%-3d  target=%-3d  missing=%-3d",
			r.variant, r.sourceCount, r.targetCount, len(r.missing))
	}
}

// installMountProductionOrder mirrors the production `mount` verb's
// setup order: capability mint → AddRoot → SubscribeRawAt → StartWatching.
// The order matters: subscribing BEFORE StartWatching ensures the
// engine's pathIndex is populated when the watcher's synchronous
// initialScan emits tree events. installMountQ2 in cmd_local_files_e2e_test.go
// has the order wrong (StartWatching before SubscribeRawAt) — it happens
// to pass because that test writes a single file AFTER setup, hitting
// the fsnotify event path which arrives after subscription registration.
func installMountProductionOrder(
	t *testing.T,
	ap *entitysdk.AppPeer,
	fsDir, rootName, sourcePrefix, targetPrefix string,
) error {
	return installMountProductionOrderFiltered(t, ap, fsDir, rootName, sourcePrefix, targetPrefix, nil, nil)
}

func installMountProductionOrderFiltered(
	t *testing.T,
	ap *entitysdk.AppPeer,
	fsDir, rootName, sourcePrefix, targetPrefix string,
	include, exclude []string,
) error {
	t.Helper()
	ctx := context.Background()
	localID := ap.PeerID()

	grants := []types.GrantEntry{
		{
			Handlers:   types.CapabilityScope{Include: []string{workbench.NotificationIngestPattern}},
			Operations: types.CapabilityScope{Include: []string{"receive"}},
		},
	}
	if _, err := ap.MintChainCapabilityBound(grants,
		"system/capability/grants/chain/local-files/"+rootName); err != nil {
		return fmt.Errorf("mint chain cap: %w", err)
	}

	lfHandler := ap.LocalFilesHandler()
	if lfHandler == nil {
		return fmt.Errorf("local/files handler not wired")
	}
	cfg := localfiles.RootConfigData{
		Prefix:         sourcePrefix,
		FilesystemRoot: fsDir,
		Include:        include,
		Exclude:        exclude,
	}
	if err := lfHandler.AddRoot(rootName, cfg, ap.RawContentStore(), ap.RawLocationIndex()); err != nil {
		return fmt.Errorf("AddRoot: %w", err)
	}

	// Subscribe BEFORE StartWatching. Production-correct order.
	deliverURI := fmt.Sprintf("entity://%s/%s", localID, workbench.NotificationIngestPattern)
	if _, err := ap.SubscribeRawAt(localID, sourcePrefix+"*", deliverURI,
		"receive", entitysdk.SubscribeOpts{Events: []string{"created", "updated"}}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	if err := lfHandler.StartWatching(ctx, rootName, ap.RawContentStore(),
		ap.RawLocationIndex(), ap.IdentityHash()); err != nil {
		return fmt.Errorf("StartWatching: %w", err)
	}
	return nil
}

func countPrefix(ap *entitysdk.AppPeer, prefix string) int {
	return len(listPrefix(ap, prefix))
}

func listPrefix(ap *entitysdk.AppPeer, prefix string) []string {
	pc := ap.PeerContext()
	_ = pc // cache removed
	out := []string{}
	for _, e := range pc.Store().List("") {
		bare := stripPeer(e.Path)
		if strings.HasPrefix(bare, prefix) {
			out = append(out, e.Path)
		}
	}
	sort.Strings(out)
	return out
}

// stripPeer removes the "/{peerID}/" namespace from a qualified path.
// Returns the remainder; if the path has no leading slash or no
// embedded slash, returns it unchanged.
func stripPeer(p string) string {
	if len(p) == 0 || p[0] != '/' {
		return p
	}
	rest := p[1:]
	idx := strings.IndexByte(rest, '/')
	if idx < 0 {
		return rest
	}
	return rest[idx+1:]
}
