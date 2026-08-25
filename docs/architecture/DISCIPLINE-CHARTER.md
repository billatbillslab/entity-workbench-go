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

## 2. The 22 disciplines

D1-D11 are inherited verbatim from the entity-OS discipline charter
(originating in godot-entity-core-rust, ratified by egui-entity-core-rust).
They are stack-agnostic; they govern how we use the substrate.

D12-D22 are native to our stack (Avalonia + .NET + cgo + Go + the
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

### Native to our stack — earned by shipped bugs (D12-D17, D19, D20, D21, D22) and feedback episodes (D18)

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

**D20 — Price the work against the substrate, not against our own code.**
*Source:* two estimates a session apart, same class, different shape.
(1) `HANDOFF-2026-08-18` §2 priced the conformant publisher exit as "its
own arc" because *our* closure walker is shallow and the spec requires
the trie closure. Both halves true; the conclusion wrong, because
`tree.CollectNodeClosure` in `entity-core-go` already implemented the
obligation **and cited the same amendment in its doc comment**. The
estimate measured our code instead of the requirement, and the arc
collapsed to one session. (2) The connectivity scoping — ours and
`entity-browser-rust`'s independently — listed four app-tier pieces as
missing. Two of the four (`system/peer/status` liveness, the
`maintain-peer` / reconnect graph) were already implemented in the
kernel, in `core/peer` and `ext/network`, needing registration and a
consumer rather than authoring. A third (WebSocket transport) was
already working and needed only to be *advertised*.
*Why:* we sit on a kernel we did not write and do not read daily. An
absence in our tree is evidence about our tree and nothing else, yet it
reads as evidence about the system — which is how a one-session task
gets deferred as an arc, and how a "we need to build X" scoping survives
into a plan when X exists one directory over. The failure is
directional: it always over-estimates, so it never announces itself as a
surprise, only as work that quietly did not happen.
*How:*
- Before estimating anything that names a spec obligation or a protocol
  surface, **grep the substrate for it by name** — the primitive, the
  amendment number, the entity type. `../entity-core-go` first, then the
  spec.
- A scoping row that says "we don't have X" states **where it looked**.
  "Absent in `entitysdk/`" and "absent in the cohort" are different
  claims and only one of them justifies building X.
- When the substrate already has it, the work is registration, wiring
  and a consumer — plan *that*, and say so in the packet, because the
  sibling seat that scoped it for us is carrying the same wrong estimate.
*Enforcement:* every "we need to build X" line in a handoff, plan, or
routed packet carries the search that established the absence (a
`file:line` miss, or a named grep). The three that exist because of this
rule: `publish/signed_root.go`'s note on `CollectNodeClosure`,
`entitysdk/peer_status.go`'s division-of-labour note (core/peer owns the
imperative half), and `entitysdk/network.go`'s (the handler owns the
reactive half).

**D21 — Every packet that names this repo is inbound. The `To:` line is
not the filter.**
*Source:* AP12 promoted — two misses, a day apart, in different shapes.
(1) `ROUTING-2026-08-17-l` §2 was addressed to us by name, sat unopened,
and we shipped three commits past a live conformance finding against
`publish/publish.go`. The enforcement we wrote for it said *"session
start reads the sibling repo for packets **addressed to us**"* — and
that sentence is precisely what let the second one through. (2)
`ROUTING-2026-08-18-l` was **cc'd** to us with the note *"§1 changes the
default dispatch table"*. It re-keyed `EXTENSION-REGISTRY` §4.1 step 2
from remoteness to name transmission — a MUST we enforce as a refusal in
`ValidateResolverConfig`. We did not open it, and our guard went on
rejecting a config the spec permits until arch told us twice, in two
later packets, in different words.
*Why:* a routing header describes who **owes work**, not who is
**affected**. The seat that has to change code is on the `To:` line; the
seat whose landed behavior just became wrong is very often on the `cc`.
Those are different questions and only one of them is answered by the
header. A cc is also the cheaper miss to make — nothing is owed, so
nothing chases it.
*How:*
- Session start greps the sibling `../entity-system-architecture` for
  **every** document naming this repo, not the ones whose header names
  us: `grep -ril 'workbench-go' ../entity-system-architecture/docs/status/`.
  Read anything newer than the last one STATUS acknowledges.
