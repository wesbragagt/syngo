package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syngo/internal/compose"
	"github.com/wesbragagt/syngo/internal/domain/conflict"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
	"github.com/wesbragagt/syngo/internal/domain/syncplan"
)

// TestRestore_AnEditAfterAnOldDatabaseIsRestoredIsNotLost covers the counter
// floor. A database restored from a backup holds counters the peers have long
// passed, so without a floor the first edit made after the restore would be
// dominated by the version the peer already has and silently overwritten. The
// first contact after the restore raises the floor above what the peer holds,
// so the edit wins instead.
func TestRestore_AnEditAfterAnOldDatabaseIsRestoredIsNotLost(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, b, "shared.txt", []byte("the first version, written on b"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	// The backup is taken with the daemon stopped, which is the procedure
	// STATE.md documents.
	b.Stop()
	backup := filepath.Join(t.TempDir(), "state.db")
	copyFile(t, filepath.Join(b.DataDir, compose.DatabaseFile), backup)
	b.Restart()

	// Two more versions of the same path travel to a, so a holds counters
	// of b the backup knows nothing about.
	writeFile(t, b, "shared.txt", []byte("the second version, written on b"))
	waitFileContent(t, a, "shared.txt", "the second version, written on b")
	writeFile(t, b, "shared.txt", []byte("the third version, written on b"))
	waitFileContent(t, a, "shared.txt", "the third version, written on b")

	// The restore: b is stopped, its database is replaced by the backup,
	// and it comes back and reconnects.
	b.Stop()
	copyFile(t, backup, filepath.Join(b.DataDir, compose.DatabaseFile))
	b.Restart()
	waitFor(t, "b to connect to a again", convergeTimeout, func() bool {
		peers := b.Status().Peers
		return len(peers) == 1 && peers[0].Connected
	})
	// The restore replaced the database, not the folder, so the file on
	// disk already holds the third version and waiting for its content
	// would say nothing about whether b has spoken to a yet. What proves
	// the first contact happened is b's record of the path: the restored
	// database describes the first version, and only a sync cycle against
	// a can put a's version of the path into it. b's own rescan cannot,
	// because the counters it hands out come from the restored database.
	waitVersionAdopted(t, b, a, "shared.txt")

	// The first contact lifted b's counter floor above every counter of its
	// own that a holds, so the next version b writes cannot be one a
	// already knows. Without it the floor would still be at what the
	// backup recorded.
	floor, err := b.App.Store.CounterFloor(controlCtx(t), b.ID)
	if err != nil {
		t.Fatalf("counter floor of b: %v", err)
	}
	if floor < 3 {
		t.Fatalf("counter floor of b = %d, want at least the 3 versions a holds", floor)
	}

	// The edit made after the restore reaches a rather than being
	// overwritten by what a already holds.
	writeFile(t, b, "shared.txt", []byte("the version written after the restore"))
	waitFileContent(t, a, "shared.txt", "the version written after the restore")
	waitTreeEqual(t, b, a)
}

// TestRestore_AnEditMadeBeforeTheFirstContactIsNotLost covers the window the
// counter floor alone cannot close. The floor is learned from a peer on first
// contact, so between putting an old database back and reaching anyone, b
// hands out counters from whatever the backup recorded. An edit made in that
// window gets a counter a already holds for b, so a's version of the path
// appears to descend from the edit when in fact the two were written
// independently. Following a would throw the edit away without a word.
//
// Both versions are kept instead: the edit ends up under a conflict name, on
// both devices, and `conflict list` names it. An extra conflict the user
// resolves by hand beats an automatic resolution that loses data.
func TestRestore_AnEditMadeBeforeTheFirstContactIsNotLost(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, b, "shared.txt", []byte("the first version, written on b"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	// The backup, taken with the daemon stopped as STATE.md documents. It
	// records b's counter of shared.txt at 1.
	b.Stop()
	backup := filepath.Join(t.TempDir(), "state.db")
	copyFile(t, filepath.Join(b.DataDir, compose.DatabaseFile), backup)
	b.Restart()

	// Two more versions travel to a, so a holds counters of b the backup
	// knows nothing about.
	const peerVersion = "the third version, written on b"
	writeFile(t, b, "shared.txt", []byte("the second version, written on b"))
	waitFileContent(t, a, "shared.txt", "the second version, written on b")
	writeFile(t, b, "shared.txt", []byte(peerVersion))
	waitFileContent(t, a, "shared.txt", peerVersion)
	// The content being on a's disk is not the same as a having recorded
	// it: a daemon stopped between installing a file and writing its row
	// records the file as its own local creation at its next start, which
	// would give a a vector it never had and take the test off the case it
	// is about. Waiting for the transfer to be gone is waiting for the row.
	waitConverged(t, b, a)

	// a goes away, so nothing can confirm b's floor while the window is
	// open. It comes back further down still holding the third version.
	a.Stop()

	// The restore, and the edit made before b has spoken to anyone. Both
	// happen with b stopped, so the scan b runs at its next start is the
	// one that records the edit, and it records it before any peer could
	// have been reached: there is no window left for a sync cycle to slip
	// into and make the test depend on which ran first.
	const editAfterRestore = "the edit made in the window"
	b.Stop()
	copyFile(t, backup, filepath.Join(b.DataDir, compose.DatabaseFile))
	writeFile(t, b, "shared.txt", []byte(editAfterRestore))
	b.Restart()
	waitFor(t, "b to record the edit made before its first contact", convergeTimeout, func() bool {
		m, err := b.App.Store.LoadManifest(controlCtx(t), folderID)
		if err != nil {
			return false
		}
		e := m.Entries["shared.txt"]
		return e.Provisional && e.Version.Counter(b.ID) > 1
	})

	a.Restart()
	waitFor(t, "b to connect to a again", convergeTimeout, func() bool {
		peers := b.Status().Peers
		return len(peers) == 1 && peers[0].Connected
	})
	waitConverged(t, a, b)

	// Neither version is gone, on either device.
	for _, n := range []*node{a, b} {
		files := contentsOf(t, n)
		if !holds(files, editAfterRestore) {
			t.Fatalf("%s holds %v, want the edit made before the first contact", n.name, files)
		}
		if !holds(files, peerVersion) {
			t.Fatalf("%s holds %v, want the version a kept as well", n.name, files)
		}
		if files["shared.txt"] != peerVersion {
			t.Fatalf("shared.txt on %s = %q, want the peer's version at the path",
				n.name, files["shared.txt"])
		}
		if copies := copiesIn(files); len(copies) != 1 {
			t.Fatalf("%s has conflict copies %v, want exactly one holding the edit",
				n.name, copies)
		}
	}

	// b is the only device that can see the divergence, so b is where the
	// user is told about it.
	rows := conflictsOf(t, b)
	if len(rows) != 1 || rows[0].Reason != syncplan.ReasonUnverifiedFloor {
		t.Fatalf("b reports %+v, want one %q conflict", rows, syncplan.ReasonUnverifiedFloor)
	}
	if rows[0].Path != "shared.txt" || !conflict.IsCopy(rows[0].ConflictPath) {
		t.Fatalf("conflict row = %+v, want shared.txt with the edit at a conflict name", rows[0])
	}
}

// copyFile copies a file, replacing the destination.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", to, err)
	}
}

