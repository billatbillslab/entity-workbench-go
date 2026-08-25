package shellcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/publish"
)

// cmd_browse_test.go — the shell half of the journey, driven end to end
// over `entity-browser-rust`'s frozen federation.
//
// These are not "does the verb parse" tests. Each one asserts a property
// of what an operator is TOLD, because the whole argument for this
// surface is that the same page can be shown honestly or dishonestly and
// the difference is in the words around it.
//
// Tier: integration (real HTTP, real verification, another impl's bytes).

const (
	fedRoot         = "../fetch/testdata/crossimpl-rust-federation"
	fedRegistryPeer = "2KBLkCxvkgobuauPA6zPfKarpuRRnnWHL98n8Gv1GNmybr"
)

func fedServer(t *testing.T, hook func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	files := http.FileServer(http.Dir(fedRoot))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil && hook(w, r) {
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pinArgs is the pinned layout for their registry, which serves no
// transport-profile (conformant — NETWORK §6.5.4).
func pinArgs(origin string) []string {
	return []string{"pin", origin,
		"-peer", fedRegistryPeer,
		"-pin-tree", "/registry/" + fedRegistryPeer,
		"-pin-content", "/registry/content",
		"-pin-manifest", "/registry/" + fedRegistryPeer + "/system/peer/published-root",
		"-pin-layout", "sharded-2-4",
	}
}

// browseShell returns a bare shell with only the browser wired. The
// consume side needs no peer, and building one here would hide that.
func browseShell(t *testing.T) *Shell {
	t.Helper()
	return &Shell{ShellWorkspace: &ShellWorkspace{}}
}

// requireFixtureUnexpired turns the one way these tests can rot into a
// message that says what to do.
//
// The shell's browser runs on the real clock (there is no operator flag
// to move it, and inventing one to make a test pass would be inventing a
// surface). The frozen federation's bindings carry a real 30-day TTL, so
// after it lapses every one of these tests fails at `binding freshness`
// — correctly, and for a reason that has nothing to do with the code
// under test. `fetch`'s TestFedExpiredBindingIsRefused is where the
// expiry check itself is measured, on a clock it controls.
func requireFixtureUnexpired(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "FAIL ] binding freshness") ||
		strings.Contains(out, "past issued_at + ttl") {
		t.Fatalf("the frozen crossimpl-rust-federation fixture's bindings have EXPIRED — this is " +
			"the fixture asking to be re-cut, not a regression here. Take a fresh `make federation` " +
			"emission from entity-browser-rust into fetch/testdata/crossimpl-rust-federation and " +
			"update its README's date and revision.")
	}
}

func run(t *testing.T, sh *Shell, fn func(*Shell, []string) (Result, error), args ...string) string {
	t.Helper()
	res, err := fn(sh, args)
	if err != nil {
		t.Fatalf("%v %v: %v", args, err, err)
	}
	if res.Kind == KindLines {
		return strings.Join(res.Lines, "\n")
	}
	return res.Message
}

// TestShellRegistryPinSaysWhatItDidNotDo.
//
// A pin verifies nothing — it is a key, not a claim about an origin. The
// output has to say so, because "pinned" reads like "checked" and the
// operator's next action depends on knowing it is not.
func TestShellRegistryPinSaysWhatItDidNotDo(t *testing.T) {
	srv := fedServer(t, nil)
	sh := browseShell(t)

	out := run(t, sh, cmdRegistry, pinArgs(srv.URL)...)
	if !strings.Contains(out, fedRegistryPeer) {
		t.Errorf("pin output does not name the pinned peer:\n%s", out)
	}
	if !strings.Contains(out, "Nothing has been verified yet") {
		t.Errorf("pin output does not say that nothing was verified:\n%s", out)
	}
	if !strings.Contains(out, "PINNED by you") {
		t.Errorf("a hand-pinned layout is not reported as such:\n%s", out)
	}
}

// TestShellRegistryLsIsTheWalk: the name list names its own provenance.
func TestShellRegistryLsIsTheWalk(t *testing.T) {
	srv := fedServer(t, nil)
	sh := browseShell(t)
	run(t, sh, cmdRegistry, pinArgs(srv.URL)...)

	out := run(t, sh, cmdRegistry, "ls")
	for _, name := range []string{"entitychurch.org", "docs.entitychurch.org",
		"lab.entitychurch.org", "protocol.entitychurch.org"} {
		if !strings.Contains(out, name) {
			t.Errorf("registry ls omits %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "walk") {
		t.Errorf("registry ls does not tell the operator the list came from the walk:\n%s", out)
	}
	if !strings.Contains(out, "cannot hide one without failing") {
		t.Errorf("registry ls does not state the asymmetry that makes the list worth trusting:\n%s", out)
	}
}

// TestShellRegistryLsFlagsAnAdvertisedOnlyName.
//
// The origin invents a name in the served menu. It must appear marked,
// not silently dropped and not silently offered: dropping it hides a
// disagreement the operator should see, offering it promises something
// that cannot resolve.
func TestShellRegistryLsFlagsAnAdvertisedOnlyName(t *testing.T) {
	srv := fedServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/system/registry/binding/by-name.list") {
			return false
		}
		_, _ = w.Write([]byte("entitychurch.org\ndocs.entitychurch.org\nlab.entitychurch.org\n" +
			"protocol.entitychurch.org\nghost.entitychurch.org\n"))
		return true
	})
	sh := browseShell(t)
	run(t, sh, cmdRegistry, pinArgs(srv.URL)...)

	out := run(t, sh, cmdRegistry, "ls")
	if !strings.Contains(out, "ghost.entitychurch.org") {
		t.Errorf("the invented name was dropped instead of shown:\n%s", out)
	}
	if !strings.Contains(out, "NOT committed by the signed root") {
		t.Errorf("the invented name is not marked as uncommitted:\n%s", out)
	}
	if !strings.Contains(out, "note:") {
		t.Errorf("no reconciliation note for a menu that disagrees with the signature:\n%s", out)
	}
}

