package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// tuiStatus is the part of `status --json` this test asserts on. It is
// declared here rather than imported from internal/app for the same reason
// internal/tui declares its own: what the test checks is the published
// document.
type tuiStatus struct {
	Device struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"device"`
	Daemon struct {
		Running bool   `json:"running"`
		PID     int    `json:"pid"`
		Listen  string `json:"listen"`
	} `json:"daemon"`
	Folders []struct {
		ID    string `json:"id"`
		Files int64  `json:"files"`
	} `json:"folders"`
	Peers []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"peers"`
	Transfers struct {
		Active  int `json:"active"`
		Pending int `json:"pending"`
	} `json:"transfers"`
}

// decodeStatus reads a status document.
func decodeStatus(t *testing.T, raw string) tuiStatus {
	t.Helper()
	var out tuiStatus
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("read status: %v\n%s", err, raw)
	}
	return out
}

// transferSize is the file the two devices move during the test. It is large
// enough, against the bandwidth limit below, that it is still in flight when
// the TUI is killed, which is the only thing that makes the claim interesting.
const transferSize = 4 << 20

// transferLimit paces the receiving side so the transfer lasts about eight
// seconds. Without it a 4 MiB file over loopback is gone before anything can
// be killed in the middle of it.
const transferLimit = "512KiB"

