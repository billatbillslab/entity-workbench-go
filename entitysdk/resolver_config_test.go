package entitysdk

import (
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/types"
)

// TestDefaultNameFormatDispatch_MatchesTheSpecTable pins §4.1a's table
// **at EXTENSION-REGISTRY 1.13** row for row: the six patterns and the
// exact backend_kinds each is eligible at.
//
// The version in that sentence is load-bearing. This table is the one
// artifact the spec tells implementations to copy verbatim, and it
// moved three times on 2026-08-18 — row 6 went `pinned` → removed →
// `out-of-band`, and entity-browser-rust pinned the middle revision,
// which had silently lost a legitimate capability. Arch's own reading
// (ROUTING-2026-08-18-q §5): "a table change is a cohort event". A pin
// that names the revision it was taken at is how the next such move
// shows up as a red test with a version to compare, rather than as two
// app-tier seats disagreeing in trees neither of them reads.
//
// It pins the ROWS, not an evaluation order. §4 (1.7) makes the list a
// FILTER expressing no precedence — a name matching several entries is
// eligible at the union, and `resolver_chain[].priority` decides which
// backend answers. The `#` column is reference numbering.
//
// This pin previously asserted the sequence as the contract, on §4 1.6's
// "ORDERED list, first-match-wins (MUST)". Arch withdrew that in
// `3670283` (D1 of PROPOSAL-DEFAULT-NAME-FORMAT-DISPATCH), ~78 minutes
// after we shipped against it. The sequence assertion is gone rather
// than inverted: order is now inert, so pinning it either way would
// pin a fact the spec does not carry.
//
// Tier: contract pin.
func TestDefaultNameFormatDispatch_MatchesTheSpecTable(t *testing.T) {
	want := []struct {
		pattern string
		kinds   []string
	}{
		{"did:web:*", []string{"did-web"}},
		{"did:key:*", []string{"self-certifying"}},
		{"*.eth", []string{"consensus-anchored"}},
		{"*@*.*", []string{"dns-txt", "well-known-url"}},
		{"*@*", []string{"peer-issued"}},
		{"*", []string{"local-name", "self-certifying", "out-of-band", "peer-issued"}},
	}
	got := DefaultNameFormatDispatch()
	if len(got) != len(want) {
		t.Fatalf("default dispatch has %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Pattern != want[i].pattern {
			t.Errorf("entry %d pattern = %q, want %q", i+1, got[i].Pattern, want[i].pattern)
		}
		if strings.Join(got[i].BackendKinds, ",") != strings.Join(want[i].kinds, ",") {
			t.Errorf("entry %d kinds = %v, want %v", i+1, got[i].BackendKinds, want[i].kinds)
		}
	}

	// The catch-all must be PRESENT and free of name-transmitting
	// backends. Not "last" — under the filter reading its position
	// carries nothing, and refusing a non-final catch-all would reject
	// a config §4 permits.
	catchAll := -1
	for i, d := range got {
		if d.Pattern == CatchAllPattern {
			catchAll = i
		}
	}
	if catchAll < 0 {
		t.Fatalf("no catch-all entry: a name matching nothing is treated as matching it (§4.1a)")
	}
	if err := ValidateResolverConfig(types.ResolverConfigData{NameFormatDispatch: got}); err != nil {
		t.Errorf("the default list must satisfy its own §4.1 step 2 MUST: %v", err)
	}
}

// TestValidateResolverConfig_CatchAllPositionIsNotADefect pins the
// withdrawal directly: a catch-all that is not the final entry is a
// LEGAL config under §4 (1.7), because the list is a filter and the
// entries after it stay eligible at the union.
//
// We refused this until the audit of 2026-08-18 — a refusal built on
// §4 1.6's first-match-wins reading, which arch withdrew in `3670283`.
// Refusing it rejected a config the spec permits.
//
// Tier: contract pin (regression).
func TestValidateResolverConfig_CatchAllPositionIsNotADefect(t *testing.T) {
	cfg := types.ResolverConfigData{
		NameFormatDispatch: []types.DispatchEntry{
			{Pattern: CatchAllPattern, BackendKinds: []string{types.BackendKindLocalName}},
			{Pattern: "*@*.*", BackendKinds: []string{types.BackendKindDNSTXT}},
		},
	}
	if err := ValidateResolverConfig(cfg); err != nil {
		t.Errorf("catch-all in a non-final position was refused (%v), but §4 (1.7) makes the list a "+
			"filter with no precedence — the later entry is still eligible, and priority orders the "+
			"chain. Only what the catch-all NAMES is a MUST.", err)
	}
}