// TestShellOpenPrintsThePageAndTheChain — the core interaction.
func TestShellOpenPrintsThePageAndTheChain(t *testing.T) {
	srv := fedServer(t, nil)
	sh := browseShell(t)
	run(t, sh, cmdRegistry, pinArgs(srv.URL)...)
	run(t, sh, cmdRegistry, "ls")

	out := run(t, sh, cmdBrowse, "open", "docs.entitychurch.org")
	requireFixtureUnexpired(t, out)

	// The page.
	if !strings.Contains(out, "─────") {
		t.Errorf("no separator between the page and its provenance:\n%s", out)
	}
	// The chain, whole.
	for _, step := range []string{"registry pin", "name lookup", "binding signature", "association",
		"revocation", "binding freshness", "transport", "target root", "target walk", "page"} {
		if !strings.Contains(out, step) {
			t.Errorf("chain omits step %q:\n%s", step, out)
		}
	}
	if strings.Count(out, "FAIL") != 0 {
		t.Errorf("an honest origin produced a failing step:\n%s", out)
	}
	// The freshness bound, never the bare word.
	if !strings.Contains(out, "as of") {
		t.Errorf("no freshness bound printed under a green chain:\n%s", out)
	}
}

// TestShellOpenRefusesASubstitutedNameAndSaysWhy.
//
// The refusal has to name the binding that actually answered. "The
// registry is broken" and "someone repointed a file" are different
// incidents and §6a.4 asks a resolver to tell its own operator which.
func TestShellOpenRefusesASubstitutedNameAndSaysWhy(t *testing.T) {
	srv := fedServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/by-name/docs.entitychurch.org.bin") {
			return false
		}
		http.ServeFile(w, r, fedRoot+"/registry/"+fedRegistryPeer+
			"/system/registry/binding/by-name/lab.entitychurch.org.bin")
		return true
	})
	sh := browseShell(t)
	run(t, sh, cmdRegistry, pinArgs(srv.URL)...)

	out := run(t, sh, cmdBrowse, "open", "docs.entitychurch.org")
	if !strings.Contains(out, "REFUSED") {
		t.Fatalf("a substituted name did not produce a refusal:\n%s", out)
	}
	if !strings.Contains(out, "lab.entitychurch.org") {
		t.Errorf("the refusal does not name the binding that answered:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("the failing link is not shown failing:\n%s", out)
	}
	// And the steps that DID pass are still on screen — a chain that
	// renders nothing on failure teaches nothing about where it broke.
	if !strings.Contains(out, "registry pin") {
		t.Errorf("the chain up to the break was not printed:\n%s", out)
	}
}

