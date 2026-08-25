# Discipline charter — entity-workbench-go

Canonical. Living doc — edit in place as understanding evolves.
This is **the** rule set for the workbench-go project.

The charter has three jobs:

1. Name the rules we hold ourselves to (the D-disciplines).
2. Give us a checklist to run on every diff (the review questions).
3. Pin the bug classes we've already paid for so we don't pay again
   (the anti-pattern catalog).

Anything you propose in this repo is measured against this charter.
If a proposal violates a discipline, the proposal changes — or the
discipline does, with an explicit edit here and a reason.

---

## 1. What we're standing on

The workbench-go project is the **substrate-supporting application**
for the entity-system on the Go side. We ship:

- `entitysdk/` — the ergonomic SDK layer (will eventually move out).
- `workbench/` — **renderer-neutral models** (`wb.LogFilterModel`,
  `wb.TreeBrowserModel`, `wb.MarkdownFilesModel`, etc.). Every
  panel-shaped surface across every renderer is backed by one
  of these.
- `shellcmd/` — verb dispatch + cross-renderer integration.
- `shellboot/` — boot wiring shared by all renderers.
- `entity-shell` — the primary CLI (per `SHELL-DIRECTION.md`).
- `entity-console` — the Bubble-Tea TUI.
- `entity-avalonia` — the Phase-I Avalonia/.NET desktop renderer.

**The two-renderer fact.** Both `entity-console` and `entity-avalonia`
are presentation layers over the same `workbench/` models. A
`wb.TreeBrowserModel` instance backs `console/tree_view.go` AND
`avalonia/frontend/Panels/TreeViewPanel.cs`. Features land in
`workbench/`; renderers do nothing but present. **This is the
load-bearing structural invariant of the project** — formalized as
D18.

**Maintenance posture (the asymmetry we accept).**

| Renderer | Posture | Why |
|----------|---------|-----|
| `entity-avalonia` | **Actively developed** (Phase I). New panels, new patterns, new framework discoveries land here. The `GUIDE-AVALONIA-PANEL-PATTERNS.md` recipes + `MODEL-AVALONIA-RUNTIME.md` are Avalonia-specific. | The desktop renderer is where the architectural surface (multi-peer, rich rendering, real UI threading) is being exercised; that's where the bugs surface and the disciplines get earned. |
| `entity-console` | **Passively maintained.** Inherits every `wb.*Model` change at compile time via Go's type system. No active feature development. Tview's primitives (`TextView` scrollback, `TreeView` widget) don't share Avalonia's failure modes — no per-block paint recursion, no compositor batch pipeline, no GC-handle race. No parallel patterns guide owed. | The TUI's value is the SSH / headless / single-peer use case. Keeping it green via `make build` is cheap; rewriting it for every model contract change is expensive and pointless when nothing has earned the work. |
| `entity-shell` | **Primary CLI** (per `SHELL-DIRECTION.md`). Verb dispatch only — not a renderer in the panel sense, but a shellcmd consumer. | Verbs are the shared substrate that BOTH `entity-console` (interactively) and `entity-avalonia` (via bridge) drive through. Features land here when they have a verb shape. |

**The gate that holds D18 honest.** `make build` from the repo root
must keep all three binaries (`entity-shell`, `entity-console`,
`entity-publish` + friends) compiling. A model contract change that
breaks `entity-console` is a discipline signal — either the model
broke its contract (fix the model) or the renderer's adaptation has
drifted (fix the renderer in the same commit). Running just
`make test-workbench` is necessary but not sufficient; the
two-renderer **compile gate** is the cheapest D18 enforcement we
have today.

Stacked layer view (deepest at bottom, ours at top):

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

L4-L7 we wrote. L5-L6 is our contract — implicit, becoming explicit.
L0-L3 we discover by crashing. This charter is the discipline that
turns "discover by crashing" into "discover by reading and modeling."

The companion runtime model (`MODEL-AVALONIA-RUNTIME.md`) is where
L0-L3's actual behavior gets documented forensically.

---

## 2. The 19 disciplines

D1-D11 are inherited verbatim from the entity-OS discipline charter
(originating in godot-entity-core-rust, ratified by egui-entity-core-rust).
They are stack-agnostic; they govern how we use the substrate.

