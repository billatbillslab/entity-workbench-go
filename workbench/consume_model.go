package workbench

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/fetch"
)

var _ Model[ConsumeOutput] = (*ConsumeModel)(nil)

// ConsumeModel is the renderer-neutral model for **verifying a published
// site over the CDN corridor** — the app-tier surface of `fetch`'s
// consume stack (manifest → signature → CHAMP trie walk → leaves).
//
// **It renders the verification CHAIN, not the site.** That is a
// deliberate difference from `entity-browser-rust`'s Site Browser, which
// renders the pages and puts trust in the chrome ("not verified" as a
// banner). Both are legitimate and the cohort needs both: theirs is the
// end-user reading surface, ours is the operator's inspector — you point
// it at an origin and it tells you which link of the chain held, which
// one broke, and what each one actually proves. The two answer different
// questions about the same bytes, and the interesting UX research is in
// what an operator does with the second one.
//
// The step list is the load-bearing part of the design. Every step in
// this chain is satisfiable by a dishonest origin except the walk, and a
// UI that collapses the chain to a green tick teaches an operator that
// "verified" is one fact. It is six, and one of them (`published_at`) is
// a moment rather than a state.
//
// Threading: Verify blocks on network I/O and takes no lock while doing
// it; Render is cheap and safe to call from a UI thread at any time,
// including mid-run (it reports Running with the steps completed so
// far). That split exists because the console and Avalonia renderers
// both drive this off a UI thread and neither may block one.
type ConsumeModel struct {
	client *http.Client

	mu      sync.Mutex
	origin  string
	peerID  string
	pin     *types.TransportEndpoint
	opts    fetch.ConsumeOpts
	out     ConsumeOutput
	running bool
}

// ConsumeStep is one link of the chain with its verdict, and — the part
// a UI must not drop — what it proves.
type ConsumeStep struct {
	Name   string
	Status ConsumeStatus
	// Detail is the concrete thing this step touched: a URL, a count,
	// an algorithm. Never a summary adjective.
	Detail string
	// Proves is the honest scope of a green verdict on this step. A
	// renderer SHOULD surface it — the whole failure mode this model is
	// shaped against is a green tick that is read as more than it is.
	Proves string
	Err    string
}

// ConsumeStatus is a step verdict.
type ConsumeStatus string

const (
	StepPending ConsumeStatus = "pending"
	StepOK      ConsumeStatus = "ok"
	StepFailed  ConsumeStatus = "failed"
	StepSkipped ConsumeStatus = "skipped"
)

// ConsumeKeyRow is one committed key's verdict.
type ConsumeKeyRow struct {
	Key        string
	Hash       string
	Type       string
	Bytes      int
	Reconciled bool
	Err        string
}

// ConsumeOutput is what a renderer draws.
type ConsumeOutput struct {
	Origin string
	PeerID string
	// Discovered is true when the layout came from the origin's
	// well-known `transport-profile`, false when it was pinned by the
	// operator. A UI must show which: a pinned layout is a human's
	// claim about the origin, and an origin that serves no profile is
	// indistinguishable from one whose layout we guessed wrong.
	Discovered bool

	Steps []ConsumeStep
	Keys  []ConsumeKeyRow

	RootHash    string
	Prefix      string
	Seq         uint64
	PublishedAt uint64
	// AbsolutePrefix is `prefix` resolved to the absolute form keys
	// reconstruct against (EXTENSION-TREE §3.3). Shown beside the raw
	// field because the two differ for two of the three admissible
	// shapes and every publisher in this cohort picks a different one.
	AbsolutePrefix string

	Nodes      int
	KeysTotal  int
	KeysOK     int
	KeysFailed int

	AbsentProbe   string
	AbsentCorrect bool

	// Verified is true only when every step that ran came back OK.
	Verified bool
	// Incomplete marks the specific verdict that the origin does not
	// serve the closure its own signed root commits to. It is called
	// out separately from Err because it is the one failure that is a
	// statement about the PUBLISHER rather than about reachability.
	Incomplete bool
	Err        string

	Running  bool
	Elapsed  time.Duration
	LastRun  time.Time
	HasRun   bool
	Duration string
}

