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

	"entity-workbench-go/entitysdk/publishedroot"
	"entity-workbench-go/fetch"
)

var _ Model[BrowseOutput] = (*BrowseModel)(nil)

// browse_model.go — the renderer-neutral **browser**: a location, a
// history, a page, and the provenance of the exact bytes on screen.
//
// # What this is, against what we already had
//
// [ConsumeModel] answers *"is this origin lying?"* for one origin an
// operator already knows the URL of. It is an inspector and it renders a
// chain instead of a page. That was the right first surface and it is
// half a journey: it cannot answer *what is out there*, *take me
// there*, or *where am I*.
//
// This model is the other half. The pipeline it drives is fixed by the
// substrate, not invented here (arch `STATUS-2026-08-19-b` §2):
//
//	name → registry resolve → peer-id + transports
//	     → the target's signed root
//	     → walk the trie → THE KEY SET IS THE METADATA
//	     → interpret the first segment by convention → sites/{id}/manifest
//
// Every one of those arrows is a check, and **the interesting design
// question is what a user is shown while they happen** — because at the
// end of it the screen shows a page, and a page looks the same whether
// eleven checks passed or none did.
//
// # The contrast with `entity-browser-rust`, stated so it is testable
//
// Theirs renders the pages and puts trust in the chrome — the reader's
// browser, and the right shape for a reader. Ours keeps the chain
// **beside** the page as a first-class, navigable object: eleven steps,
// each with its verdict and what a green verdict on that step actually
// proves, for the bytes currently displayed and no others.
//
// Two rules fall out of that and both are enforced below rather than
// left to a renderer's judgement:
//
//  1. **A step that could not be established is shown failing, never
//     omitted.** A chain that renders six green rows and then stops
//     looks green at a glance.
//  2. **No green verdict is ever rendered as the bare word "verified".**
//     Every one of them is *verified as of* a moment — the registry's
//     `published_at`, the binding's `issued_at`, the target's
//     `published_at` — and a quiet publisher is indistinguishable from a
//     withholding origin at every one of them (§6.5.3.1, D6/D7).
//
// Threading matches [ConsumeModel]: Open blocks on network I/O and holds
// no lock while doing it; Render is cheap and safe from a UI thread at
// any time, including mid-navigation.

// Address is where the browser is pointed.
//
// It is a *location*, not a URL: the wire URLs are the publisher's to
// declare (that is the whole AP21 lesson) and are derived at navigation
// time from the transport profile the binding names. What a user types
// and what history stores is this.
type Address struct {
	// Name is the registry name, when the address was reached by name.
	// Empty means the user supplied a peer-id directly — which is a
	// materially weaker starting point and the chain says so.
	Name string
	// PeerID is the target publisher. Populated from the resolve when
	// the address was a name.
	PeerID string
	SiteID string
	Page   string
}

// Host is the addressing authority: the name if there is one, else the
// peer-id. It is what an address bar shows.
func (a Address) Host() string {
	if a.Name != "" {
		return a.Name
	}
	return a.PeerID
}

// String renders the canonical `entity://` form.
func (a Address) String() string {
	var b strings.Builder
	b.WriteString("entity://")
	b.WriteString(a.Host())
	if a.SiteID != "" {
		b.WriteString("/")
		b.WriteString(a.SiteID)
	}
	if a.Page != "" {
		b.WriteString("/")
		b.WriteString(a.Page)
	}
	return b.String()
}

// ParseAddress reads what a user typed.
//
// Accepted: `entity://host/site/page`, `host/site/page`, `host`. The
// host is a **peer-id when it parses as one** and a registry name
// otherwise — the same literal-or-parse-as-peer-id decision NETWORK
// §6.5.6 makes at the URL demux, made once, here, so no surface below
// has to guess.
func ParseAddress(s string) (Address, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "entity://")
	s = strings.Trim(s, "/")
	if s == "" {
		return Address{}, errors.New("an address needs at least a name or a peer-id")
	}
	host, rest, _ := strings.Cut(s, "/")
	site, page, _ := strings.Cut(rest, "/")

	a := Address{SiteID: site, Page: page}
	if _, _, err := publishedroot.DeriveKey(host); err == nil {
		a.PeerID = host
	} else {
		if _, err := fetch.NormalizeName(host); err != nil {
			return Address{}, err
		}
		a.Name = host
	}
	return a, nil
}