D12-D19 are native to our stack (Avalonia + .NET + cgo + Go + the
two-renderer architecture). They are **earned** by shipped bugs and
explicit feedback episodes. Each cites the commit or pin that proved
we needed it.

### Inherited from the entity-OS layer (D1-D11)

| #   | Rule | How to apply |
|-----|------|--------------|
| D1  | Use the kernel; don't reinvent extensions | Reactivity → subscription. Audit → history. Lookup → query. Long ops → continuation. If we built our own dispatcher, our own watch loop, our own cache layer — we did it wrong. |
| D2  | L1 dispatch is the default; L0 is the back door | App writes go through `shellcmd` verbs (validated). Raw `tree:put` / direct `Store` access is exceptional, named, and gated. |
| D3  | Capability-typed dispatch (surface present) | Verbs declare `required_capability`. Contexts carry held-cap sets. Fail closed. Permissive default OK today; the surface must exist. |
| D4  | Bounded interfaces; one channel does one thing | Action dispatch dispatches actions. Selection sinks carry selection. Panel hosts host panels. No god channels. No "events" enum that grows forever. |
| D5  | Declarative composition; boot dependencies declared | Every renderer's boot path lists its dependencies (peer manager, bridge, panel registry, shell pump). Missing dep fails at boot, not at first use. |
| D6  | Per-host namespaces formalized | Per-window namespace resolves panel mounts, action targets, selection sinks. Cross-window via explicit handle, not global lookup. |
| D7  | Kernel keeps working when apps misbehave | **Symmetric state.** Every push has a paired pop. Every subscribe has a paired unsubscribe. Every open has a paired close. A panel crash must never crash the host. |
| D8  | Trust the spec; surface drift, don't normalize it | Spec is read *against* code, not *as* code. File questions to `reviews/`; don't paper over with workarounds. (See `feedback_verify_protocol_claims`, `feedback_check_spec_before_extending`.) |
| D9  | Memory & store accounting | **Runtime:** every add has a paired remove identified at the same change OR an explicit exemption. **Persistence:** every entity write has a writer / reader-at-boot / GC story OR exemption. |
| D10 | Real-session coverage | Cross-boot + headed + real-store paths for load-bearing changes. Headless green is necessary, not sufficient. Eight crash-hunt commits proved this on the Avalonia side. |
| D11 | Inventory-boundary declaration (meta) | At audit open: name what's in scope **and what's not.** Findings that surface outside the boundary extend the boundary for the next audit. |

### Native to our stack — earned by shipped bugs (D12-D17, D19) and feedback episodes (D18)

**D12 — Cross-language lifetime accounting.**
*Source:* the cgo + GCHandle FFI discipline.
*Why:* every C# delegate handed across the cgo boundary lives on the
.NET GC heap; Go holds it as a raw function pointer. If the .NET GC
collects it while Go still has the pointer, the next call SIGSEGVs.
Conversely, every Go panic that crosses cgo without a recover aborts
the host process. **The two heaps don't know about each other.**
*How:* every delegate handed to Go is `GCHandle.Alloc`-pinned for the
lifetime of the Go-side use. Every cgo export that touches the store
defers `recoverToErrorEnvelope`. Goroutine cleanup joins via a
`wakeDoneCh` before the C# side releases the GCHandle.
*Enforcement:* grep `GetFunctionPointerForDelegate` in C# — each must
be paired with a `GCHandle.Alloc` in the same scope or registered to
a long-lived `Bridge.handles` list. Grep `//export ` in Go — each
must defer `recoverToErrorEnvelope`.
*Grounded by:* commits `958b3fe`, `f95c0c6` (AP2, AP3).

