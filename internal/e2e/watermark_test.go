package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// watermarkFiles is how many files the measured folder holds. It is large
// enough that walking it is visible work and small enough that the first scan
// still finishes within a tick or two.
const watermarkFiles = 300

// scanWork sums what the scans a node recorded had to read: the entries they
// visited and the files they hashed.
func scanWork(events []ports.Event) (scans, scanned, hashed int) {
	for _, ev := range events {
		if ev.Kind != "scan.completed" {
			continue
		}
		scans++
		scanned += intField(ev.Fields["scanned"])
		hashed += intField(ev.Fields["hashed"])
	}
	return scans, scanned, hashed
}

// intField reads a numeric event field, whatever numeric type it carries.
func intField(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

// waitFileCount waits until the folder of a node holds n recorded files.
func waitFileCount(t *testing.T, n *node, want int64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d files to be recorded on %s", want, n.name), convergeTimeout, func() bool {
		stats, err := n.App.Store.FolderStats(context.Background(), folderID)
		return err == nil && stats.Files == want
	})
}

// TestWatermark_AnUnchangedFolderIsNotWalkedAgain is the Phase 5 claim,
// measured: once the folder has been read with the watch up, the rescan ticks
// stop walking it. The first window is what one full pass costs; the second
// window is the same number of ticks with nothing changed.
func TestWatermark_AnUnchangedFolderIsNotWalkedAgain(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)

	for i := range watermarkFiles {
		writeFile(t, a, fmt.Sprintf("d%02d/f%03d.txt", i%10, i), payload(512, byte(i)))
	}
	waitFileCount(t, a, watermarkFiles)

	// Let the debounced scans of the burst settle, then measure what the
	// folder has cost so far.
	time.Sleep(3 * testRescanInterval)
	firstScans, firstScanned, firstHashed := scanWork(a.Events.All())
	firstSkipped := a.Events.Count("scan.skipped")
	if firstScanned < watermarkFiles {
		t.Fatalf("the first window visited %d entries, want at least %d", firstScanned, watermarkFiles)
	}

	// The same number of ticks again, with nothing changing.
	ticks := 5
	time.Sleep(time.Duration(ticks) * testRescanInterval * time.Duration(timeoutScale))

	scans, scanned, hashed := scanWork(a.Events.All())
	skipped := a.Events.Count("scan.skipped")
	t.Logf("first window: %d scans, %d entries visited, %d files hashed", firstScans, firstScanned, firstHashed)
	t.Logf("second window: %d scans, %d entries visited, %d files hashed, %d ticks skipped",
		scans-firstScans, scanned-firstScanned, hashed-firstHashed, skipped-firstSkipped)

	if got := scanned - firstScanned; got != 0 {
		t.Fatalf("the second window visited %d entries, want 0", got)
	}
	if got := scans - firstScans; got != 0 {
		t.Fatalf("the second window ran %d scans, want 0", got)
	}
	if got := skipped - firstSkipped; got < ticks-2 {
		t.Fatalf("%d ticks were skipped, want about %d", got, ticks)
	}

	// The folder is still being watched: a change is picked up at once.
	writeFile(t, a, "d00/late.txt", payload(64, 9))
	waitFileCount(t, a, watermarkFiles+1)
}

// TestWatermark_AChangeMadeWhileNothingWatchedIsFound is the fallback that
// keeps the saving honest: a file created while the daemon, and so the
// watcher, was not running is found by the full scan the next start does.
func TestWatermark_AChangeMadeWhileNothingWatchedIsFound(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	writeFile(t, a, "before.txt", payload(64, 1))
	waitFileCount(t, a, 1)

	a.Stop()
	// Nothing is watching the folder now, so nothing can report this.
	writeFileIn(t, a.Root, "offline.txt", payload(1024, 2))
	if err := os.Remove(filepath.Join(a.Root, "before.txt")); err != nil {
		t.Fatalf("remove before.txt: %v", err)
	}
	a.Restart()

	waitFileCount(t, a, 1)
	waitFor(t, "the offline file to be recorded", convergeTimeout, func() bool {
		e, err := a.App.Store.GetFile(context.Background(), folderID, "offline.txt")
		return err == nil && !e.Deleted && e.Size == 1024
	})
	waitFor(t, "the offline deletion to be recorded", convergeTimeout, func() bool {
		e, err := a.App.Store.GetFile(context.Background(), folderID, "before.txt")
		return err == nil && e.Deleted
	})
	if scans, _, _ := scanWork(a.Events.All()); scans == 0 {
		t.Fatal("the restarted daemon recorded no scan at all")
	}
}
