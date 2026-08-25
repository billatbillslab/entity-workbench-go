package entitysdk

import (
	"fmt"
	"strings"

	"go.entitychurch.org/entity-core-go/core/types"
)

// The resolver-config surface — EXTENSION-REGISTRY §4, and specifically
// §4.1a's recommended default dispatch list, folded by arch on
// 2026-08-18 (PROPOSAL-DEFAULT-NAME-FORMAT-DISPATCH).
//
// Why this is app-tier work and not the kernel's: core-go's registry
// handler READS `system/registry/resolver-config` and applies
// `name_format_dispatch` faithfully, but nothing in the cohort WRITES a
// default one — an exhaustive search of `entity-core-go` finds the type,
// the storage path, the reader, and construction only inside the
// `validate` harness. A peer with no config resolves every name through
// the whole chain, which is harmless while the chain is local-only and
// is a disclosure the moment it is not.
//
// The load-bearing rule here is one line of §4.1 step 2, and it is a
// MUST: **the catch-all routes to local-only backends, never to a remote
// registry.** Step 2 calls itself the primary privacy mechanism; a
// catch-all bound to a remote backend discloses every unscoped name a
// user types — the whole private namespace, one name per keystroke, to
// a third party. InstallResolverConfig refuses such a config rather
// than writing it (§11.1 vector REG-DISPATCH-CATCHALL-LOCAL-1: "a
// resolver-config whose catch-all names a remote backend MUST be
// refused or normalized at load").

// CatchAllPattern is the §4.1a catch-all — the entry every name that is
// not explicitly scoped falls to.
const CatchAllPattern = "*"

// BackendKindPinned is the pinned-bindings pseudo-backend named by
// §4.1a's catch-all row.
//
// It has no constant in `entity-core-go/core/types` (which enumerates
// local-name, dns-txt, well-known-url, did-web, peer-issued,
// out-of-band, consensus-anchored, self-certifying) because pinned
// bindings are resolved at §4.1 step 1, before dispatch runs at all —
// so nothing ever registers a backend under this kind. Named here
// because the interoperable default list names it, and shipping the
// list means shipping it verbatim. Routed to arch as an observation.
const BackendKindPinned = "pinned"

// backendKindDIDKey is the kind §4.1a's rule 2 routes `did:key:*` to.
//
// Note the cohort mismatch, deliberately preserved: the spec table says
// `did-key`; core-go's enum has `BackendKindSelfCertifying =
// "self-certifying"` and no `did-key`. Both are inert today (no such
// backend is registered either way, and §4.1a says entries naming an
// absent backend are inert rather than harmful), so this costs nothing
// now — but an interoperable default is exactly the thing two
// implementations are supposed to ship identically. We ship the spec's
// string and routed the mismatch.
const backendKindDIDKey = "did-key"

// localOnlyBackendKinds is the set the catch-all may name. A backend is
// local-only when answering from it involves no request to another
// party: the local-name store, and pinned bindings that never leave
// step 1.
//
// Deliberately NOT including `self-certifying` / `did-key`: resolving a
// did:key is local computation, but it reaches the catch-all only for a
// name that did not match rule 2, and admitting the kind here would
// make the guard's meaning "backends that happen not to dial" rather
// than "backends that cannot disclose". The narrow set is the one that
// stays correct as backends are added.
var localOnlyBackendKinds = map[string]bool{
	types.BackendKindLocalName: true,
	BackendKindPinned:          true,
}

// DefaultNameFormatDispatch returns EXTENSION-REGISTRY §4.1a's
// recommended default list (spec 1.7).
//
//  1. did:web:*   → did-web                        scheme-typed
//  2. did:key:*   → did-key                        scheme-typed, self-certifying
//  3. *.eth       → consensus-anchored             scheme-typed by suffix
//  4. *@*.*       → dns-txt, well-known-url        domain-scoped, DOTTED authority
//  5. *@*         → peer-issued                    registry-scoped, undotted handle
//  6. *           → local-name, pinned             catch-all, LOCAL ONLY (MUST)
//
// **The list is a FILTER and expresses no precedence** (§4, spec 1.7).
// A name matching several entries is eligible at the UNION of their
// backend_kinds; evaluation does not stop at the first match. Rules 4
// and 5 genuinely overlap — a POSIX glob cannot express "undotted", so
// `*@*` also matches `alice@example.org`, which is eligible at dns-txt,
// well-known-url AND peer-issued. What chooses between them is
// `resolver_chain[].priority` (§4.1 step 3, ascending) with the first
// validated hit winning (§4.1.1) — never the row order. The `#` column
// above is reference numbering, not evaluation order.
//
// We emit the rows in the spec's own sequence because it is the spec's
// sequence, not because the sequence carries meaning. A deployment that
// does not want a dotted name reaching its peer-issued registry
// expresses that by priority, or by narrowing rule 5 to its own handle
// (`*@entity-church`) — never by relying on row order.
//
// What bounds a broad pattern is therefore not its position but what it
// is permitted to NAME — §4.1 step 2's catch-all MUST, enforced in
// ValidateResolverConfig.
//
// Rules 1–4 name backends nothing in this cohort has built. That is the
// point rather than a gap: without them a web-native name shape falls
// through to the catch-all, which is the disclosure §4.1 step 2
// forbids, reached by a different door. An entry narrowing to an absent
// backend yields the empty set and the chain reports `chain_exhausted`,
// fail-closed.
func DefaultNameFormatDispatch() []types.DispatchEntry {
	return []types.DispatchEntry{
		{Pattern: "did:web:*", BackendKinds: []string{types.BackendKindDIDWeb}},
		{Pattern: "did:key:*", BackendKinds: []string{backendKindDIDKey}},
		{Pattern: "*.eth", BackendKinds: []string{types.BackendKindConsensusAnchored}},
		{Pattern: "*@*.*", BackendKinds: []string{types.BackendKindDNSTXT, types.BackendKindWellKnownURL}},
		{Pattern: "*@*", BackendKinds: []string{types.BackendKindPeerIssued}},
		{Pattern: CatchAllPattern, BackendKinds: []string{types.BackendKindLocalName, BackendKindPinned}},
	}
}

