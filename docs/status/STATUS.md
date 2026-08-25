# entity-workbench-go — status

_Updated: 2026-08-18 · public: v0.8.0 (master) · working branch: `dev` (ahead of `master`)_

## Where it is

The Go **reference application** built on the Entity Core Protocol — an opinionated,
worked example of an entity-native app, **not** a conformance implementation and not a
mandate. It is a leaf in the stack (depends on the `entity-core-go` kernel — `core` + `ext`
— via local `replace`; nothing depends on it). It ships the in-tree `entitysdk/` (the de
facto reference Go SDK: typed wrappers, storage, identity bundles, revision/continuation/
discovery helpers), **`entity-shell`** (the primary CLI and leading edge of feature work),
an **Avalonia/.NET desktop GUI** driven through a Go c-shared bridge (podman-only build),
a frozen **`console`** (tview) TUI kept as a renderer-neutrality enforcer, the
**`programs/`** entity-native programs track (generic compute host + descriptors), and a
small CDN corridor (`entity-publish` / `entity-vcs` / `entity-fetch`, plus
`entity-seed-site` / `entity-serve-cors`). Maturity: **v0.8.0 research preview**.

`master` carries v0.8.0. **`dev` is well ahead of it** and is where the compute work lives;
it is deliberately unmerged (see the guardrail below).

## Where we left off

Three threads: the share arc (open, moving), the compute floor (held), and a closed
stabilization pass.

### 1. The compute floor (the primary arc — Doom-class realtime) — WORKBENCH SIDE IS DRY

Six sessions (2026-07-15 → 07-19) characterized parallel compute on the entity model
end-to-end. **Everything on the workbench side is built, measured, and green; every
remaining lever is gated on arch or core-go.**

The one doc to read is
**`docs/architecture/reviews/COMPUTE-SHARDING-INTO-HOST-2026-07-18.md`** — §10 is the ledger
of all five routed levers and who owns each; §11 is the latest result. Not duplicated here.

Headlines, so this doc stands alone:
- **Host-managed static-k sharding** is a real mounted-program mode, not a test rig. The
  gate proves *parallel == serial == unsharded* state hashes, generation for generation.
- **64×64 reach** through the generic host against an independent Go Life oracle
  (~850 ms/tick on Stage-1 — correct, not realtime).
- **The ~1.8× parallel ceiling was diagnosed, not accepted.** A compute-bound control shows
  speedup climbing with op-cost (1.70 → 2.60×), so the flat 1.8× was *store reads*, not a
  limit on entity-compute parallelism.
- **Axis-1-engine host**: ~20–28× wall-time; the bottleneck then *moved off* the evaluator
  onto store I/O.
- **The time-axis negative**: the floor parallelizes **maps, not folds**; the serial floor
  is k+1 barriers, O(1) in N.
- **The whole-state O(N)-per-tick floor is a representation choice, and it collapses.** Arch
  reframed it; the probe (`programs/subtree_test.go`) confirmed per-tile subtree state +
  content dedup gives an **O(changed)** tick — sparse collapses **21.6×**, dense stays O(N).
  Doom-class sims are sparse, so they never hit this floor. The former Doom blocker is now a
  co-design (a subtree-state descriptor/host convention), not a wall.

**Waiting on arch** for the subtree-state descriptor shape, the collection primitive
(`concat`), and `PROPOSAL-CONTINUATION-STANDING-MODEL` §4. Nothing unblocked remains here.

### 1a. AE-5 Axis-1 conformance admission — **GREEN (LOCKED)**, 2026-07-23

Core-go's AE-5 packet (`entity-core-go/docs/status/ROUTING-2026-07-23-ae5-axis1-inproc-admission.md`)
asked workbench to run the frozen 330-vector inproc compute corpus in-process through **Axis-1** (the
only alternate engine, and it lives here) and hand back the alternate-engine emission; a green run
folds EXTENSION-COMPUTE §11. **Done and green.** Full write-up:
`docs/architecture/reviews/COMPUTE-AE5-AXIS1-ADMISSION-RESULT-2026-07-23.md`.

