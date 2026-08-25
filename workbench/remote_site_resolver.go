package workbench

import (
	"context"
	"sort"
	"strings"
	"sync"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/hash"

	"entity-workbench-go/fetch"
)

var _ ContentResolver = (*RemoteSiteResolver)(nil)

// remote_site_resolver.go — the join that makes a published origin
// readable by the surface that already knows how to draw a site.
//
// `SiteModel` has never cared where its bytes come from: `ContentResolver`
// is the transport seam, and until now the only implementation was
// [LocalTreeResolver], reading a peer's own tree. This is the second
// one, over a **verified static origin** — so the same model, the same
// nav, the same breadcrumbs, driven by bytes fetched from a CDN and
// checked against a signature instead of read out of our own store.
//
// **What is different, and it is not a detail: the site's structure
// comes from the signed key set.**
//
// A local resolver lists children by asking the store what paths exist,
// and the store is ours, so the answer is trustworthy for free. A remote
// one cannot ask the origin — an origin can answer that question any way
// it likes, and a `.list` artifact signs nothing (EXTENSION-REGISTRY
// §6a.3a says this of registries; EXTENSION-TREE §3.1/§3.5 make it true
// of any published trie). So this resolver never asks. It is constructed
// from a completed [fetch.WalkResult] — the key set the publisher's own
// signature commits to — and every path question is answered out of
// that.
//
// The consequence is worth stating plainly, because it is the property
// the whole session is about: **a page that is not in this site's
// navigation but IS in the signed key set will be listed here, and a
// page the origin advertises that the signature does not cover will
// not.** The manifest's `nav` is the publisher's curation; the key set
// is the publisher's commitment. This resolver draws the first and
// answers structural questions from the second.
//
// Body fetches are lazy and cached: the walk gives us every key and
// every content hash up front, so resolving a page is one content
// fetch, hash-verified by [fetch.Consumer.Blob].
type RemoteSiteResolver struct {
	consumer *fetch.Consumer
	peerID   string
	prefix   string

	// keys maps a peer-relative tree path → the content hash the SIGNED
	// ROOT committed to for it. Built once from the walk; never
	// refreshed from the origin, because a mid-session refresh from an
	// unverified source is exactly the substitution the walk prevents.
	keys map[string]hash.Hash
	// sorted is `keys` in byte order — SITE v0.4.2 §4.2's ordering floor.
	sorted []string

	mu    sync.Mutex
	cache map[hash.Hash][]byte
	// ctx is the fetch context. ContentResolver is a synchronous
	// interface (LocalTreeResolver does L0 reads), so the network call
	// happens inside ResolvePage; callers drive this off a worker, never
	// a UI thread — the same rule ConsumeModel states.
	ctx context.Context
}

// NewRemoteSiteResolver binds a resolver to one verified origin.
//
// `walk` MUST be the result of a completed [fetch.Consumer.Walk] against
// `root` — a partial or best-effort traversal here would silently
// present a withholding origin as a smaller site, which is the failure
// AP29 is named for.
func NewRemoteSiteResolver(ctx context.Context, c *fetch.Consumer, root fetch.VerifiedRoot, walk fetch.WalkResult) *RemoteSiteResolver {
	r := &RemoteSiteResolver{
		consumer: c,
		peerID:   c.Layout.PeerID,
		prefix:   root.Data.Prefix,
		keys:     make(map[string]hash.Hash, len(walk.Bindings)),
		cache:    map[hash.Hash][]byte{},
		ctx:      ctx,
	}
	for _, b := range walk.Bindings {
		abs := fetch.AbsolutePath(root.Data.Prefix, r.peerID, b.Key)
		rel := strings.TrimPrefix(strings.TrimPrefix(abs, "/"+r.peerID), "/")
		r.keys[rel] = b.Hash
		r.sorted = append(r.sorted, rel)
	}
	sort.Strings(r.sorted)
	return r
}

// BoundPeer is the publisher whose signed root backs this resolver.
func (r *RemoteSiteResolver) BoundPeer() string { return r.peerID }

