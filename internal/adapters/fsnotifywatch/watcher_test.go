package fsnotifywatch_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/adapters/fsnotifywatch"
	"github.com/wesbragagt/syngo/internal/app/ports"
)

// eventTimeout bounds how long a test waits for a filesystem event.
const eventTimeout = 5 * time.Second

// watch starts a watcher on root and returns its event channel.
func watch(t *testing.T, root string) (<-chan ports.WatchEvent, context.CancelFunc, <-chan error) {
	t.Helper()
	events := make(chan ports.WatchEvent, 256)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fsnotifywatch.New(nil).Watch(ctx, root, events) }()

	// inotify watches are registered before Watch blocks, but the goroutine
	// above may not have run yet; a create event right now could be missed.
	waitForWatch(t, root, events)
	t.Cleanup(cancel)
	return events, cancel, done
}

// waitForWatch writes into root until the watcher reports the change, so the
// test knows the watch is active.
func waitForWatch(t *testing.T, root string, events <-chan ports.WatchEvent) {
	t.Helper()
	probe := filepath.Join(root, ".watch-probe")
	deadline := time.Now().Add(eventTimeout)
	for time.Now().Before(deadline) {
		if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
			t.Fatalf("write probe: %v", err)
		}
		select {
		case <-events:
			if err := os.Remove(probe); err != nil {
				t.Fatalf("remove probe: %v", err)
			}
			drain(events)
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("watcher never reported the probe file")
}

// drain empties the event channel.
func drain(events <-chan ports.WatchEvent) {
	for {
		select {
		case <-events:
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

// waitForEvent returns the first event for path, or fails.
func waitForEvent(t *testing.T, events <-chan ports.WatchEvent, path string, ops ...ports.WatchOp) ports.WatchEvent {
	t.Helper()
	deadline := time.After(eventTimeout)
	for {
		select {
		case ev := <-events:
			if ev.Path != path {
				continue
			}
			if len(ops) == 0 {
				return ev
			}
			for _, op := range ops {
				if ev.Op == op {
					return ev
				}
			}
		case <-deadline:
			t.Fatalf("no %v event for %s", ops, path)
		}
	}
}

func TestWatch_EmitsCreateWrite(t *testing.T) {
	root := t.TempDir()
	events, _, _ := watch(t, root)

	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	waitForEvent(t, events, path, ports.OpCreate, ports.OpWrite)
}

func TestWatch_EmitsRemove(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	events, _, _ := watch(t, root)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	waitForEvent(t, events, path, ports.OpRemove)
}

func TestWatch_NewSubdirIsWatched(t *testing.T) {
	root := t.TempDir()
	events, _, _ := watch(t, root)

	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	waitForEvent(t, events, sub, ports.OpCreate)

	// The watch on the new directory is added while the create event is
	// dispatched, so retry until the file inside it is reported.
	inner := filepath.Join(sub, "b.txt")
	deadline := time.Now().Add(eventTimeout)
	for time.Now().Before(deadline) {
		if err := os.WriteFile(inner, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		select {
		case ev := <-events:
			if ev.Path == inner {
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("no event for a file created in a new subdirectory")
}

func TestWatch_IgnoresReservedDir(t *testing.T) {
	root := t.TempDir()
	reserved := filepath.Join(root, ".syngo", "tmp")
	if err := os.MkdirAll(reserved, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	events, _, _ := watch(t, root)

	if err := os.WriteFile(filepath.Join(reserved, "partial.part"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The event for the folder root arrives after the one for the reserved
	// directory would have, so seeing it first proves the latter is muted.
	for {
		ev := <-events
		if ev.Path == filepath.Join(root, "a.txt") {
			return
		}
		if filepath.Dir(ev.Path) == reserved {
			t.Fatalf("event reported for the reserved directory: %+v", ev)
		}
	}
}

func TestWatch_StopsOnCancel(t *testing.T) {
	root := t.TempDir()
	_, cancel, done := watch(t, root)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch() = %v, want context.Canceled", err)
		}
	case <-time.After(eventTimeout):
		t.Fatalf("Watch did not return after cancel")
	}
}

func TestWatch_MissingRootFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")

	err := fsnotifywatch.New(nil).Watch(context.Background(), missing, make(chan ports.WatchEvent, 1))
	if !errors.Is(err, fsnotifywatch.ErrRootMissing) {
		t.Fatalf("Watch error = %v, want ErrRootMissing", err)
	}
}

func TestWatch_SecondWatchOnTheSameRootStillReportsChanges(t *testing.T) {
	root := t.TempDir()
	events, cancel, done := watch(t, root)

	// The first watch goes away, as it does when a scan fails, and the root
	// is watched again.
	cancel()
	select {
	case <-done:
	case <-time.After(eventTimeout):
		t.Fatal("the first watch did not stop")
	}
	drain(events)

	second, _, _ := watch(t, root)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForEvent(t, second, filepath.Join(root, "a.txt"), ports.OpCreate, ports.OpWrite)
}
