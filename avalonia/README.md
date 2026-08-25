# avalonia

The Avalonia (.NET) desktop renderer for entity-workbench-go — the
**primary** frontend and where feature work lands. It is a thin I/O
layer over a Go c-shared bridge carrying the same
`entitysdk` + `shellboot` + `shellcmd` stack the `entity-shell` REPL
uses. All business logic (entity resolution, formatting, tree and
selection state, the content models) lives renderer-neutral in
`workbench/`; nothing in `frontend/` reimplements it.

**Status:** shipped, not a spike. 14 panel types (17 registry entries),
95 bridge exports, headless
UI tests plus real-X11 smoke drivers per panel. The `console/` (tview)
renderer is kept frozen and single-peer as a discipline enforcer — it is
not a parity obligation, and Avalonia is expected to outpace it. (The
canvas/raylib renderer was removed.)

---

## Starting it

**From the repo root** — this is the main route, and it is the one to
document to a newcomer:

```bash
make doctor      # is this machine set up? (podman, sibling kernel, GUI image)
make gui         # build the image + extract + launch      <- first time
make gui-run     # launch what is already extracted        <- every time after
make gui-test    # headless UI tests
```

`make gui` rebuilds the container image before launching. That is the
correct choice after any Go or C# change and the wrong one when you just
want to look at the app: podman caches unchanged stages, but the first
build pulls the .NET SDK and takes minutes. `make gui-run` rebuilds
nothing (extracting `dist-native/` first if it is missing).

**The host never needs .NET.** Everything compiles inside podman; the
extracted `dist-native/` is a self-contained native binary. Do not
`dnf install dotnet`.

### Passing app flags

Both launch targets forward `ARGS` to the binary:

```bash
make gui-run ARGS="--identity me --storage sqlite"
make -C avalonia host-run ARGS="--listen 127.0.0.1:9000"
```

With no flags the GUI runs as an **ephemeral in-memory peer and loses
everything on exit** — fine for a look, wrong for real use. Pair
`--storage sqlite` with `--identity NAME`: without a named identity each
run generates a fresh keypair, so every launch writes into a different
namespace of the same database and nothing you wrote last time is
visible. `./entity-avalonia --help` (inside `dist-native/`) is the full
flag list.

### From `avalonia/` directly

```bash
make build      # multi-stage image (Go bridge + .NET publish + test stage)
make extract    # copy /app out of the image into ./dist-native/
make host-run   # launch (extracts if needed; does NOT rebuild)
make up         # build + extract + launch, output tee'd to dist-native/run.log
make test       # headless UI tests (Avalonia.Headless.XUnit) inside podman
make crash      # decode the most recent coredump's libbridge.so backtrace
make run        # run inside the container with X11 forwarded (cookie-fragile
                # on Wayland; host-run is the dev loop)
make clean      # remove dist-native/ and the local image
```

`make help` in this directory lists the smoke drivers too.

### Render mode

Software Skia is the **default** since 2026-08-19. Hardware GL
intermittently SIGSEGVs on some mesa drivers, and the paint path is
proven crash-free in software (the headless stress tests rasterize a
64×64 grid 400× with no GPU). Opt back in with `WB_GPU_RENDER=1`. The
mode is printed on the first line of every launch.

### When it crashes

`make up` tees stdout+stderr to `dist-native/run.log` and enables .NET
minidumps; on a non-zero exit it prints the last 40 lines and points at
`make crash`, which resolves the `libbridge.so` frames to Go
`file:line` (symbols are deliberately kept — no `-s -w` in the
Containerfile). Panel breadcrumbs (`WB_PANEL_LOG=1`, set by the launch
wrapper) are the forensic surface: the last line before the crash says
what was being rendered. See `docs/architecture/LOGGING-CONVENTIONS.md`.

---

## Layout

```
avalonia/
  bridge/          Go c-shared library (-buildmode=c-shared), 95 exports
    main.go        init/shutdown, peer lifecycle, tree + watch, dispatch
    peer_connections.go  connection pool (aliases you can dial/drop)
    liveness.go    system/peer/status — the TREE's lifecycle record
    discovery.go   mDNS "nearby peers"
    handlers.go    handler browser
    shell.go       shell panel dispatch
    site.go        site view
    program.go     the generic compute host — every program, one seam
  frontend/        Avalonia 11 app, plain code (no XAML)
    Program.cs     argv → BridgeConfig JSON → BridgeInit; render-mode switch
    MainWindow.cs  window chrome, peer tabs
    PeerView.cs    per-peer slot layout; implements IPanelHost
    Bridge.cs      P/Invoke surface (one DllImport per export)
    Panels/        14 panel types + PanelRegistry, PanelStack, PanelSlot
  tests/           Workbench.Headless.Tests (Avalonia.Headless.XUnit)
  Containerfile    multi-stage Fedora build (Go + .NET SDK + tester stage)
  Makefile         podman build / extract / run / smoke targets
  bridge_smoke.c   C runner that exercises the bridge without Avalonia
```

## Bridge contract

Every panel-bearing surface uses the same **handle lifecycle**, and new
ones should lift it rather than invent a shape:

| Call | Returns | Notes |
|---|---|---|
| `XOpen(peerHandle)` | `{ok,handle}` | Allocates bridge-side state; tagged with the peer handle for cascade-on-destroy |
| `XRegisterWake(h, cb)` | `{ok}` | `cb` is `void(*)(int64_t)`; a goroutine fans store events to it. The C# delegate **must** be GC-rooted (`GCHandle.Alloc`) for the handle's lifetime |
| `XRender(h)` | `{ok,result}` | Snapshot of the renderer-neutral model. Never triggers I/O the caller did not ask for |
| `XClose(h)` | — | Cancels subscriptions and **joins** the wake goroutine before returning, so the caller can free the delegate immediately after |

