package shellcmd

import "fmt"

// Command is a registered shell command. Handler executes the
// command against the shell state and returns a renderer-neutral
// Result. Errors are returned to the caller and presented uniformly
// (REPL prints to stderr, one-shot mode exits non-zero, panel adapter
// emits a kind-Error OutputLine).
//
// ResolveArgs, if non-nil, is invoked by the dispatcher (Registry.Dispatch)
// between argument parsing and Handler dispatch — see
// GUIDE-SHELL-FRAMING.md §8.1 ("alias resolution happens at the
// dispatcher tier, between argument parsing and verb-parser dispatch;
// verb-parsers receive already-resolved paths and identifier-typed
// args"). The returned slice replaces args before Handler runs.
//
// Use PathArgs(positions...) to declare static path-arg positions
// (e.g., PathArgs(0) for `cd <path>`, PathArgs(0, 1) for
// `cp <src> <dst>`). Commands with flag-interleaved or sub-op-positioned
// paths (cat -d, tree -depth, exec, revision sub-ops) supply a
// custom resolver or leave nil and resolve in the handler.
//
// Handlers MAY call sh.Resolve on their args defensively; sh.Resolve
// is idempotent on already-resolved paths, so dispatcher-tier
// resolution + handler-tier resolution compose safely.
type Command struct {
	Name        string
	Usage       string
	Help        string
	Handler     func(sh *Shell, args []string) (Result, error)
	ResolveArgs func(sh *Shell, args []string) []string
}

// PathArgs returns a ResolveArgs callback that expands @alias (and
// legacy alias:) references at the given positional argument
// indexes. Indices out of range (e.g., optional args the user didn't
// supply) are skipped. Per GUIDE-SHELL-FRAMING.md §8.1, this is the
// minimum-metadata variant of dispatcher-tier alias resolution.
//
// Usage at Command registration:
//
//	r.Register(Command{Name: "cd", Handler: cmdCd, ResolveArgs: PathArgs(0)})
//	r.Register(Command{Name: "cp", Handler: cmdCp, ResolveArgs: PathArgs(0, 1)})
func PathArgs(positions ...int) func(sh *Shell, args []string) []string {
	return func(sh *Shell, args []string) []string {
		out := make([]string, len(args))
		copy(out, args)
		for _, i := range positions {
			if i < 0 || i >= len(out) {
				continue
			}
			out[i] = string(sh.Resolve(out[i]))
		}
		return out
	}
}

// Registry holds the dispatch table. The default registry is
// populated by init() with the v1 command set; tests and embedded
// uses may construct their own Registry.
type Registry struct {
	commands []Command
	byName   map[string]*Command
}

// NewRegistry creates an empty registry. Use Default() for the
// stock command set.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]*Command)}
}

// Register adds a command. Panics on duplicate name — the registry
// is constructed at init time and conflicts indicate a programming
// error, not a runtime condition.
func (r *Registry) Register(c Command) {
	if _, exists := r.byName[c.Name]; exists {
		panic("shellcmd: duplicate command " + c.Name)
	}
	r.commands = append(r.commands, c)
	r.byName[c.Name] = &r.commands[len(r.commands)-1]
}

// Lookup returns the command bound to name, or nil.
func (r *Registry) Lookup(name string) *Command {
	return r.byName[name]
}

// Commands returns the registered commands in registration order.
func (r *Registry) Commands() []Command {
	return r.commands
}

// Dispatch runs the named command against the shell. Returns
// (Result, nil) on success, (zero Result, error) on failure. Per
// GUIDE-SHELL-FRAMING.md §8.1, dispatcher-tier alias resolution
// runs here: when the looked-up Command declares ResolveArgs, those
// positions are expanded against the shell's alias table before the
// handler is invoked.
func (r *Registry) Dispatch(sh *Shell, name string, args []string) (Result, error) {
	c := r.Lookup(name)
	if c == nil {
		return Result{}, fmt.Errorf("unknown command: %s (type 'help' for commands)", name)
	}
	if c.ResolveArgs != nil {
		args = c.ResolveArgs(sh, args)
	}
	return c.Handler(sh, args)
}

var defaultRegistry *Registry

// Default returns the stock command registry. Includes all commands
// declared in this package: connect, disconnect, ls, cd, pwd, cat,
// tree, exec, info, help.
func Default() *Registry {
	return defaultRegistry
}