// TestValidateResolverConfig_CatchAllMustNotTransmitTheName pins the
// one MUST inside §4.1a, and it is the security-relevant one: §4.1
// step 2 calls itself the primary privacy mechanism, so a catch-all
// naming a name-transmitting backend discloses every unscoped name a
// user types.
//
// **The banned property is name transmission, not remoteness** (spec
// 1.8). This test asserted remoteness until 2026-08-18 and passed the
// whole time, because the only remote kind it tried — `peer-issued` —
// happens to be refused under the old keying and permitted under the
// new one. A test that passes under two incompatible rules is not
// evidence for either, so the cases below name the property directly:
// the four kinds §4.1 step 2 lists as disclosing, and the four it
// admits.
//
// Tier: contract pin (regression).
func TestValidateResolverConfig_CatchAllMustNotTransmitTheName(t *testing.T) {
	if err := ValidateResolverConfig(DefaultResolverConfig()); err != nil {
		t.Fatalf("the shipped default must validate: %v", err)
	}

	withCatchAll := func(kinds ...string) types.ResolverConfigData {
		cfg := DefaultResolverConfig()
		cfg.NameFormatDispatch[len(cfg.NameFormatDispatch)-1] = types.DispatchEntry{
			Pattern:      CatchAllPattern,
			BackendKinds: kinds,
		}
		return cfg
	}

	// The four §4.1 step 2 names as disclosing — consultation IS the
	// disclosure, whether by DNS query, URL path, or document name.
	for _, kind := range []string{
		types.BackendKindDNSTXT,
		types.BackendKindWellKnownURL,
		types.BackendKindDIDWeb,
		types.BackendKindConsensusAnchored,
	} {
		err := ValidateResolverConfig(withCatchAll(kind))
		if err == nil {
			t.Errorf("a catch-all naming %s was accepted; every unscoped name would leave the box", kind)
			continue
		}
		if !strings.Contains(err.Error(), "catchall_transmits_name") {
			t.Errorf("%s: error does not carry the code: %v", kind, err)
		}
	}

	// The four it admits. `peer-issued` is the one that makes the
	// re-key visible: it is a REMOTE backend and the catch-all MAY name
	// it, because §6a.4 resolves it by content address through a signed
	// root and the queried name never appears in a request. Under the
	// remoteness keying we shipped first, this row was refused — a
	// deployment the spec permits, rejected.
	for _, kind := range []string{
		types.BackendKindLocalName,
		types.BackendKindSelfCertifying,
		types.BackendKindOutOfBand,
		types.BackendKindPeerIssued,
	} {
		if err := ValidateResolverConfig(withCatchAll(kind)); err != nil {
			t.Errorf("a catch-all naming %s was refused (%v), but §4.1 step 2 admits it: "+
				"consulting it transmits no name", kind, err)
		}
	}

	// Mixed safe + disclosing is still disclosing — one member is
	// enough, and the safe members do not launder it.
	if err := ValidateResolverConfig(withCatchAll(types.BackendKindLocalName, types.BackendKindDNSTXT)); err == nil {
		t.Error("a catch-all naming local-name AND dns-txt was accepted; one disclosing member is enough")
	}

	// An unrecognized kind is INERT, not a refusal: §4.2 makes a
	// conformant peer skip the entry with a warning, so nothing is
	// registered under it to disclose anything. Refusing here would
	// reject a config authored against a newer vocabulary than ours —
	// the failure mode this guard has now had twice.
	if err := ValidateResolverConfig(withCatchAll("some-future-kind")); err != nil {
		t.Errorf("a catch-all naming an unknown kind was refused (%v); §4.2 makes it inert, "+
			"and refusing rejects a config a later vocabulary permits", err)
	}

	// A non-final catch-all is checked the same way as any other: on
	// what it NAMES. Position is not a defect (see
	// TestValidateResolverConfig_CatchAllPositionIsNotADefect), but a
	// non-final catch-all naming a remote backend still discloses.
	notLastDisclosing := types.ResolverConfigData{
		NameFormatDispatch: []types.DispatchEntry{
			{Pattern: CatchAllPattern, BackendKinds: []string{types.BackendKindDNSTXT}},
			{Pattern: "*@*", BackendKinds: []string{types.BackendKindPeerIssued}},
		},
	}
	if err := ValidateResolverConfig(notLastDisclosing); err == nil {
		t.Error("a non-final catch-all naming dns-txt was accepted; position does not exempt it from the MUST")
	} else if !strings.Contains(err.Error(), "catchall_transmits_name") {
		t.Errorf("error does not carry the code: %v", err)
	}

	// An empty config still validates — but for a narrower reason than
	// this test claimed until 2026-08-19. Shipping no dispatch list is
	// §4.1a's SHOULD and EnsureResolverConfig's job, so an empty list
	// over an empty chain discloses nothing. It stops being harmless
	// the moment the chain holds a name-transmitting kind, which is
	// door 2 (TestValidateResolverConfig_TheRuleBindsTheConfiguration).
	if err := ValidateResolverConfig(types.ResolverConfigData{}); err != nil {
		t.Errorf("an empty config over an empty chain must validate: %v", err)
	}
}

