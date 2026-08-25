# Entity Shell — Usage

**Owner:** workbench team. **Companion to:** `SHELL-DIRECTION.md` (design).

This doc shows how to run the shell and walks through common
scenarios. The design rationale lives in SHELL-DIRECTION; this is
purely the user-facing playbook.

---

## 1. Running the shell

The shell is in `shell/cmd/entity-shell`. There are two ways to run
it; prefer the first during development.

### 1.1 From source (recommended during development)

```sh
make shell                  # interactive REPL
make shell ARGS="ls"        # one-shot
make shell ARGS="info"      # one-shot, alternate command
make shell ARGS="--json info"  # JSON output (one-shot only)
make shell-help             # show registered commands
```

`make shell` invokes `go run ./shell/cmd/entity-shell`, so the binary
is always built fresh from the current source tree — no risk of
running stale code. `ARGS` is forwarded verbatim, so you can pass
flags and command args.

To run the test suite covering the shell + SDK:

```sh
make shell-test
```

### 1.2 Building a binary (when you actually want one)

```sh
cd shell/cmd/entity-shell
go build -o ../../../bin/entity-shell .
../../../bin/entity-shell    # then invoke directly
```

Use this only when you need a deployable binary. For day-to-day
development, `make shell` is faster and never stale.

### 1.3 Flags

```
entity-shell [flags] [<command> [args]]

  -identity NAME      Use a stored identity from ~/.entity/identities/
                      (flat keypair or directory bundle — auto-
                      detected). Omit for an ephemeral keypair per
                      invocation.
  -alias NAME         Local peer alias in the shell (default "local").
  -json               Emit results as JSON. Only meaningful in
                      one-shot mode (REPL output is always text).
  -storage KIND       Storage backend — "memory" (default) or
                      "sqlite".
  -storage-path PATH  SQLite DB path. Required when
                      -storage=sqlite. Use ":memory:" for an
                      in-process SQL DB that disappears on exit.
```

No args → REPL. With args → one-shot dispatch, exits after the
single command.

#### Persistent peers

By default the local peer is in-memory: every `entity-shell`
invocation starts with an empty tree. To keep state across
invocations:

```
entity-shell -identity peerA \
             -storage sqlite \
             -storage-path ~/.entity/peers/peerA/peer.db
```

The DB file is created on first use and reopened on subsequent
runs. Pair this with `-identity NAME` so the peer-id stays stable
across restarts. The tree is a universal namespace and every peer
operates under its own peer-id subtree — a fresh ephemeral keypair
on each launch would start as a different peer with an empty view
of the same DB. The previous peer's data is still there, but from
the new peer's perspective it's just someone else's namespace.

### 1.4 REPL ergonomics

The REPL uses `peterh/liner` for line editing when stdin is an
interactive TTY:

- **Arrow keys** — left/right move within the line; up/down scroll
  history.
- **Home/End** — jump to start/end of the line.
- **Tab** — context-aware completion:
  - At the start of a line: completes the command verb
    (`c<TAB>` → `cat / cd / connect / cp`).
  - On a path argument: lists the directory the user is mid-typing
    into via live `AppPeer.List`. Works for local and remote peers
    (one dispatched listing per TAB; no background fetching).
    Completes alias prefixes at the shell root (`/lo<TAB>` →
    `/local/`), absolute paths (`ls /local/sys<TAB>` →
    `ls /local/system/`), alias-relative
    (`cat local:system/han<TAB>` → `cat local:system/handler/`),
    and WD-relative after `cd alias:`.
