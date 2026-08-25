# `crossimpl-rust-federation` — a frozen `entity-browser-rust` **naming** emission

**Another implementation's bytes. Do not regenerate, edit, or reformat them.**

Where `crossimpl-rust-site/` freezes one published *domain*, this freezes a whole
**federation**: a static registry that signs four `name → peer-id` bindings, and
the four static domains those names point at. It is the fixture for the hop the
site fixture cannot reach — **hop 1, the name** — and for the journey that joins
the two hops into something a user does.

Copied verbatim on **2026-08-21** from `entity-browser-rust`'s working tree,
which reported `dev` @ **`e26651c`** at the time of the copy. Their
`dist-federation/` is a **build artifact and is gitignored there**
(`.gitignore:7`), emitted by `make federation` → `tools/local-federation.sh`;
**there is therefore no commit anywhere that contains these bytes**, and the
revision above names the tree they were cut beside rather than one that holds
them. Their working tree was not inspected for cleanliness and no claim is made
about it. If that distinction ever matters — it does for re-cutting — ask them
to tag an emission.

Copied unchanged except for dropping the `*.wasm` / `*.js` / `*.html` bundle,
which is their browser plus the legacy-web projection of the same pages, not
part of the published-tree surface. What is here is `transport-profile`, each
peer subtree, and each `content/` blob store.

## What it is

```
registry   2KBLkCxvkgobuauPA6zPfKarpuRRnnWHL98n8Gv1GNmybr   ← THE PIN
  system/registry/binding/by-name/{name}      → the binding's system/hash
  system/registry/binding/{binding_hash}      → the body pointer
  system/signature/{binding_hash}             → the registry's signature
  system/peer/published-root  + its signature ← a signed root over ALL of that

entitychurch.org           → foundation  2KGTrr4LxfFJjUBUD74XJZoze74TrQFpXqWA19qeskz9sS
protocol.entitychurch.org  → protocol    2KAdu6wwTNAoQiqXmN93vbjHxk3QosG7hxhXGZtN8wZF31
docs.entitychurch.org      → docs        2KFRBJ9feEPCZZiNaCEKsCAVkGkp1htWZk9a8jz5n5D2sS
lab.entitychurch.org       → lab         2KEb7HgmoCRF1VpNeCYusiubnn94ke4uK1hUxBcQ1PTtNA
```

Each domain is an independent publisher with its **own** key and its **own**
signed root, carrying SITE-convention content at `sites/{site}/manifest` +
`sites/{site}/pages/**` (`APP-CONVENTION-SEMANTIC-CONTENT-SITE` v0.5 — the same
paths `workbench/site_paths.go` already parses).

**A consumer starts holding exactly one string: the registry's peer-id.**
Everything else is reached and everything else is checked.

## The two layout shapes are both here, on purpose

| | registry | the four domains |
|---|---|---|
| `transport-profile` | **absent** | present |
| how a consumer gets a layout | **pinned** (`fetch.PinnedLayout`) | **discovered** (`fetch.LoadLayout`) |

That asymmetry is not an oversight in their emitter, it is `EXTENSION-NETWORK`
§6.5.4 — profile distribution is out-of-band in v1 — and it is arch's **R-28**.
It means this one fixture exercises both of our layout modes in a single run,
and it is why every surface we ship prints *which mode it ran in*: a wrong pin
and a withholding origin are byte-identical at the consumer.

Both live `prefix` shapes are here too. The registry and the domains publish the
**peer-qualified** form (`/{peer_id}/`); our own `publish/` emits the
**peer-relative** form (`docs/`). See `fetch.AbsolutePrefix` (AP30) — the join
rule is a three-row table and getting it wrong reports as *the other side's*
defect.

## What it proves that nothing else we hold does

`crossimpl-rust-site` proves a Go reader can verify a Rust **publisher**. This
proves a Go reader can verify a Rust **registry** — a different key, a different
entity type, a different §6a.4 check set — and then follow what that registry
said to a *second* Rust publisher and read a page out of it. A signed root
verified only by its own language's reader is cohort-consistent, not independent
convergence (ADR-0012); a *name* resolved only by its own language's resolver is
the same claim one layer up, and until this fixture existed nobody in the cohort
had measured it.

The negative controls that make the positives worth anything live in
`fetch/registry_test.go` — substitution (repointing a by-name key at another
legitimately-signed binding), a withheld interior node, and the listing/walk
disagreement. All three are constructed **from these bytes at read time**; none
of them edits a file here.

**Re-cutting.** Take a fresh `make federation` emission from their tree at a
named revision and update the date and revision above — never hand-edit a byte.
Bodies are content-addressed, so an edited one fails its own hash check: the
fixture would fail loudly, which is correct but a slow way to learn it.