// TestValidateResolverConfig_TheRuleBindsTheConfiguration pins the
// 1.14 widening: §4.1 step 2's MUST binds the **configuration**, not
// the catch-all row (arch ROUTING-2026-08-19-b §2, proposal D4). A rule
// that binds one row is evaded by not writing that row, and our
// validator's loop — `if d.Pattern != CatchAllPattern { continue }` —
// could see only that row.
//
// Two doors, one case each, and the second is the one that matters
// more: it has no rule to inspect at all.
//
// Tier: contract pin.
func TestValidateResolverConfig_TheRuleBindsTheConfiguration(t *testing.T) {
	// Door 1 — a BROAD pattern that is not the catch-all. `al*`
	// matches every unscoped name beginning `al`, which is arch's own
	// example, and `*` never appears in the config.
	broad := types.ResolverConfigData{
		ResolverChain: []types.ResolverChainEntry{{BackendKind: types.BackendKindDNSTXT, Priority: 0}},
		NameFormatDispatch: []types.DispatchEntry{
			{Pattern: "al*", BackendKinds: []string{types.BackendKindDNSTXT}},
			{Pattern: CatchAllPattern, BackendKinds: []string{types.BackendKindLocalName}},
		},
	}
	if err := ValidateResolverConfig(broad); err == nil {
		t.Error("`al*` naming dns-txt was accepted; it matches unscoped names, and the MUST binds " +
			"the configuration rather than the catch-all row")
	} else if !strings.Contains(err.Error(), "broad_pattern_transmits_name") {
		t.Errorf("error does not carry the code: %v", err)
	}

	// Door 2 — no dispatch list, and a name-transmitting kind in the
	// chain. §4.1 step 2's eligible_kinds returns ALL when there are no
	// rules: the filter is DISABLED, so every name reaches dns-txt and
	// there is no row anywhere to point at.
	filterDisabled := types.ResolverConfigData{
		ResolverChain: []types.ResolverChainEntry{
			{BackendKind: types.BackendKindLocalName, Priority: 0},
			{BackendKind: types.BackendKindWellKnownURL, Priority: 10},
		},
	}
	if err := ValidateResolverConfig(filterDisabled); err == nil {
		t.Error("an absent name_format_dispatch over a chain holding well-known-url was accepted; " +
			"with no rules the filter is disabled and every unscoped name is eligible for it")
	} else if !strings.Contains(err.Error(), "filter_disabled_transmits_name") {
		t.Errorf("error does not carry the code: %v", err)
	}

	// The same chain WITH the default list is fine, and that is the
	// remedy the error names: the kinds are scoped to the name shapes
	// they answer for, so no bare name reaches them.
	scoped := filterDisabled
	scoped.NameFormatDispatch = DefaultNameFormatDispatch()
	if err := ValidateResolverConfig(scoped); err != nil {
		t.Errorf("the same chain under §4.1a's default list was refused (%v); the list is exactly "+
			"what scopes a name-transmitting kind to a name shape that carries an authority", err)
	}

	// A chain holding only name-blind kinds is not reached by door 2 at
	// all — the filter being disabled discloses nothing when nothing in
	// the chain transmits. This is the discontinuity §4.1 step 2 calls
	// deliberate, and it is why door 2 keys on the chain and not on the
	// absence.
	blind := types.ResolverConfigData{
		ResolverChain: []types.ResolverChainEntry{
			{BackendKind: types.BackendKindLocalName, Priority: 0},
			{BackendKind: types.BackendKindPeerIssued, Priority: 10},
		},
	}
	if err := ValidateResolverConfig(blind); err != nil {
		t.Errorf("an absent dispatch list over a name-blind chain was refused (%v); "+
			"a peer whose chain holds only name-blind backends transmits nothing either way", err)
	}
}

