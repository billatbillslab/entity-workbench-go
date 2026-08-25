# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.9.0]

The first release since `v0.8.0`. The theme is **reach**: several arcs that were
complete at the model layer but had no surface a person could touch now have one,
and the CDN corridor gained its second end.

### Added

- **The consume leg of the CDN corridor — a journey, not an inspector.** A name now
  resolves the whole way: registry → binding → transports → the target's signed
  published root → a verified walk → a page. Three surfaces drive it — the
  `registry` / `browse` / `open` verbs in `entity-shell`, the **Browser** panel in the
  desktop app, and `entity-fetch -registry`.
- **A publisher that emits a real signed root.** `entity-publish` writes a signed
  `system/peer/published-root` and uploads the full closure of `root_hash`; the
  http-poll transport profile it advertises is what a consumer enters through, so no
  URL is derived by convention on either side.
- **Peer liveness read from the tree.** `peer status` in the shell and a liveness
  surface in the desktop app, both backed by the `system/peer/status` entity rather
  than by a connection-pool snapshot — so `suspect` is expressible and a transition
  carries its reason.
- **Transport self-publication.** A listening peer advertises its own transport
  profile under its own peer-id, wired into the shell's listener bind.
- **The handler browser and the name arc**, each reachable from a menu or a verb
  rather than existing only as a model.
- **`programs/`** — a module of its own (extracted from `workbench/`) holding the
  generic compute host, the program descriptors, and the Life / Snake / Asteroids /
  heavyfield programs, with a standard controller input model and an interactive Life.
- **A front door.** `make doctor` (is this machine set up?), `make run`, `make gui`,
  `make gui-run`, `make demo`, and `make test-each` — the last runs every Go suite to
  completion and prints a pass/fail table instead of stopping at the first failure.
- **`make reachability`** — a sweep that fails when a model has no user-reachable
  surface, and **`make crossimpl-go`**, which stands up the Go reference
  implementation's federation publisher in a container and drives our verifying
  consumer at it over a real network.

### Changed

- The desktop app defaults to **software rendering**; the GPU path is opt-in.
- The three legacy per-program panels were retired in favour of the generic host.
- `make build` / `test` / `lint` / `gui` now run a **preflight** that names the
  missing sibling checkout in one sentence instead of failing forty lines deep in
  module resolution.

### Fixed

- **A crash that had gone unreproduced for a month** in the desktop app under real
  pointer input: exhaustion of the UI thread's alternate signal stack, whose platform
  default is 16 KB. Measured at 6 of 8 seeds crashing before, 0 of 8 after.
- **Interactive Life's on-screen controller was inert from the day it shipped**,
  through two independent defects — the host sampled its input ports only at tick
  time, so a click shorter than a tick was never observed (1 of 6 presses landed), and
  the toolkit was silently discarding the panel's pointer handlers. Every value
  offered to a port is now observed by exactly one tick, in order.
- **Life's "Regen" was sliding one fixed pattern rather than generating a new one** —
  a linear hash under a power-of-two modulus makes bumping the generation counter
  arithmetically equivalent to shifting the cell index.
- **The query index did not survive a restart.** With a persistent store the tree
  came back and the index did not, so `find`, `grep` and `compute aggregate` were
  blind to everything written before the process started while `ls` listed it
  happily. The index is now rebuilt when the peer opens.
- **`compute aggregate` could not read a numeric field written by the shell's own
  `put`** — JSON numbers decode as floats and the aggregate accepted only integer
  kinds. Non-integral values are now refused rather than truncated.
- A peer held ~20 MB for a delivery ring it never released.
- A persistent store now yields a persistent peer identity across restarts.
- The watch hub could send on a closed channel.

### Known limitations

Stated because they are real and reproducible, not because they are comfortable:

- **A sibling `entity-core-go` checkout is required to build.** Every module resolves
  the kernel through a local `replace`, so this repo must be cloned beside it —
  README § *Repository layout* has the shape, `make doctor` verifies it, and the build
  now refuses early with instructions. There is no published module path yet, so
  `go get` of the SDK is not available in this release.
- **One differential test fails**: a compute equivalence case whose contained-error
  semantics the Axis-1 engine has not yet adopted. It is a known, characterized
  divergence, not a regression.
- **A rare failure under full-suite load** in a bidirectional burst-write end-to-end
  test, traced to a terminal write loss below this layer and routed upstream; and one
  unidentified desktop-test flake observed once in eight runs.
- **117 short-SHA citations across the published documents do not resolve for a reader
  of the public history**, which is authored fresh at the release boundary. They are
  provenance notes on internal commits — not links you can follow — and replacing them
  with content-addressed citations is in progress. **72 of them are in
  `docs/STATUS.md`**, the rolling engineering log, which is published deliberately: it
  is written for the next working session first, and it cites the commits that session
  would look up. The remainder sit in the framework documents, where each pins the
  defect that earned a rule.

---

## [0.8.0]

- Initial public research-preview release.
