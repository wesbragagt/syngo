package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/app/scan"
	"github.com/wesbragagt/syngo/internal/app/status"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// eventBuffer is how many watch events a folder may queue while a scan runs.
const eventBuffer = 1024

// folderLoop owns one folder: it watches it, coalesces the events, scans what
// changed and walks the whole folder whenever its watermark cannot say the
// watcher covered it. It never exits on a scan
// error; the error is logged and stored on the folder.
type folderLoop struct {
	daemon *Daemon
	// folder is the configuration the loop started with. It is read by
	// other goroutines, so the loop re-reads the folder from the store
	// before every scan instead of updating this copy.
	folder folder.Folder

	// rescans carries full-scan requests from the control API. It is
	// buffered so a request never blocks the caller; a second request while
	// one is queued is redundant and dropped.
	rescans chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}

	// events carries what the watcher reports. It is read by the loop
	// goroutine only.
	events chan ports.WatchEvent
	// watching, watchCancel and watchDone track the watch goroutine. Only
	// the loop goroutine touches them, so they need no lock; watchDone is
	// how the watch goroutine reports that it stopped.
	watching    bool
	watchCancel context.CancelFunc
	watchDone   chan struct{}

	// mark is how far the loop has proved the stored manifest to describe
	// the tree. It is only touched by the loop goroutine.
	mark watermark
}

// watermark is the point up to which a folder has been fully read, and the
// claim that every change since then came through the watcher.
//
// seq is the folder sequence the last scan wrote. valid says the claim holds:
// a full scan of this folder finished in this process, the watch was up before
// it started and has been up ever since. While it holds, the periodic tick
// does not walk the folder at all; the watcher already reports what changes,
// and a debounced partial scan covers exactly that.
//
// The claim is dropped, and the next tick walks the whole folder, whenever any
// of these happens:
//
//   - the loop starts, which is every daemon start and every folder whose
//     configuration was changed: nothing watched the folder while the process
//     was down, and a database put back from a backup is put back then;
//   - the watch is not running, stopped on its own or had to be attached
//     again, so there is a gap nothing reported;
//   - the watcher dropped events, which it reports as an overflow;
//   - a scan failed or was superseded, so what it read was never written;
//   - the stored folder sequence is below the watermark, which means the
//     database is not the one the watermark was taken from.
//
// A full scan is therefore the answer whenever the watermark cannot be
// trusted, and `syngo rescan` asks for one at any time.
type watermark struct {
	seq   uint64
	valid bool
}

// newFolderLoop returns a loop for f. Call start to run it.
func newFolderLoop(d *Daemon, f folder.Folder) *folderLoop {
	return &folderLoop{
		daemon:    d,
		folder:    f,
		rescans:   make(chan struct{}, 1),
		done:      make(chan struct{}),
		events:    make(chan ports.WatchEvent, eventBuffer),
		watchDone: make(chan struct{}, 1),
	}
}

// start runs the loop in its own goroutine.
func (l *folderLoop) start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go func() {
		defer close(l.done)
		defer cancel()
		l.run(ctx)
	}()
}

// stop cancels the loop and waits for it to return.
func (l *folderLoop) stop() {
	l.cancel()
	<-l.done
}

// requestRescan queues a full scan. It never blocks.
func (l *folderLoop) requestRescan() {
	select {
	case l.rescans <- struct{}{}:
	default:
	}
}

