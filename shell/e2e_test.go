package shell_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/crypto"
)

// versionRowRE matches one row of `revision log` output.
//
// Rows look like:
//
//	"* 00abc1234567..."   (head — leading * and one space)
//	"  00def4567890..."   (older versions — two-space indent)
//
// Anchor to end-of-line so we don't match version hashes that
// appear inside other lines (e.g. "committed 00abc... @ root ..."
// or the head line in `revision status`). Tolerate optional
// preceding REPL prompt junk (the first line of a LinesResult
// can have "entity:self:/ > " glued before it when the prompt
// hasn't flushed a newline).
var versionRowRE = regexp.MustCompile(`(?m)(?:^|>\s*)(?:\* |  )00[0-9a-f]{10}\.\.\.\s*$`)

// End-to-end CLI tests for entity-shell.
//
// These tests build the actual `entity-shell` binary, exec it with a
// REPL script piped via stdin, capture the combined output, and
// assert against substring patterns. They exercise the full path:
// flag parsing, REPL line scanning, command dispatch, formatting —
// the whole thing as a user would experience it.
//
// Compared to the in-process shellcmd/* tests (which call the
// command registry directly), these add ~0.5–1s per test for the
// build + exec overhead, but catch regressions the in-process tests
// miss: REPL escape handling, stdout/stderr split, exit codes, and
// any drift in main.go's wiring.
//
// TestMain builds the binary once and reuses it across tests.

var shellBinary string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "entity-shell-e2e-*")
	if err != nil {
		panic("e2e: tempdir: " + err.Error())
	}
	defer os.RemoveAll(tmp)

	bin := filepath.Join(tmp, "entity-shell")
	// Inject a known version string so TestE2E_VersionFlag can pin
	// the exact output. Real builds use the Makefile's git-derived
	// version; tests use this synthetic marker.
	build := exec.Command("go", "build",
		"-ldflags", "-X main.version=test-e2e-stamp",
		"-o", bin, "./cmd/entity-shell")
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("e2e: build entity-shell: " + err.Error())
	}
	shellBinary = bin

	os.Exit(m.Run())
}

// runShellScript executes the entity-shell binary in REPL mode with
// the given script piped via stdin. Returns combined stdout+stderr.
// Each test is given a fresh $HOME (tempdir) so identity/config
// state from one test doesn't leak into another.
func runShellScript(t *testing.T, script string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	cmd := exec.Command(shellBinary)
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		t.Fatalf("shell exited with error: %v\noutput:\n%s", err, out.String())
	}
	return out.String()
}

// mustContain asserts that haystack contains every needle.
func mustContain(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("output missing %q\n--- output ---\n%s", n, haystack)
		}
	}
}

// mustNotContain asserts that haystack does not contain any needle.
func mustNotContain(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			t.Errorf("output unexpectedly contains %q\n--- output ---\n%s", n, haystack)
		}
	}
}

// countOccurrences returns how many times needle appears in haystack.
func countOccurrences(haystack, needle string) int {
	return strings.Count(haystack, needle)
}

// ingestFixtureDir returns the absolute path to the self-contained markdown
// tree the `ingest tree` scenarios drive (shell/testdata/ingest-tree/),
// derived from this test file's own location rather than a hardcoded host
// path. A dedicated fixture — NOT the repo's live docs/ — is deliberate: docs/
// changes over time AND the publish pipeline filters it to a canonical subset
// (the canonical-docs filter), so a test coupled to docs/ passes natively but
// fails on the published mirror. The fixture ships unfiltered (under testdata/,
// which Go tooling ignores and the publish keeps), so these tests pass natively,
// inside the make+podman bare-box container (repo bind-mounted at /src/...), on
// any CI checkout, AND against the published mirror.
func ingestFixtureDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("ingestFixtureDir: runtime.Caller failed")
	}
	// thisFile = <repo>/shell/e2e_test.go → <repo>/shell/testdata/ingest-tree
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "testdata", "ingest-tree"))
	if err != nil {
		t.Fatalf("ingestFixtureDir: abs: %v", err)
	}
	return abs
}

// --- Scenarios ---

