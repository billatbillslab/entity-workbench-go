package workbench

import (
	"strings"
	"sync"

	"go.entitychurch.org/entity-core-go/core/store"
)

var _ Model[TreeBrowserOutput] = (*TreeBrowserModel)(nil)

// TreeBrowserRow is a flat, renderer-neutral row in the tree
// browser output. It carries everything a renderer needs to draw
// one line — including descendant leaf count for the collapsed
// "(N)" hint — without exposing the underlying *TreeNode graph.
type TreeBrowserRow struct {
	Path        string
	Segment     string
	Depth       int
	HasChildren bool
	Expanded    bool
	HasEntry    bool
	LeafCount   int
}

// TreeBrowserOutput is the renderer-neutral output of the tree
// browser model.
type TreeBrowserOutput struct {
	Rows       []TreeBrowserRow
	SearchText string
	MatchCount int
}

// TreeBrowserModel is the business logic for the tree browser panel.
//
// Cost model: the model subscribes once via Store.OnPrefixChange("")
// (all paths under this peer). The seed phase pays O(N) at
// construction to build the initial tree. Per-event cost is
// O(depth) for the InsertOrUpdate plus O(siblings-at-leaf) for the
// depth-bookkeeping walk that calls parentOf (parentOf is a recursive
// tree scan with early-return on match — for a leaf with K siblings
// under its parent prefix, finding the parent walks ~K children before
// matching). Refresh is a re-flatten of *visible* rows (O(visible),
// not O(N)). See the production-readiness review for the
// empirical scaling; the O(siblings) cost is the documented limit.
//
// Heartbeats and other background writes update the internal tree but
// do not produce extra render work unless their path is under an
// expanded node.
type TreeBrowserModel struct {
	peerCtx *PeerContext

	Root        *TreeNode
	VisibleRows []VisibleRow
	SearchText  string
	MatchCount  int

	// Subscription state.
	cancel func()

	// All paths the model knows about, by qualified path. Maintained
	// incrementally by the event handler. Used for search filter
	// (filterEntries) which scans the known set.
	mu    sync.Mutex
	known map[string]store.LocationEntry
	dirty bool // tree needs RebuildVisible on next Refresh

	lastSyncedPath string

	// Selection slot binding (set via BindSelection). selCancel
	// cancels the OnSelectionChange subscription; extSelPath caches
	// the last path delivered by it so SyncFromSelection stays a
	// field read, not a per-frame store Get (matches the entity-detail
	// panel's Stage-5 pattern — draw() runs every frame in canvas).
	selState   *WorkspaceState
	selScreen  int
	selCancel  func()
	extSelPath string
}

// PeerCtx returns the underlying PeerContext.
func (m *TreeBrowserModel) PeerCtx() *PeerContext { return m.peerCtx }

// NewTreeBrowserModel creates a tree browser model and starts its
// subscription. The first Refresh flattens the current tree (seed
// already populated by the subscription goroutine — possibly still in
// progress; tests poll until ready).
func NewTreeBrowserModel(peerCtx *PeerContext) *TreeBrowserModel {
	m := &TreeBrowserModel{
		peerCtx: peerCtx,
		Root:    &TreeNode{Segment: "", FullPath: "", Expanded: true, Depth: -1},
		known:   make(map[string]store.LocationEntry),
		dirty:   true,
	}
	if peerCtx != nil && peerCtx.Store() != nil {
		m.cancel = peerCtx.Store().OnPrefixChange("", m.onEvent)
	}
	return m
}

// SelectedPath returns the currently-selected path as observed from
// the screen selection slot (cached; no store read on the hot path).
func (m *TreeBrowserModel) SelectedPath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.extSelPath
}

// Close cancels the subscriptions. Idempotent.
func (m *TreeBrowserModel) Close() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.selCancel != nil {
		m.selCancel()
		m.selCancel = nil
	}
}

// BindSelection wires this model to a screen's selection slot — the
// authoritative selection value (in the entity tree) replaces the old
// in-memory SelectionState. Reads (SyncFromSelection) observe a cached
// path kept current by an OnSelectionChange subscription; writes
// (PublishSelection) go straight to the slot. Nil state (unit tests /
// no presentation context) is a no-op for publish and an empty path
// for sync. Idempotent: rebinding cancels the prior subscription.
func (m *TreeBrowserModel) BindSelection(state *WorkspaceState, screenIdx int) {
	m.mu.Lock()
	if m.selCancel != nil {
		m.selCancel()
		m.selCancel = nil
	}
	m.selState = state
	m.selScreen = screenIdx
	if state != nil {
		if sel, ok := state.ReadSelection(screenIdx); ok {
			m.extSelPath = sel.Path
		}
	}
	m.mu.Unlock()
	m.selCancel = bindSelectionWatch(state, screenIdx, func(sel Selection) {
		m.mu.Lock()
		m.extSelPath = sel.Path
		m.mu.Unlock()
	})
}

