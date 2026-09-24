package e2edocker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/testutil/tree"
)

// TestStar_HubRelaysBetweenSpokes pairs two devices with a hub and not with
// each other. Neither spoke knows the other exists, so everything one of them
// writes reaches the other only because the hub carries it. The spokes bind
// their listener to an interface instead of accepting every address, which is
// the other way the daemon finds an address to announce.
func TestStar_HubRelaysBetweenSpokes(t *testing.T) {
	c := StartWith(t, ClusterOptions{
		Topology: Star(0),
		Devices: []DeviceSpec{
			{Name: "hub"},
			{Name: "b", BindIface: true},
			{Name: "c", BindIface: true},
		},
	})
	hub, b, cc := c.Device(0), c.Device(1), c.Device(2)

	// A spoke bound to an interface announces that interface's address,
	// not a wildcard: a peer has something to dial.
	for _, dev := range []*Device{b, cc} {
		if want := fmt.Sprintf("%s:%d", dev.IP, peerPort); dev.Status().Daemon.Listen != want {
			t.Errorf("%s listens on %q, want %q", dev.Name, dev.Status().Daemon.Listen, want)
		}
		if peers := dev.Status().Peers; len(peers) != 1 || peers[0].ID != hub.ID {
			t.Fatalf("%s knows %d peers, want only the hub", dev.Name, len(peers))
		}
	}

	fromB := payload(3*chunkSize, 11)
	b.WriteFile("from-b.bin", fromB)
	c.WaitConverged()

	fromC := payload(2*chunkSize+7, 22)
	cc.WriteFile("from-c.bin", fromC)
	c.WaitConverged()
	c.CheckInvariants()

	if got := cc.ReadFile("from-b.bin"); !equalBytes(got, fromB) {
		t.Errorf("c holds from-b.bin as %s, want b's %s", hashOf(got), hashOf(fromB))
	}
	if got := b.ReadFile("from-c.bin"); !equalBytes(got, fromC) {
		t.Errorf("b holds from-c.bin as %s, want c's %s", hashOf(got), hashOf(fromC))
	}
}

// TestModes_SendOnlyHubReceiveOnlySpokes is the shape of a machine that
// publishes a folder to devices that only consume it: the hub sends and never
// pulls, the spokes pull and never serve. Everything the hub does has to reach
// both spokes, deletions included, and nothing a spoke does may leave it.
func TestModes_SendOnlyHubReceiveOnlySpokes(t *testing.T) {
	c := StartWith(t, ClusterOptions{
		Topology: Star(0),
		Devices: []DeviceSpec{
			{Name: "hub", Mode: "sendonly", ExtraFolder: true},
			{Name: "b", Mode: "receiveonly", ExtraFolder: true},
			{Name: "c", Mode: "receiveonly", ExtraFolder: true},
		},
	})
	hub, b, cc := c.Device(0), c.Device(1), c.Device(2)

	hub.WriteFile("published/one.bin", payload(2*chunkSize, 1))
	hub.WriteFile("published/two.bin", payload(chunkSize+5, 2))
	c.WaitConverged()

	// The spoke writes a file of its own. The marker proves both the hub
	// and the other spoke ran a cycle involving b afterwards, so the file
	// is missing because a receive-only folder never served it.
	b.WriteFile("local-only.bin", payload(chunkSize, 3))
	c.Marker(1, 0, "cycle-ran-on-the-hub.txt")
	c.Marker(0, 2, "cycle-ran-on-c.txt")
	if hub.Has("local-only.bin") {
		t.Error("the hub pulled a file from a spoke although its folder is send-only")
	}
	if cc.Has("local-only.bin") {
		t.Error("a receive-only spoke served its local file to the other spoke")
	}

	// What the hub deletes leaves the spokes: they take everything the hub
	// says about the folder, including that a file is gone.
	hub.Remove("published/one.bin")
	b.WaitAbsent("published/one.bin")
	cc.WaitAbsent("published/one.bin")

	hubTree := hub.Tree()
	for _, spoke := range []*Device{b, cc} {
		spokeTree := spoke.Tree()
		for path, want := range hubTree {
			got, ok := spokeTree[path]
			if !ok {
				t.Errorf("%s is missing %s, which the hub holds", spoke.Name, path)
				continue
			}
			if got.Hash != want.Hash {
				t.Errorf("%s holds %s as %s, want the hub's %s", spoke.Name, path, got.Hash, want.Hash)
			}
		}
	}
	if !b.Has("local-only.bin") {
		t.Error("b lost the file it wrote itself")
	}
}

