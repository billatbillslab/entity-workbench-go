package workbench_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/workbench"
)

// browse_model_test.go — the journey, at the model layer, over another
// implementation's bytes.
//
// The fixture lives in `fetch/testdata/crossimpl-rust-federation`
// (provenance in its README) and is reached from here by relative path
// rather than copied: one frozen emission, one place to re-cut.
//
// Tier: integration (real HTTP, real verification, another impl's bytes).

const (
	fedRegistryPeer = "2KBLkCxvkgobuauPA6zPfKarpuRRnnWHL98n8Gv1GNmybr"
	fedDocs         = "2KFRBJ9feEPCZZiNaCEKsCAVkGkp1htWZk9a8jz5n5D2sS"
	fedRoot         = "../fetch/testdata/crossimpl-rust-federation"
)

func serveFederation(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir(fedRoot)))
	t.Cleanup(srv.Close)
	return srv
}

// registryPin is the pinned layout for their registry, which serves no
// `transport-profile` (conformant — NETWORK §6.5.4 puts profile
// distribution out-of-band in v1).
func registryPin() *types.TransportEndpoint {
	return &types.TransportEndpoint{
		TreeURLPrefix:     "/registry/" + fedRegistryPeer,
		ContentURLPrefix:  "/registry/content",
		ManifestURLPrefix: "/registry/" + fedRegistryPeer + "/system/peer/published-root",
		ContentLayout:     types.ContentLayoutSharded24,
		TreeLeafSuffix:    ".bin",
		TreeListingSuffix: ".list",
	}
}

// browserAt builds a browser pinned to the fixture's registry, with the
// clock parked at the epoch — before the bindings expire, and by a wide
// margin.
//
// The clock is pinned for the reason `fetch/registry_test.go` gives: the
// fixture's TTLs are real wall-clock durations, so a test running on the
// real clock passes today and fails silently in a month, rotting the
// fixture into a broken build with no diff to blame. The epoch is
// deliberately *not* "inside the validity window" — it is simply before
// the expiry, which is all these tests need, and it keeps the parking
// spot from depending on the emission date. **The expiry check itself is
// asserted where it belongs**, on both sides of the boundary, in
// `fetch`'s TestFedExpiredBindingIsRefused; nothing here would notice if
// it stopped firing, and nothing here claims to.
func browserAt(t *testing.T) (*workbench.BrowseModel, *httptest.Server) {
	t.Helper()
	srv := serveFederation(t)
	m := workbench.NewBrowseModel(nil)
	m.Now = func() time.Time { return time.UnixMilli(0) }
	if err := m.PinRegistry(context.Background(), srv.URL, fedRegistryPeer, registryPin()); err != nil {
		t.Fatalf("PinRegistry: %v", err)
	}
	if err := m.RefreshNames(context.Background()); err != nil {
		t.Fatalf("RefreshNames: %v", err)
	}
	return m, srv
}

// TestBrowseRegistryIsTheWalk: the registry browser's rows come from the
// signed key set, and say so.
func TestBrowseRegistryIsTheWalk(t *testing.T) {
	m, _ := browserAt(t)
	out := m.Render()

	if len(out.Names) != 4 {
		t.Fatalf("registry browser shows %d names, the fixture publishes 4: %+v", len(out.Names), out.Names)
	}
	for _, row := range out.Names {
		if !row.Committed {
			t.Errorf("row %q is not committed by the signed root but is being shown as available", row.Name)
		}
		if row.BindingHash == "" {
			t.Errorf("row %q carries no binding hash", row.Name)
		}
	}
	if !strings.Contains(out.NamesAuthority, "walk") {
		t.Errorf("NamesAuthority = %q — a renderer reads this to tell a user where the list came "+
			"from, and the answer has to name the walk", out.NamesAuthority)
	}
	if out.NamesNote != "" {
		t.Errorf("menu and signed key set disagree on an honest fixture: %s", out.NamesNote)
	}
	if out.RegistryDiscovered {
		t.Error("the registry layout was pinned by hand; reporting it as discovered hides that a " +
			"wrong pin and a withholding origin look identical from here")
	}
	if out.RegistryFresh == "" || out.RegistryFresh == "no published_at" {
		t.Errorf("registry freshness = %q; every green verdict here is 'as of' a moment", out.RegistryFresh)
	}
}