// onEvent applies one tree mutation to the local model. O(depth) per
// event — no full-tree scan. Runs on the SDK goroutine.
func (m *TreeBrowserModel) onEvent(ev ChangeEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch ev.EventType {
	case ChangePut:
		entry := store.LocationEntry{Path: ev.Path, Hash: ev.NewHash}
		m.known[ev.Path] = entry
		// On the very first put we may be growing the initial tree.
		// Expand the immediate root children up to depth 1 the first
		// time their parents become non-empty, matching BuildTree's
		// ExpandToDepth(1) behavior so the panel looks the same as
		// before.
		node := InsertOrUpdate(m.Root, entry)
		// Walk up and ensure root-level node is expanded (depth 0).
		for n := node; n != nil; n = parentOf(m.Root, n) {
			if n.Depth == 0 {
				n.Expanded = true
				break
			}
		}
	case ChangeRemove:
		delete(m.known, ev.Path)
		Remove(m.Root, ev.Path)
	}
	m.dirty = true
}

// parentOf returns the parent of node within root, or nil if node is
// root or not present. Recursive tree scan with early-return on
// match — O(siblings under target's parent prefix) in the typical
// case, O(N) worst case. Used only for depth-bookkeeping on insert.
// At heartbeat rates (~0.5 puts/sec) the absolute cost is negligible
// up to ~500K entities (see the production-readiness review);
// if higher write rates or larger trees become routine, replace with
// parent pointers on TreeNode.
func parentOf(root, target *TreeNode) *TreeNode {
	for _, child := range root.Children {
		if child == target {
			return root
		}
		if p := parentOf(child, target); p != nil {
			return p
		}
	}
	return nil
}

// Refresh re-flattens visible rows from the internal tree IF the
// subscription has signaled a change since the last refresh. Returns
// true when state changed (caller should re-render); false means
// no-op and the caller can skip its own re-render.
func (m *TreeBrowserModel) Refresh() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dirty {
		return false
	}
	m.dirty = false
	m.rebuildVisibleLocked()
	return true
}

// RebuildVisible is the public form — same as Refresh but always runs.
// Kept for callers that want to force a re-flatten (e.g. after toggling
// expand state from outside).
func (m *TreeBrowserModel) RebuildVisible() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildVisibleLocked()
}

func (m *TreeBrowserModel) rebuildVisibleLocked() {
	if m.SearchText == "" {
		m.VisibleRows = FlattenVisible(m.Root)
		m.MatchCount = len(m.known)
		return
	}
	m.VisibleRows = m.filterEntriesLocked()
	m.MatchCount = len(m.VisibleRows)
}

// SetSearch updates the search text and rebuilds visible rows.
func (m *TreeBrowserModel) SetSearch(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SearchText = text
	m.rebuildVisibleLocked()
}

// ClearSearch clears the search and rebuilds from the tree.
func (m *TreeBrowserModel) ClearSearch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SearchText = ""
	m.rebuildVisibleLocked()
}

// Render produces the renderer-neutral tree browser output.
func (m *TreeBrowserModel) Render() TreeBrowserOutput {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := make([]TreeBrowserRow, len(m.VisibleRows))
	for i, vr := range m.VisibleRows {
		hasChildren := len(vr.Node.Children) > 0
		row := TreeBrowserRow{
			Path:        vr.Node.FullPath,
			Segment:     vr.Node.Segment,
			Depth:       vr.Depth,
			HasChildren: hasChildren,
			Expanded:    vr.Node.Expanded,
			HasEntry:    vr.Node.HasEntry,
		}
		if hasChildren && !vr.Node.Expanded {
			row.LeafCount = CountLeaves(vr.Node)
		}
		rows[i] = row
	}
	return TreeBrowserOutput{
		Rows:       rows,
		SearchText: m.SearchText,
		MatchCount: m.MatchCount,
	}
}

// CopyExpansionStateFrom copies the expand state from src into m.
// Both models must already have been populated. Used when cloning a
// tree-browser window so the new instance inherits the user's expand
// state without renderers reaching into *TreeNode internals.
func (m *TreeBrowserModel) CopyExpansionStateFrom(src *TreeBrowserModel) {
	if src == nil {
		return
	}
	src.mu.Lock()
	expanded := CollectExpanded(src.Root)
	src.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Root == nil {
		return
	}
	RestoreExpanded(m.Root, expanded)
	m.rebuildVisibleLocked()
}

