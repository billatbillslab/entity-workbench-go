package shellcmd

import (
	"strings"
	"testing"

	"entity-workbench-go/workbench"
)

// The `site verify` renderer's contract is what it REFUSES to print.
//
// This verb's whole reason to exist is that five of the seven links in a
// published-origin verification chain are satisfiable by an origin that
// is lying — the trie walk is the one that is not. So the assertions
// here are about the failure surface: a run that did not verify must not
// produce a sentence an operator can read as "verified", and the link
// that broke must be visible rather than merely absent.
//
// Tier: unit (pure rendering; the chain itself is tested in `workbench`
// and `fetch`).

func TestRenderConsume_FailedRunNeverReadsAsVerified(t *testing.T) {
	out := workbench.ConsumeOutput{
		Origin: "http://origin.example",
		Steps: []workbench.ConsumeStep{
			{Name: "layout", Status: workbench.StepOK, Detail: "…/transport-profile",
				Proves: "where the publisher says its objects live"},
			{Name: "manifest + signature", Status: workbench.StepFailed,
				Err: "fetch manifest: GET …/manifest: 404 Not Found"},
		},
		Err:    "fetch manifest: GET …/manifest: 404 Not Found",
		HasRun: true, Duration: "12ms",
	}
	got := strings.Join(renderConsume(out, false), "\n")

	if strings.Contains(got, "verified as of") {
		t.Error("a failed run produced a freshness claim")
	}
	if !strings.Contains(got, "VERDICT: FAILED") {
		t.Errorf("no failure verdict in:\n%s", got)
	}
	// The step that broke is shown as broken. A chain that renders one
	// ok row and then stops looks green at a glance, which on this verb
	// is the exact misreading it exists to prevent.
	if !strings.Contains(got, "FAIL") || !strings.Contains(got, "manifest + signature") {
		t.Errorf("the failing link is not visible in:\n%s", got)
	}
}

func TestRenderConsume_IncompleteWalkIsAboutThePublisher(t *testing.T) {
	out := workbench.ConsumeOutput{
		Origin:     "http://origin.example",
		Incomplete: true,
		Err:        "tree/incomplete-walk: …",
		Steps: []workbench.ConsumeStep{
			{Name: "trie walk", Status: workbench.StepFailed, Err: "tree/incomplete-walk: …"},
		},
		HasRun: true, Duration: "9ms",
	}
	got := strings.Join(renderConsume(out, false), "\n")

	if !strings.Contains(got, "INCOMPLETE WALK") {
		t.Errorf("the incomplete-walk verdict is not named:\n%s", got)
	}
	// The sentence that keeps an operator from restarting a healthy
	// server: this failure is a statement about what the publisher
	// serves, and every per-leaf fetch against the same origin succeeds.
	if !strings.Contains(got, "PUBLISHER") {
		t.Errorf("the verdict does not say whose defect this is:\n%s", got)
	}
}

func TestRenderConsume_PinnedLayoutIsDisclosed(t *testing.T) {
	discovered := strings.Join(renderConsume(workbench.ConsumeOutput{Discovered: true}, false), "\n")
	if !strings.Contains(discovered, "discovered from the origin's transport-profile") {
		t.Errorf("a discovered layout is not disclosed:\n%s", discovered)
	}

	pinned := strings.Join(renderConsume(workbench.ConsumeOutput{Discovered: false}, false), "\n")
	if !strings.Contains(pinned, "PINNED") {
		t.Errorf("a pinned layout is not disclosed:\n%s", pinned)
	}
	// A wrong pin and a withholding origin are byte-identical at the
	// consumer (arch R-28), so the disclosure has to carry the reason,
	// not just the word.
	if !strings.Contains(pinned, "withholding origin") {
		t.Errorf("the pin disclosure does not say why it matters:\n%s", pinned)
	}
}

func TestRenderConsume_GreenRunCarriesTheFreshnessBound(t *testing.T) {
	out := workbench.ConsumeOutput{
		Origin: "http://origin.example", PeerID: "2Kexample",
		Discovered: true, Verified: true, HasRun: true, Duration: "18ms",
		PublishedAt: 1787279734361, Seq: 3,
		RootHash: "ecf-sha256:abc", Prefix: "docs/", AbsolutePrefix: "/2Kexample/docs/",
		KeysTotal: 2, KeysOK: 2,
		Steps: []workbench.ConsumeStep{{Name: "trie walk", Status: workbench.StepOK,
			Detail: "3 CHAMP nodes", Proves: "the origin serves every node it committed to"}},
	}
	got := strings.Join(renderConsume(out, false), "\n")

	if !strings.Contains(got, "verified as of published_at") {
		t.Errorf("a green run did not bound its claim in time:\n%s", got)
	}
	if !strings.Contains(got, "never simply") {
		t.Errorf("the freshness caveat was dropped:\n%s", got)
	}
	// Both prefix forms, because two of §3.3's three shapes differ and
	// an operator comparing a key against their own tree needs the
	// absolute one.
	if !strings.Contains(got, `"docs/"`) || !strings.Contains(got, "/2Kexample/docs/") {
		t.Errorf("the prefix resolution is not shown:\n%s", got)
	}
	// Every green step's scope is printed, not hidden behind a tick.
	if !strings.Contains(got, "proves:") {
		t.Errorf("a green step printed no scope:\n%s", got)
	}
}
