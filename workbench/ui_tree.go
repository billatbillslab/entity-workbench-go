package workbench

import (
	"sort"
	"strings"

	"go.entitychurch.org/entity-core-go/core/store"
)

// TreeNode represents a node in the hierarchical path tree.
type TreeNode struct {
	Segment  string
	FullPath string
	Children []*TreeNode
	HasEntry bool
	Entry    store.LocationEntry
	Expanded bool
	Depth    int
}

// VisibleRow is a flattened tree node with its display depth.
type VisibleRow struct {
	Node  *TreeNode
	Depth int
}

// BuildTree constructs a tree from sorted location entries.
func BuildTree(entries []store.LocationEntry) *TreeNode {
	root := &TreeNode{Segment: "", FullPath: "", Expanded: true, Depth: -1}
	for _, entry := range entries {
		parts := strings.Split(entry.Path, "/")
		node := root
		for i, part := range parts {
			found := false
			for _, child := range node.Children {
				if child.Segment == part {
					node = child
					found = true
					break
				}
			}
			if !found {
				fullPath := strings.Join(parts[:i+1], "/")
				child := &TreeNode{Segment: part, FullPath: fullPath, Depth: i}
				node.Children = append(node.Children, child)
				node = child
			}
		}
		node.HasEntry = true
		node.Entry = entry
	}
	SortTree(root)
	ExpandToDepth(root, 1)
	return root
}

// SortTree sorts children at every level alphabetically.
func SortTree(node *TreeNode) {
	sort.Slice(node.Children, func(i, j int) bool {
		return node.Children[i].Segment < node.Children[j].Segment
	})
	for _, child := range node.Children {
		SortTree(child)
	}
}

// ExpandToDepth expands all nodes up to the given depth.
func ExpandToDepth(node *TreeNode, maxDepth int) {
	if node.Depth < maxDepth {
		node.Expanded = true
	}
	for _, child := range node.Children {
		ExpandToDepth(child, maxDepth)
	}
}

// FlattenVisible returns all visible rows (expanded nodes and their children).
func FlattenVisible(root *TreeNode) []VisibleRow {
	var rows []VisibleRow
	for _, child := range root.Children {
		flattenNode(child, &rows)
	}
	return rows
}

func flattenNode(node *TreeNode, rows *[]VisibleRow) {
	*rows = append(*rows, VisibleRow{Node: node, Depth: node.Depth})
	if node.Expanded {
		for _, child := range node.Children {
			flattenNode(child, rows)
		}
	}
}

// CountLeaves counts leaf entries under a node.
func CountLeaves(node *TreeNode) int {
	if len(node.Children) == 0 {
		if node.HasEntry {
			return 1
		}
		return 0
	}
	count := 0
	if node.HasEntry {
		count = 1
	}
	for _, child := range node.Children {
		count += CountLeaves(child)
	}
	return count
}

// CollectExpanded returns a set of expanded node paths.
func CollectExpanded(node *TreeNode) map[string]bool {
	m := make(map[string]bool)
	var walk func(n *TreeNode)
	walk = func(n *TreeNode) {
		if n.Expanded && n.FullPath != "" {
			m[n.FullPath] = true
		}
		for _, child := range n.Children {
			walk(child)
		}
	}
	walk(node)
	return m
}

// RestoreExpanded re-expands nodes from a saved set.
func RestoreExpanded(node *TreeNode, expanded map[string]bool) {
	if expanded[node.FullPath] {
		node.Expanded = true
	}
	for _, child := range node.Children {
		RestoreExpanded(child, expanded)
	}
}

// ExpandAncestors expands all ancestors of the given path.
func ExpandAncestors(root *TreeNode, path string) {
	parts := strings.Split(path, "/")
	node := root
	for i := 0; i < len(parts); i++ {
		for _, child := range node.Children {
			if child.Segment == parts[i] {
				child.Expanded = true
				node = child
				break
			}
		}
	}
}

// InsertOrUpdate inserts a path into the tree, or updates the binding
// if the path already exists. Intermediate nodes are created as
// needed. Children at each newly-created intermediate level are sorted
// at insertion time so the tree stays ordered. Returns the leaf node.
//
// Intermediate parents (folder nodes) inserted along the way are NOT
// expanded — the caller is responsible for expand state.
func InsertOrUpdate(root *TreeNode, entry store.LocationEntry) *TreeNode {
	parts := strings.Split(entry.Path, "/")
	node := root
	for i, part := range parts {
		idx := childIndexBySegment(node.Children, part)
		var child *TreeNode
		if idx >= 0 {
			child = node.Children[idx]
		} else {
			fullPath := strings.Join(parts[:i+1], "/")
			child = &TreeNode{Segment: part, FullPath: fullPath, Depth: i}
			node.Children = insertChildSorted(node.Children, child)
		}
		node = child
	}
	node.HasEntry = true
	node.Entry = entry
	return node
}

// Remove unbinds a path. If the leaf has no children, the leaf and any
// empty ancestor folders are pruned. Returns true if the path was
// found.
func Remove(root *TreeNode, path string) bool {
	if path == "" {
		return false
	}
	parts := strings.Split(path, "/")
	// Walk down keeping parent pointers so we can prune upward.
	type stackFrame struct {
		parent *TreeNode
		idx    int
	}
	stack := make([]stackFrame, 0, len(parts))
	node := root
	for _, part := range parts {
		idx := childIndexBySegment(node.Children, part)
		if idx < 0 {
			return false
		}
		stack = append(stack, stackFrame{parent: node, idx: idx})
		node = node.Children[idx]
	}
	if !node.HasEntry {
		return false
	}
	node.HasEntry = false
	node.Entry = store.LocationEntry{}

	// Prune empty ancestors: if a node has no entry AND no children,
	// it's gone. Walk back up.
	for i := len(stack) - 1; i >= 0; i-- {
		frame := stack[i]
		child := frame.parent.Children[frame.idx]
		if child.HasEntry || len(child.Children) > 0 {
			break
		}
		frame.parent.Children = append(frame.parent.Children[:frame.idx], frame.parent.Children[frame.idx+1:]...)
	}
	return true
}

// childIndexBySegment finds the child with matching segment via binary
// search (children are kept sorted by segment). Returns -1 if absent.
func childIndexBySegment(children []*TreeNode, segment string) int {
	lo, hi := 0, len(children)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case children[mid].Segment < segment:
			lo = mid + 1
		case children[mid].Segment > segment:
			hi = mid
		default:
			return mid
		}
	}
	return -1
}

// insertChildSorted inserts child into children at the right position
// to maintain sort order by Segment. Returns the new slice.
func insertChildSorted(children []*TreeNode, child *TreeNode) []*TreeNode {
	lo, hi := 0, len(children)
	for lo < hi {
		mid := (lo + hi) / 2
		if children[mid].Segment < child.Segment {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	children = append(children, nil)
	copy(children[lo+1:], children[lo:])
	children[lo] = child
	return children
}