// TestBrowseOpenByNameRendersAPage is the whole arc through the model:
// a name in, a rendered page out, with the chain beside it.
func TestBrowseOpenByNameRendersAPage(t *testing.T) {
	m, _ := browserAt(t)
	ctx := context.Background()

	if err := m.Open(ctx, "docs.entitychurch.org"); err != nil {
		t.Fatalf("open docs.entitychurch.org: %v\nsteps: %s", err, stepDump(m))
	}
	out := m.Render()
	// This model's clock IS injectable and is pinned in browserAt, so
	// expiry cannot rot this test the way it can the shell's — noted
	// because the guard's absence here is deliberate, not an oversight.

	if out.Content.BodyMarkdown == "" {
		t.Errorf("navigated successfully and rendered no body")
	}
	if out.Content.SiteTitle == "" {
		t.Errorf("no site title — the manifest did not resolve through the verified walk")
	}
	if out.Host != "docs.entitychurch.org" {
		t.Errorf("Host = %q, want the name the user typed", out.Host)
	}
	if !strings.HasPrefix(out.Address, "entity://docs.entitychurch.org/") {
		t.Errorf("Address = %q", out.Address)
	}
	if len(out.Sites) == 0 {
		t.Error("no sites listed — the authoritative 'what is published here' is the walk's key set")
	}

	// The chain: every step present, every step green, and every green
	// step carrying what it proves.
	want := []string{"registry pin", "name lookup", "binding signature", "association",
		"revocation", "binding freshness", "transport", "target root", "target walk", "page"}
	if got := stepNames(out.Steps); !equalStrings(got, want) {
		t.Fatalf("chain steps = %v, want %v", got, want)
	}
	for _, s := range out.Steps {
		if s.Status != workbench.StepOK {
			t.Errorf("step %q is %s: %s", s.Name, s.Status, s.Err)
		}
		if s.Proves == "" {
			t.Errorf("step %q has a verdict and no statement of what it proves — that field is "+
				"the whole design, not decoration", s.Name)
		}
	}
	if !strings.Contains(out.Freshness, "as of") {
		t.Errorf("Freshness = %q — a green chain is 'verified as of' a moment, never the bare word",
			out.Freshness)
	}
	if strings.Contains(strings.ToLower(out.Freshness), "verified.") {
		t.Errorf("Freshness overclaims: %q", out.Freshness)
	}
}

// TestBrowseByPeerIDSkipsTheNamingHopVisibly.
//
// Addressing a peer-id directly is legitimate and weaker: nothing
// vouches that this peer is what any human name refers to. The chain has
// to *show* that, because a browser that just started at "target root"
// would render a shorter, cleaner, equally-green chain for a materially
// weaker claim.
func TestBrowseByPeerIDSkipsTheNamingHopVisibly(t *testing.T) {
	m, srv := browserAt(t)
	ctx := context.Background()
	m.SetTargetOrigin(srv.URL + "/docs")

	if err := m.Open(ctx, fedDocs); err != nil {
		t.Fatalf("open by peer-id: %v\nsteps: %s", err, stepDump(m))
	}
	out := m.Render()

	skipped := 0
	for _, s := range out.Steps {
		if s.Status == workbench.StepSkipped {
			skipped++
			if s.Proves == "" {
				t.Errorf("skipped step %q gives no reason", s.Name)
			}
		}
	}
	if skipped != 6 {
		t.Errorf("%d steps skipped, want the whole 6-step naming hop shown as not-run", skipped)
	}
	if out.Content.BodyMarkdown == "" {
		t.Error("the content half should still resolve: the peer-id IS the key")
	}
}

