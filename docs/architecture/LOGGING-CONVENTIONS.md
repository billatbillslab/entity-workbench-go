# Logging conventions — entity-workbench-go

Canonical. Living doc — edit in place.

Logging is **diagnostic discipline**, not verbosity. The crash-hunt
included one defect (`8b3963e`, AP7) where a stack overflow left no
managed dump, no useful trace, and we got nothing — until we added the
pre-crash breadcrumb that names the dying op. This doc captures that
pattern as the project's logging convention.

Three audiences:

1. **The future debugger** (Claude, or a human) reading stderr after a
   crash. Wants: the last successful operation named.
2. **The test framework** dumping logs on failure. Wants: categorized
   output it can filter.
3. **The contributor** turning on verbose mode to understand a flow.
   Wants: stable category names + meaningful levels.

---

## 1. The two surfaces

We log in two places, with two different purposes:

### Avalonia frontend — `PanelLog` (C#)

Per-panel breadcrumb. Stderr-only, opt-in via `WB_PANEL_LOG=1`. Flushed
every line. **Last-line-before-crash is the entire reason this exists.**

Reference: `avalonia/frontend/Panels/PanelLog.cs`.

```csharp
PanelLog.Write("markdown-view", $"SwapBody h={_handle} bytes={n}");
```

### Go bridge — `wb/` logging (Go)

Structured event log going through the entitysdk's `EventLog` / log
filter model. Categories + levels + per-peer routing. Visible in the
LogViewer panel when running interactively; goes to stderr in headless
/ Xvfb runs.

Reference: `wb/logging.go` (or wherever `LogFilterModel` lives — used
by the Avalonia `LogOpen` / `LogRender` bridge surface).

The two surfaces serve different needs and should not be conflated.
`PanelLog` is the **forensic breadcrumb**; the structured `EventLog`
is the **observable runtime**.

---

## 2. Five levels

Semantic, not numeric. The level is **what the message means**, not
how loud it is.

| Level | Meaning | Default visibility |
|-------|---------|---------------------|
| **ERROR** | An invariant is broken. The system is in a bad state. | Always shown |
| **WARN** | A precondition was violated but recovered cleanly. | Always shown |
| **INFO** | A user-visible state change ("peer started", "path loaded"). | Shown by default |
| **DEBUG** | An internal helper fired. Useful when chasing a specific subsystem. | Quiet by default |
| **TRACE** | Hot-path / per-event detail (every wake, every render batch). | Never on broadly |

Rules:

- **ERROR** does not mean "scary"; it means "invariant violation."
  A failed entity lookup that the caller handles cleanly is not ERROR,
  it's INFO (state) or DEBUG (helper).
- **WARN** is the rarest level. If a precondition is violated and
  recovery isn't clean → ERROR. If it's expected → INFO.
- **TRACE** is for the case where you need to see every event. Never
  enabled across the whole app at once.

---

## 3. Categories

Categories are **pre-registered**, not invented per-file. New categories
require a doc edit here (this section).

### Frontend (PanelLog) categories

