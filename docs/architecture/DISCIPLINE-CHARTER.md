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

## 2. The 24 disciplines

D1-D11 are inherited verbatim from the entity-OS discipline charter
(originating in godot-entity-core-rust, ratified by egui-entity-core-rust).
They are stack-agnostic; they govern how we use the substrate.

D12-D24 are native to our stack (Avalonia + .NET + cgo + Go + the
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

### Native to our stack — earned by shipped bugs (D12-D17, D19-D24) and feedback episodes (D18)

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

**D23 — A model with no shipped surface is not shipped. Reachability is
part of the feature, and only a sweep can tell you it is missing.**
*Source:* three instances of one shape, found by audit rather than by
test. (1) The **name arc** — `ResolveName` / `BindLocalName` / the
resolver-config validator / the v1.13 adoption were all green in
`entitysdk`, and `shellboot` never set `Extensions.Registry`, so **no
shipped binary carried the handler a verb would dispatch to**.
(2) The **handler browser** — `wb.HandlerBrowserModel` was complete and
renderer-neutral; only tview drove it, so the primary renderer had no
path to it. (3) **`PeerLiveness`** (2026-08-20) — the model was tested,
the bridge exported it, and **no C# file referenced the export**, while
`PeerConnectionsPanel` went on rendering `ConnectionsOpen`: the
connection-pool snapshot that cannot express `suspect`, cannot say why a
peer went away, and disagrees with the tree whenever a connection is
evicted without a demotion. The GUI showed a strictly weaker and
occasionally wrong answer with the correct one one unused export away.
*Why:* every layer's own tests pass, because each layer is correct. The
defect lives in the **absence of an edge** between two correct layers,
and no test that starts inside one of them can see an edge that was
never drawn. It is D22's failure mode with the join not merely untested
but missing — and unlike a broken join it produces no symptom at all,
just a capability that quietly does not exist. Worse, in case (3) the
weaker surface it left in place *looked* like the feature working.
*How:*
- Landing a renderer-neutral model is **half** a feature. The other half
  is a user-reachable path, in the same session, or the model ships
  disabled with the gap named in `STATUS.md`.
- Run the two sweeps below at every audit, not at every diff — they are
  cheap and they are the only instrument that sees this.
- When a new surface **supersedes** an old one, wire it and relabel the
  old one in the same diff. Two answers to one question, one of them
  unlabelled, is how the weaker one stays authoritative.
- A test that mounts the panel and asserts the handle/envelope is what
  crosses "can a user reach this" — cheaper than the audit that found
  all three of these.
*Enforcement:* **`make reachability`** runs both sweeps and exits
non-zero on the first orphan. They are also written out here so the
instrument survives the target:

```bash
# 1. bridge exports no C# consumes
for e in $(grep -h '^//export ' avalonia/bridge/*.go | awk '{print $2}' | sort -u); do
  grep -rq "\b$e\b" avalonia/frontend/ avalonia/tests/ || echo "$e"; done
# 2. workbench models with no renderer or verb driving them.
#    Underscores are stripped from BOTH sides: the model file is
#    peer_liveness_model.go and the consumer is PeerLivenessPanel /
#    LivenessRender, so a naive snake_case grep reports every model as
#    missing — a sweep that cries wolf gets ignored, which is the same
#    failure one level up.
for f in workbench/*_model.go; do
  m=$(basename "$f" _model.go)
  cat avalonia/frontend/Panels/*.cs console/*.go shellcmd/*.go 2>/dev/null \
    | tr -d '_' | grep -qi "$(echo "$m" | tr -d '_')" || echo "$m"
done
```

Plus the panel-mount tier: `PeerConnectionsPanelTests` now asserts the
liveness handle is allocated and its envelope parses
(`Mount_Opens_Liveness_Handle_And_Renders_Counts`).

**D24 — A negative result is evidence only about the region the
instrument can reach. Name the region, or the result is worthless.**
*Source:* three instances, different shapes, the first two
self-reported as rigour. (1) The **four repro negatives** for the managed stack overflow
(2026-07-18 → 2026-08-20): headless collapse-to-zero, `WindowState`
cycling, the Xvfb window driver, and a 60-iteration verified-transition
sweep. All four were honestly run, honestly logged, and recorded as
narrowing the search. **They narrowed nothing** — every one of them
drove a *model method* and none could emit an X11 pointer event, and the
bug lived only in input dispatch. One real-input run
(`smoke-xvfb-click`) hit on the first seed. (2) **AP32**, the same error
inverted: four headless tests reported green while settling on a
property that was true before the work began, so the region they
measured was empty.
(3) **AP36 + AP37**, 2026-08-21 — the strongest instance, because
nobody was even claiming a negative. Interactive Life's controller was
inert from the day it shipped, through **two** independent defects, and
every suite on both sides of the seam was green: `programs`' four
life-edit tests write the input mask and then call `tickOnce()`
themselves, so the sampling window a real driver races cannot exist for
them; the Avalonia suite drove `StartForTests` and asserted on a status
label, so it never dispatched a pointer event. Each side's tests were
complete *about its own side*. **A two-language seam needs a test that
crosses it** — the one that did found both defects in a single run.
*Why:* an instrument that cannot reach the defect returns the same
answer as a fixed bug. Absence of evidence gets written down as evidence
of absence, and — worse — it *accumulates confidence*: four negatives
read as "we have looked hard", which is what let a month-old entry keep
steering work while the one instrument that could see the bug did not
exist. The failure is not the negative result; it is stating it without
its reach.
*How:*
- Every negative result records **what the instrument could not do**, in
  the same sentence as the result. "Headless survived 25 collapse
  cycles" is incomplete; "…and headless runs no X11 backend, so it
  cannot reach input dispatch" is the finding.
- Before adding a repro attempt, ask which boundary
  (`MODEL-AVALONIA-RUNTIME.md §6`) it touches. If it is the same one the
  last three touched, it is not a new rung.
- A gate that is green because it measured nothing is worse than a
  missing gate — prefer a completion signal the *producer* owns
  (a monotonic counter) over one the *consumer* derives (AP32).
*Enforcement:* `make -C avalonia smoke-xvfb-click` and `crash-hunt` are
the input-region instruments, named in `AGENTS.md` beside the suites;
`run-xvfb-smoke.sh` prints an explicit "this run is NOT evidence" line
when its own transition counter is zero (the pattern the window driver
already established). STATUS entries carrying a negative are reviewed
for a named region at audit.

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
    compiling? (D18) **And: can a user reach it?** Name the shipped
    surface — verb, panel, or menu entry. "The model is done" is half a
    feature; if the other half is deferred, it is a row in `STATUS.md`,
    not an assumption. (D23)

---

## 4. The anti-pattern catalog (AP1-AP38)

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
`reviews/archive/CORE-GO-LOCAL-DISPATCH-RESOURCE-2026-08-18.md` rather than in our suites, because a test
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

**AP25 — the resolver-config load-refusal (2026-08-19).** Earned by the D21 session-start
read finding two packets past our last-read letter, both addressed to core-go, both moving a
MUST we had shipped the day before.

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP25 | `shellboot/bootstrap.go` + `entitysdk.EnsureResolverConfig` before `2d312f1`, against EXTENSION-REGISTRY 1.17 §4.1 | **An enforcement point that cannot observe the rule's subject.** EXTENSION-REGISTRY §11.1 said a violating `resolver-config` MUST be *"refused or normalized **at load**"*, so we refused at load, and `shellboot` turned that into a fatal — `entity-shell` would not start. Arch **withdrew the clause** the next day: §4.1 step 2 binds *a distribution shipping* a config and *a peer storing* one, and §6a.9.2's store-first rule puts an operator's deliberate edit and a distribution's seed **in one entity at one path**, so a loading resolver cannot tell which act produced the bytes. It therefore necessarily over-enforces, and the over-enforcement deleted the operator `MAY` granted in the same paragraph. The rule was never wrong; its *placement* was, and placement is the half no test on either side could discriminate. **Before implementing a rule, ask whether the point you are implementing it at can observe the thing the rule is about** — the tell is a check whose subject ("did a *distribution* ship this?", "was this *latched* or re-read?") names an actor or an act that the data at that point does not carry. When it cannot, the check will be right about the predicate and wrong about who it binds, and the failure lands as a refusal of something legal. | D8, D18, D21 |

*Enforcement:* three pins, all mutation-checked against the pre-fix code.
`TestResolverConfig_SurfacesAViolatingConfigAtLoadAndRunsAnyway` asserts all three halves of the
replacement MUST (surface / never normalize / never refuse to start) and is itself a **reversal**
of the pin that asserted the withdrawn behaviour — its doc comment carries the withdrawal so the
next reader does not restore it. `TestEnsureResolverConfig_AnUnreadableConfigIsStillFatal` fences
the direction a reversal like this overshoots in: only the *policy* condition is a diagnostic, and
a config the peer cannot read stays fatal. `TestBootstrap_ANameDisclosingConfigDoesNotStopTheBoot`
crosses the seam (D22) — two `Bootstrap`s over one SQLite file under one keypair — because the SDK
call was correct in isolation and the fatal lived in the caller.

**AP26 — the discovery-substrate listener gate (2026-08-19).** Earned building the second
DISCOVERY backend, which is the first time the substrate had more than one.

| AP  | Source | Pattern (the name we use for it) | Discipline |
|-----|--------|----------------------------------|------------|
| AP26 | `entitysdk/app.go`'s `if cfg.ListenAddr != ""` around `discovery.NewHandler()`, before `021e5c2` | **The first consumer's precondition became the substrate's.** The `system/discovery` substrate was wired only for peers with a `ListenAddr`. That is not a property of discovery; it is a property of **mDNS**, which announces a port and has nothing to say without one. When mDNS was the only backend the two were indistinguishable, so the gate was written in terms of the mechanism. The second backend inverted it exactly: a `rendezvous` peer stands at a mailbox **because it has no reachable listener**, so the gate excluded precisely the peers the backend exists to serve. Nothing was broken before, which is the trap — the constraint is invisible until a second consumer arrives, and by then it reads as load-bearing. **The tell is a substrate gate expressed in terms of a mechanism (a port, a socket, a file) rather than in terms of what the substrate does.** Ask what the *abstraction* needs, not what today's only implementation needs; when they differ, the implementation carries its own precondition and the substrate carries none. | D4, D5, D18 |
| AP27 | `reviews/COMPUTE-HOLD-IMPACT-2026-08-20.md` §2, first version — corrected within the hour | **Our own stale record used as the substrate.** Asked what a parked upstream proposal cost us, we read the review packet that named the blocker (*"the generic panel cannot show program-specific status; the honest fix is a `text` HUD"*), priced the work from it, and routed the cost to arch. The HUD had shipped **ten days after that packet was written**, a month before we read it — `programs/authoring.go::buildStatusTextExpr`, whose own doc comment also answered the open question we were about to ask (*"no string or concat primitive, so the line is assembled by an indexed map over fixed positions"*). Both facts were one grep away. **D20 says price against the substrate rather than our own tree; this is the same error one level in — pricing against our own *documents* rather than our own *code*.** A dated packet is a snapshot of a moment, and the tree moves underneath it; the older the packet, the more confidently wrong it reads, because nothing about a well-written record signals that it has expired. The tell is a plan whose blocker is quoted from a document rather than demonstrated from a file. **Before reporting that something is blocked, grep for the thing you say does not exist** — the search that proves the absence is the same search that would have found it. | D19, D20, D8 |
| AP28 | `reviews/PROPOSAL-GENERIC-HOST-POINTER-INPUT-DEVICE-2026-07-27.md` — authored 07-27, found undelivered 08-20 | **A packet we never sent, recorded as sent.** The proposal is addressed *"To: arch + entity-browser-rust"*, `STATUS.md` carried it for 24 days as *"stays blocked on arch, as before"*, and an exhaustive search of both sibling trees — by filename and by five distinctive phrases — returns **zero** hits; arch's board has no row for it. **Writing a packet and routing a packet are two actions, and only the first leaves evidence in our own tree**, which is the only tree we look at when we update our own status. This is **AP12's mirror image**: that one was a routed finding nobody opened, this one is an unopened finding nobody routed, and ours is the worse failure mode because the ledger reads *waiting on them* — it converts our own inaction into an entry on someone else's account and then stops asking. Deferral by decision (§ compute) is a state a counterpart can confirm; deferral by silence is indistinguishable from a lost packet, and the driver cannot tell them apart from inside. **The tell is a "waiting on X" row whose only citation is a path inside our own repo.** A `To:` line is an intention; delivery is a fact, and D21's outbound direction needs the same evidence its inbound direction already demands. | D19, D21, D8 |
| AP29 | `core/tree.CollectAllBindings` / `CollectNodeClosure`, and `entity-core-go`'s WS-A handoff §3.3 telling us to use the first one | **A best-effort helper reused across a trust boundary it was not written for.** The handoff's build list said to walk the published trie with `tree.CollectAllBindings(cs, rootHash, "")` — a three-line delegation, and the same walk their own consumer uses. Both helpers document the behaviour that makes that wrong here: *"Missing nodes are skipped — the walk is best-effort."* Over a **local** store that is correct, because the store is ours and a missing node is our bug. Over an **HTTP origin** it converts a withholding origin into a smaller site, silently — which is precisely the B4 defect (`entity-core-go` `dabd076`, a `0xC0C1C2…` literal root) the walk was added to catch, and which every per-leaf consumer in this cohort had already reported green against. **A helper whose error path is `continue` encodes a trust assumption about its backing store, and that assumption does not travel with the function.** Caught at design time by reading the helper before calling it (D18), not by a failing test — there is no test that fails, which is the point. The repair is to re-implement the traversal over the same kernel **types** (the Layer-2 contract that must not vary) while failing closed on the first unresolvable node. | D18, D20, D6 |
| AP30 | `fetch/consume.go::PointerFor`, first version, against EXTENSION-TREE §3.3's three-shape table | **A join that is correct for every publisher you have, and wrong by construction.** Reconstructing a committed key's absolute path is `absolute_prefix + relative_key`, and the field on the wire is the **configured** prefix, which §3.3 resolves through a three-row table. Our first version concatenated the field verbatim. That is right for `entity-browser-rust`'s peer-qualified `/{peer}/` **and** right for our own peer-relative `docs/` — for two different reasons, neither of them the rule. Both live publishers passed. The tell is a reconstruction written from the emissions in front of you rather than from the rule's own table, and the failure mode is what makes it expensive: **a mis-joined path is a 404, and at a consumer a 404 is indistinguishable from a withholding origin** — so the bug reports as the other side's defect. | D18, D8, D19 |

*Enforcement:* the D21 session-start sweep gains an **outbound arm**, recorded in `AGENTS.md` beside
the inbound one — before carrying a *"waiting on X"* row forward, grep the counterpart's board and
specs for the packet's **subject**. The filename is the wrong instrument and we measured that the
same session: twelve `reviews/` packets appear by name in **no** sibling tree, and eleven had
plainly landed (AE-5 folded into `EXTENSION-COMPUTE` §11, `EXTENSION-DISCOVERY` naming this repo,
four `WORKSTREAMS` rows carrying our transport findings). Siblings cite our commits and claims, not
our paths. The pointer proposal is the one that failed on **subject** too — five distinctive
phrases, both trees, zero hits, no row on arch's board — which is what separates *undelivered* from
*delivered and quiet*. **A discipline whose gate fires on everything is theater in the other
direction**; the subject test fired once out of twelve.

| AP31 | `avalonia/bridge/browse.go::BrowseGo`, first version | **A C string read after the call that owns it returned.** The bridge's async exports return immediately and do the work on a goroutine — that is the whole P3′ shape. `BrowseGo` took the address as a `*C.char` and called `C.GoString` **inside** the goroutine. The buffer belongs to the .NET marshaller and is freed when the P/Invoke returns, so by then it is freed memory. **It does not crash.** It reads as the empty string, an empty address fails to parse, and the panel reports *"an address needs at least a name or a peer-id"* — a user error, in a panel the user typed an address into. Every layer is individually correct: the marshaller freed what it owned, cgo converted what it was given, the model refused what it was handed. **The rule: an async export must copy every C-owned argument into Go memory before the goroutine that uses it is launched** — the lifetime that matters belongs to the caller and ends at the return, and the synchronous exports beside it (`BrowsePin`, `VerifyConfigure`) are safe for a reason that does not transfer. Caught by a headless panel test asserting on the error text, not by a crash. | D14, D3 |
| AP32 | `avalonia/frontend/Panels/BrowserPanel.cs`, first test harness | **A completion signal that is a race in the direction that makes the test pass.** The headless tests settle an async operation by polling `Refresh()` until a control re-enables — `_goButton.IsEnabled = !view.Running`. But `Running` is set by the model when the *goroutine enters* the operation, and the bridge call returns before that: there is a window where the operation has been started, `Running` is still false, and the button is still enabled. Settle returned inside it, and every assertion downstream ran against an empty view — four tests green on nothing, one test failing for an unrelated reason, which is how it was found at all. **A derived UI property is not a completion signal.** The repair is a monotonic completed-op counter on the bridge handle, surfaced on the render envelope, which the harness waits to *increase*; it also lets the panel ignore a wake for an operation it has already drawn. | D10, D14 |

| AP33 | `shellcmd/cmd_tree.go::cmdPut`, and the two bugs in it | **A fallback that turns malformed input into a well-formed entity.** `put <path> <type> <json>` read `args[2]` alone — so any payload containing a space was truncated at the first one — and then, when the fragment failed to parse, silently stored it *as a literal string*: `// Not valid JSON — treat as literal string.` The put succeeded, printed a content hash, and wrote an entity nothing can decode. **The cost is paid a layer and an hour away**: the failure surfaces in a consumer as `cbor: cannot unmarshal UTF-8 text string into Go value of type SiteManifest`, which reads as the *consumer's* bug — the same displacement AP30 and the §6a.3a signature trap have. A tolerant fallback is right for input that was never trying to be structured and wrong for input that plainly was; the discriminator is one `HasPrefix("{")`. Found by seeding a site by hand to demo the browser, not by any test — every layer's tests passed, because each layer did exactly what it was told. | D8, D19 |
| AP34 | the 2026-08-21 crash hunt, and a month of reading the wrong signal | **Believing a signal that was re-raised.** CoreCLR's handler re-raises any fault it cannot classify, so what reaches the coredump carries `si_code 128` (SI_KERNEL), `si_addr 0`, and a register context belonging to the *handler*. Two desktop dumps were read as null dereferences on exactly that evidence; the real fault was `si_code 2` (SEGV_ACCERR) with `si_addr = rsp-8` and rip on a `call` — a guard-page hit, i.e. a stack overflow, and on the **alternate signal stack** rather than the managed one. That is also why the runtime never printed `Stack overflow.` and createdump never fired: by the time it faults there is no stack to report on. Three further forensic channels were dead ends that each *looked* like a finding — the DAC rejects a systemd ELF core (`0x80004002`), the shipped `dotnet-dump` cannot run on the host at all (framework-dependent beside a self-contained publish), and `make crash` only ever decoded Go symbols for a crash with zero Go frames. **Read `si_code` before `si_addr`; catch the FIRST signal live before trusting any dump.** | D13, D19 |
| AP35 | four "negative" repro attempts, 2026-07-18 → 2026-08-20 | **A harness that drives the model under the control cannot find a bug in the control.** Every driver here called the method the click would have called — `NavigateForTests`, `HandlerBrowserModel`, the window driver — which is a deliberate and good design for testing models, and by construction executes no input dispatch, no hit-testing, no focus transfer, and nothing that runs before a panel's own handler. A fatal crash lived in exactly that gap for a month while four separate repro attempts came back negative and were honestly recorded as narrowing the search. **They narrowed nothing**: the search space they covered never contained the bug. A negative result is only evidence about the region the instrument can reach — so name the region. The instrument that was missing is real input (`make -C avalonia smoke-xvfb-click`), and it hit on the first seed. | D10, D23 |

| AP36 | `programs/host.go::Input`, from the generic host's first day until 2026-08-21 | **A periodic sampler used as an event sink.** A driver wrote the held-key mask straight to the input port; the tick read that port and only that port, at its own rate. Both halves are individually correct and the composition drops input: at interactive Life's 6 Hz the window is **166.7 ms**, a mouse click is ~25 ms, and a press that is superseded by its own release before the next tick **was never observed by anything**. Measured on a clocked host: **1 of 6 d-pad clicks moved the cursor; 6 of 6 when the button was held past a tick** — and 25/166.7 ≈ 15% is exactly 1-in-6, so the arithmetic and the observation agree. Nothing was broken in the panel, the bridge, the keymap or the step. The tell is **a producer and a consumer on different clocks sharing one cell**; the repair is a queue with the contract stated — *every value offered is observed by exactly one tick, in order* — bounded, coalescing at the tail, and shape-agnostic so the host still never decodes a program's bytes. | D10, D24 |
| AP37 | `avalonia/frontend/Panels/ProgramPanel.cs::HookPressRelease`, shipped 2026-07-27, dead the whole time | **`btn.PointerPressed += …` on an Avalonia `Button` never runs.** `Button` marks `PointerPressed` and `PointerReleased` **handled** in its own class handler, and class handlers are added to the event route *ahead of* instance handlers on the same element — so a plain `+=` subscription is silently discarded. The on-screen controller therefore never set the bit on press and never cleared it on release: a click was byte-for-byte indistinguishable from no click. **It shipped in a session that verified the controller pixel-for-pixel** (`smoke-xvfb-program`), which is the point — the buttons *rendered* perfectly, and rendering was the only thing measured. `AddHandler(…, Tunnel \| Bubble, handledEventsToo: true)` is the form that works; the duplicate write it can produce is free because the same value twice is one value. Note this defect is invisible from Go and AP36 is invisible from C#: **two independent breaks in one seam, each hidden from one side's tests, both found by the first test that crossed it.** | D10, D18, D24 |

| AP38 | `programs/life_edit.go`'s regen soup, shipped 2026-07-27, reported by an operator 2026-08-22 | **"Different" asserted where "independent" was meant.** Interactive Life's Regen button hashed `(gen, i)` to a new board — and the hash was **linear in both** under a power-of-two modulus. Only bits 16..18 of an LCG step are read, and bit *k* of that step depends on bits 0..*k* of its input, so the three bits read depend on nothing but `(gen·C + i) mod 2^19`: changing the generation is *arithmetically indistinguishable* from changing the cell index by a constant. The board was a 256-cell window into one fixed pattern and Regen only slid the window. Measured: consecutive regens matched the previous board at **0.980 under a cyclic shift of 9 cells** (gap 1), **0.965 @ 18** (gap 2), **1.000 @ 2** (gap 60) — and the operator's report was, verbatim, *"it just moves it one or two over."* The board is 16 wide; 9 cells is half a row. **The test that should have caught it was green and had an explicit anti-vacuity clause** — `if lifeCellsEqual(before, after) { t.Fatal("regen did nothing") }` — which is exactly the too-weak property: a translation is never equal, so the assertion passes on every one of these boards. The second tell was free and unread: a translation preserves the live-cell count, so population across regens had **σ = 0.76** where an independent draw at 3/8 density gives **σ = 7.75** — every board it ever produced had 96 cells alive. **A generator needs one nonlinear round**; the repair squares the mixed value (so the counter's contribution depends on the index it is mixed with) and folds the square's high bits down (squaring mod 2^k leaves the low bits weak). Cross-check the rule against AP20 and D19's family: the code was *correct as written and correctly reviewed* — the defect is in the property the test named. | D10, D24 |