// RegistryRow is one name in the pinned registry's browsable list.
type RegistryRow struct {
	Name string
	// Target is the peer-id the binding names, once resolved. Empty
	// until a row is resolved — enumeration yields names and binding
	// hashes, and resolving each one is a separate verification.
	Target string
	// BindingHash is what the signed root committed for this name.
	BindingHash string
	// Committed is true when the name is in the signed key set. False
	// means it appeared **only in the served listing** — the origin is
	// advertising a name the registry has not signed into this root, and
	// a row like that must never be presented as available.
	Committed bool
	// Listed is true when the served menu also carries it. A committed
	// name that is not listed is a name hidden from the menu; benign
	// alone, and the shape a targeted withholding takes.
	Listed bool
	// ExpiresAt is the binding's `issued_at + ttl`, once resolved.
	ExpiresAt string
	// Err is why this row would not resolve, if it was tried.
	Err string
}

// BrowseOutput is what a renderer draws.
type BrowseOutput struct {
	// Address is the canonical location of what is displayed. Empty
	// before the first navigation.
	Address string
	// Host / Site / Page are the parts, for a renderer that wants to
	// draw them separately (an address bar with a name segment styled
	// differently from the path, say).
	Host string
	Site string
	Page string

	// Registry is the pinned registry's peer-id, or empty. **The pin is
	// the only thing a user supplies out-of-band** and the only thing
	// trusted a priori, so it is on every render, not tucked in a
	// settings pane.
	Registry string
	// RegistryOrigin is where its bytes are being served from — a
	// different fact from who signs them, and the §6a.1a fourth actor
	// lives in the gap between the two.
	RegistryOrigin string
	// RegistryDiscovered is false when the registry's layout was pinned
	// by hand rather than read from a `transport-profile`.
	RegistryDiscovered bool
	// RegistryFresh is the registry root's `published_at`, rendered.
	RegistryFresh string

	// Names is the registry browser's rows.
	Names []RegistryRow
	// NamesAuthority says where Names came from, in words a user reads:
	// the walk is authoritative, the listing is a menu.
	NamesAuthority string
	// NamesNote carries a reconciliation disagreement, when there is
	// one. Empty when the menu and the signed key set agree.
	NamesNote string

	// Sites is every site the current target's signed root commits to —
	// the authoritative "what is published here".
	Sites []string
	// SiteDefaulted is true when the address named no site and one was
	// chosen for the user.
	//
	// It is surfaced because there is **no landing-site field anywhere in
	// the tree** to consult: `entity-browser-rust` carries theirs in an
	// `entity-deployment.json` beside the emission, which is deployment
	// configuration and not something a signature covers. So the choice
	// here is first-in-byte-order, which is arbitrary, and an arbitrary
	// choice presented as "the site" is a small lie that compounds — a
	// user reads a page believing it is the publisher's front door.
	SiteDefaulted bool

	// Site content, via [SiteModel] over a [RemoteSiteResolver].
	Content SiteRenderOutput

	// Steps is the trust chain for THESE bytes. Never a summary, never
	// collapsed, and a step that could not run is Failed or Skipped with
	// a reason — never absent.
	Steps []ConsumeStep

	// Freshness is the honest one-line scope of the whole chain. It
	// names a moment and never says "verified" alone.
	Freshness string

	CanBack    bool
	CanForward bool
	Running    bool
	// Err is a navigation-level failure. The step list says which link
	// broke; this is the sentence.
	Err string

	// hostName / hostPeer are the parts Host was built from, kept so
	// history can be re-pushed without re-parsing the rendered form.
	hostName string
	hostPeer string
}

