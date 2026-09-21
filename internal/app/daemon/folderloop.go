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
}

// newFolderLoop returns a loop for f. Call start to run it.
func newFolderLoop(d *Daemon, f folder.Folder) *folderLoop {
	return &folderLoop{
		daemon:  d,
		folder:  f,
		rescans: make(chan struct{}, 1),
		done:    make(chan struct{}),
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
	events := make(chan ports.WatchEvent, eventBuffer)
	go func() {
		if err := l.daemon.deps.Watcher.Watch(ctx, l.folder.Path, events); err != nil && ctx.Err() == nil {
			log.Error("watch folder", "path", l.folder.Path, "error", err)
		}
	}()

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

	// A folder is scanned once at startup so a daemon that was down while
	// files changed catches up without waiting for the first tick.
	l.scan(ctx, nil)

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			coalescer.Add(ev)
			stopTimer(debounce)
			debounce.Reset(l.daemon.cfg.debounce())
		case <-debounce.C:
			paths, full := coalescer.Flush()
			if full {
				log.Debug("watcher overflow, scanning whole folder")
				l.scan(ctx, nil)
			} else if len(paths) > 0 {
				l.scan(ctx, paths)
			}
		case <-rescan.C:
			l.scan(ctx, nil)
		case <-l.rescans:
			// A full scan covers everything the coalescer collected.
			coalescer.Flush()
			stopTimer(debounce)
			l.scan(ctx, nil)
		}
	}
}

// scan runs one scan of the folder unless it is paused. paths limits the scan
// to those subtrees; nil scans everything.
func (l *folderLoop) scan(ctx context.Context, paths []string) {
	log := l.daemon.deps.Log.With("folder", l.folder.ID)

	current, err := l.daemon.deps.Store.GetFolder(ctx, l.folder.ID)
	if errors.Is(err, ports.ErrNotFound) {
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Error("read folder", "error", err)
		}
		return
	}
	if current.Paused {
		log.Debug("folder paused, skipping scan")
		return
	}
	l.daemon.setState(current.ID, status.StateScanning)
	defer l.daemon.setState(current.ID, "")

	result, err := l.daemon.deps.Scanner.Scan(ctx, current, scan.Options{Paths: paths})
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		// The scanner records the failure on the folder itself, so the
		// loop only reports it.
		log.Error("scan folder", "error", err, "paths", paths)
		return
	}
	log.Debug("scan finished", "changes", len(result.Changes), "paths", paths)

	// Peers pull, so they only need to be told that there is something new.
	if len(result.Changes) > 0 && l.daemon.deps.Listener != nil {
		l.daemon.deps.Listener.Broadcast(current.ID, result.Sequence)
	}
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
