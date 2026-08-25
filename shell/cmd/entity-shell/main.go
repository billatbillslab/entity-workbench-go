package main

import (
	"flag"
	"fmt"
	"os"

	"entity-workbench-go/shell"
)

// version is the build-time-stamped binary version. The Makefile's
// `shell` and `shell-once` targets inject this via:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// When the binary is built without that flag (e.g. plain `go run`),
// the placeholder below is what `-version` reports. That's a clear
// signal "this is a dev build, not a release."
var version = "0.8.0-dev (unstamped)"

const usage = `Usage:
  entity-shell [flags]                  Start interactive REPL
  entity-shell [flags] <command> [args] Run one command and exit

Flags:
  -identity NAME      Use named identity from ~/.entity/identities/
  -alias NAME         Alias for the in-process peer in the shell
                      (default: the -identity name, or "self" if none).
                      The term "local" is reserved for the local/*
                      extension namespace and is not a peer alias.
  -json               Emit results as JSON (one-shot mode only)
  -storage KIND       Storage backend: "memory" (default) or "sqlite"
  -storage-path PATH  SQLite DB path. When -storage=sqlite and -identity
                      NAME is set, defaults to ~/.entity/peers/NAME/store.db
                      per GUIDE-PERSISTENCE.md §1.1. Use ":memory:" for an
                      in-process SQL DB. Required when sqlite is used
                      without -identity.
  -listen ADDR        TCP listener for inbound peer connections,
                      e.g. ":9100" or "127.0.0.1:9100". When empty
                      the peer is local-only (no inbound dial).
  -open-access        DEVELOPMENT FLAG: grant all connecting peers
                      wildcard capabilities. Production peers should
                      configure scoped grants via the role extension.
                      Required for the prototype multi-peer guide.
  -disable-registry   Turn OFF name resolution (EXTENSION-REGISTRY).
                      On by default; the 'name' verb needs it. Costs
                      +8 paths once, nothing per restart.
  -version            Print the binary version and exit

Run 'entity-shell help' inside the REPL for command details.
`

func main() {
	identity := flag.String("identity", "", "identity name from ~/.entity/identities/ (default: ephemeral)")
	alias := flag.String("alias", "", "alias for the in-process peer (default: identity name, else \"self\")")
	jsonOut := flag.Bool("json", false, "emit results as JSON (one-shot mode only)")
	storage := flag.String("storage", "", "storage backend (memory, sqlite)")
	storagePath := flag.String("storage-path", "", "path to the SQLite DB (use \":memory:\" for in-process SQL)")
	listenAddr := flag.String("listen", "", "TCP listener address for inbound connections (empty = no listener)")
	openAccess := flag.Bool("open-access", false, "DEV: grant wildcard capabilities to all connecting peers")
	disableRegistry := flag.Bool("disable-registry", false, "turn off the EXTENSION-REGISTRY name-resolution substrate (the `name` verb needs it)")
	showVersion := flag.Bool("version", false, "print the binary version and exit")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if *openAccess {
		fmt.Fprintln(os.Stderr, "entity-shell: WARNING — running with -open-access; all connecting peers receive wildcard capabilities (dev only)")
	}

	app, err := shell.New(shell.Config{
		Identity:    *identity,
		LocalAlias:  *alias,
		JSON:        *jsonOut,
		StorageKind: *storage,
		StoragePath: *storagePath,
		ListenAddr:  *listenAddr,
		OpenAccess:  *openAccess,

		DisableRegistry: *disableRegistry,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "entity-shell: %v\n", err)
		os.Exit(1)
	}
	defer app.Close()

	args := flag.Args()
	if len(args) == 0 {
		if err := app.RunREPL(os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "entity-shell: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := app.RunOnce(args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "entity-shell: %v\n", err)
		os.Exit(1)
	}
}
