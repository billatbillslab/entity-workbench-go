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
// MUST: **a shipped config MUST NOT make a backend whose consultation
// transmits the queried name eligible for an unscoped name.** Step 2
// calls itself the primary privacy mechanism; a name-transmitting
// backend reachable from a bare name discloses every unscoped name a
// user types — the whole private namespace, one name per keystroke, to
// a third party. InstallResolverConfig refuses such a config rather
// than writing it.
//
// **The rule binds the configuration, not the catch-all row** (spec
// 1.14, arch ROUTING-2026-08-19-b §2). It was written at the width of
// the instance and widened to the width of the invariant, because a
// rule that binds one row is evaded by not writing that row — either by
// using a different broad pattern, or by shipping no dispatch list at
// all, which disables the filter entirely.
//
// **The banned property is name transmission, not remoteness** (spec
// 1.8, arch ROUTING-2026-08-18-l §1). We shipped the remoteness reading
// first, which is what the rule's own text said before the re-key, and
// it was too narrow in both directions that matter: it refused
// `self-certifying` and `out-of-band`, which dial nobody, and it would
// have refused `peer-issued`, which §6a.4 resolves through a signed
// root by content address so the queried name never appears in a
// request. See catchAllSafeBackendKinds for the full classification.

// CatchAllPattern is the §4.1a catch-all — the entry every name that is
// not explicitly scoped falls to.
const CatchAllPattern = "*"

// The §4.1 step 2 classification of every backend kind §2.4.1
// declares, keyed on the property the MUST is actually about: does
// consulting this backend transmit the queried name to another party?
//
// Both maps together are TOTAL over core-go's eight `BackendKind*`
// constants, which is pinned by
// TestCatchAllClassification_CoversTheDeclaredVocabulary. That pin
// catches our own drift; it cannot catch core-go declaring a ninth
// kind, because a Go test cannot enumerate constants that did not exist
// when it was written. Stated rather than papered over — if the
// vocabulary grows, the growth arrives through a core-go bump and the
// classification below has to be revisited by hand.
//
// We deliberately do NOT invent local constants for either list. The
// last version of this file carried two — `BackendKindPinned` and
// `backendKindDIDKey` — because §4.1a's table named strings core-go's
// enum did not declare. That was the tell, and we filed it as an inert
// cohort observation: arch's ROUTING-2026-08-18-p §5 came back that
// both were **dead config in every conformant peer**, since §4.2 makes
// an unknown `backend_kind` MUST-skip with a warning. A constant we
// have to define ourselves to satisfy a spec table means one of the two
// documents is wrong; it is never a naming gap to fill locally.
var (
	// catchAllSafeBackendKinds — §4.1 step 2's MAY column.
	//
	// `out-of-band` is the kind a pin's synthesized binding carries
	// (§4.1.2). It is name-blind, and §6a.4 makes it dispatchable in as
	// many words: "a pin matches only if explicitly configured as its
	// own chain entry". The `pinned` string is NOT a backend kind and
	// cannot be reached from dispatch at all — §4.1 step 1 returns a
	// pinned match before the step-2 filter runs, and §4.1.2 uses
	// `pinned` as a `backend_id`, a result label rather than a dispatch
	// target.
	//
	// `peer-issued` is safe because §6a.4 fixes the mechanism in the
	// safe direction: signature, name-association and revocation are
	// verified INSIDE the signed tree, §6a.3a forbids presenting a
	// host-served listing as authoritative, and every fetch is by
	// content hash. The residual is a hash-prefix oracle on a miss and
	// a public binding's blob on a hit — categorically weaker than
	// handing a private name to a third-party resolver, and neither
	// reaches a name the registry does not carry.
	catchAllSafeBackendKinds = map[string]bool{
		types.BackendKindLocalName:      true,
		types.BackendKindSelfCertifying: true,
		types.BackendKindOutOfBand:      true,
		types.BackendKindPeerIssued:     true,
	}

	// nameTransmittingBackendKinds — §4.1 step 2's MUST NOT column.
	// Consultation IS disclosure: the name goes to a third party as a
	// query, a path segment, or a document name.
	nameTransmittingBackendKinds = map[string]bool{
		types.BackendKindDNSTXT:            true,
		types.BackendKindWellKnownURL:      true,
		types.BackendKindDIDWeb:            true,
		types.BackendKindConsensusAnchored: true,
	}
)

