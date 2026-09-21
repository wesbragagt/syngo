package e2e

import (
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// waitConnected waits until a node reports the peer it knows as connected.
func waitConnected(t *testing.T, n *node) {
	t.Helper()
	waitFor(t, "a connected peer on "+n.name, convergeTimeout, func() bool {
		report := n.Status()
		return len(report.Peers) == 1 && report.Peers[0].Connected
	})
}

// cliOK runs a CLI command against a node's data directory and fails the test
// when it does not succeed.
func cliOK(t *testing.T, n *node, args ...string) string {
	t.Helper()
	code, stdout := runCLI(t, n.DataDir, args...)
	if code != 0 {
		t.Fatalf("%v on %s exited %d:\n%s", args, n.name, code, stdout)
	}
	return stdout
}

func TestReload_FolderAddedAfterPairingIsSynchronized(t *testing.T) {
	// Neither node has a folder yet, which is what pairing before
	// configuring looks like.
	a := startNodeWith(t, "a", nodeConfig{NoFolder: true})
	b := startNodeWith(t, "b", nodeConfig{NoFolder: true})
	pair(t, a, b)
	waitConnected(t, a)
	waitConnected(t, b)

	// The folder is added through the CLI on both sides, with the daemons
	// running and already connected. Neither is restarted.
	cliOK(t, a, "folder", "add", a.Root, "--id", string(folderID))
	cliOK(t, b, "folder", "add", b.Root, "--id", string(folderID))

	writeFile(t, a, "greeting.txt", []byte("hello from a"))
	waitTreeEqual(t, a, b)
}

func TestReload_ModeChangeAppliesToALiveConnection(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	pair(t, a, b)

	// The file arriving on b proves the connection is up and that a knows
	// b's folder as receive-only, so it never asks b for anything.
	writeFile(t, a, "from-a.txt", payload(2000, 1))
	waitPresent(t, b, "from-a.txt")

	cliOK(t, b, "folder", "set", string(folderID), "--mode", string(folder.ModeSendReceive))
	writeFile(t, b, "from-b.txt", payload(3000, 2))

	// a learns the new mode over the connection it already has.
	waitPresent(t, a, "from-b.txt")
}

func TestReload_PeerAddedWhileTheDaemonsRunConnects(t *testing.T) {
	// An hour of rescan interval on b: only the reload that `peer add`
	// asks for can make it dial a.
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeSendReceive, RescanInterval: time.Hour})

	// Only a knows b, so nothing is connected yet: b refuses a as an
	// unknown device.
	saveDevice(t, a, b)
	writeFile(t, a, "from-a.txt", payload(2500, 3))

	cliOK(t, b, "peer", "add", a.Listen, "--yes")

	waitConnected(t, b)
	waitPresent(t, b, "from-a.txt")
}