// BrowseModel is the browser.
type BrowseModel struct {
	client *http.Client

	mu sync.Mutex

	reg       *fetch.Registry
	regRoot   *fetch.VerifiedRoot
	regPinned bool
	nameSet   *fetch.NameSet

	// targetOrigin overrides where a resolved binding's origin-relative
	// URLs are rooted. Empty means "the registry's own origin", which is
	// correct for every single-host deployment and stated at the seam
	// rather than assumed here.
	targetOrigin string

	history []Address
	hpos    int

	site *SiteModel
	out  BrowseOutput

	running   bool
	listeners []func()

	// Now backs the binding-expiry check. Nil means time.Now.
	Now func() time.Time
}

// NewBrowseModel builds an unpinned browser. Nothing resolves until a
// registry is pinned or an address names a peer-id outright.
func NewBrowseModel(client *http.Client) *BrowseModel {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &BrowseModel{client: client, hpos: -1}
}

// PinRegistry sets the trust root.
//
// `ep` nil means discover the layout from the origin's well-known
// `transport-profile`; non-nil pins it. Both are legitimate and the
// difference is reported on every render — NETWORK §6.5.4 makes profile
// distribution out-of-band in v1, so an origin serving no profile is
// conformant, and a wrong pin is byte-identical to a withholding origin
// from here.
func (m *BrowseModel) PinRegistry(ctx context.Context, origin, peerID string, ep *types.TransportEndpoint) error {
	var (
		layout fetch.Layout
		err    error
	)
	if ep != nil {
		layout, err = fetch.PinnedLayout(origin, peerID, *ep)
	} else {
		layout, err = fetch.LoadLayout(ctx, origin, m.client)
		if err == nil && peerID != "" && layout.PeerID != peerID {
			// The operator pinned a key and the origin advertised a
			// different one. That is not a mismatch to reconcile — it is
			// the origin claiming to be someone else, and the pin wins
			// by refusing rather than by overriding.
			return fmt.Errorf("origin %s advertises peer %s; you pinned %s — refusing rather than "+
				"picking one, because the pin is the only thing here you brought yourself",
				origin, layout.PeerID, peerID)
		}
	}
	if err != nil {
		return err
	}
	reg, err := fetch.NewRegistry(layout, m.client)
	if err != nil {
		return err
	}
	if m.Now != nil {
		reg.Now = m.Now
	}

	m.mu.Lock()
	m.reg, m.regPinned, m.nameSet, m.regRoot = reg, ep != nil, nil, nil
	m.out.Registry = layout.PeerID
	m.out.RegistryOrigin = layout.Origin
	m.out.RegistryDiscovered = ep == nil
	m.mu.Unlock()
	m.fire()
	return nil
}

// SetTargetOrigin overrides where resolved bindings' origin-relative
// URLs are rooted. See [BrowseModel.targetOrigin].
func (m *BrowseModel) SetTargetOrigin(origin string) {
	m.mu.Lock()
	m.targetOrigin = origin
	m.mu.Unlock()
}

// Registry reports the pinned registry, or "" .
func (m *BrowseModel) Registry() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reg == nil {
		return ""
	}
	return m.reg.PeerID()
}

// RefreshNames enumerates the pinned registry — §6a.3a, the walk.
//
// It is a separate operation from navigation on purpose: enumerating is
// O(the registry) and resolving one name is O(1), so a browser that
// enumerated on every navigation would make the cheap path pay for the
// expensive one. A user asks for the list; a user does not ask for it
// again on every click.
func (m *BrowseModel) RefreshNames(ctx context.Context) error {
	m.mu.Lock()
	reg := m.reg
	m.mu.Unlock()
	if reg == nil {
		return errors.New("no registry is pinned: there is nothing to enumerate, and a browser " +
			"cannot invent a name authority")
	}

	set, err := reg.Enumerate(ctx)
	m.mu.Lock()
	defer func() { m.mu.Unlock(); m.fire() }()
	if err != nil {
		m.out.Names = nil
		m.out.NamesAuthority = "enumeration failed"
		m.out.NamesNote = err.Error()
		return err
	}
	m.nameSet = &set
	m.regRoot = &set.Root
	m.out.RegistryFresh = freshnessOf(set.Root.Data.PublishedAt)
	m.out.Names = rowsFrom(set)
	m.out.NamesAuthority = fmt.Sprintf("%d names, from the walk of signed root %s — "+
		"the origin cannot hide one of these without the walk failing",
		len(set.Names), shortHash(set.Root.Data.RootHash.String()))
	m.out.NamesNote = reconcileNote(set)
	return nil
}