// TestE2E_BasicTreeOps: a sanity floor — write, read, list. If this
// breaks, every other scenario is moot.
func TestE2E_BasicTreeOps(t *testing.T) {
	out := runShellScript(t, `
cd self:
put scratch/foo test/v 42
cat scratch/foo
exit
`)
	mustContain(t, out,
		"put /",
		"scratch/foo",
		"Type:  test/v",
	)
}

// TestE2E_RevisionManualMode: the canonical authoring loop —
// commit when ready, see the commits, status reflects head. The
// log should contain *exactly* the commits we made (no
// auto-version noise) — that's the key behavioral claim of manual
// mode that USAGE-REVISION-HISTORY.md §3.1 makes.
func TestE2E_RevisionManualMode(t *testing.T) {
	out := runShellScript(t, `
cd self:
put docs/a test/v 1
put docs/b test/v 2
revision commit docs/ first manual
put docs/a test/v 100
revision commit docs/ second
revision log docs/
revision status docs/
exit
`)
	mustContain(t, out,
		"committed ",
		"head:",
		"conflicts: 0",
		"pending:   0",
	)

	// 2 commits → revision log emits 2 version rows. Each version
	// row contains a short-form hash like "00d8933a28be...".
	if got := len(versionRowRE.FindAllString(out, -1)); got != 2 {
		t.Errorf("expected exactly 2 versions in revision log, found %d\n--- output ---\n%s",
			got, out)
	}
}

// TestE2E_RevisionManualMode_CheckoutNoExtraVersions: the key
// behavioral claim that distinguishes manual mode from auto-version
// mode. Under manual mode (no config), checkout swaps the working
// tree state but does NOT create new version entries. After 3
// commits + checkout to v1, log should still show exactly 1 version
// (the v1 we checked out to — its ancestors before it).
//
// Hash prefixes here are content-deterministic (peer-id is stable
// across runs because the test rebuilds from the same script under
// a fresh tempdir HOME; ed25519 keypair derives from a deterministic
// path in the shell startup). Prefix `ba89` is v1's short form
// (post-EXTENSION-TREE v4.0 substrate).
func TestE2E_RevisionManualMode_CheckoutNoExtraVersions(t *testing.T) {
	out := runShellScript(t, `
cd self:
put docs/a test/v 1
revision commit docs/ v1
put docs/a test/v 2
revision commit docs/ v2
put docs/a test/v 3
revision commit docs/ v3
revision log docs/
revision branch create docs/ trunk HEAD
revision checkout docs/ ba89
cat docs/a
revision log docs/
exit
`)
	// Phase 1: 3 commits → log shows 3 versions
	// Phase 2: checkout to v1 → log shows just v1 (head moved to v1)
	// Total log lines printed across both `revision log` calls = 3 + 1 = 4

	// Successful checkout printed.
	mustContain(t, out, "checked out 00ba89dc6078...")

	// After checkout, cat shows the v1 value (1, not 3).
	// Find the line right after "checked out" containing "Data:".
	if !strings.Contains(out, "Data:\n  1\n") {
		t.Errorf("after checkout to v1, cat docs/a should show 1\n--- output ---\n%s", out)
	}

	// No extra-version noise: count "committed " messages — should
	// be exactly 3 (one per explicit revision commit, no synthetic
	// commits from the checkout).
	if got := countOccurrences(out, "committed "); got != 3 {
		t.Errorf("expected 3 'committed' lines (one per manual commit), got %d\n--- output ---\n%s",
			got, out)
	}
}

// TestE2E_RevisionBranchPreservation: the canonical "branch as
// bookmark" pattern from USAGE-REVISION-HISTORY §4.1. Without an
// explicit branch, history walked from a moved head appears
// truncated; with a branch, the original tip stays reachable.
func TestE2E_RevisionBranchPreservation(t *testing.T) {
	out := runShellScript(t, `
cd self:
put docs/a test/v 1
revision commit docs/ v1
put docs/a test/v 2
revision commit docs/ v2
put docs/a test/v 3
revision commit docs/ v3
revision branch create docs/ trunk HEAD
revision checkout docs/ ba89
revision branch list docs/
revision checkout docs/ trunk
cat docs/a
exit
`)
	// Prefix `ba89` = v1 short form, `bb17` = v3 short form (trunk's
	// tip), under EXTENSION-TREE v4.0 substrate hashing.
	mustContain(t, out,
		`created branch "trunk"`,
		"trunk", // appears in branch list
		"checked out 00bb174f5627",
		// after returning to trunk, we should see v3's value
		"Data:\n  3\n",
	)
}

