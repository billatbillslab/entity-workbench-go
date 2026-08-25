# entity-workbench-go

Read **AGENTS-STANDARD.md** first. This file adds entity-workbench-go specifics.

> **Reference an ADR; do not copy one.** `[ADR-NNNN]` unqualified means the *ecosystem* ADR —
> read it at its source rather than keeping a copy here, because a copy goes stale silently.
> Cite this repo's own as `[<repo>-ADR-NNNN]`. Two that change daily work: **[ADR-0031]**
> (`docs/status/` publishes nothing — so it is written for the next session, with no scrub
> obligation) and **[ADR-0012] Am. 1** (in a *canonical* doc, cite by content or a release tag,
> never a branch SHA, because published history is authored fresh at the release boundary and an
> internal SHA resolves to nothing for a reader).
>
> **`git add -A` is not safe in this tree.** `AGENTS-STANDARD.md` and `METHODOLOGY.md` are shared
> files maintained outside this repo and updated in place, so they can change underneath you
> mid-session — and a blanket `add -A` then sweeps thousands of lines you did not write into a
> commit about something else. It has happened here. **Stage explicit paths, or read
> `git status` before staging.** The same habit generalizes: when a tool or a teammate tells you
> what landed in your tree, measure it in your tree before repeating the number.

## Overview

Application + performance layer for the entity ecosystem — **not** a conformance
implementation. Ships **entity-shell** (the primary CLI / leading edge of feature
development) and an **Avalonia** desktop frontend, plus a frozen **console** (tview)
renderer, all over the Go workbench stack / V7 protocol / `entity-core-go` store. The
in-tree `entitysdk/` is the **de facto reference SDK** (stewarded here until it spins out
— treat it as the authoritative Go SDK impl, not workbench-internal glue).

## How we work here — Disciplines & Doctrines · tier **FULL**

This repo runs the entity-OS methodology at the **Full** tier for the Avalonia/.NET UI runtime
— held where conformance alone can't reach a GUI. The framework is `METHODOLOGY.md` (maintained
upstream, identical in every repo); the charter below carries the local grounding, and **this repo is one of
the worked instances the framework was reconciled from** — D1–D11 there are inherited verbatim,
D12–D24 here are ours, earned on the eight crash-hunt commits, two feedback episodes, the
2026-08-18 publisher/connectivity pair, the v1.13 adoption trio, the 2026-08-19 cross-impl
consume run, the 2026-08-20 reachability audit, and the 2026-08-21 crash hunt that found a
month-old fatal bug the moment an instrument could reach it.
- **Disciplines** (invariants — the *what*): `docs/architecture/DISCIPLINE-CHARTER.md` —
  D1–D24, the ten review questions, the anti-pattern catalog AP1–AP43, and the promotion
  criteria (§5) that the ecosystem ladder generalizes.