// TestMatchesUnscopedNames_TheClassificationTheMUSTKeysOn pins how we
// decide "matches unscoped names", because §4.1 step 2 states the
// property and supplies no decision procedure.
//
// The load-bearing row is `*.eth`: read literally a name is a flat
// string, so `alice.eth` is syntactically bare and row 3 of §4.1a would
// violate the MUST the same table declares. "Unscoped" therefore means
// "carrying no explicit authority marker", and the shipped default is
// the fixture that proves it — every row of it names a transmitting
// kind or does not, and the whole list must validate.
//
// Tier: contract pin.
func TestMatchesUnscopedNames_TheClassificationTheMUSTKeysOn(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		broad   bool
		why     string
	}{
		{"*", true, "the catch-all — every bare name"},
		{"al*", true, "a literal prefix and no authority; arch's own example"},
		{"*ice", true, "a wildcard-free tail that is not a dotted suffix is not a marker"},
		{"*a*", true, "two wildcards, so nothing is required of the tail"},
		{"alice", true, "an exact bare name is still a bare name"},
		{"", true, "matches the empty name, which carries no authority"},
		{"*@*", false, "@ is in every matching name — an authority part"},
		{"*@*.*", false, "§4.1a row 4"},
		{"did:web:*", false, "a scheme prefix — §4.1a row 1"},
		{"did:key:*", false, "§4.1a row 2"},
		{"*.eth", false, "a dotted authority suffix — §4.1a row 3"},
		{"*.example.org", false, "the same shape, one authority deep"},
	} {
		if got := matchesUnscopedNames(tc.pattern); got != tc.broad {
			t.Errorf("matchesUnscopedNames(%q) = %v, want %v — %s", tc.pattern, got, tc.broad, tc.why)
		}
	}

	// The classification is only correct if §4.1a's shipped list
	// survives it. Rows 1, 3 and 4 name name-transmitting kinds; if any
	// of them classified as broad, the spec's own recommended default
	// would be self-violating.
	if err := ValidateResolverConfig(DefaultResolverConfig()); err != nil {
		t.Fatalf("the §4.1a default list does not survive its own MUST: %v", err)
	}
}

// TestEnsureResolverConfig_InstallsOnceAndDoesNotOverwrite pins the
// round trip and the idempotence. An operator's config is theirs; a
// bootstrap helper that rewrote it every start would be a configuration
// surface that silently reverts.
// TestResolverConfig_RefusesAViolatingConfigAtLoad pins §11.1's
// PLACEMENT: "refused or normalized **at load**". Enforcing only on
// author is the variant that fails, because what a config means depends
// on a vocabulary outside it — a kind that was inert under §4.2 when it
// was written becomes disclosing the moment it is declared, and nothing
// re-examines a config that was validated once.
//
// The fixture writes the entity straight to the tree, bypassing
// InstallResolverConfig, which is exactly how a config authored by an
// older build (or another tool) arrives.
//
// Tier: contract pin.
func TestResolverConfig_RefusesAViolatingConfigAtLoad(t *testing.T) {
	ap, err := CreatePeer(PeerConfig{Extensions: ExtensionsConfig{Registry: &RegistryConfig{}}})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	violating := types.ResolverConfigData{
		ResolverChain: []types.ResolverChainEntry{{BackendKind: types.BackendKindDNSTXT, Priority: 0}},
		NameFormatDispatch: []types.DispatchEntry{
			{Pattern: CatchAllPattern, BackendKinds: []string{types.BackendKindDNSTXT}},
		},
	}
	ent, err := violating.ToEntity()
	if err != nil {
		t.Fatalf("ToEntity: %v", err)
	}
	if _, err := ap.PutEntity(types.ResolverConfigStoragePath, ent); err != nil {
		t.Fatalf("PutEntity: %v", err)
	}

	cfg, found, err := ap.ResolverConfig()
	if err == nil {
		t.Fatal("a stored config whose catch-all names dns-txt loaded clean; §11.1 refuses at load, " +
			"and a write-time-only check never re-examines what is already in the tree")
	}
	if !found {
		t.Error("the refusal also reported not-found; the entity is there and an operator has to see it")
	}
	if len(cfg.NameFormatDispatch) != 1 {
		t.Errorf("the refusal withheld the config (%+v); a load-time refusal denies USE, not SIGHT — "+
			"an operator cannot repair bytes they cannot read", cfg)
	}

	// And Ensure does not paper over it by reinstalling the default.
	// That would be §11.1's normalization half, applied to somebody
	// else's privacy configuration, on a boot they did not ask about.
	if wrote, err := ap.EnsureResolverConfig(); err == nil {
		t.Error("EnsureResolverConfig accepted a violating stored config")
	} else if wrote {
		t.Error("EnsureResolverConfig overwrote a violating config instead of refusing it")
	}
}