| Category | Used by | Typical entries |
|----------|---------|------------------|
| `bridge` | `Bridge.cs`, `MainWindow` | `Init success`, `Shutdown begin`, envelope-decode errors |
| `markdown-view` | `MarkdownViewPanel` | `SwapBody`, `EmitBatch`, `LoadPath` |
| `markdown-files` | `MarkdownFilesPanel` | mount, wake, render |
| `tree-view` | `TreeViewPanel` | mount, render, toggle-expand |
| `log-viewer` | `LogViewerPanel` | mount, wake, level-cycle |
| `peer-info` | `PeerInfoPanel` | mount, render, peer-id binding |
| `query-browser` | `QueryBrowserPanel` | execute, paginate, render |
| `detail` | `DetailPanel` | mount, ShowEntity (bridge EntityGet), dispose |
| `site-view` | `SiteViewPanel` | Mount, Navigate, GoBack, SwapBody, Render, Dispose |
| `peer-connections` | `PeerConnectionsPanel` | mount, render, connect, disconnect, nearby-render |
| `verify` | `PublisherVerifyPanel` | Mount, Start (+ the bridge's reply envelope), render-decode failures, Dispose |
| `main-window` | `MainWindow` | startup, peer-tab swap, shutdown |
| `peer-resolver` | `PeerResolver` | resolve, alias lookup |
| `smoke-driver` | `SmokeDriver` | cycle N, ingest, exit-timer |

Conventionally `kebab-case`. Match the file/panel name.

### Bridge / Go-side categories

(`wb/logging.go` — these go through the structured log, not `PanelLog`.)

| Category | Used by | Typical entries |
|----------|---------|------------------|
| `boot` | `BridgeInit` path | peer create, identity load |
| `peer` | per-peer lifecycle | create, destroy, restore |
| `watch` | `WatchSubscribe` / `WatchUnsubscribe` | subscribe, fanout, unsubscribe |
| `tree` | tree-panel handle ops | open, render, toggle, close |
| `dispatch` | `DispatchLine`, `Complete` | shell input, completion |
| `cap` | capability checks | grant, check fail |
| `replication` | replication / sync flows | (when wired) |

Categories cross the bridge: when an Avalonia panel logs `markdown-view`
and the bridge logs `tree` for the same user action, the test-failure
log tail should show both interleaved. (`PanelLog` and the structured
log both write to stderr in Xvfb runs, so this works naturally.)

---

## 4. The pre-crash breadcrumb discipline

**The rule:** every long-running operation logs a breadcrumb at start.
A "long-running operation" is anything that could plausibly crash
(parse, attach, render, bridge call, file IO).

**Why:** when the process dies in native code (stack overflow, SIGSEGV
in Skia, X11 protocol error), the .NET runtime gets no chance to write
a crash dump. Stderr **is** the post-mortem. The last line tells you
which operation was in flight when the bottom fell out.

**Shape:**

```csharp
PanelLog.Write("markdown-view", $"SwapBody h={_handle} bytes={markdown.Length}");
//  ↑ logged BEFORE the operation starts. If we crash, this line is the last thing in stderr.
var inlines = MarkdownRenderer.BuildInlines(markdown);
PanelLog.Write("markdown-view", $"SwapBody h={_handle} parsed {inlines.Count} inlines");
//  ↑ logged AFTER parse succeeds. Tells the debugger "parse was fine; whatever crashed came later."
```

**Anti-pattern:**

```csharp
var inlines = MarkdownRenderer.BuildInlines(markdown);
PanelLog.Write("markdown-view", $"got {inlines.Count} inlines");
//  ↑ logged AFTER. If the parse crashes, this line never fires. The crash is mute.
```

**Always log the inputs that bound the crash.** Handle, byte count,
inline count, batch index — whatever names the slice of work in
progress. "About to do X with size N" is the canonical breadcrumb.

---

## 5. Format

`PanelLog.Write(category, message)` produces:

```
[panel 14:23:45.123] markdown-view: SwapBody h=42 bytes=72018
```

The leading `[panel ...]` prefix lets log readers distinguish frontend
breadcrumbs from the structured Go-side log, and from anything Avalonia
itself writes to stderr (occasional warnings about deprecated APIs,
etc.).

The Go-side structured log has its own format defined by the EventLog
type — not constrained by this doc.

---

## 6. When to use which surface

| You want to... | Use |
|-----------------|-----|
| Leave a breadcrumb before a potentially-crashing op | `PanelLog` |
| Record a state change the user might care about (peer started, file loaded) | Structured log (Go side, via EventLog) |
| Trace per-render or per-wake detail for debugging | `PanelLog` at DEBUG-equivalent (always-on under `WB_PANEL_LOG=1`) |
| Surface a per-peer event in the LogViewer panel | Structured log (Go side) |
| Diagnose a cross-language race (delegate GC, panic at boundary) | Both — `PanelLog` on the C# side, structured log on the Go side, then interleave |

**Don't:**

- Use `Console.Write*` directly. Use `PanelLog` (it knows about the
  enable flag and flushes properly).
- Invent new categories inline. Add them to §3 first.
- Log secrets, identity-bundle contents, or capability tokens. Per
  `reference_delete_is_not_erase` memory pin.

---

## 7. Test integration

Failed tests should automatically attach the last 100 lines of
`PanelLog` output. Today this happens manually (re-run with
`WB_PANEL_LOG=1` and read stderr). The dump-on-failure hook is owed
work — mirror of Godot's `DebugLog.start_capture` /
`drain_captured` from `tests/lib/test_base.gd`.

**Owed:** `TestBase.CaptureBreadcrumbs()` / `DumpOnFailure()` helpers in
`avalonia/tests/Workbench.Headless.Tests/`. Cross-references `TESTING-STRATEGY.md §7`.

---

## 8. Diagnostic shortcuts (aspirational)

Godot's project has F8 / F9 / F10 keys bound to debug operations
(snapshot state, dump mouse state, toggle logging globally). We don't
have these in Avalonia today — the equivalent would be:

- **Toggle `WB_PANEL_LOG`** at runtime (currently env-var only — would
  need a runtime flag flip).
- **Snapshot panel state** (panel handle map, peer roster, current
  selections) to stderr on a hotkey.
- **Dump dispatcher queue depth + active subscriptions** on a hotkey.

Not blocking; useful when crash hunts return.

---

## 9. What this doc does NOT do

- It does not specify the Go-side `EventLog`'s internal API — that
  lives in `wb/` and predates this doc. This doc names how the two
  surfaces relate.
- It does not extend to `entity-shell` / `entity-console` logging.
  Those use the same Go-side EventLog; the `PanelLog` surface is
  Avalonia-specific.
- It does not cover production telemetry (we don't ship any today —
  per `DEPLOYMENT-DIRECTION.md` we're wipe-and-rebuild local-first).

---

## 10. Pinning

Linked from:

- `DISCIPLINE-CHARTER.md §8` — reading order points here when chasing
  a crash.
- `GUIDE-AVALONIA-PANEL-PATTERNS.md §8` — P0 (the breadcrumb pattern)
  cites this doc for category list.
- `MODEL-AVALONIA-RUNTIME.md` — observability rows reference this.
- `TESTING-STRATEGY.md §7` — failure-aware artifact capture references
  the owed dump-on-failure hook.

When new categories are added or surfaces are wired, edit §3 / §1.