// rowsFrom merges the two provenances into one list without flattening
// the difference.
//
// A listed-only name is included — deliberately. Dropping it would hide
// the disagreement, and a browser whose list silently differs from the
// origin's own menu is harder to debug than one that shows the extra row
// marked as uncommitted.
func rowsFrom(set fetch.NameSet) []RegistryRow {
	listed := map[string]bool{}
	for _, l := range set.Listing {
		listed[l] = true
	}
	rows := make([]RegistryRow, 0, len(set.Names)+len(set.AdvertisedOnly))
	for _, e := range set.Names {
		rows = append(rows, RegistryRow{
			Name:        e.Name,
			BindingHash: shortHash(e.BindingHash.String()),
			Committed:   true,
			Listed:      listed[e.Name],
		})
	}
	for _, n := range set.AdvertisedOnly {
		rows = append(rows, RegistryRow{
			Name:      n,
			Committed: false,
			Listed:    true,
			Err: "advertised in the served listing, not committed by the signed root — " +
				"this name has no binding the registry has signed into this root",
		})
	}
	return rows
}

func reconcileNote(set fetch.NameSet) string {
	switch {
	case set.ListingErr != nil:
		return "the origin serves no by-name listing; the walk answered on its own (a listing is " +
			"a convenience, never the authority)"
	case len(set.AdvertisedOnly) > 0 && len(set.CommittedOnly) > 0:
		return fmt.Sprintf("the served menu invents %v and hides %v — it disagrees with the signature in both directions",
			set.AdvertisedOnly, set.CommittedOnly)
	case len(set.AdvertisedOnly) > 0:
		return fmt.Sprintf("the served menu advertises %v with no signed binding behind it", set.AdvertisedOnly)
	case len(set.CommittedOnly) > 0:
		return fmt.Sprintf("the signed root commits to %v, which the served menu omits — "+
			"an origin can hide a name from a menu undetectably, and cannot hide one from the walk", set.CommittedOnly)
	default:
		return ""
	}
}

// Open navigates to an address and pushes it onto the history.
func (m *BrowseModel) Open(ctx context.Context, address string) error {
	addr, err := ParseAddress(address)
	if err != nil {
		m.mu.Lock()
		m.out.Err = err.Error()
		m.mu.Unlock()
		m.fire()
		return err
	}
	if err := m.goTo(ctx, addr); err != nil {
		return err
	}
	m.mu.Lock()
	m.history = append(m.history[:m.hpos+1], m.resolvedAddr())
	m.hpos = len(m.history) - 1
	m.syncNavLocked()
	m.mu.Unlock()
	m.fire()
	return nil
}

// Back / Forward move through history without re-pushing.
//
// They re-run the whole chain rather than replaying a cached page. That
// is a cost and it is the honest one: a cached page is a claim about a
// moment that has passed, and this model's entire contract is that
// nothing on screen outlives the verification that produced it.
func (m *BrowseModel) Back(ctx context.Context) error { return m.step(ctx, -1) }

// Forward is Back's mirror.
func (m *BrowseModel) Forward(ctx context.Context) error { return m.step(ctx, +1) }

func (m *BrowseModel) step(ctx context.Context, delta int) error {
	m.mu.Lock()
	next := m.hpos + delta
	if next < 0 || next >= len(m.history) {
		m.mu.Unlock()
		return nil
	}
	addr := m.history[next]
	m.hpos = next
	m.syncNavLocked()
	m.mu.Unlock()

	err := m.goTo(ctx, addr)
	m.fire()
	return err
}

func (m *BrowseModel) syncNavLocked() {
	m.out.CanBack = m.hpos > 0
	m.out.CanForward = m.hpos >= 0 && m.hpos < len(m.history)-1
}