func TestEnsureResolverConfig_InstallsOnceAndDoesNotOverwrite(t *testing.T) {
	ap, err := CreatePeer(PeerConfig{Extensions: ExtensionsConfig{Registry: &RegistryConfig{}}})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	if _, found, err := ap.ResolverConfig(); err != nil {
		t.Fatalf("ResolverConfig before install: %v", err)
	} else if found {
		t.Fatal("a fresh peer already has a resolver-config")
	}

	wrote, err := ap.EnsureResolverConfig()
	if err != nil {
		t.Fatalf("EnsureResolverConfig: %v", err)
	}
	if !wrote {
		t.Fatal("EnsureResolverConfig reported no write on a peer with no config")
	}

	cfg, found, err := ap.ResolverConfig()
	if err != nil || !found {
		t.Fatalf("ResolverConfig after install: found=%v err=%v", found, err)
	}
	if len(cfg.NameFormatDispatch) != 6 {
		t.Errorf("installed dispatch has %d entries, want 6", len(cfg.NameFormatDispatch))
	}
	if len(cfg.ResolverChain) != 1 || cfg.ResolverChain[0].BackendKind != types.BackendKindLocalName {
		t.Errorf("installed chain = %+v, want local-name only", cfg.ResolverChain)
	}

	// Second call must not overwrite — including an operator's edit.
	custom := DefaultResolverConfig()
	custom.PinnedBindings = []types.PinnedEntry{{Name: "alice", TargetPeerID: "somepeer"}}
	if err := ap.InstallResolverConfig(custom); err != nil {
		t.Fatalf("InstallResolverConfig: %v", err)
	}
	wrote, err = ap.EnsureResolverConfig()
	if err != nil {
		t.Fatalf("EnsureResolverConfig (second): %v", err)
	}
	if wrote {
		t.Error("EnsureResolverConfig overwrote an existing config")
	}
	cfg, _, err = ap.ResolverConfig()
	if err != nil {
		t.Fatalf("ResolverConfig after second Ensure: %v", err)
	}
	if len(cfg.PinnedBindings) != 1 {
		t.Errorf("the operator's pinned binding was lost: %+v", cfg.PinnedBindings)
	}
}

// TestInstallResolverConfig_RefusesTheDisclosure is the end-to-end half
// of the MUST: the refusal has to happen at the write, not only in a
// pure function nobody is obliged to call.
func TestInstallResolverConfig_RefusesTheDisclosure(t *testing.T) {
	ap, err := CreatePeer(PeerConfig{Extensions: ExtensionsConfig{Registry: &RegistryConfig{}}})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()

	bad := DefaultResolverConfig()
	bad.NameFormatDispatch[len(bad.NameFormatDispatch)-1] = types.DispatchEntry{
		Pattern:      CatchAllPattern,
		BackendKinds: []string{types.BackendKindDNSTXT},
	}
	if err := ap.InstallResolverConfig(bad); err == nil {
		t.Fatal("InstallResolverConfig wrote a disclosing catch-all")
	}
	if _, found, err := ap.ResolverConfig(); err != nil {
		t.Fatalf("ResolverConfig: %v", err)
	} else if found {
		t.Error("the refused config was written anyway")
	}
}