// NewConsumeModel builds the model. A nil client means the default.
func NewConsumeModel(client *http.Client) *ConsumeModel {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &ConsumeModel{client: client, opts: fetch.ConsumeOpts{Bodies: true, Reconcile: true}}
}

// SetOrigin points the model at an origin, clearing any prior result.
// A result left on screen while the operator types a new origin is a
// result attributed to the wrong publisher.
func (m *ConsumeModel) SetOrigin(origin string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.origin = strings.TrimSpace(origin)
	m.out = ConsumeOutput{Origin: m.origin, PeerID: m.peerID}
}

// SetPeerID sets the expected publisher. With a discovered layout it is
// a cross-check; with a pinned one it is REQUIRED, because it is the key
// the signature verifies against.
func (m *ConsumeModel) SetPeerID(peerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peerID = strings.TrimSpace(peerID)
}

// Pin supplies a hand-written layout, skipping profile discovery. Pass
// nil to go back to discovery.
//
// This exists for origins that serve no `transport-profile` — which is
// conformant (§6.5.4 makes profile distribution out-of-band in v1) and
// is what `entity-core-go`'s federation origin does. See arch R-28.
func (m *ConsumeModel) Pin(ep *types.TransportEndpoint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pin = ep
}

// SetOpts tunes what the run does beyond the mandatory chain.
func (m *ConsumeModel) SetOpts(opts fetch.ConsumeOpts) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts = opts
}

// Render returns the current view. Safe to call at any time.
func (m *ConsumeModel) Render() ConsumeOutput {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.out
	out.Running = m.running
	out.Origin, out.PeerID = m.origin, m.peerID
	return out
}

// Verify runs the whole chain and records it. It is safe to call
// concurrently with Render and refuses to run twice at once — a second
// run would interleave two origins' steps into one list.
func (m *ConsumeModel) Verify(ctx context.Context) ConsumeOutput {
	m.mu.Lock()
	if m.running {
		out := m.out
		out.Running = true
		m.mu.Unlock()
		return out
	}
	origin, peerID, pin, opts := m.origin, m.peerID, m.pin, m.opts
	m.running = true
	m.out = ConsumeOutput{Origin: origin, PeerID: peerID, Discovered: pin == nil}
	m.mu.Unlock()

	started := time.Now()
	out := runConsume(ctx, m.client, origin, peerID, pin, opts)
	out.Elapsed = time.Since(started)
	out.Duration = out.Elapsed.Round(time.Millisecond).String()
	out.LastRun = time.Now()
	out.HasRun = true

	m.mu.Lock()
	m.running = false
	m.out = out
	m.mu.Unlock()
	return out
}