// TestModes_DeletePolicyNeverOnOneSpoke gives one spoke a folder that applies
// everything but deletions. The other spoke and the hub agree that a deleted
// file is gone; the spoke that keeps deletions keeps the file, says why, and
// does not push it back at anyone.
func TestModes_DeletePolicyNeverOnOneSpoke(t *testing.T) {
	c := StartWith(t, ClusterOptions{
		Topology: Star(0),
		Devices: []DeviceSpec{
			{Name: "hub", ExtraFolder: true},
			{Name: "b"},
			// The archiving spoke is receive-only as well as
			// delete-never: a folder that keeps what the others
			// deleted and still serves its manifest would offer the
			// file back and undo the deletion everywhere.
			{Name: "c", Mode: "receiveonly", DeletePolicy: "never", ExtraFolder: true},
		},
	})
	hub, b, cc := c.Device(0), c.Device(1), c.Device(2)

	hub.WriteFile("keep.txt", payload(chunkSize, 7))
	hub.WriteFile("other.txt", payload(1000, 8))
	c.WaitConverged()

	hub.Remove("keep.txt")
	b.WaitAbsent("keep.txt")
	// The marker proves c ran a cycle against the hub after the deletion,
	// so the file is still there by policy rather than by being early.
	c.Marker(0, 2, "delete-was-seen.txt")

	if !cc.Has("keep.txt") {
		t.Fatal("the delete policy of c is never, but the file was removed")
	}
	if got, want := cc.ReadFile("keep.txt"), payload(chunkSize, 7); !equalBytes(got, want) {
		t.Errorf("c holds keep.txt as %s, want the version it was given, %s", hashOf(got), hashOf(want))
	}

	// The skipped deletion is a decision the device can explain.
	out, code := cc.Run("sync-plan", folderID)
	if code != 0 {
		t.Errorf("sync-plan on c exited %d, want 0: a skipped delete changes nothing\n%s", code, out)
	}
	if !strings.Contains(out, "keep.txt") || !strings.Contains(out, "delete-policy-never") {
		t.Errorf("sync-plan on c does not name the delete it skipped and why:\n%s", out)
	}

	// The hub and the other spoke are in sync, and c differs from them by
	// the one file it kept and nothing else.
	c.WaitConverged(0, 1)
	problems := tree.Diff(hub.Tree(), cc.Tree())
	if len(problems) != 1 || problems[0] != "unexpected: keep.txt" {
		t.Errorf("c differs from the hub by %v, want only the file it kept", problems)
	}
}

// TestModes_ReadOnlyRootIsReportedNotFatal gives one device a folder it may
// not write. Everything it is offered fails to land, which is a state it has
// to report rather than a reason to stop: the daemon stays up, the other two
// devices carry on, and the folder says what is wrong with it.
func TestModes_ReadOnlyRootIsReportedNotFatal(t *testing.T) {
	c := StartWith(t, ClusterOptions{Devices: []DeviceSpec{
		{Name: "a"},
		{Name: "b"},
		{Name: "c", ReadOnlyRoot: true},
	}})
	a, cc := c.Device(0), c.Device(2)

	for i := 0; i < 5; i++ {
		a.WriteFile(fmt.Sprintf("offered/f%d.bin", i), payload(chunkSize+i, byte(i)))
	}

	// The two writable devices are unaffected by the third.
	c.WaitConverged(0, 1)
	c.CheckInvariants(0, 1)

	// The device that cannot write reports the folder as failing, with the
	// reason, and keeps answering.
	waitFor(t, "c reports its folder as failing", convergeTimeout, func() bool {
		for _, f := range cc.Status().Folders {
			if f.ID == folderID && f.State == "error" && f.Error != nil {
				return true
			}
		}
		return false
	})
	report := cc.Status()
	if !report.Daemon.Running {
		t.Fatal("the daemon of c stopped over a folder it cannot write")
	}
	// The work it could not do is counted, either as journal rows or as
	// the items the folder still owes; a root that refuses even the temp
	// directory leaves no journal row to count.
	pending := 0
	for _, f := range report.Folders {
		if f.ID == folderID {
			pending = f.PendingChanges
		}
	}
	if got := report.Transfers.Failed + report.Transfers.Pending + report.Transfers.Active + pending; got == 0 {
		t.Errorf("c has nothing to show for five files it cannot write: %+v, pending_changes %d",
			report.Transfers, pending)
	}
	if len(cc.Tree()) != 0 {
		t.Errorf("c wrote into a read-only mount: %v", cc.Tree())
	}
}

// TestModes_DifferentUIDs gives every device its own user. Ownership is a
// local matter and modes are not: a file that is executable on one device has
// to arrive executable on the others, whoever owns it there.
func TestModes_DifferentUIDs(t *testing.T) {
	c := StartWith(t, ClusterOptions{Devices: []DeviceSpec{
		{Name: "a", UID: 1000, GID: 1000},
		{Name: "b", UID: 2000, GID: 2000},
		{Name: "c", UID: 3000, GID: 3000},
	}})

	for _, dev := range c.Devices {
		dev.WriteFileAs(dev.Name+"/data.bin", payload(chunkSize+3, byte(dev.Index)), 0o644)
		dev.WriteFileAs(dev.Name+"/run.sh", payload(500, byte(100+dev.Index)), 0o755)
	}

	// The comparison covers the modes as well as the content, so a mode
	// that did not survive the transfer is a failure to converge.
	c.WaitConverged()
	c.CheckInvariants()

	for _, dev := range c.Devices {
		entries := dev.Tree()
		for _, owner := range c.Devices {
			for name, want := range map[string]uint32{"data.bin": 0o644, "run.sh": 0o755} {
				path := owner.Name + "/" + name
				entry, ok := entries[path]
				if !ok {
					t.Errorf("%s is missing %s", dev.Name, path)
					continue
				}
				if uint32(entry.Mode.Perm()) != want {
					t.Errorf("%s holds %s as %o, want %o", dev.Name, path, entry.Mode.Perm(), want)
				}
			}
		}
		for _, line := range dev.LogLines("error") {
			if strings.Contains(strings.ToLower(line.Msg), "chown") ||
				strings.Contains(strings.ToLower(line.Raw), "operation not permitted") {
				t.Errorf("%s failed over ownership: %s", dev.Name, line.Raw)
			}
		}
	}
}
