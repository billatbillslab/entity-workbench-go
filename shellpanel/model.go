package shellpanel

import (
	"entity-workbench-go/shellcmd"
	wb "entity-workbench-go/workbench"
)

// ShellModel is the renderer-neutral business logic of an embedded
// shell panel. It owns per-panel state (history, output scrollback),
// shares a *shellcmd.Shell with the canvas/console workspace, and
// dispatches commands via shellcmd.Registry — the same registry the
// standalone entity-shell binary drives.
//
// Per-panel: WD (via the held *Shell), History, HistIdx, Output.
// Shared (via shellcmd.ShellWorkspace embedded in *Shell): AppPeer,
// alias table, identity, workbench handler refs.
//
// Multi-shell deployments construct one *shellcmd.Shell per panel
// from the same workspace; each ShellModel wraps one Shell.
type ShellModel struct {
	sh  *shellcmd.Shell
	reg *shellcmd.Registry

	Output  []wb.OutputLine
	History []string
	HistIdx int
}

// New builds a panel model bound to an existing per-panel *shellcmd.Shell.
// The Shell's workspace is whatever the caller assembled — typically
// one workspace per process, one Shell per panel.
func New(sh *shellcmd.Shell) *ShellModel {
	m := &ShellModel{
		sh:  sh,
		reg: shellcmd.Default(),
	}
	m.appendLine("Entity shell — type 'help' for commands", wb.KindNull)
	m.appendLine("", wb.KindNull)
	return m
}

// Shell exposes the underlying shellcmd.Shell so the renderer can
// read sh.WD for prompt display and write WD when needed.
func (m *ShellModel) Shell() *shellcmd.Shell { return m.sh }

// Submit records line in history and runs it through the registry.
// Output is appended (the prompt echo first, then the rendered result
// or error). Empty submissions are no-ops.
func (m *ShellModel) Submit(line string) {
	if line == "" {
		return
	}
	m.History = append(m.History, line)
	m.HistIdx = len(m.History)
	m.execute(line)
}

// HistoryPrev returns the previous command in history.
func (m *ShellModel) HistoryPrev() (string, bool) {
	if m.HistIdx > 0 {
		m.HistIdx--
		return m.History[m.HistIdx], true
	}
	return "", false
}

// HistoryNext returns the next command in history (or empty string at
// the end, signalling "cleared input").
func (m *ShellModel) HistoryNext() (string, bool) {
	if m.HistIdx < len(m.History)-1 {
		m.HistIdx++
		return m.History[m.HistIdx], true
	}
	m.HistIdx = len(m.History)
	return "", true
}

// Clear empties the output buffer (matches the REPL's clear verb).
func (m *ShellModel) Clear() { m.Output = m.Output[:0] }

// OutputLen returns the number of buffered output lines.
func (m *ShellModel) OutputLen() int { return len(m.Output) }

// Render returns the renderer-neutral snapshot the panel paints.
func (m *ShellModel) Render() ShellOutput { return ShellOutput{Lines: m.Output} }

// Prompt returns the panel prompt for this shell's current WD,
// substituting peer aliases for peer-ids where available. Matches
// the shape used by shell/repl.go::prompt for parity with the
// standalone REPL.
func (m *ShellModel) Prompt() string {
	wd := m.sh.WD
	peerID := wd.PeerID()
	alias := ""
	if peerID != "" {
		alias = m.sh.AliasFor(peerID)
	}
	if alias != "" {
		bare := wd.BarePath()
		if bare == "" {
			return "entity:" + alias + ":/ > "
		}
		return "entity:" + alias + ":/" + bare + " > "
	}
	return "entity:" + string(wd) + " > "
}

func (m *ShellModel) execute(line string) {
	m.appendLine(m.Prompt()+line, wb.KindPath)

	args := shellcmd.SplitArgs(line)
	if len(args) == 0 {
		return
	}
	cmd := args[0]
	if cmd == "clear" {
		m.Clear()
		return
	}
	if cmd == "quit" || cmd == "exit" {
		m.appendLine("(panel shell does not exit; close the panel)", wb.KindNull)
		return
	}

	result, err := m.reg.Dispatch(m.sh, cmd, args[1:])
	if err != nil {
		m.Output = append(m.Output, RenderError(err))
		return
	}
	m.Output = append(m.Output, RenderResult(result)...)
}

func (m *ShellModel) appendLine(text string, kind wb.ValueKind) {
	m.Output = append(m.Output, wb.OutputLine{Text: text, Kind: kind})
}

// ShellOutput is the renderer-neutral output (matches the workbench
// model interface convention: each Model has its own Render() output
// type).
type ShellOutput struct {
	Lines []wb.OutputLine
}
