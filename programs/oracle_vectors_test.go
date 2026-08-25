package programs

// TIER: integration (TESTING-STRATEGY) — a real AppPeer, a real store, the real
// compute evaluator. Naming the tier is the discipline.
//
// THE FROZEN ORACLE. `TestMount_LifeMatchesHardCodedModel` (host_test.go)
// proves the mounted program computes what the hard-coded model computes by
// running BOTH and comparing. That is the right gate while both exist, and it
// is the reason the legacy models cannot be deleted: the test's reference is
// the legacy code itself.
//
// This file re-pins that reference as **frozen vectors** — the twelve state
// hashes the two sides agreed on, recorded — so the legacy models stop being
// load-bearing test infrastructure and can be retired on their own schedule.
//
// It is not merely a replacement. It catches something the mutual comparison
// **structurally cannot**: if the kernel's entity encoding or content-hash
// changes, both sides shift together and the comparison stays green while
// every hash on disk moved. The frozen vector fails, loudly, which is the
// correct outcome — a program whose state hashes changed is a program a
// consumer's mirror no longer matches.
//
// **When is regenerating these legitimate?** Only when the program's semantics
// are deliberately changed, or the kernel's encoding changes under us and the
// cohort agrees on the new bytes. Never to make a red test green. A diff that
// edits this array and nothing else in `programs/` is a diff that changed what
// Life computes without meaning to.
//
// Regenerate with (temporary file, deleted after — see git history 2026-08-20):
//
//	old, _ := NewLifeGameModel(ap, "legacy/life", lifeOracleSeed)
//	for i := 0; i < lifeOracleTicks; i++ { old.tickOnce(); print(stateHashAt(...)) }

import (
	"strings"
	"testing"
)

const (
	// lifeOracleSeed / lifeOracleTicks MUST match the constants in
	// TestMount_LifeMatchesHardCodedModel — the vectors below are that
	// test's `wantHashes`, captured. A different seed here would freeze a
	// different run and quietly stop being the same oracle.
	lifeOracleSeed  = uint64(12345)
	lifeOracleTicks = 12
)

// lifeOracleStateHashes are the hard-coded Life model's state-entity content
// hashes for ticks 1..12 at seed 12345, captured 2026-08-20 against
// entity-core-go @ 5020e62, at the commit where the mounted program and the
// hard-coded model were verified equal tick-for-tick.
var lifeOracleStateHashes = []string{
	"ecf-sha256:e77490832919fa01da0ec316ad109fcf47a02721add9529e6328729aa485c4a9",
	"ecf-sha256:5a71a8c4df126a1cc8eea997fe9dfa438ccf6edaaf839c52f4fc3b97c20fb8f4",
	"ecf-sha256:d2472a59790a504b0d2fcb10bf30198e8e420c9b56f2b956930c408360520793",
	"ecf-sha256:0deaf664a77687c625a86c67d63f7e285fd75db665597a473580574fb491acab",
	"ecf-sha256:8e812e30d634a8dcd6ee45b2c73b01778d7fdea413589c34b102cf741e266236",
	"ecf-sha256:e09502f61ddeb1fc36ffe7c9dd5cb7472469add70708f1b979053d8516c449d0",
	"ecf-sha256:3d532a7bfdc835092c4d527bc2bcbce0dba0daf8a34ca3b6f2e5625da963a411",
	"ecf-sha256:2ce9b0102d1304472573ef6a28d987c3ce64a4bc7a2a384dfdb85713eeea7863",
	"ecf-sha256:150c571527b6d393ef4ef33d0923272be5d569a48aff95557d230af5584a8de7",
	"ecf-sha256:471e1138ac6a49494a3195edc7c656957fb548b63fc6f7694b4fc35343ac8aa0",
	"ecf-sha256:d80d49d9fd8ab688dc6cc8abae423bbf9dcd3232554eaf296f640d9c3222d01b",
	"ecf-sha256:4cb0b599baeec3636d955eb571a1297c09b538ca447b8113df7a7b7d4865eecd",
}

// TestMount_LifeMatchesFrozenOracle is the retirement-safe half of the
// acceptance criterion: the MOUNTED program alone, with no legacy model in the
// process, must reproduce the frozen hash sequence tick for tick.
//
// ANTI-VACUITY, same as the mutual test: the grid must actually evolve. A
// frozen vector of twelve identical hashes would pass trivially against a Life
// that went extinct on tick 1, so the distinctness of the FROZEN side is
// asserted before anything is compared. An assertion that cannot come out any
// other way is not a measurement.
func TestMount_LifeMatchesFrozenOracle(t *testing.T) {
	if len(lifeOracleStateHashes) != lifeOracleTicks {
		t.Fatalf("vector length %d != lifeOracleTicks %d — the frozen set and the "+
			"tick count disagree, so this test is asserting on a truncated run",
			len(lifeOracleStateHashes), lifeOracleTicks)
	}
	distinct := map[string]bool{}
	for _, s := range lifeOracleStateHashes {
		distinct[s] = true
	}
	if len(distinct) < lifeOracleTicks/2 {
		t.Fatalf("VACUOUS: the frozen vector has only %d distinct hashes across %d ticks — "+
			"it was captured from a run that was not evolving, so equality proves nothing",
			len(distinct), lifeOracleTicks)
	}

	ap := newTestPeer(t)
	descPath, err := AuthorLife(ap, LifeRoot, lifeOracleSeed)
	if err != nil {
		t.Fatalf("AuthorLife: %v", err)
	}
	h, err := Mount(ap, descPath)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(h.Close)

	for i := 0; i < lifeOracleTicks; i++ {
		if !h.tickOnce() {
			t.Fatalf("mounted program faulted at tick %d: %s", i, h.Render().Err)
		}
		got := stateHashAt(t, ap, h.Descriptor().StatePath)
		// Spelling before value: if the string form of a hash ever changes,
		// every tick below mismatches and the failure reads as twelve broken
		// ticks instead of one renamed encoding.
		if i == 0 {
			gotAlgo, _, _ := strings.Cut(got.String(), ":")
			wantAlgo, _, _ := strings.Cut(lifeOracleStateHashes[0], ":")
			if gotAlgo != wantAlgo {
				t.Fatalf("hash string form moved: live hashes are %q, the frozen vector is %q. "+
					"This is an encoding change, not a program change — re-capture the vectors "+
					"only once the cohort agrees on the new spelling.", gotAlgo, wantAlgo)
			}
		}
		if got.String() != lifeOracleStateHashes[i] {
			t.Fatalf("tick %d: mounted state hash %s != frozen oracle %s.\n"+
				"Either the program's semantics moved, or the kernel's entity encoding / "+
				"content hash did. Both are real findings; neither is fixed by editing the "+
				"vector. Check whether TestMount_LifeMatchesHardCodedModel still passes: if "+
				"it does, the two sides shifted TOGETHER and the change is under us in the "+
				"kernel; if it fails too, the change is in this repo.",
				i, got, lifeOracleStateHashes[i])
		}
	}
}
