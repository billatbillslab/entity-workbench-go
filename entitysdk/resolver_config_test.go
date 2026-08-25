package entitysdk

import (
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/types"
)

// TestDefaultNameFormatDispatch_MatchesTheSpecTable pins §4.1a's table
// (EXTENSION-REGISTRY 1.7) row for row: the six patterns and the exact
// backend_kinds each is eligible at.
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
		{"did:key:*", []string{"did-key"}},
		{"*.eth", []string{"consensus-anchored"}},
		{"*@*.*", []string{"dns-txt", "well-known-url"}},
		{"*@*", []string{"peer-issued"}},
		{"*", []string{"local-name", "pinned"}},
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

	// The catch-all must be PRESENT and local-only. Not "last" — under
	// the filter reading its position carries nothing, and refusing a
	// non-final catch-all would reject a config §4 permits.
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

// TestValidateResolverConfig_CatchAllMustBeLocal pins the one MUST
// inside §4.1a, and it is the security-relevant one: §4.1 step 2 calls
// itself the primary privacy mechanism, so a catch-all bound to a
// remote backend discloses every unscoped name a user types.
func TestValidateResolverConfig_CatchAllMustBeLocal(t *testing.T) {
	if err := ValidateResolverConfig(DefaultResolverConfig()); err != nil {
		t.Fatalf("the shipped default must validate: %v", err)
	}

	// A catch-all routed at a remote registry — the disclosure case.
	bad := DefaultResolverConfig()
	bad.NameFormatDispatch[len(bad.NameFormatDispatch)-1] = types.DispatchEntry{
		Pattern:      CatchAllPattern,
		BackendKinds: []string{types.BackendKindPeerIssued},
	}
	err := ValidateResolverConfig(bad)
	if err == nil {
		t.Fatal("a catch-all naming peer-issued was accepted; every unscoped name would leave the box")
	}
	if !strings.Contains(err.Error(), "catchall_not_local") {
		t.Errorf("error does not carry the code: %v", err)
	}

	// Mixed local + remote is still remote.
	mixed := DefaultResolverConfig()
	mixed.NameFormatDispatch[len(mixed.NameFormatDispatch)-1] = types.DispatchEntry{
		Pattern:      CatchAllPattern,
		BackendKinds: []string{types.BackendKindLocalName, types.BackendKindDNSTXT},
	}
	if err := ValidateResolverConfig(mixed); err == nil {
		t.Error("a catch-all naming local-name AND dns-txt was accepted; one remote member is enough to disclose")
	}

	// A non-final catch-all is checked the same way as any other: on
	// what it NAMES. Position is not a defect (see
	// TestValidateResolverConfig_CatchAllPositionIsNotADefect), but a
	// non-final catch-all naming a remote backend still discloses.
	notLastRemote := types.ResolverConfigData{
		NameFormatDispatch: []types.DispatchEntry{
			{Pattern: CatchAllPattern, BackendKinds: []string{types.BackendKindPeerIssued}},
			{Pattern: "*@*", BackendKinds: []string{types.BackendKindPeerIssued}},
		},
	}
	if err := ValidateResolverConfig(notLastRemote); err == nil {
		t.Error("a non-final catch-all naming peer-issued was accepted; position does not exempt it from the MUST")
	} else if !strings.Contains(err.Error(), "catchall_not_local") {
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
		BackendKinds: []string{types.BackendKindPeerIssued},
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
