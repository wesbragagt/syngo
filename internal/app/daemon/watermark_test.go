package daemon_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/daemon"
	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/ports"
)

// startDaemonWithEvents wires a daemon whose own events a test can read.
func startDaemonWithEvents(t *testing.T, cfg daemon.Config, watcher ports.Watcher) (*harness, *fakes.Events) {
	t.Helper()
	events := fakes.NewEvents()
	h := startDaemonTweaked(t, cfg, watcher, func(d *daemon.Deps) { d.Events = events },
		nil, testFolder("docs", false))
	return h, events
}

// TestWatermark_TickDoesNotWalkAFolderTheWatcherCovers is the saving: once a
// full scan has run with the watch up, the periodic tick reads one row instead
// of the whole tree, and the folder is not scanned again until something
// changes.
func TestWatermark_TickDoesNotWalkAFolderTheWatcherCovers(t *testing.T) {
	h, events := startDaemonWithEvents(t, daemon.Config{RescanInterval: 20 * time.Millisecond}, nil)
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	// Ten ticks' worth of time with nothing changing.
	if !events.Wait("scan.skipped", 5, 3*time.Second) {
		t.Fatalf("%d ticks skipped the scan, want at least 5", events.Count("scan.skipped"))
	}
	if calls := h.scanner.snapshot(); len(calls) != 1 {
		t.Fatalf("scans = %d, want only the startup one: %+v", len(calls), calls)
	}
	t.Logf("%d ticks cost no scan at all", events.Count("scan.skipped"))

	// The watermark is not a way of ignoring changes: one watch event still
	// gets its subtree scanned.
	h.push(t, ports.WatchEvent{Path: folderRoot + "/sub/a.txt", Op: ports.OpWrite})
	waitFor(t, "the change to be scanned", func() bool { return len(h.scanner.snapshot()) == 2 })
	if call := h.scanner.snapshot()[1]; call.full() {
		t.Fatalf("scan after a watch event = %+v, want the subtree only", call)
	}
}

// stoppingWatcher returns from every watch, which is what a watcher that fails
// or is closed under the loop looks like.
type stoppingWatcher struct {
	mu       sync.Mutex
	attached int
}

// Watch returns at once, so the folder is never actually watched however often
// the loop attaches it.
func (w *stoppingWatcher) Watch(_ context.Context, _ string, _ chan<- ports.WatchEvent) error {
	w.mu.Lock()
	w.attached++
	w.mu.Unlock()
	return nil
}

// attaches reports how often the watch was established.
func (w *stoppingWatcher) attaches() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attached
}

// TestWatermark_AFolderNothingWatchesIsScannedInFull is the fallback: a watch
// that is not running covers nothing, so every tick walks the folder again.
func TestWatermark_AFolderNothingWatchesIsScannedInFull(t *testing.T) {
	watcher := &stoppingWatcher{}
	h, events := startDaemonWithEvents(t, daemon.Config{RescanInterval: 20 * time.Millisecond}, watcher)
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	// The watch stops. Every tick from here on attaches a new one, which
	// closes nothing: the time in between was unwatched.
	waitFor(t, "scans after the watch stopped", func() bool { return len(h.scanner.snapshot()) >= 4 })
	for _, call := range h.scanner.snapshot() {
		if !call.full() {
			t.Fatalf("call = %+v, want full scans only", call)
		}
	}
	if n := events.Count("scan.skipped"); n != 0 {
		t.Fatalf("%d ticks were skipped for an unwatched folder, want 0", n)
	}
	if watcher.attaches() < 2 {
		t.Fatalf("watch attaches = %d, want the loop to keep trying", watcher.attaches())
	}
}

// TestWatermark_ADatabaseThatWentBackwardsIsScannedInFull is the restore case:
// the folder sequence is below the watermark this process took, so the store
// is not the one the watermark describes and the folder is read again.
func TestWatermark_ADatabaseThatWentBackwardsIsScannedInFull(t *testing.T) {
	h, events := startDaemonWithEvents(t, daemon.Config{RescanInterval: 20 * time.Millisecond}, nil)
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })
	h.store.SetSequence("docs", 40)

	// The scanner reports sequence 40 for the scan that follows, so the
	// watermark this process holds is 40.
	h.scanner.reportSequence(40)
	if err := h.daemon.Rescan(h.ctx, "docs"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	waitFor(t, "the watermark scan", func() bool { return len(h.scanner.snapshot()) >= 2 })
	if !events.Wait("scan.skipped", 1, 3*time.Second) {
		t.Fatal("no tick was skipped, so the watermark was never taken")
	}

	// A database restored from a backup holds a sequence the watermark has
	// long passed.
	before := len(h.scanner.snapshot())
	h.store.SetSequence("docs", 7)
	waitFor(t, "a full scan after the sequence went backwards", func() bool {
		return len(h.scanner.snapshot()) > before
	})
	if call := h.scanner.snapshot()[before]; !call.full() {
		t.Fatalf("scan after the restore = %+v, want a full scan", call)
	}
}

// TestWatermark_AScanThatDeferredPathsDoesNotTakeTheWatermark closes the gap
// between the scan retry and the watermark. A scan that loses a write race
// defers the paths another writer changed under it and leaves them to a later
// scan. The watermark says the opposite, that this scan covered everything the
// watcher reported since the last one, so taking it would let every following
// tick skip the walk the deferred paths are waiting for: the path stays
// unrecorded until a fresh filesystem event, a watch drop or a restart. A
// writer that changes rows without touching disk, such as the merge path or
// another process on the same database, generates no such event at all.
func TestWatermark_AScanThatDeferredPathsDoesNotTakeTheWatermark(t *testing.T) {
	h, events := startDaemonWithEvents(t, daemon.Config{RescanInterval: 20 * time.Millisecond}, nil)
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })
	if !events.Wait("scan.skipped", 1, 3*time.Second) {
		t.Fatal("no tick was skipped, so the watermark was never taken")
	}

	// The next scan loses a write race and defers a path.
	h.scanner.reportDeferred(1)
	if err := h.daemon.Rescan(h.ctx, "docs"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	waitFor(t, "the deferring scan", func() bool { return len(h.scanner.snapshot()) >= 2 })
	after := len(h.scanner.snapshot())

	// The tick has to walk: the deferred path is not in the store and
	// nothing else is going to report it.
	waitFor(t, "a walk on the tick after a scan deferred a path", func() bool {
		return len(h.scanner.snapshot()) > after
	})
	if call := h.scanner.snapshot()[after]; !call.full() {
		t.Fatalf("scan after the deferral = %+v, want a full scan", call)
	}
}