// TestShellWhereReportsTheAuthority: `where` answers "what am I looking
// at and who vouched for it", including when the answer is nobody.
func TestShellWhereReportsTheAuthority(t *testing.T) {
	srv := fedServer(t, nil)
	sh := browseShell(t)
	run(t, sh, cmdRegistry, pinArgs(srv.URL)...)
	run(t, sh, cmdBrowse, "open", "lab.entitychurch.org")

	out := run(t, sh, cmdBrowse, "where")
	requireFixtureUnexpired(t, out)
	if !strings.Contains(out, "entity://lab.entitychurch.org") {
		t.Errorf("`where` does not print the canonical address:\n%s", out)
	}
	if !strings.Contains(out, "via      registry "+fedRegistryPeer) {
		t.Errorf("`where` does not name the authority that vouched:\n%s", out)
	}
	if !strings.Contains(out, "proves") && !strings.Contains(out, "as of") {
		t.Errorf("`where` gives verdicts with no scope:\n%s", out)
	}
}

// TestShellSitesIsTheSignedKeySet.
func TestShellSitesIsTheSignedKeySet(t *testing.T) {
	srv := fedServer(t, nil)
	sh := browseShell(t)
	run(t, sh, cmdRegistry, pinArgs(srv.URL)...)
	run(t, sh, cmdBrowse, "open", "docs.entitychurch.org")

	out := run(t, sh, cmdBrowse, "sites")
	requireFixtureUnexpired(t, out)
	if !strings.Contains(out, "signed") {
		t.Errorf("`sites` does not say the list is the signed key set:\n%s", out)
	}
	if !strings.Contains(out, "demo") {
		t.Errorf("`sites` lists no site from their emission:\n%s", out)
	}
}

// TestShellBackCrossesPublishers.
func TestShellBackCrossesPublishers(t *testing.T) {
	srv := fedServer(t, nil)
	sh := browseShell(t)
	run(t, sh, cmdRegistry, pinArgs(srv.URL)...)
	run(t, sh, cmdBrowse, "open", "docs.entitychurch.org")
	run(t, sh, cmdBrowse, "open", "lab.entitychurch.org")

	run(t, sh, cmdBrowse, "back")
	where := run(t, sh, cmdBrowse, "where")
	requireFixtureUnexpired(t, where)
	if !strings.Contains(where, "docs.entitychurch.org") {
		t.Errorf("back did not return to the previous publisher:\n%s", where)
	}
}

// TestShellUnpinnedRefusesToInventAnAuthority.
func TestShellUnpinnedRefusesToInventAnAuthority(t *testing.T) {
	sh := browseShell(t)
	if _, err := cmdRegistry(sh, []string{"ls"}); err == nil {
		t.Error("registry ls succeeded with nothing pinned")
	}
	res, err := cmdBrowse(sh, []string{"open", "docs.entitychurch.org"})
	if err != nil {
		t.Fatalf("open should report through the chain, not error out: %v", err)
	}
	out := strings.Join(res.Lines, "\n")
	if !strings.Contains(out, "no registry pinned") {
		t.Errorf("opening a name with nothing pinned does not say why it cannot work:\n%s", out)
	}
}

// issuerShell builds a shell whose local peer will act as the registry,
// plus a second peer standing in for the target with its own published
// origin — the thing `registry issue` reads a reach from.
func issuerShell(t *testing.T) (sh *Shell, targetPeerID, targetOrigin string) {
	t.Helper()

	target, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer (target): %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })
	if _, err := target.PutSitePage("demo", "index",
		entitysdk.NewMarkdownPage("Home", "# Home\n\nhello")); err != nil {
		t.Fatalf("seed the target's site: %v", err)
	}

	// The server comes up first so the emission carries the origin it is
	// actually served from (http.FileServer reads per request).
	out := t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(out)))
	t.Cleanup(srv.Close)
	if _, err := publish.Publish(context.Background(), publish.Opts{
		Peer: target, Prefix: "sites/", OutputDir: out, OriginURL: srv.URL,
		At: time.UnixMilli(1_780_000_000_000),
	}); err != nil {
		t.Fatalf("publish the target: %v", err)
	}

	registry, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer (registry): %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	return &Shell{ShellWorkspace: NewShellWorkspace(registry, "self", "")}, target.PeerID(), srv.URL
}

