# `crossimpl-rust-site` — a frozen `entity-browser-rust` emission

**Another implementation's bytes. Do not regenerate, edit, or reformat them.**

This is the static site `entity-browser-rust`'s `make site` emitted, copied
verbatim on **2026-08-19** from their `dist/` at **`dev` @ `fbc2c5c`**
("feat(publish): close both halves of the discovered front door"), working tree
clean. Copied unchanged except for dropping the wasm/js/html bundle, which is
their browser and not part of the published-tree surface: what is here is
`transport-profile`, the peer subtree `2KEE55…/`, and the content blob store
`content/`.

It exists so `TestConsumeBrowserRustSite` can prove our consumer resolves a
**Rust** publisher's emission rather than our own. A signed root verified only
by its own language's reader is cohort-consistent, not independent convergence
(ADR-0012) — this fixture is what makes the difference measurable in CI instead
of once, by hand, on a day two repos happened to be checked out side by side.
It is the mirror of the fixture they hold of ours
(`entity-browser-rust/tests/fixtures/crossimpl-go-site/`, emitted by our
`publish/cmd/crossimpl-fixture`).

What the emission commits to, and what our consumer therefore has to read
rather than assume:

| Field | Their value | Ours, for contrast |
|---|---|---|
| `tree_url_prefix` | `/2KEE55…` — **peer-rooted**, origin-relative | the bare origin — **origin-rooted** |
| `content_url_prefix` | `/content` | `{origin}/content` |
| `manifest_url_prefix` | `/2KEE55…/system/peer/published-root` | `{origin}/manifest` |
| `content_layout` | `sharded-2-4` | `sharded-2-4` |
| `tree_leaf_suffix` / `tree_listing_suffix` | `.bin` / `.list` | `.bin` / `.list` |

The first three differ, which is the point: a consumer that derives any of them
works against exactly one publisher.

**Re-cutting.** If it needs refreshing, take a fresh emission from their tree at
a named commit and update the commit and date above — never hand-edit a byte.
The content blobs are content-addressed, so an edited body fails its own hash
check and the fixture would fail as a fixture, loudly, which is the correct
outcome but a slow way to learn it.