// DefaultResolverConfig returns the config a distribution SHOULD ship
// per §4.1a: the default dispatch list, and a resolver chain containing
// only the local-name backend.
//
// Local-only by construction. Adding a remote backend is an explicit
// act — which is the §6.4a shape, and the reason the default is worth
// shipping rather than leaving the field empty.
func DefaultResolverConfig() types.ResolverConfigData {
	return types.ResolverConfigData{
		ResolverChain: []types.ResolverChainEntry{
			{BackendKind: types.BackendKindLocalName, Priority: 0},
		},
		NameFormatDispatch: DefaultNameFormatDispatch(),
	}
}

// ValidateResolverConfig enforces the one MUST inside §4.1a before a
// config can reach the tree.
//
// Two failures, both refusals rather than normalizations. Normalizing
// would be conformant per §11.1 ("refused or normalized at load"), but
// silently rewriting an operator's privacy configuration into a
// different one is the wrong half of that choice: the operator asked
// for something and would not learn they did not get it.
//
//   - A catch-all naming a non-local backend. The disclosure case, and
//     now the only one: under the filter reading (§4, spec 1.7) a broad
//     pattern is bounded by what it may NAME, not by where it sits, so
//     catch-all position is not a defect and is not refused here.
func ValidateResolverConfig(cfg types.ResolverConfigData) error {
	for _, d := range cfg.NameFormatDispatch {
		if d.Pattern != CatchAllPattern {
			continue
		}
		for _, kind := range d.BackendKinds {
			if !localOnlyBackendKinds[kind] {
				return NewError(400, "catchall_not_local",
					fmt.Sprintf("resolver-config catch-all %q names backend kind %q, which is not local-only "+
						"(EXTENSION-REGISTRY §4.1 step 2, MUST): every unscoped name a user types would be "+
						"disclosed to it. Local-only kinds: %s",
						CatchAllPattern, kind, strings.Join(sortedLocalKinds(), ", ")))
			}
		}
	}
	return nil
}

func sortedLocalKinds() []string {
	out := make([]string, 0, len(localOnlyBackendKinds))
	for k := range localOnlyBackendKinds {
		out = append(out, k)
	}
	// Small fixed set; insertion order is not stable across map
	// iteration, and this string lands in an error a human reads.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// InstallResolverConfig validates and writes the resolver-config at its
// canonical path. The entity is peer-local and not synced (§4).
func (a *AppPeer) InstallResolverConfig(cfg types.ResolverConfigData) error {
	if err := ValidateResolverConfig(cfg); err != nil {
		return err
	}
	ent, err := cfg.ToEntity()
	if err != nil {
		return WrapError(500, "encode_config", "encode resolver-config", err)
	}
	if _, err := a.PutEntity(types.ResolverConfigStoragePath, ent); err != nil {
		return err
	}
	return nil
}

// ResolverConfig reads the installed resolver-config, if any.
func (a *AppPeer) ResolverConfig() (types.ResolverConfigData, bool, error) {
	ent, ok := a.store.Get(types.ResolverConfigStoragePath)
	if !ok {
		return types.ResolverConfigData{}, false, nil
	}
	if ent.Type != types.TypeRegistryResolverConfig {
		return types.ResolverConfigData{}, false, NewError(500, "unexpected_result_type",
			fmt.Sprintf("%s holds type %q, want %s",
				types.ResolverConfigStoragePath, ent.Type, types.TypeRegistryResolverConfig))
	}
	cfg, err := types.ResolverConfigDataFromEntity(ent)
	if err != nil {
		return types.ResolverConfigData{}, false, WrapError(500, "decode_failed", "decode resolver-config", err)
	}
	return cfg, true, nil
}

// EnsureResolverConfig installs DefaultResolverConfig if no config is
// present, and leaves an existing one untouched. Returns whether it
// wrote one.
//
// Idempotent on purpose: an operator's config is theirs, and a
// bootstrap helper that overwrote it on every start would be a
// configuration surface that silently reverts.
func (a *AppPeer) EnsureResolverConfig() (bool, error) {
	if _, found, err := a.ResolverConfig(); err != nil {
		return false, err
	} else if found {
		return false, nil
	}
	if err := a.InstallResolverConfig(DefaultResolverConfig()); err != nil {
		return false, err
	}
	return true, nil
}
