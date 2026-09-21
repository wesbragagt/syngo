package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/jsonconfig"
	"github.com/wesbragagt/syncthis/internal/adapters/tcpwire"
	"github.com/wesbragagt/syncthis/internal/app/configbundle"
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

func TestReload_PeerRemovedWhileTheDaemonsRunIsDisconnected(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	pair(t, a, b)

	writeFile(t, a, "from-a.txt", payload(2500, 5))
	waitTreeEqual(t, a, b)
	waitConnected(t, a)
	waitConnected(t, b)

	// b forgets a. The connection a opened towards b is b's to close: a
	// keeps it otherwise, and goes on being served by a peer that no
	// longer synchronizes with it.
	cliOK(t, b, "peer", "remove", string(a.ID), "--yes")

	waitFor(t, "a to see the connection go", convergeTimeout, func() bool {
		report := a.Status()
		return len(report.Peers) == 1 && !report.Peers[0].Connected
	})

	// Nothing written on b after the removal may reach a.
	writeFile(t, b, "after.txt", payload(1500, 6))
	waitScanned(t, b, folderID, 2)

	// a keeps dialling b, and b refuses it now. Waiting for a refusal
	// reported after the file exists is what says a had its chance: a peer
	// that was still served would have pulled the file in the same cycle.
	waitFor(t, "a to be refused by the peer that removed it", convergeTimeout, func() bool {
		report := a.Status()
		if len(report.Peers) != 1 || report.Peers[0].Connected || report.Peers[0].Error == nil {
			return false
		}
		return strings.Contains(*report.Peers[0].Error, tcpwire.ReasonUnknownDevice)
	})
	if exists(t, a, "after.txt") {
		t.Fatal("a pulled a file from the peer that removed it")
	}
}

// TestConfigImport_ReloadsRunningDaemon proves that a configuration bundle
// applied to a running daemon takes effect at once: b starts with nothing
// configured and is given a's folder and a's peer row through `config import`.
func TestConfigImport_ReloadsRunningDaemon(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNodeWith(t, "b", nodeConfig{NoFolder: true, RescanInterval: time.Hour})

	bundle := configbundle.Bundle{
		SchemaVersion: configbundle.SchemaVersion,
		Settings:      bundleSettings(),
		Folders: []configbundle.Folder{{
			ID: string(folderID), Path: b.Root, Mode: string(folder.ModeSendReceive),
			DeletePolicy: string(folder.DeletePropagate), Symlinks: string(folder.SymlinkStore),
			KeepEmptyDirs: true, IgnorePatterns: []string{}, SyncWindows: []string{},
		}},
		Peers: []configbundle.Peer{{
			ID: string(a.ID), Name: "a", Address: a.Listen, Verified: true,
		}},
	}
	file := filepath.Join(t.TempDir(), "bundle.json")
	f, err := os.Create(file)
	if err != nil {
		t.Fatalf("create %s: %v", file, err)
	}
	if err := configbundle.Encode(f, bundle); err != nil {
		t.Fatalf("encode bundle: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", file, err)
	}

	// a has to know b as well; only b's side is configured by the bundle.
	saveDevice(t, a, b)
	cliOK(t, b, "config", "import", file)

	writeFile(t, a, "from-a.txt", payload(2500, 31))
	waitTreeEqual(t, a, b)
}

// bundleSettings are the settings the imported bundle carries: the short
// timings the tests run with, so the daemon that picks them up behaves like
// the ones the harness starts.
func bundleSettings() configbundle.Settings {
	return configbundle.Settings{
		Listen:         "127.0.0.1:0",
		ChunkSize:      testChunkSize,
		MaxFileSize:    jsonconfig.DefaultMaxFileSize,
		RescanInterval: testRescanInterval.String(),
		SyncInterval:   testSyncInterval.String(),
		LogLevel:       "info",
	}
}