// TestE2E_RevisionRefResolution: every command that takes a hash
// should accept HEAD / branch / tag / short-prefix refs, not just
// full hashes. Validates the resolveRevisionRef path end-to-end.
func TestE2E_RevisionRefResolution(t *testing.T) {
	out := runShellScript(t, `
cd self:
put docs/a test/v 1
revision commit docs/ v1
put docs/a test/v 2
revision commit docs/ v2
revision tag create docs/ stable HEAD
revision show docs/ stable
revision show docs/ HEAD
revision show docs/ ba89
exit
`)
	// Prefix `ba89` = v1 short form (v2 prefix is `5118`); under
	// EXTENSION-TREE v4.0 substrate hashing.
	mustContain(t, out,
		`tagged `,
		"ref:      stable (tag)",
		"ref:      HEAD (HEAD)",
		"ref:      ba89 (short)",
	)
}

// TestE2E_HistoryConfigAndQuery: history is opt-in via config; once
// installed, mutations get recorded; rollback works against a
// captured hash. End-to-end through the binary.
func TestE2E_HistoryConfigAndQuery(t *testing.T) {
	out := runShellScript(t, `
cd self:
history config notes/*
put notes/foo test/v 1
put notes/foo test/v 2
history query notes/foo
exit
`)
	mustContain(t, out,
		`recording enabled for "notes/*"`,
		"created", // first write event
		"updated", // second write event
	)
}

// TestE2E_HistoryRollback: drives a rollback via the ecf-sha256:
// hash form copied from cat -diag — verifies the parser accepts
// the form users actually copy.
func TestE2E_HistoryRollback(t *testing.T) {
	out := runShellScript(t, `
cd self:
history config notes/*
put notes/foo test/v 1
put notes/foo test/v 2
history rollback notes/foo ecf-sha256:0037bba37032a4f0986ccceb429f2de3bf1555913abd5658c5b2115812bcb358
cat notes/foo
exit
`)
	mustContain(t, out,
		"rolled back",
		"Data:\n  1\n", // value rolled back to original
	)
}

// TestE2E_IngestTree exercises bulk ingest of a markdown directory
// tree. Drives `ingest tree` against the self-contained fixture at
// shell/testdata/ingest-tree/ (a small, real-shaped folder structure),
// then verifies the tree mirrors the source layout: top-level files
// land at the prefix root, subdirectories appear as dirs in `ls`,
// and a known nested file round-trips through `cat` with full
// content intact.
//
// The same path also pins the workflow of "ingest → commit →
// log" — proving that ingest + revision compose end-to-end
// without anything special at the boundary.
func TestE2E_IngestTree(t *testing.T) {
	out := runShellScript(t, fmt.Sprintf(`
cd self:
ingest tree %s archives/wb/
ls archives/wb/
ls archives/wb/architecture/
revision commit archives/ "wb docs snapshot"
revision log archives/
exit
`, ingestFixtureDir(t)))
	mustContain(t, out,
		"prefix:   archives/wb/",
		"skipped:  0",
		"ENTITY-SYSTEM.md", // top-level file appears in ls archives/wb/
		"architecture/",    // subdir appears as a dir
		"ARCHITECTURE.md",  // file appears in ls archives/wb/architecture/
		"reviews/",         // nested subdir
		"committed ",       // revision commit succeeded
	)

	// `created: NNN` should be > 0 — defensive lower bound. We assert
	// the count is non-zero rather than an exact number so adding or
	// removing a fixture file doesn't break the test.
	createdRE := regexp.MustCompile(`created:\s+(\d+)`)
	m := createdRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("ingest output missing created count\n--- output ---\n%s", out)
	}
	if m[1] == "0" {
		t.Errorf("ingest created 0 files — source dir empty?\n--- output ---\n%s", out)
	}
}

