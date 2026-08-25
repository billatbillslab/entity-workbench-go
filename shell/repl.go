package shell

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/peterh/liner"

	"entity-workbench-go/shellcmd"
)

// RunREPL drives the interactive read-eval-print loop. When in is an
// interactive terminal it uses liner for line editing (Home/End,
// arrow keys, history); otherwise it falls back to a bufio.Scanner so
// piped or scripted input still works.
//
// Returns nil on EOF or explicit `quit`/`exit`; returns an error only
// on I/O trouble.
func (a *App) RunREPL(in io.Reader, out, errOut io.Writer) error {
	fmt.Fprintln(out, "Entity Shell — type 'help' for commands")
	if a.cfg.Identity != "" {
		fmt.Fprintf(out, "Identity: %s\n", a.cfg.Identity)
	}
	fmt.Fprintf(out, "Local peer: %s (peer-id %s)\n", a.sh.Local.Alias, shortID(a.sh.Local.PeerID))

	if useLiner(in) {
		return a.runLiner(out, errOut)
	}
	return a.runScanner(in, out, errOut)
}

// useLiner reports whether the input reader is the controlling TTY's
// stdin. We only enable liner in that case — piped or programmatic
// input goes through the simpler scanner path.
func useLiner(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	if f.Fd() != os.Stdin.Fd() {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (a *App) runLiner(out, errOut io.Writer) error {
	line := liner.NewLiner()
	defer line.Close()

	line.SetCtrlCAborts(true)

	// Persist history at ~/.entity/shell/history. Best-effort: if the
	// directory can't be created, history just lives for this session.
	histPath, err := historyFilePath()
	if err == nil {
		if f, err := os.Open(histPath); err == nil {
			_, _ = line.ReadHistory(f)
			f.Close()
		}
	}

	// Tab completion: command verbs at the start of the line, then
	// path arguments (live AppPeer.List against the dir the user is
	// mid-typing into). See complete.go.
	line.SetCompleter(a.completer)

	for {
		input, err := line.Prompt(prompt(a.sh))
		if err != nil {
			if errors.Is(err, liner.ErrPromptAborted) || errors.Is(err, io.EOF) {
				fmt.Fprintln(out)
				break
			}
			return err
		}
		text := strings.TrimSpace(input)
		if text == "" {
			continue
		}
		line.AppendHistory(text)

		args := shellcmd.SplitArgs(text)
		if len(args) == 0 {
			continue
		}
		cmd := args[0]
		if cmd == "quit" || cmd == "exit" {
			break
		}

		result, err := a.reg.Dispatch(a.sh, cmd, args[1:])
		if err != nil {
			fmt.Fprintf(errOut, "error: %v\n", err)
			continue
		}
		FormatText(out, result)
	}

	if histPath != "" {
		if f, err := os.Create(histPath); err == nil {
			_, _ = line.WriteHistory(f)
			f.Close()
		}
	}
	return nil
}

func (a *App) runScanner(in io.Reader, out, errOut io.Writer) error {
	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, prompt(a.sh))
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		args := shellcmd.SplitArgs(text)
		if len(args) == 0 {
			continue
		}
		cmd := args[0]
		if cmd == "quit" || cmd == "exit" {
			return nil
		}
		result, err := a.reg.Dispatch(a.sh, cmd, args[1:])
		if err != nil {
			fmt.Fprintf(errOut, "error: %v\n", err)
			continue
		}
		FormatText(out, result)
	}
	return scanner.Err()
}

// historyFilePath returns ~/.entity/shell/history, creating the
// containing directory if needed. Returns an empty path and an error
// if the home directory can't be determined.
func historyFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".entity", "shell")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "history"), nil
}

// RunOnce runs a single command then returns. argv is the command
// followed by its arguments (no flags). Output goes to out (text or
// JSON depending on App.cfg.JSON), errors to errOut.
func (a *App) RunOnce(argv []string, out, errOut io.Writer) error {
	if len(argv) == 0 {
		return fmt.Errorf("no command given")
	}
	cmd := argv[0]
	result, err := a.reg.Dispatch(a.sh, cmd, argv[1:])
	if err != nil {
		return err
	}
	if a.cfg.JSON {
		return FormatJSON(out, result)
	}
	FormatText(out, result)
	return nil
}

// prompt renders the REPL prompt based on the current working
// directory, substituting peer aliases for peer-ids when available.
func prompt(sh *shellcmd.Shell) string {
	wd := string(sh.WD)
	peerID := sh.WD.PeerID()
	alias := ""
	if peerID != "" {
		alias = sh.AliasFor(peerID)
	}
	if alias != "" {
		bare := sh.WD.BarePath()
		if bare == "" {
			return fmt.Sprintf("entity:%s:/ > ", alias)
		}
		return fmt.Sprintf("entity:%s:/%s > ", alias, bare)
	}
	return fmt.Sprintf("entity:%s > ", wd)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "..."
	}
	return id
}
