package workbench

import (
	"sort"
	"strings"

	"go.entitychurch.org/entity-core-go/core/ecf"
)

// Site read-projection resolver seam — format ⊥ transport.
//
// A ContentResolver turns a Location into a ResolvedPage. The model
// calls it on Navigate; the renderer only ever reads the resolved
// output. This is the seam that lets transports swap underneath
// without touching the renderer.
//
// v1 ships LocalTreeResolver only. Cross-peer reads land in the local
// tree via the SDK's subscription extension at a layer ABOVE this
// model — the model never sees HTTP, never sees a goroutine, never
// owns a fetch. Future cross-peer / HTTP-poll resolvers slot in
// behind the same ContentResolver interface, returning Ready=false
// (pending) and signaling the model when the fetch lands.
//
// Mirrors egui-rust src/content_site/{resolver,location,discovery}.rs.

// Location names a page within a site, possibly on a remote peer.
// Empty PeerID means "the resolver's bound peer." Empty Page means
// "the manifest's declared root page (params.root)."
type Location struct {
	PeerID string
	SiteID string
	Page   string
}

// SiteRoot returns a Location addressing a site's root page on the
// resolver's bound peer.
func SiteRoot(siteID string) Location {
	return Location{SiteID: siteID}
}

// ResolveError classifies why a resolve failed. Zero value = no error.
type ResolveError int

const (
	// ResolveOK is the zero value (Ready && Page != nil).
	ResolveOK ResolveError = 0
	// ManifestMissing — no manifest entity at the site's manifest path.
	ManifestMissing ResolveError = iota
	// PageMissing — no page entity at the requested (or root) page
	// path, AND the path is not a section with descendant pages.
	PageMissing
	// DecodeError — entity bytes failed to decode.
	DecodeError
)

func (e ResolveError) String() string {
	switch e {
	case ResolveOK:
		return ""
	case ManifestMissing:
		return "manifest missing"
	case PageMissing:
		return "page missing"
	case DecodeError:
		return "decode error"
	default:
		return "unknown error"
	}
}

// ResolvedPage is a fully-resolved page ready to render: the page
// itself plus the site manifest (for nav/chrome) and the concrete
// location reached (root resolved to a page slug).
type ResolvedPage struct {
	Location Location
	Manifest SiteManifest
	Page     SitePage
}

// ResolveOutcome is the result of beginning a resolve. Ready=true
// means a synchronous answer; Ready=false means an async transport
// is fetching and the model should re-resolve when it signals.
type ResolveOutcome struct {
	Ready bool
	Page  *ResolvedPage
	Err   ResolveError
}

// readyOK builds a Ready Outcome carrying a successfully resolved page.
func readyOK(p ResolvedPage) ResolveOutcome {
	return ResolveOutcome{Ready: true, Page: &p}
}

// readyErr builds a Ready Outcome carrying a resolve error.
func readyErr(e ResolveError) ResolveOutcome {
	return ResolveOutcome{Ready: true, Err: e}
}

// ChildEntry is one immediate child under a pages prefix — body-free
// (we know the name and whether it's a leaf page and/or a section
// without loading any entity). A name can be both is_page and
// is_section simultaneously (a section with an explicit index page).
type ChildEntry struct {
	Name      string
	IsPage    bool
	IsSection bool
}

// ContentResolver is the transport seam. Implementors fetch a page
// for a location and list immediate children under a pages prefix.
type ContentResolver interface {
	// ResolvePage begins resolving loc. See ResolveOutcome.
	ResolvePage(loc Location) ResolveOutcome
	// ListChildren returns the immediate children of loc's site under
	// the page-prefix under ("" = page root, "guide/" = the Guide
	// section). Body-free. Empty for a not-yet-local peer/site.
	ListChildren(loc Location, under string) []ChildEntry
}

// LocalTreeResolver resolves pages from the bound peer's tree via
// the entitysdk Store. Synchronous L0 reads — no I/O, no HTTP, no
// goroutine.
type LocalTreeResolver struct {
	store     *Store
	boundPeer string
}

// NewLocalTreeResolver builds a resolver bound to a peer's store.
// boundPeer is substituted for an empty Location.PeerID.
func NewLocalTreeResolver(store *Store, boundPeer string) *LocalTreeResolver {
	if store == nil {
		panic("workbench: NewLocalTreeResolver requires non-nil Store")
	}
	if boundPeer == "" {
		panic("workbench: NewLocalTreeResolver requires non-empty boundPeer")
	}
	return &LocalTreeResolver{store: store, boundPeer: boundPeer}
}

// BoundPeer returns the resolver's bound peer id.
func (r *LocalTreeResolver) BoundPeer() string { return r.boundPeer }

