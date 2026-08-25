// Package axis1 is an experiment-tier alternate execution engine for
// entity-compute: it decodes an expression graph ONCE into a resolved
// in-memory node tree and evaluates that tree every tick, instead of
// re-decoding every node from CBOR and reloading captured scope per element.
//
// This is rung 2 / "Axis 1" of the compile ladder — one faster general engine
// per runtime that runs any IR, NOT a program-specific compiler. The IR remains
// the only transferable artifact; this engine is peer-local and oracle-validated.
//
// Design source (do not re-derive — read these first):
//   - entity-system-architecture: EXPLORATION-COMPUTE-PROGRAM-RUNTIME-CONTRACT.md
//     §13 (the concrete design), §12 (the ladder and why Axis-1 first).
//   - HANDOFF-2026-07-16-compute-axis1-interpreter-prototype.md (the ask).
//   - docs/architecture/reviews/COMPUTE-PROGRAM-POC-FINDINGS-2026-07-15.md §2
//     (the profile that motivates it), COMPUTE-AXIS1-ORACLE-GAPS-2026-07-16.md
//     (why the oracle here is differential rather than vector-based).
//
// # The contract
//
//	Same IR in; bit-identical materialized boundary entities out;
//	the interior representation is free.
//
// # The two costs this deletes (POC §2, measured)
//
//	~54% per-node cbor.Unmarshal  → decode-once resolved node graph
//	LoadScope (67% in the F-D2 case) → live frames, never round-tripped
//	                                   through CaptureScope/LoadScope
//
// # Reference
//
// The Stage-1 reference implementation is entity-core-go ext/compute, read at
// commit 769a888. Every semantic rule here is re-derived from that source
// because ext/compute exports none of it (only Evaluate, Scope, CaptureScope,
// LoadScope, Budget, IsComputeExpression). The re-derivation is deliberate:
// sharing code with the reference would make the equivalence oracle circular.
// Independence is what makes cross-engine agreement mean anything.
//
// Each re-derived rule cites its reference symbol + file. When Stage-1 changes,
// those citations are the diff list.
//
// # What must NOT drift (exploration §13.5 — not this package's call)
//
//   - Materialized boundary entities are canonical-CBOR bit-identical and
//     same-content-hash as Stage-1. This is the only equivalence checked and
//     the whole conformance surface.
//   - The impure frontier: lookup/tree is impure; lookup/scope and lookup/hash
//     are pure.
//   - Evaluation semantics: error-as-value propagation, lazy `if` vs eager
//     `let`, canonical map-key ordering, integer/fixed-point determinism
//     (introduce no float), and the S1 builder's let-name-sort visibility.
//   - Budget accounting counts the same logical ops as Stage-1, so any movement
//     of the 100k-op cliff is explained by fewer materialized artifacts rather
//     than by silent metering drift.
//
// # Interior artifacts are NOT part of that contract
//
// A Stage-1 run leaves compute/scope entities in the content store that an
// Axis-1 run does not — the CaptureScope/LoadScope round-trip is an
// interpretation artifact (exploration §9.1 applied to scope), not a boundary
// entity. Comparing content-store *contents* across engines is therefore the
// wrong test and will fail for the right reason; compare materialized boundary
// hashes.
//
// # Scope
//
// In: the 16 pure expression node types. Out: compute/apply in dispatch mode
// (capability checks, handler dispatch, builtins/store), which falls back to
// the Stage-1 evaluator at a seam where Stage-1 materializes anyway. Also out,
// per the handoff §6 fence: JIT/native codegen, a portable bytecode, per-program
// compilation (Axis 2), and any wire/IR/spec change.
package axis1
