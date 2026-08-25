package workbench

import (
	"sort"
	"sync"
)

var _ Model[PeerLivenessOutput] = (*PeerLivenessModel)(nil)

// PeerLivenessModel is the renderer-neutral model for "which peers are
// we in touch with, and what happened to the ones we are not."
//
// It is the replacement for reading AppPeer.ConnectedPeers() in a
// renderer. That call reports the local connection POOL — our own
// plumbing — and cannot express `suspect`, cannot say why a peer went
// away, and disagrees with the tree whenever a connection is evicted
// without a demotion (the dispatch-fallback and relay terminal-hop
// paths evict without demoting, deliberately). This model reads
// `system/peer/status`, which is the lifecycle signal the whole cohort
// shares: the same view a remote consumer, a continuation, or the
// reconnect graph sees.
//
// Cost model: O(N) seed at construction over the status namespace
// (whose size is "peers we have ever transitioned", not tree size),
// then O(1) per transition. Render is O(N log N) only when a
// transition has actually arrived since the last one — the status
// entity is transition-written, so that is genuinely rare, unlike a
// path-count model that churns on every write.
//
// What it deliberately does NOT do: infer staleness from LastSeen. The
// status entity is not a heartbeat and a successful keepalive writes
// nothing, so "last seen 4 minutes ago" is a statement about the last
// TRANSITION, not about silence. A renderer that ages rows off that
// field invents a liveness contract the protocol does not offer.
type PeerLivenessModel struct {
	store *Store

	cancel func()

	mu      sync.Mutex
	byPeer  map[string]PeerLiveness
	sorted  []PeerLiveness
	dirty   bool
	seedErr error
}

// PeerLivenessOutput is the renderer-neutral output: one row per peer
// we have ever recorded a transition for, sorted by peer-id, plus the
// counts a status bar wants without re-walking the rows.
type PeerLivenessOutput struct {
	Peers        []PeerLiveness
	Connected    int
	Suspect      int
	Disconnected int
	// SeedError is non-nil when the initial read failed. Surfaced
	// rather than swallowed: an empty list from a failed seed and an
	// empty list from a peer that has talked to nobody look identical
	// in a UI, and only one of them is fine.
	SeedError error
}

// NewPeerLivenessModel builds the model and subscribes to lifecycle
// transitions. Prefix-subscribed, never scan-and-filter.
func NewPeerLivenessModel(st *Store) *PeerLivenessModel {
	m := &PeerLivenessModel{
		store:  st,
		byPeer: make(map[string]PeerLiveness),
	}
	if st == nil {
		return m
	}
	// Seed first, then subscribe. The reverse order drops any
	// transition that lands between the two.
	seed, err := st.PeerLivenessAll()
	if err != nil {
		m.seedErr = err
	}
	for _, l := range seed {
		m.byPeer[l.PeerID] = l
	}
	m.dirty = true
	m.cancel = st.OnPeerLivenessChange(m.onTransition)
	return m
}

// Close cancels the subscription. Idempotent.
func (m *PeerLivenessModel) Close() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// onTransition records the newest state for a peer. Last write wins:
// the entity is transition-written, so each event IS the current
// state, and there is no merge to do.
func (m *PeerLivenessModel) onTransition(l PeerLiveness) {
	if l.PeerID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byPeer[l.PeerID] = l
	m.dirty = true
}

// Render returns the current liveness view.
func (m *PeerLivenessModel) Render() PeerLivenessOutput {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.dirty {
		m.dirty = false
		m.sorted = m.sorted[:0]
		if cap(m.sorted) < len(m.byPeer) {
			m.sorted = make([]PeerLiveness, 0, len(m.byPeer))
		}
		for _, l := range m.byPeer {
			m.sorted = append(m.sorted, l)
		}
		sort.Slice(m.sorted, func(i, j int) bool { return m.sorted[i].PeerID < m.sorted[j].PeerID })
	}

	out := PeerLivenessOutput{
		Peers:     make([]PeerLiveness, len(m.sorted)),
		SeedError: m.seedErr,
	}
	copy(out.Peers, m.sorted)
	for _, l := range m.sorted {
		switch l.Status {
		case PeerStatusConnected:
			out.Connected++
		case PeerStatusSuspect:
			out.Suspect++
		case PeerStatusDisconnected:
			out.Disconnected++
		}
	}
	return out
}
