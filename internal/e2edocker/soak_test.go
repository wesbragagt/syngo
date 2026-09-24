package e2edocker

import (
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// soakChaos is the set of things the soak does to the cluster between rounds.
// The full set is the default: it rewrites, deletes and renames what earlier
// rounds wrote, has two devices write one path at the same instant, and adds
// a partition and a shaped link to the daemons it kills. The basic set is
// creations and restarts alone, and is asked for with
// SYNGO_E2E_SOAK_CHAOS=basic for a run that needs no NET_ADMIN.
type soakChaos struct {
	full bool
}

// soakSettings is how long the soak runs and how large it is.
type soakSettings struct {
	minutes int
	devices int
	chaos   soakChaos
}

// soakEnv reads the settings, or skips the test when it was not asked for.
func soakEnv(t *testing.T) soakSettings {
	t.Helper()
	raw := os.Getenv("SYNGO_E2E_SOAK_MINUTES")
	if raw == "" {
		t.Skip("the soak runs for as long as SYNGO_E2E_SOAK_MINUTES says; see make e2e-docker-soak")
	}
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes <= 0 {
		t.Fatalf("SYNGO_E2E_SOAK_MINUTES=%q is not a positive number of minutes", raw)
	}
	devices := 3
	if raw := os.Getenv("SYNGO_E2E_SOAK_DEVICES"); raw != "" {
		devices, err = strconv.Atoi(raw)
		if err != nil || devices < 2 {
			t.Fatalf("SYNGO_E2E_SOAK_DEVICES=%q is not a device count of two or more", raw)
		}
	}
	return soakSettings{
		minutes: minutes,
		devices: devices,
		chaos:   soakChaos{full: os.Getenv("SYNGO_E2E_SOAK_CHAOS") != "basic"},
	}
}

// ledger records what was written where, round by round: for every path, the
// content each device put there. It is what the zero-loss check is made of.
type ledger struct {
	// live is the content every device must hold at the end of the round,
	// for the paths only one device wrote.
	live map[string][]byte
	// collisions are the paths more than one device wrote in the round,
	// with every version that was written.
	collisions map[string][][]byte
}

// TestSoak keeps a cluster changing and breaking for as long as it is given,
// and checks after every round that nothing was lost: the devices hold the
// same folder, no download is left half finished, and where two devices wrote
// the same path at the same time one version keeps the path and every other
// one is on every device as a conflict copy.
func TestSoak(t *testing.T) {
	settings := soakEnv(t)
	c := Start(t, settings.devices)
	if settings.chaos.full {
		c.requireNetAdmin()
	}

	deadline := time.Now().Add(time.Duration(settings.minutes) * time.Minute)
	random := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 99))
	// live is the content of every path that exists, carried between
	// rounds so a later round can rewrite or delete what an earlier one
	// wrote.
	live := map[string][]byte{}
	rounds, chaosActions := 0, 0
	// collided are the paths two devices wrote at the same instant, over
	// the whole run.
	var collided []string

	for time.Now().Before(deadline) {
		rounds++
		book := c.soakRound(random, live, settings.chaos)

		if rounds%5 == 0 {
			c.soakChaosAction(random, settings.chaos)
			chaosActions++
		}

		c.WaitPeersConnected()
		c.WaitConverged()
		collided = append(collided, c.checkLedger(rounds, book)...)
		for _, dev := range c.Devices {
			if parts := dev.PartFiles(); len(parts) != 0 {
				t.Fatalf("round %d: %s left partial downloads: %v", rounds, dev.Name, parts)
			}
		}
		t.Logf("round %d done (%d paths live, %d chaos actions, %s left)",
			rounds, len(live), chaosActions, time.Until(deadline).Round(time.Second))
	}

	c.CheckInvariants()
	// Nothing is left to fix anywhere, and every path two devices wrote at
	// once was recorded as a conflict somewhere. A conflict row is the
	// record of a resolution a device made for itself, so the devices do
	// not record the same rows: the device that had a version of its own
	// to move aside records one, and a device that received the winner and
	// the conflict copy as ordinary files has nothing to record. What every
	// device must hold is every version, and that is what checkLedger
	// asserts round by round.
	recorded := map[string]bool{}
	rows := 0
	for _, dev := range c.Devices {
		if out, code := dev.Run("repair", folderID, "--dry-run"); code != 0 {
			t.Errorf("repair --dry-run on %s exited %d, want 0: %s", dev.Name, code, out)
		}
		var found []conflictRow
		dev.RunJSON(&found, "conflict", "list", "--all")
		rows += len(found)
		for _, row := range found {
			recorded[row.Path] = true
		}
	}
	for _, path := range collided {
		if !recorded[path] {
			t.Errorf("no device recorded a conflict for %s, which two devices wrote at once", path)
		}
	}
	t.Logf("soak finished: %d rounds, %d chaos actions, %d collided paths, %d conflict rows across the cluster",
		rounds, chaosActions, len(collided), rows)
}

