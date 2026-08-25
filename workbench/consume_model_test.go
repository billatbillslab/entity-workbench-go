package workbench_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/fetch"
	"entity-workbench-go/workbench"
)

// rustFixtureDir is `fetch`'s frozen `entity-browser-rust` emission,
// pointed at rather than copied.
//
// A second copy of another team's bytes is a second thing to re-cut when
// they re-emit, and the two copies would drift silently — the fixture's
// own README makes "never hand-edit a byte" the rule, and duplicating it
// is the same mistake one directory over. `fetch` owns it; this test
// borrows it.
const rustFixtureDir = "../fetch/testdata/crossimpl-rust-site"

func rustOrigin(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir(rustFixtureDir)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestConsumeModel_RendersTheChainNotJustTheVerdict is the model-layer
// contract behind the `site verify` verb and the Publisher Verify panel.
//
// **The step list is the deliverable, not the boolean.** Five of the six
// links in this chain are satisfiable by an origin that is lying — an
// origin serving a correctly-signed root that commits to nothing passes
// layout, manifest, signature, and every per-leaf fetch anyone makes.
// So a model that returned `Verified bool` and nothing else would be a
// model that cannot be rendered honestly, and both renderers would have
// to reconstruct the reasoning independently.
//
// Tier: integration (real HTTP over another implementation's bytes).
func TestConsumeModel_RendersTheChainNotJustTheVerdict(t *testing.T) {
	m := workbench.NewConsumeModel(nil)
	m.SetOrigin(rustOrigin(t))
	m.SetOpts(fetch.ConsumeOpts{Bodies: true, Reconcile: true, AbsentProbe: "sites/demo/pages/nope"})

	out := m.Verify(context.Background())
	if !out.Verified {
		t.Fatalf("the frozen rust site did not verify: %s", out.Err)
	}
	if !out.Discovered {
		t.Error("their origin serves a transport-profile; the layout should be discovered, not pinned")
	}

	want := []string{"layout", "manifest + signature", "trie walk", "enumerate",
		"leaf bodies", "reconcile", "absent control"}
	if len(out.Steps) != len(want) {
		t.Fatalf("chain has %d steps, want %d: %+v", len(out.Steps), len(want), out.Steps)
	}
	for i, name := range want {
		if out.Steps[i].Name != name {
			t.Errorf("step %d is %q, want %q — the ORDER is the contract: a renderer draws "+
				"the chain in the order the guarantees actually accumulate", i, out.Steps[i].Name, name)
		}
		if out.Steps[i].Status != workbench.StepOK {
			t.Errorf("step %q = %s (%s)", name, out.Steps[i].Status, out.Steps[i].Err)
		}
		// Every green step carries its own scope. A renderer that has
		// nothing to print here prints a tick, and a tick is what gets
		// misread.
		if out.Steps[i].Proves == "" {
			t.Errorf("step %q proves nothing in particular, according to itself", name)
		}
	}

	if out.Nodes < 1 || out.KeysTotal == 0 {
		t.Errorf("walk reported %d nodes / %d keys", out.Nodes, out.KeysTotal)
	}
	if out.KeysFailed != 0 {
		t.Errorf("%d keys failed", out.KeysFailed)
	}
	if !out.AbsentCorrect {
		t.Error("absent-key control did not fire")
	}

	// The freshness sentence exists and names the moment. A green
	// result that renders as "verified" full stop is the claim §6.5.3.1
	// says the corridor cannot support.
	note := out.FreshnessNote()
	if !strings.Contains(note, "published_at") || !strings.Contains(note, "never simply") {
		t.Errorf("freshness note does not bound the claim: %q", note)
	}
}

// TestConsumeModel_PinnedLayoutSaysSo pins the disclosure that matters
// on a pinned run.
//
// A pinned layout is a HUMAN's claim about an origin, and a wrong pin is
// byte-identical at the consumer to a withholding origin — every blob
// 404s either way (arch R-28). So `Discovered` is part of the output and
// a renderer is expected to say which mode it ran in.
//
// Tier: contract pin.
func TestConsumeModel_PinnedLayoutSaysSo(t *testing.T) {
	origin := rustOrigin(t)
	const peer = "2KEE55MMWBvXE8ozrm45kwTchzFaQP1dzBnU9rifGTdmUa"

	m := workbench.NewConsumeModel(nil)
	m.SetOrigin(origin)
	m.SetPeerID(peer)
	m.Pin(&types.TransportEndpoint{
		TreeURLPrefix:     "/" + peer,
		ContentURLPrefix:  "/content",
		ManifestURLPrefix: "/" + peer + "/system/peer/published-root",
		ContentLayout:     types.ContentLayoutSharded24,
		TreeLeafSuffix:    ".bin",
		TreeListingSuffix: ".list",
	})
	out := m.Verify(context.Background())
	if !out.Verified {
		t.Fatalf("pinned run failed: %s", out.Err)
	}
	if out.Discovered {
		t.Error("a pinned run reported itself as discovered")
	}
	if !strings.Contains(out.Steps[0].Detail, "PINNED") {
		t.Errorf("the layout step does not disclose the pin: %q", out.Steps[0].Detail)
	}
}

// TestConsumeModel_UnreachableOriginFailsAtTheNamedStep — a failure has
// to say WHICH link broke.
//
// "Could not verify" sends an operator to look at the whole chain; "the
// layout step could not read the profile" sends them to one object.
// Both renderers show the step list, so the model owes them a step to
// show.
//
// Tier: unit.
func TestConsumeModel_UnreachableOriginFailsAtTheNamedStep(t *testing.T) {
	m := workbench.NewConsumeModel(nil)
	m.SetOrigin("http://127.0.0.1:1")
	out := m.Verify(context.Background())

	if out.Verified {
		t.Fatal("an unreachable origin reported verified")
	}
	if len(out.Steps) != 1 || out.Steps[0].Name != "layout" {
		t.Fatalf("expected exactly the failing layout step, got %+v", out.Steps)
	}
	if out.Steps[0].Status != workbench.StepFailed || out.Steps[0].Err == "" {
		t.Errorf("the failing step is not marked failed: %+v", out.Steps[0])
	}
	if out.FreshnessNote() != "" {
		t.Error("a failed run must not produce a freshness claim")
	}
}

// TestConsumeModel_RenderIsSafeBeforeAnyRun guards the state a renderer
// sees at mount: a panel that opens and draws before the operator has
// typed an origin must get an empty, honest view rather than a zero
// value that reads as a completed verification.
//
// Tier: unit.
func TestConsumeModel_RenderIsSafeBeforeAnyRun(t *testing.T) {
	m := workbench.NewConsumeModel(nil)
	out := m.Render()
	if out.Verified || out.HasRun || len(out.Steps) != 0 {
		t.Fatalf("a model that has never run rendered as %+v", out)
	}
	if out.FreshnessNote() != "" {
		t.Error("a model that has never run produced a freshness claim")
	}
}
