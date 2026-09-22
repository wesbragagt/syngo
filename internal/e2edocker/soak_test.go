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
// The default set is what the product survives today; the full set adds the
// actions and the collisions that run into the known failures of section 10
// of the plan, and is asked for with SYNCTHIS_E2E_SOAK_CHAOS=all.
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
	raw := os.Getenv("SYNCTHIS_E2E_SOAK_MINUTES")
	if raw == "" {
		t.Skip("the soak runs for as long as SYNCTHIS_E2E_SOAK_MINUTES says; see make e2e-docker-soak")
	}
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes <= 0 {
		t.Fatalf("SYNCTHIS_E2E_SOAK_MINUTES=%q is not a positive number of minutes", raw)
	}
	devices := 3
	if raw := os.Getenv("SYNCTHIS_E2E_SOAK_DEVICES"); raw != "" {
		devices, err = strconv.Atoi(raw)
		if err != nil || devices < 2 {
			t.Fatalf("SYNCTHIS_E2E_SOAK_DEVICES=%q is not a device count of two or more", raw)
		}
	}
	return soakSettings{
		minutes: minutes,
		devices: devices,
		chaos:   soakChaos{full: os.Getenv("SYNCTHIS_E2E_SOAK_CHAOS") == "all"},
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
	rounds, chaosActions, conflicts := 0, 0, 0

	for time.Now().Before(deadline) {
		rounds++
		book := c.soakRound(random, live, settings.chaos)

		if rounds%5 == 0 {
			c.soakChaosAction(random, settings.chaos)
			chaosActions++
		}

		c.WaitPeersConnected()
		c.WaitConverged()
		conflicts += c.checkLedger(rounds, book)
		for _, dev := range c.Devices {
			if parts := dev.PartFiles(); len(parts) != 0 {
				t.Fatalf("round %d: %s left partial downloads: %v", rounds, dev.Name, parts)
			}
		}
		t.Logf("round %d done (%d paths live, %d chaos actions, %s left)",
			rounds, len(live), chaosActions, time.Until(deadline).Round(time.Second))
	}

	c.CheckInvariants()
	// Nothing is left to fix anywhere, and every device recorded the same
	// conflicts: a device that noticed fewer of them lost a version
	// quietly.
	counts := make([]int, len(c.Devices))
	for i, dev := range c.Devices {
		if out, code := dev.Run("repair", folderID, "--dry-run"); code != 0 {
			t.Errorf("repair --dry-run on %s exited %d, want 0: %s", dev.Name, code, out)
		}
		var rows []conflictRow
		dev.RunJSON(&rows, "conflict", "list", "--all")
		counts[i] = len(rows)
	}
	for i, got := range counts {
		if got != counts[0] {
			t.Errorf("%s recorded %d conflicts, %s recorded %d: they saw different histories",
				c.Devices[i].Name, got, c.Devices[0].Name, counts[0])
		}
	}
	t.Logf("soak finished: %d rounds, %d chaos actions, %d collided paths, %d conflicts recorded per device",
		rounds, chaosActions, conflicts, counts[0])
}

// soakRound makes one round of changes, every device at the same instant, and
// returns what was written.
func (c *Cluster) soakRound(random *rand.Rand, live map[string][]byte, chaos soakChaos) ledger {
	c.T.Helper()
	book := ledger{live: map[string][]byte{}, collisions: map[string][][]byte{}}

	// Every device owns the directory named after it, so two devices only
	// ever write the same path when the round says so.
	type op struct {
		path    string
		content []byte
		remove  bool
		renamed string
	}
	plan := make([][]op, len(c.Devices))
	writers := random.Perm(len(c.Devices))[:1+random.IntN(len(c.Devices))]
	for _, i := range writers {
		dev := c.Devices[i]
		owned := ownedPaths(live, dev.Name)
		for n := 0; n < 1+random.IntN(3); n++ {
			// Only creations are in the default set. Changing a
			// path a peer may be asking about leaves a permanent
			// failed row in the journal, whichever way it is
			// changed: a rewrite is refused with hash_changed, a
			// delete or a rename with not_found. That is KF-1 of
			// the plan, and a soak runs into it within a dozen
			// rounds every time, so the rest is asked for with
			// SYNCTHIS_E2E_SOAK_CHAOS=all.
			action := random.IntN(4)
			if !chaos.full {
				action = 0
			}
			switch {
			case action == 1 && len(owned) > 0:
				// Rewrite something this device already owns.
				path := owned[random.IntN(len(owned))]
				content := payload(random.IntN(3)*chunkSize+random.IntN(500)+1, byte(random.IntN(256)))
				plan[i] = append(plan[i], op{path: path, content: content})
				live[path], book.live[path] = content, content
			case action == 2 && len(owned) > 0:
				path := owned[random.IntN(len(owned))]
				plan[i] = append(plan[i], op{path: path, remove: true})
				delete(live, path)
				delete(book.live, path)
			case action == 3 && len(owned) > 0:
				from := owned[random.IntN(len(owned))]
				to := fmt.Sprintf("%s/moved-%d.bin", dev.Name, random.IntN(1<<20))
				plan[i] = append(plan[i], op{path: from, renamed: to})
				live[to], book.live[to] = live[from], live[from]
				delete(live, from)
				delete(book.live, from)
			default:
				path := fmt.Sprintf("%s/f-%d.bin", dev.Name, random.IntN(1<<20))
				content := payload(random.IntN(4)*chunkSize+random.IntN(500)+1, byte(random.IntN(256)))
				plan[i] = append(plan[i], op{path: path, content: content})
				live[path], book.live[path] = content, content
			}
		}
	}

	// A tenth of the rounds have two devices write the same path at the
	// same instant, which is the case the zero-loss check is for. It is
	// part of the full chaos set only: a path two devices rewrite at once
	// runs into KF-1 and KF-2 of the plan.
	if chaos.full && len(writers) > 1 && random.IntN(10) == 0 {
		path := fmt.Sprintf("shared/c-%d.bin", random.IntN(1<<20))
		for _, i := range writers[:2] {
			content := payload(chunkSize+random.IntN(500)+1, byte(random.IntN(256)))
			plan[i] = append(plan[i], op{path: path, content: content})
			book.collisions[path] = append(book.collisions[path], content)
		}
		delete(live, path)
	}

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
					dev.WriteFileAtomic(o.path, o.content)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	return book
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

// soakChaosAction breaks something. The default set is what the product
// survives today: a daemon that was killed, a container that was killed, a
// daemon that was restarted. The full set adds a partition and a shaped link,
// both of which run into the known failures of the plan.
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
// every device. It returns how many paths collided.
func (c *Cluster) checkLedger(round int, book ledger) int {
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

	for path, versions := range book.collisions {
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
	return len(book.collisions)
}
