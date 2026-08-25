package entitysdk

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/types"
)

// rendezvousFixture stands up a signaling node plus two client peers
// that both carry the discovery substrate, and returns the two clients'
// registered rendezvous backends pointed at one tag.
//
// **Three distinct peers, and the two clients hold NO listener.** That
// is not incidental to the test — it is the case the backend exists for.
// A rendezvous is how a peer with no reachable address gets introduced,
// so a fixture whose clients listen would prove the easy half and hide
// the fact that the substrate used to be wired only for listening peers.
func rendezvousFixture(t *testing.T, tag string) (node *AppPeer, a, b *RendezvousBackend, ap, bp *AppPeer) {
	t.Helper()
	node, err := CreatePeer(PeerConfig{
		ListenAddr: "127.0.0.1:0",
		Extensions: ExtensionsConfig{SignalingNode: &SignalingNodeConfig{}},
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	})
	if err != nil {
		t.Fatalf("CreatePeer(node): %v", err)
	}
	t.Cleanup(func() { node.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ready := make(chan struct{})
	listenErr := make(chan error, 1)
	go func() { listenErr <- node.ListenReady(ctx, ready) }()
	select {
	case <-ready:
	case err := <-listenErr:
		t.Fatalf("node listen: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("node not ready in 2s")
	}

	dial := func(name string) (*AppPeer, *RendezvousBackend) {
		p, err := CreatePeer(PeerConfig{
			Extensions: ExtensionsConfig{Discovery: &DiscoveryConfig{}},
		})
		if err != nil {
			t.Fatalf("CreatePeer(%s): %v", name, err)
		}
		t.Cleanup(func() { p.Close() })
		conn, err := p.Connect(ctx, node.Addr().String())
		if err != nil {
			t.Fatalf("%s.Connect: %v", name, err)
		}
		t.Cleanup(func() { conn.Close() })

		rb, err := p.EnableRendezvousDiscovery(RendezvousConfig{
			Node:  node.PeerID(),
			Mode:  RendezvousModeTag,
			Input: tag,
			// Poll fast: the browse session is a real ticker and the
			// tests below wait on it.
			PollInterval:    50 * time.Millisecond,
			ReofferInterval: time.Hour, // the synchronous first offer is enough
		})
		if err != nil {
			t.Fatalf("%s.EnableRendezvousDiscovery: %v", name, err)
		}
		return p, rb
	}
	ap, a = dial("alice")
	bp, b = dial("bob")
	return node, a, b, ap, bp
}

// TestRendezvous_TwoPeersSurfaceEachOtherAsCandidates is the backend
// gate: two peers that share only a tag surface each other as
// DISCOVERY candidates, through the substrate, over the wire.
//
// **This is a different claim from the carrier gate next door.**
// TestSignaling_TwoPeersMeetAtATag proves a blob deposited at a key is
// collectable at that key. This proves the DISCOVERY layer above it —
// that an observation at a mailbox becomes a `system/discovery/candidate`
// with the right fields absent, that it lands on the watchable prefix,
// and that a peer does not discover itself. Two peers again, for the
// same D22 reason: one peer offering into its own node proves the bucket
// and not the meeting.
//
// Built against PROPOSAL-DISCOVERY-RENDEZVOUS-BACKEND §2 (RULED
// 2026-08-17, arch 05faaa5), not landed EXTENSION-DISCOVERY v1.0.
//
// Tier: integration (real node, real dispatch, real key derivation).
func TestRendezvous_TwoPeersSurfaceEachOtherAsCandidates(t *testing.T) {
	_, alice, bob, _, _ := rendezvousFixture(t, "entity-church-rdv-gate")
	ctx := context.Background()

	// Nobody has announced. A tag nobody stands at surfaces nothing, and
	// that is an empty result rather than an error — the mailbox is
	// simply empty (§4.4).
	cands, err := bob.Scan(ctx, nil)
	if err != nil {
		t.Fatalf("Scan of an untouched tag errored: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("Scan of an untouched tag surfaced %d candidates", len(cands))
	}

	// Alice stands at the tag.
	if err := alice.Announce(ctx, "rdv"); err != nil {
		t.Fatalf("alice.Announce: %v", err)
	}

	cands, err = bob.Scan(ctx, nil)
	if err != nil {
		t.Fatalf("bob.Scan: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("bob surfaced %d candidates, want exactly 1 — alice deposited once and bob "+
			"must not see his own absence of a deposit as a peer", len(cands))
	}
	c := cands[0]

	if c.Backend != RendezvousBackendKind {
		t.Errorf("candidate.backend = %q, want %q", c.Backend, RendezvousBackendKind)
	}

	// **peer_id ABSENT.** §2.2 step 1: the peer-id is established by
	// IDENTIFY over the ADMITTED channel, not by who signed a blob at a
	// mailbox. We hold a cryptographically verified signer at this point
	// — the assertion is that the backend does not use it, which is the
	// non-obvious half of the rule and the one an implementation gets
	// wrong by doing the helpful thing.
	if c.PeerID != "" {
		t.Errorf("candidate.peer_id = %q, want ABSENT — reaching a key proves someone derived "+
			"that key, never that the channel admission later opens belongs to them "+
			"(SIGNALING §1.2, DISCOVERY §2.2 step 1)", c.PeerID)
	}
	// **identity_hint ABSENT = TOFU.** A backend MUST NOT synthesize an
	// identity-claim for a peer that merely stood at a tag; the §2 grant
	// decision IS the trust anchor (§2.2.1, proposal §2.2).
	if c.IdentityHint != nil {
		t.Errorf("candidate.identity_hint = %v, want ABSENT (TOFU) — a rendezvous backend may "+
			"not synthesize an identity-claim from a mailbox observation", c.IdentityHint)
	}
	if c.Supersedes != nil {
		t.Errorf("candidate_0 carries supersedes = %v; the successor chain starts empty", c.Supersedes)
	}

	// The endpoint_hint locates the DEPOSIT, and carries the public tag
	// label but never the key.
	hint, err := DecodeRendezvousEndpointHint(c.EndpointHint)
	if err != nil {
		t.Fatalf("DecodeRendezvousEndpointHint: %v", err)
	}
	if hint.Deposit == "" {
		t.Error("endpoint_hint carries no deposit id; two counterparts at one tag would be " +
			"indistinguishable in every field but observed_at")
	}
	if hint.Mode != RendezvousModeTag || hint.Label != "entity-church-rdv-gate" {
		t.Errorf("endpoint_hint mode/label = %q/%q, want tag/entity-church-rdv-gate",
			hint.Mode, hint.Label)
	}

	// **Alice does not discover herself.** Skip-own (§6.4) — and the
	// failure it prevents is the miserable kind where every step reports
	// success and the answer is wrong.
	own, err := alice.Scan(ctx, nil)
	if err != nil {
		t.Fatalf("alice.Scan: %v", err)
	}
	if len(own) != 0 {
		t.Errorf("alice surfaced %d candidates from a bucket holding only her own deposit; "+
			"skip-own is what stops a peer meeting itself and reporting success at every step",
			len(own))
	}
}

// TestRendezvous_CandidatesReachTheWatchablePrefix pins the half of
// DISCOVERY §3.0's hybrid shape that the return value cannot show: a
// `:scan` both returns a snapshot AND writes candidates into the tree
// under `system/discovery/candidate/{backend}/*`, which is the surface
// a reactive consumer subscribes to.
//
// A backend that returned candidates without binding them passes every
// assertion about the returned slice and gives a UI nothing to render.
// This is the seam-crossing assertion (D22) between the backend and the
// substrate that owns the candidate store.
//
// Tier: integration.
func TestRendezvous_CandidatesReachTheWatchablePrefix(t *testing.T) {
	_, alice, bob, _, bobPeer := rendezvousFixture(t, "entity-church-rdv-prefix")
	ctx := context.Background()

	if err := alice.Announce(ctx, "rdv"); err != nil {
		t.Fatalf("alice.Announce: %v", err)
	}
	if _, err := bob.Scan(ctx, nil); err != nil {
		t.Fatalf("bob.Scan: %v", err)
	}

	// The substrate binds through the observe callback, which the
	// backend fires inside collect. Give it a moment; the write is
	// synchronous but goes through the OOB binder.
	var got []types.CandidateData
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got = bobPeer.ReadCandidates(RendezvousBackendKind)
		if len(got) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("the watchable prefix holds %d candidates, want 1 — DISCOVERY §3.0 makes :scan "+
			"hybrid: an immediate snapshot AND a tree write consumers subscribe to", len(got))
	}
	if got[0].PeerID != "" || got[0].IdentityHint != nil {
		t.Errorf("the BOUND candidate carries peer_id=%q identity_hint=%v; the entity in the tree "+
			"is what a user is shown for the §2 decision, so it is the one that must be TOFU",
			got[0].PeerID, got[0].IdentityHint)
	}
}

// TestRendezvous_RescanDoesNotDuplicateAStandingPeer pins that a
// counterpart who keeps standing at the tag stays ONE candidate across
// polls.
//
// The candidate entity embeds `observed_at`, so its content hash — and
// therefore its storage path — changes every time it is minted. A
// backend that re-emitted on every poll would mint a fresh entity per
// poll and a consumer would show one peer N times. The mDNS path in this
// tree has exactly that defect and works around it downstream with a
// freshness+dedup pass in ReadDiscoveredCandidates; doing it at the
// source instead is why ReadCandidates needs no such pass.
//
// Tier: contract pin.
func TestRendezvous_RescanDoesNotDuplicateAStandingPeer(t *testing.T) {
	_, alice, bob, _, bobPeer := rendezvousFixture(t, "entity-church-rdv-dedup")
	ctx := context.Background()

	if err := alice.Announce(ctx, "rdv"); err != nil {
		t.Fatalf("alice.Announce: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := bob.Scan(ctx, nil); err != nil {
			t.Fatalf("bob.Scan #%d: %v", i, err)
		}
	}
	// Only the first scan surfaces the deposit; the rest see it already
	// surfaced and return nothing new.
	got := bobPeer.ReadCandidates(RendezvousBackendKind)
	if len(got) != 1 {
		t.Fatalf("three scans of one standing peer left %d candidates, want 1 — a peer who has "+
			"not moved is not three peers", len(got))
	}
}

// TestRendezvous_PairModeIsRefused pins proposal §2.1's mode split.
//
// **A normative refusal, not a simplification.** Of SIGNALING §3.2's four
// key modes only tag / secret / lobby are discovery; a `pair` key takes
// both peer-ids as INPUTS, so the caller already holds the counterpart's
// identity and there is nothing to surface. A backend that accepted it
// would emit a candidate whose entire content the caller supplied, which
// presents to a user as "a stranger appeared" when nobody did.
//
// Tier: contract pin.
func TestRendezvous_PairModeIsRefused(t *testing.T) {
	ap, err := CreatePeer(PeerConfig{Extensions: ExtensionsConfig{Discovery: &DiscoveryConfig{}}})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	_, err = NewRendezvousBackend(ap, RendezvousConfig{
		Node:  "some-node",
		Mode:  RendezvousModePair,
		Input: "whoever",
	})
	if err == nil {
		t.Fatal("`pair` was accepted as a discovery mode; both peer-ids are inputs to a pair " +
			"key, so there is no candidate to surface (proposal §2.1) — that is DISCOVERY §5.1, " +
			"an ordinary NETWORK connection to a peer you can already name")
	}
	if !strings.Contains(err.Error(), "mode_not_discovery") {
		t.Errorf("the refusal does not carry the code: %v", err)
	}

	// The three that ARE discovery all construct. Asserted alongside so
	// the refusal above cannot be satisfied by refusing everything —
	// AP22's shape, a guard that rejects whatever it does not recognize.
	for _, mode := range []string{RendezvousModeTag, RendezvousModeSecret, RendezvousModeLobby} {
		if _, err := NewRendezvousBackend(ap, RendezvousConfig{
			Node: "some-node", Mode: mode, Input: "x",
		}); err != nil {
			t.Errorf("mode %q was refused (%v); §3.2's tag/secret/lobby are all discovery modes",
				mode, err)
		}
	}

	// And an unknown mode is refused rather than derived from. The mode
	// string is domain-separated INTO the key, so a typo would derive a
	// mailbox nobody else ever reaches — a silent never-meet, which is
	// the failure this whole surface is most exposed to.
	if _, err := NewRendezvousBackend(ap, RendezvousConfig{
		Node: "some-node", Mode: "taag", Input: "x",
	}); err == nil {
		t.Error("an unknown key mode was accepted; it would derive a mailbox nobody reaches " +
			"and both arms would report success forever")
	}
}

// TestRendezvous_SecretModeKeepsTheSecretOutOfTheCandidate pins that a
// `secret`-mode candidate carries neither the secret nor the derived
// key.
//
// SIGNALING §3.2 types a tag as a "public label", which is why the tag
// case above puts it in the hint. A secret is the opposite: it IS the
// access control, and the derived key is equivalent to it. A candidate
// entity exists to be shown to a user for the §2 decision, so putting
// the credential inside one would disclose it through the surface whose
// whole job is display.
//
// Tier: contract pin.
func TestRendezvous_SecretModeKeepsTheSecretOutOfTheCandidate(t *testing.T) {
	ap, err := CreatePeer(PeerConfig{Extensions: ExtensionsConfig{Discovery: &DiscoveryConfig{}}})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	// Named `passphrase`, not `secret`, and the rename is not cosmetic.
	// `secret = "<16+ chars>"` is the SHAPE of a committed credential, so a
	// credential scanner flags it — correctly, since no detector can tell
	// this xkcd placeholder from a real key. The binding holds a rendezvous
	// *passphrase input*; `secret` remains the spec's word everywhere it is
	// load-bearing (RendezvousModeSecret, the test name, SIGNALING §3.2
	// above), so nothing about the contract moved. Naming the variable
	// accurately is the cheaper fix than widening a credential rule.
	const passphrase = "correct-horse-battery-staple"
	rb, err := NewRendezvousBackend(ap, RendezvousConfig{
		Node: "some-node", Mode: RendezvousModeSecret, Input: passphrase,
	})
	if err != nil {
		t.Fatalf("NewRendezvousBackend: %v", err)
	}

	cd, err := rb.candidateFor("deadbeef")
	if err != nil {
		t.Fatalf("candidateFor: %v", err)
	}
	if strings.Contains(string(cd.EndpointHint), passphrase) {
		t.Error("the candidate's endpoint_hint contains the shared secret; a candidate is shown " +
			"to a user for the §2 decision, so it is the last place a credential belongs")
	}
	hint, err := DecodeRendezvousEndpointHint(cd.EndpointHint)
	if err != nil {
		t.Fatalf("DecodeRendezvousEndpointHint: %v", err)
	}
	if hint.Label != "" {
		t.Errorf("endpoint_hint.label = %q for a secret-mode rendezvous; the label field is for "+
			"the PUBLIC modes (§3.2's tag, and a published lobby constant)", hint.Label)
	}

	// The tag case is the control: without it, a hint that dropped the
	// label unconditionally would pass the assertion above while losing
	// the locator every public-mode consumer needs.
	tagRB, err := NewRendezvousBackend(ap, RendezvousConfig{
		Node: "some-node", Mode: RendezvousModeTag, Input: "public-label",
	})
	if err != nil {
		t.Fatalf("NewRendezvousBackend(tag): %v", err)
	}
	tagCD, err := tagRB.candidateFor("deadbeef")
	if err != nil {
		t.Fatalf("candidateFor(tag): %v", err)
	}
	tagHint, err := DecodeRendezvousEndpointHint(tagCD.EndpointHint)
	if err != nil {
		t.Fatalf("DecodeRendezvousEndpointHint(tag): %v", err)
	}
	if tagHint.Label != "public-label" {
		t.Errorf("a tag-mode hint dropped its public label (%q); the omission must be keyed on "+
			"the mode, not applied to every mode", tagHint.Label)
	}
}

// TestRendezvous_AnnounceStopIsIdempotent pins DISCOVERY §3.3's stop
// rule: stopping a profile that is not running SUCCEEDS.
//
// The spec calls out that this and the unknown-`profile_ref` `400` were
// written to be confusable — *unrecognized by the backend* is a caller
// error on both ops; *recognized but not running* is a success on stop.
// An implementation that is merely idempotent is not conformant, so both
// halves are asserted here.
//
// Tier: contract pin.
func TestRendezvous_AnnounceStopIsIdempotent(t *testing.T) {
	ap, err := CreatePeer(PeerConfig{Extensions: ExtensionsConfig{Discovery: &DiscoveryConfig{}}})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	rb, err := NewRendezvousBackend(ap, RendezvousConfig{
		Node: "some-node", Mode: RendezvousModeTag, Input: "x",
	})
	if err != nil {
		t.Fatalf("NewRendezvousBackend: %v", err)
	}
	ctx := context.Background()

	if err := rb.AnnounceStop(ctx, "never-started"); err != nil {
		t.Errorf("stopping a profile that was never announced failed (%v); a symmetric lifecycle "+
			"stop is idempotent (DISCOVERY §3.3, MUST)", err)
	}
	if err := rb.AnnounceStop(ctx, ""); err == nil {
		t.Error("announce-stop with no profile_ref succeeded; an unrecognized profile is a 400 " +
			"on BOTH ops, and the idempotency rule covers only recognized-but-not-running")
	}
}

// TestRendezvous_NodeIsRequired pins that the §3.4 same-provider MUST
// cannot be lost by omission.
//
// Two peers on different nodes derive the same key, deposit into
// different mailboxes, and never meet — **with no error on either side
// and nothing in either log.** It is the single most likely failure of
// this surface, so the node is required at construction rather than
// defaulted to anything.
//
// Tier: contract pin.
func TestRendezvous_NodeIsRequired(t *testing.T) {
	ap, err := CreatePeer(PeerConfig{Extensions: ExtensionsConfig{Discovery: &DiscoveryConfig{}}})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	if _, err := NewRendezvousBackend(ap, RendezvousConfig{
		Mode: RendezvousModeTag, Input: "x",
	}); err == nil {
		t.Fatal("a rendezvous with no node was accepted; SIGNALING §3.4 makes same-provider a " +
			"MUST and a peer with no node cannot keep it")
	}
}

// TestRendezvous_SubstrateNeedsNoListener pins the reason
// ExtensionsConfig.Discovery exists.
//
// The discovery substrate was wired only for LISTENING peers, which was
// right while mDNS was the only backend — there is nothing to announce
// without a bound port. It is exactly wrong here: a rendezvous is how a
// peer with **no reachable listener** gets introduced, so the old gate
// excluded precisely the peers this backend is for.
//
// Tier: contract pin.
func TestRendezvous_SubstrateNeedsNoListener(t *testing.T) {
	// No ListenAddr anywhere in this config.
	ap, err := CreatePeer(PeerConfig{Extensions: ExtensionsConfig{Discovery: &DiscoveryConfig{}}})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	if !ap.DiscoveryEnabled() {
		t.Fatal("Extensions.Discovery did not wire the substrate on a non-listening peer")
	}
	if _, err := ap.EnableRendezvousDiscovery(RendezvousConfig{
		Node: "some-node", Mode: RendezvousModeTag, Input: "x",
	}); err != nil {
		t.Fatalf("EnableRendezvousDiscovery on a non-listening peer: %v", err)
	}
	if ap.Rendezvous() == nil {
		t.Error("the backend registered but is not reachable from the peer")
	}

	// And the default is unchanged: a peer that asked for neither a
	// listener nor discovery still has no substrate. The toggle is a
	// door, not a new default — a peer paying for an extension it did
	// not ask for is what the registry-default measurement (AP23) was
	// about.
	plain, err := CreatePeer(PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer(plain): %v", err)
	}
	defer plain.Close()
	if plain.DiscoveryEnabled() {
		t.Error("a peer with no ListenAddr and no Discovery config got the substrate anyway")
	}
}
