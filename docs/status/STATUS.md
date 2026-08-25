# entity-workbench-go — status

_Updated: 2026-08-19 · public: v0.8.0 (master) · working branch: `dev` (ahead of `master`)_

**Green as of `c13dfe2`:** ten suites, run **individually to completion** (AP15 — a count from
`make test` stops at the first failing package). `entitysdk` 195.5s · `inspect` 1.8s · `shell` 3.1s ·
`shellboot` 10.2s · `shellcmd` 285.1s · `shellpanel` 1.1s · `workbench` 2.1s · `programs` 134.9s ·
`publish` 1.5s · `fetch` 1.0s. Zero failures. `make lint` clean. The command was
`for t in sdk inspect shell shellboot shellcmd shellpanel workbench programs publish fetch; do make test-$t; done`.
**`fetch` is new to the list** — it had no `make` target at all until 2026-08-19 (§6d).

**Latest arch packet read: `ROUTING-2026-08-19-c`** (arch `c984f93`); browser-rust's
`ROUTING-2026-08-19-d` read at their `fbc2c5c`. D21 — this line is the subtraction that tells the
next session what it has not opened. Read *every* document naming this repo, `cc` included:
`grep -ril 'workbench-go' ../entity-system-architecture/docs/status/`.

**Inbound this session, all three answered:** `-19-b` §2 (validator widening → §6c), `-19-c` (the
registry board is closed; our only row was the widening), and browser-rust's `-19-d` (consume-us
ask → §6d, plus their F6 correction folded in as AP22).

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
| 5 | The first `app/share/*` record | **arch half DELIVERED, our half STARTED (`146f9a4`).** `APP-CONVENTION-SHARE` v0.1 authored (`bb86cd1`, `ROUTING-i` §4); `entitysdk/share.go` ships the `app/share/*` type vocabulary, the tagged target union, `ShareGrants` (with `peers` omitted), `ValidateShareGrants` (the §1.1 MUST as a refusal), `AuthorShare` and `ShareWithdrawalNotice`. Vectors **SHARE-4** and **SHARE-6** pass, plus three shape pins. The authoring/validation layer is pure, so it is green **through** the kernel block; persisting the record + delivering tokens is the half that waits. `strategy` still open on browser-rust (step 4) |
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

**Piece 1 of 4 landed 2026-08-18 — self-publication.** `AppPeer.AdvertiseTransport(dialURL)`
writes a `system/peer/transport/*` profile under **our own** peer-id, and `PeerManager.Create`
calls it when the listener binds (new `Config.AdvertiseURL` for when the routable address differs
from the bound one). Wildcard and port-0 advertisements are **refused, non-fatally** — the peer
still listens, `HostedPeer.AdvertiseErr` says why, and nothing false goes in the tree. Pinned by a
real-session test through `Create`, mutation-checked. Found on the way: the Avalonia bridge's
`PeerListenAddr` reported `listening: false` for **every** WebSocket peer, because it gated on
core-go's `Peer.Addr()` and only `ListenReady` sets `p.listener`; fixed to gate on `ListenScheme`.
**Pieces 2 and 3 landed the same day.** The lesson from AP13 applied immediately: **core-go
already had both halves.** `core/peer` writes every liveness transition (`connected` at handshake,
`suspect` at the dispatch seam, `disconnected` on keepalive miss) whether or not anything is
listening, and `ext/network` implements maintain-peer / release-peer / status / close plus the
§4.1 reconnect continuation graph. Neither needed authoring — they needed *registering* and a
consumer.

- **Liveness read-model** — `Store.PeerLivenessOf` / `PeerLivenessAll` / `OnPeerLivenessChange`
  (+ `AppPeer` wrappers) over `system/peer/status`, and `workbench.PeerLivenessModel` as the
  renderer-neutral view with connected/suspect/disconnected counts. Prefix-subscribed, never
  scan-and-filter. This replaces reading `ConnectedPeers()` in a renderer: the pool snapshot
  cannot express `suspect`, cannot say *why* a peer went, and disagrees with the tree whenever a
  connection is evicted without a demotion. Three properties the model carries deliberately:
  absence is reported as absence (a stranger is not a goodbye), `LastSeen` is a transition
  snapshot and **not** a heartbeat (§5.4.1), and the enum is three-state — `reconnecting` is not
  a status the tree can hold.
