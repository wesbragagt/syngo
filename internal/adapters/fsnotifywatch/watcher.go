// Package fsnotifywatch provides a ports.Watcher backed by fsnotify. The whole
// tree beneath the folder root is watched: directories created later are added
// to the watch as they appear.
package fsnotifywatch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

var _ ports.Watcher = Watcher{}

// reservedDir is the per-folder state directory; it holds partial downloads
// and is never part of a manifest, so it is not worth watching.
const reservedDir = ".syncthis"

// Watcher reports filesystem changes through inotify.
type Watcher struct {
	log *slog.Logger
}

// New returns a watcher. log may be nil.
func New(log *slog.Logger) Watcher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return Watcher{log: log}
}

// Watch sends every change beneath root to out until ctx is done. It returns
// the context error on cancellation.
func (w Watcher) Watch(ctx context.Context, root string, out chan<- ports.WatchEvent) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher for %s: %w", root, err)
	}
	defer func() { _ = watcher.Close() }()

	added := w.addTree(watcher, root)
	w.log.Debug("watching tree", "root", root, "directories", added)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			w.dispatch(ctx, watcher, ev, out)
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				w.log.Warn("watcher overflow, a full scan is needed", "root", root)
				if !send(ctx, out, ports.WatchEvent{Path: root, Op: ports.OpOverflow}) {
					return ctx.Err()
				}
				continue
			}
			w.log.Error("watcher error", "root", root, "error", err)
		}
	}
}

// dispatch translates one fsnotify event and keeps the watch recursive.
func (w Watcher) dispatch(ctx context.Context, watcher *fsnotify.Watcher, ev fsnotify.Event, out chan<- ports.WatchEvent) {
	if ev.Has(fsnotify.Create) && isDir(ev.Name) {
		w.addTree(watcher, ev.Name)
	}
	for _, op := range ops(ev.Op) {
		if !send(ctx, out, ports.WatchEvent{Path: ev.Name, Op: op}) {
			return
		}
	}
}

// addTree adds a watch for dir and every directory beneath it, and returns how
// many watches were added. Directories that vanish while walking are ignored.
func (w Watcher) addTree(watcher *fsnotify.Watcher, dir string) int {
	added := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == reservedDir && p != dir {
			return fs.SkipDir
		}
		if err := watcher.Add(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			// A failed watch, typically the inotify limit, loses events
			// for that directory; the periodic full rescan is the net.
			w.log.Warn("cannot watch directory", "path", p, "error", err)
			return nil
		}
		added++
		return nil
	})
	if err != nil {
		w.log.Warn("cannot walk watched tree", "path", dir, "error", err)
	}
	return added
}

// isDir reports whether path is a directory right now.
func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

// ops splits an fsnotify op set into the port's operations.
func ops(op fsnotify.Op) []ports.WatchOp {
	var out []ports.WatchOp
	for _, pair := range []struct {
		from fsnotify.Op
		to   ports.WatchOp
	}{
		{fsnotify.Create, ports.OpCreate},
		{fsnotify.Write, ports.OpWrite},
		{fsnotify.Remove, ports.OpRemove},
		{fsnotify.Rename, ports.OpRename},
		{fsnotify.Chmod, ports.OpChmod},
	} {
		if op.Has(pair.from) {
			out = append(out, pair.to)
		}
	}
	return out
}

// send delivers one event unless the context is done first.
func send(ctx context.Context, out chan<- ports.WatchEvent, ev ports.WatchEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