// soakOp is one change a round makes on one device.
type soakOp struct {
	path    string
	content []byte
	remove  bool
	renamed string
}

// planSoakRound decides what every device changes in one round, against the
// state the round starts from. It returns the ops of every device and the
// ledger of what the round writes, and advances live to the state the round
// reaches.
func planSoakRound(random *rand.Rand, names []string, live map[string][]byte, chaos soakChaos) ([][]soakOp, ledger) {
	book := ledger{live: map[string][]byte{}, collisions: map[string][][]byte{}}

	// Every device owns the directory named after it, so two devices only
	// ever write the same path when the round says so.
	plan := make([][]soakOp, len(names))
	writers := random.Perm(len(names))[:1+random.IntN(len(names))]
	for _, i := range writers {
		owned := ownedPaths(live, names[i])
		for n := 0; n < 1+random.IntN(3); n++ {
			// Only creations are in the basic set: rewriting,
			// deleting or renaming a path a peer may be asking
			// about is the case KF-1 of the plan was about, and
			// the basic set is there for a run that wants none of
			// it.
			action := random.IntN(4)
			if !chaos.full {
				action = 0
			}
			switch {
			case action == 1 && len(owned) > 0:
				// Rewrite something this device already owns.
				// The path stays in owned: a later op of the
				// same round may change it again, and the file
				// is still there for it.
				path := owned[random.IntN(len(owned))]
				content := payload(random.IntN(3)*chunkSize+random.IntN(500)+1, byte(random.IntN(256)))
				plan[i] = append(plan[i], soakOp{path: path, content: content})
				live[path], book.live[path] = content, content
			case action == 2 && len(owned) > 0:
				var path string
				path, owned = takePath(owned, random.IntN(len(owned)))
				plan[i] = append(plan[i], soakOp{path: path, remove: true})
				delete(live, path)
				delete(book.live, path)
			case action == 3 && len(owned) > 0:
				var from string
				from, owned = takePath(owned, random.IntN(len(owned)))
				to := fmt.Sprintf("%s/moved-%d.bin", names[i], random.IntN(1<<20))
				plan[i] = append(plan[i], soakOp{path: from, renamed: to})
				live[to], book.live[to] = live[from], live[from]
				delete(live, from)
				delete(book.live, from)
			default:
				path := fmt.Sprintf("%s/f-%d.bin", names[i], random.IntN(1<<20))
				content := payload(random.IntN(4)*chunkSize+random.IntN(500)+1, byte(random.IntN(256)))
				plan[i] = append(plan[i], soakOp{path: path, content: content})
				live[path], book.live[path] = content, content
			}
		}
	}

	// A tenth of the rounds have two devices write the same path at the
	// same instant, which is the case the zero-loss check is for. It is
	// part of the full chaos set only.
	if chaos.full && len(writers) > 1 && random.IntN(10) == 0 {
		path := fmt.Sprintf("shared/c-%d.bin", random.IntN(1<<20))
		for _, i := range writers[:2] {
			content := payload(chunkSize+random.IntN(500)+1, byte(random.IntN(256)))
			plan[i] = append(plan[i], soakOp{path: path, content: content})
			book.collisions[path] = append(book.collisions[path], content)
		}
		delete(live, path)
	}

	return plan, book
}

