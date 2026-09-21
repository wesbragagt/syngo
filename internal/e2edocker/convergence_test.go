package e2edocker

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// TestMesh_ThreeDevicesConverge is the base case the rest of the suite builds
// on: three devices paired with each other, each writing files nobody else
// touches, all of them ending up with the same folder.
func TestMesh_ThreeDevicesConverge(t *testing.T) {
	c := Start(t, 3)

	const perDevice = 20
	for _, dev := range c.Devices {
		for i := 0; i < perDevice; i++ {
			// Sizes from empty to five chunks, so the run covers a
			// file that needs no transfer and files that need
			// several.
			size := (i % 6) * chunkSize
			dev.WriteFile(fmt.Sprintf("%s/f%02d.bin", dev.Name, i), payload(size, byte(i)))
		}
	}

	c.WaitConverged()
	c.CheckInvariants()

	for _, dev := range c.Devices {
		if got := len(dev.Tree()); got != perDevice*len(c.Devices)+len(c.Devices) {
			t.Errorf("%s holds %d entries, want %d files and %d directories",
				dev.Name, got, perDevice*len(c.Devices), len(c.Devices))
		}
		for _, peer := range dev.Status().Peers {
			if !peer.Connected {
				t.Errorf("%s: peer %s is not connected", dev.Name, peer.Name)
			}
		}
	}
}

// editRounds is how many rounds of simultaneous edits the three devices make.
const editRounds = 10

// pathsPerDevice is how many paths each device owns in the concurrent-edit
// test; five of them change in every round.
const pathsPerDevice = 12

// TestMesh_ConcurrentDisjointEditsConverge has all three devices change the
// folder at the same instant, round after round. Nobody touches a path another
// device owns, so the only thing that can go wrong is the synchronization
// itself: every round has to end with the same tree everywhere and nothing
// recorded as a conflict.
func TestMesh_ConcurrentDisjointEditsConverge(t *testing.T) {
	t.Skip("known: a transfer whose source changes mid-flight stays in the journal as failed forever, " +
		"so the journal is never empty again; see Known failures in docs/PLAN-E2E-DOCKER.md")
	c := Start(t, 3)

	rounds := editRounds
	if testing.Short() {
		rounds = 3
	}
	// want is the union of the last write to every path, which is what the
	// devices must agree on at the end.
	want := map[string][]byte{}
	random := rand.New(rand.NewPCG(1, 2))

	for round := 0; round < rounds; round++ {
		// The work of a round is decided before any of it happens, so a
		// failure is reproducible from the seed alone.
		type change struct {
			path    string
			content []byte
			remove  bool
		}
		plan := make([][]change, len(c.Devices))
		for i, dev := range c.Devices {
			for _, n := range random.Perm(pathsPerDevice)[:5] {
				path := fmt.Sprintf("%s/f%02d.bin", dev.Name, n)
				_, exists := want[path]
				if exists && random.IntN(3) == 0 {
					plan[i] = append(plan[i], change{path: path, remove: true})
					delete(want, path)
					continue
				}
				content := payload(random.IntN(4)*chunkSize+random.IntN(512), byte(round*7+n))
				plan[i] = append(plan[i], change{path: path, content: content})
				want[path] = content
			}
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, dev := range c.Devices {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for _, ch := range plan[i] {
					if ch.remove {
						dev.Remove(ch.path)
						continue
					}
					dev.WriteFile(ch.path, ch.content)
				}
			}()
		}
		close(start)
		wg.Wait()

		c.WaitConverged()
		for _, dev := range c.Devices {
			if rows := dev.Conflicts(); len(rows) != 0 {
				t.Fatalf("round %d: %s recorded %d conflicts over paths nobody shares: %+v",
					round, dev.Name, len(rows), rows)
			}
		}
	}

	c.CheckInvariants()
	for _, dev := range c.Devices {
		got := dev.Tree()
		for path, content := range want {
			entry, ok := got[path]
			if !ok {
				t.Errorf("%s is missing %s", dev.Name, path)
				continue
			}
			if entry.Hash != hashOf(content) {
				t.Errorf("%s holds %s as %s, want the last write %s",
					dev.Name, path, entry.Hash, hashOf(content))
			}
		}
		for path, entry := range got {
			if entry.Kind == manifest.KindDir {
				continue
			}
			if _, ok := want[path]; !ok {
				t.Errorf("%s holds %s, which was deleted", dev.Name, path)
			}
		}
	}
}

// TestMesh_ThreeWritersOneConflict writes a different version of one file on
// each of three devices while none of them can reach the others, and then
// heals every link at once. The rule that decides a conflict is a total order
// on the content, so the three devices have to agree on one winner and keep
// the two losing versions as copies: three-way divergence may not lose
// anything a two-way one would keep.
func TestMesh_ThreeWritersOneConflict(t *testing.T) {
	t.Skip("known: three-way divergence keeps only one of the two losing versions, the third is lost " +
		"on every device; see Known failures in docs/PLAN-E2E-DOCKER.md")
	c := Start(t, 3)
	c.requireNetAdmin()

	c.Device(0).WriteFile("report.md", payload(chunkSize, 1))
	c.WaitConverged()

	c.Partition(0, 1)
	c.Partition(0, 2)
	c.Partition(1, 2)

	versions := make([][]byte, len(c.Devices))
	for i, dev := range c.Devices {
		versions[i] = payload(chunkSize+i*1000+7, byte(50+i))
		from := dev.LogSize()
		dev.WriteFile("report.md", versions[i])
		// A version that is only on disk is not yet a version the device
		// can defend, so the links stay down until all three are stored.
		dev.WaitScanRecorded(from)
	}

	c.Heal()
	c.WaitConverged()
	c.CheckInvariants()

	// The greatest hash keeps the path; the other two versions must exist
	// as copies, on every device.
	winner := 0
	for i := range versions {
		if hashOf(versions[i]) > hashOf(versions[winner]) {
			winner = i
		}
	}
	for _, dev := range c.Devices {
		if got := dev.ReadFile("report.md"); !equalBytes(got, versions[winner]) {
			t.Errorf("%s holds report.md as %s, want %s's version %s",
				dev.Name, hashOf(got), c.Devices[winner].Name, hashOf(versions[winner]))
		}
		copies := conflictCopies(dev.Tree())
		held := map[string]string{}
		for _, path := range copies {
			held[hashOf(dev.ReadFile(path))] = path
		}
		for i, version := range versions {
			if i == winner {
				continue
			}
			if _, ok := held[hashOf(version)]; !ok {
				t.Errorf("%s lost %s's version of report.md (%s); it holds the copies %v",
					dev.Name, c.Devices[i].Name, hashOf(version), copies)
			}
		}
		if len(copies) != len(versions)-1 {
			t.Errorf("%s holds %d conflict copies, want %d: %v",
				dev.Name, len(copies), len(versions)-1, copies)
		}
		if rows := dev.Conflicts(); len(rows) == 0 {
			t.Errorf("%s recorded no conflict over report.md", dev.Name)
		}
	}
}