*Enforcement (AP38):* `programs/life_edit_test.go::TestLifeEdit_RegenIsNotATranslation`, with
`lifeBestCyclicMatch` as the instrument — best agreement over every cyclic shift, which is what
an elementwise comparison structurally cannot see. It gates in three places: two regens through
the **running host** (the way the button is actually pressed), an oracle sweep across generation
gaps 1…1000, and the population-σ check. Thresholds are calibrated, not guessed — over 2500 board
pairs the defective hash scored 0.953–0.992 and the fixed one 0.578–0.734, so the 0.85 cut has
margin on both sides; the gate was **run against the old hash and shown to fire on all seven
gaps** before it was committed. *(A regression test never shown red is decoration.)*

*Enforcement (AP36 + AP37):* `avalonia/tests/Workbench.Headless.Tests/ProgramPanelInputTests.cs`
is the crossing test — real `MouseDown`/`MouseUp` at the d-pad's hit-tested coordinates, through
Avalonia's own dispatch, asserting on **program state** (the cursor's cell index, read out of the
rendered display list) rather than on any UI property. Its diagnostic case reports each stage of
the route — hit test, tunnel, bubble, bubble-with-handled — *in the failure message*, so the next
break says which layer dropped the press. Go side: `programs/host_input_queue_test.go`, whose
`TestHostInput_SubTickClicksReachAClockedProgram` drives a **running clock** from outside, the
one thing `life_edit_test.go`'s `press` helper (write, then `tickOnce()` yourself) structurally
cannot do.

