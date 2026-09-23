package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/tui"
)

// guidedBudget is the wall-clock bound this test holds the guided walk to.
//
// It is a proxy, and the difference matters. The claim Phase 6 makes is that a
// person can pair two devices and synchronize a folder in under five minutes
// with no documentation. No automated test can measure that: it cannot read,
// think, or mistype, and it already knows the answers. What it can measure is
// that the guided walk asks for nothing the screen does not present, that no
// step needs a command typed outside it, and that the whole path from an empty
// data directory to a converged folder completes against real daemons well
// inside a bound a human pace would dwarf. Thirty seconds is that bound: a
// human doing the same nine steps, reading each one, would take minutes, so a
// walk that takes an automated driver half a minute is not the thing that
// would make the five minutes impossible.
//
// What this test does prove without qualification: the walk works end to end,
// every step of it is a documented command, and nothing in it requires
// knowledge that is not on the screen at the time.
var guidedBudget = 30 * time.Second * timeoutScale

// TestTUI_GuidedWalkPairsAndSyncsAFolder drives the guided setup from an
// uninitialized data directory to a folder that has converged with a peer,
// using only what each screen presents.
func TestTUI_GuidedWalkPairsAndSyncsAFolder(t *testing.T) {
	started := time.Now()

	// The other device: a daemon with the same folder id, already running,
	// which is what a second machine looks like from here.
	other := startNode(t, "nas", folder.ModeSendReceive)

	// This device: an empty data directory and an empty folder root. The
	// walk is given nothing else.
	dataDir := t.TempDir()
	root := filepath.Join(t.TempDir(), string(folderID))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create the folder root: %v", err)
	}
	listen := freeLoopbackAddr(t)

	client := tui.Client{Runner: tuiRunner(t), DataDir: dataDir}
	d := newDriver(t, tui.New(tui.Options{Client: client, Poll: 100 * time.Millisecond}))

	// Step 1 and 2: the identity.
	d.waitFrame("the first question", "What is this device called?", daemonTimeout)
	d.snapshot("the first question")
	d.typeLine("laptop")
	d.waitFrame("the listen address question", "Which address should peers connect to?", daemonTimeout)
	d.snapshot("the listen address")
	d.typeLine(listen)

	// Step 3 and 4: the folder.
	d.waitFrame("the folder question", "Which directory should be synchronized?", daemonTimeout)
	d.snapshot("the folder directory")
	d.typeLine(root)
	// The walk proposes an id from the directory name, which is what the
	// other device is already using, so the answer is to accept it.
	d.waitFrame("the folder id question", "What is the folder called?", daemonTimeout)
	if !strings.Contains(d.view(), string(folderID)) {
		t.Fatalf("the walk did not propose the folder id %q:\n%s", folderID, d.view())
	}
	d.snapshot("the folder id")
	d.send(tea.KeyMsg{Type: tea.KeyEnter})

	// Step 5: the walk waits for a daemon and says which command starts
	// one. Start it, the way the screen says to.
	d.waitFrame("the daemon step", "waiting for a daemon", daemonTimeout)
	if !strings.Contains(d.view(), "daemon run") {
		t.Fatalf("the daemon step does not name the command:\n%s", d.view())
	}
	// Test tuning, not a step of the walk: the shipped defaults reconcile
	// every ten minutes and ask a peer what changed every thirty seconds,
	// which the other device in this test does not use either. Nothing a
	// user has to know or do.
	d.snapshot("waiting for a daemon")
	tighten(t, dataDir)
	runDaemonCLI(t, dataDir)

	// Step 6 and 7: the peer, shown before it is trusted.
	d.waitFrame("the peer question", "Which device should this one pair with?", daemonTimeout)
	d.snapshot("the peer address")
	d.typeLine(other.Listen)
	// The other device has not been told about this one yet, so it refuses
	// the handshake. That is where every first pairing starts and the walk
	// says so rather than reporting a failure.
	d.waitFrame("the pairing confirmation", "Add it anyway?", daemonTimeout)
	if !strings.Contains(d.view(), other.Listen) {
		t.Fatalf("the confirmation does not name the address it would pair with:\n%s", d.view())
	}
	d.snapshot("the pairing confirmation")
	d.key('y')

	// Step 8 happens on its own: the folder is granted to the peer.
	// Step 9 waits for the two daemons to connect, which needs the other
	// device to have added this one as well. That is the second half of
	// pairing, and on a real setup it is the same walk run there.
	d.waitFrame("the pairing step", "Run the same setup on the other device", guidedBudget)
	d.snapshot("waiting for the other device to pair back")
	pairBackFromOtherDevice(t, other, client)

	d.waitFrame("the end of the walk", "paired and synchronizing", guidedBudget)

	d.snapshot("the end of the walk")

	// A file put in the folder now travels, which is what "synchronized"
	// has to mean for the walk to have been worth anything.
	writeFileIn(t, root, "greeting.txt", []byte("hello from the guided setup"))
	defer func() {
		if t.Failed() {
			report, _ := client.Status(context.Background())
			t.Logf("this device: %+v", report)
			t.Logf("other device: %+v", other.Status())
		}
	}()
	waitFor(t, "the file to reach the other device", convergeTimeout, func() bool {
		data, err := os.ReadFile(filepath.Join(other.Root, "greeting.txt"))
		return err == nil && string(data) == "hello from the guided setup"
	})

	elapsed := time.Since(started)
	if elapsed > guidedBudget {
		t.Fatalf("the guided walk took %s, over the %s budget", elapsed.Round(time.Millisecond), guidedBudget)
	}
	// The dashboard the walk hands over to, with the device it just paired
	// with and the folder it just created, live.
	d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	d.waitFrame("the dashboard", "DEVICES", daemonTimeout)
	d.waitFrame("the folder on the dashboard", string(folderID), daemonTimeout)
	d.snapshot("the dashboard the walk hands over to")

	t.Logf("guided walk: empty data directory to a converged folder in %s (budget %s)",
		elapsed.Round(time.Millisecond), guidedBudget)
}

// pairBackFromOtherDevice is the other half of pairing: the second device
// trusting the first and sharing the folder with it. On a real setup it is the
// same guided walk run there; here it is done directly, because this test is
// about the walk on one device and not about running two of them at once.
func pairBackFromOtherDevice(t *testing.T, other *node, client tui.Client) {
	t.Helper()
	ctx := context.Background()

	report, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("read this device's status: %v", err)
	}
	if report.Device.ID == "" || report.Daemon.Listen == "" {
		t.Fatalf("this device does not report an identity and an address: %+v", report)
	}

	err = other.App.Store.SaveDevice(ctx, device.Device{
		ID: device.ID(report.Device.ID), Name: report.Device.Name,
		Address: report.Daemon.Listen, Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("the other device could not trust this one: %v", err)
	}
	if err := other.App.Folders.Grant(ctx, folderID, device.ID(report.Device.ID)); err != nil {
		t.Fatalf("the other device could not share the folder: %v", err)
	}
}