- **`system/network` handler wired** (`ExtensionsConfig.Network`, default-on) with the
  post-construction `Bind`, plus `entitysdk.NetworkClient` — `MaintainPeer`, `ReleasePeer`,
  `Status`, `MaintainedPeers`, `Close`. Registering it starts nothing: the continuation graph is
  installed per-peer by a maintain-peer call.

**Spec finding, found by running it:** §2.7's `maintained_peers` is **not** the maintained set.
§4.3's own pseudocode enumerates every entity under `system/peer/status/`, so a released peer
keeps its row and loses only its `session_id` — verified against a real handler when the obvious
assertion failed. Routed to arch; `NetworkClient.MaintainedPeers()` is the `session_id != ""`
filter in the meantime.

**Remaining on the four: connector registry + `meet`** — and both of its gates have now returned,
so the next session starts here rather than re-scoping it. **D20 pre-check done (2026-08-18), so
nobody prices this against our own tree again:**

- **The shape is ruled.** Our `CONNECTIVITY-CONVERGENCE` §2 said we would take piece 4 earlier *"if
  your `meet`-as-DISCOVERY-backend question (arch Q8) returns in a shape that makes 4 cheap."* It
  returned on **2026-08-17** — `ROUTING-2026-08-17-c` §1: **confirmed, token minted `rendezvous`**,
  `pair` mode is **not** discovery, and the candidate is the §2.2 successor pair with
  `identity_hint` **absent** (TOFU). `meet` is a DISCOVERY backend implemented on the SIGNALING
  carrier — *the key introduces; it never authorizes* (SIGNALING §1.2) meeting DISCOVERY §2's
  *discovery is the initiator of the grant, never the authority*. Read
  `PROPOSAL-DISCOVERY-RENDEZVOUS-BACKEND`, not the summary, before building.
- **The carrier already exists in the substrate.** `../entity-core-go/ext/signaling/` ships
  `HandlerPattern = "system/signaling"`, ops `offer` / `collect` / `advertise` (`const.go`), a
  `Client` (`client.go`), plus `punch.go`, `webrtc.go`, `reflection.go`, `pool.go`, `coordination.go`
  and the rendezvous `key.go`. **There is no `meet` op and there should not be** — `meet` is the
  DISCOVERY-side logic over this carrier, which is the piece that is genuinely ours.
- So piece 4 is the same shape pieces 2 and 3 turned out to be (AP13/D20): **registration and a
  consumer, not authoring.** What is absent in our tree is an `ext/signaling` consumer — grep:
  `grep -rn 'ext/signaling' entitysdk/ workbench/ shellcmd/ shellboot/` returns nothing.
- Constraints unchanged and confirmed: SIGNALING §3.4 same-provider is a **MUST** (both arms on the
  same pool or silent never-meet); `data_relay` is `policy: open` only; **no TURN credential
  mechanism exists anywhere in the corpus** (arch Q18) — build against static config and route the
  wall rather than invent a credential shape.

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

### 5. R3 — the resolver-config ships (2026-08-18)

Arch's `096fa96` folded the default `name_format_dispatch` globs into
`EXTENSION-REGISTRY` §4.1a. That was **R1**, the item R3 was waiting on, so R3 — ours jointly
with browser-rust — is unblocked and now done on our side.

D20 check first, and this time the substrate did **not** have it: core-go reads the config and
applies the dispatch list, but nothing in the cohort writes a default one (construction exists
only in the `validate` harness). Shipping it is app-tier work, as arch said.

