# Model — Avalonia / .NET runtime (as we actually run it)

Canonical. Living doc — edit in place as we learn what the platform
actually does.

This document is the **forensic** record of how Avalonia, .NET, Skia,
and our cgo bridge behave under real load. Not the docs version; the
real-runtime version.

The charter (`DISCIPLINE-CHARTER.md`) names the rules we hold ourselves
to. This doc names the platform's rules — the ones the disciplines exist
to respect.

> **Status — deep-dives folded in.** The Avalonia + Skia source-reading
> sessions are done; the findings are integrated below. The root cause
> of open follow-on #4 (the ~15-18 render accumulation crash) is now
> understood at the structural level (§4, §6 Boundary B, §8). Specific
> file:line refs into the Avalonia / SkiaSharp / Skia source come from
> WebFetch-based research and should be verified against current
> upstream HEAD when used as fix anchors.

---

## 1. Stack diagram

(Copied from the charter so this doc reads cold; charter remains canonical.)

```
L8  entity-core-go              (peer, store, protocol — upstream)
L7  entity-workbench-go         (entitysdk, shellcmd, shellboot — ours)
L6  Go c-shared bridge          (cgo + JSON envelopes — ours)
L5  C# bridge surface           (DllImport, delegates, GCHandle)
L4  App + panels (C#)           (MainWindow, PeerView, MarkdownView)
L3  Avalonia.Controls           (UserControl, Inlines, ScrollViewer)
L2  Avalonia.Skia / X11         (real paint pipeline)
L1  libSkiaSharp + HarfBuzz     (native rendering)
L0  X11 (or Wayland) / Xvfb     (display server)
```

Two languages, two heaps, one process. The "one process" part is the
key: a SIGSEGV at L0 takes everything with it. There is no fault
isolation between layers.

---

## 2. Type-lifecycle matrix

Who allocates, who frees, when, on which heap, and what the failure
mode looks like.

| Type | Allocated by | Freed by | Heap | Failure if mishandled |
|------|--------------|----------|------|------------------------|
| C# managed objects (`UserControl`, `string`, `List<T>`) | C# `new` | .NET GC (non-deterministic) | Managed | Memory pressure if accumulated; no use-after-free (GC blocks reclamation) |
| `GCHandle.Alloc(delegate)` | C# explicit | `GCHandle.Free()` explicit | Managed (pinned) | If freed too early, native pointer dangles → SIGSEGV on next Go callback |
| `Marshal.GetFunctionPointerForDelegate` result | C#, derived from a delegate | Implicit when GCHandle freed | (function-table slot) | If the source delegate is GC'd, pointer dangles. **AP2 is exactly this.** |
| Avalonia `Visual` / `Control` (`SelectableTextBlock`, `StackPanel`) | C# `new` (inside dispatcher) | Detach from tree → GC eligible **but** the layout manager + compositor serialization queue hold references until processed | Managed | Mutation off the UI thread = race; mutation during paint = SIGSEGV in Skia. The detached-but-not-yet-GC'd window lasts longer than intuition suggests — see §4. |
| Avalonia `TextLayout` / `_textRunCache` | Per `TextBlock` (managed) | **Not auto-cleared on Inlines mutation.** Recreated lazily on next measure; old layout dies with the TextBlock | Managed wrapper of native handles | Per-TextBlock; the *native* SkTextBlob references it owns are the actual accumulation surface (see Skia row). |
| Skia `SkTextBlob` / `GlyphRunImpl` cache | Native (libSkiaSharp) — created per text-run during Skia paint | Disposed when its owning managed wrapper is finalized | Native | **Held across renders until GC runs.** This is the cache that piles up — finalizer-driven release means timing is non-deterministic. |
| Skia `SkStrikeCache` (process-global) | Native (Skia) — populated by every text-shaping op | Internal LRU eviction at a default ~2 MB budget / 2048 glyph entries; can be explicitly purged via `SKGraphics.PurgeFontCache()` | Native (process-global) | **The root accumulation surface for open #4.** Each new doc adds new strikes; HiDPI makes each strike ~3× larger; cache eviction at saturation races finalizers of older `SkTextBlob`s → SIGSEGV in libSkiaSharp. |
| HarfBuzz font / shaper handles | Native (HarfBuzz) — via `SKShaper` | Refcounted; released when the wrapping SkTypeface releases | Native | Per-typeface; bounded by font set in use (small for our markdown). Not the open #4 root, but watch under script/language variation. |
| `IntPtr` returned by `BridgeFoo(...)` | Go (`C.CString` → manualloc) | C# `Bridge.FreeString(p)` after `PtrToStringAnsi` | Native (C heap) | If C# forgets to `FreeString`, leak. If C# uses after free, SIGSEGV. (Mitigated by `Bridge.TakeString` chokepoint.) |
| Go goroutine + channel (wake fanout) | Go side at `*RegisterWake` | Joined via `wakeDoneCh` on `*Close` | Go heap | If Go panics and `recoverToErrorEnvelope` doesn't catch, process aborts. |
| Long-lived Go handles (peer, tree, watch, log, markdownView, markdownFiles, query, peerInfo) | Go side, registered in a map | Released on `*Close` | Go heap | If C# uses a handle after `*Close`, Go returns an error envelope (not a crash). If Go forgets to register, C# gets `handle==0`. |
| SQLite store rows | core-go (modernc.org/sqlite) | Wipe-and-rebuild (or explicit delete) | Native | Persistence per D17. |
| Identity bundle (on disk) | identity extension on peer create | Identity-bundle backup; preserved across wipe | Disk | Per `DEPLOYMENT-DIRECTION.md §3` — keypair / identity entity / identity bundle distinction matters. |

