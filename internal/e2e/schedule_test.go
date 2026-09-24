package e2e

import (
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/status"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// closedWindowSpec returns a one-minute window six hours from now in local
// time. The test never runs long enough to reach it, so the folder it is set
// on stays outside its window throughout.
func closedWindowSpec() string {
	start := time.Now().Add(6 * time.Hour)
	return start.Format("15:04") + "-" + start.Add(time.Minute).Format("15:04")
}

// folderStatus returns what a node reports about one of its folders.
func folderStatus(t *testing.T, n *node, id string) status.Folder {
	t.Helper()
	for _, f := range n.Status().Folders {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("%s reports no folder %s", n.name, id)
	return status.Folder{}
}

// TestSyncWindow_ClosedFolderScansAndServesButDoesNotPull proves the three
// halves of a sync window: the folder keeps scanning, keeps serving what it
// has, and does not pull until the window opens.
func TestSyncWindow_ClosedFolderScansAndServesButDoesNotPull(t *testing.T) {
	a := startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendReceive, ExtraFolder: true})
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeSendReceive, ExtraFolder: true})
	cliOK(t, b, "folder", "set", string(folderID), "--sync-window", closedWindowSpec())
	pair(t, a, b)

	writeFile(t, a, "x.txt", payload(2000, 21))
	// The second folder has no window, so its file arriving proves that b
	// ran pull cycles against a while docs was closed.
	marker(t, a, b, "y.txt")

	if exists(t, b, "x.txt") {
		t.Fatal("b pulled a file for a folder outside its sync window")
	}
	waitFor(t, "docs to be reported as scheduled on b", convergeTimeout, func() bool {
		f := folderStatus(t, b, string(folderID))
		return f.State == status.StateScheduled && !f.InWindow && f.NextWindow != nil
	})

	// b still serves what it has: a pulls a file written on b.
	writeFile(t, b, "from-b.txt", payload(1500, 22))
	waitPresent(t, a, "from-b.txt")
	// And b still scans: the file written into it is counted, without a
	// pull having brought anything in.
	waitScanned(t, b, folderID, 1)

	// Removing the window reloads the daemon, which syncs at once.
	cliOK(t, b, "folder", "set", string(folderID), "--clear-sync-windows")
	waitTreeEqual(t, a, b)
}