// TestE2E_CatDirectoryRendersListing: directories aren't real
// entities — they're an emergent property of the path index. But
// `cat` historically just dispatched system/tree:get which returns
// a synthesized `system/tree/listing` entity for trailing-slash
// paths. From the user's perspective that's "I cat'd a directory
// and got a confusing entity dump." The shell now detects the
// listing-entity case and renders it as a clean ls-style listing.
//
// `cat <empty-dir>/` → friendly empty message.
// `cat <real-dir>/`  → listing rows for children.
// `cat <real-dir>/ -diag` → still the underlying entity form (escape hatch).
func TestE2E_CatDirectoryRendersListing(t *testing.T) {
	out := runShellScript(t, fmt.Sprintf(`
cd self:
ingest tree %s archives/wb/
cat archives/wb/architecture/
cat archives/wb/missing/
exit
`, ingestFixtureDir(t)))
	mustContain(t, out,
		// real dir → ls-style rows (no Type/Hash/Data envelope)
		"ARCHITECTURE.md",
		"reviews/",
		// empty dir → friendly message
		"(empty directory:",
	)
	// Should NOT show the raw entity envelope for the populated dir.
	// The synthetic listing's envelope contains "system/tree/listing"
	// in the type field — its absence proves we took the listing
	// branch, not the raw-entity branch.
	mustNotContain(t, out,
		"Type:  system/tree/listing",
	)
}

// TestE2E_FindAndGrep validates the shell-side search verbs against
// the ingest fixture tree. find = substring path search;
// grep = regex content search across `doc/markdown-file` entities.
// Both layer on top of `system/query` — PathPrefix server-side,
// filter client-side. Idiomatic Go in the shell beats waiting for
// a content-search query extension.
func TestE2E_FindAndGrep(t *testing.T) {
	out := runShellScript(t, fmt.Sprintf(`
cd self:
ingest tree %s archives/wb/
find archives/wb/ revision
grep archives/wb/ "manual mode" -i -l
exit
`, ingestFixtureDir(t)))
	mustContain(t, out,
		"USAGE-REVISION-HISTORY.md", // find hits the doc with "revision" in path
		"matched ",                  // grep prints the match count header
	)
	if strings.Contains(out, "matched 0/") {
		t.Errorf("grep produced zero matches; expected at least one\n--- output ---\n%s", out)
	}
}

// tree write produces a version entry — verify the log grows
// per-write, not per-commit. The opposite behavior from manual mode.
func TestE2E_AutoVersionMode(t *testing.T) {
	out := runShellScript(t, `
cd self:
revision config put auto1 docs/ -auto
put docs/a test/v 1
put docs/a test/v 2
put docs/a test/v 3
revision log docs/
exit
`)
	mustContain(t, out, `wrote config "auto1"`)

	// Three puts + auto-version → 3 versions in log. None preceded
	// by `revision commit`.
	if got := len(versionRowRE.FindAllString(out, -1)); got < 3 {
		t.Errorf("auto-version: expected ≥3 versions in log, got %d\n--- output ---\n%s",
			got, out)
	}

	// And no "committed" lines because we never called revision commit.
	if got := countOccurrences(out, "committed "); got != 0 {
		t.Errorf("auto-version should not produce 'committed' lines (they're for manual commits), got %d\n--- output ---\n%s",
			got, out)
	}
}

// runShellWithEnv executes entity-shell with the given HOME directory
// and CLI arguments, piping the script via stdin. Unlike runShellScript,
// HOME is caller-controlled — so multiple invocations within one test
// can share an on-disk identity + sqlite DB to exercise persistence
// across process restarts.
func runShellWithEnv(t *testing.T, home string, args []string, script string) string {
	t.Helper()

	cmd := exec.Command(shellBinary, args...)
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+home)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		t.Fatalf("shell exited with error: %v\nargs: %v\noutput:\n%s", err, args, out.String())
	}
	return out.String()
}