*Enforcement (AP34):* `make -C avalonia smoke-xvfb-click GDB=1` runs the app under gdb with
`handle SIGSEGV stop print nopass`, so the first fault is caught before the re-raise, and the
target prints `si_code`/`si_addr`/rip/the guard-page mapping. `make -C avalonia crash` now
prints the triage discriminators (GPU-module count, libbridge frame count, si_code) with the
SI_KERNEL caveat inline, and runs the managed half inside the builder image instead of
invoking a tool on the host that could never have worked.

*Enforcement (AP35):* `smoke-xvfb-click` and `crash-hunt` are the input-path gates, listed in
`AGENTS.md`'s build-and-test section beside the suites. Clicks are seeded and every coordinate
is logged to `clicks.log`, so a crashing run replays with `CLICK_SEED=n` rather than being
retold as a story about randomness.

*Enforcement (AP33):* `cmdPut` joins `args[2:]`, and refuses a payload that opens with `{` or
`[` and does not parse — with the fix in the message, because the shell's `SplitArgs` strips
quotes as *shell* quoting and the honest instruction is "single-quote the whole payload".
`TestPutTakesTheWholePayloadNotTheFirstToken` and
`TestPutRefusesBrokenJSONRatherThanStoringAString` are the gates.

*Enforcement (AP31):* `BrowseGo` copies with `addr := C.GoString(cAddr)` above the
`browseNavigate` call and the reason is in the body, not a commit message. The behavioural
gate is `BrowserPanelTests.Open_By_Name_Renders_A_Page_With_The_Whole_Chain`, which asserts
the error line is **empty** — a panel that silently navigated nowhere fails it.