// run is the loop body.
func (l *folderLoop) run(ctx context.Context) {
	log := l.daemon.deps.Log.With("folder", l.folder.ID)

	coalescer := NewCoalescer(l.folder.Path)
	debounce := time.NewTimer(l.daemon.cfg.debounce())
	stopTimer(debounce)
	defer stopTimer(debounce)
	rescan := time.NewTicker(l.daemon.cfg.rescanInterval())
	defer rescan.Stop()

	// Transfers interrupted by the last shutdown are picked up before the
	// folder is scanned, so their partial files are not mistaken for
	// leftovers.
	if l.daemon.deps.Syncer != nil {
		if err := l.daemon.deps.Syncer.ResumeAll(ctx, l.folder); err != nil && ctx.Err() == nil {
			log.Error("resume transfers", "error", err)
		}
	}

	// The watch is attached before the startup scan, so a change made
	// between the scan and the watch cannot go unnoticed.
	l.ensureWatch(ctx)

	// A folder is scanned once at startup so a daemon that was down while
	// files changed catches up without waiting for the first tick.
	l.scanOrRequeue(ctx, coalescer, debounce, nil)

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-l.events:
			coalescer.Add(ev)
			stopTimer(debounce)
			debounce.Reset(l.daemon.cfg.debounce())
		case <-debounce.C:
			paths, full := coalescer.Flush()
			if full {
				log.Debug("watcher overflow, scanning whole folder")
				// Events were dropped, so what the watcher
				// reported no longer covers the folder.
				l.mark = watermark{}
				l.scanOrRequeue(ctx, coalescer, debounce, nil)
			} else if len(paths) > 0 {
				l.scanOrRequeue(ctx, coalescer, debounce, paths)
			}
		case <-rescan.C:
			// A root that came back is watched again before it is
			// scanned; a root that is still gone leaves the watch
			// off and the scan records the error.
			l.ensureWatch(ctx)
			// A folder the watcher has covered without a gap since
			// the last full scan is not walked again: the tick
			// costs one row read instead of the whole tree.
			if l.watermarkHolds(ctx) {
				continue
			}
			l.scanOrRequeue(ctx, coalescer, debounce, nil)
		case <-l.rescans:
			// A full scan covers everything the coalescer collected.
			coalescer.Flush()
			stopTimer(debounce)
			l.scanOrRequeue(ctx, coalescer, debounce, nil)
		}
	}
}

// ensureWatch attaches the watch unless one is already running. A watch that
// stopped, because the root went away or the watcher failed, is attached again
// by the next call.
func (l *folderLoop) ensureWatch(ctx context.Context) bool {
	select {
	case <-l.watchDone:
		l.watching = false
		// The watch stopped on its own, so the changes made since are
		// nobody's report.
		l.mark = watermark{}
	default:
	}
	if l.watching || ctx.Err() != nil {
		return false
	}

	// A watch that has to be attached leaves the time before it unwatched,
	// so only a full scan can say what the folder holds.
	l.mark = watermark{}
	watchCtx, cancel := context.WithCancel(ctx)
	l.watchCancel = cancel
	l.watching = true
	go func() {
		defer func() { l.watchDone <- struct{}{} }()
		if err := l.daemon.deps.Watcher.Watch(watchCtx, l.folder.Path, l.events); err != nil && watchCtx.Err() == nil {
			l.daemon.deps.Log.Error("watch folder", "folder", l.folder.ID, "path", l.folder.Path, "error", err)
		}
	}()
	return true
}

// dropWatch stops the watch and waits for its goroutine, so the next
// ensureWatch starts exactly one new watch.
func (l *folderLoop) dropWatch() {
	l.mark = watermark{}
	if !l.watching {
		return
	}
	l.watchCancel()
	<-l.watchDone
	l.watching = false
}

// scanOrRequeue scans and, when the scan was superseded by a write that
// landed while it ran, puts its subtrees back on the coalescer and arms the
// debounce timer. The work is still wanted, so it is queued rather than left
// to the next tick, and it goes through the coalescer so a burst of
// supersessions collapses into one scan.
func (l *folderLoop) scanOrRequeue(ctx context.Context, coalescer *Coalescer, debounce *time.Timer, paths []string) {
	if !l.scan(ctx, paths) {
		return
	}
	coalescer.Requeue(paths)
	stopTimer(debounce)
	debounce.Reset(l.daemon.cfg.debounce())
}

