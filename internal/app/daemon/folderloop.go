package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// eventBuffer is how many watch events a folder may queue while a scan runs.
const eventBuffer = 1024

// folderLoop owns one folder: it watches it, coalesces the events, scans what
// changed and rescans the whole folder periodically. It never exits on a scan
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
				l.scanOrRequeue(ctx, coalescer, debounce, nil)
			} else if len(paths) > 0 {
				l.scanOrRequeue(ctx, coalescer, debounce, paths)
			}
		case <-rescan.C:
			// A root that came back is watched again before it is
			// scanned; a root that is still gone leaves the watch
			// off and the scan records the error.
			l.ensureWatch(ctx)
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
func (l *folderLoop) ensureWatch(ctx context.Context) {
	select {
	case <-l.watchDone:
		l.watching = false
	default:
	}
	if l.watching || ctx.Err() != nil {
		return
	}

	watchCtx, cancel := context.WithCancel(ctx)
	l.watchCancel = cancel
	l.watching = true
	go func() {
		defer func() { l.watchDone <- struct{}{} }()
		if err := l.daemon.deps.Watcher.Watch(watchCtx, l.folder.Path, l.events); err != nil && watchCtx.Err() == nil {
			l.daemon.deps.Log.Error("watch folder", "folder", l.folder.ID, "path", l.folder.Path, "error", err)
		}
	}()
}

// dropWatch stops the watch and waits for its goroutine, so the next
// ensureWatch starts exactly one new watch.
func (l *folderLoop) dropWatch() {
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

	result, err := l.daemon.deps.Scanner.Scan(ctx, current, scan.Options{Paths: paths})
	switch {
	case ctx.Err() != nil:
		return false
	case errors.Is(err, ports.ErrStaleManifest):
		// The folder was written while the scan was reading it, most
		// often by this device's own puller, so the scan wrote nothing
		// and has to read again. The tree is fine: the watch stays on
		// and only the scan is repeated.
		log.Info("scan superseded, rescheduling", "paths", paths, "reason", err)
		return true
	case err != nil:
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
	l.ensureWatch(ctx)
	log.Debug("scan finished", "changes", len(result.Changes), "paths", paths)

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