Cross-cutting rules:

- **Every string returned by the bridge must be freed** with
  `FreeString` — `Bridge.TakeString` does this on the C# side.
- **Watch callbacks must copy the JSON before returning**; Go frees it
  the moment the invoke returns.
- Errors are envelopes (`{"ok":false,"error":...}`), never panics:
  exports wrap `recoverToErrorEnvelope`.
- Handles are peer-scoped and auto-torn-down by the `OnPeerDestroyed`
  cascade in `BridgeInit`.

The authoritative export list is the source —
`grep '^//export ' bridge/*.go`.

### Two connection surfaces, deliberately

`PeerConnections*` reports the local connection **pool**: the aliases
this peer can dial or drop. `Liveness*` reports the **tree's** lifecycle
record (`system/peer/status`, EXTENSION-NETWORK §3.13) — the same view a
remote consumer or the reconnect graph sees. They are different sets and
are not expected to agree: a peer that dialed *us* appears only in
liveness, a connection evicted without a demotion leaves liveness saying
`connected`, and only liveness can say `suspect` or say *why* a peer went
away. `PeerConnectionsPanel` renders both, labelled separately. Neither
exposes `last_seen`: the status entity is transition-written (§5.4.1
MUST), so that stamp is not a freshness signal.

### Three views of a published site, deliberately

`BrowserPanel` is the **journey**: pin one name authority, walk it for the
names it carries, open one, and read a page — with the ten-step trust
chain in the right-hand column, showing the provenance of the bytes in
the middle column and no others. The address bar takes a name
(`docs.entitychurch.org/demo/index`) or a peer-id; history crosses
publishers.

**It is not a second Site Browser.** `entity-browser-rust` renders the
pages with trust in the chrome — the reader's browser, and the right
shape for a reader. This one keeps the chain **beside** the page because
its user is the person deciding whether to believe it. Two rules the
model enforces and the panel must not undo: a step that could not be
established is drawn **failing**, never omitted (a rail with six green
rows that stops looks green at a glance), and the rail is **cleared** when
a navigation starts (a stale chain beside fresh bytes is the exact lie
the surface exists to prevent).

Addressing a peer-id directly draws the six naming steps as `skip`, with
the reason — a browser that simply started at "target root" would render
a shorter, cleaner, equally-green chain for a materially weaker claim.

Its shape departures are `GUIDE-AVALONIA-PANEL-PATTERNS` §9's **P3″**:
per-operation single flight (enumerating and navigating do not block each
other; a second navigation is refused) and a monotonic completed-op
counter on the render envelope, because a derived UI property is not a
completion signal (AP32).

### Two views of the same origin, deliberately

`SiteViewPanel` renders a published site's **pages** — the reader's
surface, the same question `entity-browser-rust`'s Site Browser answers.
`PublisherVerifyPanel` renders the **verification chain** over the same
bytes: manifest → signature → CHAMP trie walk from the signed root →
leaves → enumerate → reconcile → absent control, each step with its
verdict *and what a green verdict on that step proves*.

**The `proves` line is the design.** Five of those seven steps are
satisfiable by an origin that is lying — an origin serving a
correctly-signed root that commits to nothing passes the layout, the
manifest, the signature and every per-leaf fetch anyone makes; only the
trie walk catches it. A UI that collapses the chain into one tick
teaches an operator that "verified" is one fact. It is seven, and one of
them (`published_at`) is a **moment**, which is why a green verdict is
always followed by the freshness bound and never by the bare word.

Two shape departures, both intentional (`GUIDE-AVALONIA-PANEL-PATTERNS`
§9, P3′): the wake is **operation-triggered** (once per run, on
completion — not a tree event, so nothing to debounce; the single-flight
guard sits on `VerifyStart` instead), and `VerifyOpen()` takes **no peer
handle**, because an EXTENSION-NETWORK §6.5.3 Mode A2 consumer is not a
peer.

## Validating the bridge without Avalonia

`bridge_smoke.c` dlopens `libbridge.so` and exercises the core exports —
useful when iterating on the Go half:

```bash
make extract
cd dist-native
gcc ../bridge_smoke.c -o /tmp/smoke -ldl
LD_LIBRARY_PATH=. /tmp/smoke
```

A compile-only check of the bridge (no container, no .NET):

```bash
cd bridge && CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/libbridge-test.so .
```

## Testing

Four tiers, per `docs/architecture/TESTING-STRATEGY.md`; naming the tier
is the discipline.

- **`make test`** — headless UI tests. No display, no GPU. Catches mount,
  wiring, parse, dispose, and re-entrancy faults.
- **`make smoke-xvfb…`** — the real binary under Xvfb with software Skia:
  real X11, real paint, real window. Per-panel drivers
  (`smoke-xvfb-handlers`, `-site`, `-life`, `-snake`, `-asteroids`,
  `-program PROGRAM=…`, `-hidpi`, `-window MODE=minimize|resize|both`).
  These catch what headless cannot.
- Model-level behavior belongs in `workbench/` Go tests, not here.

## Known gaps

- **Managed stack overflow on window minimize** under some compositors —
  characterized, not reproduced in-harness (three attempts; `ClientSize`
  did not collapse under openbox/Xvfb, which damaged the working
  hypothesis). `smoke-xvfb-window` is the gate that would catch it.
- **GPU-driver SIGSEGV** — product call made: software Skia default,
  `WB_GPU_RENDER=1` opts in. Not a code bug we own.
- **Cross-platform** — Linux is the only tested host. macOS/Windows are
  unvalidated, not unsupported-by-design.