func (m *BrowseModel) resolvedAddr() Address {
	return Address{
		Name:   m.out.hostName,
		PeerID: m.out.hostPeer,
		SiteID: m.out.Site,
		Page:   m.out.Page,
	}
}

// goTo runs the chain. It holds no lock across network I/O.
func (m *BrowseModel) goTo(ctx context.Context, addr Address) error {
	m.mu.Lock()
	reg, set, targetOrigin := m.reg, m.nameSet, m.targetOrigin
	m.running = true
	m.out.Running = true
	m.out.Err = ""
	m.out.Steps = nil
	m.mu.Unlock()
	m.fire()

	nav := newChain()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.out.Running = false
		m.out.Steps = nav.steps
		m.out.Freshness = nav.freshness
		m.mu.Unlock()
	}()

	// ---------------------------------------------------------------
	// hop 1 — the name
	// ---------------------------------------------------------------
	var (
		res    fetch.NameResolution
		origin fetch.Origin
		err    error
	)
	if addr.Name == "" {
		nav.skipAll(
			"You supplied a peer-id directly, so no name authority was consulted and none " +
				"vouched for this target. The peer-id IS the key (V7 §1.5), so the content " +
				"signature below still means everything it normally means — what is missing is " +
				"any statement that this peer is the one a human name refers to.")
	} else {
		if reg == nil {
			nav.fail("registry pin", "no registry pinned",
				"A name cannot resolve without a name authority, and a browser must not invent one.")
			return m.failed(errors.New("no registry pinned, so a name cannot be resolved"))
		}
		nav.ok("registry pin", fmt.Sprintf("%s at %s", reg.PeerID(), reg.Layout.Origin),
			"The registry's peer-id carries its public key (§6a.5), so this pin needs no key "+
				"distribution and the host serving the bytes is trusted for nothing.")

		if set == nil {
			// Resolve through the served pointer — §6a.4's floor.
			res, err = reg.Resolve(ctx, addr.Name)
			nav.record("name lookup", err, fmt.Sprintf("by-name pointer for %q (no enumeration loaded)", addr.Name),
				"The by-name pointer is served by the origin, so WHICH binding answers this name "+
					"is the host's choice. That is what the association step below exists to catch.")
		} else {
			res, err = reg.ResolveIn(ctx, *set, addr.Name)
			nav.record("name lookup", err, fmt.Sprintf("%q found in the signed key set of root %s",
				addr.Name, shortHash(set.Root.Data.RootHash.String())),
				"The binding hash came out of the walk, so the origin had no say in which binding "+
					"answers this name — a substitution is not merely detected here, it is impossible.")
		}
		if err != nil {
			return m.failedChain(nav, err, res)
		}

		nav.ok("binding signature", fmt.Sprintf("ed25519, signed by the registry (%s)", shortHash(res.BindingHash.String())),
			"The registry issued this binding. It does NOT prove what the binding was issued for — "+
				"that is the next step, and a verifier that stops here accepts a valid binding for "+
				"one name in answer to a query for another.")
		nav.ok("association", fmt.Sprintf("binding names %q, which is what was asked", res.Binding.Name),
			"§6a.4's MUST. The signed body carries `name`, so the pairing was always committed; "+
				"the defect this catches is a verifier discarding it.")
		// The detail carries the caveat rather than a bare tick: this
		// step is green in the sense that nothing revoked the binding,
		// and Bound() says exactly how little that is worth when the
		// walk did not cover the revocation prefix.
		nav.ok("revocation", res.Revocation.Bound(),
			"Presence proves revocation; absence proves nothing unless the walk covered it. "+
				"The bound on a revocation a hostile origin withholds is the binding's TTL.")
		nav.ok("binding freshness", fmt.Sprintf("issued %s, expires %s",
			res.IssuedAt.Format(time.RFC3339), res.ExpiresAt.Format(time.RFC3339)),
			"Checked against our own clock, from inside the signed body — the one check on this "+
				"hop a hostile byte-server cannot influence at all.")

		addr.PeerID = res.PeerID()
	}

	// ---------------------------------------------------------------
	// hop 2 — the target
	// ---------------------------------------------------------------
	var layout fetch.Layout
	if addr.Name == "" {
		if targetOrigin == "" {
			return m.failedChain(nav, errors.New(
				"a peer-id address needs an origin to fetch from: nothing in a peer-id says where "+
					"its bytes are served, and NETWORK §6.5.4 makes profile distribution "+
					"out-of-band in v1"), res)
		}
		layout, err = fetch.LoadLayout(ctx, targetOrigin, m.client)
		nav.record("transport", err, fmt.Sprintf("discovered profile at %s", targetOrigin),
			"The layout came from the origin's own well-known profile, so no URL here was derived "+
				"by convention (AP21).")
		if err != nil {
			return m.failedChain(nav, err, res)
		}
	} else {
		origin, err = reg.OriginFor(ctx, res, targetOrigin)
		detail := fmt.Sprintf("%d transport(s) on the binding", len(res.Transports()))
		if err == nil {
			detail = fmt.Sprintf("http-poll at %s (carried %s in the binding)",
				origin.Layout.Origin, origin.Ref.Kind)
			layout = origin.Layout
		}
		nav.record("transport", err, detail,
			"The registry told us how to reach this peer and we followed it verbatim. Nothing "+
				"here was derived from the peer-id — a consumer that derives a layout works "+
				"against exactly one publisher.")
		if err != nil {
			return m.failedChain(nav, err, res)
		}
	}

	consumer := fetch.NewConsumer(layout, m.client)
	root, err := consumer.VerifiedRoot(ctx)
	nav.record("target root", err, fmt.Sprintf("%s seq=%d prefix=%q",
		shortHash(root.Data.RootHash.String()), root.Data.Seq, root.Data.Prefix),
		"A DIFFERENT key from the registry's: this peer signs its own root, and the registry's "+
			"signature has no standing over its content. Verified as of published_at — never "+
			"fresh, because a quiet publisher and a withholding origin look identical from here.")
	if err != nil {
		return m.failedChain(nav, err, res)
	}
	nav.freshness = fmt.Sprintf("verified as of %s (the target's published_at); "+
		"a withholding origin and a quiet publisher are indistinguishable from here",
		freshnessOf(root.Data.PublishedAt))

	walk, err := consumer.Walk(ctx, root.Data.RootHash)
	nav.record("target walk", err, fmt.Sprintf("%d CHAMP nodes, %d committed keys",
		walk.Nodes(), len(walk.Bindings)),
		"The only step in this chain a withholding origin cannot pass. Every other one is "+
			"satisfiable by an origin serving a correctly-signed root that commits to nothing.")
	if err != nil {
		return m.failedChain(nav, err, res)
	}

	resolver := NewRemoteSiteResolver(ctx, consumer, root, walk)
	sites := resolver.Sites()
	defaulted := false
	if addr.SiteID == "" && len(sites) > 0 {
		addr.SiteID, defaulted = sites[0], true
	}

	site := NewSiteModel(resolver, Location{PeerID: layout.PeerID, SiteID: addr.SiteID, Page: addr.Page})
	content := site.Render()
	if content.Error != "" {
		nav.fail("page", content.Error,
			"The bytes were committed and verified; this is the SITE convention failing to find "+
				"what it expects at the paths it expects.")
	} else {
		nav.ok("page", fmt.Sprintf("%s/%s (%d bytes of %s)",
			addr.SiteID, content.CurrentPage, len(content.BodyMarkdown), content.BodyFormat),
			"These exact bytes hash to what the signed root committed for this path. That is the "+
				"whole of what a green chain claims.")
	}

	m.mu.Lock()
	m.site = site
	m.out.hostName, m.out.hostPeer = addr.Name, layout.PeerID
	m.out.Host = addr.Host()
	if m.out.Host == "" {
		m.out.Host = layout.PeerID
	}
	m.out.Site = addr.SiteID
	m.out.Page = content.CurrentPage
	m.out.Address = Address{Name: addr.Name, PeerID: layout.PeerID,
		SiteID: addr.SiteID, Page: content.CurrentPage}.String()
	m.out.Sites = sites
	m.out.SiteDefaulted = defaulted && len(sites) > 1
	m.out.Content = content
	m.mu.Unlock()
	return nil
}