**D13 — UI-thread / dispatcher integrity.**
*Source:* the Avalonia dispatcher contract.
*Why:* Avalonia has one UI thread; all visual-tree mutation and all
input dispatch run on it. Any operation that scales with input size
(parse, attach, layout, network) **must** yield the dispatcher at
frame cadence. A blocked UI thread is indistinguishable from a hung
app. A panic during paint takes the whole window down with no
managed dump.
*How:* operations >5 ms emit in batches; between batches the
dispatcher is `Post`-ed at `Background` priority so input/scroll/paint
interleave. Render work is cancellation-aware (a new render
cancels the in-flight one). No `Wait()` / `Result` on Task in any
panel code path that runs on the UI thread.
*Enforcement:* grep `.Result\|.Wait(\|GetAwaiter().GetResult()` in
`avalonia/frontend/` — each must be justified or removed. New render
pipelines lift the `MarkdownViewPanel.SwapBody` shape: chunked emit
+ cancellation token + adaptive batch sizing.
*Grounded by:* commits `7d92934`, `8b3963e`, and the still-open
PHASE-I-RELIABILITY-PLAN follow-on #4 (AP8, AP9).

**D14 — Cross-language IPC discipline (JSON envelope contract).**
*Source:* our own bridge surface (`avalonia/bridge/`).
*Why:* every cgo call uses a JSON envelope with ok/error binary
outcome. Any drift between Go-side and C#-side schema produces
silent-fail / mis-decode bugs that are agonizing to diagnose. Panics
in Go must be transformed into error envelopes at the cgo seam, not
propagated as host process aborts.
*How:* the envelope shape is fixed (`{ok|error|result|handle}`).
Every cgo export validates its input shape, returns an error envelope
on failure, never propagates Go panics. C# decoders fail loudly on
unexpected shape (not silently). The envelope schema is documented in
`MODEL-AVALONIA-RUNTIME.md §6` (Boundary C).
*Enforcement:* the Go side has `recoverToErrorEnvelope`; the C# side
has a single `Bridge.Decode` chokepoint. The schema must be checked
into the model doc before being widened.

**D15 — Bounded payloads.**
*Source:* Avalonia visual-tree and Skia paint-recursion limits.
*Why:* no visual-tree node may have unbounded children. A single
`SelectableTextBlock` with 2880 inlines blew Skia's paint recursion
(commit `7d92934`). Any unbounded `ObservableCollection` will,
sooner or later, do the same. `ItemsControl` without virtualization
is a future crash waiting for production data.
*How:* every panel that renders a list has either:
(a) virtualization (`ItemsControl` with `VirtualizingStackPanel`),
(b) a hard per-block cap (e.g., `MaxInlinesPerBlock = 500`),
(c) a top-N + paginate, or
(d) server-side `HasMore` truncation with a user-visible cap.
Pick one explicitly; no implicit unboundedness.
*Enforcement:* every new panel review answers question 6 (below).
*Grounded by:* commit `7d92934` (AP8); pending fixes in
PHASE-I-RELIABILITY-PLAN follow-ons #5-#9.

**D16 — Test-depth honesty.**
*Source:* the headless / Xvfb / real-display tier split.
*Why:* the headless harness (no Skia, no X11) catches one bug class.
The Xvfb harness (real Skia + X11 + paint pipeline) catches a
different one. Stress mode (cycled renders) catches a third. Calling
"all tests green" without naming the tier is the AP4 / AP5 / AP6
pattern: green-in-the-cheap-tier blinded us for three of the eight
crash-hunt commits.
*How:* the four-tier taxonomy is documented in
`TESTING-STRATEGY.md`. Every CI gate names the tier. Every claim of
"this is fixed" names the tier the fix was verified against. Headless
green is never the standard for a render-pipeline change.
*Enforcement:* the term "passes" in any commit message must be
qualified by the tier (e.g., "passes headless + xvfb-smoke"; bare
"passes" is a smell).
*Grounded by:* commits `6c09f09`, `f25e7cb`, `1467985` (AP4, AP5, AP6).

**D17 — Persistence honesty (wipe-and-rebuild).**
*Source:* `DEPLOYMENT-DIRECTION.md §1` — wipe-and-rebuild is the
operational posture; identity bundles are the one thing that survives.
*Why:* anything written to the local store is presumed disposable.
Identity bundles are the one exception; they go through the
identity-bundle backup path. If a future feature stores
non-reproducible state outside that path, it gets lost on wipe and
the user finds out by losing their work.
*How:* for every store touched, document: what survives wipe / what
gets re-derived / where backup lives / cold-return story. Sensitive
material is either never-at-rest or encrypted-at-rest (per the
`reference_delete_is_not_erase` memory pin).
*Enforcement:* PRs adding new persistent state name the
wipe-and-rebuild story in the description. Audit pass on every
phase close.

