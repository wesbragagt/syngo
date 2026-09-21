package e2edocker

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
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

// containerKills is how often the container is killed while the writer keeps
// going, and writesDuringKills how many files it writes in total.
const (
	containerKills     = 3
	writesDuringKills  = 200
	restartStormRounds = 20
)

// TestCrash_ContainerKilledDuringWrites takes a whole device away in the
// middle of a stream of changes, the way a machine losing power does: no
// signal, no flush, nothing the daemon can do about it. The database has to
// survive it, because the device has to come back and catch up.
func TestCrash_ContainerKilledDuringWrites(t *testing.T) {
	c := Start(t, 3)
	a, b := c.Device(0), c.Device(1)

	writes := writesDuringKills
	if testing.Short() {
		writes = 50
	}
	done := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		defer close(done)
		for i := 0; i < writes; i++ {
			// Every path is written once. A path rewritten while a
			// peer pulls it would run into KF-1 of the plan, which
			// is not what this test is about.
			a.WriteFile(fmt.Sprintf("stream/f%03d.bin", i), payload((i%3)*chunkSize+64, byte(i)))
			time.Sleep(20 * time.Millisecond)
		}
	}()

	for kill := 0; kill < containerKills; kill++ {
		// The kills are spread through the stream rather than all
		// landing at its start, so one of them catches a transfer.
		want := (kill + 1) * writes / 4
		waitFor(t, fmt.Sprintf("b holds more than %d files", want), convergeTimeout, func() bool {
			return len(b.Tree()) > want
		})
		b.KillContainer()
		b.StartDaemon()
	}
	<-done
	writer.Wait()

	c.WaitPeersConnected()
	c.WaitConverged()
	c.CheckInvariants()

	if got := len(b.Tree()); got != writes+1 {
		t.Errorf("b holds %d entries, want %d files and their directory", got, writes)
	}
	// Nothing is left to fix: a dry run that would change something exits
	// with the dry-run code instead of zero.
	if out, code := b.Run("repair", folderID, "--dry-run"); code != 0 {
		t.Errorf("repair --dry-run on b exited %d, want 0: %s", code, out)
	}
}

// TestCrash_RestartStorm restarts devices over and over while the folder keeps
// changing. A daemon that comes back has to claim the data directory its
// predecessor left behind, whether it shut down cleanly or was killed; a start
// that exits 6 means it refused to, believing the dead daemon is still there.
func TestCrash_RestartStorm(t *testing.T) {
	c := Start(t, 3)
	writer := c.Device(0)

	rounds := restartStormRounds
	if testing.Short() {
		rounds = 5
	}
	stop := make(chan struct{})
	var writing sync.WaitGroup
	writing.Add(1)
	go func() {
		defer writing.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			writer.WriteFile(fmt.Sprintf("storm/f%03d.bin", i), payload(chunkSize+i, byte(i)))
			time.Sleep(100 * time.Millisecond)
		}
	}()

	random := rand.New(rand.NewPCG(7, 11))
	for round := 0; round < rounds; round++ {
		dev := c.Device(random.IntN(len(c.Devices)))
		// Every fourth round the daemon is killed instead of asked to
		// stop, so the next start finds a pid file and a socket nobody
		// removed.
		if round%4 == 3 {
			dev.KillDaemon()
			dev.StartDaemon()
		} else {
			dev.Restart()
		}
		time.Sleep(time.Duration(random.IntN(500)) * time.Millisecond)
	}
	close(stop)
	writing.Wait()

	c.WaitPeersConnected()
	c.WaitConverged()
	c.CheckInvariants()

	for _, dev := range c.Devices {
		for i, code := range dev.DaemonExits() {
			if code == 6 {
				t.Errorf("%s: daemon run %d exited 6: it refused to claim a data directory nobody owns",
					dev.Name, i)
			}
		}
	}
}