// soakRound makes one round of changes, every device at the same instant, and
// returns what was written.
func (c *Cluster) soakRound(random *rand.Rand, live map[string][]byte, chaos soakChaos) ledger {
	c.T.Helper()
	names := make([]string, len(c.Devices))
	for i, dev := range c.Devices {
		names[i] = dev.Name
	}
	plan, book := planSoakRound(random, names, live, chaos)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, dev := range c.Devices {
		if len(plan[i]) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for _, o := range plan[i] {
				switch {
				case o.remove:
					dev.Remove(o.path)
				case o.renamed != "":
					dev.Rename(o.path, o.renamed)
				default:
					dev.WriteFile(o.path, o.content)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	return book
}

// takePath picks the path at an index out of the list of the paths a device
// may still act on, and returns the list without it. A round plans several ops
// at once against the state it saw at the start of the round, so a path a
// planned op deletes or renames away has to leave the list: planning a second
// op against a path that is gone by the time the round runs describes a state
// the round never reaches, and the rename or the delete fails on a source that
// is no longer there.
func takePath(owned []string, idx int) (string, []string) {
	path := owned[idx]
	return path, append(owned[:idx:idx], owned[idx+1:]...)
}

// ownedPaths lists the paths of the folder a device owns, which are the ones
// in the directory named after it.
func ownedPaths(live map[string][]byte, name string) []string {
	var owned []string
	for path := range live {
		if len(path) > len(name) && path[:len(name)+1] == name+"/" {
			owned = append(owned, path)
		}
	}
	// The list is ordered so the same seed picks the same path.
	sort.Strings(owned)
	return owned
}

// soakChaosAction breaks something: a daemon that was killed, a container that
// was killed, a daemon that was restarted, and, in the full set, a partition
// and a shaped link.
func (c *Cluster) soakChaosAction(random *rand.Rand, chaos soakChaos) {
	c.T.Helper()
	actions := 3
	if chaos.full {
		actions = 5
	}
	dev := c.Devices[random.IntN(len(c.Devices))]
	switch random.IntN(actions) {
	case 0:
		c.T.Logf("chaos: killing the daemon of %s", dev.Name)
		dev.KillDaemon()
		dev.StartDaemon()
	case 1:
		c.T.Logf("chaos: killing the container of %s", dev.Name)
		dev.KillContainer()
		dev.StartDaemon()
	case 2:
		c.T.Logf("chaos: restarting %s", dev.Name)
		dev.Restart()
	case 3:
		other := (dev.Index + 1 + random.IntN(len(c.Devices)-1)) % len(c.Devices)
		c.T.Logf("chaos: partitioning %s from %s for ten seconds", dev.Name, c.Devices[other].Name)
		c.Partition(dev.Index, other)
		time.Sleep(10 * time.Second)
		c.Heal()
	default:
		c.T.Logf("chaos: shaping the link of %s for one round", dev.Name)
		c.Netem(dev.Index, "delay 50ms 10ms loss 1%")
		c.ClearNetem(dev.Index)
	}
}

// checkLedger asserts that the round lost nothing: every path one device wrote
// holds that content everywhere, and every path two devices wrote at once
// holds one of the two versions with the other kept as a conflict copy on
// every device. It returns the paths that collided.
func (c *Cluster) checkLedger(round int, book ledger) []string {
	c.T.Helper()
	for path, want := range book.live {
		for _, dev := range c.Devices {
			entry, ok := dev.Tree()[path]
			if !ok {
				c.T.Fatalf("round %d: %s is missing %s, which was written this round", round, dev.Name, path)
			}
			if entry.Hash != hashOf(want) {
				c.T.Fatalf("round %d: %s holds %s as %s, want the version written this round, %s",
					round, dev.Name, path, entry.Hash, hashOf(want))
			}
		}
	}

	var collided []string
	for path, versions := range book.collisions {
		collided = append(collided, path)
		for _, dev := range c.Devices {
			// One of the versions keeps the path, and the ones that
			// did not are on this device under a conflict name.
			held := map[string]bool{}
			entries := dev.Tree()
			winner, ok := entries[path]
			if !ok {
				c.T.Fatalf("round %d: %s holds nothing at %s, which two devices wrote", round, dev.Name, path)
			}
			held[winner.Hash] = true
			for _, copyPath := range conflictCopies(entries) {
				held[hashOf(dev.ReadFile(copyPath))] = true
			}
			for _, version := range versions {
				if !held[hashOf(version)] {
					c.T.Fatalf("round %d: %s lost the version %s of %s: it holds %v",
						round, dev.Name, hashOf(version), path, held)
				}
			}
		}
	}
	return collided
}
