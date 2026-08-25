package main

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// tviewDemoContent exercises tview widgets to understand their behavior
// within the panel system. Tab cycles between widgets.
type tviewDemoContent struct {
	root *tview.Flex

	// Widgets
	list     *tview.List
	dropdown *tview.DropDown
	input    *tview.InputField
	checkbox *tview.Checkbox
	output   *tview.TextView
	table    *tview.Table
}

func newTviewDemo(ws *workspace) *tviewDemoContent {
	d := &tviewDemoContent{}

	// Output log (created first — other widgets write to it)
	d.output = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)
	d.output.SetBorder(true).SetTitle(" Events ").SetBorderColor(tcell.ColorDarkCyan)
	fmt.Fprintf(d.output, "[gray]Tab to cycle widgets, interact with each[-]\n")

	// 1. List
	d.list = tview.NewList().
		AddItem("Alpha", "First item", 0, nil).
		AddItem("Beta", "Second item", 0, nil).
		AddItem("Gamma", "Third item", 0, nil).
		AddItem("Delta", "Fourth item", 0, nil).
		AddItem("Epsilon", "Fifth item", 0, nil)
	d.list.SetHighlightFullLine(true).
		SetMainTextColor(tcell.ColorSkyblue).
		SetSelectedBackgroundColor(tcell.ColorDarkSlateGray)
	d.list.SetBorder(true).SetTitle(" List ").SetBorderColor(tcell.ColorDarkCyan)
	d.list.SetChangedFunc(func(index int, main, secondary string, shortcut rune) {
		fmt.Fprintf(d.output, "[gray]list changed:[-] %s\n", main)
		d.output.ScrollToEnd()
	})

	// 2. DropDown
	d.dropdown = tview.NewDropDown().
		SetLabel("Pick: ").
		SetOptions([]string{"Option A", "Option B", "Option C", "Option D"}, func(text string, index int) {
			fmt.Fprintf(d.output, "[gray]dropdown selected:[-] %s (idx %d)\n", text, index)
			d.output.ScrollToEnd()
		}).
		SetFieldBackgroundColor(tcell.ColorBlack).
		SetCurrentOption(0)

	// 3. InputField
	d.input = tview.NewInputField().
		SetLabel("Text: ").
		SetFieldBackgroundColor(tcell.ColorBlack).
		SetPlaceholder("type something...")
	d.input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			fmt.Fprintf(d.output, "[gray]input submitted:[-] %s\n", d.input.GetText())
			d.output.ScrollToEnd()
			d.input.SetText("")
		}
	})

	// 4. Checkbox
	d.checkbox = tview.NewCheckbox().
		SetLabel("Toggle: ").
		SetChecked(false)
	d.checkbox.SetChangedFunc(func(checked bool) {
		fmt.Fprintf(d.output, "[gray]checkbox:[-] %v\n", checked)
		d.output.ScrollToEnd()
	})

	// 5. Table
	d.table = tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetSelectedStyle(tcell.StyleDefault.Background(tcell.ColorDarkSlateGray))
	d.table.SetBorder(true).SetTitle(" Table ").SetBorderColor(tcell.ColorDarkCyan)
	// Header
	headers := []string{"Name", "Type", "Size", "Status"}
	for i, h := range headers {
		d.table.SetCell(0, i, tview.NewTableCell(h).
			SetTextColor(tcell.ColorYellow).
			SetSelectable(false).
			SetExpansion(1))
	}
	// Rows
	rows := [][]string{
		{"system/tree", "handler", "4 ops", "active"},
		{"system/connect", "handler", "3 ops", "active"},
		{"local/files", "handler", "5 ops", "active"},
		{"system/inbox", "extension", "2 ops", "active"},
		{"system/clock", "extension", "1 op", "idle"},
	}
	for r, row := range rows {
		for c, cell := range row {
			color := tcell.ColorWhite
			if c == 0 {
				color = tcell.ColorSkyblue
			} else if c == 3 {
				color = tcell.ColorGreen
			}
			d.table.SetCell(r+1, c, tview.NewTableCell(cell).
				SetTextColor(color).
				SetExpansion(1))
		}
	}
	d.table.SetSelectionChangedFunc(func(row, col int) {
		if row > 0 {
			name := d.table.GetCell(row, 0).Text
			fmt.Fprintf(d.output, "[gray]table row:[-] %s\n", name)
			d.output.ScrollToEnd()
		}
	})

	// Tab cycling between all interactive widgets
	widgets := []tview.Primitive{d.list, d.dropdown, d.input, d.checkbox, d.table}
	for i, w := range widgets {
		next := widgets[(i+1)%len(widgets)]
		w := w // capture
		switch v := w.(type) {
		case *tview.List:
			v.SetInputCapture(tabCapture(ws, next))
		case *tview.DropDown:
			v.SetInputCapture(tabCapture(ws, next))
		case *tview.InputField:
			v.SetInputCapture(tabCapture(ws, next))
		case *tview.Checkbox:
			v.SetInputCapture(tabCapture(ws, next))
		case *tview.Table:
			v.SetInputCapture(tabCapture(ws, next))
		}
	}

	// Layout
	// Left column: list + table
	leftCol := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(d.list, 0, 1, true).
		AddItem(d.table, 0, 1, false)

	// Right column: controls + output
	controls := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(d.dropdown, 1, 0, false).
		AddItem(d.input, 1, 0, false).
		AddItem(d.checkbox, 1, 0, false)

	rightCol := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(controls, 3, 0, false).
		AddItem(d.output, 0, 1, false)

	d.root = tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(leftCol, 0, 1, true).
		AddItem(rightCol, 0, 1, false)
	d.root.SetBorder(true).SetTitle(" tview Demo ").SetTitleAlign(tview.AlignLeft)
	d.root.SetBorderColor(tcell.ColorDarkCyan)

	return d
}

func tabCapture(ws *workspace, next tview.Primitive) func(*tcell.EventKey) *tcell.EventKey {
	return func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			ws.tviewApp.SetFocus(next)
			return nil
		}
		return event
	}
}

func (d *tviewDemoContent) typeName() string                            { return "tview-demo" }
func (d *tviewDemoContent) widget() tview.Primitive                     { return d.root }
func (d *tviewDemoContent) focusTarget() tview.Primitive                { return d.list }
func (d *tviewDemoContent) refresh()                                    {}
func (d *tviewDemoContent) handleEvent(event string, value string) bool { return false }
func (d *tviewDemoContent) setHighlight(mode highlightMode) {
	d.root.SetBorderColor(borderColorForMode(mode))
}