`entitysdk/resolver_config.go` — `DefaultNameFormatDispatch` (the six §4.1a rules, in order),
`DefaultResolverConfig` (that list + a local-name-only chain), `ValidateResolverConfig`, and
`AppPeer.InstallResolverConfig` / `ResolverConfig` / `EnsureResolverConfig`.
`EnableLocalNameResolver` now writes the dispatch list too — it used to write the chain alone,
which was harmless only by accident (no dispatch list ⇒ every name consults every backend, and the
chain happened to be local-only).

- **Order is the contract.** First-match-wins, and rules 4/5 overlap on every dotted authority. A
  glob cannot say "undotted", so `*@*.*` must precede `*@*`. The pin asserts the sequence — a
  set-membership test passes on the reversed list, which routes every domain-scoped name to
  peer-issued and then toward a catch-all that must not see it.
- **The catch-all MUST be local-only** (§4.1 step 2 — "the primary privacy mechanism"). We
  **refuse** rather than normalize: §11.1 permits either, but silently rewriting an operator's
  privacy config into a different one means they never learn they did not get what they asked for.
- Two cohort observations we filed as inert. **They were not** — see §6b.

**Operator surface:** `peer status` (new) renders the tree's lifecycle record —
connected/suspect/disconnected, the transition reason, and a coarse age — deliberately a
*different* answer from `peer ls`, which lists this session's alias table. A peer that connected
to **us**, or one released an hour ago, appears in the first and not the second, and only the
first can say `suspect`. Empty output says *"nothing has ever transitioned"* rather than showing a
blank table that reads as "nothing is connected". The SINCE column shows `failing_since` or
`connected_at` and **never `last_seen`** — rendering a transition snapshot as "last heard from"
would tell an operator a healthy peer had gone quiet for hours.

### 6. CLEARED — the tree was red across 6 of 9 suites, and it was core-go's (2026-08-18)

> **RESOLVED same day. core-go `7593618` — `DispatchLocalExecute` now passes
> `handler.WithResource(req.Resource)`.** Re-run here against it: **`make test` exit 0, zero
> failures** across its eight suites (`entitysdk`, `shell`, `shellboot`, `shellcmd`, `shellpanel`,
> `workbench`, `programs`, `inspect`), plus **`make test-publish` green** separately — `publish` is
> not in the `make test` target. `make lint` clean. **323 → 0.** Our call sites were correct the
> whole way down and nothing here changed to accommodate the defect — the decision not to work
> around it in the app repo is what kept the fix a one-liner in the right tree.
>
> *(Per AP15: the count above is from a run that completed. A first attempt exited 2 on a transient
> `cd: can't cd to shell` while other container jobs were touching the tree concurrently; re-run
> clean with nothing else running, `shell` passes in 2.27s. Reported rather than quietly dropped.)*
>
> **The reproducer became their regression test.** `TestDispatchLocalExecute_CarriesResourceToHandler`
> is our kernel-level shape landed in `core/protocol/local_entry_resource_test.go` — red pre-fix at
> status 200 with `Resource == nil`, green after.
>
> **Their ratchet, worth carrying here too:** `dispatch_equivalence_test.go` asserted *result*
> equality between the wire and in-process entry paths but never the handler-visible *context*, and
> `subdispatch_resource_dimension_test.go` drove both sub-dispatch directions without ever entering
> through `DispatchLocalExecute`. **Two entry paths claiming equivalence need a test that asserts
> the handler-visible context, not just the result.** The SDK path exercised it; theirs did not.
> Folded as **AP17**.

The history below is kept because the *shape* is the lesson, not the outage.

### 6 (historical). BLOCKED — `make test` is red across the tree, and it is core-go's (2026-08-18)

**Not ours, not worked around, routed — and it has LANDED in core-go (`0b9e261`), not merely
in-flight.** The change removes resource inheritance in sub-dispatch per arch
`ROUTING-2026-08-18-g` §5 (**ruled, normative** — `ENTITY-CORE-PROTOCOL` §5.2 at arch `980ddf1`). The ruling is right. Removing the inheritance also removed the only channel by which
the in-process **entry point** delivered a resource it was explicitly given:
`DispatchLocalExecute` sets `rootCtx.Resource` and then dispatches with `WithCapability` alone.