// waitVersionAdopted waits until the version n records for a path is no longer
// behind the one from holds for it, which happens when n has run a sync cycle
// against from: the plan merges the peer's vector into the local record, and
// nothing else on n can produce the peer's counters. It is a synchronization
// point, not an assertion about the counter floor: the floor is a separate
// per-device record that only adoptCounters writes.
func waitVersionAdopted(t *testing.T, n, from *node, rel string) {
	t.Helper()
	want, err := from.App.Store.LoadManifest(controlCtx(t), folderID)
	if err != nil {
		t.Fatalf("manifest of %s: %v", from.name, err)
	}
	waitFor(t, n.name+" to take the record of "+rel+" from "+from.name, convergeTimeout, func() bool {
		got, err := n.App.Store.LoadManifest(controlCtx(t), folderID)
		if err != nil {
			return false
		}
		return got.Entries[rel].Version.Compare(want.Entries[rel].Version) != manifest.Before
	})
}

// waitFileContent waits until a path inside a node's folder holds want.
func waitFileContent(t *testing.T, n *node, rel, want string) {
	t.Helper()
	waitFor(t, rel+" on "+n.name+" to hold the expected version", convergeTimeout, func() bool {
		data, err := os.ReadFile(filepath.Join(n.Root, rel))
		return err == nil && string(data) == want
	})
}
