package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
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
	// The restored database describes the first version, so b pulls a's
	// third version over it before anything else happens.
	waitFileContent(t, b, "shared.txt", "the third version, written on b")

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

// waitFileContent waits until a path inside a node's folder holds want.
func waitFileContent(t *testing.T, n *node, rel, want string) {
	t.Helper()
	waitFor(t, rel+" on "+n.name+" to hold the expected version", convergeTimeout, func() bool {
		data, err := os.ReadFile(filepath.Join(n.Root, rel))
		return err == nil && string(data) == want
	})
}