// TestBrowseSubstitutionSurfacesAsAName, not as a broken registry.
//
// §6a.4: *"a resolver SHOULD surface which require failed to its own
// operator"* — because a unit error, a clock skew and an active
// substitution attempt are otherwise one undifferentiated dead end,
// during exactly the incident where telling them apart matters.
func TestBrowseSubstitutionSurfacesAsAName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/by-name/docs.entitychurch.org.bin") {
			http.ServeFile(w, r, fedRoot+"/registry/"+fedRegistryPeer+
				"/system/registry/binding/by-name/lab.entitychurch.org.bin")
			return
		}
		http.FileServer(http.Dir(fedRoot)).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	m := workbench.NewBrowseModel(nil)
	m.Now = func() time.Time { return time.UnixMilli(0) }
	ctx := context.Background()
	if err := m.PinRegistry(ctx, srv.URL, fedRegistryPeer, registryPin()); err != nil {
		t.Fatalf("PinRegistry: %v", err)
	}
	// No enumeration: force the by-name pointer path, which is the one a
	// hostile origin can steer.
	if err := m.Open(ctx, "docs.entitychurch.org"); err == nil {
		t.Fatal("navigation to a substituted name succeeded")
	}
	out := m.Render()

	if !strings.Contains(out.Err, "lab.entitychurch.org") {
		t.Errorf("Err = %q — it must name the binding that actually answered, or an operator "+
			"reads this as 'the registry is broken'", out.Err)
	}
	if out.Content.BodyMarkdown != "" {
		t.Error("a refused navigation left content on screen")
	}

	// The failing link is SHOWN failing, and the steps before it are
	// still there. A chain that renders three ok rows and stops looks
	// green at a glance.
	var failed int
	for _, s := range out.Steps {
		if s.Status == workbench.StepFailed {
			failed++
		}
	}
	if failed == 0 {
		t.Error("no step is marked failed on a refused navigation")
	}
	if len(out.Steps) < 2 {
		t.Errorf("only %d steps recorded; the chain up to the break has to stay visible", len(out.Steps))
	}
}

// TestBrowseHistoryCrossesDomains — back and forward across two
// publishers, which is the thing a per-site history cannot do.
func TestBrowseHistoryCrossesDomains(t *testing.T) {
	m, _ := browserAt(t)
	ctx := context.Background()

	if err := m.Open(ctx, "docs.entitychurch.org"); err != nil {
		t.Fatalf("open docs: %v", err)
	}
	first := m.Render().Address
	if err := m.Open(ctx, "lab.entitychurch.org"); err != nil {
		t.Fatalf("open lab: %v", err)
	}
	second := m.Render().Address
	if first == second {
		t.Fatal("two different names produced the same address")
	}
	if !m.Render().CanBack {
		t.Fatal("CanBack is false after two navigations")
	}

	if err := m.Back(ctx); err != nil {
		t.Fatalf("back: %v", err)
	}
	if got := m.Render().Address; got != first {
		t.Errorf("back landed on %q, want %q", got, first)
	}
	if !m.Render().CanForward {
		t.Error("CanForward is false after going back")
	}
	if err := m.Forward(ctx); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if got := m.Render().Address; got != second {
		t.Errorf("forward landed on %q, want %q", got, second)
	}
}

// TestParseAddress pins the literal-or-peer-id decision.
func TestParseAddress(t *testing.T) {
	for _, tc := range []struct {
		in         string
		name, peer string
		site, page string
	}{
		{"docs.entitychurch.org", "docs.entitychurch.org", "", "", ""},
		{"docs.entitychurch.org/demo", "docs.entitychurch.org", "", "demo", ""},
		{"entity://docs.entitychurch.org/demo/guide/intro", "docs.entitychurch.org", "", "demo", "guide/intro"},
		{fedDocs + "/demo", "", fedDocs, "demo", ""},
	} {
		got, err := workbench.ParseAddress(tc.in)
		if err != nil {
			t.Errorf("ParseAddress(%q): %v", tc.in, err)
			continue
		}
		if got.Name != tc.name || got.PeerID != tc.peer || got.SiteID != tc.site || got.Page != tc.page {
			t.Errorf("ParseAddress(%q) = %+v, want name=%q peer=%q site=%q page=%q",
				tc.in, got, tc.name, tc.peer, tc.site, tc.page)
		}
	}
	if _, err := workbench.ParseAddress(""); err == nil {
		t.Error("the empty address parsed")
	}
}

func stepNames(steps []workbench.ConsumeStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

func stepDump(m *workbench.BrowseModel) string {
	var b strings.Builder
	for _, s := range m.Render().Steps {
		b.WriteString("\n  " + string(s.Status) + " " + s.Name + " — " + s.Detail + " " + s.Err)
	}
	return b.String()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