The key rows: **GCHandle**, **Avalonia Visual**, **Skia caches**.
These are where five of the eight crash-hunt commits hit. The Skia
row is where the still-open follow-on #4 lives.

---

## 3. Process-lifetime singletons

Things that live for the whole session and must shut down cleanly:

| Singleton | Owner | Init | Teardown | Failure if leaked |
|-----------|-------|------|----------|--------------------|
| `Bridge` (Go side) | `bridge/main.go` global | `BridgeInit(configJson)` | `BridgeShutdown()` (called from `MainWindow.Closing`) | Bridge state survives across `Init`s — must not happen; init is idempotent-by-error today. |
| Default peer | Bridge | First `BridgeInit` | `BridgeShutdown` cascades to all peers | Peer goroutines orphaned if Shutdown is skipped (the unclean-process-kill path). |
| `Bridge.handles` (C# side) | `Bridge.cs` (implicit via GCHandles) | As panels mount | As panels dispose | A panel that doesn't `GCHandle.Free` leaks the delegate forever (acceptable today; would matter under many open/close cycles). |
| `App` / `AppBuilder` / `Dispatcher` | Avalonia framework | `Program.cs::Main` | App quit | Standard Avalonia — well-understood. |
| `WB_PANEL_LOG` stderr breadcrumb stream | `PanelLog.cs` | First write | None — file handle is stderr | None — stderr is OS-managed. |

**Avalonia framework singletons (mapped).** Per-process:

| Singleton | Owner | Notes |
|-----------|-------|-------|
| `Application.Current` | `Application` static | Holds the `Styles` collection, resource roots. |
| `Dispatcher.UIThread` | `Avalonia.Threading.Dispatcher` static | The UI thread. Owns `DispatcherPriorityQueue`. Lazy-allocates an `AvaloniaSynchronizationContext` per priority level on first use. |
| `AvaloniaLocator` services | `AvaloniaLocator.Current` | Registered via `AppBuilder.Configure<T>().UsePlatformDetect().UseSkia()`. Includes platform impls (X11, render interface, font manager). |
| `RenderLoop` (or per-window equivalent) | Platform | Pumps `IRenderLoopTask.Render()` calls at ~60 Hz. On X11 today this runs on the UI thread via managed dispatcher (no dedicated render thread on the X11 backend — see §6). |

Per-`TopLevel` (window):

| Singleton | Notes |
|-----------|-------|
| `Compositor` | UI↔render-thread serialization manager. Has an `_objectSerializationQueue` that holds visual proxies until drained. |
| `LayoutManager` | Owns measure/arrange queues — holds references to invalidated visuals until processed. |
| `ServerCompositor` | Render-side counterpart of Compositor; processes serialized batches. |

The composition queue + layout-manager queue is the lesser-known piece:
even after you `Children.Clear()`, the cleared children may be enqueued
for one more measure pass or one more composition serialization
before they become GC-eligible. This explains why aggressive
`Inlines.Clear()` made things *worse* in earlier fix attempts (PHASE-I-
RELIABILITY-PLAN follow-on #4 record).

---

## 4. Reference cycles + accumulation

What can pile up across the session and what the rule is.

| Class | Mechanism | Mitigation today | Status |
|-------|-----------|------------------|--------|
| Panel delegate pinning | Each panel that registers a wake `GCHandle.Alloc`s its delegate; freed on dispose | Pattern enforced in MarkdownView; **other panels need audit** | OPEN — checklist item for the panel-pattern guide. |
| Visual-tree children | `Inlines.Add(...)` or `Children.Add(...)` without paired remove | MarkdownView swaps the whole body StackPanel on `LoadPath`; persistent container per `PHASE-I-RELIABILITY-PLAN §3 Layer 3` | OPEN — other panels still do clear-and-add. |
| Avalonia composition serialization queue | UI thread enqueues; render side dequeues. Backlog if render side is slow. | Pre-allocating blocks + only mutating `Inlines` (not `Children`) keeps the queue small per render. | OK in MarkdownView (post-`8149f98`); pattern needs P1 application elsewhere. |
| Skia `SkStrikeCache` (process-global glyph rasterizations) | Every `SkCanvas.DrawTextBlob` adds glyphs (font + size + options + rasterized bitmaps) to the process-global strike cache | None — pre-falsification hypothesis; cache never saturates before crash. | **NOT the open-#4 root.** Falsified by direct measurement: cache at crash sits at ~10% of budget; `PurgeFontCache` between renders made the crash worse; forced-GC reclaimed nothing. Strikes are not the pinning source. See §4 for the falsification record + revised leading candidate. |
| `SkTextBlob` / `GlyphRunImpl` wrapper retention | Created during Skia paint; managed wrapper finalizer releases native handle | Inlines.Add on attached blocks is bounded by P1 + P2 + per-block 500-inline cap. | **STRONGLY REACHABLE upstream**, not finalizer-pending (measured: `GC.Collect() + WaitForPendingFinalizers()` reclaims nothing). The reachability root is most likely the compositor batch pipeline (§4 leading candidate). |
| Compositor batch pipeline (`Compositor._objectSerializationQueue` UI side → `CompositionBatch` → `ServerCompositor._batches` render side) | UI-thread serialization queue drains into batches; render-thread queue drains one batch per `RenderCore()` tick. Each batch holds serialized references to all affected visuals + text-shaping output for at least one full render-tick after the UI thread enqueues it. | Structural: P3 wake debounce (400ms in MarkdownView, 150ms elsewhere) + cross-panel sweep means no panel does unbounded rapid rendering, so the queue never backs up. | **Leading candidate for open follow-on #4** (source-archaeology grounded; confirmation awaits direct measurement of `_batches.Count`). The mitigation makes it practically unreachable in our use; direct measurement deferred (§4 + task #17). |
| Go-side handle map | `peerHandles`, `treeHandles`, `watchHandles`, etc. live in maps in the bridge | Released on `*Close`; cascaded on `PeerDestroy` | OK — pattern is consistent. |
| EventLog / log filter state | Per `LogFilterModel`; bounded by collection level | Already bounded | OK |
| `ObservableCollection` rows in `TreeViewPanel`, `LogViewerPanel`, `MarkdownFilesPanel`, `PeerInfoPanel`, `QueryBrowserPanel` | Clear-and-add per wake; unbounded inputs | **None today** — PHASE-I-RELIABILITY-PLAN follow-ons #5-#9 | OPEN — D15 violation surface, fixes pending. |

**The story of open follow-on #4** — revised after direct-measurement
probe runs. The deep-dive hypothesis (Skia strike-cache LRU eviction
racing finalizer-pending blobs) was **falsified** by direct measurement;
the revised hypothesis points at Avalonia's compositor / layout
retention upstream of Skia.

#### What the deep-dive predicted

1. Skia's `SkStrikeCache` is ~2 MB / 2048 glyphs; each render adds strikes.
2. After ~14-18 renders the cache saturates → LRU eviction triggers.
3. SkiaSharp wrapper finalizers are non-deterministic → an evicted
   strike may still be referenced by a finalizer-pending `SkTextBlob`
   from an earlier render → next paint SIGSEGVs.
4. HiDPI amplifies because each strike is larger; saturation hits sooner.

#### What we measured (instrumentation via `SKGraphics.GetFontCacheUsed()`)

| Run | At-crash strike-cache size | Renders before crash |
|-----|-----------------------------|----------------------|
| HEAD baseline (clean) | not measured (but had to crash by ~4 renders per identical run) | 4 |
| With `SKGraphics.PurgeFontCache()` between renders | 0 (cache fully evicted) | 4 |
| With `GC.Collect()` + `WaitForPendingFinalizers()` between renders | ~73k → ~140k → ~200k (before == after — GC reclaimed nothing) | 5 |
| Diagnostic-only (no mutation) | 75k → 96k → 188k | 3 |

**Three falsifications, in order of severity:**

1. **Cache never saturates.** At the moment of crash, the cache is at
   ~200 KB, about 10% of its 2 MB budget. No LRU eviction is happening
   at all. The "saturation → eviction race" model is wrong.
2. **GC reclaims nothing.** `before == after` on every `GC.Collect()`
   call. The old text-blob wrappers are **strongly reachable** somewhere —
   not finalizer-pending. The "finalizer-pending blobs hold strike
   refs" model is wrong.
3. **Aggressive purging makes the crash deterministic.** Calling
   `PurgeFontCache()` between renders reproduced the crash within 4
   renders consistently. Manual eviction races *in-flight paint* of
   the previous render. The "we just need to control eviction timing"
   model is wrong.

#### Revised hypothesis

The pinning source is **above Skia**, in Avalonia. The Avalonia deep-dive
flagged this as Hypothesis 2 (Composition Serialization Batch Backlog)
but ranked it lower. The measurements promote it:

- **`Compositor._objectSerializationQueue`** (per-window, holds visual
  proxies until serialized to render thread) plausibly retains references
  to detached visuals + their associated text-shaping output for at
  least one more render cycle.
- **`LayoutManager`'s measure/arrange queues** also hold dequeued
  visuals until processed.
- These queues drain on each render-loop tick, but if the renderer
  is busy painting prior renders, the queue grows.

When the queue reaches some threshold (count, total native bytes,
serialization-batch size, or simply enough native memory pressure
to fail an allocation in libSkiaSharp), boom.

**Why content size scales the bug:** larger docs → more visual children
in the serialization queue per render → faster threshold approach.
Adding the discipline charter docs to the test corpus this session
shifted "renders to crash" from the prior "~15-18" baseline to "~3-5"
without any code change.

**Why HiDPI amplifies:** at 2560×1440 each render does ~4× the
text-shaping work → ~4× the serialization payload → threshold hit
~14× sooner (memory pressure compounds non-linearly under GC budgets).

#### Leading candidate mechanism — source-archaeology grounded

We read the Avalonia 11.2.x source for the suspected retention sites.
The findings below are **consistent with** every measurement we
have, but not yet **confirmed by direct measurement** of the
suspected queue depth. Per AP9, source archaeology that's consistent
with the symptom is evidence, not proof — the strike-cache hypothesis
also looked source-consistent before measurement falsified it. Treat
this section as the leading candidate, not the grounded answer.

**The two-stage compositor pipeline that retains visual references:**

1. **UI thread side — `Compositor._objectSerializationQueue`**
   (`src/Avalonia.Base/Rendering/Composition/Compositor.cs`).
   `RegisterForSerialization()` adds visual proxies to a FIFO queue
   (deduped via a hashset). The queue drains in `CommitCore()`,
   which serializes every object into a `CompositionBatch`'s
   internal `BatchStreamWriter` byte buffer and enqueues that batch
   to the render thread via `_server.EnqueueBatch(_nextCommit)`.
   The buffer holds strong references to every serialized object
   until the batch is dequeued + applied + released.

2. **Render thread side — `ServerCompositor._batches`**
   (`src/Avalonia.Base/Rendering/Composition/Server/ServerCompositor.cs`).
   A `Queue<CompositionBatch>` lock-protected against the UI thread.
   `ApplyPendingBatches()` drains the queue one batch per
   `RenderCore()` tick. After application, batches move through
   `_reusableToNotifyProcessedList` and `_reusableToNotifyRenderedList`
   before final release via `NotifyBatchesRendered()`. The full
   batch-release window spans **at least one full render-thread
   tick** after the UI thread enqueued it.

**Why this is consistent with our falsifications:**

- The "GC reclaims nothing" measurement directly indicates the old
  text-blob wrappers are strongly reachable somewhere; a serialized
  batch buffer holding them through 2-3 render cycles would produce
  exactly this signature.
- The "cache never saturates" measurement is then expected: the
  process dies in the composition / batch pipeline upstream before
  the strike cache reaches LRU pressure.
- "Larger content scales the bug" is expected: each batch's
  serialized payload scales with visual-children count, so the
  retention window's bytes-pinned grow proportionally.
- HiDPI amplifies because text-shaping output is ~3-4× per glyph at
  2560×1440, growing the per-batch payload at the same rate.

**What would actually confirm this mechanism (the measurement we
don't yet have):**

Instrument `ServerCompositor._batches.Count` between smoke-driver
renders. If under-150ms-cadence renders cause the count to grow
monotonically — and the crash correlates with a numeric threshold
on that count — the diagnosis is confirmed. If the count stays
bounded but the crash still fires, the retention is somewhere else
(per-batch buffer growth? a third queue?) and the model needs another
revision.

**Why we haven't done that measurement yet:** the structural mitigation
(P3 debounce raised to 400ms + cross-panel sweep landed
ensures no panel does unbounded rapid rendering) makes the bug
practically unreachable. Direct measurement is owed work, but not
urgent. Pinned as task #17 (compositor queue depth probe).

**Alternative paths to closure that don't require our measurement:**

- **Try Avalonia 11.3+ when it ships.** Compositor retention is the
  kind of bug that upstream maintainers chase. If 11.3 closes it,
  the diagnosis was right; if it persists, the leading candidate
  needs another revision.
- **Reproduce on a non-text-heavy panel.** If the same accumulation
  pattern surfaces in a panel that doesn't use `SelectableTextBlock`
  / `Inlines`, the mechanism is purely compositor-side. If it
  doesn't, text-shaping is load-bearing somewhere.

---

## 5. Teardown timeline

What runs in what order when a window closes / app exits.

```
MainWindow.Closing fires
  ↓
MainWindow.OnClosing
  ↓  (synchronous)
  Bridge.Shutdown()
    ↓ (cgo call)
    Go: BridgeShutdown
      ↓
      For each peer: Peer.Close()
        ↓ goroutines join via wakeDoneCh
        ↓ Store closes
      Handle maps cleared
    Returns
  ↓
  Application exits
  ↓
  .NET runtime tears down
    ↓ Finalizers run (non-deterministic ordering)
    ↓ GCHandles released
  ↓
  Process exits
```

**Exit-path notes (from the deep-dive).** On clean exit, Avalonia
walks `IClassicDesktopStyleApplicationLifetime.Shutdown` → fires
window `Closed` events → drains the dispatcher → finalizers run on
their thread. Skia native handles are released by their managed
wrapper's finalizer, which is **non-deterministic** in ordering and
timing — there is no guarantee the strike cache is purged or that
SkSurfaces are released before process exit. We rely on OS process
teardown for native heap, which is fine for the clean case.

The unclean-exit path (SIGSEGV before Shutdown runs) leaves Go peer
goroutines orphaned and SQLite WAL files potentially mid-flush.
Wipe-and-rebuild (D17) covers us; identity bundles are the one
non-disposable, and they're backed up out-of-band.

---

## 6. Boundary map — the seven rows where bugs live

Adapted from EGUI's `MODEL-BROWSER-WASM-RUNTIME §10`. Every shipped
bug we've forensically diagnosed sits at one of these boundaries.

### Boundary A — C# panel ↔ Avalonia control tree

- **Currency:** `Inlines`, `Children`, routed events, dispatcher work items, `Compositor._objectSerializationQueue`, `LayoutManager` measure/arrange queues.
- **Bug class:** mutate-during-paint, layout-state race, unbounded children, paint-recursion depth, **compositor/layout retention accumulating across renders (open #4)**.
- **Forensic record:** commits `7d92934` (AP8 — 2880 inlines, per-render bounded-payload); open follow-on #4 (compositor-queue retention, cross-render accumulation; root cause measured + diagnosis revised by direct measurement).
- **Discipline:** D13 (UI-thread integrity), D15 (bounded payloads).
- **Pattern:** `GUIDE-AVALONIA-PANEL-PATTERNS.md` P1 (adaptive emit), P2 (persistent container), P3 (wake debounce — *new lever for open #4*), P4 (bounded list).
- **What Avalonia retains (from deep-dive + source archaeology):** A detached `Visual` is *not* immediately GC-eligible. The `LayoutManager`'s measure/arrange queues hold references until the next layout pass drains; the `Compositor`'s `_objectSerializationQueue` holds visual proxies until `CommitCore()` serializes them into a `CompositionBatch`; the render-thread `ServerCompositor._batches` queue holds those batches (with their serialized payload of visual + text-shaping state) until `ApplyPendingBatches()` + `NotifyBatchesRendered()` complete on a future render-thread tick. The two-stage pipeline is the leading candidate for open follow-on #4's cross-render accumulation (see §4 — confirmation awaits direct measurement of `_batches.Count`). This is also why `Inlines.Clear()` mid-batch made things worse historically — the cleared inlines were still in the composition queue, their parents in the layout queue.
- **What does **not** auto-clear:** A `TextBlock`'s internal `_textLayout` / `_textRunCache` is not cleared when its `Inlines` mutate. The old layout dies with the TextBlock (or with the next remeasure that overwrites it). The native text-shaping output transiently lives at Boundary B but is **pinned from Boundary A**.
- **Routed-event delivery order, and the trap in it (2026-08-21, AP37).** At a single element the route carries **class handlers before instance handlers**, and a control's own `OnPointerPressed` override *is* a class handler. `Button` sets `e.Handled = true` for a left press (and again on release), so **`btn.PointerPressed += …` on a `Button` never runs** — the subscription is accepted, is never invoked, and produces no warning at build or run time. This is not a niche case: it is the ordinary way anyone wires press-and-hold behaviour, and it shipped here as an on-screen game controller that rendered perfectly and did nothing. The forms that work are `AddHandler(…, RoutingStrategies.Tunnel)` (the tunnel pass reaches the element before the bubble class handler) or `handledEventsToo: true`; registering both is safe when the write is idempotent. **Corollary for testing:** because the failure is *silent subscription*, no unit test that calls the handler's target directly can see it — only dispatch through `TopLevel` can. `Avalonia.Headless`'s `MouseDown`/`MouseUp`/`KeyPressQwerty` run the real route and are cheap; `ProgramPanelInputTests` is the worked example.

### Boundary B — Avalonia ↔ Skia / HarfBuzz

- **Currency:** SkCanvas draw commands, SkTextBlobs, SkPaint, the process-global SkStrikeCache, HarfBuzz shapers.
- **Bug class:** paint-recursion depth (per-block — one-shot, *not* the open-#4 root).
- **Forensic record:** commit `7d92934` (paint recursion depth, the per-block one-shot class).
- **Discipline:** D15 (bounded payloads — per-node).
- **Notes from the probe:**
  - `SkStrikeCache` does NOT saturate before the open-#4 crash. Cache at crash is ~200 KB; the budget is ~2 MB. The "LRU eviction race" hypothesis was falsified by direct measurement (see §4).
  - `SKGraphics.PurgeFontCache()` between renders MAKES THE CRASH WORSE. Manual eviction races in-flight paint of the previous render. This is a useful "don't do this" data point — pinned in invariant #11.
  - `GC.Collect()` + `WaitForPendingFinalizers()` between renders does NOT reduce cache size (before == after). Old blobs are strongly reachable upstream (Boundary A), not finalizer-pending.
- **Lesson for new panels at Boundary B:** keep `SKGraphics.GetFontCacheUsed()` instrumentation cheap and per-render; it gives the cleanest forensic signal we have for cross-render accumulation. Do NOT call `PurgeFontCache` or force-GC at the render seam without verifying the diagnosis structurally first.

### Boundary C — C# ↔ Go (cgo, JSON envelope)

- **Currency:** `DllImport` calls, `GCHandle`-pinned delegates, JSON `{ok|error|result|handle}` envelopes, `IntPtr` C-strings.
- **Envelope schema (fixed):**
  - Success: `{"ok":true, "<fields>"}` (e.g., `{"ok":true,"handle":42}`, `{"ok":true,"lines":[...],"prompt":"..."}`).
  - Failure: `{"ok":false,"error":"..."}`.
  - Some shape-preserving APIs return raw structured data with `"ok"` implicit (e.g., `MarkdownViewRender` returns the body envelope directly). **These are exceptions and should be reduced.**
- **Bug class:** delegate GC race, panic propagation, JSON parse failures, schema drift.
- **Forensic record:** commits `958b3fe` (AP2), `f95c0c6` (AP3), `8b3963e` (AP7 — diagnostic blind spot).
- **Discipline:** D12 (cross-language lifetime), D14 (IPC envelope contract).
- **Enforcement:** `recoverToErrorEnvelope` deferred in every `//export` (Go side, `main.go:97`). `GCHandle.Alloc` paired with every `GetFunctionPointerForDelegate` (C# side, e.g., `MarkdownViewPanel.cs:170`).

### Boundary D — Avalonia ↔ X11 / Wayland

- **Currency:** window messages, `_NET_WM_*`, focus events, DPI scaling.
- **Bug class:** HiDPI amplification, Wayland-only paths, real-display scaling, font fallback.
- **Forensic record:** the Xvfb-hidpi smoke harness (PHASE-I-RELIABILITY-PLAN §1467985 area) confirmed HiDPI amplifies the open follow-on #4 (~1 iter at 2560×1440 vs ~18 at 1920×1080).
- **Discipline:** D10 (real-session coverage), D16 (test-depth honesty).
- **[DEEP-DIVE PENDING]:** Wayland path coverage (currently zero — Xvfb is X11-only).

### Boundary E — Go bridge ↔ entitysdk

- **Currency:** `shellcmd` verb invocations, `Store` ops, subscriptions, capability checks.
- **Bug class:** wrong-peer routing, subscription leakage, capability bypass, watch handle leaks.
- **Forensic record:** none on Avalonia side — the bridge consistently routes per `peerHandle`. **But this is where wrong-peer routing would live** if we got it wrong. EGUI's AP2 (defaults-to-primary scoping) is the analogous defect class.
- **Discipline:** D2 (L1 dispatch default), D3 (capability surface), D6 (per-host namespaces).
- **Status:** OK on Avalonia side (every per-peer op takes leading `long peerHandle`). Audit owed before adding new per-peer ops.

### Boundary F — App ↔ persistent store

- **Currency:** SQLite databases, identity bundles on disk, runtime paths.
- **Bug class:** what survives wipe / what gets re-derived / where backup lives.
- **Discipline:** D17 (persistence honesty).
- **Status:** wipe-and-rebuild is the operational posture; identity bundles are the exception. No Avalonia-side persistence yet (all state lives in the underlying peer's tree).

### Boundary G — the process's POSIX signal layer

**Added 2026-08-21, by a crash that no other boundary could explain.** Six boundaries
described everything we had diagnosed and none of them described this one, which is
the tell that the map was short a row rather than that the bug was exotic.

- **Currency:** POSIX signals, `sigaction` handlers, and the per-thread **alternate signal
  stack** (`sigaltstack`). Three parties install handlers in this process and none of them
  knows about the others: CoreCLR's PAL (`SIGSEGV`/`SIGBUS`/`SIGFPE` for null checks and
  GC write barriers, `SIGRTMIN`/SIG34 to inject thread suspension), the Go runtime inside
  `libbridge.so`, and glibc.
- **Bug class:** **alternate-signal-stack exhaustion.** The PAL gives a thread a **16 KB**
  altstack. Handlers run there, and handlers can nest — a signal arriving while the thread
  is already on the altstack does not switch stacks, it keeps consuming the same 16 KB. When
  it runs out, the next `call` pushes its return address into the guard page below.
- **Why it is nearly invisible:** the failure has no honest signature by default.
  1. CoreCLR cannot classify the fault, so it **re-raises**. What reaches the coredump is
     the *second* signal: `si_code 128` (SI_KERNEL), `si_addr 0`, and the handler's register
     context. Read naively, that is a null dereference. It is not.
  2. The runtime never prints `Stack overflow.` and `createdump` never fires — by the time
     it faults there is no stack left to report on.
  3. The DAC refuses a systemd ELF core (`0x80004002`), so there is no managed stack either.
  4. `libbridge.so` appears in **zero** frames, so the bridge looks innocent — and is.
  The true signature is only visible by catching the FIRST signal live:
  `si_code 2` (SEGV_ACCERR), `si_addr = rsp-8`, rip on a `call`, `rsp` inside a `PROT_NONE`
  page whose mapping is the altstack rather than the managed stack.
- **Amplifier:** real pointer input. Clicking drives hit-testing, focus transfer and layout,
  which raises signal traffic; the model-level drivers that call the method *under* the
  control raise none of it and cannot reach this boundary at all.
- **Forensic record:** the month-long "managed stack overflow on minimize" (STATUS) was this,
  reached by a different route. Four repro attempts came back negative because every one of
  them drove the model, not the input.
- **Discipline:** D13 (observability surface), D19 (measure, don't argue), D24 (name the
  instrument's region), AP34, AP35.
- **Status:** **mitigated, not fully explained.** The UI thread now installs a 1 MB altstack
  at startup (`CrashDiagnostics.EnlargeAltStack`, `WB_ALTSTACK_BYTES=0` restores stock).
  A/B on the click fuzz: **6/8 seeds crash at 16 KB, 0/8 at 1 MB**, same binary, same seeds.
  **Every other managed thread still runs the stock 16 KB**, and *what* consumes more than
  16 KB remains unidentified — nested delivery is the leading candidate, with Go's async
  preemption (`GODEBUG=asyncpreemptoff=1`), `DOTNET_gcConcurrent=0` and
  `DOTNET_TieredCompilation=0` all ruled out as the trigger.

---

## 7. Invariants

The invariants the model establishes. Every change is checked against these.

1. **Two heaps, one process.** A SIGSEGV at L0-L1 takes everything down. Fault isolation is the discipline, not the runtime.
2. **GCHandle-or-die.** Every delegate handed to Go is `GCHandle.Alloc`-pinned for the lifetime of Go's use. Free only after the goroutine has joined.
3. **`recoverToErrorEnvelope` is mandatory.** Every `//export` in `bridge/main.go` defers it. A Go panic crossing cgo without it aborts the host.
4. **One UI thread.** All visual-tree mutation runs on the Avalonia dispatcher. Long-running work yields between batches at `Background` priority.
5. **Bounded children, always.** No `Inlines.Add` / `Children.Add` without a cap, virtualization, or paginate.
6. **Handles are flat namespaces.** Peer handles, watch handles, tree handles, log handles, markdownView handles, markdownFiles handles, query handles, peerInfo handles — each in its own map. Cross-namespace use is a bug.
7. **Cascade on PeerDestroy.** Every per-peer handle type is registered against its peer and freed when the peer is destroyed. C# doesn't need to enumerate handles to clean up.
8. **Envelope contract is fixed.** `{ok|error|result|handle}` shape. Widening requires a doc edit here first.

### Earned from the Avalonia/Skia deep-dive

9. **A detached Visual is not GC-eligible immediately.** The LayoutManager + Compositor serialization queue hold references until their next drain. Code that mutates the visual tree must assume detached visuals are still being processed for at least one more layout/composition cycle. Aggressive `Clear()` mid-batch makes things worse.
10. **`TextBlock` internal layout caches do not auto-clear on `Inlines` mutation.** They die with the TextBlock or are overwritten by the next remeasure. If you reuse a TextBlock across renders and want its caches gone, replace the TextBlock.
11. **`SkStrikeCache` is process-global and finite.** ~2 MB / 2048 glyphs by default. Cache size is a useful forensic signal (`SKGraphics.GetFontCacheUsed()`) but it does NOT bound the open #4 crash — the crash fires at ~10% of budget. **Do NOT call `SKGraphics.PurgeFontCache()` between renders**; the probe runs proved this makes the crash strictly worse (race with in-flight paint).
12. **HiDPI multiplies cross-render workload ~3-4×.** Every per-render workload bound on the X11 backend must be reverified under HiDPI before being declared safe. Today this is the strongest amplifier for the open #4 compositor-retention bug.
13. **SkiaSharp finalizers don't reach old text-blob wrappers when they're upstream-pinned.** A native handle whose managed wrapper is GC-eligible is released on the finalizer thread; but if the wrapper is held by Avalonia's compositor / layout retention (which it is, between renders), GC never marks it eligible at all. Forcing `GC.Collect()` + `WaitForPendingFinalizers()` between renders reclaims nothing in this case. **Do NOT add forced-GC barriers between renders** without a measurement-driven justification; we tried and it made the crash worse by racing the compositor.
14. **Render-priority and Background-priority work can interleave.** Avalonia's dispatcher drains highest priority first per tick, but a Background-priority callback can be running when a Render-priority measure pass starts. Mutations to visual *structure* (Add/Remove children) during a Background-priority callback race the paint pipeline. Mutations to `Inlines` on already-attached blocks are safe. (This is the structural rationale for the P1 adaptive-emit pattern.)
15. **The alternate signal stack is a bounded resource, and 16 KB of it is not enough.** The
    PAL default is 16384 bytes per thread; handlers nest on it; exhaustion presents as a
    SIGSEGV that is indistinguishable from a null dereference *after* CoreCLR re-raises it.
    The UI thread now runs a 1 MB altstack. **Do not "clean up" that startup call**, and do
    not read `si_addr` on any .NET Linux crash without checking `si_code` first — 128
    (SI_KERNEL) means the signal was re-raised and the register context is the handler's.
16. **A driver that calls the model method under a control cannot exercise the control.** No
    amount of `NavigateForTests`-style coverage reaches input dispatch, hit-testing or focus
    transfer. Real input needs real input (`make -C avalonia smoke-xvfb-click`). A negative
    result from a model-level driver is evidence about the model, not about the app.
17. **A `+=` subscription on a routed event is not a guarantee of delivery.** Class handlers
    precede instance handlers at the same element, and a control that handles the event in
    its own override — `Button` does, for `PointerPressed` and `PointerReleased` — silently
    discards every `+=` subscriber. Use `AddHandler` with `Tunnel` and/or
    `handledEventsToo: true` for any control-level input wiring, and gate it with a test that
    dispatches through `TopLevel`. There is no build-time or run-time signal for getting this
    wrong; the only symptom is that nothing happens.
18. **Headless is enough to test input, and cheaper than Xvfb.** `Avalonia.Headless`'s
    `MouseDown` / `MouseUp` / `KeyPressQwerty` run the *real* route — hit test, capture, class
    and instance handlers — so invariant 16's gap is closable in the xunit suite for anything
    that is not a *platform* bug. Xvfb + `xdotool` remains the instrument for faults below
    Avalonia (Boundary D and G); headless covers Boundary A. Use the cheap one first.

---

## 8. Empirical bug-class location

Where each shipped defect actually lived:

| Commit  | Boundary | Type | What broke |
|---------|----------|------|------------|
| `d742050` | (cross-cut) | Observability | `DOTNET_EnableDiagnostics=0` silenced runtime crash output |
| `958b3fe` | C | Lifetime | Delegate GC race |
| `f95c0c6` | C | Lifetime | Same defect at every panel's wake-register site |
| `6c09f09` | (test harness) | Coverage | Tests stubbed the renderer |
| `f25e7cb` | (test harness) | Coverage | Headless tests skipped Skia |
| `1467985` | (test fixtures) | Coverage | Synthetic docs didn't match real |
| `8b3963e` | (cross-cut) | Observability | Stack overflow left no diagnostic trail |
| `7d92934` | A + B | Bounded payload | 2880 inlines → Skia paint recursion |
| **(open #4)** | **A** (revised by direct measurement) | Accumulation | Avalonia compositor / layout retention upstream of Skia. Original "strike-cache LRU race" hypothesis falsified by direct measurement; revised hypothesis is compositor serialization-queue / layout-manager retention growing across rapid renders. See §4. |

The pattern: **A and C dominate**. Earlier in the day we suspected B
(Skia) but probe runs (`PurgeFontCache` and forced-GC) made the crash
strictly worse and revealed the pinning source is upstream. D, E, F
remain uncharted; when D (HiDPI / X11 / Wayland) gets new test
coverage we'll likely find more there.

**Note on this revision** — the strike-cache hypothesis was published
in commit `8b0b82f` and falsified in the same day's probe runs. The
falsification is itself forensic evidence (it tells us where the
pinning is *not*), so the model preserves both the original deep-dive
and the falsification record per D8 (trust the spec, surface drift,
don't normalize).

---

## 9. What this doc still does NOT do

The Avalonia + Skia deep-dives closed the main gaps. The following are
the remaining holes — none currently blocking, but worth knowing about:

- **Wayland behaviors.** Zero coverage today; Xvfb is X11-only. D10 /
  D16 gap. Will need its own deep-dive if/when Avalonia ships with
  Wayland as the default backend (current Avalonia 11.x still defaults
  to X11 on Linux).
- **Custom drawing primitives.** Our markdown render uses only standard
  text + layout. If we add a panel that does custom `DrawingContext`
  work (graphs, syntax-highlighted source, scrollable canvas), we'll
  need to extend §6 Boundary B with the corresponding paint-path
  invariants.
- **GPU vs CPU rendering.** Avalonia 11 on X11 defaults to GPU
  rendering via OpenGL. We haven't probed what happens when GPU init
  fails (CPU fallback path). HiDPI amplification (§4) suggests memory
  pressure is the active failure mode regardless of GPU/CPU.
- **Finalizer ordering at process exit.** We rely on OS teardown via
  D17 (wipe-and-rebuild) — fine for now. If we ever ship state-bearing
  user data that isn't backed up, this becomes a real concern.
- **Concrete fix verification for open follow-on #4.** The deep-dive
  identified the root cause hypothesis (`SkStrikeCache` saturation +
  finalizer race). The fix candidates (`SKGraphics.PurgeFontCache()`,
  `GC.WaitForPendingFinalizers()`) need to be probed against the
  actual crash. That's the next-up work, not a doc gap.

---

## 10. Reading order

When you need to understand why a panel works the way it does:

1. `DISCIPLINE-CHARTER.md` — the rules.
2. **This file** — what the platform actually does.
3. `GUIDE-AVALONIA-PANEL-PATTERNS.md` — the recipes that respect the rules.

When you've hit a crash:

1. The breadcrumb (last line of stderr under `WB_PANEL_LOG=1`).
2. `LOGGING-CONVENTIONS.md` — to decode what category was last active.
3. **This file §6 (Boundary map)** — to locate which boundary the
   crash sits at.
4. The corresponding discipline in the charter — to pick the fix shape.

---

## 11. Pinning

Linked from:

- `DISCIPLINE-CHARTER.md §1`, §8 — the charter calls this its
  companion runtime doc.

Edit in place as understanding evolves. Date the **[DEEP-DIVE PENDING]**
sections when they close.