// TestE2E_StoragePersistsAcrossRestart is the real-world persistence
// validation. Two consecutive entity-shell invocations share an
// on-disk identity bundle and a SQLite DB file. The first invocation
// writes entities and commits a revision; the second invocation
// — a fresh process — reads the same DB and confirms the workspace
// state is intact: cat returns the same content, revision log walks
// both commits, revision status reports the right head, and the
// branch we created in pass 1 still resolves.
//
// This is the test that backs the "deployment-ready" claim in
// REPOSITORY-WORKSPACE-ROADMAP.md Phase D — without it, persistence
// is just a code path nobody has exercised through the binary.
func TestE2E_StoragePersistsAcrossRestart(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "peer.db")

	// Seed an on-disk identity so both binary invocations adopt the
	// same peer-id. The tree is a universal namespace; every peer has
	// its own subtree under its peer-id. A fresh keypair on pass 2
	// would be a different peer looking at someone else's data —
	// readable but irrelevant to its own view (e.g. `revision log
	// workspace/` queries this peer's revision-head, not the prior
	// peer's). Identity continuity makes pass 2 the same peer.
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatalf("crypto.Generate: %v", err)
	}
	if err := crypto.SaveIdentityToDir(filepath.Join(home, ".entity", "identities"), "peerA", kp); err != nil {
		t.Fatalf("SaveIdentityToDir: %v", err)
	}

	args := []string{
		"-identity", "peerA",
		"-storage", "sqlite",
		"-storage-path", dbPath,
	}

	// --- Pass 1: write + commit + branch ---
	out1 := runShellWithEnv(t, home, args, `
cd self:
put workspace/note test/v 42
put workspace/sub/a test/v hello
revision commit workspace/ initial
put workspace/note test/v 100
revision commit workspace/ second
revision branch create workspace/ trunk HEAD
exit
`)
	mustContain(t, out1,
		"workspace/note",
		"committed ",
		`created branch "trunk"`,
	)
	if got := countOccurrences(out1, "committed "); got != 2 {
		t.Fatalf("pass 1: expected 2 commits, got %d\n--- output ---\n%s", got, out1)
	}

	// Assert the DB file actually got created and is non-trivial.
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("DB file missing after pass 1: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("DB file is empty after pass 1 — sqlite store didn't persist anything")
	}

	// --- Pass 2: fresh process, same HOME + DB, verify state ---
	out2 := runShellWithEnv(t, home, args, `
cd self:
revision log workspace/
revision status workspace/
revision branch list workspace/
cat workspace/note
cat workspace/sub/a
exit
`)

	// Status should report a non-empty head, no conflicts, no pending.
	mustContain(t, out2,
		"head:",
		"conflicts: 0",
		"pending:   0",
		"trunk", // branch list
		"Type:  test/v",
	)

	// Log should walk both commits (newest first). Pinned by the
	// shared regex used elsewhere in this file.
	if got := len(versionRowRE.FindAllString(out2, -1)); got != 2 {
		t.Errorf("pass 2: revision log should show exactly 2 versions, found %d\n--- output ---\n%s",
			got, out2)
	}

	// `cat workspace/note` after pass 1's second commit was 100; the
	// second `cat` should reflect the earlier put.
	mustContain(t, out2, "Data:\n  100\n")
	mustContain(t, out2, "Data:\n  \"hello\"\n")
}

// TestE2E_StorageAccumulatesAcrossRepeatedRestarts is the long-term
// rigour pass: same identity, same DB, N consecutive entity-shell
// invocations. Each one writes one new entity + commits one revision
// and then verifies the revision log has grown by exactly one. After
// N cycles the log should walk all N commits and every per-cycle
// entity should still be readable. This is what catches drift that
// only shows up after repeated use — duplicate writes on reopen,
// stale handles surviving Close, head pointers regressing, etc.
//
// Cycle count is modest (5) because each cycle pays the binary's
// build-once + exec-once overhead. The SDK-level analogue
// (TestStorage_Sqlite_RepeatedRestartsAccumulate) runs 8 cycles in
// process and catches the same drift in ~2s; this is the binary-
// level confirmation.
func TestE2E_StorageAccumulatesAcrossRepeatedRestarts(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "peer.db")

	kp, err := crypto.Generate()
	if err != nil {
		t.Fatalf("crypto.Generate: %v", err)
	}
	if err := crypto.SaveIdentityToDir(filepath.Join(home, ".entity", "identities"), "peerA", kp); err != nil {
		t.Fatalf("SaveIdentityToDir: %v", err)
	}

	args := []string{
		"-identity", "peerA",
		"-storage", "sqlite",
		"-storage-path", dbPath,
	}

	const cycles = 5

	for i := 0; i < cycles; i++ {
		path := fmt.Sprintf("workspace/note-%02d", i)
		script := fmt.Sprintf(`
cd self:
put %s test/v %d
revision commit workspace/ cycle-%d
revision log workspace/
exit
`, path, 100+i, i)

		out := runShellWithEnv(t, home, args, script)

		// This cycle's commit fired.
		if got := countOccurrences(out, "committed "); got != 1 {
			t.Fatalf("cycle %d: expected exactly 1 commit, got %d\n--- output ---\n%s",
				i, got, out)
		}
		// Log should show i+1 versions cumulatively.
		want := i + 1
		if got := len(versionRowRE.FindAllString(out, -1)); got != want {
			t.Fatalf("cycle %d: log should show %d versions, found %d\n--- output ---\n%s",
				i, want, got, out)
		}
	}

	// Final pass — one more shell, no writes, just verify every
	// per-cycle path is still readable and the full log walks all
	// cycles' commits.
	verifyScript := "cd self:\nrevision log workspace/\n"
	for i := 0; i < cycles; i++ {
		verifyScript += fmt.Sprintf("cat workspace/note-%02d\n", i)
	}
	verifyScript += "exit\n"

	out := runShellWithEnv(t, home, args, verifyScript)

	if got := len(versionRowRE.FindAllString(out, -1)); got != cycles {
		t.Errorf("final pass: log should show %d versions, found %d\n--- output ---\n%s",
			cycles, got, out)
	}
	for i := 0; i < cycles; i++ {
		want := fmt.Sprintf("Data:\n  %d\n", 100+i)
		if !strings.Contains(out, want) {
			t.Errorf("final pass: missing payload for cycle %d (expected %q)", i, want)
		}
	}
}

