# Entity Workbench (Go)

A **reference application** built on the [Entity Core Protocol](https://github.com/EntityChurch/entity-core-protocol).
It demonstrates one coherent way to build an entity-native application — it is
**a reference paradigm, not a mandate**. The protocol requires none of these
shapes; the workbench is an opinionated, worked example you can learn from,
lift from, or ignore.

## Quickstart

```bash
git clone <this repo>                    # and its sibling, see "Repository layout"
cd entity-workbench-go

make doctor      # is this machine set up? names anything missing, exits non-zero on blockers
make build       # every Go binary -> ./bin
make demo        # a scripted tour of the CLI in a throwaway HOME (deleted on exit)
```

`make demo` is the fastest way to see whether this works on your machine: it
creates an identity, writes and reads a tree entity, binds and resolves a
name, and shows a name failing closed — all through the shipped
`entity-shell` binary, in a temp directory it removes on exit.

Then pick an interface:

```bash
make run                 # entity-shell REPL (the CLI)
make run ARGS="name ls"  # ...or a one-shot command
make gui                 # the Avalonia desktop app (podman; first build is slow)
```

And to check the tree's state:

```bash
make test-each   # EVERY Go suite to completion + a summary table
make gui-test    # Avalonia headless UI tests
```

> **Use `make test-each`, not `make test`, when you want to know where things
> stand.** `make test` is fail-fast: it stops at the first failing package, so
> a failure count read from it covers one package and is a lower bound.
> `test-each` runs all ten suites regardless, prints a pass/fail table, and
> leaves per-suite logs in `.test-logs/`.

`make help` lists everything.

## Requirements

| Need | For | Notes |
|---|---|---|
| `make` + `podman` | everything | The only hard requirements. `make build` / `make test` run inside a pinned `golang:1.25-bookworm` image. |
| sibling `../entity-core-go` | everything | **Required.** Every `go.mod` resolves the kernel through a local `replace`. Without it the build dies at module resolution. See [Repository layout](#repository-layout-sibling-dependency). |
| Go toolchain (host) | optional | Enables the faster `-native` targets (`make build-native`). The Makefile pins `GOTOOLCHAIN=go1.25.1`; never set it yourself. |
| .NET SDK | **never** | The Avalonia GUI builds entirely inside podman. Do not install dotnet on the host. |

**Platform: Linux is the only tested host.** The Go sources carry no
platform-specific code — no `syscall` use, no `golang.org/x/sys`, no
`//go:build linux` tags anywhere in the shipped modules, and the SQLite driver
(`modernc.org/sqlite`) is pure Go — so the CLI and TUI have no *known* reason
not to build elsewhere. That is an argument, not a measurement: **nothing but
Linux has been run.** The GUI is built and smoke-tested against X11/Xvfb inside
a Fedora-based container, so it is the least portable piece by construction.
`make doctor` warns on a non-Linux host rather than pretending. macOS and
Windows are unvalidated, not unsupported-by-design.

## Where this sits in the stack

```
entity-core-architecture   the protocol specification
        │
entity-core-go             the Go reference implementation (the kernel)
        │
entity-workbench-go        ← this repo: bindings / apps layer
```

Workbench **depends on** `entity-core-go` (core + ext). **Nothing depends on
the workbench** — it is a leaf. It ships:

- **`entitysdk/`** — a Go SDK over the kernel (typed wrappers, storage,
  identity bundles, revision/continuation helpers).
- **`shell/` + `shellcmd/`** — `entity-shell`, the primary CLI (REPL +
  one-shot) for peer / identity / capability / tree management.
- **`console/`** — `entity-console`, a TUI renderer (tcell + tview, pure Go,
  no CGo). Kept as a frozen discipline-enforcer.
- **`avalonia/`** — the primary desktop GUI (Avalonia/.NET, driven by a Go
  c-shared bridge). Built and tested in a podman container.
- **`workbench/` + `shellboot/` + `shellpanel/`** — the shared,
  renderer-neutral application library the frontends wire into.
- **CDN corridor** — `entity-publish`, `entity-vcs`, `entity-fetch`: small
  binaries for publishing / fetching a peer's tree over HTTP.

For design orientation start with `docs/architecture/` — the canonical surface
is enumerated in [`CANONICAL-DOCS.toml`](CANONICAL-DOCS.toml) (the discipline
charter, the Avalonia runtime model, the panel patterns, and the testing +
logging conventions, plus the usage guides).

---

## Build & run — everything goes through `make` + `podman`

`make` is the whole interface; `make help` is the index. The table below is
the full set most people need.

| Command | Does |
|---|---|
| `make doctor` | Check prerequisites, the sibling kernel, and the GUI image |
| `make build` | Every shipped Go binary → `./bin` (in-container) |
| `make run` | Build + start `entity-shell` (REPL); `ARGS="…"` for one-shot |
| `make gui` | Build + launch the Avalonia desktop app (podman) |
| `make demo` | Scripted CLI tour in a throwaway HOME |
| `make test-each` | Every Go suite **to completion** + summary table |
| `make test` | Full `-race` sweep — **fail-fast**, stops at the first failure |
| `make gui-test` | Avalonia headless UI tests (`Avalonia.Headless.XUnit`) |
| `make lint` / `make fmt` | `go vet` (read-only) / `gofmt -w` (writes) |
| `make check` | `lint` + `test` — the green gate |
| `make clean` | Remove build outputs |

The GUI targets are thin passthroughs to `avalonia/Makefile`, which has its own
richer surface — in particular the X11 smoke drivers that exercise a panel in a
real window under Xvfb (`make -C avalonia smoke-xvfb-handlers`, `…-life`,
`…-window`, …). Those catch what headless cannot; see
`docs/architecture/TESTING-STRATEGY.md`.

Add `-native` to run a Go target on a host toolchain instead of in the
container (`make build-native`, `make test-each-native`).

### Identity and storage

`entity-shell`, `entity-console`, and the Avalonia frontend share the same
flags:

```bash
entity-shell   -identity peerA -storage sqlite   # path defaults to ~/.entity/peers/peerA/store.db
entity-console -identity peerA -storage sqlite
```

> **Use `-identity` whenever you use `-storage sqlite`.** Without a named
> identity the shell generates a **fresh keypair per invocation**, so each run
> writes into a different namespace of the same database and nothing you wrote
> last time is visible. Everything still "works" — it just silently doesn't
> persist. (Known rough edge; see `docs/status/STATUS.md`.)

Name resolution is on by default (`EXTENSION-REGISTRY`), which is what the
`name` verb needs — `name bind` / `resolve` / `ls` / `config`. Turn it off with
`-disable-registry`.

Markdown and other files enter the workbench via the shell's `mount` verb,
which bridges a filesystem directory to a tree prefix; GUI edits round-trip
back to disk. See `docs/architecture/USAGE-SHELL.md` and
`docs/architecture/USAGE-PROTOTYPE-FILESYSTEM-SYNC.md`.

### Resource caps (podman)

Every podman build/run is fenced with hard memory ceilings (zero swap) so a
build cannot take the host down. The committed defaults live in
[`caps.mk`](caps.mk) (memory sized to the Avalonia build's measured peak +
headroom). Override per machine **without editing the tracked file** via an
env var (`CAP_MEM=8g make -C avalonia build`) or a gitignored `caps.local.mk`.

---

## Repository layout (sibling dependency)

This repo currently builds against `entity-core-go` as a **sibling directory**:

```
entity-systems/
├── entity-workbench-go/        ← this repo
└── entity-core-go/             ← required sibling (core/ + ext/)
```

Each `go.mod` requires the kernel by its canonical module path
(`go.entitychurch.org/entity-core-go/{core,ext}`) and resolves it through a
local `replace` to `../../entity-core-go/{core,ext}`. This is the **in-between
zone**: the published vanity module path is wired up, but resolution is still
local — no network fetch, no tag required. The final cutover (when the vanity
path is actually published) is a one-line change per module: drop the
`replace`, and the existing `require … @v0.8.0` fetches from the network.

If `../entity-core-go/` is missing, the build fails at module resolution.

The repo-root `go.work` composes the in-repo modules for editor / language
server convenience; cross-repo resolution to the kernel happens via the
per-`go.mod` `replace` directives above.

---

## Versioning

Module / app version is **0.8.0** (preview). The repo is not git-tagged yet —
tagging is a freeze action reserved for the release cut.

---

## Supporting the project

This project is developed in the open. If it's useful to you, the best support is
to use it, report issues, and contribute back — see
[CONTRIBUTING.md](CONTRIBUTING.md).

To support the work directly, see the project's funding page.