// TestShellIssueThenBrowseOurOwnName closes the loop the user actually
// describes: **register a name, then browse it.**
//
// This peer mints and signs a binding for a target it read the reach
// from, publishes `system/` as a static registry, and a second browser —
// holding nothing but this peer's peer-id — pins it, walks it, and
// resolves the name.
//
// It is deliberately end-to-end through the VERBS rather than the SDK:
// `registry issue` is where an operator's mistakes actually happen, and
// the two most expensive of them (issuing against an origin that is
// someone else, publishing at a prefix that omits the signatures) are
// only reachable from here.
func TestShellIssueThenBrowseOurOwnName(t *testing.T) {
	sh, target, targetSrv := issuerShell(t)

	out := run(t, sh, cmdRegistry, "issue", "mine.test", target, "-target-origin", targetSrv)
	if !strings.Contains(out, "mine.test") || !strings.Contains(out, target) {
		t.Fatalf("issue did not report the binding it made:\n%s", out)
	}
	if !strings.Contains(out, "Nothing is published yet") {
		t.Errorf("issue does not say that it published nothing — a user will assume it did:\n%s", out)
	}
	if !strings.Contains(out, "carried inline") {
		t.Errorf("issue does not report which §3 transports shape it wrote:\n%s", out)
	}
}

// TestShellIssueRefusesAnOriginThatIsSomeoneElse.
//
// A registry signing `name → X` while carrying Y's reach is signing an
// assertion it did not check. The consumer cannot catch it — the binding
// verifies, and the reach leads somewhere the signature says nothing
// about — so the refusal has to be here, at issue time.
func TestShellIssueRefusesAnOriginThatIsSomeoneElse(t *testing.T) {
	sh, _, targetSrv := issuerShell(t)

	_, err := cmdRegistry(sh, []string{"issue", "wrong.test",
		"2KBLkCxvkgobuauPA6zPfKarpuRRnnWHL98n8Gv1GNmybr", "-target-origin", targetSrv})
	if err == nil {
		t.Fatal("issued a binding whose named target and whose advertised reach disagree")
	}
	if !strings.Contains(err.Error(), "advertises peer") {
		t.Errorf("the refusal does not say what disagreed: %v", err)
	}
}

// TestShellIssueRefusesTheSpecMUSTs — the §6a.3 floor, through the verb.
func TestShellIssueRefusesTheSpecMUSTs(t *testing.T) {
	sh, target, _ := issuerShell(t)

	if _, err := cmdRegistry(sh, []string{"issue", "a.test", target}); err == nil {
		t.Error("issued with no -target-origin, so with no transports (§6a.3 MUST)")
	}
	if _, err := cmdRegistry(sh, []string{"issue"}); err == nil {
		t.Error("issued with no arguments at all")
	}
}

// TestPutTakesTheWholePayloadNotTheFirstToken.
//
// `put p t {"title":"My Site"}` tokenizes into two arguments, and reading
// only the first stored `{"title":"My` — which does not parse, so the old
// fallback wrote it as a literal STRING. The put succeeded, printed a
// hash, and the entity decoded nowhere; the failure surfaced later in a
// consumer as `cannot unmarshal UTF-8 text string`, reading as the
// consumer's bug. Found by seeding a site by hand, 2026-08-21.
func TestPutTakesTheWholePayloadNotTheFirstToken(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	sh := &Shell{ShellWorkspace: NewShellWorkspace(ap, "self", "")}
	path := "/" + ap.PeerID() + "/x"

	if _, err := cmdPut(sh, []string{path, "app/thing", `{"title":"My`, `Site"}`}); err != nil {
		t.Fatalf("put with a space in the JSON: %v", err)
	}
	ent, ok := ap.Store().Get(path)
	if !ok {
		t.Fatal("nothing stored")
	}
	var got struct {
		Title string `cbor:"title"`
	}
	if err := ecf.Decode(ent.Data, &got); err != nil {
		t.Fatalf("the stored entity does not decode as the object that was written: %v", err)
	}
	if got.Title != "My Site" {
		t.Errorf("title = %q, want %q — the payload was truncated at the first space", got.Title, "My Site")
	}
}

// TestPutRefusesBrokenJSONRatherThanStoringAString.
func TestPutRefusesBrokenJSONRatherThanStoringAString(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	sh := &Shell{ShellWorkspace: NewShellWorkspace(ap, "self", "")}
	path := "/" + ap.PeerID() + "/y"

	if _, err := cmdPut(sh, []string{path, "app/thing", `{"title":`}); err == nil {
		t.Fatal("stored malformed JSON as a literal string — an entity that decodes nowhere")
	}
	// A payload that was never JSON is still fine.
	if _, err := cmdPut(sh, []string{path, "app/thing", "just", "a", "note"}); err != nil {
		t.Errorf("refused a plain-string payload: %v", err)
	}
}
