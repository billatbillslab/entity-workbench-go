package main

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"entity-workbench-go/shellcmd"
	"entity-workbench-go/shellpanel"
	wb "entity-workbench-go/workbench"
)

// entityShellContent is the console renderer for the entity shell REPL.
// Business logic lives in shellpanel.ShellModel; this file is pure
// tview I/O.
type entityShellContent struct {
	root   *tview.Flex
	output *tview.TextView
	input  *tview.InputField
	model  *shellpanel.ShellModel

	// Track how many output lines we've rendered to avoid re-rendering all.
	renderedLines int
}

func newEntityShell(shWs *shellcmd.ShellWorkspace, publishWD func(prev, next shellcmd.Path)) *entityShellContent {
	per := shellcmd.NewShellInWorkspace(shWs)
	per.OnWDChanged = publishWD
	sh := &entityShellContent{
		model: shellpanel.New(per),
	}

	sh.output = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWordWrap(true)

	sh.input = tview.NewInputField().
		SetLabel(sh.model.Prompt()).
		SetFieldBackgroundColor(tcell.ColorBlack).
		SetLabelColor(tcell.ColorSkyblue)

	sh.input.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEnter:
			cmd := sh.input.GetText()
			if cmd != "" {
				sh.model.Submit(cmd)
				sh.input.SetText("")
				sh.input.SetLabel(sh.model.Prompt())
				sh.renderNewOutput()
				sh.output.ScrollToEnd()
			}
			return nil
		case tcell.KeyUp:
			if cmd, ok := sh.model.HistoryPrev(); ok {
				sh.input.SetText(cmd)
			}
			return nil
		case tcell.KeyDown:
			if cmd, ok := sh.model.HistoryNext(); ok {
				sh.input.SetText(cmd)
			}
			return nil
		}
		return event
	})

	sh.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(sh.output, 0, 1, false).
		AddItem(sh.input, 1, 0, true)
	sh.root.SetBorder(true).SetTitle(" Entity Shell ").SetTitleAlign(tview.AlignLeft)
	sh.root.SetBorderColor(tcell.ColorDarkCyan)

	// Render initial welcome message
	sh.renderNewOutput()

	return sh
}

func (sh *entityShellContent) typeName() string             { return "entity-shell" }
func (sh *entityShellContent) widget() tview.Primitive      { return sh.root }
func (sh *entityShellContent) focusTarget() tview.Primitive { return sh.input }
func (sh *entityShellContent) refresh()                     {} // command-driven, not event-driven
func (sh *entityShellContent) handleEvent(e, v string) bool { return false }
func (sh *entityShellContent) setHighlight(m highlightMode) {
	sh.root.SetBorderColor(borderColorForMode(m))
}

// renderNewOutput writes any new output lines from the model to the tview TextView.
func (sh *entityShellContent) renderNewOutput() {
	lines := sh.model.Render().Lines
	if len(lines) == 0 && sh.renderedLines > 0 {
		// Model was cleared
		sh.output.Clear()
		sh.renderedLines = 0
		return
	}
	for i := sh.renderedLines; i < len(lines); i++ {
		line := lines[i]
		fmt.Fprintf(sh.output, "%s%s[-]\n", tviewColorTag(line.Kind), tview.Escape(line.Text))
	}
	sh.renderedLines = len(lines)
}

// tviewColorTag returns the tview color tag for a ValueKind.
func tviewColorTag(kind wb.ValueKind) string {
	switch kind {
	case wb.KindNull:
		return "[gray]"
	case wb.KindBool:
		return "[purple]"
	case wb.KindString:
		return "[green]"
	case wb.KindNumber:
		return "[purple]"
	case wb.KindBytes:
		return "[teal]"
	case wb.KindHash:
		return "[blue]"
	case wb.KindKey:
		return "[yellow]"
	case wb.KindIndex:
		return "[gray]"
	case wb.KindError:
		return "[red]"
	case wb.KindPath:
		return "[skyblue]"
	default:
		return "[white]"
	}
}