// runConsume is the chain, expressed as steps. Pure with respect to the
// model so it can be tested without one.
func runConsume(ctx context.Context, client *http.Client, origin, peerID string,
	pin *types.TransportEndpoint, opts fetch.ConsumeOpts) ConsumeOutput {

	out := ConsumeOutput{Origin: origin, PeerID: peerID, Discovered: pin == nil}
	fail := func(step ConsumeStep, err error) ConsumeOutput {
		step.Status, step.Err = StepFailed, err.Error()
		out.Steps = append(out.Steps, step)
		out.Err = err.Error()
		out.Incomplete = errors.Is(err, fetch.ErrIncompleteWalk)
		return out
	}

	if origin == "" {
		return fail(ConsumeStep{Name: "layout"}, errors.New("no origin given"))
	}

	// 1 — layout.
	layoutStep := ConsumeStep{
		Name:   "layout",
		Proves: "where the publisher says its objects live. Nothing is derived by convention.",
	}
	var layout fetch.Layout
	var err error
	if pin != nil {
		if peerID == "" {
			return fail(layoutStep, errors.New("a pinned layout needs the publisher's peer-id — "+
				"it is the key the signature verifies against, and nothing else supplies it"))
		}
		layout, err = fetch.PinnedLayout(origin, peerID, *pin)
		layoutStep.Detail = "PINNED by the operator (this origin serves no transport-profile)"
		layoutStep.Proves = "nothing about the origin — a pinned layout is a human's claim. " +
			"A wrong pin and a withholding origin look identical from here."
	} else {
		layout, err = fetch.LoadLayout(ctx, origin, client)
		layoutStep.Detail = strings.TrimRight(origin, "/") + "/" + fetch.ProfileFile
	}
	if err != nil {
		return fail(layoutStep, err)
	}
	if peerID != "" && peerID != layout.PeerID {
		return fail(layoutStep, fmt.Errorf("this origin's profile is peer %s, you asked for %s — "+
			"the bytes and the name would not be the same publisher's", layout.PeerID, peerID))
	}
	out.PeerID = layout.PeerID
	layoutStep.Status = StepOK
	if pin == nil {
		layoutStep.Detail += fmt.Sprintf("  (peer %s, content_layout %q)",
			layout.PeerID, layout.Endpoint.ContentLayout)
	}
	out.Steps = append(out.Steps, layoutStep)

	c := fetch.NewConsumer(layout, client)

	// 2+3 — manifest and signature. fetch runs them as one operation
	// because the signature is resolved from the hash the manifest
	// recomputed to; splitting them in the UI would suggest an ordering
	// a caller could choose.
	rootStep := ConsumeStep{
		Name: "manifest + signature",
		Proves: "the publisher signed this root, and the key came out of their peer-id — " +
			"no key distribution anywhere in the chain. It does NOT prove the root is current.",
	}
	root, err := c.VerifiedRoot(ctx)
	if err != nil {
		return fail(rootStep, err)
	}
	rootStep.Status = StepOK
	rootStep.Detail = fmt.Sprintf("%s → %s (%s)", root.ManifestURL, root.SignatureURL,
		root.Signature.Algorithm)
	out.Steps = append(out.Steps, rootStep)

	out.RootHash = root.Data.RootHash.String()
	out.Prefix = root.Data.Prefix
	out.AbsolutePrefix = fetch.AbsolutePrefix(root.Data.Prefix, layout.PeerID)
	out.Seq = root.Data.Seq
	out.PublishedAt = root.Data.PublishedAt

	// 4 — the walk. The only step a withholding origin cannot pass.
	walkStep := ConsumeStep{
		Name: "trie walk",
		Proves: "the origin serves every CHAMP node its signed root commits to. This is the " +
			"one step a lying or withholding origin cannot pass — every other step is " +
			"satisfiable by an origin serving a signed root that commits to nothing.",
	}
	walk, err := c.Walk(ctx, root.Data.RootHash)
	if err != nil {
		return fail(walkStep, err)
	}
	walkStep.Status = StepOK
	walkStep.Detail = fmt.Sprintf("%d CHAMP nodes resolved and hash-verified", walk.Nodes())
	out.Steps = append(out.Steps, walkStep)
	out.Nodes = walk.Nodes()

	// 5 — enumerate, plus the leaf and reconcile passes.
	//
	// ConsumeFrom, not Consume: this function has already fetched the
	// manifest, verified the signature and walked the trie, and Consume
	// would do all three again. That is not just wasteful — it would
	// mean **two verifications run and one reported**, so a root that
	// changed between them would be resolved silently in favour of
	// whichever the report happened to carry.
	rep := c.ConsumeFrom(ctx, root, walk, opts)
	enumStep := ConsumeStep{
		Name:   "enumerate",
		Status: StepOK,
		Detail: fmt.Sprintf("%d committed keys", len(rep.Walk.Bindings)),
		Proves: "the exact committed key set — no more, and none hidden behind a link the " +
			"origin will not resolve.",
	}
	out.Steps = append(out.Steps, enumStep)

	out.KeysTotal = len(rep.Keys)
	for _, k := range rep.Keys {
		row := ConsumeKeyRow{Key: k.Key, Hash: k.TrieHash.String(), Type: k.Type,
			Bytes: k.Bytes, Reconciled: k.Reconciled}
		if k.Err != nil {
			row.Err = k.Err.Error()
			out.KeysFailed++
		} else {
			out.KeysOK++
		}
		out.Keys = append(out.Keys, row)
	}

	bodyStep := ConsumeStep{Name: "leaf bodies", Status: StepSkipped,
		Detail: "not requested",
		Proves: "each body is the pre-image of the hash the signed root bound to its key."}
	if opts.Bodies {
		bodyStep.Status = StepOK
		bodyStep.Detail = fmt.Sprintf("%d verified, %d failed", out.KeysOK, out.KeysFailed)
		if out.KeysFailed > 0 {
			bodyStep.Status = StepFailed
			bodyStep.Err = fmt.Sprintf("%d of %d committed keys failed", out.KeysFailed, out.KeysTotal)
		}
	}
	out.Steps = append(out.Steps, bodyStep)

	recStep := ConsumeStep{Name: "reconcile", Status: StepSkipped, Detail: "not requested",
		Proves: "the origin's own tree-leaf URL agrees with the root it SIGNED. An origin " +
			"answering differently on the two paths is serving two trees, one of them unsigned."}
	if opts.Reconcile {
		n := 0
		for _, k := range out.Keys {
			if k.Reconciled {
				n++
			}
		}
		recStep.Status = StepOK
		recStep.Detail = fmt.Sprintf("%d/%d keys agree with the advertised leaf", n, out.KeysTotal)
		if out.KeysFailed > 0 {
			recStep.Status = StepFailed
		}
	}
	out.Steps = append(out.Steps, recStep)

	absStep := ConsumeStep{Name: "absent control", Status: StepSkipped, Detail: "no probe set",
		Proves: "a key nobody published reports ABSENT rather than as an unreachable origin. " +
			"Meaningful only after a non-empty enumeration — an empty root answers absent " +
			"to everything."}
	if rep.AbsentProbe != "" {
		out.AbsentProbe = rep.AbsentProbe
		out.AbsentCorrect = rep.AbsentCorrect
		if rep.AbsentCorrect {
			absStep.Status, absStep.Detail = StepOK, fmt.Sprintf("%q → ABSENT", rep.AbsentProbe)
		} else {
			absStep.Status, absStep.Detail = StepFailed, fmt.Sprintf("%q", rep.AbsentProbe)
			if rep.AbsentErr != nil {
				absStep.Err = rep.AbsentErr.Error()
			}
		}
	}
	out.Steps = append(out.Steps, absStep)

	out.Verified = out.KeysFailed == 0
	for _, s := range out.Steps {
		if s.Status == StepFailed {
			out.Verified = false
		}
	}
	return out
}

// FreshnessNote is the sentence a renderer puts under a green result.
//
// It is a function rather than a constant because the honest claim names
// the moment: §6.5.3.1 / D6 / D7 — a quiet publisher and a withholding
// origin are indistinguishable at the consumer, so "verified" without a
// timestamp is a claim the corridor cannot support.
func (o ConsumeOutput) FreshnessNote() string {
	if !o.Verified || o.PublishedAt == 0 {
		return ""
	}
	t := time.UnixMilli(int64(o.PublishedAt)).UTC()
	return fmt.Sprintf("verified as of published_at %s (seq %d) — never simply \"verified\": "+
		"a quiet publisher and a withholding origin are indistinguishable from here",
		t.Format(time.RFC3339), o.Seq)
}
