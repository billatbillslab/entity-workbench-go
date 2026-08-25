package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	wb "entity-workbench-go/workbench"
)

// entityDetailContent is the console renderer for the entity detail panel.
// Business logic lives in wb.DetailModel; this file is tview I/O.
//
// The current path comes from an SDK selection subscription
// (OnSelectionChange against the screen aggregate slot). Handler runs
// on the SDK goroutine and marshals back to the tview thread via
// QueueUpdateDraw; the mutex guards currentPath against the tview
// render thread reading it.
type entityDetailContent struct {
	view    *tview.TextView
	model   *wb.DetailModel
	ws      *workspace
	peerCtx *wb.PeerContext

	mu              sync.Mutex
	currentPath     string
	cancelSelection func() // unsubscribe from selection slot
	cancelContent   func() // unsubscribe from current-path content watch
}

func newEntityDetail(ws *workspace, peerCtx *wb.PeerContext, state *wb.WorkspaceState, screenIdx int) *entityDetailContent {
	ed := &entityDetailContent{
		view: tview.NewTextView().
			SetDynamicColors(true).
			SetScrollable(true).
			SetWordWrap(true),
		model:   wb.NewDetailModel(peerCtx),
		ws:      ws,
		peerCtx: peerCtx,
	}

	ed.view.SetBorder(true).SetTitle(" Entity Detail ").SetTitleAlign(tview.AlignLeft)
	ed.view.SetBorderColor(tcell.ColorDarkCyan)

	ed.view.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			ed.model.ToggleRaw()
			ed.render()
			return nil
		}
		return event
	})

	if state != nil {
		// Seed synchronously so first render has the right path.
		if sel, ok := state.ReadSelection(screenIdx); ok {
			ed.currentPath = sel.Path
			ed.rebindContentWatch(sel.Path)
		}
		ed.cancelSelection = state.OnSelectionChange(state.ScreenSelectionPath(screenIdx), func(sel wb.Selection) {
			ws.tviewApp.QueueUpdateDraw(func() {
				ed.mu.Lock()
				ed.currentPath = sel.Path
				ed.mu.Unlock()
				ed.rebindContentWatch(sel.Path)
				ed.render()
			})
		})
	}

	// Render once so the panel has initial content.
	ed.render()
	return ed
}

// rebindContentWatch cancels the previous content-watch (if any) and
// registers a new one on the given path. The watch fires only when the
// entity at currentPath is mutated; the handler triggers a re-render.
// This is what eliminates the "re-resolve entity on every refresh tick"
// anti-pattern — render is event-driven on both selection AND content.
func (ed *entityDetailContent) rebindContentWatch(path string) {
	if ed.cancelContent != nil {
		ed.cancelContent()
		ed.cancelContent = nil
	}
	if path == "" || ed.peerCtx == nil || ed.peerCtx.Store() == nil {
		return
	}
	w, err := ed.peerCtx.Store().Watch(path)
	if err != nil {
		// Store may have no watch hub (test scaffolding) or the path
		// may be invalid. Either way, no live re-render. The selection
		// change still triggers an initial render.
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range w.Events() {
			ed.ws.tviewApp.QueueUpdateDraw(func() { ed.render() })
		}
	}()
	ed.cancelContent = func() { w.Close(); <-done }
}

func (ed *entityDetailContent) close() {
	if ed.cancelSelection != nil {
		ed.cancelSelection()
		ed.cancelSelection = nil
	}
	if ed.cancelContent != nil {
		ed.cancelContent()
		ed.cancelContent = nil
	}
}

func (ed *entityDetailContent) typeName() string             { return "entity-detail" }
func (ed *entityDetailContent) widget() tview.Primitive      { return ed.view }
func (ed *entityDetailContent) focusTarget() tview.Primitive { return ed.view }

// refresh is a no-op: render is driven by OnSelectionChange (path
// changed) + the per-path Store.Watch (entity content changed). The
// global queueRefresh tick no longer triggers a redraw — keeps
// heartbeats and other irrelevant writes off the inspector's render
// path entirely.
func (ed *entityDetailContent) refresh() {}
func (ed *entityDetailContent) setHighlight(m highlightMode) {
	ed.view.SetBorderColor(borderColorForMode(m))
}

func (ed *entityDetailContent) handleEvent(event string, value string) bool {
	switch event {
	case "toggle_raw":
		ed.model.ToggleRaw()
		ed.render()
		return true
	}
	return false
}

func (ed *entityDetailContent) render() {
	ed.view.Clear()
	ed.mu.Lock()
	path := ed.currentPath
	ed.mu.Unlock()
	ed.model.LoadPath(path)
	out := ed.model.Render()

	if out.Empty {
		fmt.Fprintf(ed.view, "[gray]No entity selected[-]")
		return
	}

	if out.Directory {
		fmt.Fprintf(ed.view, "[gray]%s/[-]", out.Path)
		return
	}

	hdr := out.Header
	fmt.Fprintf(ed.view, "[gray]Path[-]  [skyblue]%s[-]\n", tview.Escape(hdr.Path))
	fmt.Fprintf(ed.view, "[gray]Type[-]  [white]%s[-]\n", tview.Escape(hdr.Type))
	fmt.Fprintf(ed.view, "[gray]Hash[-]  [darkgray]%s[-]\n", hdr.Hash)
	fmt.Fprintf(ed.view, "[gray]Size[-]  [darkgray]%d bytes[-]\n", hdr.Size)

	if out.RawView {
		fmt.Fprintf(ed.view, "\n[orange]RAW[-] [gray](Tab to toggle)[-]\n")
		fmt.Fprintf(ed.view, "[gray]───[-]\n")
		for _, line := range strings.Split(out.DiagHash, "\n") {
			if line != "" {
				fmt.Fprintf(ed.view, "[white]%s[-]\n", tview.Escape(line))
			}
		}
		fmt.Fprintf(ed.view, "\n[gray]Data hex:[-]\n")
		for _, line := range out.HexLines {
			fmt.Fprintf(ed.view, "[teal]%s[-]\n", line)
		}
	} else {
		fmt.Fprintf(ed.view, "\n[green]RENDERED[-] [gray](Tab to toggle)[-]\n")
		fmt.Fprintf(ed.view, "[gray]───[-]\n")
		if len(out.Lines) == 0 {
			fmt.Fprintf(ed.view, "[red]decode error[-]\n")
		} else {
			for _, line := range out.Lines {
				writeFormattedLine(ed.view, line, 0)
			}
		}
	}
}
