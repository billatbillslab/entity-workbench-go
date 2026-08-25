package main

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	wb "entity-workbench-go/workbench"
)

// logViewerContent is the console renderer for the event log.
// Business logic and state persistence live in wb.LogFilterModel.
// This file is tview I/O only.
type logViewerContent struct {
	view     *tview.TextView
	model    *wb.LogFilterModel
	windowID uint32
}

func newLogViewer(eventLog *wb.EventLog, windowID uint32) *logViewerContent {
	lv := &logViewerContent{
		view: tview.NewTextView().
			SetDynamicColors(true).
			SetScrollable(true),
		model:    wb.NewLogFilterModel(eventLog),
		windowID: windowID,
	}

	lv.view.SetBorder(true).SetTitleAlign(tview.AlignLeft)
	lv.view.SetBorderColor(tcell.ColorDarkCyan)
	lv.updateTitle()

	lv.view.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			lv.cycleDisplayLevel()
			return nil
		}
		if event.Key() == tcell.KeyCtrlL {
			lv.cycleCollectionLevel()
			return nil
		}
		return event
	})

	return lv
}

func (lv *logViewerContent) cycleDisplayLevel() {
	lv.model.CycleDisplayLevel()
	lv.updateTitle()
	lv.model.EventLog().Appendf("window %d display → %s", lv.windowID, lv.model.DisplayLevelName())
	lv.fullRerender()
}

func (lv *logViewerContent) cycleCollectionLevel() {
	lv.model.CycleCollectionLevel()
	lv.updateTitle()
	lv.model.EventLog().Appendf("collection level → %s", lv.model.CollectionLevelName())
	lv.fullRerender()
}

func (lv *logViewerContent) updateTitle() {
	lv.view.SetTitle(fmt.Sprintf(" %s ", lv.model.Render().Title))
}

func (lv *logViewerContent) typeName() string             { return "log-viewer" }
func (lv *logViewerContent) widget() tview.Primitive      { return lv.view }
func (lv *logViewerContent) focusTarget() tview.Primitive { return lv.view }

func (lv *logViewerContent) refresh() {
	entries := lv.model.NewEntries()
	for _, e := range entries {
		ts := e.Time.Format("15:04:05")
		var levelTag string
		switch e.Level {
		case wb.LogVerbose:
			levelTag = "[darkcyan]"
		case wb.LogDebug:
			levelTag = "[gray]"
		default:
			levelTag = "[white]"
		}
		fmt.Fprintf(lv.view, "[gray]%s[-] %s%s[-]\n", ts, levelTag, tview.Escape(e.Message))
	}
}

func (lv *logViewerContent) fullRerender() {
	lv.view.Clear()
	lv.model.ResetSequence()
	lv.refresh()
	lv.view.ScrollToEnd()
}

func (lv *logViewerContent) handleEvent(event string, value string) bool {
	switch event {
	case "clear":
		lv.view.Clear()
		lv.view.ScrollToEnd()
		return true
	}
	return false
}

func (lv *logViewerContent) setHighlight(mode highlightMode) {
	lv.view.SetBorderColor(borderColorForMode(mode))
}

// settingValue extracts the "value" field from a decoded app/state/setting entity.
func settingValue(r wb.ResolvedEntity) string {
	m, ok := r.Decoded.(map[interface{}]interface{})
	if !ok {
		return ""
	}
	v, ok := m["value"].(string)
	if !ok {
		return ""
	}
	return v
}