**Measured tree-wide in the 2026-08-18 audit — 323 failures across 6 of 9 suites**, which is
substantially worse than first reported. `make test` stops at the first failing package, so the
earlier per-suite numbers were taken through a keyhole; each suite must be run on its own
(`make test-sdk`, `test-shellcmd`, …) to see the radius:

| suite | failures | |
|---|---:|---|
| `entitysdk` | **171** | first reported as 5 |
| `shellcmd` | **71** | |
| `programs` | **46** | 52 with subtests |
| `shell` | **14** | first reported green — it was not |
| `shellboot` | **12** | first reported green — it was not |
| `inspect` | **9** | not previously reported |
| `workbench`, `shellpanel`, `publish` | 0 | genuinely green |

**It wears five faces, and that is the part worth remembering** — the same defect will not present
the same way twice, because each handler validates its resource independently and says so in its
own words: `resource target path is required` (tree, 306), `bind_cap` (37, downstream of a failed
put), `resource target is required for subscribe` (17), `ambiguous_resource: install requires
exactly one resource` (17), `missing_resource_path` (role, 5). Do not diagnose these separately.
The rest are **cascade** — a test whose setup `Put` was refused then reads an empty tree and
reports a wrong count, a missing path, a surviving roster entry. All one defect.

**The mechanism is now isolated, not just argued** (the measurement the previous handoff flagged as
owed). A kernel-level reproducer — `protocol.NewDispatcher` + `DispatchLocalExecute` with a
wildcard grant and a handler that records `req.Context.Resource`, **no workbench code in the
path** — shows the handler running at status 200 and seeing `Resource == nil`. It is in the packet
verbatim, as the test core-go is missing.

Full packet, including the one-line fix, the reproducer, and the coverage gap that let it through:
`docs/architecture/reviews/CORE-GO-LOCAL-DISPATCH-RESOURCE-2026-08-18.md`.
**Re-run `make test` once it lands.** Do not work around it here — the call sites are correct.

**Still live at core-go `6ed6f95`** (re-verified in the audit): `git log 0b9e261..HEAD --
core/protocol/local.go` is empty — nothing has touched the file. Six commits have shipped on top,
including `88615f6` *"three-way release gate — go clean"*. That green is real for their suites and
does not cover this path, which is the whole point of the coverage gap. **The packet is still
unsent** — it does not reach them until `dev` is pushed.

### 5a. The cross-impl publish/consume check — three surfaces align, the front door does not (2026-08-18)

The ADR-0012 result: every signed-root result either arm holds is **same-language**, so ours and
browser-rust's agreeing with themselves is cohort-consistent, not independent convergence.
`publish/cmd/crossimpl-fixture` emits a deterministic Go site (pinned seed, so peer-id and every
hash below it are stable) to hand their reader.

Measured against browser-rust `a0145a7`'s `DirFetcher`:

- **Aligned, with no shared code:** content sharded `{aa}/{bb}/{hex}` on the 66-char wire hex,
  bare-hashable bodies, and the two-hop signature at `system/signature/{root_hex}.bin` keyed on the
  published-root entity hash. Their doc calls the latter two *"divergences from upstream"* — they
  are **not** divergences from us. That is the part worth keeping.
- **Not aligned — and it is hop 0.** `DirFetcher::manifest()` reads
  `{base}/{peer_id}/system/peer/published-root`. **Half of this is now FIXED in the kernel:**
  core-go `2bd2380` (ruled by arch *from this run*) dropped the `/{base58_peer_id}` segment that
  `PublishedRootStoragePath` appended — the qualified binding had named the peer twice, and
  core-go's writer and reader shared the helper, so Go-on-Go passed deceptively. Adopted here in
  `00b92c7`; the directory collision is gone.
  **The remainder, re-measured:** we emit `…/published-root**.bin**` (our advertised
  `tree_leaf_suffix`, since the head pointer *is* a tree leaf) holding a 2-key `system/hash`
  pointer; their `manifest()` reads the suffix-less path and expects the 3-key wire entity, which
  we emit at `{out}/manifest`. `ENOENT` now rather than `EISDIR` — still hop 0, and now a narrow
  question about which artifact belongs at which path.