func init() {
	r := NewRegistry()
	r.Register(Command{
		Name:    "connect",
		Usage:   "connect <alias> <host:port>",
		Help:    "Connect to a remote peer and perform handshake.",
		Handler: cmdConnect,
	})
	r.Register(Command{
		Name:    "disconnect",
		Usage:   "disconnect <alias>",
		Help:    "Disconnect from a peer.",
		Handler: cmdDisconnect,
	})
	r.Register(Command{
		Name:    "ls",
		Usage:   "ls [path]",
		Help:    "List children at path (or current directory).",
		Handler: cmdLs,
	})
	r.Register(Command{
		Name:        "cd",
		Usage:       "cd <path>",
		Help:        "Change working directory. 'cd @alias' jumps to a peer's root.",
		Handler:     cmdCd,
		ResolveArgs: PathArgs(0),
	})
	r.Register(Command{
		Name:    "pwd",
		Usage:   "pwd",
		Help:    "Print working directory.",
		Handler: cmdPwd,
	})
	r.Register(Command{
		Name:    "cat",
		Usage:   "cat <path> [-diag]",
		Help:    "Display the entity bound at path. -diag emits the diagnostic-hash form.",
		Handler: cmdCat,
	})
	r.Register(Command{
		Name:    "tree",
		Usage:   "tree [path] [-depth N] [-v]",
		Help:    "Recursive tree listing. -v shows entity details.",
		Handler: cmdTree,
	})
	r.Register(Command{
		Name:    "exec",
		Usage:   "exec <handler> <op> [resource] [json-params]",
		Help:    "Execute a handler operation on the current peer.",
		Handler: cmdExec,
	})
	r.Register(Command{
		Name:        "put",
		Usage:       "put <path> <type> <json-data>",
		Help:        "Store an entity at path. Data is parsed as JSON; non-JSON treated as a literal string.",
		Handler:     cmdPut,
		ResolveArgs: PathArgs(0),
	})
	r.Register(Command{
		Name:        "rm",
		Usage:       "rm <path>",
		Help:        "Remove the entity binding at path.",
		Handler:     cmdRm,
		ResolveArgs: PathArgs(0),
	})
	r.Register(Command{
		Name:        "has",
		Usage:       "has <path>",
		Help:        "Report whether path is currently bound.",
		Handler:     cmdHas,
		ResolveArgs: PathArgs(0),
	})
	r.Register(Command{
		Name:        "cp",
		Usage:       "cp <src> <dst>",
		Help:        "Copy the entity at src to dst (preserves content hash; cross-peer-capable).",
		Handler:     cmdCp,
		ResolveArgs: PathArgs(0, 1),
	})
	r.Register(Command{
		Name:    "info",
		Usage:   "info [alias]",
		Help:    "Show connection details.",
		Handler: cmdInfo,
	})
	r.Register(Command{
		Name:    "peer",
		Usage:   "peer <ls|status|info|rename> [args]",
		Help:    "Peer-management surface. 'peer ls' lists this session's known peers; 'peer status' shows the tree's lifecycle record (connected/suspect/disconnected, and why); 'peer info' delegates to info; 'peer rename <old> <new>' retags an alias.",
		Handler: cmdPeer,
	})
	r.Register(Command{
		Name:  "name",
		Usage: "name <ls|resolve|bind|unbind|config> [args]",
		Help: "Local name book + name resolution (EXTENSION-REGISTRY §11.2). " +
			"'name bind <name> <@alias|peer-id>' makes a name resolve for THIS peer only; " +
			"'name resolve <name> [-pin <peer-id>]' walks the resolver chain and reports the rung it stops at; " +
			"'name config' shows which backends each name shape is eligible for.",
		Handler: cmdName,
	})
	r.Register(Command{
		Name:  "registry",
		Usage: "registry <pin|ls|show|resolve|unpin> [args]",
		Help: "Pin a name authority and browse what it carries (EXTENSION-REGISTRY §6a). " +
			"'registry pin <origin> -peer <id>' is the one fact you supply out of band — for an " +
			"identity-form peer-id the pin IS the key, so the host serving the bytes is trusted " +
			"for nothing. 'registry ls' WALKS the signed root for the name set (§6a.3a): a served " +
			"listing can hide a name undetectably, a walk cannot hide one without failing.",
		Handler: cmdRegistry,
	})
	r.Register(Command{
		Name:  "browse",
		Usage: "browse <open|back|forward|where|sites> [args]",
		Help: "Go somewhere by name and see what verified it. 'browse open <name>' runs the whole " +
			"chain — registry pin, binding signature, association, revocation, freshness, " +
			"transport, the target's own signed root, the trie walk, the page — and prints the " +
			"page with that chain under it. 'browse where' repeats the chain with what each step " +
			"proves. History crosses publishers.",
		Handler: cmdBrowse,
	})
	r.Register(Command{
		Name:  "open",
		Usage: "open <name>[/<site>[/<page>]] [-origin <url>]",
		Help:  "Alias for 'browse open' — the verb you type most.",
		Handler: func(sh *Shell, args []string) (Result, error) {
			return cmdBrowse(sh, append([]string{"open"}, args...))
		},
	})
	r.Register(Command{
		Name:  "site",
		Usage: "site verify <origin> [-peer <id>] [-keys] [-absent <key>] [-pin-* ...]",
		Help: "Verify a published origin over the CDN corridor: manifest -> signature -> " +
			"CHAMP trie walk from the signed root -> leaves. Reports the CHAIN (which link " +
			"held and what each proves), not the site's pages — the trie walk is the only " +
			"step a withholding origin cannot pass.",
		Handler: cmdSite,
	})
	r.Register(Command{
		Name:    "identity",
		Usage:   "identity <list|create|use|bootstrap> [args]",
		Help:    "Manage V7 identities + run the identity-aware bootstrap ceremony on the local peer.",
		Handler: cmdIdentity,
	})
	r.Register(Command{
		Name:    "role",
		Usage:   "role <define|assign|unassign|exclude|unexclude|re-derive> [args]",
		Help:    "Manage roles + assignments on the local peer.",
		Handler: cmdRole,
	})
	r.Register(Command{
		Name:    "revision",
		Usage:   "revision <commit|log|status|diff|find-ancestor|branch|tag|checkout|cherry-pick|revert|merge|resolve|config> [args]",
		Help:    "Manage versioned snapshots of tree subtrees on the local peer.",
		Handler: cmdRevision,
	})
	r.Register(Command{
		Name:    "history",
		Usage:   "history <config|query|rollback> [args]",
		Help:    "Per-path transition log + rollback. Recording is opt-in via 'history config <pattern>'.",
		Handler: cmdHistory,
	})
	r.Register(Command{
		Name:    "ingest",
		Usage:   "ingest tree <src-dir> <tree-prefix>",
		Help:    "Bulk-import a directory of markdown files into the entity tree, preserving folder structure.",
		Handler: cmdIngest,
	})
	r.Register(Command{
		Name:    "find",
		Usage:   "find <prefix> <substring> [-limit N]",
		Help:    "Substring path search across entities under prefix (case-insensitive).",
		Handler: cmdFind,
	})
	r.Register(Command{
		Name:    "query",
		Usage:   "query <prefix> [-type T] [-field F=V] [-limit N]",
		Help:    "Run system/query find under prefix. -type filters by entity type; -field F=V predicates a CBOR-encoded equality match.",
		Handler: cmdQuery,
	})
	r.Register(Command{
		Name:    "count",
		Usage:   "count <prefix> [-type T] [-field F=V]",
		Help:    "Cardinality companion to query — same filters, returns just the count.",
		Handler: cmdCount,
	})
	r.Register(Command{
		Name:    "grep",
		Usage:   "grep <prefix> <regex> [-i] [-l] [-context N]",
		Help:    "Content search across entities under prefix. -i case-insensitive, -l paths only, -context N for surrounding lines.",
		Handler: cmdGrep,
	})
	r.Register(Command{
		Name:    "continuation",
		Usage:   "continuation <ls|suspended|inspect|abandon|resume> [args]",
		Help:    "List + manage continuation entities. The ps-equivalent for the entity system's deferred processes.",
		Handler: cmdContinuation,
	})
	r.Register(Command{
		Name:    "subscription",
		Usage:   "subscription <ls|inspect|rm> [args]",
		Help:    "List + manage active subscriptions on the local peer.",
		Handler: cmdSubscription,
	})
	r.Register(Command{
		Name:        "tail",
		Usage:       "tail <path> [-n N] [-timeout DUR]",
		Help:        "Wait for the next N change events on path (or trailing * for prefix). Defaults: -n 1, -timeout 30s.",
		Handler:     cmdTail,
		ResolveArgs: PathArgs(0),
	})
	r.Register(Command{
		Name:    "mount",
		Usage:   "mount <fs-dir> <tree-prefix>",
		Help:    "Bridge a filesystem directory into a revision-tracked tree prefix. File changes propagate through the workbench's ingest chain into the entity tree.",
		Handler: cmdMount,
	})
	r.Register(Command{
		Name:    "unmount",
		Usage:   "unmount <root-name>",
		Help:    "Tear down a bridge installed via 'mount'. Idempotent.",
		Handler: cmdUnmount,
	})
	r.Register(Command{
		Name:    "mounts",
		Usage:   "mounts",
		Help:    "List currently-mounted filesystem bridges.",
		Handler: cmdMounts,
	})
	r.Register(Command{
		Name:    "compute",
		Usage:   "compute <subop> [args]",
		Help:    "Author + inspect compute expressions. Subops: show <path>, register <pattern> <expr-path>, aggregate <prefix>.",
		Handler: cmdCompute,
	})
	r.Register(Command{
		Name:        "inspect",
		Usage:       "inspect <entity|dump|find|errors|chain> [args]",
		Help:        "Diagnostic surface per GUIDE-INSPECTABILITY v1.1. Snapshot queries. Sub: entity <path>, dump <hash>, find <substr>, errors, chain <ls|show|errors>. chain ls enumerates discoverable chain_ids; chain show <id> walks one chain.",
		Handler:     cmdInspect,
		ResolveArgs: inspectResolveArgs,
	})
	r.Register(Command{
		Name:    "help",
		Usage:   "help [command]",
		Help:    "Show help.",
		Handler: cmdHelp,
	})
	defaultRegistry = r
}