// TestCatchAllClassification_CoversTheDeclaredVocabulary asserts the
// two §4.1 step 2 sets are TOTAL over core-go's declared backend-kind
// vocabulary (§2.4.1), and disjoint.
//
// This is the pin that would have caught the defect this file shipped
// with. We had `pinned` and `did-key` in the default list and in the
// safe set, and neither is a declared kind — §4.2 makes a conformant
// peer skip such an entry with a warning, so both rows were dead config
// wherever they were installed. A classification that has to be total
// over the enum cannot quietly contain a string the enum does not
// carry.
//
// What it does NOT cover, stated rather than implied: a NINTH kind
// added to core-go later. A Go test cannot enumerate constants that did
// not exist when it was written, so the list below is hand-maintained
// and a vocabulary bump has to be reconciled by hand. The pin catches
// our drift, not theirs.
//
// Tier: contract pin.
func TestCatchAllClassification_CoversTheDeclaredVocabulary(t *testing.T) {
	declared := []string{
		types.BackendKindSelfCertifying,
		types.BackendKindLocalName,
		types.BackendKindDNSTXT,
		types.BackendKindWellKnownURL,
		types.BackendKindDIDWeb,
		types.BackendKindPeerIssued,
		types.BackendKindOutOfBand,
		types.BackendKindConsensusAnchored,
	}

	for _, kind := range declared {
		safe := catchAllSafeBackendKinds[kind]
		transmits := nameTransmittingBackendKinds[kind]
		switch {
		case safe && transmits:
			t.Errorf("%s is in both §4.1 step 2 columns; the classification must be disjoint", kind)
		case !safe && !transmits:
			t.Errorf("%s is declared in §2.4.1 but classified in neither column — §4.1 step 2 "+
				"classifies every declared kind, so an unclassified one means the table moved", kind)
		}
	}

	// And nothing invented locally: every string in either set is a
	// declared kind. A constant we had to define ourselves to satisfy a
	// spec table is the tell that one of the two documents is wrong.
	declaredSet := map[string]bool{}
	for _, k := range declared {
		declaredSet[k] = true
	}
	for _, set := range []map[string]bool{catchAllSafeBackendKinds, nameTransmittingBackendKinds} {
		for kind := range set {
			if !declaredSet[kind] {
				t.Errorf("%q is classified but is not a §2.4.1 backend_kind; §4.2 makes a conformant "+
					"peer skip it with a warning, so any row naming it is dead config", kind)
			}
		}
	}
}

// TestValidateResolverConfig_DoesNotRejectPatterns pins the refusal
// half of REG-DISPATCH-GRAMMAR-1 (EXTENSION-REGISTRY 1.13): the
// dispatch-pattern grammar is CLOSED and every byte that is not `*` is
// a literal, so **no pattern is invalid** and a registry MUST NOT
// reject one for containing `?`, `[`, or `\`.
//
// We author patterns; we do not match them (the matcher is the registry
// handler's, and delegating it to path.Match / fnmatch is forbidden by
// the same ruling, because those grant `?` and `[…]` meaning this
// grammar does not and stop `*` at `/`). So our whole exposure to the
// grammar is this: a validator that grew a well-formedness check would
// refuse a legal config. Pinned because "closed grammar" has meant
// "reject at write" everywhere else in this corpus — EXTENSION-REVISION's
// four forms need a 400 — and the opposite reading here is exactly the
// symmetry a future reader would restore in good faith.
//
// Tier: contract pin.
func TestValidateResolverConfig_DoesNotRejectPatterns(t *testing.T) {
	for _, pattern := range []string{
		"a?c",    // `?` is a literal, not a single-char wildcard
		"a[bc]d", // `[` `]` are literals, not a character class
		`a\*b`,   // `\` is a literal; it escapes nothing
		"x*z",    // `*` crosses `/` — `x/y/z` matches
		"*@*.*",  // three wildcards, and it is in §4.1a's own table
		"",       // empty is a pattern like any other: it matches only ""
		"**",     // no `**` token exists; this is just two wildcards
	} {
		cfg := types.ResolverConfigData{
			NameFormatDispatch: []types.DispatchEntry{
				{Pattern: pattern, BackendKinds: []string{types.BackendKindLocalName}},
			},
		}
		if err := ValidateResolverConfig(cfg); err != nil {
			t.Errorf("pattern %q was rejected (%v); the grammar is closed and every non-* byte is a "+
				"literal, so every string is a well-formed pattern (REG-DISPATCH-GRAMMAR-1)", pattern, err)
		}
	}
}
