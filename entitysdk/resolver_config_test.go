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

	// No dispatch list at all is not this validator's business: it is
	// §4.1a's SHOULD, not its MUST, and EnsureResolverConfig is what
	// supplies the default.
	if err := ValidateResolverConfig(types.ResolverConfigData{}); err != nil {
		t.Errorf("an empty config must validate (the SHOULD is Ensure's job, not the validator's): %v", err)
	}
}

// TestEnsureResolverConfig_InstallsOnceAndDoesNotOverwrite pins the
// round trip and the idempotence. An operator's config is theirs; a
// bootstrap helper that rewrote it every start would be a configuration
// surface that silently reverts.
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