func (m *BrowseModel) failed(err error) error {
	m.mu.Lock()
	m.out.Err = err.Error()
	m.mu.Unlock()
	return err
}

// failedChain records a navigation failure with the resolution's own
// diagnostic attached, which is §6a.4's "surface which require failed to
// your own operator".
func (m *BrowseModel) failedChain(nav *chain, err error, res fetch.NameResolution) error {
	msg := err.Error()
	if errors.Is(err, fetch.ErrNameAssociation) && res.Binding.Name != "" {
		msg = fmt.Sprintf("%s — the origin served a binding the registry legitimately signed for "+
			"%q, in answer to a different question", msg, res.Binding.Name)
	}
	m.mu.Lock()
	m.out.Err = msg
	m.out.Content = SiteRenderOutput{}
	m.out.Sites = nil
	m.mu.Unlock()
	return err
}

// OnChange registers a render-invalidation listener; the returned func
// unsubscribes.
func (m *BrowseModel) OnChange(h func()) func() {
	m.mu.Lock()
	m.listeners = append(m.listeners, h)
	i := len(m.listeners) - 1
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if i < len(m.listeners) {
			m.listeners[i] = nil
		}
		m.mu.Unlock()
	}
}

func (m *BrowseModel) fire() {
	m.mu.Lock()
	ls := append([]func(){}, m.listeners...)
	m.mu.Unlock()
	for _, h := range ls {
		if h != nil {
			h()
		}
	}
}