**RULED 2026-08-18 in our favour — `ROUTING-2026-08-18-p` §3, `EXTENSION-NETWORK` 1.8.** The
manifest's location is **discovered** from `manifest_url_prefix`, never derived by convention from
the tree path, and *a consumer MUST NOT join `signed_pointer` onto an origin*. The two fields answer
different questions: `manifest_url_prefix` is where to GET it, `signed_pointer` is what the origin
is asserting. `{origin}/manifest` and `{origin}/{peer}/system/peer/published-root` are equally
conformant; only the advertised one is findable. **`DirFetcher::manifest()` is the defect and the
fix is browser-rust's.** Arch rejected "serve it at both paths" — *"two front doors is not
compatibility; it is the divergence, ratified"* — and upheld the decision not to move our layout.
Nothing owed here; our `manifest_url_prefix` advertisement was conformant throughout.

**Fixture re-cut and handed over (`d940ce0`), and the re-cut found a defect of ours.** The packet
told browser-rust the emission was byte-identical on their machine. It was not: `published_at` is a
field **of** the published-root entity, so a fresh clock moved the root's content hash, the
`system/signature/{root_hex}.bin` binding named after it, two content shards and `{out}/manifest` —
every artifact their reader enters through. Only the trie root and the entities beneath it were ever
stable. `publish.Opts.At` now pins the instant (zero still means `time.Now()`), the fixture pins it,
and two fresh runs diff clean. **AP18** — a claim about emitted bytes settled by reading the emitter
instead of emitting twice and diffing.

Corrected fixture facts (core-go `7593618`): `peer_id 2KLv2nhwtPrL…`, trie
`ecf-sha256:f567bfbd…`, published-root `00e0138dbeb374…`, signature `000ef5f255803…`.

**Still not run end to end:** executing their reader against our fixture needs a test in *their*
tree. Per D19/AP10 everything above except the emitted bytes ships as a prediction with a
reproducer attached.
Packet: `docs/architecture/reviews/CROSSIMPL-PUBLISH-CONSUME-2026-08-18.md` (UPDATE 2 carries the
ruling and the corrected hashes).

### 6a. CORRECTED — R3 shipped against a spec sentence arch withdrew 78 minutes later (2026-08-18)

`24169b9` shipped the resolver-config against `EXTENSION-REGISTRY` **1.6** §4 (*"an ORDERED list,
first-match-wins (MUST)"*). Arch withdrew that in `3670283` → **1.7**: the list is a **filter**, a
name matching several entries is eligible at the **union**, and precedence is
`resolver_chain[].priority`. The `#` column is reference numbering, not evaluation order.

**One live defect came out of it and is fixed.** `ValidateResolverConfig` refused a config whose
catch-all was not the final entry (`catchall_not_last`) — correct under 1.6, where everything below
a catch-all was dead config; under 1.7 those entries stay eligible, so the refusal **rejected a
deployment the spec permits**. Removed, with a regression pin naming the withdrawal
(`TestValidateResolverConfig_CatchAllPositionIsNotADefect`). The order pin became
`TestDefaultNameFormatDispatch_MatchesTheSpecTable` — it pins the six rows and their
`backend_kinds`, not a sequence.

**What did not change:** the six default rows, emitted verbatim in the table's sequence (now
documented as presentational), and **§4.1 step 2's catch-all local-only MUST, still enforced at the
write as a refusal**. 1.7 makes that the load-bearing rule explicitly — the same conclusion resting
on the right sentence.

