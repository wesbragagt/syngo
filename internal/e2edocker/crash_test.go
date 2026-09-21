package e2edocker

import (
	"encoding/json"
	"strings"
	"testing"
)

// crashFileSize is big enough that a daemon killed while it is being
// transferred is killed in the middle of it.
const crashFileSize = 64 << 20

// minChunksBeforeKill is how much of the file has to be verified before the
// kill, so the resume has something to keep.
const minChunksBeforeKill = 10

// transferPair brings up a send-only device and a receive-only one, which is
// the shape that makes a transfer easy to reason about: everything moves from
// A to B and nothing comes back.
func transferPair(t *testing.T) *Cluster {
	t.Helper()
	return StartWith(t, ClusterOptions{Devices: []DeviceSpec{
		{Name: "a", Mode: "sendonly"},
		{Name: "b", Mode: "receiveonly"},
	}})
}

// bigFileSize is the size the crash tests move, reduced for a short run.
func bigFileSize(t *testing.T) int {
	t.Helper()
	if testing.Short() {
		return crashFileSize / 4
	}
	return crashFileSize
}

// waitMidTransfer waits until the device is in the middle of one download:
// some chunks verified, not all of them. It returns the journal row.
func waitMidTransfer(c *Cluster, dev *Device, path string) transferRow {
	c.T.Helper()
	var row transferRow
	waitFor(c.T, dev.Name+" is in the middle of "+path, convergeTimeout, func() bool {
		for _, candidate := range dev.Transfers() {
			if candidate.Path != path {
				continue
			}
			if candidate.ChunksDone >= minChunksBeforeKill && candidate.ChunksDone < candidate.ChunksTotal {
				row = candidate
				return true
			}
		}
		return false
	})
	return row
}

// resumedChunks is what the daemon said it kept when it picked its unfinished
// transfers up again, after the given position in the log. The sender does not
// log the chunks it serves, so this is the only place the daemon says how much
// of a transfer survived a crash.
func resumedChunks(dev *Device, from int) []int {
	dev.c.T.Helper()
	log := dev.Logs()
	if len(log) <= from {
		return nil
	}
	var done []int
	for _, raw := range strings.Split(log[from:], "\n") {
		var line slogLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			continue
		}
		if line.Msg == "transfer picked up again" {
			done = append(done, line.ChunksDone)
		}
	}
	return done
}

// TestCrash_ReceiverKilledMidTransfer kills the daemon that is downloading a
// file, with no chance to clean up, and starts it again. The chunks it had
// already verified must still count: a crash may cost the transfer its
// progress in memory, never the bytes on disk.
func TestCrash_ReceiverKilledMidTransfer(t *testing.T) {
	c := transferPair(t)
	a, b := c.Device(0), c.Device(1)

	a.WriteFile("huge.bin", payload(bigFileSize(t), 17))
	interrupted := waitMidTransfer(c, b, "huge.bin")

	from := b.LogSize()
	b.KillDaemon()

	// The journal is readable without a daemon, which is how a crashed
	// device reports what it was doing.
	if report := b.Status(); report.Daemon.Running {
		t.Fatal("b reports a running daemon after its daemon was killed")
	}
	recorded := transferRow{}
	for _, row := range b.Transfers() {
		if row.ID == interrupted.ID {
			recorded = row
		}
	}
	if recorded.ID == "" {
		t.Fatalf("the killed transfer %s is not in b's journal: %+v", interrupted.ID, b.Transfers())
	}
	if recorded.ChunksDone < minChunksBeforeKill || recorded.ChunksDone >= recorded.ChunksTotal {
		t.Fatalf("b recorded %d of %d chunks, want a transfer that is neither empty nor finished",
			recorded.ChunksDone, recorded.ChunksTotal)
	}
	if parts := b.PartFiles(); len(parts) != 1 {
		t.Fatalf("b holds %v partial files during the transfer, want exactly one", parts)
	}

	b.StartDaemon()

	// The daemon says how much of the transfer it kept as it picks it up.
	waitFor(t, "b picked its transfer up again", daemonTimeout, func() bool {
		return len(resumedChunks(b, from)) > 0
	})
	for _, done := range resumedChunks(b, from) {
		if done < recorded.ChunksDone {
			t.Errorf("b resumed with %d chunks, below the %d it had verified: it is downloading them again",
				done, recorded.ChunksDone)
		}
	}

	c.WaitConverged()
	c.CheckInvariants()

	if parts := b.PartFiles(); len(parts) != 0 {
		t.Errorf("b left partial files behind: %v", parts)
	}
	if got, want := b.Tree()["huge.bin"].Hash, a.Tree()["huge.bin"].Hash; got != want {
		t.Errorf("b holds huge.bin as %s, want the sender's %s", got, want)
	}
}

// TestCrash_SenderKilledMidTransfer kills the daemon that is serving a file.
// What the receiver has is still the sender's own version, so nothing about it
// is a conflict, and the transfer has to continue once the sender is back
// rather than start over or be written off.
func TestCrash_SenderKilledMidTransfer(t *testing.T) {
	c := transferPair(t)
	a, b := c.Device(0), c.Device(1)

	a.WriteFile("huge.bin", payload(bigFileSize(t), 23))
	interrupted := waitMidTransfer(c, b, "huge.bin")

	a.KillDaemon()
	a.StartDaemon()

	// The receiver keeps the transfer it started: a peer that went away is
	// not a reason to give up on the file.
	waitFor(t, "b is downloading again", convergeTimeout, func() bool {
		for _, row := range b.Transfers() {
			if row.ID == interrupted.ID {
				return row.State == "active" || row.State == "pending" || row.State == "verifying"
			}
		}
		// The transfer finished, which is the same thing arriving later.
		return len(b.Transfers()) == 0
	})

	c.WaitConverged()
	c.CheckInvariants()

	if failed := b.Status().Transfers.Failed; failed != 0 {
		t.Errorf("b has %d failed transfers after the sender came back, want none", failed)
	}
	for _, dev := range c.Devices {
		if rows := dev.Conflicts(); len(rows) != 0 {
			t.Errorf("%s called the sender's own version a conflict: %+v", dev.Name, rows)
		}
	}
	if got, want := b.Tree()["huge.bin"].Hash, a.Tree()["huge.bin"].Hash; got != want {
		t.Errorf("b holds huge.bin as %s, want the sender's %s", got, want)
	}
}
