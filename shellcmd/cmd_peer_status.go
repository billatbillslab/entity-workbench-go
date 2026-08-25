package shellcmd

import (
	"fmt"
	"sort"
	"time"

	"entity-workbench-go/entitysdk"
)

// cmdPeerStatus renders the lifecycle view from `system/peer/status`
// (EXTENSION-NETWORK §3.13) — what the tree records about every peer we
// have ever transitioned, not what our connection pool currently holds.
//
// It is deliberately a different answer from `peer ls`, and the two
// disagreeing is information rather than a bug:
//
//   - `peer ls` lists the shell's own alias table — peers this session
//     dialed and named. A peer with no alias is not in it.
//   - `peer status` lists the tree's record. A peer that connected to
//     US, or that we released an hour ago, appears here and nowhere
//     else; and it can say `suspect`, which the pool cannot express at
//     all.
//
// Empty output means no transition has ever been recorded, which is not
// the same as "nothing is connected" — it is "nothing has ever
// happened." Said in the message rather than implied by a blank table.
func cmdPeerStatus(sh *Shell, _ []string) (Result, error) {
	if sh.Local == nil || sh.Local.Peer == nil {
		return MessageResult("(no local peer)"), nil
	}
	rows, err := sh.Local.Peer.PeerLivenessAll()
	if err != nil {
		return Result{}, fmt.Errorf("peer status: %w", err)
	}
	if len(rows) == 0 {
		return MessageResult("(no peer lifecycle recorded — nothing has connected, and nothing has disconnected)"), nil
	}

	// Alias lookup so the operator sees the name they typed, when there
	// is one. Absence of an alias is normal here and is rendered as a
	// dash rather than blank, so the column never reads as truncation.
	aliasOf := make(map[string]string, len(sh.Conns)+1)
	aliasOf[sh.Local.PeerID] = sh.Local.Alias
	for _, pc := range sh.Conns {
		if pc != nil {
			aliasOf[pc.PeerID] = pc.Alias
		}
	}

	lines := []string{
		fmt.Sprintf("%-16s  %-14s  %-13s  %-16s  %s", "ALIAS", "PEER-ID", "STATUS", "REASON", "SINCE"),
	}
	sort.Slice(rows, func(i, j int) bool {
		// Failing peers first — the rows an operator is looking for —
		// then by peer-id for a stable order within each group.
		if rows[i].Failing() != rows[j].Failing() {
			return rows[i].Failing()
		}
		return rows[i].PeerID < rows[j].PeerID
	})
	for _, l := range rows {
		lines = append(lines, formatLivenessRow(l, aliasOf[l.PeerID]))
	}
	return LinesResult(lines), nil
}

// formatLivenessRow renders one status row.
//
// The SINCE column shows `failing_since` for a peer in a failure
// episode and `connected_at` otherwise — never `last_seen`. That is
// deliberate and it is the one thing in this file worth not
// "improving": `system/peer/status` is transition-written (§5.4.1
// MUST), a successful keepalive writes nothing, and `last_seen` is a
// snapshot taken at the transition. Rendering it as "last heard from"
// would tell an operator a live peer had gone quiet for hours, which is
// a freshness claim the protocol explicitly declines to make.
func formatLivenessRow(l entitysdk.PeerLiveness, alias string) string {
	short := l.PeerID
	if len(short) > 14 {
		short = short[:14]
	}
	if alias == "" {
		alias = "-"
	}
	reason := l.Reason
	if reason == "" {
		reason = "-"
	}

	since := "-"
	switch {
	case l.FailingSince != 0:
		since = "failing " + humanizeSince(l.FailingSince)
	case l.ConnectedAt != 0:
		since = "up " + humanizeSince(l.ConnectedAt)
	}
	return fmt.Sprintf("%-16s  %-14s  %-13s  %-16s  %s", alias, short, l.Status, reason, since)
}

// humanizeSince renders an ms-since-epoch stamp as a coarse age. Coarse
// on purpose: these stamps mark transitions, and a precise-looking
// duration invites reading them as liveness.
func humanizeSince(ms uint64) string {
	if ms == 0 {
		return "-"
	}
	d := time.Since(time.UnixMilli(int64(ms)))
	switch {
	case d < 0:
		// A remote peer's clock, or ours, is off. Say so rather than
		// printing a negative age.
		return "(clock skew)"
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
