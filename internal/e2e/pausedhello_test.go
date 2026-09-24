package e2e

import (
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// TestPause_AConnectionMadeWhilePausedStillPullsAfterResume is D3: a paused
// folder is left out of the Hello, and the peer filters on that list for the
// life of the connection. Resuming has to tell the peer the new list, or the
// folder never syncs over that connection again.
func TestPause_AConnectionMadeWhilePausedStillPullsAfterResume(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "start.txt", []byte("the file both sides start with"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	if err := b.App.Control.Pause(controlCtx(t), folderID); err != nil {
		t.Fatalf("pause b: %v", err)
	}
	// a reconnects while the folder is paused on b, so the Hello it gets
	// lists nothing.
	a.Restart()
	waitFor(t, "a to connect to b again", convergeTimeout, func() bool {
		peers := a.Status().Peers
		return len(peers) == 1 && peers[0].Connected
	})

	if err := b.App.Control.Resume(controlCtx(t), folderID); err != nil {
		t.Fatalf("resume b: %v", err)
	}
	writeFile(t, b, "after-resume.txt", []byte("written on b after it resumed"))

	// Without a restart of either daemon, a pulls the folder again.
	waitPresent(t, a, "after-resume.txt")
}