// Render returns the current output. Cheap; safe from a UI thread.
func (m *BrowseModel) Render() BrowseOutput {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.out
	out.Steps = append([]ConsumeStep{}, m.out.Steps...)
	out.Names = append([]RegistryRow{}, m.out.Names...)
	out.Sites = append([]string{}, m.out.Sites...)
	return out
}

// -------------------------------------------------------------------
// the chain
// -------------------------------------------------------------------

// chain accumulates the trust steps for one navigation.
//
// It exists so that "a step that could not be established is shown
// failing, never omitted" is a property of the type rather than a
// discipline every call site has to remember: [chain.record] takes the
// error and decides, and there is no way to add a step without one.
type chain struct {
	steps     []ConsumeStep
	freshness string
}

func newChain() *chain { return &chain{} }

func (c *chain) ok(name, detail, proves string) {
	c.steps = append(c.steps, ConsumeStep{Name: name, Status: StepOK, Detail: detail, Proves: proves})
}

func (c *chain) fail(name, reason, proves string) {
	c.steps = append(c.steps, ConsumeStep{
		Name: name, Status: StepFailed, Detail: reason, Proves: proves, Err: reason,
	})
}

func (c *chain) record(name string, err error, detail, proves string) {
	st := ConsumeStep{Name: name, Status: StepOK, Detail: detail, Proves: proves}
	if err != nil {
		st.Status, st.Err = StepFailed, err.Error()
	}
	c.steps = append(c.steps, st)
}

// skipAll marks the whole naming hop as not-run, with the reason.
//
// Skipped is not a quiet pass. Every one of these rows is drawn, because
// "no name authority was consulted" is a fact about what the user is
// looking at and a chain that simply started at the target would read as
// a shorter, cleaner, equally-green chain.
func (c *chain) skipAll(why string) {
	for _, n := range []string{"registry pin", "name lookup", "binding signature", "association",
		"revocation", "binding freshness"} {
		c.steps = append(c.steps, ConsumeStep{Name: n, Status: StepSkipped, Detail: "not run", Proves: why})
	}
}

func freshnessOf(publishedAt uint64) string {
	if publishedAt == 0 {
		return "no published_at"
	}
	return time.UnixMilli(int64(publishedAt)).UTC().Format(time.RFC3339)
}

func shortHash(s string) string {
	if i := strings.Index(s, ":"); i >= 0 && len(s) > i+13 {
		return s[:i+13] + "…"
	}
	return s
}
