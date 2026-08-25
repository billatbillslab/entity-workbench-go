package shellcmd

import (
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
)

func seedStatus(t *testing.T, ap *entitysdk.AppPeer, remote crypto.PeerID, status, reason string, failingSince uint64) {
	t.Helper()
	idHash, err := types.ComputePeerIdentityHashFromPeerID(remote)
	if err != nil {
		t.Fatal(err)
	}
	d := types.PeerStatusData{
		PeerID:       string(remote),
		Status:       status,
		Reason:       reason,
		FailingSince: failingSince,
	}
	path := types.TypePeerStatus + "/" + types.PeerIdentityHashHex(idHash)
	if _, err := ap.Store().Put(path, types.TypePeerStatus, d); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// TestPeerStatus_EmptySaysNothingHasHappened pins the wording, because
// the blank-table version of this is actively misleading: "no rows" is
// "nothing has ever transitioned", not "nothing is connected."
func TestPeerStatus_EmptySaysNothingHasHappened(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()
	sh := NewShell(ap, "local", "")

	res, err := cmdPeer(sh, []string{"status"})
	if err != nil {
		t.Fatalf("peer status: %v", err)
	}
	out := strings.Join(res.Lines, "\n") + res.Message
	if !strings.Contains(out, "no peer lifecycle recorded") {
		t.Errorf("empty output = %q, want the explicit 'nothing has happened' message", out)
	}
}

// TestPeerStatus_RendersTheTreeRecordNotThePool is the load-bearing
// one. It seeds a peer the shell has NO alias for and never dialed, and
// asserts it shows up — because that is the whole reason this verb
// exists next to `peer ls`. A peer that connected to us, or one we
// released, lives in the tree record and nowhere else.
func TestPeerStatus_RendersTheTreeRecordNotThePool(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	defer ap.Close()
	sh := NewShell(ap, "local", "")

	stranger, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	suspect, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	seedStatus(t, ap, stranger.PeerID(), types.PeerStatusConnected, "", 0)
	seedStatus(t, ap, suspect.PeerID(), types.PeerStatusSuspect, types.PeerStatusReasonTransportError, 1)

	res, err := cmdPeer(sh, []string{"status"})
	if err != nil {
		t.Fatalf("peer status: %v", err)
	}
	out := strings.Join(res.Lines, "\n")

	for _, want := range []string{
		string(stranger.PeerID())[:14],
		string(suspect.PeerID())[:14],
		"connected",
		"suspect",
		types.PeerStatusReasonTransportError,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// `peer ls` must NOT show them — the two verbs answer different
	// questions and the test says so rather than assuming it.
	lsRes, err := cmdPeer(sh, []string{"ls"})
	if err != nil {
		t.Fatalf("peer ls: %v", err)
	}
	lsOut := strings.Join(lsRes.Lines, "\n")
	if strings.Contains(lsOut, string(stranger.PeerID())[:14]) {
		t.Errorf("peer ls listed a peer this session never dialed:\n%s", lsOut)
	}

	// Failing peers sort first — an operator scanning the table is
	// looking for the broken ones.
	rows := res.Lines[1:] // skip header
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if !strings.Contains(rows[0], "suspect") {
		t.Errorf("first row is not the failing peer:\n%s", strings.Join(rows, "\n"))
	}
}

// TestHumanizeSince_NegativeIsSkewNotAge pins the one arithmetic edge
// that would otherwise print a negative duration at an operator: a
// stamp from the future means a clock disagreement, and saying so beats
// rendering "-3m" as though time ran backwards.
func TestHumanizeSince_NegativeIsSkewNotAge(t *testing.T) {
	future := uint64(9999999999999) // well past now, in ms
	if got := humanizeSince(future); got != "(clock skew)" {
		t.Errorf("humanizeSince(future) = %q, want (clock skew)", got)
	}
	if got := humanizeSince(0); got != "-" {
		t.Errorf("humanizeSince(0) = %q, want -", got)
	}
}