// TestE2E_VersionFlag verifies the -version flag prints the
// build-time-stamped version and exits 0. The test binary is built
// with `-X main.version=test-e2e-stamp` in TestMain, so the expected
// output is fixed regardless of git state.
func TestE2E_VersionFlag(t *testing.T) {
	cmd := exec.Command(shellBinary, "-version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("-version exited with error: %v\noutput:\n%s", err, out.String())
	}
	got := strings.TrimSpace(out.String())
	if got != "test-e2e-stamp" {
		t.Errorf("-version output = %q, want %q", got, "test-e2e-stamp")
	}
}

// TestE2E_StorageNewIdentityShowsEmptyView is the explicit negative:
// open the same DB with a fresh keypair, the new peer sees its own
// (empty) namespace, not the prior peer's data. This documents the
// "tree is a universal namespace; identity defines which subtree is
// yours" invariant — there's no implicit migration or shared view.
func TestE2E_StorageNewIdentityShowsEmptyView(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "peer.db")

	idDir := filepath.Join(home, ".entity", "identities")
	kpA, err := crypto.Generate()
	if err != nil {
		t.Fatalf("crypto.Generate A: %v", err)
	}
	if err := crypto.SaveIdentityToDir(idDir, "peerA", kpA); err != nil {
		t.Fatalf("SaveIdentityToDir A: %v", err)
	}
	kpB, err := crypto.Generate()
	if err != nil {
		t.Fatalf("crypto.Generate B: %v", err)
	}
	if err := crypto.SaveIdentityToDir(idDir, "peerB", kpB); err != nil {
		t.Fatalf("SaveIdentityToDir B: %v", err)
	}

	// peerA writes + commits.
	out1 := runShellWithEnv(t, home,
		[]string{"-identity", "peerA", "-storage", "sqlite", "-storage-path", dbPath},
		`
cd self:
put workspace/note test/v 42
revision commit workspace/ from-peerA
revision log workspace/
exit
`)
	if got := len(versionRowRE.FindAllString(out1, -1)); got != 1 {
		t.Fatalf("peerA: expected 1 version in log, got %d\n--- output ---\n%s", got, out1)
	}

	// peerB opens the same DB. Its own view of workspace/ should be
	// empty — no commits, no head — even though peerA's data is in
	// the same SQLite file.
	out2 := runShellWithEnv(t, home,
		[]string{"-identity", "peerB", "-storage", "sqlite", "-storage-path", dbPath},
		`
cd self:
revision log workspace/
exit
`)
	if got := len(versionRowRE.FindAllString(out2, -1)); got != 0 {
		t.Errorf("peerB should see 0 versions for its own workspace/ view, got %d\n--- output ---\n%s",
			got, out2)
	}
}