- **Ctrl+C** — abort the current line (clean break).
- **Ctrl+D / `exit` / `quit`** — exit the shell.
- **Persistent history** — saved to `~/.entity/shell/history`
  across sessions (best-effort; falls back to in-memory if the
  directory can't be created).

When stdin isn't a TTY (e.g. `printf '...' | make shell`), the REPL
falls back to a simple line scanner — handy for scripted tests, no
escape-code handling needed.

---

## 2. Quick tour — local-only

The shell starts with one local in-process peer. You can poke at it
without connecting to anything else.

```
$ make shell
> info
Alias:   local
Address: (local)
PeerID:  2KZXq14M2rniz...

> ls
  local        (local)

> cd local:
> ls
  system        dir+entity
  …

> tree -depth 2
  system/
    capability/
    handler/
    type/
    …

> put scratch/note text/scalar '"hello"'
put /2KZXq14M2rniz.../scratch/note [text/scalar] → 8b…

> cat scratch/note
[text/scalar] "hello"

> has scratch/note
yes — /2KZXq14M2rniz.../scratch/note exists

> rm scratch/note
removed /2KZXq14M2rniz.../scratch/note
```

The local peer's tree is in-memory; everything is gone when you exit
the shell. Persistence (`--persistent`) is on the roadmap.

---

## 3. Connecting to a remote peer

You need a peer to connect to. The simplest one is core-go's
`entity-peer` running with open access:

```sh
# In another terminal, in entity-core-go/cmd/entity-peer/:
go run . --addr :9002 --open-access
```

Now from the shell:

```
> connect serv 127.0.0.1:9002
connected to serv (127.0.0.1:9002, peer-id 2KShTH9eAMBxoG...)

> ls
  local        (local)
  serv         127.0.0.1:9002

> cd serv:
> ls
  system        dir+entity
  …

> tree system/handler -depth 2
  system/
    handler/
      …

> disconnect serv
disconnected from serv
```

`connect` performs a real TCP handshake and caches the connection in
the local peer's pool. Subsequent commands targeting `serv:` reuse
that connection — no reconnect overhead.

---

## 4. The two ways to address remote paths

Both forms work everywhere the shell takes a path:

### 4.1 Alias-relative

```
> ls serv:system/handler          # serv is the alias
> cat serv:system/handler/system/tree
> put serv:demo/x  test/scalar  '42'
> rm  serv:demo/x
```

### 4.2 Working-directory-relative (after `cd`)

```
> cd serv:                         # WD = /{servPeerID}/
> ls system/handler                # relative to serv
> cat system/handler/system/tree
> put demo/x test/scalar '42'
> rm demo/x
```

The shell's working directory carries the peer-id, so plain
relative paths route to whatever peer you `cd`'d into.

### 4.3 Peer-qualified (low-level)

```
> ls /2KShTH9eAMBxoG.../system/handler   # full peer-id segment
> cat entity://2KShTH.../system/handler/system/tree
```

These work but you'll rarely want to type a peer-id. Aliases exist
so you don't have to.

---

## 5. Cross-peer copy

`cp` moves an entity between peers — the source can be remote and
the destination local, or vice versa, or both ends remote. Content
hash is preserved (no re-encoding):

```
> connect serv 127.0.0.1:9002

> put serv:demo/golden test/scalar '"important"'
put /2KShTH.../demo/golden [test/scalar] → 8b…

> cp serv:demo/golden local:archive/golden
copied /2KShTH.../demo/golden → /2KZXq14.../archive/golden [test/scalar] (8b…)

> cat local:archive/golden
[text/scalar] "important"
```

The copy goes through the shell's local AppPeer dispatcher: `Get`
fetches via the pool from `serv`, then `PutEntity` writes locally.
The same flow works the other direction (`local:foo` →
`serv:bar`) and between two remote peers (cross-peer-cross-peer is
two pool dispatches).

---

## 6. One-shot mode and JSON output

For scripting:

```sh
make shell ARGS="info"
make shell ARGS="--json info"
```

In one-shot mode the shell starts an ephemeral peer, runs the single
command, and exits. Use `--json` for machine-readable output —
useful in pipes:

```sh
make shell ARGS="--json info" | jq .peer_id
```

Note: one-shot mode doesn't currently persist a connection across
invocations (the peer is ephemeral). For multi-step interactions
with a remote, run the REPL.

---

## 7. The `exec` escape hatch

For handler operations the shell doesn't have a dedicated verb:

```
> exec system/tree get demo/path
> exec system/clock now
> exec system/some-extension custom-op  '{"key": "value"}'
```

`exec <handler> <op> [resource] [json-params]` dispatches against
the current working directory's peer. Use it to talk to extensions
or to operations that haven't been wrapped in a higher-level command.

---

## 8. Two-shell demo (try it)

To see remote dispatch end-to-end on one machine, run two terminals:

**Terminal A** — the listening peer:

```sh
cd entity-core-go/cmd/entity-peer
go run . --addr :9002 --open-access
```

**Terminal B** — the shell:

```
> connect a 127.0.0.1:9002
connected to a (127.0.0.1:9002, peer-id …)
> cd a:
> tree -depth 2
…
> put demo/value test/scalar '"world"'
> cat demo/value
> disconnect a
```

You can also run two `entity-peer`s on different ports, connect to
both from one shell, and `cp` between them.

---

## 9. Path forms reference

Every command that takes a path accepts these. They're equivalent
representations of the same thing.

| Form | Example | Meaning |
|---|---|---|
| Shell root | `/` | Lists connected peers; not a target for ops. |
| Alias-prefix (peer root) | `local:` | Jump to peer's root via alias. |
| Alias-prefix (subpath) | `local:foo/bar` | Path inside a peer via alias. |
| Absolute via alias | `/local/foo/bar` | Same. Alias appears as first segment. |
| Absolute via peer-id | `/{peerID}/foo/bar` | Same, with literal peer-id. |
| Relative | `foo/bar` | Only when WD is inside a peer (after `cd alias:`). |
| Parent | `..` | One segment up. |

Unknown alias names produce a clean error downstream
("no connection for path /unknown/…") rather than being silently
treated as a peer-id.

---

## 10. Known limitations (today)

- **`put`'s payload must be single-quoted.** `SplitArgs` strips `"` and `'` as *shell*
  quoting, so an unquoted JSON object arrives with its string quotes gone. Write
  `put P T '{"a":1}'`. Since 2026-08-21 `put` **refuses** a payload that opens with `{` or
  `[` and does not parse, rather than storing it as a literal string — the old behaviour
  wrote an entity that decoded nowhere and surfaced as the *consumer's* bug an hour later.

- Remote peers configured with non-open grants will reject the
  shell's ephemeral keypair at handshake. Pass `-identity NAME`
  with a named identity authorized on the remote, or use
  `--open-access` on dev peers.
- One-shot mode spins up a fresh peer per invocation — no
  connection caching across `make shell ARGS="…"` calls. Pair with
  `-storage sqlite -storage-path …` if you need state to persist.

---

## 10a. Browsing a published federation

The shell reads the CDN corridor two ways, and they answer different questions.

**`site verify <origin>`** is the **inspector**: you already know an origin and you want to
know whether it is serving what it signed. It renders the verification chain and never a
page.

**`registry` + `browse`** is the **journey**: you hold one pinned peer-id and end up looking
at a page, with the chain printed under it.

```
registry pin <origin> -peer <id> [-pin-* …]   trust a name authority
registry ls                                   the names it carries (WALKS the signed root)
registry show                                 what is pinned, and how fresh
registry resolve <name>                       one name, every §6a.4 check reported
registry unpin

open <name>[/<site>[/<page>]]                 go there  (alias for `browse open`)
browse back / forward                         history, across publishers
browse where                                  what is on screen and what verified it
browse sites                                  every site the target's SIGNED ROOT commits to
```

And the other direction — **being** a registry (§6a.8 curated registration; this peer's own
key signs, so this peer's peer-id is what a consumer pins):

```
registry issue <name> <target-peer-id> -target-origin <url> [-ttl 720h]
registry revoke <binding-hash> [-reason ...]
```

`-target-origin` is **fetched**, and the binding carries the profile the target itself
advertises. A registry asserting a reach it read is asserting something it checked; one
asserting a reach it composed is asserting something it invented, and a consumer cannot tell
those apart. `issue` refuses if the origin advertises a different peer than you named.

Then publish it:

```
entity-publish -prefix system/ -out ./reg -origin https://your-host/reg
```

**The prefix is `system/`, not `system/registry/`.** A binding's signature lives at
`system/signature/{hex}` (V7 §5.2's invariant pointer), which is outside every
`system/registry/…` prefix — so the narrower publish emits bindings with no signatures over
them, enumeration works, and every resolve 404s at the signature step. Measured; routed to
arch as a defect in §6a.3a's recommended prefix.

Worked example, against the frozen cross-impl federation in the tree:

```bash
python3 -m http.server 8731 --directory fetch/testdata/crossimpl-rust-federation &
P=2KBLkCxvkgobuauPA6zPfKarpuRRnnWHL98n8Gv1GNmybr
make run                       # a REPL: the pin is workspace state, so one-shot won't do
```

```
entity:/ > registry pin http://127.0.0.1:8731 -peer $P \
             -pin-tree /registry/$P -pin-content /registry/content \
             -pin-manifest /registry/$P/system/peer/published-root -pin-layout sharded-2-4
entity:/ > registry ls
entity:/ > open docs.entitychurch.org
```

**Three things the output is trying to teach, and they are not decoration:**

- **The name list comes from a WALK of the registry's signed root**, not from the listing the
  origin serves. A hostile origin can drop a name from a listing undetectably; it cannot drop
  a node from a walk without the walk failing (`EXTENSION-REGISTRY` §6a.3a). When the two
  disagree, both are shown and the uncommitted row is marked `!!`.
- **Every step of the chain prints what a green verdict on *that step* proves.** Most of them
  are satisfiable by an origin that is lying — only the trie walk is not.
- **Nothing is ever reported as plain "verified".** A green chain is *verified as of* the
  publisher's `published_at`, because a quiet publisher and a withholding origin are
  indistinguishable from a consumer.

`-pin-*` is needed only when the origin serves no `transport-profile`, which is **conformant**
(`EXTENSION-NETWORK` §6.5.4 makes profile distribution out-of-band in v1) — and the shell says
which mode it ran in, because a wrong pin and a withholding origin look identical from here.

---

## 11. Cheat sheet

```
ls [path]                    list children (or peers at root)
cd [path]                    change working directory
                             cd alias:        → jump to peer's root
                             cd alias:foo/bar → jump into a subpath
                             cd ..            → go up
                             cd /             → back to shell root
pwd                          print working directory
tree [path] [-depth N] [-v]  recursive listing
cat <path> [-diag]           display the entity at path
exec <handler> <op> [resource] [json-params]
                             dispatch a handler operation
put <path> <type> <json>     store an entity
rm <path>                    remove a binding
has <path>                   yes/no
cp <src> <dst>               copy entity (cross-peer-capable)
registry pin <origin> -peer <id>
                             trust a name authority (see §10a)
registry ls                  the names it carries — from the signed-root WALK
registry issue <name> <peer> -target-origin <url>
                             mint + sign a binding (be a registry)
open <name>[/site[/page]]    go there, and print the chain that verified it
browse back|forward|where|sites
site verify <origin>         the inspector: the chain, not the page
connect <alias> <host:port>  open a session to a remote peer
disconnect <alias>           close & evict from pool
info [alias]                 connection details
help [command]               show usage
```