- **Substrate model** (ground truth): `docs/architecture/MODEL-AVALONIA-RUNTIME.md` — what
  the Avalonia/.NET/Skia/X11 runtime actually does (stack diagram, lifecycle matrix, the
  seven-boundary map — **Boundary G is the POSIX signal layer, added 2026-08-21** — and the
  invariants). Read before any layout/lifetime/render/**crash** work.
- **Recipes & conventions** (patterns that respect the rules):
  `docs/architecture/GUIDE-AVALONIA-PANEL-PATTERNS.md` (P0–P7, new panels lift these) +
  `TESTING-STRATEGY.md` + `LOGGING-CONVENTIONS.md`.

Session start: read the charter → the substrate model → anything newer than the point
`docs/STATUS.md` records as last read, and update that marker. A specification change that moves a
*table*, a *default*, or a **MUST** is read the same session it is found, **before feature work** —
it has twice been the case here that a landed spec change sat unread while we shipped past it, once
leaving a validator rejecting a configuration that had become legal.

Three rules about work that crosses a repo boundary, each earned the hard way:

- **An artifact that exists only in your working tree does not exist.** Commit and push before the
  session that produced it ends, and cite the hash. We once had a specification revision folded
  upstream — and implemented elsewhere — on the strength of a document that was in no commit in any
  repo, so the provenance chain for a normative change terminated in one machine's working
  directory. We had written that exact rule *outward* hours earlier and could not see it pointed at
  ourselves.
- **Delivery is a fact; addressing something is an intention.** Before carrying a *"waiting on
  them"* row forward, establish the other side actually has it. Search for the **subject**, not the
  filename — people cite your commits and your claims, never your file paths, so a filename miss
  means nothing. One row here sat blocked for 24 days on a document that had never arrived.
- **When you read another repo, its git is read-only.** `git status` first, stage specific paths,
  never `git add -A` outside your own working directory.

*(Sibling repositories named in this file sit beside this one under a shared parent. A `../` path
is relative to the repo root; the `../../` form is relative to a Go module directory.)*

Task start: open the matching doctrine. Every
feature/audit ends by feeding its lessons back into the disciplines — the ratchet (*a feature
must make us stronger, not weaker*).

**Doctrines — one exists now.** `docs/architecture/DOCTRINE-CRASH-FORENSICS.md` is this
repo's first, earned on the 2026-08-21 hunt: the procedure for a crash that leaves no managed
dump, in the order that actually converges (reach before forensics; first signal before any
dump; `si_code` before `si_addr`). Open it at the *start* of any crash investigation — its
whole point is that the intuitive order wastes days.
The rest are still owed: for Feature / Audit / Foundation work, run the procedures as
`METHODOLOGY.md` §7 states them and codify the substrate-native steps here when a run surfaces
one — name the recurring cycle first, then let each step own one lever of it.

## Setup / environment

- **Go pinned to 1.25.1** (forced by core-go's `ext/go.mod` `go 1.25.0`) — the Makefile
  pins it. Per AGENTS-STANDARD, never set `GOTOOLCHAIN=` inline.
- **Sibling `../entity-core-go/` is required.** Every `go.mod` uses `replace` directives
  resolving to `../../entity-core-go/core` and `../../entity-core-go/ext`; without the
  sibling, `go build` fails at module resolution. README documents the layout.

## Build & test

`make` is the build interface (see AGENTS-STANDARD). Full target catalogue is in the
`Makefile` header.

- **`make test-each` is the target to reach for when you want to know the state of the
  tree.** It runs all ten suites **to completion** regardless of failures, prints a pass/fail
  table with per-suite timings, leaves logs in `.test-logs/`, and exits non-zero if any
  failed. ~12 min.
- `make test` — full sweep (`-race -count=1`); `entitysdk`/`shellcmd` slowest; pin tests
  live in `entitysdk`.
- **AP15 is about fail-fast REPORTING, not about `make`** — the same defect lives inside any
  test that loops. `TestAxis1Equivalence_Differential` swept 300 cases and `t.Fatalf`'d on the
  first divergence, so the tree reported **one** for three days when there were **three**. The
  tell is `Fatalf`/`break`/`return` inside a range over cases, corpora or fixtures: **the count
  it yields is a lower bound.** Collect, then report. And if you waive a known failure, waive a
  *signature* and pin the instance set, so a fourth instance is not silently absorbed and the
  waiver fails when the defect goes away.
- **A probe that does not reproduce the failing shape refutes nothing** (AP43). The same three
  divergences had a correct diagnosis, which we then retracted on two probes that ran cleanly
  and were about the wrong position — an out-of-range index in a *consumed* position, where both
  engines were already correct, when the defect lived only at a *closure-result* position. The
  cause went back to "unknown" in a published CHANGELOG for a day. **Before a refutation retires
  an explanation, show the probe reaches the mechanism**: name the position/branch the hypothesis
  predicts will fail, and demonstrate a failing run before the fix rather than only a passing one
  after. *"I measured it and the explanation is dead"* is a far more expensive claim than
  *"nobody has measured this"*, and it travels faster because it sounds more rigorous.
- **When two implementations disagree on GENERATED input, the generated input is the evidence** —
  reach for this first, not after a round of hypotheses. Regenerate at the fixed seed, dump the
  IR of each diverging case, then **evaluate every subnode on both engines, children first, and
  print the deepest disagreement.** One throwaway test file converted three days of "cause
  unknown" into a named position in a spec, in one run. The scaffolding is deliberately not kept
  in the tree; it is twenty minutes to rewrite and it is wrong to maintain a debugger as a test.
- **`make test` STOPS at the first failing package, and `-k` does not cross the container**
  (`test` = `IN_CONTAINER make test-native`, so `make -k test` still aborts at the first
  sub-target). **Never quote a failure count taken from a red `make test` run** — it is a
  lower bound covering one package. This is **AP15** — we routed "shell and shellboot are
  green" to another repo when they had 26 failures between them, and reported `entitysdk` as
  5 when it was 171. `make test-each` exists precisely so the correct thing is also the easy
  thing; it replaces the hand-typed `for t in sdk inspect …; do make test-$t; done` loop this
  file used to make you remember.
- **Never run `make test-each` and `make -C avalonia test` at the same time.** Both bind-mount
  this tree into podman with `:Z` (private SELinux relabel), and the second relabel revokes the
  first container's access mid-run: every suite after the first reports as failed with
  `.test-logs/<suite>.log: Permission denied` while the log itself says `ok`. **A sweep that
  reports red for a reason outside the tree is worse than no sweep**, and this one is
  indistinguishable from a real failure at a glance. Run them serially. (Measured 2026-08-21.)
- **Some failures are load-dependent** — `TestE2E_Bidirectional_BurstWrites_NoFS` fails under
  full-suite load and passes when run alone, so a targeted re-run is **not** evidence a
  `test-each` failure was spurious. Check `docs/STATUS.md`'s green line before
  attributing a red suite to your own diff.
- **`make lint` is `go vet` only — it does not check formatting.** Nothing gates gofmt, so
  drift accumulates silently (60 files at the 2026-08-18 audit). Run `make fmt` as its own
  commit, never folded into a feature diff.
- `make test ARGS="-run X -v"` — single test / forwarded flags (`ARGS=` is the only
  passthrough syntax `make` accepts).
- `make test-sdk` / `test-shell` / `test-shellcmd` / `test-workbench` — per-package suites.
- **`test-publish` and `test-fetch` are the CDN corridor's two ends**, and both joined
  `test-native` on 2026-08-19. Before that `fetch` had no target and `publish` was outside the
  sweep, which is how the two halves drifted four ways apart with every suite green (AP21/D22).
- **`make crossimpl-go` is the LIVE cross-impl consume leg** (`scripts/crossimpl-go.sh`) — it
  stands `entity-core-go`'s federation publisher up in its own container on a podman bridge
  (their script, unmodified) and drives our verifying consumer at it from a second container on
  that bridge: manifest → signature → CHAMP trie walk → leaves. **Deliberately outside
  `test-native`** — it needs podman and a buildable sibling checkout, and a sweep target that
  can go red for a neighbour's reasons teaches people to ignore the sweep. First green
  2026-08-20. What a green run claims (and, more importantly, what it does not) is in the
  script header; do not restate it looser anywhere else.
- **`make -C avalonia smoke-xvfb-click` is the real-input gate under the X11 platform**, and
  `make -C avalonia crash-hunt` is its unattended form (sweeps seeds, stops at the first
  crash, prints the replay command). Most drivers in this repo call the model method
  *under* the control — `SiteViewPanel.NavigateForTests`, `HandlerBrowserModel`, the window
  driver — so **none of them can execute input dispatch, hit-testing, focus transfer, or any
  handler that runs before a panel's own code.** That blind spot hid a fatal crash for a
  month across four "negative" repro attempts (STATUS, 2026-08-21). Clicks are seeded and
  every coordinate is logged, so a crashing run replays: `CLICK_SEED=n`.
- **For anything above the platform, the headless suite can drive real input too, and it is
  much cheaper** (added 2026-08-21). `Avalonia.Headless`'s `MouseDown` / `MouseUp` /
  `KeyPressQwerty` run the genuine route — hit test, capture, class **and** instance handlers —
  in ordinary xunit. `ProgramPanelInputTests` is the worked example: it clicks the on-screen
  d-pad at its hit-tested coordinates and asserts on **program state** (the cursor's cell index
  read out of the rendered display list), and it found two shipped defects in its first run.
  Reach for xvfb when the suspect is *below* Avalonia (Boundary D/G); reach for headless first
  otherwise.
- **Never wire a control's pointer input with `+=`** (AP37, P7). Avalonia delivers class
  handlers before instance handlers at the same element, and `Button` marks `PointerPressed`
  **and** `PointerReleased` handled in its own override — so `btn.PointerPressed += …` is
  accepted, never invoked, and warns about nothing. Use `AddHandler(…, Tunnel | Bubble,
  handledEventsToo: true)`. The on-screen game controller shipped in the `+=` form on
  2026-07-27, rendered perfectly, and set no bit for three weeks.
- `make build` — all shipped Go binaries (entity-shell + entity-console).
- **Every build/test target now refuses early if the sibling kernel is missing** (`preflight`,
  added 2026-08-24, AP41). `doctor` had that check from the day it was written and **nothing
  called it**, so cloning this repo on its own produced forty lines of module-resolution spew and
  no cause — measured by someone doing exactly that. The predicate lives once as
  `SIBLING_PRESENT` and is shared with `doctor`. **No suite in this repo can regress
  this**: a suite that runs at all is running in a tree where the sibling resolved, so if you
  touch it, test it the only way that works — `make preflight PARENT=/tmp/no-such-parent`.
- **Entry points, for when you need to actually run the thing:** `make doctor` (prerequisites
  + the sibling kernel — run this first on a strange machine), `make run` (build + REPL;
  `ARGS=` for one-shot), `make gui` / `gui-run` / `gui-build` / `gui-test` (root-level
  passthroughs to `avalonia/`), `make demo` (scripted CLI tour in a throwaway HOME — the
  fastest end-to-end validation that the shipped binary works).
- **`make gui` rebuilds the image; `make gui-run` does not.** Use `gui` after any Go or C#
  change, `gui-run` to launch what is already extracted. Both forward the app's own flags —
  `make gui-run ARGS="--identity me --storage sqlite"` (double-dash: the .NET frontend does
  not use Go's `flag` spelling). **With no flags the GUI is an ephemeral in-memory peer and
  loses everything on exit.** `avalonia/README.md` is the full entry-path doc.
- `make go ARGS="..."` — escape hatch; `ARGS` carries the subcommand (`vet ./...`,
  `mod tidy`, `env`, …).
- **A tolerant fallback that turns malformed input into a well-formed entity is a bug, not
  leniency** (AP33). `put`'s `// Not valid JSON — treat as literal string` wrote an entity
  that decodes nowhere, and the failure surfaced an hour later in a consumer as *the
  consumer's* bug. Be liberal about input that was never trying to be structured; refuse
  input that plainly was. Note also that the shell's `SplitArgs` strips quotes as **shell**
  quoting — JSON payloads must be single-quoted (`put P T '{"a":1}'`), which the usage doc's
  examples show and which is easy to miss.
- **A `.list` artifact is a `system/tree/listing` ENTITY, not text** (`ENTITY-SYSTEM-REFERENCE`
  §168). `entity-browser-rust` emits newline text; `fetch.parseListing` reads both and
  guards on printability, because feeding a CBOR body to a newline splitter yields fragments
  that then "disagree" with the signed key set — a false alarm about the other side's honesty.
- **An async cgo export must copy every C-owned argument into Go memory BEFORE launching the
  goroutine** (AP31). A `*C.char` belongs to the .NET marshaller and is freed when the P/Invoke
  returns; reading it on the goroutine is a use-after-free that **does not crash** — it reads as
  the empty string and surfaces as a plausible user error. The synchronous exports beside it are
  safe for a reason that does not transfer.
- **On a .NET Linux crash, read `si_code` before you believe `si_addr`** (AP34). CoreCLR's
  handler **re-raises** any fault it cannot classify, so the signal that reaches the coredump
  carries `si_code 128` (SI_KERNEL) and `si_addr 0` — and a register context that is the
  handler's, not the fault's. Both 2026-08-21 desktop dumps read as null dereferences on that
  basis and were nothing of the kind. The real fault (`si_code 2`, SEGV_ACCERR, `si_addr =
  rsp-8`, rip on a `call`) is only visible by catching the FIRST SIGSEGV live —
  `make -C avalonia smoke-xvfb-click GDB=1`, which runs under gdb with `nopass`. Corollary:
  **a systemd ELF core cannot give you a managed stack** (the DAC rejects it, `0x80004002`),
  and `dist-native/tools/dotnet-dump` cannot run on the host at all — it is
  framework-dependent beside a self-contained publish. `make -C avalonia crash` now does the
  managed half inside the builder image.
- **The UI thread's alternate signal stack is 1 MB, on purpose, and it is load-bearing.** The
  PAL default is **16 KB**, which is not enough for this process's handler chain under real
  pointer input: the click fuzz crashed **6/8 seeds** at 16 KB and **0/8** at 1 MB, same
  binary, same seeds. Installed at startup by `CrashDiagnostics.EnlargeAltStack`;
  `WB_ALTSTACK_BYTES=0` restores stock, which is the only way to re-measure the bug. Only the
  UI thread is covered — a crash on another managed thread would look identical and is not
  fixed by this.
- **A derived UI property is not a completion signal** (AP32). A headless test that settles on
  "the button re-enabled" returns in the window between the bridge call returning and the
  goroutine entering the operation, and then asserts against an empty view — green, measuring
  nothing. Wait on the bridge handle's monotonic `ops` counter instead.
- **Avalonia builds go through podman, always** — `cd avalonia && make build && make
  extract`, then `make host-run`. The host has no .NET and never needs it; never `dnf
  install dotnet`. Bridge-only smoke check: `cd avalonia/bridge && CGO_ENABLED=1 go build
  -buildmode=c-shared -o /tmp/libbridge-test.so .`.

## Code style

- **Read the source before asserting** path shape / addressing / namespace claims (see
  AGENTS-STANDARD). The whole tree is peer-id-namespaced, so **"peer-id keyed" is almost
  never a valid distinguishing claim** — if you reach for it to explain why something
  matters, you're probably about to mislead. Cite `file:line` in test comments and doc
  explanations.
- The project measures everything against the **24 disciplines (D1–D24)**, ten review
  questions, and anti-pattern catalog (AP1–AP43) in `docs/architecture/DISCIPLINE-CHARTER.md`.
- **A model with no shipped surface is not shipped** (D23). Landing a renderer-neutral model
  is half a feature; the other half is a verb, panel, or menu entry a user can reach, in the
  same session. Three times now — the name arc, the handler browser, `PeerLiveness` — every
  layer was green and no edge connected them, and two of the three were found by audit
  because no test crosses "can a user reach this". **`make reachability`** is the sweep.
- **A "no change needed" claim about another layer or repo is a hypothesis until the
  operation has been run end to end** (D19, AP10). Reading the code path establishes what
  that path does, not what the operation does — the two claims we routed on the strength of
  a correct source reading both missed a layer underneath (a lock released before a send; a
  capability pre-check). Route the measurement, not the argument.
- **Price work against the substrate, not against our own tree** (D20). An absence in
  `entitysdk/` is evidence about `entitysdk/`, not about the system. Before estimating
  anything that names a spec obligation or protocol surface, **grep `../entity-core-go` for
  it by name** — twice on 2026-08-18 the kernel already had what we were about to plan
  (`tree.CollectNodeClosure` for the §6.5.3 closure; `core/peer` + `ext/network` for two of
  the four connectivity pieces). The error always over-estimates, so it never surfaces as a
  surprise — only as work that quietly did not happen. Every "we need to build X" line
  carries the search that established the absence.
- **Logging:** `PanelLog` breadcrumb discipline + category list per
  `docs/architecture/LOGGING-CONVENTIONS.md`; the pre-crash breadcrumb is the forensic surface.
- **Testing:** four tiers per `docs/architecture/TESTING-STRATEGY.md` — naming the tier is
  the discipline.

## Project structure

Architecture is a **dependency graph, not a strict stack** — five layers, where the panel
framework and the application are **siblings** (the panel framework could work without
entities):

1. **Entity Core** — peer, store, protocol (the `../entity-core-go` sibling).
2. **Entity Developer Framework** — Executor, PeerContext, Resolve, Format.
3. **Panel Framework** — panel, focus, actions, content contracts (entity-independent).
4. **Application** — content models, panel declarations, state persistence.
5. **Renderers** — medium-specific ordering + manifestation.

- Go packages: `entitysdk`, `workbench` (renderer-neutral models + business logic),
  `programs` (the entity-native programs track), `shellcmd` / `shell` (entity-shell verb-ops
  + REPL), `shellboot` (shared bootstrap + `PeerManager`/multi-peer lifecycle), `avalonia/`
  (bridge + C# frontend), `console/`. `ext/identity/` is the identity extension.
  Dep direction: `workbench → entitysdk`; `programs → entitysdk`;
  `shellcmd → entitysdk, workbench`; `shellboot → entitysdk, shellcmd, workbench`.
  **`workbench` cannot import `shellcmd`.**
- **`programs/` is a sibling of `workbench`, not a layer of it.** It holds the generic
  compute host, program descriptors, and the Life/Snake/Asteroids/heavyfield programs —
  extracted from `workbench/program_*.go` on 2026-07-22 because a research track does not
  belong inside the app's renderer-neutral model layer. It depends on `entitysdk` **only**;
  it must never import `workbench` (that dependency was zero at extraction — keep it zero).
  `avalonia/bridge` imports it as `pg`. Tests: `make test-programs`.
- **`Host.Input` ENQUEUES; it does not write through** (AP36). A clock-driven program reads its
  input ports at tick time and only then, so a value superseded before the next tick was never
  observed by anything — at Life's 6 Hz that window is **167 ms** and a mouse click is ~25 ms.
  Measured: **1 of 6 d-pad clicks moved the cursor**; 6 of 6 when held past a tick. The contract
  now is *every value offered to a port is observed by exactly one tick, in order* — bounded at
  `inputQueueMax`, coalescing at the tail on overflow, deduping an identical consecutive value,
  and **shape-agnostic** (the host still never decodes a program's bytes). The known limit is
  stated in the doc comment: a bit released and re-pressed between two ticks reads as one
  continuous hold, because the queue carries port *values*, not an event stream.
  **A test that writes the input and then calls `tickOnce()` itself cannot see any of this** —
  that is what every pre-2026-08-21 program test does. The region test is
  `programs/host_input_queue_test.go`, which drives a **running clock** from outside.
- **Compute has no randomness, on purpose — so a program's "randomizer" is a hash, and a
  hash that is linear in its varying input is a TRANSLATION** (AP38). Interactive Life's
  Regen slid one fixed pattern for a month: under a power-of-two modulus, bit *k* of an LCG
  step depends only on bits 0..*k* of its input, so bumping the generation counter is
  arithmetically the same as shifting the cell index. Measured at 0.98–1.00 agreement under
  a cyclic shift, and the operator's report was *"it moves the same map one or two over."*
  **One nonlinear round** (square, then fold the high bits back down — squaring mod 2^k
  leaves the low bits weak) is the minimum. Two things to carry: `!equal(before, after)` is
  the wrong assertion — a translation is never equal, so an anti-vacuity clause passes on
  every one of these boards — and **population/variance is the cheap tell**, since a
  translation preserves the count (σ 0.76 where an independent draw gives 7.75). Gate:
  `TestLifeEdit_RegenIsNotATranslation`. Determinism itself is correct and load-bearing —
  reproducible state hashes are the whole point; the entropy is the tick counter at the
  moment of the press.
- `docs/architecture/` — canonical framework (charter, `MODEL-AVALONIA-RUNTIME.md`,
  `GUIDE-AVALONIA-PANEL-PATTERNS.md` recipes P0–P7, `TESTING-STRATEGY.md`,
  `LOGGING-CONVENTIONS.md`, `DEPLOYMENT-DIRECTION.md`, `SHELL-DIRECTION.md`,
  `CROSS-IMPL-HELPER-REFERENCE.md`, `PERFORMANCE-CHARACTERISTICS.md`). These are **undated /
  living** — edit in place.
  - **`docs/STATUS.md` — the rolling status log, and it lives OUTSIDE `docs/status/` on
    purpose** (moved there 2026-08-24, to match the rest of the ecosystem).
    `docs/status/` is stripped wholesale from the published tree under [ADR-0031], so a
    rolling log left inside it is a canonical document sitting in a directory whose whole
    meaning is "none of this publishes". The path *is* the declaration: out of that
    directory means canonical, in it means working memory. **`docs/STATUS.md` is published
    — write it for the next session, but know a stranger can read it.**
  - `docs/status/` — the ephemeral area: dated `STATUS-YYYY-MM-DD.md` snapshots (immutable
    once published) and `HANDOFF-*`. Never published, no scrub obligation.
  - `docs/architecture/reviews/` — dated cross-team exchanges (`reviews/{TOPIC}-{DATE}.md`);
    closed ones move to `reviews/archive/`.
- **`CANONICAL-DOCS.toml` is a published artifact, not configuration** (AP42). It is the whole
  interface to the release pipeline — we declare, it publishes ([ADR-0031]) — and its `blurb` is the prose a
  public reader gets **instead of** the document. Two rules, both earned on 2026-08-24: **(1) a
  blurb describes, it does not count** — every numeric range in it had gone false (D1–D23 for
  D1–D24, AP1–AP27 for AP1–AP40, "six-boundary" for seven, P0–P6 for P0–P7) plus a `github =`
  org that does not exist, because a `.toml` gets read as config and skipped by review; **(2)
  declaring a doc is part of adding it** — `AGENTS.md` is public and told the reader to open
  `DOCTRINE-CRASH-FORENSICS.md`, which was undeclared, so the instruction shipped broken. If a
  diff changes a count a blurb restates, or adds a doc a published doc cites, the manifest is
  part of that diff. **(3) An omission in a keep-list is an act of DELETION** — undeclared means
  dropped, so a root doc that is already on public `master` and missing from this file gets
  *withdrawn* at the next release. That was live on 2026-08-24 for seven files including
  `SECURITY.md`. Review the manifest against **what is currently published**, not just against
  the tree — the two move independently, so the answer changes without this repo changing.
  Compare a filtered export of `dev` against the published remote (**never a local `master`** —
  it drifts from what is actually published and answers this question wrong). The procedure and
  the local tool paths belong in your git-ignored `AGENTS.local.md` / `.agents/`, not here.

  **(4) Declaring a doc changes what "internal" means about it.** `AGENTS.md` is written for us
  and is *published*, and internal infrastructure names are exactly what an internal-audience
  document is made of — several had to be rewritten out of this file on the day it was declared,
  **and more had to come out on 2026-08-25**, which is the part worth learning from. What
  survived the first pass was everything that read as *engineering*: another team's incident
  history, their process, packet identifiers, who miscounted what. It was all true and all
  useless to the reader it was being shipped to. **A machine-local or internal path belongs in
  the git-ignored `AGENTS.local.md` / `.agents/`** ([ADR-0020]), never here. Assume you
  **cannot** self-check this category in one pass — the second reading is the one that finds it,
  and the test is not "is this true" but "is this the reader's business."

  Note rule (5) below already said this and was written to be applied to `docs/STATUS.md`. It
  binds every declared file, this one included. **A rule stated in a document does not exempt
  that document.**

  **(5) The rolling log is about this project.** `docs/STATUS.md` publishes; it carries our tree
  state, our defects, our decisions. Coordination with other teams — their processes, their
  tooling, their internal state — is **theirs**, and goes in `docs/status/` or a `reviews/`
  packet, neither of which publishes. "It explains why we changed a file" is not an exception;
  say what we changed and why it is right for this repo.
- **Don't synthesize project state from `git log` or top-down code reading** — use the
  framework + latest status snapshot + the newest `docs/status/HANDOFF-*`. **There is no
  roadmap doc and there has never been one** — this file named a
  `REPOSITORY-WORKSPACE-ROADMAP.md` and `PHASE-*-PLAN.md` for months and neither exists, which
  sent every new session looking for a file to orient from. "What's next" lives in the newest
  handoff's recommended-order section and in `STATUS.md`'s "Waiting on"; one direction doc per
  topic covers the rest. Corrected 2026-08-20, by audit.

## Boundaries — do NOT modify

- **You work on `dev`. You do not touch `master`, ever.** `master` is the **public canonical
  mirror** and is republished at each release — it is not a branch this repo's working sessions
  advance, propose advancing, or reason about. Promotion `dev → master` is the **release act**,
  performed by maintainers; [ADR-0015] and its amendment are the authority, [ADR-0022] covers the
  pipeline. **`dev` being ahead of `master` is the normal, expected steady state** — not a pending
  decision, not a status-file row, simply unreleased work. This file used to carry *"the one
  decision left: `dev` is N commits ahead of `master`"* as an open item across sessions, which
  presented someone else's act as our decision and invited a future session to act on it.
  **A session that finds itself weighing a merge to `master` has already gone wrong; there is
  nothing to weigh.**
- **`../entity-core-go/` is a sibling dependency, not part of this repo.** Read it for
  protocol/store behavior; never edit it from here (route cross-impl changes via `reviews/`
  per AGENTS-STANDARD).
- **Status snapshots (`docs/status/STATUS-YYYY-MM-DD.md`) are immutable once published** — never
  re-open a closed snapshot to add work; write a new dated one.
- **Avalonia is podman-only** — don't touch the host package set for the .NET toolchain.

## Repo-specific gotchas

- **"identity" vs "keypair" — keep these distinct** (the word is overloaded across the
  codebase; full discussion `DEPLOYMENT-DIRECTION.md §3`):
  - **keypair** — the bare Ed25519 keypair (what `crypto.LoadIdentity/SaveIdentity`
    load/save — upstream misnomer; don't propagate it).
  - **identity bundle** — the on-disk directory shape
    (`entitysdk/identity_bundle.go::IdentityBundle`).
  - **identity entity** — the V7 hash-addressed public-key entity (`peer.Identity()`).
  - **identity extension** — the attestation + quorum + identity stack (`ext/identity/`).
- **workbench is the brain; renderers are thin I/O.** All business logic (entity
  resolution, CBOR/markdown formatting, handler discovery, tree/selection state, the
  content models — `tree_model`, `detail_model`, `shell_model`, `peer_info_model`,
  `log_model`, `handler_model`, …) lives renderer-neutral in `workbench/`; `Render()`
  returns a plain struct that any renderer drives. **Never reimplement model logic in C#
  or tview.** Treat `workbench` as the Go "standard library" for entity apps —
  protocol-first (execute / tree get-put), never direct store/index access from app code.
- **Multiple renderers are a discipline enforcer, not a parity obligation.** Console is
  kept (frozen, single-peer) purely to keep the renderer-neutral core honest; Avalonia
  drives all feature work and may outpace it. If a model-layer change breaks console,
  that's a signal the abstraction was wrong — fix the model, don't gate Avalonia on console
  parity. (The canvas/raylib renderer has been removed.)
- **DRY the integration, not the renderer.** Shared shell↔workspace wiring lives once in
  `shellcmd/integration.go` (e.g. `PersistAliases`, `PublishWDTo`); renderers add one line
  of wiring. Same closure in two renderers = extract it.
- **`shell.WD` is stored canonical `/{peerID}/...`, never `/@alias/...`.** The alias form is
  display-only (`shellpanel.Prompt()` applies `AliasFor` at render). Store-side surfaces
  (`Store.List`, `NamespacedIndex.canonicalize`) **panic** on a leading `@alias`. When
  seeding WD in a renderer/test, use `shellcmd.Path("/" + peerID + "/")`.
- **Path syntax migration owed:** `alias:path` ships today, but `@alias` is the pinned
  peer-id substitution sigil (`:` is reserved for `<handler-path>:<op>`), so prefer `@alias`
  in new user-facing docs/examples to avoid re-churn when the migration lands.
- **SDK shape:** `entitysdk.AppPeer` **is** a peer (always has a tree + full handler set +
  dispatcher + pool); `entitysdk.Client` is **not** (bare TCP wrapper, deferred). The
  keypair you operate under picks the surface; never open a fresh client connection to a
  peer your AppPeer already pooled under the same identity. Don't add `PeerSurface` /
  `*From` variants — URIs (`entity://{peer-id}/...`) encode the target.
- **Build the typed SDK clients against `core/types`** via `entitysdk/extdispatch.go`'s
  `extDispatch` — do **not** consume core-go's `ext/{identity,role}/sdk` proto-SDK (Go-only,
  no SDK error mapping; would puncture `*entitysdk.Error` predicates). **Layer-2 algorithm
  contract:** any operation whose bytes land in the tree (chunking params, slug/path
  canonicalization, CBOR canonical encoding, subscription pattern matching, …) must be
  byte-identical across impls — extract named constants + reference vectors. Layer-1
  ergonomics may vary freely.
- **A dispatched read of a peer-qualified path is a REMOTE read** (AP11). `AppPeer.Get` /
  `List` / `Has` route by peer-id: `List("/{them}/…")` dispatches to *that peer* and
  returns *their* tree, not our cached mirror of it. To assert on a mirror — or on anything
  we hold in another peer's namespace — read `AppPeer.Store()` (L0) instead. A test that
  gets this wrong is green whether or not the mirror was ever written.
- **Mirroring another peer's subtree needs a capability that names their namespace.** The
  owner self-cap's `Resources: ["*"]` is peer-**local** under §PR-8, so `tree:merge` 403s on
  every `/{them}/…` target. Use `AppPeer.MintMirrorCapability` +
  `Executor.executeAs`; the destination itself comes from the publisher's signed
  published-root via `AppPeer.MirrorDestination`, never from a caller-chosen string.
- **Two directions of transport profile — keep them straight.** `AppPeer.Connect` registers a
  profile for the peer we **dialed** (the address-book direction, core-go's `RegisterRemote*`).
  `AppPeer.AdvertiseTransport` publishes one for **us**, under our own peer-id — §6.5.1a D1
  self-publication, wired into `shellboot`'s listener bind. Path segment is the peer
  **identity-hash hex**, not the Base58 id (`core/types/crypto.go` pins hex for non-root path
  positions); the Base58 form is the profile's `peer_id` field. **Never advertise a wildcard or
  port-0 address** — a durable profile nobody can dial is worse than none, and the SDK refuses it.
- **Liveness comes from the tree, not the pool.** `AppPeer.ConnectedPeers()` is a connection-pool
  snapshot; the lifecycle signal is `system/peer/status`, read via `Store.PeerLivenessOf` /
  `PeerLivenessAll` / `OnPeerLivenessChange` and rendered through `workbench.PeerLivenessModel`.
  The pool cannot express `suspect`, cannot say *why*, and diverges whenever a connection is
  evicted without a demotion. **The status entity is transition-written, not a heartbeat** —
  `LastSeen` is a snapshot taken at the transition, so ageing rows off it invents a contract the
  protocol does not offer (§5.4.1). Absence means "no transition ever recorded", never
  "disconnected". `system/network`'s §2.7 `maintained_peers` is **not** the maintained set (it
  enumerates every status entity); `session_id` is the discriminator, and
  `NetworkClient.MaintainedPeers()` is the filter.
- **Subscribe by prefix; never scan-and-filter.** Consumer refresh uses `Store.Watch`/
  `OnSelectionChange` (or `OnPrefixChange`) — listing the whole tree and filtering in-proc
  is a structural anti-pattern (O(store size) per render). Don't build a workspace-level
  polling dispatcher over the SDK; the SDK is the dispatcher.
- **App handler integration:** the app registers a handler at `workspace/app`; targeted
  refresh flows subscription engine → inbox → app handler → Go channel → UI (not
  "refresh-all on every tree event").
- **Perf measurement:** `modernc.org/sqlite` under `-race` is ~17× slower — perf benches
  must override the default flags (`GOTEST_FLAGS="-count=1"`); never trust SQL bench numbers
  taken with `-race`. To reconcile store/index count discrepancies, `SqliteStore.DB()` lets
  you `SELECT` the `entities` table directly.
- **Spec discipline:** workbench-application logic (workbench-owned handlers /
  `app/workbench/`, `archives/` namespaces) extends freely; **spec-adjacent** behavior
  (anything `system/*` or a documented domain handler) needs a spec read + cross-impl
  coordination first. To check whether an op is spec'd, read the `EXTENSION-*.md` section
  outline + manifest YAML (`pull: {input_type: ...}`) — grep with impl-style patterns misses
  unquoted YAML declarations.
- **A `RULED` proposal is buildable. Build it.**
  `AGENTS-STANDARD`'s *"implement against the landed spec, not in-flight proposals"* targets
  **unruled** proposals — a shape nobody has decided yet. A proposal stamped `RULED` in its
  header **is** a decision, and implementing ahead of the editorial fold is normal practice
  here: it is one of the ways a spec gets validated before it hardens, and what the build
  surfaces goes back to the authoring repo as feedback.
  **Do not treat "the fold has not landed" as a blocker** — that reading cost us a
  self-inflicted stop on `PROPOSAL-DISCOVERY-RENDEZVOUS-BACKEND`, whose whole normative
  content (one enum value + a composition subsection) was already determined.
  What the implementer owes instead: **name the source in the commit** — "built against
  `PROPOSAL-X` at arch `<sha>`, not against landed `EXTENSION-Y`" — so the coupling is
  greppable when the fold lands and the diff is re-derivable if a token's spelling moves.
  *(Not the same as AP20: that one is about inventing a constant whose referent exists in no
  document. A ruling is a referent.)*
- **`publish/` (the CDN corridor) emits a real signed root as of 2026-08-18.** `{out}/manifest`
  is the signed `system/peer/published-root`; the http-poll transport profile moved to
  `{out}/transport-profile` (§6.5.4 / D5); the §6.5.3 closure of `root_hash` is uploaded in full.
  Three rules that came out of building it:
  - **A signed root and a filtered publish are incompatible** — `Opts.IncludePath`/`IncludeType`
    now **refuse**. The closure obligation would upload the filtered-out entities' bytes anyway
    (a leak the operator did not ask for), and withholding them instead shortens a consumer's
    walk silently. Narrowing the published set is `-prefix`'s job.
  - **`seq`/`predecessor` come from the store, not from the engine.** `ext/publishedroot.Publisher`
    keeps both in process memory and never seeds them, so a batch publisher restarts at `seq=1`
    forever (measured). Ours reads the prior root; the ask is routed to core-go.
  - **A workbench-published site is "verified as of `published_at`", never "verified"** — a quiet
    publisher and a withholding origin are indistinguishable at the consumer (§6.5.3.1, D6/D7).
  Result packet: `docs/architecture/reviews/archive/PUBLISHER-CONFORMANCE-RESULT-2026-08-18.md`.
- **The consume side is a JOURNEY now, not an inspector** (2026-08-21). `fetch.Registry` +
  `workbench.BrowseModel` do `name → binding → transports → the target's signed root → walk →
  page`, and the three surfaces are `entity-shell`'s `registry` / `browse` / `open`, the Avalonia
  **Browser** panel, and `entity-fetch -registry`. Four rules from building it:
  - **Do not re-implement §6a.4.** `entity-core-go`'s `ext/registry/peerissued` has the whole
    algorithm *and* an `HTTPPollReader`. `entitysdk.AppPeer.PinRegistry` registers **their**
    backend; `fetch.Registry` exists only because their `Resolve` needs a store + location index
    and `entity-fetch` links neither. The split is held by
    `workbench/registry_differential_test.go`, which runs both over the same frozen bytes — **if
    that test goes, `fetch`'s resolver goes with it.**
  - **`transports` on a §3 binding has two live readings and they do not interoperate.** core-rust
    emits an inline endpoint object, core-go's `BindingData` says `[]hash.Hash`, and core-go's
    backend therefore **cannot decode any binding in the cohort's only live federation**. Ours
    reads both and keeps them distinguishable (`TransportRef.Kind`) rather than normalizing.
    Routed: `reviews/REGISTRY-BINDING-TRANSPORTS-DIVERGENCE-2026-08-21.md`. **Do not "fix" this by
    making our SDK succeed where the reference implementation fails** — that hides it in our tree.
  - **The walk is the authority; a served listing is a menu** (§6a.3a). `Registry.Enumerate` walks
    the signed root over the `by-name/` prefix and reports both sets *and their disagreement*.
    The spec authors' standing ask: say which one produced a row **in the artifact**, not only
    in the code.
  - **An origin-relative transport prefix resolves against a scheme://host:port, never against the
    path the profile was fetched under** (`fetch.OriginRoot`). A registry served at `host/registry`
    names domains at `host/docs`.
- **`fetch/` enters through the publisher's advertised layout and derives nothing** (2026-08-19).
  `fetch.Layout` is built from the http-poll transport profile at `{origin}/transport-profile` —
  peer-id, all three URL prefixes, content layout, both suffixes. The one convention left is that
  well-known object. **Never re-derive a URL here**: the pre-2026-08-19 version derived all three
  and could not fetch a byte from our own publisher (AP21). Two live joins exist in this cohort and
  we consume both — if the `tree_url_prefix`'s **last segment is exactly the peer-id** it is
  peer-rooted (browser-rust), otherwise the peer-id is ours to append (ours); arch owns the ruling
  that will kill one branch. Content URLs use the **33-byte wire hex** (66 chars, `00`-prefixed),
  which §6.5.3.1 MUSTs. core-go's `types.BuildContentURL` **used to** hex the digest only; as of
  their `7f39eb3` it hexes `h.Bytes()` and agrees with us — but we still build our own, because
  noticing is not adopting: `fetch/crossimpl_test.go::TestContentURLUsesWireHexNotDigestHex` is
  the tripwire that logs the agreement, and the delegation owes a round-trip measurement against
  **both** live joins before `fetch.contentURL` becomes a call. Cross-impl gate:
  `fetch/testdata/crossimpl-rust-site/` (their bytes, frozen, provenance in its README).
- **A persistent store does not make the query index persistent** (AP39). core-go ships only
  in-memory query indexes, fed solely by the `"query"` sync hook — i.e. by *this process's*
  writes. `entitysdk`'s `assembleAppPeer` now calls `query.IndexMaintainer.Rebuild` at startup,
  which is the kernel's own answer (*"use for recovery or startup with persisted stores"*) and
  which we simply never called. Before it, `-storage sqlite` gave you a tree that survived a
  restart and an index that did not, so **`find` / `grep` / `compute aggregate` were blind to
  everything written before the process started** while `ls` listed it happily. If you add
  another derived index, the question to answer at the same change is *what rebuilds it on
  open* — and the gate has to cross a process boundary
  (`entitysdk/query_index_restart_test.go`), because in-process it is green either way.
  **This is a correctness fix, not the shape fix** — the rebuild is O(store), measured at
  ~8.7 µs/entity (`query_index_rebuild_bench_test.go`), so it is a startup tax that grows with
  the store forever. The shape fix is backlog row **PR-1**: a SQLite-backed query index in the
  store's own database, which `SqliteStore.DB()` exists to allow ("*so co-located extensions can
  create their own tables in the same database*"). Do not let "it's fixed" close PR-1.
- **A persistence test that reads back through one path certifies that path, not persistence**
  (AP39, and the reason it shipped). `TestStorage_Sqlite_LargeCorpusSurvivesRestart` is a strong
  restart test — 500 entities, hash byte-equality, revision log, `List` cardinality — and it
  reads the reopened peer **only** through `Get`/`List`, which the persistent location index
  serves. The volatile query path had no restart coverage at all. When a store serves more than
  one read path, enumerate them and restart-cover **each**: in-process they answer identically,
  and they diverge only on reopen.
- **`put` stores JSON numbers as floats, so any reader of a numeric field needs the float case**
  (AP40). `json.Unmarshal` into an `interface{}` makes every JSON number a `float64` and CBOR
  core-deterministic encoding keeps it one. `compute aggregate` accepted only integer kinds and
  therefore skipped **every** entity created the documented way, reporting "3 entities scanned,
  3 skipped" while its own unit tests — which build Go ints directly — stayed green. Refuse a
  non-integral value rather than truncating it (AP33). When a fixture has to prove a cross-verb
  type contract, encode it through the *writing* verb's path, not by hand.
- **A compute error is a VALUE, and what happens to it depends on the POSITION, not on how it
  arrived** (`entitysdk/axis1/contain.go`, AP43). Two representations exist at every result site —
  *minted* (the evaluator raised it) and *value-form* (a `compute/error` entity arrived as an
  ordinary result), and §2.4 forbids them taking different paths. Three positions, and each call
  site in `axis1/eval.go` names its own: **CONSUMED** — the result is READ (arith/compare/logic
  operand, `if` condition, cast value, field target, construct field, index and its array, any
  collection operand, a filter predicate) → both short-circuit, via `evaluator.operand`;
  **CONTAINED** — the result is PLACED without being read (`map`'s output element, `fold`'s
  accumulator and `initial`) → both become a value in that slot, except `budget_exhausted` /
  `cascade_limit`, whose counters are not restored on unwind (`depth` *is*, so `depth_exceeded`
  contains like anything else); **BOUNDARY** — a contained error element materializes
  **code-only**, because `message` is prose no spec pins and containing it forks the array's bytes
  cross-impl. Getting the split wrong is silent under any test whose closures cannot fail — the
  filter half of the defect **kept** elements whose predicate had failed rather than erroring.
  Gates: `TestAxis1Equivalence_ContainedErrorPositions` (one vector per position) and, the real
  one, **arch's differential corpus below.**
- **`TestAxis1Admission_*` SKIPS unless you set `AXIS1_ADMISSION_CORPUS`, so a green
  `make test-sdk` says NOTHING about AE-5** — and a skip counts as a failure (AGENTS-STANDARD).
  This is the gate that matters and it is not wired into any target. Run it:

  ```
  (cd ../entity-core-go && go run ./cmd/internal/compute-corpus generate --profile inproc --out /tmp/c.cbor \
     && go run ./cmd/internal/compute-corpus emit --corpus /tmp/c.cbor --out /tmp/ref.cbor)
  (cd entitysdk && AXIS1_ADMISSION_CORPUS=/tmp/c.cbor AXIS1_ADMISSION_OUT=/tmp/a1.cbor go test -run TestAxis1Admission .)
  (cd ../entity-core-go && go run ./cmd/internal/compute-corpus cross-bless --corpus /tmp/c.cbor \
     --emission /tmp/ref.cbor --emission /tmp/a1.cbor)
  ```

  **Measured 2026-08-25 (corpus `8d2f55c8`, 362 vectors, core-go `13a42ea`): 334 agree, 0 diverge,
  28 INCOMPLETE — NOT LOCKED.** Every vector Axis-1 *answers* is byte-identical to the reference;
  the 28 deopt to Stage-1, and under **AE-6 a deopted vector's evidence is void**, so **AE-5 is not
  green and has not been since the v3.24/v3.25 primitives landed.** Do not quote the 2026-07-23
  admission as current. The corpus names the divergence-prone classes in its vector IDs
  (`cv8a-map-contains-minted-error`, `cv8c-filter-predicate-error-shortcircuit`,
  `cv9a-map-depth-exceeded-contains`, `worked/value-error/*`) — **it catches the AP43 bug in five
  vectors**, measured by re-running it against the pre-fix engine. Our home-grown 300-case fuzz
  found three cases and no cause; this corpus names them. **Reach for it first.**
- **Not the conformance team.** When a cross-impl wire bug surfaces during perf/feature
  work, capture `file:line` + reproducer and route it (Python encoder → Python team, spec
  ambiguity → arch, conformance test-gap → core-go) — don't extend the probe into a
  validation harness. That's core-go's `validate-peer`.
