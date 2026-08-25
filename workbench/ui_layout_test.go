package workbench

import "testing"

// Use int as a simple window ID for tests.

func TestLayoutNode_Leaf(t *testing.T) {
	n := LeafNode(1)
	if !n.IsLeaf() {
		t.Error("expected leaf")
	}
	wins := n.AllWindows()
	if len(wins) != 1 || wins[0] != 1 {
		t.Errorf("windows = %v, want [1]", wins)
	}
}

func TestLayoutNode_Split(t *testing.T) {
	n := LeafNode(1)
	if !n.Split(1, SplitH, 2) {
		t.Fatal("split should succeed")
	}
	if n.IsLeaf() {
		t.Error("should not be leaf after split")
	}
	wins := n.AllWindows()
	if len(wins) != 2 || wins[0] != 1 || wins[1] != 2 {
		t.Errorf("windows = %v, want [1 2]", wins)
	}
}

func TestLayoutNode_SplitDeep(t *testing.T) {
	n := LeafNode(1)
	n.Split(1, SplitH, 2)
	n.Split(2, SplitV, 3)

	wins := n.AllWindows()
	if len(wins) != 3 {
		t.Fatalf("windows = %v, want 3", wins)
	}
	// Tree order: 1, 2, 3
	if wins[0] != 1 || wins[1] != 2 || wins[2] != 3 {
		t.Errorf("windows = %v, want [1 2 3]", wins)
	}
}

func TestLayoutNode_SplitNotFound(t *testing.T) {
	n := LeafNode(1)
	if n.Split(99, SplitH, 2) {
		t.Error("split should fail for non-existent window")
	}
}

func TestLayoutNode_Close(t *testing.T) {
	n := LeafNode(1)
	n.Split(1, SplitH, 2)

	sibling, ok := n.Close(1)
	if !ok {
		t.Fatal("close should succeed")
	}
	if sibling != 2 {
		t.Errorf("sibling = %d, want 2", sibling)
	}
	if !n.IsLeaf() {
		t.Error("should be leaf after close")
	}
	wins := n.AllWindows()
	if len(wins) != 1 || wins[0] != 2 {
		t.Errorf("windows = %v, want [2]", wins)
	}
}

func TestLayoutNode_CloseSecond(t *testing.T) {
	n := LeafNode(1)
	n.Split(1, SplitH, 2)

	sibling, ok := n.Close(2)
	if !ok {
		t.Fatal("close should succeed")
	}
	if sibling != 1 {
		t.Errorf("sibling = %d, want 1", sibling)
	}
}

func TestLayoutNode_CloseDeep(t *testing.T) {
	n := LeafNode(1)
	n.Split(1, SplitH, 2)
	n.Split(2, SplitV, 3)

	// Close window 3 (deep leaf)
	sibling, ok := n.Close(3)
	if !ok {
		t.Fatal("close should succeed")
	}
	if sibling != 2 {
		t.Errorf("sibling = %d, want 2", sibling)
	}
	wins := n.AllWindows()
	if len(wins) != 2 {
		t.Fatalf("windows = %v, want 2", wins)
	}
}

func TestLayoutNode_CloseNotFound(t *testing.T) {
	n := LeafNode(1)
	_, ok := n.Close(99)
	if ok {
		t.Error("close should fail for non-existent window")
	}
}

func TestComputeRects(t *testing.T) {
	n := LeafNode(1)
	n.Split(1, SplitH, 2)

	rects := ComputeRects(n, 0, 0, 1, 1)
	if len(rects) != 2 {
		t.Fatalf("rects = %d, want 2", len(rects))
	}

	// Horizontal split: left half + right half
	left := rects[0]
	right := rects[1]
	if left.Win != 1 || right.Win != 2 {
		t.Errorf("wins = %d, %d, want 1, 2", left.Win, right.Win)
	}
	if left.W2 != 0.5 || right.W2 != 0.5 {
		t.Errorf("widths = %f, %f, want 0.5, 0.5", left.W2, right.W2)
	}
	if left.X != 0 || right.X != 0.5 {
		t.Errorf("x = %f, %f, want 0, 0.5", left.X, right.X)
	}
}

func TestComputeRects_Vertical(t *testing.T) {
	n := LeafNode(1)
	n.Split(1, SplitV, 2)

	rects := ComputeRects(n, 0, 0, 1, 1)
	top := rects[0]
	bottom := rects[1]
	if top.H != 0.5 || bottom.H != 0.5 {
		t.Errorf("heights = %f, %f, want 0.5, 0.5", top.H, bottom.H)
	}
}

func TestFindRect(t *testing.T) {
	n := LeafNode(1)
	n.Split(1, SplitH, 2)

	r := FindRect(n, 2)
	if r == nil {
		t.Fatal("expected rect")
	}
	if r.Win != 2 {
		t.Errorf("win = %d, want 2", r.Win)
	}

	r = FindRect(n, 99)
	if r != nil {
		t.Error("expected nil for missing window")
	}
}

func TestNavigate(t *testing.T) {
	// Layout: [1 | 2]  (horizontal split)
	n := LeafNode(1)
	n.Split(1, SplitH, 2)

	// From 1, go right → 2
	w, ok := Navigate(n, 1, NavRight)
	if !ok || w != 2 {
		t.Errorf("right from 1 = %d, %v, want 2", w, ok)
	}

	// From 2, go left → 1
	w, ok = Navigate(n, 2, NavLeft)
	if !ok || w != 1 {
		t.Errorf("left from 2 = %d, %v, want 1", w, ok)
	}

	// From 1, go left → nothing
	_, ok = Navigate(n, 1, NavLeft)
	if ok {
		t.Error("left from 1 should fail")
	}
}

func TestNavigate_Vertical(t *testing.T) {
	// Layout: [1] / [2]  (vertical split)
	n := LeafNode(1)
	n.Split(1, SplitV, 2)

	w, ok := Navigate(n, 1, NavDown)
	if !ok || w != 2 {
		t.Errorf("down from 1 = %d, %v, want 2", w, ok)
	}

	w, ok = Navigate(n, 2, NavUp)
	if !ok || w != 1 {
		t.Errorf("up from 2 = %d, %v, want 1", w, ok)
	}
}

func TestNavigate_Grid(t *testing.T) {
	// Layout: [1 | 2] / [3 | 4]
	n := LeafNode(1)
	n.Split(1, SplitV, 3) // top=1, bottom=3
	n.Split(1, SplitH, 2) // top-left=1, top-right=2
	n.Split(3, SplitH, 4) // bottom-left=3, bottom-right=4

	// From 1, go right → 2
	w, ok := Navigate(n, 1, NavRight)
	if !ok || w != 2 {
		t.Errorf("right from 1 = %d", w)
	}

	// From 1, go down → 3
	w, ok = Navigate(n, 1, NavDown)
	if !ok || w != 3 {
		t.Errorf("down from 1 = %d", w)
	}

	// From 4, go up → 2
	w, ok = Navigate(n, 4, NavUp)
	if !ok || w != 2 {
		t.Errorf("up from 4 = %d", w)
	}

	// From 4, go left → 3
	w, ok = Navigate(n, 4, NavLeft)
	if !ok || w != 3 {
		t.Errorf("left from 4 = %d", w)
	}
}

func TestNavigate_SingleWindow(t *testing.T) {
	n := LeafNode(1)
	_, ok := Navigate(n, 1, NavRight)
	if ok {
		t.Error("navigate should fail with single window")
	}
}