- STATUS records the **last packet letter read**, so the gap is a
  subtraction rather than a judgement.
- A packet that says it changes a table, a default, or a MUST is read
  the same session regardless of header. Those three words are the
  trigger.
*Enforcement:* `AGENTS.md`'s session-start line names the grep and the
letter-tracking; `docs/status/STATUS.md` §"Latest arch packet read"
carries the letter. If a packet naming us is found unread again, the
next step is a checked-in script, not a third prose rule.

**D22 — A contract between two components is only tested by a test that
crosses it. Per-side tests are evidence about each side.**
*Source:* AP17 plus AP21 — two shapes, two weeks apart, one lesson.
(1) `DispatchLocalExecute` claimed to be *"the in-process equivalent of
wire EXECUTE"*; both sides had tests, the equivalence test asserted
**return values**, and the resource stopped reaching the handler with
every assertion still green (323 failures in our tree, zero in the
kernel's). (2) Our own CDN corridor: `publish` had a test asserting the
layout it emits, `fetch` had a test asserting the URLs it builds, and
they were **four different layouts** — no `tree/` segment vs one,
`sharded-2-4` vs `sharded-2-flat`, wire hex vs digest-only hex,
Amendment 6 pointer vs raw hash. Both suites green the whole time, and
it took another implementation asking us to consume *them* to discover
it about *ourselves*.
*Why:* a per-side test asserts the side's own idea of the contract, so
two sides can each be self-consistent and jointly wrong — and nothing in
either suite can say so, because the contract exists in neither file. The
failure is silent by construction: green on both ends is exactly what a
broken join looks like. It is D19's *"route the measurement, not the
argument"* turned inward — reading (or testing) one half establishes that
half, not the operation.
*How:*
- Every emitter/consumer pair, every in-process/wire equivalence claim,
  and every cross-repo artifact gets **one test that runs the whole
  path** over the real artifact — real HTTP, real emitted directory, no
  fakes on either side.
- The gate asserts what the **far side sees**, not what the near side
  produced: the callee's context, the consumer's decoded entity, the
  bytes off the wire.
- Cross-impl: point it at the **other implementation's** bytes, frozen
  in `testdata/` with provenance. A corridor verified only by its own
  language's other half is cohort-consistent, not convergent
  (ADR-0012).
*Enforcement:* `publish/consume_test.go::TestPublishThenFetch_TheTwoHalvesOfOurOwnCorridor`
(our publisher → our consumer, over `httptest`) and
`fetch/crossimpl_test.go::TestConsumeBrowserRustSite` (their emission,
frozen at `fetch/testdata/crossimpl-rust-site/`). Both are in
`make test-native` — which is half the discipline: `fetch` had **no test
target at all** and `publish` was outside the sweep, so the corridor
could not have been caught by running everything. A boundary gate that
is not in the default sweep is not a gate.

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

## 4. The anti-pattern catalog (AP1-AP24)

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

> **PROMOTED to D21 (2026-08-18).** It bit a second time the same week, in the shape the
> enforcement sentence above left open: `ROUTING-2026-08-18-l` was **cc'd** rather than
> addressed to us, re-keyed the §4.1 step 2 MUST our validator enforces, and went unread
> while the validator went on refusing a legal config. *"Packets addressed to us"* was the
> filter that let it through, which is why the discipline is keyed on the repo name
> appearing anywhere in the document rather than on the header.

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

**AP15 / AP16 — the audit pair (2026-08-18).** Both earned by the audit session that followed
the publisher/connectivity work. Catalog-level; each has bitten once.

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP15 | `HANDOFF-2026-08-18-b` §4 vs the audit's per-suite run | **A failure count read through a stopping build.** We reported the kernel block as *"all of `programs`, `shellcmd` (71), plus five in `entitysdk`"* and called `shell`/`shellboot` green. `make test` **stops at the first failing package**, so every number after the first was invisible: the real radius was **323 across 6 of 9 suites**, `entitysdk` alone was 171, and the two "green" suites had 26 failures between them. We then routed those numbers to another repo. **A blast-radius number from a fail-fast runner is a lower bound, not a measurement** — run each suite to completion (`make test-<pkg>`) before any count leaves the tree. | D9 (accounting), D17 |
| AP16 | `entitysdk/resolver_config.go` `catchall_not_last` | **A refusal outliving the sentence that justified it.** We enforced "the catch-all must be last" as a 400 because §4 (1.6) said the dispatch list was first-match-wins. Arch withdrew that sentence 78 minutes after we shipped; under 1.7 the list is a filter and later entries stay eligible, so our refusal **rejected a config the spec permits**. A refusal is the most expensive thing to get wrong — it is the one behavior an operator cannot work around. **When the spec sentence a refusal rests on moves, the refusal is the first thing to re-derive**, and a refusal should cite the sentence in its error text so the coupling is greppable. | D8 (surface spec drift), D3 |

*Enforcement:* AP15 — the STATUS §6 inventory is per-suite and any packet quoting a failure count
names the command that produced it. AP16 — `TestValidateResolverConfig_CatchAllPositionIsNotADefect`
pins the withdrawal by name, and every refusal in `ValidateResolverConfig` carries its spec citation
(`§4.1 step 2`) in the error string, so `grep -rn "§4" entitysdk/*.go` enumerates what a spec change
must be re-read against.

**AP17 — equivalence asserted on the result, never on the context (2026-08-18).** Earned in
core-go's tree, on our finding, and kept here because **we are the seat that exercises the path**.

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP17 | core-go `0b9e261` → `7593618`, found from `entity-workbench-go` | **Two entry paths that claim equivalence, tested only on their return value.** `DispatchLocalExecute` is documented as *"the in-process equivalent of wire EXECUTE"*. `dispatch_equivalence_test.go` asserted the two paths returned equal **results**; `subdispatch_resource_dimension_test.go` drove both sub-dispatch directions from a hand-built parent context. Neither ever entered through the entry point, so when the resource stopped reaching the handler, **every result stayed equal and the handler-visible context was silently different** — 323 failures in our tree, zero in theirs. **When two paths claim equivalence, the test must assert what the callee SEES, not what the caller GETS.** The corollary for us: an app repo is the seat that walks the SDK path, so a kernel gap that only that path exercises is ours to find and route, never to work around. | D10, D19, D8 |

*Enforcement:* the kernel-level reproducer shape (drive the real entry point, record
`req.Context.*` inside the handler, assert on the recording) is the pattern for any future
in-process/wire equivalence claim we depend on. It landed in core-go as
`TestDispatchLocalExecute_CarriesResourceToHandler`; ours lives in the routing packet
`reviews/CORE-GO-LOCAL-DISPATCH-RESOURCE-2026-08-18.md` rather than in our suites, because a test
asserting kernel behavior belongs in the kernel's tree.

**AP18 / AP19 / AP20 — the v1.13 adoption trio (2026-08-18).** Earned on one session: re-cutting
the cross-impl fixture arch asked for, and adopting `EXTENSION-REGISTRY` 1.13. All three are
catalog-level; each has bitten once. **AP18 and AP20 are the same family as AP13/AP10** — a claim
about something outside our source, settled from inside it.

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP18 | `publish/cmd/crossimpl-fixture/main.go` doc comment vs `diff -r` of two runs | **A claim about emitted bytes, verified by reading the code that emits them.** We told `entity-browser-rust` the fixture was *"byte-identical on your machine"* and wrote a doc comment naming `advertised_at` as the one unstable field, *"not part of the signed root's closure"*. We had read the source for fields that looked like clocks. `published_at` is a field **of the published-root entity**, so a fresh clock moved the root's content hash, the `system/signature/{root_hex}.bin` binding named after it, two content shards and `{out}/manifest` — everything a consumer's reader enters through. The check is one line of shell: **emit twice into different directories and diff.** A determinism claim is a claim about outputs and is only ever settled by comparing outputs. | D19, D9 |
| AP19 | `entitysdk/resolver_config_test.go::TestValidateResolverConfig_CatchAllMustBeLocal` (now replaced) | **A pin that passes under both the rule it claims and the rule that replaced it.** The test asserted §4.1 step 2's catch-all MUST by trying exactly one forbidden kind — `peer-issued`. When arch re-keyed the rule from *remoteness* to *name transmission* (1.8), `peer-issued` moved from forbidden to **permitted**, and the test kept passing because it never distinguished the property from the example. A green pin was our only evidence the guard was right, and it was compatible with the guard being exactly backwards. **A test whose assertion cannot tell the old rule from the new one is evidence for neither** — enumerate the property's whole domain (all four disclosing kinds, all four admitted ones), not one member of it. | D10, D8 |
| AP20 | `entitysdk/resolver_config.go` — the deleted `BackendKindPinned` and `backendKindDIDKey` | **The locally invented constant.** §4.1a's default list named `pinned` and `did-key`; core-go's `core/types` declares neither. To ship the table we defined both ourselves, each with a doc comment explaining why the spec's string had no referent — and filed the mismatch as an inert cohort observation. It was not inert: §4.2 makes a conformant peer **skip** an entry with an unknown `backend_kind`, so both rows were dead config in every peer that installed our default. **A constant you have to invent locally to satisfy a spec table is a defect in one of the two documents, never a naming gap to fill.** The doc comment explaining the absence is the tell; we wrote it twice. | D18, D8 |

*Enforcement:* AP18 — `publish/publish_test.go::TestPublish_IsByteStableWithAPinnedInstant` emits
twice and diffs the bytes on disk, and asserts `Opts.At` is load-bearing so a refactor that drops it
fails here rather than in another implementation's test run. AP19 —
`TestValidateResolverConfig_CatchAllMustNotTransmitTheName` enumerates all eight declared kinds
across both columns. AP20 — `TestCatchAllClassification_CoversTheDeclaredVocabulary` requires the
classification to be total and disjoint over `core/types`' eight `BackendKind*` constants and to
contain nothing the enum does not carry; a string we would have to invent cannot pass it.

**AP21 / AP22 — the cross-impl consume run (2026-08-19).** Earned answering
`entity-browser-rust`'s ask to consume their published tree. **AP21 is what promoted D22.**

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP21 | `fetch/fetch.go` before `c13dfe2`, against `publish/publish.go` | **A corridor whose two halves are each tested against their own idea of the layout.** `publish` asserted the directory it emits; `fetch` asserted the URLs it builds; nothing asserted they were the same layout, and they were not — four ways at once (`/{peer}/tree/{path}.bin` vs no `tree/` segment, `sharded-2-flat` vs `sharded-2-4`, digest-only hex vs the wire hex §6.5.3.1 MUSTs, a raw 33-byte leaf vs the Amendment 6 `system/hash` pointer). Both suites green. Our consumer could not fetch one byte from our own publisher, and we found out because **another implementation asked us to consume them**. Compounding it structurally: `fetch` had no `make` test target and `publish` was not in `test-native`, so "run everything" never ran either end. **Two green halves of one corridor is not a working corridor.** | D22, D10, D19 |
| AP22 | `fetch/fetch.go::decodeVerified`, and browser-rust's own audit F6 | **A guard that refuses everything unfamiliar.** `entity.Validate()` enforces the 3-key wire invariant, so it rejected **every conformant `CONTENT_GET` body** — those are the bare 2-key hashable form, and the absent `content_hash` read as a zero hash it then failed against. The same shape in their tree refused every publisher that was not their own emitter (their F6, reversed the same day), and the same shape in ours twice before (the withdrawn `catchall_not_last`, the remoteness keying). **A guard written to stop one bad input, expressed as a refusal of everything it does not recognize, is a bug waiting for the first legitimate stranger.** The repair is never to loosen it: replace the familiarity check with the *property* check — here, recompute the hash over (type, data) and require equality with what the tree bound, which is strictly stronger than what `Validate` was standing in for. | D22, D8 |

*Enforcement:* AP21 — the two corridor gates named under D22, both in `make test-native`, both
mutation-checked (drop the peer-rooted bridge and the cross-impl gate 404s at hop 0; revert the
layout join and our own gate fails at the first page). AP22 — the same gates are what a
too-strict guard now fails against, because they run over *another implementation's* bytes: a
refusal of the unfamiliar cannot survive a fixture that is, by construction, unfamiliar.

**AP23 / AP24 — the name-arc reachability run (2026-08-19).** Earned closing
EXTENSION-REGISTRY §11.2's owed CLI surface and the last console→Avalonia parity gap.

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP23 | `entitysdk/registry_bootstrap_cost_test.go`, first version | **A measurement with no control arm.** The registry extension was opt-in, justified by a comment claiming the local-name handler re-mints its default-grant caps every bootstrap and grows the store linearly. That claim is what left `entity-shell` with no name resolution at all. Priced it properly per D20 — and the *first* probe reopened one SQLite database five times with a zero-value `PeerConfig`, which **generates a fresh keypair per call**, so it measured five different peers sharing a file, not five restarts. `Store.PathCount` is `LenPrefix("")`, a `COUNT(*)` over the whole index, so it read Δ370 paths per "reopen": a catastrophic leak, entirely manufactured. **The control is what caught it** — the arm *without* the extension was equally catastrophic, which is never the shape of a real per-component leak. With the keypair pinned, the registry's marginal per-restart cost is **zero on both counters** and its whole cost is +8 paths once. **Attributing growth to a component requires the arm without that component; a single-arm measurement can only ever confirm what you already believed.** | D9, D19, D20 |
| AP24 | `avalonia/tests/…/HandlerBrowserPanelTests.cs`, first version, vs `make smoke-xvfb-handlers` | **A test that asserts on the first item cannot see a bug that needs a second one.** The new handler-browser suite looped over discovered handlers "to find one with operations" and broke at the first match — index 0, which was already selected — so it **never changed the selection**. Five tests green. The X11 driver, which walks all 21, died silently after one iteration: clearing an `ObservableCollection` while its `ListBox` holds a selection makes Avalonia's `SelectionModel` re-read the stale index and throw from inside `SelectingItemsControl`, with no frame of this repo in the stack. The expensive gate found what the cheap one was structurally blind to; the repair is both — `Walking_Every_Handler_Survives_Selection_Churn` now runs the driver's exact loop and reproduces it in 11ms. **When a test iterates to find a subject, the loop is the test — an early break turns a sweep into a single-case assertion, silently.** | D10, D11, D15 |

*Enforcement:* AP23 — `TestRegistryExtension_AddsNoMarginalRestartCost` runs both arms in one
test and asserts the *differential*, never an absolute count, so it cannot be read without its
control and does not become a tripwire when core-go adds a handler. AP24 —
`Walking_Every_Handler_Survives_Selection_Churn` in the headless suite, plus
`make smoke-xvfb-handlers`, which logs the transition count it actually completed and says so
explicitly when that count is zero (the same self-check the minimize gate earned).

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
subsystems, same shape. **D20 was earned by AP13 plus the connectivity
scoping** — two estimates priced against our own tree instead of the
substrate. **D21 is AP12 promoted**: a packet addressed to us went
unopened, then a packet *cc'd* to us went unopened and took a landed
refusal down with it — same lesson, and the second shape is the one the
first fix's own wording excluded. **D22 was earned by AP17 plus AP21** —
a kernel equivalence claim tested on return values, and our own CDN
corridor tested one half at a time; different repos, different layers,
one shape: a contract asserted from each side separately is asserted by
nobody.

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
