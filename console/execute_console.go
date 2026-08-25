package main

import (
	"encoding/hex"
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	wb "entity-workbench-go/workbench"
)

// executeConsoleContent is the console renderer for the handler browser.
// Business logic lives in wb.HandlerBrowserModel; this file is tview I/O.
type executeConsoleContent struct {
	root *tview.Flex
	ws   *workspace

	model *wb.HandlerBrowserModel

	// tview UI widgets
	output      *tview.TextView
	handlerList *tview.List
	opList      *tview.List
	uriField    *tview.InputField
	opField     *tview.InputField
	resField    *tview.InputField
	specView    *tview.TextView

	refreshing      bool
	lastHandlerHash string
}

func newExecuteConsole(ws *workspace, peerCtx *wb.PeerContext, eventLog *wb.EventLog, dispatch wb.DispatchFunc) *executeConsoleContent {
	ec := &executeConsoleContent{
		ws:    ws,
		model: wb.NewHandlerBrowserModel(peerCtx, dispatch),
	}

	// Output first — callbacks write here
	ec.output = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWordWrap(true)

	// Spec view
	ec.specView = tview.NewTextView().SetDynamicColors(true)

	// Handler list
	ec.handlerList = tview.NewList().
		SetHighlightFullLine(true).
		SetMainTextColor(tcell.ColorSkyblue).
		SetSecondaryTextColor(tcell.ColorGray).
		SetSelectedBackgroundColor(tcell.ColorDarkSlateGray)
	ec.handlerList.SetBorder(true).SetTitle(" Handlers ").SetBorderColor(tcell.ColorDarkCyan)
	ec.handlerList.SetChangedFunc(func(index int, main, secondary string, shortcut rune) {
		if ec.refreshing {
			return
		}
		ec.model.SelectHandler(index)
		ec.populateOperations()
		ec.syncFields()
	})
	ec.handlerList.SetSelectedFunc(func(index int, main, secondary string, shortcut rune) {
		ws.tviewApp.SetFocus(ec.opList)
	})

	// Operation list
	ec.opList = tview.NewList().
		SetHighlightFullLine(true).
		SetMainTextColor(tcell.ColorWhite).
		SetSecondaryTextColor(tcell.ColorGray).
		SetSelectedBackgroundColor(tcell.ColorDarkSlateGray)
	ec.opList.SetBorder(true).SetTitle(" Operations ").SetBorderColor(tcell.ColorDarkCyan)
	ec.opList.SetChangedFunc(func(index int, main, secondary string, shortcut rune) {
		if ec.refreshing {
			return
		}
		ec.model.SelectOperation(index)
		ec.syncFields()
	})
	ec.opList.SetSelectedFunc(func(index int, main, secondary string, shortcut rune) {
		ec.execute()
	})

	// Fields
	ec.uriField = tview.NewInputField().
		SetLabel("[yellow]URI[-]       ").
		SetFieldBackgroundColor(tcell.ColorBlack)
	ec.opField = tview.NewInputField().
		SetLabel("[yellow]Operation[-] ").
		SetFieldBackgroundColor(tcell.ColorBlack)
	ec.resField = tview.NewInputField().
		SetLabel("[yellow]Resource[-]  ").
		SetFieldBackgroundColor(tcell.ColorBlack).
		SetPlaceholder("(optional)")

	// Populate from model
	ec.populateHandlerList()
	ec.populateOperations()
	ec.syncFields()

	// Tab cycling
	widgets := []tview.Primitive{ec.handlerList, ec.opList, ec.uriField, ec.opField, ec.resField}
	for i := range widgets {
		next := widgets[(i+1)%len(widgets)]
		switch w := widgets[i].(type) {
		case *tview.List:
			w.SetInputCapture(makeTabCapture(ws, next))
		case *tview.InputField:
			w.SetInputCapture(makeTabEnterCapture(ws, next, func() { ec.execute() }))
		}
	}

	// Layout
	selectors := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(ec.handlerList, 0, 1, true).
		AddItem(ec.opList, 0, 1, false)

	fields := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(ec.uriField, 1, 0, false).
		AddItem(ec.opField, 1, 0, false).
		AddItem(ec.resField, 1, 0, false)

	rightSide := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(ec.specView, 4, 0, false).
		AddItem(fields, 3, 0, false).
		AddItem(ec.output, 0, 1, false)

	ec.root = tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(selectors, 0, 1, true).
		AddItem(rightSide, 0, 1, false)
	ec.root.SetBorder(true).SetTitle(" Execute Console ").SetTitleAlign(tview.AlignLeft)
	ec.root.SetBorderColor(tcell.ColorDarkCyan)

	return ec
}

func makeTabCapture(ws *workspace, next tview.Primitive) func(*tcell.EventKey) *tcell.EventKey {
	return func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			ws.tviewApp.SetFocus(next)
			return nil
		}
		return event
	}
}