// DefaultNameFormatDispatch returns EXTENSION-REGISTRY §4.1a's
// recommended default list (spec 1.13).
//
//  1. did:web:*   → did-web                        scheme-typed
//  2. did:key:*   → self-certifying                scheme-typed, self-certifying
//  3. *.eth       → consensus-anchored             scheme-typed by suffix
//  4. *@*.*       → dns-txt, well-known-url        domain-scoped, DOTTED authority
//  5. *@*         → peer-issued                    registry-scoped, undotted handle
//  6. *           → local-name, self-certifying, out-of-band, peer-issued
//     (catch-all — NO NAME-TRANSMITTING BACKEND, MUST)
//
// Rows 2 and 6 are corrected at 1.13 and both were live defects here:
// we shipped `did-key` and `pinned`, neither of which §2.4.1 declares,
// so §4.2's forward-compat rule made a conformant peer skip both rows
// with a warning. Row 6 in particular moved three times in one day —
// `pinned` → removed → `out-of-band` — and 1.13 is the settled form.
// A pin against the middle revision loses a real capability, which is
// why the reference below is to the version and not to "the table".
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
		{Pattern: "did:key:*", BackendKinds: []string{types.BackendKindSelfCertifying}},
		{Pattern: "*.eth", BackendKinds: []string{types.BackendKindConsensusAnchored}},
		{Pattern: "*@*.*", BackendKinds: []string{types.BackendKindDNSTXT, types.BackendKindWellKnownURL}},
		{Pattern: "*@*", BackendKinds: []string{types.BackendKindPeerIssued}},
		{Pattern: CatchAllPattern, BackendKinds: []string{
			types.BackendKindLocalName,
			types.BackendKindSelfCertifying,
			types.BackendKindOutOfBand,
			types.BackendKindPeerIssued,
		}},
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

// matchesUnscopedNames reports whether a dispatch pattern can match a
// name that carries no explicit authority marker — the property §4.1
// step 2's MUST is keyed on at 1.14 ("any rule whose pattern matches
// unscoped names").
//
// **The spec states the property and gives no decision procedure**, and
// one is not derivable from the text alone: read literally, a name is a
// flat string, so `alice.eth` is "unscoped" and §4.1a row 3
// (`*.eth` → `consensus-anchored`) would violate the MUST the same
// table declares. So "unscoped" cannot mean "syntactically bare" — it
// means "carrying no explicit authority marker", and the markers are
// the ones §4.1a's own rows use: an `@` authority part, a `scheme:`
// prefix, and a dotted authority suffix.
//
// A pattern is NARROW (cannot match an unscoped name) when it forces
// every name it matches to carry one of those. Under §4's closed
// grammar that is decidable, because every non-`*` byte is a literal:
//
//   - it contains `@` — every matching name carries an authority part
//     (`*@*`, `*@*.*`);
//   - it contains `:` — every matching name carries a scheme
//     (`did:web:*`);
//   - it is a single leading `*` followed by a wildcard-free tail that
//     begins with `.` — every matching name ends in that dotted
//     authority suffix (`*.eth`, `*.example.org`).
//
// Everything else is BROAD, including the ones that are only
// *probably* broad. That direction is the point: calling a broad
// pattern narrow is what leaks, and the leak is silent and on the happy
// path.
//
// **Cross-impl note, routed rather than resolved here.**
// `entity-browser-rust`'s `is_broad` (`src/content_site/name_dispatch.rs`)
// is the same shape with a looser third clause: any wildcard-free tail
// counts, not only a dotted one. The two agree on every row of §4.1a
// and diverge on patterns like `*e`, which requires a literal `e` and
// no authority — narrow for them, broad for us. We took the strict
// side because it is the one their own doc sentence argues for ("a
// pattern we cannot confidently classify is treated as broad"), and
// because a validator is not a matcher: refusing an exotic config costs
// an operator an error message, and admitting one costs them every name
// they type. Which of the two is conformant needs a §11.1 row; the ask
// is routed.
func matchesUnscopedNames(pattern string) bool {
	if strings.ContainsAny(pattern, "@:") {
		return false
	}
	if tail := strings.TrimPrefix(pattern, "*"); tail != pattern &&
		tail != "" && !strings.Contains(tail, "*") && strings.HasPrefix(tail, ".") {
		return false
	}
	return true
}