Routed: `docs/architecture/reviews/RESOLVER-CONFIG-FILTER-CORRECTION-2026-08-18.md`, which also
carries the two cohort observations (`did-key` vs `self-certifying`; `pinned` has no constant) and
the `ROUTING-2026-08-18-i` acknowledgement.

### 6b. REGISTRY v1.13 adopted — both "inert" observations were live defects (2026-08-18)

`f79cc4a`. Arch's `-p` §5 came back on the two cohort observations §5 filed as inert: **they were
dead config in every conformant peer**, because §4.2 makes an unknown `backend_kind` MUST-skip with
a warning, so our shipped default list contained rows a conformant implementation is *required to
discard*.

| row | was | now (v1.13) |
|---|---|---|
| 2 `did:key:*` | `["did-key"]` | `["self-certifying"]` |
| 6 `*` | `["local-name", "pinned"]` | `["local-name", "self-certifying", "out-of-band", "peer-issued"]` |

Row 6 is **not** what `-p` said — v1.12 removed the undeclared `pinned` without naming the declared
token that does the job, and `-q` §2 corrects it to `out-of-band` (§4.1.2: the kind a pin's
synthesized binding carries; §6a.4 makes it dispatchable where `pinned` is not). The row moved three
times in one day and browser-rust pinned the middle version. **Our pin now names the spec revision
it was taken at**, so the next move presents as a red test with a version to compare.

**The tell was in our own source: we had to invent both constants.** `BackendKindPinned` and
`backendKindDIDKey` existed only because §4.1a named strings core-go's enum does not declare, each
with a doc comment explaining the absence. We wrote that explanation twice and still filed it as an
observation. **AP20** — a constant you have to invent locally to satisfy a spec table is a defect in
one of the two documents, never a naming gap.

**The catch-all MUST is re-keyed, and our guard had been refusing a legal config.** `-l` §1
(REGISTRY 1.8) was cc'd to us and unopened: the banned property is **name transmission, not
remoteness**. The two come apart exactly at `peer-issued`, which §6a.4 resolves by content address
through a signed root so the queried name never appears in a request. Our allow-list was
`{local-name, pinned}` — it refused `self-certifying` and `out-of-band`, which dial nobody, and once
rows 2/6 were corrected `DefaultResolverConfig()` failed `ValidateResolverConfig()`: the helper that
ships the default could no longer install it. Now a deny-list over the four disclosing kinds
(`dns-txt`, `well-known-url`, `did-web`, `consensus-anchored`), because §4.2 makes an unrecognized
kind inert and refusing on account of one rejects a config a newer vocabulary permits. Code renamed
`catchall_not_local` → `catchall_transmits_name`.

**The old pin passed under both rules** — it tried exactly one forbidden kind, `peer-issued`, which
the re-key moved from forbidden to permitted. Green was our only evidence the guard was right and it
was compatible with the guard being backwards. **AP19.** The replacement enumerates all four
disclosing kinds and all four admitted ones.

**Finding routed to arch:** §11.1's `REG-DISPATCH-CATCHALL-LOCAL-1` was **not** moved with §4.1
step 2. It still says a catch-all naming *"a remote backend"* MUST be refused and that resolving a
bare name MUST produce *"no read against any remote registry"* — both false against row 6, which now
ships `peer-issued`. **An implementation passing that vector literally refuses the default list the
same document tells it to ship.** Same failure as rows 2/6 one layer out: the rule was re-keyed and
the artifact that tests it stayed on the old property. Nobody copies §11.1, so it drifted silently.