func makeTabEnterCapture(ws *workspace, next tview.Primitive, onEnter func()) func(*tcell.EventKey) *tcell.EventKey {
	return func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			ws.tviewApp.SetFocus(next)
			return nil
		}
		if event.Key() == tcell.KeyEnter {
			onEnter()
			return nil
		}
		return event
	}
}

func (ec *executeConsoleContent) syncFields() {
	ec.specView.Clear()
	out := ec.model.Render()
	if out.Selected == nil {
		return
	}
	h := out.Selected
	ec.uriField.SetText(h.Pattern)

	op := out.SelectedOpName
	if op != "" {
		ec.opField.SetText(op)
		spec := h.Specs[op]
		fmt.Fprintf(ec.specView, "[yellow]%s[-] [white]%s[-]", h.Pattern, op)
		if spec.InputType != "" {
			fmt.Fprintf(ec.specView, "  [gray]in:[-][skyblue]%s[-]", spec.InputType)
		}
		if spec.OutputType != "" {
			fmt.Fprintf(ec.specView, "  [gray]out:[-][skyblue]%s[-]", spec.OutputType)
		}
		fmt.Fprintf(ec.specView, "\n")
	}
}

func (ec *executeConsoleContent) populateHandlerList() {
	ec.handlerList.Clear()
	out := ec.model.Render()
	for _, h := range out.Handlers {
		label := h.Pattern
		desc := h.Name
		if desc == "" {
			desc = fmt.Sprintf("%d ops", len(h.Operations))
		}
		ec.handlerList.AddItem(label, desc, 0, nil)
	}
	if len(out.Handlers) > 0 && out.SelectedHandler < len(out.Handlers) {
		ec.handlerList.SetCurrentItem(out.SelectedHandler)
	}
}

func (ec *executeConsoleContent) populateOperations() {
	ec.opList.Clear()
	out := ec.model.Render()
	if out.Selected == nil {
		return
	}
	h := out.Selected
	for _, op := range h.Operations {
		spec := h.Specs[op]
		desc := ""
		if spec.InputType != "" {
			desc = spec.InputType
		}
		ec.opList.AddItem(op, desc, 0, nil)
	}
	if len(h.Operations) > 0 {
		if out.SelectedOp >= len(h.Operations) {
			ec.model.SelectOperation(0)
			out = ec.model.Render()
		}
		ec.opList.SetCurrentItem(out.SelectedOp)
	}
}

func (ec *executeConsoleContent) execute() {
	uri := ec.uriField.GetText()
	op := ec.opField.GetText()
	res := ec.resField.GetText()

	if uri == "" || op == "" {
		fmt.Fprintf(ec.output, "[red]need URI and operation[-]\n")
		ec.output.ScrollToEnd()
		return
	}

	path := uri
	if res != "" {
		path = res
	}

	fmt.Fprintf(ec.output, "[skyblue]> %s %s[-]", uri, op)
	if res != "" {
		fmt.Fprintf(ec.output, " [gray]%s[-]", tview.Escape(res))
	}
	fmt.Fprintf(ec.output, "\n")

	resp, err := ec.model.DispatchFn()(path, op)
	if err != nil {
		fmt.Fprintf(ec.output, "[red]ERROR[-] %s\n", tview.Escape(err.Error()))
		ec.output.ScrollToEnd()
		return
	}

	fmt.Fprintf(ec.output, "[green]%d[-] [white]%s[-]\n", resp.Status, tview.Escape(resp.Type))

	if len(resp.Data) > 0 {
		if decoded, ok := wb.DecodeEntityData(resp.Data); ok {
			for _, line := range wb.FormatCBOR(decoded) {
				writeFormattedLine(ec.output, line, 1)
			}
		} else {
			fmt.Fprintf(ec.output, "[teal]%s[-]\n", hex.EncodeToString(resp.Data))
		}
	}

	if len(resp.Included) > 0 {
		fmt.Fprintf(ec.output, "[gray]+%d included[-]\n", len(resp.Included))
	}

	ec.output.ScrollToEnd()
}

// --- windowContent ---

func (ec *executeConsoleContent) typeName() string             { return "execute-console" }
func (ec *executeConsoleContent) widget() tview.Primitive      { return ec.root }
func (ec *executeConsoleContent) focusTarget() tview.Primitive { return ec.handlerList }
func (ec *executeConsoleContent) handleEvent(e, v string) bool { return false }
func (ec *executeConsoleContent) setHighlight(m highlightMode) {
	ec.root.SetBorderColor(borderColorForMode(m))
}

func (ec *executeConsoleContent) refresh() {
	if !ec.model.Refresh() {
		return
	}

	ec.refreshing = true
	defer func() { ec.refreshing = false }()
	ec.populateHandlerList()
	ec.populateOperations()
	ec.syncFields()
}