*Enforcement (AP32):* `BrowseRender`'s envelope carries `ops`, and `BrowserPanel.Settle`
takes the pre-call value and waits for it to pass. The counter is on the **bridge**, not the
panel, because the panel is the layer that cannot see when the goroutine started.

*Enforcement (AP29):* the walk lives in `fetch/consume.go::Consumer.Walk` with the reason in its
doc comment, and `publish`'s `TestConsumeWithholdingInteriorNode` withholds **one interior CHAMP
node** from an otherwise perfect emission and requires `ErrIncompleteWalk`. A best-effort walk
passes that test with fewer keys and no error, which is exactly how it would ship.

*Enforcement (AP30):* `fetch.AbsolutePrefix` implements §3.3's table as a table, and
`TestAbsolutePrefixResolvesAllThreeShapes` pins all three rows — including the universal case,
whose trim is a **no-op** and which must therefore *not* be peer-joined. The third shape is also
measured live: `entity-core-go`'s federation origin publishes `prefix: "system/"`, and
`make crossimpl-go` reconciles every committed key through it.

*Enforcement (AP26):* `TestRendezvous_SubstrateNeedsNoListener` stands up a peer with **no** `ListenAddr`,
asserts the substrate is present and a backend registers on it — and carries a **control arm**
(AP23) asserting that a peer asking for neither a listener nor discovery still has no substrate, so
the fix cannot be satisfied by making it unconditional. `Extensions.Discovery` is the explicit
door; its doc comment names the reason so the gate is not re-added as a tidy-up.

