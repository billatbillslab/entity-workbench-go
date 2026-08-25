package main

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	wb "entity-workbench-go/workbench"
)

// peerInfoContent is the console renderer for peer statistics.
// Business logic lives in wb.PeerInfoModel; this file is tview I/O.
type peerInfoContent struct {
	view  *tview.TextView
	model *wb.PeerInfoModel
}

func newPeerInfo(peerCtx *wb.PeerContext) *peerInfoContent {
	pi := &peerInfoContent{
		view: tview.NewTextView().
			SetDynamicColors(true).
			SetScrollable(true),
		model: wb.NewPeerInfoModel(peerCtx),
	}

	pi.view.SetBorder(true).SetTitle(" Peer Info ").SetTitleAlign(tview.AlignLeft)
	pi.view.SetBorderColor(tcell.ColorDarkCyan)

	return pi
}

func (pi *peerInfoContent) typeName() string             { return "peer-info" }
func (pi *peerInfoContent) widget() tview.Primitive      { return pi.view }
func (pi *peerInfoContent) focusTarget() tview.Primitive { return pi.view }
func (pi *peerInfoContent) handleEvent(e, v string) bool { return false }
func (pi *peerInfoContent) setHighlight(m highlightMode) {
	pi.view.SetBorderColor(borderColorForMode(m))
}

func (pi *peerInfoContent) refresh() {
	pi.view.Clear()

	stats := pi.model.Render()

	fmt.Fprintf(pi.view, "[skyblue]Peer Status[-]\n\n")
	fmt.Fprintf(pi.view, "[yellow]Entities[-]  [white]%d[-]\n", stats.EntityCount)
	fmt.Fprintf(pi.view, "[yellow]Paths[-]     [white]%d[-]\n", stats.PathCount)

	if stats.PathCount > 0 {
		fmt.Fprintf(pi.view, "\n[skyblue]Paths[-]\n")
		for _, path := range stats.Paths {
			fmt.Fprintf(pi.view, "  [gray]%s[-]\n", tview.Escape(path))
		}
	}
}
