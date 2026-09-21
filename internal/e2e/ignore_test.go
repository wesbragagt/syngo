package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// TestIgnore_IncomingFileIsNotInstalled proves the receiver's ignore rules
// apply to what a peer offers. The sender has no such rules and offers both
// files; only the one the receiver does not ignore arrives.
func TestIgnore_IncomingFileIsNotInstalled(t *testing.T) {
	sender := startNode(t, "sender", folder.ModeSendReceive)
	receiver := startNode(t, "receiver", folder.ModeSendReceive)
	writeFileIn(t, receiver.Root, ".syncthisignore", []byte("*.tmp\n"))
	pair(t, sender, receiver)

	writeFile(t, sender, "junk.tmp", payload(2048, 1))
	writeFile(t, sender, "keep.txt", payload(2048, 2))

	waitFor(t, "the file that is not ignored to arrive", convergeTimeout, func() bool {
		_, err := os.Stat(filepath.Join(receiver.Root, "keep.txt"))
		return err == nil
	})
	// The ignored file travelled in the same manifest as the one that
	// arrived, so a cycle that installed it would have done so by now.
	// A further wait covers a cycle that was still running.
	time.Sleep(time.Second * timeoutScale)

	if _, err := os.Stat(filepath.Join(receiver.Root, "junk.tmp")); err == nil {
		t.Fatal("the receiver installed a file its ignore rules cover")
	}
}