**Neither AP25 nor AP26 is promoted, and the count is the reason.** Each has bitten **once**. The
neighbouring instances are different failures: AP19 is a *test* that could not tell two rules
apart, AP22 is a guard keyed on familiarity rather than a property. If a second enforcement-point
error lands — a check placed where its subject is unobservable — it earns a discipline then.

**Evidence for D21, worth recording where D21 lives.** Both packets were addressed to
`entity-core-go`; neither named us in its `To:` line; §7 of the first says explicitly *"not yours
to chase."* The rule that says *a packet changing a table, a default, or a MUST is read the same
session regardless of who it is addressed to* is the only reason this was found before it reached
a user, and it was found on the session-start step rather than by feature work tripping over it.

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
nobody. **D23 was earned three times before it was written** — the name
arc (handler never registered in `shellboot`), the handler browser
(model complete, only tview drove it), and `PeerLiveness` (exported,
never referenced). Two of the three were found by audit rather than by
test, and the promotion criteria above call for ratification at two; the
third is the one that says the sweep belongs in the audit procedure
rather than in anybody's memory.

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
| D23        | **`make reachability`** — both sweeps, exits non-zero on the first orphan. Green at 2026-08-20 after the liveness wiring. Plus panel-mount tests asserting the handle + envelope for each bridge surface. | Join it to `check` once it has survived a few sessions without a false positive; today it is a target you run, not a gate that blocks. |

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