// ResolvePage fetches the manifest + page entity from the tree.
// Empty Location.Page falls back to manifest.Root (default "index").
// Missing page entities at a section path synthesize a section-index
// page (so a breadcrumb to a section is a real destination).
func (r *LocalTreeResolver) ResolvePage(loc Location) ResolveOutcome {
	pid := loc.PeerID
	if pid == "" {
		pid = r.boundPeer
	}

	manifestEnt, ok := r.store.Get(ManifestPath(pid, loc.SiteID))
	if !ok {
		return readyErr(ManifestMissing)
	}
	var manifest SiteManifest
	if err := ecf.Decode(manifestEnt.Data, &manifest); err != nil {
		return readyErr(DecodeError)
	}

	// Empty page → the manifest's declared root page.
	pageSlug := loc.Page
	if pageSlug == "" {
		pageSlug = manifest.Root()
	}

	pageEnt, ok := r.store.Get(PagePath(pid, loc.SiteID, pageSlug))
	if !ok {
		// No page entity. If the slug is a SECTION (has child pages),
		// synthesize a section-index listing them. Otherwise, page
		// missing.
		children := r.ListChildren(Location{PeerID: pid, SiteID: loc.SiteID}, pageSlug+"/")
		if len(children) == 0 {
			return readyErr(PageMissing)
		}
		return readyOK(ResolvedPage{
			Location: Location{PeerID: pid, SiteID: loc.SiteID, Page: pageSlug},
			Manifest: manifest,
			Page:     sectionIndexPage(pageSlug, children),
		})
	}
	var page SitePage
	if err := ecf.Decode(pageEnt.Data, &page); err != nil {
		return readyErr(DecodeError)
	}
	// Default format if absent — wire-tolerant.
	if page.Format == "" {
		page.Format = DefaultPageFormat
	}
	return readyOK(ResolvedPage{
		Location: Location{PeerID: pid, SiteID: loc.SiteID, Page: pageSlug},
		Manifest: manifest,
		Page:     page,
	})
}

// ListChildren lists the immediate children under loc.SiteID's pages
// prefix joined with under. Body-free — paths only, no entity bodies
// loaded. Results are sorted byte-wise (lex on slug); the SITE v0.4.2
// §4.2 ordering floor.
func (r *LocalTreeResolver) ListChildren(loc Location, under string) []ChildEntry {
	pid := loc.PeerID
	if pid == "" {
		pid = r.boundPeer
	}
	prefix := PagesPrefix(pid, loc.SiteID) + under

	// One ChildEntry per immediate name; a name can be both is_page
	// (an entry exactly at prefix+name) and is_section (a deeper
	// segment exists under prefix+name/).
	byName := make(map[string]*ChildEntry)
	for _, e := range r.store.List(prefix) {
		rest := strings.TrimPrefix(e.Path, prefix)
		rest = strings.TrimLeft(rest, "/")
		if rest == "" {
			continue
		}
		name, hasMore := splitFirstSeg(rest)
		if name == "" {
			continue
		}
		child, ok := byName[name]
		if !ok {
			child = &ChildEntry{Name: name}
			byName[name] = child
		}
		if hasMore {
			child.IsSection = true
		} else {
			child.IsPage = true
		}
	}

	out := make([]ChildEntry, 0, len(byName))
	for _, c := range byName {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// splitFirstSeg returns (firstSegment, hasMore). "guide" → ("guide",
// false); "guide/intro" → ("guide", true).
func splitFirstSeg(s string) (string, bool) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], i < len(s)-1
	}
	return s, false
}

// sectionIndexPage builds a synthetic "section index" page: a heading
// + markdown list of links to the section's immediate children. The
// renderer turns it into HTML and rewrites the links like any page.
// Mirrors egui's discovery.rs synthesis.
func sectionIndexPage(sectionSlug string, children []ChildEntry) SitePage {
	title := humanize(lastPathSeg(sectionSlug))
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(title)
	b.WriteString("\n\n")
	for _, c := range children {
		b.WriteString("- [")
		b.WriteString(humanize(c.Name))
		b.WriteString("](./")
		b.WriteString(sectionSlug)
		b.WriteString("/")
		b.WriteString(c.Name)
		b.WriteString(")\n")
	}
	return NewMarkdownPage(title, b.String())
}

// humanize title-cases a path segment for display:
// `getting-started` → `Getting started`.
func humanize(seg string) string {
	if seg == "" {
		return ""
	}
	spaced := strings.NewReplacer("-", " ", "_", " ").Replace(seg)
	return strings.ToUpper(spaced[:1]) + spaced[1:]
}

// lastPathSeg returns the substring after the final "/", or the
// whole string if there's no slash.
func lastPathSeg(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}