- Harness `entitysdk/axis1_admission_test.go` mirrors core-go's reference emit loop, swapping in
  Axis-1; corpus + reference emission reproduce byte-exact (`9131a93d` / `419ca55d`); the emission is
  accepted by core-go's `verify`/`cross-bless` with no adapter.
- The run found **two real Axis-1 bugs, both fixed** (Axis-1 is workbench-owned): the uint-index
  `type_mismatch`/`index_out_of_range` divergence (F-2 ruling never transcribed — `axis1/arith.go`),
  and the missing native `compute/apply` closure application that forced a tail-recursion deopt
  (`axis1/{node,decode,eval}.go`). After both: `verify --require-alternate` 7/7 guards pass,
  `cross-bless` **330 agree, LOCKED**.
- **Owed:** core-go re-runs verify + cross-bless on its side to bless, then arch folds §11.
- **Rider for arch:** AE-5/§11 is Axis-1's graduation to a conformance-admitted engine — the
  Axis-1-home question (workbench research engine vs a second `ext/compute` engine) is surfaced in the
  review doc §5.

### 1b. Avalonia program-chrome catch-up (2026-07-27) — CLOSED

Closed the C# side of `docs/status/HANDOFF-2026-07-27-avalonia-program-chrome-catchup.md`
(the workbench half — interactive Life + the pointer-gap proposal — shipped the same day;
that handoff is left as the historical ask). All four items landed in `ProgramPanel.cs`,
verified pixel-for-pixel via `make smoke-xvfb-program PROGRAM={life,snake,asteroids,life-edit}`
(real X11 + Skia, not just headless) and the 51-test headless suite (`make test`), both green:

- **Item C (was a live bug):** `BitForKey` only parsed the legacy string keymap form, so
  keyboard input was dead for Asteroids and interactive Life since their 2026-07-24 roled
  re-declaration. Fixed to accept both forms (mirrors `programs/controls.go::ParseKeymap`).
  Added the on-screen standard controller (d-pad + labelled action buttons, built once per
  key-set input from `scene.keymap`) — buttons pair glyph+label text since the podman
  runtime's font set lacks colour-emoji coverage (bare glyphs render as tofu).
- **Item B (was a live bug):** views were keyed by shape, so the `status` port drew on top
  of the `display` port for every program. Rekeyed by port name; `status` now renders as a
  caption docked above the board. Caught a second latent bug fixing this: the caption's
  custom-drawn `Control` has no `MeasureOverride`, so a bare `DockPanel.Dock.Top` child
  collapsed to zero height — wrapped it in a fixed-height `Panel` (a `Panel` arranges
  children to its own bounds regardless of their `DesiredSize`, same reason `_stage` already
  worked for the board views).
- **Item A:** `DisplayListShapeView` now reads `scene.render` and fills closed quads
  (skipping the reserved `DisplayKindBackground` kind) when `render == "fill"` (Life/Snake);
  Asteroids' default `stroke` wireframe is unchanged.
