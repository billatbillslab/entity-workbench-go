package workbench

import "math"

// SplitDir is the direction of a layout split.
type SplitDir int

const (
	SplitH SplitDir = iota // left | right
	SplitV                 // top / bottom
)

// MaxScreens is the maximum number of screens in the windowing framework.
const MaxScreens = 9

// LayoutNode is a generic binary split tree for window layout.
// W is the window/leaf type (must be comparable for identity checks).
// Leaf nodes have Win set and no children.
// Split nodes have Win as zero value, with Dir, First, Second set.
type LayoutNode[W comparable] struct {
	Win    W
	Dir    SplitDir
	First  *LayoutNode[W]
	Second *LayoutNode[W]
}

// LeafNode creates a leaf layout node containing a window.
func LeafNode[W comparable](w W) *LayoutNode[W] {
	return &LayoutNode[W]{Win: w}
}

// SplitNode creates a split node with two children.
func SplitNode[W comparable](dir SplitDir, first, second *LayoutNode[W]) *LayoutNode[W] {
	return &LayoutNode[W]{
		Dir:    dir,
		First:  first,
		Second: second,
	}
}

// IsLeaf returns true if this node is a leaf (has a window).
func (n *LayoutNode[W]) IsLeaf() bool {
	var zero W
	return n.Win != zero
}

// AllWindows collects all leaf windows in tree order.
func (n *LayoutNode[W]) AllWindows() []W {
	if n.IsLeaf() {
		return []W{n.Win}
	}
	return append(n.First.AllWindows(), n.Second.AllWindows()...)
}

// Split replaces the leaf containing target with a split node,
// putting the original in First and newWin in Second.
// Returns true if target was found and split.
func (n *LayoutNode[W]) Split(target W, dir SplitDir, newWin W) bool {
	if n.Win == target {
		n.Dir = dir
		n.First = LeafNode(n.Win)
		n.Second = LeafNode(newWin)
		var zero W
		n.Win = zero
		return true
	}
	if n.IsLeaf() {
		return false
	}
	return n.First.Split(target, dir, newWin) ||
		n.Second.Split(target, dir, newWin)
}

// Close removes the leaf containing target and promotes its sibling.
// Returns the sibling leaf window (for focus) and true, or zero+false
// if target was not found.
func (n *LayoutNode[W]) Close(target W) (W, bool) {
	var zero W
	if n.IsLeaf() {
		return zero, false
	}

	// Check if target is a direct child
	if n.First.Win == target {
		sibling := n.Second
		n.Win = sibling.Win
		n.Dir = sibling.Dir
		n.First = sibling.First
		n.Second = sibling.Second
		if n.IsLeaf() {
			return n.Win, true
		}
		windows := n.AllWindows()
		if len(windows) > 0 {
			return windows[0], true
		}
		return zero, true
	}
	if n.Second.Win == target {
		sibling := n.First
		n.Win = sibling.Win
		n.Dir = sibling.Dir
		n.First = sibling.First
		n.Second = sibling.Second
		if n.IsLeaf() {
			return n.Win, true
		}
		windows := n.AllWindows()
		if len(windows) > 0 {
			return windows[0], true
		}
		return zero, true
	}

	if w, ok := n.First.Close(target); ok {
		return w, true
	}
	return n.Second.Close(target)
}

// --- Spatial Navigation ---

// NavDir is a navigation direction.
type NavDir int

const (
	NavLeft NavDir = iota
	NavRight
	NavUp
	NavDown
)

// WindowRect describes a window's position in normalized (0-1) space.
type WindowRect[W comparable] struct {
	Win         W
	X, Y, W2, H float64
}

// ComputeRects assigns normalized rectangles (0-1 space) to each
// leaf window. Used for spatial navigation.
func ComputeRects[W comparable](n *LayoutNode[W], x, y, w, h float64) []WindowRect[W] {
	if n.IsLeaf() {
		return []WindowRect[W]{{Win: n.Win, X: x, Y: y, W2: w, H: h}}
	}
	switch n.Dir {
	case SplitH:
		halfW := w * 0.5
		var rects []WindowRect[W]
		rects = append(rects, ComputeRects(n.First, x, y, halfW, h)...)
		rects = append(rects, ComputeRects(n.Second, x+halfW, y, w-halfW, h)...)
		return rects
	case SplitV:
		halfH := h * 0.5
		var rects []WindowRect[W]
		rects = append(rects, ComputeRects(n.First, x, y, w, halfH)...)
		rects = append(rects, ComputeRects(n.Second, x, y+halfH, w, h-halfH)...)
		return rects
	}
	return nil
}

// FindRect returns the normalized rect for a specific window, or nil.
func FindRect[W comparable](n *LayoutNode[W], target W) *WindowRect[W] {
	rects := ComputeRects(n, 0, 0, 1, 1)
	for _, r := range rects {
		if r.Win == target {
			return &r
		}
	}
	return nil
}

// Navigate finds the closest window in the given direction from current.
// Returns the zero value if no window is found in that direction.
func Navigate[W comparable](n *LayoutNode[W], current W, dir NavDir) (W, bool) {
	var zero W
	rects := ComputeRects(n, 0, 0, 1, 1)
	if len(rects) <= 1 {
		return zero, false
	}

	var cur WindowRect[W]
	for _, r := range rects {
		if r.Win == current {
			cur = r
			break
		}
	}

	cx := cur.X + cur.W2/2
	cy := cur.Y + cur.H/2

	var best W
	bestDist := math.MaxFloat64
	found := false

	for _, r := range rects {
		if r.Win == current {
			continue
		}
		rx := r.X + r.W2/2
		ry := r.Y + r.H/2
		dx := rx - cx
		dy := ry - cy

		switch dir {
		case NavLeft:
			if dx >= 0 {
				continue
			}
		case NavRight:
			if dx <= 0 {
				continue
			}
		case NavUp:
			if dy >= 0 {
				continue
			}
		case NavDown:
			if dy <= 0 {
				continue
			}
		}

		dist := dx*dx + dy*dy
		if dist < bestDist {
			bestDist = dist
			best = r.Win
			found = true
		}
	}

	return best, found
}
