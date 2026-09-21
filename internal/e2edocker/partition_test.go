package e2edocker

import (
	"fmt"
	"testing"
)

// TestPartition_EditBothSidesThenHeal cuts one device off from the other two,
// lets all three write while the link is down, and heals it. The two devices
// that stayed together must agree the whole time, and what they and the lone
// device did to the same path must end as a keep-both with nothing lost.
func TestPartition_EditBothSidesThenHeal(t *testing.T) {
	c := Start(t, 3)
	c.requireNetAdmin()
	a, b, cc := c.Device(0), c.Device(1), c.Device(2)

	a.WriteFile("x.txt", payload(2*chunkSize, 1))
	a.WriteFile("y.txt", payload(chunkSize, 2))
	c.WaitConverged()

	// A is alone on one side, B and C on the other.
	c.Partition(0, 1)
	c.Partition(0, 2)

	fromA := payload(3*chunkSize, 11)
	fromB := payload(chunkSize+7, 22)
	fromC := payload(4*chunkSize, 33)
	a.WriteFile("x.txt", fromA)
	b.WriteFile("y.txt", fromB)
	cc.WriteFile("x.txt", fromC)

	// The side that is still linked keeps working while the other is
	// unreachable: B and C agree before anything is healed.
	c.WaitConverged(1, 2)
	if got := b.ReadFile("x.txt"); !equalBytes(got, fromC) {
		t.Fatalf("during the partition b holds %d bytes of x.txt, want c's %d", len(got), len(fromC))
	}
	if got := cc.ReadFile("y.txt"); !equalBytes(got, fromB) {
		t.Fatalf("during the partition c holds %d bytes of y.txt, want b's %d", len(got), len(fromB))
	}

	c.Heal()
	c.WaitConverged()
	c.CheckInvariants()

	// B's edit was the only one to y.txt, so it wins outright.
	for _, dev := range c.Devices {
		if got := dev.ReadFile("y.txt"); !equalBytes(got, fromB) {
			t.Errorf("%s holds %d bytes of y.txt, want b's %d", dev.Name, len(got), len(fromB))
		}
	}

	// x.txt was written on both sides of the partition, so the greater
	// hash keeps the path and the other version survives as a copy.
	winner, loser := fromA, fromC
	if hashOf(fromC) > hashOf(fromA) {
		winner, loser = fromC, fromA
	}
	for _, dev := range c.Devices {
		if got := dev.ReadFile("x.txt"); !equalBytes(got, winner) {
			t.Errorf("%s holds the wrong version of x.txt: %s, want %s",
				dev.Name, hashOf(got), hashOf(winner))
		}
		copies := conflictCopies(dev.Tree())
		if len(copies) != 1 {
			t.Errorf("%s holds %d conflict copies, want exactly one: %v", dev.Name, len(copies), copies)
			continue
		}
		if got := dev.ReadFile(copies[0]); !equalBytes(got, loser) {
			t.Errorf("%s: conflict copy %s holds %s, want the losing version %s",
				dev.Name, copies[0], hashOf(got), hashOf(loser))
		}
	}
}

// offlineRounds is how many rounds of changes the two connected devices make
// while the third one is away.
const offlineRounds = 30

// TestOffline_LongAbsenceCatchesUp takes one device off the network, keeps
// changing the folder on the other two for many rounds, and puts it back. The
// absent device has to catch up on everything at once, deletions included,
// without calling any of it a conflict: it changed nothing itself.
func TestOffline_LongAbsenceCatchesUp(t *testing.T) {
	c := Start(t, 3)
	a, b := c.Device(0), c.Device(1)

	a.WriteFile("a/seed.bin", payload(chunkSize, 1))
	b.WriteFile("b/seed.bin", payload(chunkSize, 2))
	c.WaitConverged()

	c.Isolate(2)

	rounds := offlineRounds
	if testing.Short() {
		rounds = 6
	}
	// live is what the folder should hold at the end: the writers agree on
	// it because each of them only ever touches its own directory.
	live := map[string][]byte{
		"a/seed.bin": payload(chunkSize, 1),
		"b/seed.bin": payload(chunkSize, 2),
	}
	var deleted []string
	for round := 0; round < rounds; round++ {
		writer := c.Device(round % 2)
		dir := writer.Name

		created := fmt.Sprintf("%s/r%02d.bin", dir, round)
		content := payload((round%4)*chunkSize+13, byte(round))
		writer.WriteFile(created, content)
		live[created] = content

		if round > 0 {
			// Modify, delete and rename what the same device wrote
			// two rounds ago, so every kind of change is in flight.
			previous := fmt.Sprintf("%s/r%02d.bin", dir, round-2)
			if _, ok := live[previous]; ok {
				renamed := fmt.Sprintf("%s/kept%02d.bin", dir, round-2)
				writer.Rename(previous, renamed)
				live[renamed] = live[previous]
				delete(live, previous)
				deleted = append(deleted, previous)
			}
			seed := dir + "/seed.bin"
			changed := payload(chunkSize+round, byte(100+round))
			writer.WriteFile(seed, changed)
			live[seed] = changed
		}
		if round >= 4 {
			gone := fmt.Sprintf("%s/kept%02d.bin", dir, round-4)
			if _, ok := live[gone]; ok {
				writer.Remove(gone)
				delete(live, gone)
				deleted = append(deleted, gone)
			}
		}
		c.WaitConverged(0, 1)
	}

	c.Rejoin(2)
	c.WaitPeersConnected()
	c.WaitConverged()
	c.CheckInvariants()

	absent := c.Device(2)
	for path, want := range live {
		got := absent.ReadFile(path)
		if !equalBytes(got, want) {
			t.Errorf("c holds %s as %s, want %s", path, hashOf(got), hashOf(want))
		}
	}
	for _, path := range deleted {
		if _, ok := live[path]; ok {
			continue
		}
		if _, ok := absent.Tree()[path]; ok {
			t.Errorf("c still holds %s, which was deleted while it was away", path)
		}
	}
	for _, dev := range c.Devices {
		if rows := dev.Conflicts(); len(rows) != 0 {
			t.Errorf("%s recorded %d conflicts, want none: %+v", dev.Name, len(rows), rows)
		}
	}
}