- **§2:** `life-edit` wired into `avalonia/bridge/program.go`'s dispatch and registered as
  `program-life-edit` in `Program.cs` (+ `SmokeDriver`'s program-cycle map).

Nothing here touches the still-open pointer/click gap
(`docs/architecture/reviews/PROPOSAL-GENERIC-HOST-POINTER-INPUT-DEVICE-2026-07-27.md`) —
that stays blocked on arch, as before.

### 1c. Full-repo health sweep (2026-08-13) — CLOSED

A ~2.5-week gap (07-27 → 08-13) had `dev` untouched but the sibling `entity-core-go` moved
dozens of commits, which had silently broken `make test` here — the earlier catch-up sessions
only ran `make test-programs`, not the full sweep. Ran `make test` / `make lint` /
`avalonia/bridge` smoke-compile / `avalonia make test` end to end for the first time since;
three real defects found and fixed, all on our side of the fence (core-go itself untouched):

- **Build break:** core-go's coordinated rename `types.InboxNotificationData` →
  `types.SubscriptionNotificationData` (+ wire type `system/protocol/inbox/notification` →
  `system/subscription/notification`, `d7e44f6`, a single-round MUST with no dual-kind
  window) had never been adopted here. Updated the 5 call sites
  (`entitysdk/subscription.go`, `entitysdk/subscription_handler_direct_test.go`,
  `workbench/notification_ingest.go`, `workbench/blob_resolve.go`,
  `workbench/test_helpers_test.go`) plus stale example code in
  `docs/architecture/APPLICATION-HANDLER-INTEGRATION.md`.
- **Two capability-check regressions** (`shellcmd` `TestStage3_CapDelegation_Positive`,
  `TestStage4_CaseH_RestrictedCapsMesh3`): root-caused to core-go's `43573d3` (Aug 5) —
  connect-time capability assembly now applies real advertisement-discipline filtering
  (`ConnectHandler.AssembleInboundGrants` → `filterAdvertisedGrants`), and under the §PR-8
  canonicalization a self-issued grant can only ever advertise coverage over **its own**
  peer namespace (`"*"` → `/{peerID}/*`). Both tests' scoped grants listed `Resources:
  ["*", "/*/*"]` — the `"/*/*"` (cross-peer) entry can never be covered by a same-peer
  advertisement, and since coverage requires *every* Include member to match, the **whole
  grant entry** silently dropped, not just that member. Fixed by dropping the redundant
  `"/*/*"` (the operations under test all target the granting peer's own namespace, so bare
  `"*"` is sufficient) — confirmed empirically (reverted, reproduced the 403, re-applied).
  Documented in both tests' doc comments so the next drift doesn't re-diagnose this from
  scratch.
- **Makefile hygiene:** `test-inspect` was missing from `.PHONY` (its sibling
  `test-programs` wasn't) — file-existence-based tracking on that target name was tripping
  a stat/permission error under the containerized build, failing `make test` at the last
  step. Added it.

Full sweep green after: `make test` (all 9 native modules + `inspect`), `make lint` (vet
clean across every module), `avalonia/bridge` smoke-compile, `avalonia make test` (51/51
headless). No code changes to `entity-core-go` — read-only archaeology (`git log -S`, diff
review) to root-cause, per the sibling-repo boundary.

### 2. Stabilization pass (2026-07-22) — CLOSED

Structural cleanup so the next arch ask lands on solid ground:
- **`programs/` extracted into its own module.** The 21 `workbench/program_*.go` files (the
  generic host, descriptors, authoring, Life/Snake/Asteroids/heavyfield/chain/shapes) moved
  out of the app's renderer-neutral model layer into a sibling module, and lost the now-
  redundant `program_` filename prefix. Verified zero coupling before the move: of the 316
  top-level symbols in non-`program_` workbench files, the only ones the cluster referenced
  were `entitysdk`-qualified. Dep direction is `programs → entitysdk`; `avalonia/bridge`
  repointed. New `make test-programs`; wired into `test-native` and `LINT_MODULES`.
- **perfreview rot guard landed** (the backlog item). `make lint-perfreview` runs
  `go vet -tags=perfreview`, and `lint-native` depends on it. Compile-only on purpose —
  the benches take ~20m and must never run under `-race`. It is currently **clean**, so the
  module is *not* rotted; the guard is what keeps it that way.
- **The minimize crash got a headless repro suite** —
  `avalonia/tests/Workbench.Headless.Tests/PanelStackZeroCollapseTests.cs` (see below).

### 3. The share / multi-peer arc (the live thread) — steps 3a and 3 DONE

The one doc to read is
**`docs/architecture/reviews/REVIEW-SHARE-AND-CONNECTIVITY-ALIGNMENT-2026-08-17.md`** —
§5.1 is the ordered plan, §7 is the N1 ruling, §8 is what landed 2026-08-18 and the
correction it forced. Companion packet: `reviews/CORE-GO-ASKS-2026-08-17.md`.

Where the six steps stand:

| step | what | status |
|---|---|---|
| 1 | Commit + send the review | done (`ee96c5f`, `0e86a9f`, `ca2e0cb`) |
| 2 | Foreign-namespace subscription gate on the Go arm | done (`b3848c1`) — and it found the watch-hub `send on closed channel` |
| **3a** | **Consumer-side `published-root` reader** | **done** — `entitysdk/published_root.go` |
| **3** | **Target prefix on the sync surface (source ≠ target)** | **done** — `MirrorSinceLastSeen` + `InstallRevisionMirrorChain` + `revision mirror` |
| 4 | Follow vocabulary settled with browser-rust | **open — needs browser-rust.** Arch confirms the two pieces (one follow verb with a `strategy` field; a per-follow minted capability) need no arch ruling |
| 5 | The first `app/share/*` record | blocked on 4, and on `APP-CONVENTION-SHARE` — **unauthored, #4 on arch's release slice**, and they say plainly it is not fast. They will re-order if our step 5 is otherwise ready; it is not (4 comes first) |
| 6 | `APP-CONVENTION-CHAT` review as a consumer | open, competes with nothing |

**What 3a/3 mean in practice.** A peer can now read another peer's signed
`system/peer/published-root` (full verification: content-hash recompute, signature against
the key derived from the Base58 peer-id, `prefix` §3.3a discipline, monotonic seq floor),
and mirror that peer's subtree into `/{them}/{their path}` — the V7 §1.4 cached-remote
shape browser-rust's F1 is about — instead of into our own namespace. Both the one-shot
pull and the standing follow chain. The destination is **derived from the publisher's
signed prefix**, never caller-chosen, per arch's amendment.

**The correction worth carrying.** W3 told core-go the mirror needed "no wire change — the
blocker is entirely ours, in an SDK signature." Half right. `tree:merge` pre-checks put
authorization on every target path, and a self-issued `Resources: ["*"]` is peer-local under
§PR-8, so every `/{them}/…` merge 403s. The fix is a capability that names the publisher's
namespace (`MintMirrorCapability`) plus a per-call caller-cap seam on the executor — not a
signature change. **No ask on core-go**; their behavior is correct. Full account in §8.2;
ratified as **D19** with **AP10/AP11** in the charter.

**Axis B (connectivity validation)** is unstarted, **confirmed unblocked by arch**, and the three
constraints are confirmed correct as stated (§5.2 sizes it). One caveat added 08-18: **no TURN
credential mechanism is specified anywhere in the corpus** (arch queue Q18). Build against static
config and do not invent a credential shape — hitting that wall and routing it is the forcing
function.

**Scoped with browser-rust 2026-08-18** —
`docs/architecture/reviews/CONNECTIVITY-CONVERGENCE-2026-08-18.md`. They named the four app-tier
pieces our arm is missing and we verified all four against our own tree: no `ext/signaling`
consumer, no `system/peer/status` read-model (`ConnectedPeers()` is a pool snapshot), no
`maintain-peer`, no connector/`meet`. **The transport is not the gap** — `AppPeer`
listens and dials WebSocket already (`ListenWebSocketReady`, `Connect("ws://")`), and `shellboot`
routes a `ws://` `ListenAddr` at peer creation. What is missing is **self-publication**: nothing
writes `system/peer/transport/{our-peer-id}/*`, so a browser cannot learn we accept a socket.
That inverts the order — profile publish is the precondition, not a peer, of the other three.
**No WebRTC on the Go arm** (§6.5.2d is latent without a native terminator; browser↔Go is `wss`,
browser↔browser is WebRTC).

### 4. Publisher conformance — CLOSED 2026-08-18, the corridor emits a real signed root

`publish/publish.go` advertised `signed_pointer: "system/peer/published-root"` +
`freshness: "static-immutable+signed-pointer"` and did no signing at all; the artifact at
`{manifest_url_prefix}` was the http-poll *transport profile*, which `EXTENSION-NETWORK` §6.5.3.1
rules out in as many words. Step 3a made it self-refuting — our own `ReadPublishedRoot` rejected
our own publisher's output on gate one. Arch routed it 08-17 and again 08-18.

**Exit B taken (emit a real signed root), not Exit A.** The handoff priced B as its own arc on the
strength of *"our closure walk is shallow and D3 requires the trie closure"* — true about
`publish/`'s walker over bound entities, and irrelevant to the obligation, because
`tree.CollectNodeClosure` in core-go already implements D3 exactly and cites §6.5.6 Amendment 10
as the reason it exists. **An estimate that prices a spec obligation off our own code's shape,
without checking whether the substrate already implements it, is an estimate of the wrong thing.**

What ships (`publish/signed_root.go`, new):

| object | now |
|---|---|
| `{out}/manifest` | the signed `system/peer/published-root` (3-key wire entity, `content_hash` + `prefix`) |
| `{out}/transport-profile` | the http-poll profile, out-of-band per §6.5.4 / proposal D5 |
| `{out}/content/…` | + the transitive trie closure of `root_hash`, the published-root, and its signature — the §6.5.3 publish-side MUST |
| `{out}/{peer}/system/signature/{hex}.bin` | the §5.2 invariant pointer, as an ordinary two-hop TREE_GET leaf |
| `{out}/{peer}/system/peer/published-root/{peer}.bin` | `signed_pointer` names a path, so the path resolves too |

Acceptance test is written **as a consumer** — no peer, only the emitted files: recompute the
manifest hash from its own bytes, resolve the signature two-hop, verify against the key derived
from the Base58 peer-id, walk the CHAMP trie from `root_hash` over the emitted shard asserting no
404 (`TestPublish_SignedRootVerifiesFromTheEmittedFiles`).

**Three things fell out of building it**, all in
`docs/architecture/reviews/PUBLISHER-CONFORMANCE-RESULT-2026-08-18.md`:

1. **A signed root and a filtered publish are incompatible — we refuse at the emitter.** The
   closure obligation would upload the `IncludeType`/`IncludePath`-excluded entities' bytes under
   `content_url_prefix` anyway (a leak, and *because* we advertised a signed pointer); withholding
   them instead is the silent short walk browser-rust measured (`9a9c0f5`). §6.5.3 states the
   broken-walk direction; the leak direction is unstated. Routed as a candidate.
2. **core-go ask: `published-root` `seq`/`predecessor` are process-memory only.** Measured — three
   publishes of three *different* roots through `ext/publishedroot.Publisher` emitted
   `seq=1 / predecessor=nil` every time. §6.5.6 makes both a MUST. We source them from the store
   ourselves and would rather not own a second minting site.
3. **arch ask: the content-only mirror §6.5.6 sanctions has no expressible `freshness`.** The enum
   is `live | async | static-immutable+signed-pointer`, and the MUST list requires `signed_pointer`
   for both non-live values. Found while pricing Exit A; we emit nothing into the gap.

**Unblocked by this:** the Go-published / Rust-consumed cross-check with `entity-browser-rust`
(they emit and walk signed roots already, rust↔rust) — arch calls it the most valuable interop
result on this track, and the only one that is not cohort-consistent.

**Process (AP12, ours).** Arch routed the finding on 08-17 addressed to us by name; we did not open
a row and shipped three commits past it. `AGENTS.md` now makes the sibling-arch read a session-start
step. Catalogued, not ratified — first time in this shape.

## Open bugs

- **Managed stack overflow on window minimize** (Avalonia, software-render path). A tight
  alternating A↔B JIT recursion into the .NET guard page, **zero GPU/GL modules in the
  faulting thread** — so it is ours, not the driver. Full forensics:
  `docs/status/HANDOFF-2026-07-18-avalonia-64x64-segfault.md`.
  **New this session:** `PanelStack`'s class doc records **two earlier SIGSEGVs with the
  identical signature** (PIDs 4033208, 4035311) from a GridSplitter drag driving a
  star-weighted row to **zero size** — an upstream Avalonia layout-engine recursion, fixed
  structurally with `RowDefinition.MinHeight`/`MaxHeight` pins. Minimize is the *same
  zero-size condition arriving from above* (the window collapses the ScrollViewer viewport),
  which those row pins do not cover. That is the working hypothesis.
  **The headless repro did NOT fire** (all three tests pass): 25× collapse-to-0×0-and-back,
  25× `WindowState.Minimized`/restore, and 40× collapsing the 64×64 Life panel with a live
  repaint stream in flight are all survivable under headless Skia. That is a *negative*
  result kept as a regression fence — it narrows the search without closing it, since
  headless does not run the X11 backend at all, which is where both documented predecessors
  lived. **Both original leads are now closed:** the other one — symbolizing the core dump —
  died with rotation; the `entity-avalonia` dumps are gone from
  `/var/lib/systemd/coredump`. Pinning this needs a **fresh crash capture**: grab the dump
  before it rotates and symbolize it in the .NET container (`dotnet-dump analyze` →
  `clrstack`) to name the recursive pair.
- **GPU-driver SIGSEGV** (distinct, older): the mesa hardware-GL path crashes under
  sustained compositor load. Worked around with `WB_SOFTWARE_RENDER=1`. Whether software
  render should be the default or auto-detected is still an open product call.
- **Asteroids "keys" report** — flagged by the operator, never reproduced. Key wiring was
  checked and is correct; suspected to be the GPU crash hit while interacting. A headless
  key-injection repro is still owed.

## Backlog

**Release / dependency cutover**
- **Module-path cutover.** Every `go.mod` requires the kernel by its vanity path
  (`go.entitychurch.org/entity-core-go/{core,ext}` @ v0.8.0) but resolves it through a
  local `replace` to the sibling `../../entity-core-go/{core,ext}` (offline; no network, no
  tag). When the vanity path is published + tagged, the cutover is **one line per module**:
  drop the `replace`, let `require … @v0.8.0` fetch. Then run the deferred no-siblings,
  clone-fresh `make build` to prove it.
- **Path-syntax migration.** User-facing surfaces still use `alias:path`; the pinned
  substitution sigil is `@alias` (`:` is reserved for `<handler-path>:<op>`). Prefer
  `@alias` in new docs/examples now to minimize churn when the code change lands.
- **`entitysdk` spin-out.** Stewarded in-tree as the authoritative Go SDK.
  - **Known blocker, found 2026-07-22:** the compute probe cluster in `entitysdk`
    (~9k lines: `exp_compute_*`, `axis1_*`, `continuation_shard_test.go`) **cannot be
    lifted out** as-is. It is one densely interconnected web — a shared Life workload
    fixture, the `boundary` CBOR helper, the shard rigs — with no clean cut, and 5 of the 12
    files reach `export_test.go`'s deliberately test-only engine accessors, which are
    reachable only from *inside* `entitysdk`'s own directory. Moving them would force those
    accessors into the shipped API, which that file explicitly forbids (it exists so app
    code cannot bypass protocol-first access). Untangling it means extracting the Life
    workload fixture first; until then the probes stay put. Attempted and reverted this
    session — the `programs/` extraction is the part that *was* clean.
  - Relatedly: `axis1_{cost,equivalence,tick}_test.go` are arguably API pins for the shipped
    `entitysdk/axis1` subpackage rather than probes, and should travel with the SDK.

**Known waivers / kernel-blocked (not workbench bugs — sibling-impl rule)**
- **Identity-rebootstrap storage leak.** The bounded-rebootstrap pin in `entitysdk/` is
  `t.Skip`'d with its assertion intact. Re-applying an identity bundle leaks ~+1 path /
  +4 entities **per reload** (linear, unbounded). Root cause is the kernel's identity
  *ceremony* re-issuing the local-peer→controller cap + sibling signature with
  ceremony-time-varying material instead of reproducing prior content hashes. Delete the
  Skip to re-arm the regression the moment the kernel's re-apply is idempotent.
- **Subscription slow-consumer head-of-line block.** The producer queue is bounded with
  drop-on-full; the gap is a missing **per-delivery deadline** on the consumer-side
  synchronous `Deliver`, which pins one shard worker when a consumer stalls. Kernel-side fix.
- **Subscription delivery saturation.** 100% delivery below ~2K notifs/sec; a cliff to
  ~47–49% at 5K+/sec, dropped silently. Typical workbench heartbeat is far below saturation.
  Endorsed fix is **parallel delivery workers** — kernel-side after cross-impl alignment.
- **Revision auto-version is O(N)/Put.** Per-Put latency under auto-version grows linearly
  in the existing path count under the prefix, *regardless of trie shape*, because the trie
  is rebuilt from scratch on every Put. Fix is incremental update — a kernel/spec concern.
- **Content-store GC contract.** Path-overwrite accumulates orphaned content entities
  indefinitely; a naive "delete unreferenced + VACUUM" sweep is unsafe because hash
  references are encoded across many typed sites. Awaiting a cross-team reachability/GC
  contract built on the kernel's reverse-hash index; workbench ships nothing until it lands.

**Discovery follow-ups (post-"Nearby Peers" close)**
- N-panel × M-peer scan multiplier — one scan loop per discovery handle, no dedup by handle.
- TXT-pair parsing in the Avalonia bridge duplicates the kernel's private parser (drift risk).
- Scan interval is hardcoded ~5s; no "Scan now" affordance.
- No headless test exercises a *populated* nearby list (would need mDNS in the test container).

**UI / renderer**
- **Handler-browser panel** — the one surface where `console` is still ahead of Avalonia;
  closing it ends the console→Avalonia parity gap.
- **Console multi-peer UX** (deferred): peer-picker modal, status bar, `peer create`/`destroy`.
- **Manifest-driven panel registration** (deferred from the multi-peer plan).
- Avalonia drives feature work and may outpace the frozen `console` renderer; console-parity
  is explicitly *not* an obligation.

**SDK ergonomics / compute**
- **SDK ergonomic helpers (compute "S4–S8").** Owed a research-first session.
- **Compute DSL parser.** Deferred; built *on top of* the S4–S8 helpers, only once an
  authoring workflow actually needs one.
- **Wire a real consumer of the `resolve()` seam** (`entitysdk/resolve_chain.go`).

**Hardening / cleanup**
- Revision-recovery diagnostic: hub-spoke fetch-diff recovery with auto-version *off* logs
  an independent transport failure mode; captured as an observation, the test passes.
- Selection-state reader hardening: replace the silent legacy-tolerance path in
  `entitysdk/workspace_state.go` with log-on-violation or reject-on-decode.

## Guardrail — do not merge `dev` to `master` yet

The legacy hard-coded panels (`NewLifeGameModel` / `NewSnakeGameModel` /
`NewAsteroidsGameModel`, now in `programs/`) are still the oracle for
`TestMount_LifeMatchesHardCodedModel`, and Avalonia registers both the legacy and the
generic-host copy of all three panels. Retiring them means re-pinning that oracle first.
`dev` is comparison surface, not a release.

## Waiting on

- **arch:** the subtree-state descriptor/host convention (the successor rung, and the same
  rung as Doom-realtime); the `concat` collection primitive ruling;
  `PROPOSAL-CONTINUATION-STANDING-MODEL` §4 (the continuation join-failure policy); whether a
  scan/up-sweep orchestration is in scope.
- **`entity-core-go` kernel:** published + tagged vanity module path; an idempotent
  identity-ceremony re-apply; a per-delivery deadline + parallel delivery workers;
  incremental revision-trie update.
- **Cross-team:** the content-store GC / reachability contract.
- **`entity-browser-rust`:** the follow vocabulary — one record + verb, `strategy` with a
  value that does not assume ordered delivery (share-review §3.2). Step 4 of the share arc,
  and step 5 waits on it by construction.
- **arch:** `APP-CONVENTION-SHARE` authored (their own named top item) — the last thing
  between us and writing the first `app/share/*` record.