// TestTUI_KillingItLeavesTheDaemonAndItsTransferAlone is the reattach claim as
// an end-to-end test with real processes.
//
// Two daemons, each its own process. A transfer running between them. The TUI
// a third process, killed with SIGKILL in the middle of it. The daemon must be
// the same process afterwards, the transfer must finish, and a fresh TUI must
// come up showing the same device, the same folder and the same peer.
func TestTUI_KillingItLeavesTheDaemonAndItsTransferAlone(t *testing.T) {
	bin := requireBinary(t)

	// Two devices, set up entirely through the command line, the way the
	// quick start in docs/CLI.md does it.
	dirA, rootA := t.TempDir(), t.TempDir()
	dirB, rootB := t.TempDir(), t.TempDir()
	listenA, listenB := freeLoopbackAddr(t), freeLoopbackAddr(t)

	syngo(t, bin, dirA, "init", "--name", "laptop", "--listen", listenA)
	syngo(t, bin, dirB, "init", "--name", "nas", "--listen", listenB)
	syngo(t, bin, dirA, tighteningArgs()...)
	syngo(t, bin, dirB, tighteningArgs()...)
	// Only the receiving side is paced, so the file takes long enough to
	// be interrupted in the middle of.
	syngo(t, bin, dirA, "config", "set", "--download-limit", transferLimit)
	syngo(t, bin, dirA, "folder", "add", rootA, "--id", string(folderID))
	syngo(t, bin, dirB, "folder", "add", rootB, "--id", string(folderID))

	logA := filepath.Join(dirA, "daemon.log")
	daemonA := startDaemonProcess(t, bin, dirA, logA)
	daemonB := startDaemonProcess(t, bin, dirB, filepath.Join(dirB, "daemon.log"))

	reportA := waitDaemon(t, bin, dirA)
	reportB := waitDaemon(t, bin, dirB)

	syngo(t, bin, dirA, "peer", "add", reportB.Daemon.Listen, "--yes")
	syngo(t, bin, dirB, "peer", "add", reportA.Daemon.Listen, "--yes")
	syngo(t, bin, dirA, "folder", "grant", string(folderID), reportB.Device.ID)
	syngo(t, bin, dirB, "folder", "grant", string(folderID), reportA.Device.ID)

	// The file that will be in flight.
	want := payload(transferSize, 7)
	writeFileIn(t, rootB, "big.bin", want)

	// The TUI, a third process, drawing on a pipe rather than a terminal:
	// that is what running under tmux or over SSH amounts to from here.
	tuiOne := start(t, bin, os.Stderr,
		"--data-dir", dirA, "tui", "--poll", "200ms", "--log-file", logA)
	tuiOne.waitOutput("the dashboard", "DEVICES", daemonTimeout)
	tuiOne.waitOutput("the folder", string(folderID), daemonTimeout)
	tuiOne.waitOutput("the peer", "nas", daemonTimeout)
	tuiOne.snapshot("the first TUI, before it is killed")

	// Kill it in the middle of the transfer. The file must not have
	// arrived yet, or the test would be killing the TUI after the
	// interesting part.
	waitFor(t, "the transfer to start", convergeTimeout, func() bool {
		report := tuiStatusOf(t, bin, dirA)
		return report.Transfers.Active+report.Transfers.Pending > 0
	})
	if arrived(rootA, "big.bin", want) {
		t.Fatal("the file arrived before the TUI could be killed; lower transferLimit")
	}

	daemonPIDBefore := tuiStatusOf(t, bin, dirA).Daemon.PID
	if daemonPIDBefore != daemonA.pid() {
		t.Fatalf("the daemon reports pid %d, but the process is %d", daemonPIDBefore, daemonA.pid())
	}
	tuiPID := tuiOne.pid()
	tuiOne.kill(syscall.SIGKILL)

	waitFor(t, "the TUI process to be gone", daemonTimeout, func() bool { return !tuiOne.alive() })
	t.Logf("killed the TUI (pid %d) with SIGKILL while the transfer was running", tuiPID)

	// The daemon is untouched: still running, still the same process.
	if !daemonA.alive() {
		t.Fatal("killing the TUI killed the daemon")
	}
	if !daemonB.alive() {
		t.Fatal("killing the TUI killed the other device's daemon")
	}

	// And the transfer finishes without it.
	waitFor(t, "the transfer to finish without the TUI", convergeTimeout, func() bool {
		return arrived(rootA, "big.bin", want)
	})

	after := tuiStatusOf(t, bin, dirA)
	if !after.Daemon.Running {
		t.Fatal("the daemon stopped answering after the TUI was killed")
	}
	if after.Daemon.PID != daemonPIDBefore {
		t.Fatalf("the daemon was restarted: pid %d, was %d", after.Daemon.PID, daemonPIDBefore)
	}
	t.Logf("the daemon is the same process (pid %d) and the %d byte transfer completed",
		after.Daemon.PID, transferSize)

	// A fresh TUI reattaches to the same device, with nothing carried over
	// from the one that was killed.
	tuiTwo := start(t, bin, os.Stderr,
		"--data-dir", dirA, "tui", "--poll", "200ms", "--log-file", logA)
	tuiTwo.waitOutput("the dashboard of the second TUI", "DEVICES", daemonTimeout)

	frames := tuiTwo.frames()
	for what, want := range map[string]string{
		"the device name":  after.Device.Name,
		"the folder":       string(folderID),
		"the peer":         "nas",
		"the peer's state": "connected",
		"the daemon's pid": sprintfInt(after.Daemon.PID),
	} {
		if !strings.Contains(frames, want) {
			t.Errorf("the second TUI does not show %s (%q):\n%s", what, want, lastFrame(frames))
		}
	}
	tuiTwo.snapshot("the second TUI, reattached to the same daemon")

	// The log view of the reattached TUI tails the daemon's own log file,
	// which is the record the killed process could not have carried over.
	tuiTwo.press("l")
	tuiTwo.waitOutput("the log view", "tailing "+logA[:20], daemonTimeout)
	tuiTwo.waitOutput("a line of the daemon's own log", "peer connected", daemonTimeout)
	tuiTwo.snapshot("the log view of the reattached TUI")

	if tuiTwo.pid() == tuiPID {
		t.Fatal("the second TUI is the process that was killed")
	}
	t.Logf("a fresh TUI (pid %d) reattached to the same daemon and shows the same device",
		tuiTwo.pid())

	// Nothing was lost: the folder holds the file on both sides.
	if diff := treeDiff(t, rootB, rootA); len(diff) != 0 {
		t.Errorf("the folders do not agree after the TUI was killed:\n%s", join(diff))
	}
}

// arrived reports whether a path holds exactly the wanted content.
func arrived(root, rel string, want []byte) bool {
	got, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // a path this test made
	return err == nil && len(got) == len(want) && string(got) == string(want)
}

// sprintfInt renders an int, for a substring match against a frame.
func sprintfInt(n int) string {
	digits := ""
	if n == 0 {
		return "0"
	}
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// lastFrame is the tail of what a process drew, which is the frame it was
// showing when the test looked.
func lastFrame(frames string) string {
	const keep = 2000
	if len(frames) <= keep {
		return frames
	}
	return "…" + frames[len(frames)-keep:]
}