// SyncFromSelection checks if the selection changed externally and
// returns the index of the matching row, or -1 if not found.
// Clears search and expands ancestors if needed.
func (m *TreeBrowserModel) SyncFromSelection() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	selPath := m.extSelPath
	if selPath == "" || selPath == m.lastSyncedPath {
		return -1
	}
	m.lastSyncedPath = selPath

	if m.SearchText != "" {
		m.SearchText = ""
	}
	if m.Root != nil {
		ExpandAncestors(m.Root, selPath)
	}
	m.rebuildVisibleLocked()

	for i, row := range m.VisibleRows {
		if row.Node.FullPath == selPath {
			return i
		}
	}
	return -1
}

// PublishSelection writes the given row's path to the screen selection
// slot (the value-of-truth in the tree).
func (m *TreeBrowserModel) PublishSelection(index int) {
	m.mu.Lock()
	if index < 0 || index >= len(m.VisibleRows) {
		m.mu.Unlock()
		return
	}
	path := m.VisibleRows[index].Node.FullPath
	m.mu.Unlock()
	m.PublishSelectionPath(path)
}

// PublishSelectionPath writes an explicit path to the screen selection
// slot. Used by renderers that already hold the path (e.g. console's
// tview node callbacks) rather than a visible-row index.
func (m *TreeBrowserModel) PublishSelectionPath(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.selState != nil {
		m.selState.SaveSelection(m.selScreen, Selection{Path: path, Type: "entity"})
	}
	m.lastSyncedPath = path
}

// ToggleExpand toggles expansion of the node at the given row index.
func (m *TreeBrowserModel) ToggleExpand(index int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.VisibleRows) {
		return
	}
	node := m.VisibleRows[index].Node
	if len(node.Children) > 0 {
		node.Expanded = !node.Expanded
		m.rebuildVisibleLocked()
	}
}

// Expand expands the node at the given index (no-op if already expanded or no children).
func (m *TreeBrowserModel) Expand(index int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.VisibleRows) {
		return
	}
	node := m.VisibleRows[index].Node
	if len(node.Children) > 0 && !node.Expanded {
		node.Expanded = true
		m.rebuildVisibleLocked()
	}
}

// Collapse collapses the node at the given index.
func (m *TreeBrowserModel) Collapse(index int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.VisibleRows) {
		return
	}
	node := m.VisibleRows[index].Node
	if node.Expanded && len(node.Children) > 0 {
		node.Expanded = false
		m.rebuildVisibleLocked()
	}
}

// --- Search filtering ---

// filterEntriesLocked is called with m.mu held.
func (m *TreeBrowserModel) filterEntriesLocked() []VisibleRow {
	query := strings.ToLower(m.SearchText)
	isTypeFilter := strings.HasPrefix(query, "type:")
	if isTypeFilter {
		query = strings.TrimSpace(strings.TrimPrefix(query, "type:"))
	}

	var rows []VisibleRow
	for _, entry := range m.known {
		match := false
		if isTypeFilter {
			resolved, ok := m.peerCtx.Resolve(entry.Path)
			if ok {
				match = strings.Contains(strings.ToLower(resolved.Entity.Type), query)
			}
		} else {
			match = strings.Contains(strings.ToLower(entry.Path), query)
		}
		if match {
			node := &TreeNode{
				Segment:  entry.Path,
				FullPath: entry.Path,
				HasEntry: true,
				Entry:    entry,
				Depth:    0,
			}
			rows = append(rows, VisibleRow{Node: node, Depth: 0})
		}
	}
	return rows
}

// FilterEntries returns entries matching a query string.
// Supports "type:foo" for type filtering, otherwise matches paths.
// Exported for use by renderers that manage their own display.
func FilterEntries(entries []store.LocationEntry, query string, resolve func(path string) (ResolvedEntity, bool)) []store.LocationEntry {
	q := strings.ToLower(query)
	isTypeFilter := strings.HasPrefix(q, "type:")
	if isTypeFilter {
		q = strings.TrimSpace(strings.TrimPrefix(q, "type:"))
	}

	var result []store.LocationEntry
	for _, entry := range entries {
		match := false
		if isTypeFilter {
			r, ok := resolve(entry.Path)
			if ok {
				match = strings.Contains(strings.ToLower(r.Entity.Type), q)
			}
		} else {
			match = strings.Contains(strings.ToLower(entry.Path), q)
		}
		if match {
			result = append(result, entry)
		}
	}
	return result
}