Also pinned: REG-DISPATCH-GRAMMAR-1's refusal half (`-q` §1). The grammar is closed and every non-`*`
byte is a literal, so **no pattern is invalid** and a registry MUST NOT reject one for `?`, `[`, `\`.
We author patterns and never match them, so that is our whole exposure — pinned rather than assumed,
because "closed grammar" has meant "reject at write" everywhere else in this corpus.

Packet: `docs/architecture/reviews/REGISTRY-V113-ADOPTION-2026-08-18.md`.
**D21 earned** (AP12 promoted): a *cc'd* packet is a packet. Session start now greps the arch repo
for every document naming this repo, and STATUS carries the last letter read.

### 6c. REGISTRY v1.14 — the MUST binds the configuration, and our validator saw one row (2026-08-19)

`b9e99e7`, answering `ROUTING-2026-08-19-b` §2 (arch read our tree at `0ba80c6`, source, this
session). Arch widened §4.1 step 2 from the **catch-all row** to the **configuration** — D4, because
a rule that binds one row is evaded by not writing it. Ours enforced the row: `if d.Pattern !=
CatchAllPattern { continue }`.

| door | case | code |
|---|---|---|
| 1 | a **broad** pattern that is not the catch-all — `al*` naming `dns-txt` | `broad_pattern_transmits_name` |
| 1 | the catch-all itself (unchanged, so existing diagnostics still resolve) | `catchall_transmits_name` |
| 2 | **absent/empty** `name_format_dispatch` while a name-transmitting kind sits in the chain | `filter_disabled_transmits_name` |

Door 2 is the one the loop body could not reach — with no rules there is **no row to inspect**, and
`eligible_kinds` returns ALL, so every kind is eligible for every name. `EnableLocalNameResolver`
has carried a doc comment naming this exact hazard since 2026-08-18 with nothing enforcing it: **a
named hazard with no gate is a comment.** Both doors mutation-checked against the pre-widening code.

**§11.1's placement, which we had never implemented:** *"refused or normalized at load"*. Ours ran
on author only. `AppPeer.ResolverConfig` now validates on read, returns the config **anyway**
(non-zero, beside the error — a load-time refusal denies use, not sight), and `EnsureResolverConfig`
refuses rather than reinstalling the default over an operator's config. The case is not
hypothetical: what a config *means* depends on a vocabulary outside it, so an entry that is inert
under §4.2 today becomes disclosing the moment core-go declares that kind.

**Open, routed to arch:** the widened MUST says *"any rule whose pattern matches unscoped names"*
and **supplies no decision procedure**. It cannot be read literally — a name is a flat string, so
`alice.eth` is bare and §4.1a row 3 (`*.eth` → `consensus-anchored`) would violate the MUST the same
table recommends. We read "unscoped" as *carrying no explicit authority marker*, and our
`matchesUnscopedNames` parts from browser-rust's `is_broad` on patterns like `*e` (theirs: narrow;
ours: broad). We took the strict side — it is what their own doc sentence argues for, and refusing
an exotic config costs an error message while admitting one costs every name a user types. A `§11.1`
row is needed; everything in §4.1a is grammar-identical under both readings, which is the same shape
that let `*.lab` survive review in the `name_constraints` case.

Packet: `docs/architecture/reviews/REGISTRY-V114-VALIDATOR-2026-08-19.md`.

### 6d. The CDN corridor runs in both directions — and ours was broken at our own end (2026-08-19)

`c13dfe2`, answering browser-rust's `ROUTING-2026-08-19-d` §5, the one thing they asked for:
**consume us.** Doing it found our half broken first, and worse than theirs — **`fetch` could not
read `publish`.** Four divergences at once:

| `fetch` derived | `publish` emits |
|---|---|
| `{base}/{peer}/tree/{path}.bin` | `{base}/{peer}/{path}.bin` (§6.5.3.1 has **no `tree/` reserved word**) |
| a raw 33-byte hash at the leaf | `ECF({type:"system/hash", data:H})` — Amendment 6, two-hop |
| `sharded-2-flat` | `sharded-2-4` — *and the profile declares it* |
| hex of the 32-byte digest | hex of the **33-byte wire form** (§6.5.3.1 MUST) |

**Both suites were green the whole time**, because each half asserted its own idea of the layout and
nothing asserted they were the same one. Structurally: **`fetch` had no `make` test target and
`publish` was not in `test-native`** — "run everything" ran neither end. Both are in the sweep now,
plus `fetch` in `LINT_MODULES`.

What replaced the derivation is `fetch.Layout` — the Go counterpart of browser-rust's
`PublishLayout`. Peer-id, all three URL prefixes, content layout and both suffixes come from the
publisher's http-poll profile, decoded with **core-go's own type**. One convention is left on
purpose: the well-known `{origin}/transport-profile` a cold-start consumer enters at.

Gates, both mutation-checked and both in the sweep:
- `publish/consume_test.go::TestPublishThenFetch_TheTwoHalvesOfOurOwnCorridor` — our publisher →
  our consumer over `httptest`, cold start, signed root through `manifest_url_prefix`, every page
  hash-verified, absence reported as `404` rather than as unreachability.
- `fetch/crossimpl_test.go::TestConsumeBrowserRustSite` — **their** emission, frozen at
  `fetch/testdata/crossimpl-rust-site/` (their `dev` @ `fbc2c5c`, provenance in its README), four
  entities across two of their sites. Drop the peer-rooted bridge and it 404s at hop 0 — their
  reported failure, reproduced from our side.

**Findings routed, not worked around.** (1) core-go's `types.BuildContentURL` hexes
`EffectiveDigest()` — the digest-only form §6.5.3.1 excludes **by MUST** — so it builds a URL that
resolves against neither publisher in the cohort. (2) `entity.Validate()` cannot be used on a
`CONTENT_GET` body: those are the bare 2-key hashable form, so it reads the absent `content_hash` as
a zero hash and fails against it. That was our own **AP22** instance — a guard that refuses
everything unfamiliar — the same shape as browser-rust's audit F6, which they reversed the same day.

`tree_url_prefix` stays arch's. We consume **both** joins (last segment exactly the peer-id ⇒
peer-rooted; otherwise append), which is a bridge, not a third convention.

**Ratchet: AP21 → D22.** *A contract between two components is only tested by a test that crosses
it; per-side tests are evidence about each side.* Second shape of AP17 (core-go's
`DispatchLocalExecute` equivalence claim, asserted on return values) — different repo, different
layer, one lesson. Charter is now D1–D22 / AP1–AP22.

Packet: `docs/architecture/reviews/CROSSIMPL-CONSUME-RESULT-2026-08-19.md` (to browser-rust, cc arch
+ core-go).

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
  and step 5 waits on it by construction. **Also: the fixture run.** `DirFetcher::manifest()`
  reads `manifest_url_prefix` per `-p` §3, then runs our re-cut fixture (`d940ce0`, byte-stable)
  and tells us where it actually stops.
- **arch:** `REG-DISPATCH-CATCHALL-LOCAL-1` re-keyed or explicitly scoped — §11.1's vector still
  bans remoteness while §4.1 step 2 bans name transmission, so it contradicts §4.1a row 6's
  `peer-issued` (§6b). Blocks nobody today; misleads everybody later.
- ~~**arch:** `APP-CONVENTION-SHARE` authored~~ — **DELIVERED 2026-08-18.**
  `specs/applications/APP-CONVENTION-SHARE.md` v0.1 exists (arch `bb86cd1`, routed as
  `ROUTING-2026-08-18-i` §4). **`app/share/*` is no longer blocked by arch.** Four constraints to
  build against: a share is a titled grant (`resources` = what is shared, audience = the minted
  token's `grantee`); **`peers` MUST be omitted** (populating it 403s every cross-peer presentation
  and still passes local testing, because our single-identity tests collapse root/grantee/granter);
  type tags are `app/share/*` (the index key for cross-peer aggregation — a tag under our own
  prefix breaks browser↔go interop); mirrors write the publisher's paths verbatim. Withdrawal is
  asymmetric — `request`-minted tokens are **not** recallable and a UI **MUST NOT** imply otherwise.
  Not ratifiable (zero vectors); `SHARE-4` and `SHARE-6` are the two owed vectors that fail loudly
  on the intuitive-but-wrong reading, and are where we start.
  **Step 5 is now blocked only on the core-go kernel defect (§6), not on arch.**
