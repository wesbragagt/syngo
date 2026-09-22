package e2edocker

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"
)

// TestPlanSoakRoundIsRunnable replays many rounds of the full chaos set
// against a simulated folder and checks that what a round plans is what a
// round reaches: every op runs on a path that is there when its turn comes,
// and the folder afterwards holds exactly what the ledger says it holds.
//
// The soak asked for a path it had already deleted in the same round before:
// the list of the paths a device owns was taken once at the start of the round
// and never lost the paths the round itself removed, so a round could plan a
// delete of a path and then a rename of the same path. The rename failed on a
// source that was gone, and the ledger still claimed the rename target had
// been written, which is the "missing X, which was written this round" the
// soak reported.
func TestPlanSoakRoundIsRunnable(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	chaos := soakChaos{full: true}
	names := []string{"a", "b", "c"}

	// The soak runs for a few dozen rounds over a folder of a few dozen
	// paths, which is when a round picks the same path twice often enough
	// to matter, so the replay is many short runs rather than one long
	// one.
	for session := 0; session < 400; session++ {
		// disk is the simulated folder: what every path holds right now.
		disk := map[string][]byte{}
		live := map[string][]byte{}
		for _, name := range names {
			for n := 0; n < 3; n++ {
				path := fmt.Sprintf("%s/seed-%d.bin", name, n)
				disk[path] = []byte{byte(n)}
				live[path] = []byte{byte(n)}
			}
		}

		for round := 1; round <= 25; round++ {
			plan, book := planSoakRound(random, names, live, chaos)
			for i, ops := range plan {
				for _, o := range ops {
					switch {
					case o.remove:
						delete(disk, o.path)
					case o.renamed != "":
						content, ok := disk[o.path]
						if !ok {
							t.Fatalf("session %d round %d: %s renames %s, which is not there",
								session, round, names[i], o.path)
						}
						disk[o.renamed] = content
						delete(disk, o.path)
					default:
						disk[o.path] = o.content
					}
				}
			}
			for path, want := range book.live {
				got, ok := disk[path]
				if !ok {
					t.Fatalf("session %d round %d: the ledger says %s was written, and it is not there", session, round, path)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("session %d round %d: %s holds %d bytes, the ledger says %d", session, round, path, len(got), len(want))
				}
			}
			for path := range book.collisions {
				// Two devices wrote the path at once; either version
				// may be the one on disk, so only its presence is
				// checked.
				if _, ok := disk[path]; !ok {
					t.Fatalf("session %d round %d: the ledger says %s collided, and it is not there", session, round, path)
				}
			}
		}
	}
}

func TestTakePath(t *testing.T) {
	owned := []string{"a/one", "a/two", "a/three"}
	path, rest := takePath(owned, 1)
	if path != "a/two" {
		t.Fatalf("took %q, want a/two", path)
	}
	if len(rest) != 2 || rest[0] != "a/one" || rest[1] != "a/three" {
		t.Fatalf("left %v, want [a/one a/three]", rest)
	}
	if owned[1] != "a/two" {
		t.Fatalf("the list it was given was changed: %v", owned)
	}
}