// ValidateResolverConfig enforces the MUST inside §4.1 step 2 before a
// config can reach the tree: **a distribution's shipped resolver-config
// MUST NOT make a name-transmitting backend eligible for an unscoped
// name** (spec 1.14).
//
// Refusals rather than normalizations. Normalizing would be conformant
// per §11.1 ("refused or normalized at load"), but silently rewriting
// an operator's privacy configuration into a different one is the wrong
// half of that choice: the operator asked for something and would not
// learn they did not get it.
//
// **The rule binds the configuration, not one row** — widened by arch
// at 1.14 (D4, ROUTING-2026-08-19-b §2), because a rule binding one row
// is evadable by not writing that row. It has exactly two doors, and
// this function is one check per door:
//
//  1. **A rule that matches unscoped names naming a name-transmitting
//     kind.** The catch-all `*` is the usual one and is not the only
//     one: `al*` matches unscoped names and is not `*`. See
//     matchesUnscopedNames for how the class is decided. Position is
//     still not a defect — under the filter reading (§4, spec 1.7) a
//     broad pattern is bounded by what it may NAME, not by where it
//     sits.
//  2. **An absent or empty `name_format_dispatch` while a
//     name-transmitting kind sits in the `resolver_chain`.** §4.1 step
//     2's `eligible_kinds` returns ALL when there are no rules — the
//     filter is disabled, every kind is eligible for every name, and
//     there is **no catch-all row to inspect**. This is the door that
//     stays open after the other one closes, and it is the one our
//     validator could not see: the loop body simply never executed.
//     EnableLocalNameResolver's comment has named this hazard since
//     2026-08-18 without anything enforcing it.
//
// The third door — leaving a name-transmitting kind out of every rule
// so it "defaults to match all" — is closed by construction by the
// union rule (a kind named nowhere is eligible nowhere) and needs no
// check here.
//
// The check is a DENY-list over nameTransmittingBackendKinds, not an
// allow-list over the safe set, and the direction is deliberate. An
// unrecognized kind is inert by §4.2 — a conformant peer skips the
// entry with a warning, and nothing is registered under it to
// disclose anything — so refusing the whole config on account of one
// would reject a deployment authored against a newer vocabulary than
// ours. We have been bitten twice by a guard that was too strict (the
// withdrawn `catchall_not_last`, and the remoteness keying) and never
// once by one that was too loose. Refuse what the spec names as
// disclosing; let the unknown stay inert. (Arch upheld this reading
// against browser-rust's opposite one — ROUTING-2026-08-19-b §2, D10.)
//
// Patterns themselves are never rejected. §4's grammar is closed and
// every non-`*` byte is a literal, so no pattern is malformed — a
// registry MUST NOT reject one for containing `?`, `[`, or `\`
// (REG-DISPATCH-GRAMMAR-1, spec 1.13). We author patterns and do not
// match them; the matcher lives in the registry handler.
func ValidateResolverConfig(cfg types.ResolverConfigData) error {
	// Door 2 first: it is the one an absent list opens, and a config
	// that trips it has no rule to report against.
	if len(cfg.NameFormatDispatch) == 0 {
		for _, e := range cfg.ResolverChain {
			if nameTransmittingBackendKinds[e.BackendKind] {
				return NewError(400, "filter_disabled_transmits_name",
					fmt.Sprintf("resolver-config carries no name_format_dispatch while the resolver_chain "+
						"holds backend kind %q, whose consultation transmits the queried name "+
						"(EXTENSION-REGISTRY §4.1 step 2, MUST, spec 1.14): with no rules the filter is "+
						"DISABLED and every kind is eligible for every name, so every unscoped name a user "+
						"types would be disclosed to it. Ship §4.1a's default list (DefaultNameFormatDispatch) "+
						"and scope this kind to the name shape it answers for.", e.BackendKind))
			}
		}
		return nil
	}

	for _, d := range cfg.NameFormatDispatch {
		if !matchesUnscopedNames(d.Pattern) {
			continue
		}
		for _, kind := range d.BackendKinds {
			if !nameTransmittingBackendKinds[kind] {
				continue
			}
			code, where := "broad_pattern_transmits_name", fmt.Sprintf("pattern %q, which matches names carrying no explicit authority,", d.Pattern)
			if d.Pattern == CatchAllPattern {
				code, where = "catchall_transmits_name", fmt.Sprintf("catch-all %q", CatchAllPattern)
			}
			return NewError(400, code,
				fmt.Sprintf("resolver-config %s names backend kind %q, whose consultation "+
					"transmits the queried name (EXTENSION-REGISTRY §4.1 step 2, MUST): every unscoped "+
					"name a user types would be disclosed to it. Kinds a rule matching unscoped names "+
					"may name: %s", where, kind, strings.Join(sortedKinds(catchAllSafeBackendKinds), ", ")))
		}
	}
	return nil
}

func sortedKinds(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
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

// ResolverConfig reads the installed resolver-config, if any, and
// validates it **at load** — §11.1's placement, not ours: "MUST be
// refused or normalized at load".
//
// A write-time-only check is the variant that fails here, and the
// failure is not hypothetical for us. What a config means depends on a
// vocabulary that lives outside it: a `backend_kind` unknown when the
// config was authored is inert by §4.2 and discloses nothing, and it
// stops being inert the moment core-go declares it and our
// classification maps name it. The peer that upgrades re-reads the
// stored config, and the entry that was dead config becomes a refusal
// on that read. Nothing re-examines it if the only check ran on the day
// it was authored.
//
// **The config is returned even when it fails**, non-zero, alongside
// the error. A load-time refusal that also withheld the bytes would
// leave an operator unable to see what to repair — the entity is theirs
// and it is already in their tree; what the refusal denies is *use*,
// not *sight*. Callers that only want the stored bytes (repair tools, a
// `config show` verb) use the value and log the error; callers that act
// on the config MUST treat a non-nil error as fatal.
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
	return cfg, true, ValidateResolverConfig(cfg)
}

// EnsureResolverConfig installs DefaultResolverConfig if no config is
// present, and leaves an existing one untouched. Returns whether it
// wrote one.
//
// Idempotent on purpose: an operator's config is theirs, and a
// bootstrap helper that overwrote it on every start would be a
// configuration surface that silently reverts.
//
// A stored config that fails the §4.1 step 2 MUST surfaces here as an
// error and is NOT replaced. That is the load-time refusal (see
// ResolverConfig), and overwriting instead would be the normalization
// half of §11.1 — conformant, and the wrong half: it would repair an
// operator's privacy configuration into a different one on the next
// boot, silently, which is the failure this whole surface exists to
// prevent.
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