// Sites lists every site the signed root commits to — the authoritative
// answer to "what is published here", as opposed to whatever
// `sites.list` says.
func (r *RemoteSiteResolver) Sites() []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range r.sorted {
		rest, ok := strings.CutPrefix(k, SitesSubpath+"/")
		if !ok {
			continue
		}
		id, _, found := strings.Cut(rest, "/")
		if !found || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// blob fetches and caches one committed body.
func (r *RemoteSiteResolver) blob(h hash.Hash) ([]byte, bool) {
	r.mu.Lock()
	if b, ok := r.cache[h]; ok {
		r.mu.Unlock()
		return b, true
	}
	r.mu.Unlock()

	ent, err := r.consumer.Blob(r.ctx, h)
	if err != nil {
		return nil, false
	}
	r.mu.Lock()
	r.cache[h] = ent.Data
	r.mu.Unlock()
	return ent.Data, true
}

// at resolves one peer-relative tree path out of the committed key set.
func (r *RemoteSiteResolver) at(rel string) ([]byte, bool) {
	h, ok := r.keys[rel]
	if !ok {
		return nil, false
	}
	return r.blob(h)
}

// ResolvePage implements [ContentResolver].
//
// The shape mirrors LocalTreeResolver deliberately, including the
// section-index synthesis: a slug with no page entity but with
// descendants renders as a listing rather than as "page missing". Two
// resolvers behind one model have to agree on that or the same site
// reads differently depending on where it was fetched from.
func (r *RemoteSiteResolver) ResolvePage(loc Location) ResolveOutcome {
	pid := loc.PeerID
	if pid == "" {
		pid = r.peerID
	}
	if pid != r.peerID {
		// A cross-peer link inside a site. This resolver is bound to one
		// origin and will not silently answer for another publisher —
		// resolving it would mean fetching from a peer whose root we
		// have not verified.
		return readyErr(PageMissing)
	}

	raw, ok := r.at(relPath(ManifestPath(pid, loc.SiteID), pid))
	if !ok {
		return readyErr(ManifestMissing)
	}
	var manifest SiteManifest
	if err := ecf.Decode(raw, &manifest); err != nil {
		return readyErr(DecodeError)
	}

	pageSlug := loc.Page
	if pageSlug == "" {
		pageSlug = manifest.Root()
	}

	body, ok := r.at(relPath(PagePath(pid, loc.SiteID, pageSlug), pid))
	if !ok {
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
	if err := ecf.Decode(body, &page); err != nil {
		return readyErr(DecodeError)
	}
	if page.Format == "" {
		page.Format = DefaultPageFormat
	}
	return readyOK(ResolvedPage{
		Location: Location{PeerID: pid, SiteID: loc.SiteID, Page: pageSlug},
		Manifest: manifest,
		Page:     page,
	})
}

// ListChildren implements [ContentResolver] out of the committed key
// set — no fetch, no listing artifact, nothing the origin gets a say in.
func (r *RemoteSiteResolver) ListChildren(loc Location, under string) []ChildEntry {
	pid := loc.PeerID
	if pid == "" {
		pid = r.peerID
	}
	if pid != r.peerID {
		return nil
	}
	prefix := relPath(PagesPrefix(pid, loc.SiteID), pid) + under

	byName := map[string]*ChildEntry{}
	var order []string
	for _, k := range r.sorted {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok || rest == "" {
			continue
		}
		name, tail, isSection := strings.Cut(rest, "/")
		if name == "" {
			continue
		}
		e, seen := byName[name]
		if !seen {
			e = &ChildEntry{Name: name}
			byName[name] = e
			order = append(order, name)
		}
		if isSection && tail != "" {
			e.IsSection = true
		} else {
			e.IsPage = true
		}
	}
	sort.Strings(order)
	out := make([]ChildEntry, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out
}

// relPath strips the leading `/{peer}/` from an absolute tree path.
func relPath(abs, peerID string) string {
	return strings.TrimPrefix(strings.TrimPrefix(abs, "/"+peerID), "/")
}