// scan runs one scan of the folder unless it is paused. paths limits the scan
// to those subtrees; nil scans everything. It reports whether the scan was
// superseded and has to run again.
func (l *folderLoop) scan(ctx context.Context, paths []string) bool {
	log := l.daemon.deps.Log.With("folder", l.folder.ID)

	current, err := l.daemon.deps.Store.GetFolder(ctx, l.folder.ID)
	if errors.Is(err, ports.ErrNotFound) {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Error("read folder", "error", err)
		}
		return false
	}
	if current.Paused {
		log.Debug("folder paused, skipping scan")
		return false
	}
	l.daemon.setState(current.ID, status.StateScanning)
	defer l.daemon.setState(current.ID, "")

	// Whether the watch was already up before the walk started is half of
	// what makes the result a watermark; the other half is that it is still
	// up after it.
	watchedBefore := l.watching

	result, err := l.daemon.deps.Scanner.Scan(ctx, current, scan.Options{Paths: paths})
	switch {
	case ctx.Err() != nil:
		return false
	case errors.Is(err, ports.ErrStaleManifest):
		l.mark = watermark{}
		// The folder was written while the scan was reading it, most
		// often by this device's own puller, so the scan wrote nothing
		// and has to read again. The tree is fine: the watch stays on
		// and only the scan is repeated.
		log.Info("scan superseded, rescheduling", "paths", paths, "reason", err)
		return true
	case err != nil:
		l.mark = watermark{}
		// The scanner records the failure on the folder itself, so the
		// loop only reports it. A folder that cannot be scanned, most
		// often because its root is gone, is not worth watching either;
		// the next tick attaches the watch again.
		log.Error("scan folder", "error", err, "paths", paths)
		l.dropWatch()
		return false
	}
	// A scan that succeeded after a failure means the root is usable
	// again, so the watch goes back on without waiting for the next tick.
	attached := l.ensureWatch(ctx)
	// A scan that lost a write race deferred the paths another writer had
	// changed under it, leaving them to a later scan. The watermark claims
	// the opposite, that this scan covered everything since the last one,
	// so a scan that deferred anything does not get to take it: the claim
	// would be false and the next tick would skip the walk the deferred
	// paths are waiting for.
	covered := watchedBefore && !attached && l.watching && result.Deferred == 0
	l.observe(paths, result.Sequence, covered)
	log.Debug("scan finished", "changes", len(result.Changes), "paths", paths,
		"watermark", l.mark.seq, "watermark_valid", l.mark.valid)

	// Peers pull, so they only need to be told that there is something new.
	if len(result.Changes) > 0 && l.daemon.deps.Listener != nil {
		l.daemon.deps.Listener.Broadcast(current.ID, result.Sequence)
	}
	return false
}

// stopTimer stops t and drains it so it can be reset safely.
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// observe records what a finished scan proved. covered says the watch was up
// before the scan started and is still the same one now, which is what lets a
// scan stand for the whole time since the last one: a full scan read every
// path itself, and a partial scan read the paths the watch reported.
func (l *folderLoop) observe(paths []string, seq uint64, covered bool) {
	switch {
	case !covered:
		l.mark = watermark{}
	case len(paths) == 0:
		l.mark = watermark{seq: seq, valid: true}
	case l.mark.valid:
		l.mark.seq = seq
	}
}

// watermarkHolds reports whether the folder can be left unwalked this tick. It
// costs one row: the folder sequence, which must not have fallen below the
// watermark. A database that came back from a backup, or one swapped under the
// running daemon, is exactly a sequence that went backwards, and is answered
// with a full scan.
func (l *folderLoop) watermarkHolds(ctx context.Context) bool {
	if !l.mark.valid || !l.watching {
		return false
	}
	seq, err := l.daemon.deps.Store.FolderSequence(ctx, l.folder.ID)
	if err != nil {
		if ctx.Err() == nil {
			l.daemon.deps.Log.Warn("folder sequence not read, scanning whole folder",
				"folder", l.folder.ID, "error", err)
		}
		l.mark = watermark{}
		return false
	}
	if seq < l.mark.seq {
		l.daemon.deps.Log.Info("folder sequence went backwards, scanning whole folder",
			"folder", l.folder.ID, "sequence", seq, "watermark", l.mark.seq)
		l.mark = watermark{}
		return false
	}
	// A sequence that moved forward is this device's own puller writing
	// what it installed, which the watcher reports like any other change.
	l.mark.seq = seq
	l.daemon.deps.Log.Debug("folder covered by the watcher, skipping the full scan",
		"folder", l.folder.ID, "watermark", seq)
	l.daemon.publish("scan.skipped", map[string]any{
		"folder_id": string(l.folder.ID), "watermark": seq, "reason": "watched",
	})
	return true
}