**D18 — Renderer-agnostic substrate.**
*Source:* the two-renderer architecture (`avalonia/` + `console/` over
shared `workbench/` models) is the project's load-bearing structural
invariant. Earned via `feedback_no_renderer_duplication`
("shared shell↔workspace integration lives in `shellcmd`; renderers
wire references") and `feedback_no_forced_renderer_parity`
("constraint is clean-core discipline; canvas demoted-not-deleted")
plus `project_avalonia_frontend_guidelines` ("renderer-neutral models
in `workbench/`").
*Why:* model semantics belong to the substrate, presentation
concerns belong to the renderer. Features that drift into a
renderer become invisible to the other and unportable. Conversely,
presentation choices that drift into the model force every renderer
to inherit a constraint that may not apply to it (a TUI doesn't
have a visual tree to pressure; a desktop renderer does). The
boundary keeps both honest.
*How:*
- **Model semantics** (filtering, ordering, paging contracts,
  subscription wiring, capability checks, content meaning) live in
  `workbench/*Model.go`. Every renderer instantiates the same model.
- **Presentation concerns** (row caps to bound a visual tree,
  framework-specific debouncing, recycle/diff strategies, the chosen
  P1–P6 pattern for a given list) live in the renderer. They
  may differ per renderer without violating the discipline.
- **Verbs** (anything a user might type) live in `shellcmd/`. A
  renderer **never** owns a verb; it dispatches through `shellcmd`.
- **Feature work goes into `workbench/` or `shellcmd/`.** A new
  panel kind opens with `wb.{Feature}Model` first; the renderer
  surface comes second. If a renderer-side change has no companion
  edit somewhere under `workbench/`, `shellcmd/`, or `shellboot/`,
  it is by definition not a feature — it is presentation work or
  a pattern application.
*Enforcement:*
- Every renderer file in `console/*.go` and
  `avalonia/frontend/Panels/*.cs` must consume a `wb.*Model` (Go
  side) or its bridge surface (Avalonia side). No renderer-owned
  state model.
- `make build` (Go side) + `make test-workbench` are the
  cross-renderer gates: a model change must keep BOTH the
  Bubble-Tea TUI and the Avalonia bridge compiling. If a model
  change breaks `entity-console` build, the model change is wrong
  (or the renderer's contract changed and both sides need an edit
  in the same commit).
- Cross-pattern table in `GUIDE-AVALONIA-PANEL-PATTERNS.md §9` is
  renderer-specific by design — the TUI's analog (terminal
  scrollback handles its own caps; tview rendering owns its own
  primitives) does not need an equivalent table.
*Grounded by:* the entire two-renderer codebase. Specific anchors:
`workbench/log_model.go` consumed by `console/log_viewer.go` and
`avalonia/frontend/Panels/LogViewerPanel.cs` via
`Bridge.LogOpen`/`LogRender`. Same model, two presentations.

**D19 — A layer needs no change only once the operation has run.**
*Source:* two findings a session apart, same shape. (1) `b3848c1` — we
told `entity-browser-rust` the Go arm supported foreign-namespace
subscription "by construction," reasoning correctly from `parsePattern`;
the gate test that turned the reading into a measurement failed on its
first run and surfaced a `send on closed channel` in the watch hub, one
layer below the code we had read. (2) `REVIEW-SHARE-AND-CONNECTIVITY-
ALIGNMENT-2026-08-17` W3 / §8.2 — we told core-go that landing a mirror
at `/{them}/…` needed nothing from them because `applyPrefix` is plain
concatenation. It is, and the merge still 403s: `tree:merge` pre-checks
put authorization per target path, and a self-issued `Resources: ["*"]`
is peer-local under §PR-8. Both readings were correct about the function
they read and wrong about the operation.
*Why:* reading a code path establishes what that path does. It does not
establish what the operation does, because an operation crosses layers
the reader did not open — a capability pre-check, a lock released
before a send, a lifecycle the callee owns. The failure mode is
specific and it is not sloppiness: the more precisely a claim is
argued from source, the more confidently it gets routed to another
seat, and the more expensive it is when the layer underneath disagrees.
*How:*
- A claim that a layer, an impl, or a sibling repo **needs no change**
  is not established until the operation has been **executed end to
  end** against real peers. Until then it is a hypothesis and is
  labelled one in the packet.
- The measurement is the deliverable, not the argument. "Supported by
  construction" belongs in a doc comment, never in a routed claim.
- When the measurement contradicts the reading, correct **at the point
  of the claim** in the original packet, and say which half was right —
  the reading usually was.
*Enforcement:* every routed "no change needed" row carries a test name
or is marked unmeasured. `entitysdk/foreign_namespace_subscription_test.go`,
`entitysdk/mirror_test.go::TestMirror_ForeignNamespaceMergeNeedsItsOwnCapability`,
and `shellcmd/cmd_revision_mirror_test.go` are the three that exist
because of this rule.

---

## 3. The ten review questions (run on every diff)

Short enough to run on every change. Six inherited, four substrate-native.

1. **Which layer is this?** L8 / L7 / L6 / L5 / L4 / L3 / L2 / L1 / L0.
2. **What kernel service does this consume / reimplement?** If we
   reimplement, justify against D1.
3. **What's the capability surface?** Is the privileged op gated, is
   the held-cap set explicit, does it fail closed? (D3)
4. **Failure mode if the kernel misbehaves AND if this code misbehaves?**
   Symmetric (D7).
5. **What's the accounting?** Every delegate / handle / subscription /
   collection-add → paired drop. Every persisted entity → writer /
   reader-at-boot / GC story. (D9, D12)
6. **What's the bound?** For lists / collections / inline runs: cap,
   virtualization, paginate, or `HasMore`? (D15)
7. **Does the test cross the real loops?** Headless, xvfb-smoke,
   xvfb-stress — which tiers pass? Named explicitly. (D10, D16)
8. **Can this block or crash the UI thread?** If so, does it kill the
   dispatcher? (D13)
9. **What persists, where, with what wipe-and-rebuild story?** (D17)
10. **Is this feature or presentation?** If feature, does it land in
    `workbench/` or `shellcmd/` so every renderer inherits it? If
    presentation, is the renderer-specific pattern (P1-P6, debounce
    cadence, cap value, recycle strategy) named and motivated? Does
    `make build` keep both `entity-console` and `entity-avalonia`
    compiling? (D18)

---

## 4. The anti-pattern catalog (AP1-AP14)

Each a real defect that shipped or a claim that was routed, diagnosed, and
is now pinned by a regression test.
**The discipline they ground exists so they don't recur.**

| AP  | Commit  | Pattern (the name we use for it) | Discipline |
|-----|---------|----------------------------------|------------|
| AP1 | `d742050` | Runtime diagnostic output silenced (`DOTNET_EnableDiagnostics=0` blocked managed crash dumps for four crashes before we noticed). | D13, D16 |
| AP2 | `958b3fe` | Wake-callback delegate GC'd while Go held the pointer; Go panic on bad path crashed the host. | D12 |
| AP3 | `f95c0c6` | The AP2 lifetime defect existed at every panel's wake-registration site, not just the one we found. Sweep when you find a pattern. | D11, D12 |
| AP4 | `6c09f09` | Tests stubbed the renderer; the real pipeline was never exercised. Headless-green meant nothing. | D10, D16 |
| AP5 | `f25e7cb` | Headless tests ran with `UseHeadlessDrawing=true` — Skia was skipped entirely. The "renderer" tests didn't test the renderer. | D10, D16 |
| AP6 | `1467985` | Synthetic test docs didn't match the docs users actually read. Real fixtures (`docs/architecture/*.md`) catch what lorem ipsum can't. | D10 |
| AP7 | `8b3963e` | Stack overflow left no managed dump, no useful stack trace, no log. Diagnostic blind spot. | D13 (observability surface) |
| AP8 | `7d92934` | One `SelectableTextBlock` with 2880 inlines blew Skia's paint recursion. Unbounded visual-tree children = future crash. | D15 |

**Pending — under investigation.** A structural panel-level mitigation has
landed (LoadPathDebounce raised from 150ms to 400ms, applying pattern P3).
Underlying Avalonia compositor pinning remains open and is tracked
in `MODEL-AVALONIA-RUNTIME.md §4 + §6 Boundary A`. The forensic
process — deep-dive published, hypothesis falsified by measurement,
revised, mitigation engineered + verified — is itself a recorded
example of the D8 (trust the spec, surface drift) discipline. **AP9
joins the catalog** to pin the falsification:

| AP9 | `PurgeFontCache` + forced-GC between renders | Tried as the deep-dive's predicted fix for the strike-cache LRU race; direct measurement showed cache at ~10% budget at crash, and the interventions made the crash strictly worse. The lesson: trust direct measurement over predicted models; if a fix candidate makes the symptom worse, the hypothesis is falsified, not the implementation. | D8 (trust the spec, surface drift), D13 |

**The share-arc pair (2026-08-18).** Both from §5.1 steps 3a/3 of
`reviews/REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17`. Neither is a
crash; both are the same family as AP4/AP5 — *a check that looked done and
wasn't exercised.* AP10 is what earned **D19**; AP11 is what caught AP10's
fix being mis-measured.

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP10 | `REVIEW-…-2026-08-17` §8.2 | **The layer under the layer you read.** `applyPrefix` was read correctly and the conclusion — "an absolute target needs no change anywhere" — was still wrong: `tree:merge` pre-checks put authorization on every target path, and a self-issued `Resources: ["*"]` is peer-local under §PR-8, so every `/{them}/…` merge 403s. Routed to core-go as "no change requested" before the operation had ever been run. | D19, D8 |
| AP11 | `entitysdk/mirror_test.go` | **A dispatched read of a peer-qualified path is a REMOTE read.** The mirror test's positive assertion used `AppPeer.List("/{them}/…")`, which routes to *that peer* — so it reported the publisher's own tree back to us, and stayed green with the target prefix reverted. Mirrors are asserted against `Store()` (L0), never `List`/`Get` (L1 dispatch). Found by mutation-checking the pin, not by review. | D2, D10 |

**AP12 — the inbound packet nobody opened (2026-08-18).** Catalog-level only: it has
bitten once, so per §5 it is **not** a discipline yet.

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP12 | arch `ROUTING-2026-08-17-l` §2 | **Filing is not receiving.** A packet addressed to this repo by name carried a live conformance finding against `publish/publish.go`. We never opened it — no `reviews/` row, no backlog entry, no status line — and shipped three commits of feature work past it. It surfaced a day later only because our own new reader (step 3a) contradicted our own publisher. The counterpart of arch's own **L13 — filing is not routing**, from the receiving end. | D8, D11 |

*Enforcement (the reason this is worth writing down at all):* session start reads the
sibling `entity-system-architecture` repo for packets addressed to us, and any found
gets a row in `docs/status/STATUS.md` or `docs/architecture/reviews/` **in that session,
before feature work starts.** Added to `AGENTS.md`. If it bites a second time in a
different shape, promote it.

**AP13 / AP14 — the publisher-conformance pair (2026-08-18).** From the Exit-B build in
`publish/`. Both catalog-level; each has bitten once.

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP13 | `HANDOFF-2026-08-18` §2 vs `publish/signed_root.go` | **Pricing an obligation off our own code's shape.** We deferred the release-critical exit for a day because "our closure walk is shallow and D3 requires the trie closure." True about `publish/`'s walker over bound entities; irrelevant to the obligation, because `tree.CollectNodeClosure` in core-go already implemented D3 exactly *and cites the same amendment in its doc comment*. The estimate measured our code instead of the requirement. **Before pricing a spec obligation, grep the substrate for it** — the sibling that owns the primitive usually already shipped it. | D18 (read canonical sources), D8 |
| AP14 | `publish/publish.go` filter refusal | **A filter is not a confidentiality control once you sign.** `Opts.IncludePath`/`IncludeType` looked like "publish less." Under a signed root they publish *more*: the §6.5.3 closure obligation uploads every leaf-bound hash the root commits to, filtered-out entities included, hash-addressed and fetchable. Withholding them instead shortens a consumer's enumeration **silently** (browser-rust measured 1 of 24 names hidden with no error). The emitter now refuses the combination. **When a new invariant makes an existing knob unsafe, remove the knob at the emitter — do not document around it.** | D3, D19 |

*Enforcement:* `publish/publish_test.go::TestPublish_FilterHooks` pins the refusal (and that
nothing is written before it), `TestPublish_SignedRootVerifiesFromTheEmittedFiles` pins the
closure completeness from the consumer's vantage point, and
`TestPublish_SeqAdvancesAcrossRuns` pins the §6.5.6 republish MUST that the upstream engine
does not hold on its own.

---

## 5. Promotion criteria — when does something become a discipline?

Disciplines are **promoted on bug-evidence, not speculation.** The
rule is:

- A pattern that bit us once goes in the anti-pattern catalog (AP).
- A pattern that bit us twice, in different shapes, becomes a
  discipline (D).
- A discipline added on speculation ("we should probably...") has
  to either be earned within one release cycle or removed.

D12-D17 were earned by the eight crash-hunt commits. D18 was earned
by two explicit feedback episodes (different shapes, same lesson:
the boundary between substrate and presentation is structural, not
stylistic). **D19 was earned by AP10 plus the watch-hub crash of
`b3848c1`** — two arguments-from-source, a session apart, each correct
about the function it read and wrong about the operation; different
subsystems, same shape. Future D20 waits until it's earned the same way.

---

## 6. Enforcement surfaces

| Discipline | How enforcement happens today | What we owe |
|------------|-------------------------------|-------------|
| D1-D11     | Code review, this charter cited | (inherited from entity-OS) |
| D12        | `recoverToErrorEnvelope` defer on every cgo export; `GCHandle.Alloc` paired with every delegate handed across | grep checks in CI |
| D13        | Adaptive emit pattern in `MarkdownViewPanel`; PanelLog breadcrumbs | Pattern catalog (Step 3) |
| D14        | Single `Bridge.Decode` chokepoint; envelope schema | Schema doc in `MODEL-AVALONIA-RUNTIME.md §6` |
| D15        | Per-block split + chunked emit in MarkdownView | Cross-panel pattern application (PHASE-I-RELIABILITY-PLAN #5-#9) |
| D16        | Four-tier test taxonomy (planned) | `TESTING-STRATEGY.md` + CI gate naming the tier |
| D17        | `DEPLOYMENT-DIRECTION.md §1` already names wipe-and-rebuild | PR template prompt |
| D18        | `make build` + `make test-workbench` exercise both renderers' inheritance from `workbench/*Model`. Renderer files (`console/*.go`, `avalonia/frontend/Panels/*.cs`) review-gated against owning local state. | `make` target that explicitly runs the two-renderer build matrix and reports which renderer broke first; eventual lint that flags state owned by a renderer file. |

The CI / tooling enforcement (greps, gates) is downstream work — the
charter has to land first, then we wire enforcement to it.

---

## 7. What this charter does NOT do

- **Does not specify implementation.** The disciplines say what
  invariants hold; `GUIDE-AVALONIA-PANEL-PATTERNS.md` says how to
  achieve them; `MODEL-AVALONIA-RUNTIME.md` says what the platform
  actually does. Three docs, three jobs.
- **Does not freeze the rule set.** Disciplines are promoted on
  evidence (§5). When the platform changes (Avalonia 12, Wayland
  default, .NET 10), invariants change with it. Edit this file in
  place.
- **Does not absolve responsibility.** A passing review-question
  checklist doesn't make a bad design good. The questions are
  necessary, not sufficient.

---

## 8. Reading order

For a new contributor (or a future-self cold start):

1. **This file** — the rules.
2. `MODEL-AVALONIA-RUNTIME.md` — the platform we're standing on.
3. `GUIDE-AVALONIA-PANEL-PATTERNS.md` — the recipes.
4. `TESTING-STRATEGY.md` — what each test tier proves.
5. `LOGGING-CONVENTIONS.md` — how to leave a trail.

`AGENTS.md` at the repo root points here as the entry point for any new
Avalonia-side architectural work.

---

## 9. Pinning

When a discipline shifts, edit this file in place. The charter records
the rules.
