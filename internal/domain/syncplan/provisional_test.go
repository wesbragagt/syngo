package syncplan_test

import (
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/syncplan"
)

// provisional marks an entry as written by dev from a counter floor no peer
// had confirmed, which is what a version written between a restore from a
// backup and the first contact carries.
func provisional(e manifest.Entry, dev device.ID) manifest.Entry {
	e.Provisional = true
	e.ModifiedBy = dev
	return e
}

// TestPlan_ProvisionalVersions covers the restore window: the counter floor a
// restored database hands out is the one the backup recorded, which the peers
// have long passed, so an edit made before the first contact carries a counter
// a peer may already hold. The peer's version then looks like a descendant of
// the edit when it was written independently.
//
// The plan keeps both in exactly that case and in no other, which is what the
// two halves of the table check: a peer holding a higher counter of the
// writing device than the local version carries is a database missing a write,
// and anything else is ordinary traffic that must still resolve cleanly.
func TestPlan_ProvisionalVersions(t *testing.T) {
	tests := []struct {
		name   string
		local  manifest.Manifest
		remote manifest.Manifest
		action syncplan.Action
		reason string
	}{
		{
			name: "a peer counter above the provisional one is a conflict",
			// B restored a database recording B:1, edited the file
			// before reaching anyone and handed out B:2. A holds
			// B:3, which B wrote before the restore and lost. The
			// two versions were written independently.
			local:  manifestOf(provisional(file("a.txt", "after the restore", vec("B:2")), "B")),
			remote: manifestOf(file("a.txt", "the third version", vec("B:3"))),
			action: syncplan.ActionConflict, reason: syncplan.ReasonUnverifiedFloor,
		},
		{
			name:   "a deletion above the provisional counter is a conflict",
			local:  manifestOf(provisional(file("a.txt", "after the restore", vec("B:2")), "B")),
			remote: manifestOf(tombstone("a.txt", vec("B:3"))),
			action: syncplan.ActionConflict, reason: syncplan.ReasonUnverifiedFloor,
		},
		{
			name: "a kind change above the provisional counter is a conflict",
			// ReplaceVsModify rather than the floor reason: Plan
			// keys the blocking of the path's children on it.
			local:  manifestOf(provisional(file("a.txt", "after the restore", vec("B:2")), "B")),
			remote: manifestOf(dirAt("a.txt", vec("B:3"))),
			action: syncplan.ActionConflict, reason: syncplan.ReasonReplaceVsModify,
		},
		{
			name: "a peer that built on the provisional version is pulled",
			// The everyday case, and the one that must not become a
			// conflict: B wrote the file, A took it unchanged and
			// edited it. A holds B:1, exactly what B has, so nothing
			// of B's is missing here.
			local:  manifestOf(provisional(file("a.txt", "written on b", vec("B:1")), "B")),
			remote: manifestOf(file("a.txt", "edited on a", vec("A:1,B:1"))),
			action: syncplan.ActionPull, reason: syncplan.ReasonRemoteNewer,
		},
		{
			name:   "a peer that built on a provisional deletion deletes",
			local:  manifestOf(provisional(file("a.txt", "written on b", vec("B:1")), "B")),
			remote: manifestOf(tombstone("a.txt", vec("A:1,B:1"))),
			action: syncplan.ActionDelete, reason: syncplan.ReasonRemoteDeleted,
		},
		{
			name: "a provisional version that dominates still wins",
			// An ordinary edit made while the daemon was down is
			// provisional too, because nothing has confirmed the
			// floor yet at the start that records it. It dominates,
			// so it is not touched.
			local:  manifestOf(provisional(file("a.txt", "edited offline", vec("B:6")), "B")),
			remote: manifestOf(file("a.txt", "what the peer has", vec("B:5"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name: "a stale entry nobody edited after the restore is pulled",
			// The restore brought back every entry, not only the
			// edited one. Only the versions written after it are
			// provisional, and following the peer for the rest
			// loses nothing.
			local:  manifestOf(file("a.txt", "the first version", vec("B:1"))),
			remote: manifestOf(file("a.txt", "the third version", vec("B:3"))),
			action: syncplan.ActionPull, reason: syncplan.ReasonRemoteNewer,
		},
		{
			name:   "a provisional version with the same content is in sync",
			local:  manifestOf(provisional(file("a.txt", "same", vec("B:2")), "B")),
			remote: manifestOf(file("a.txt", "same", vec("B:3"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "a provisional version concurrent with the peer's is the usual conflict",
			local:  manifestOf(provisional(file("a.txt", "after the restore", vec("B:2")), "B")),
			remote: manifestOf(file("a.txt", "written on a", vec("A:1"))),
			action: syncplan.ActionConflict, reason: syncplan.ReasonBothModified,
		},
		{
			name: "a provisional version without an author is not acted on",
			// Nothing the scanner writes looks like this. If a row
			// ever did, there is no device whose counters could be
			// compared, so the vectors decide as they always did.
			local: manifestOf(func() manifest.Entry {
				e := file("a.txt", "after the restore", vec("B:2"))
				e.Provisional = true
				return e
			}()),
			remote: manifestOf(file("a.txt", "the third version", vec("B:3"))),
			action: syncplan.ActionPull, reason: syncplan.ReasonRemoteNewer,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := only(t, syncplan.Plan(tc.local, tc.remote, testFolder()))
			if item.Action != tc.action || item.Reason != tc.reason {
				t.Fatalf("got %s(%s), want %s(%s)", item.Action, item.Reason, tc.action, tc.reason)
			}
		})
	}
}
