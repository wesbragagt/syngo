package daemon_test

import (
	"reflect"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/daemon"
	"github.com/wesbragagt/syncthis/internal/app/ports"
)

func TestCoalesce_MergesSiblingEventsToParentDir(t *testing.T) {
	c := daemon.NewCoalescer("/srv/docs")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		c.Add(ports.WatchEvent{Path: "/srv/docs/sub/" + name, Op: ports.OpWrite})
	}

	paths, full := c.Flush()
	if full {
		t.Fatalf("full = true, want false")
	}
	if want := []string{"sub"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestCoalesce_DropsPathsCoveredByAParent(t *testing.T) {
	c := daemon.NewCoalescer("/srv/docs")
	c.Add(ports.WatchEvent{Path: "/srv/docs/a/file.txt", Op: ports.OpWrite})
	c.Add(ports.WatchEvent{Path: "/srv/docs/a/b/file.txt", Op: ports.OpCreate})
	c.Add(ports.WatchEvent{Path: "/srv/docs/z/file.txt", Op: ports.OpRemove})

	paths, _ := c.Flush()
	if want := []string{"a", "z"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestCoalesce_RootLevelEventKeepsItsOwnPath(t *testing.T) {
	c := daemon.NewCoalescer("/srv/docs")
	c.Add(ports.WatchEvent{Path: "/srv/docs/new.txt", Op: ports.OpCreate})

	paths, full := c.Flush()
	if full {
		t.Fatalf("full = true, want false")
	}
	if want := []string{"new.txt"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestCoalesce_OverflowForcesFull(t *testing.T) {
	c := daemon.NewCoalescer("/srv/docs")
	c.Add(ports.WatchEvent{Path: "/srv/docs/sub/a.txt", Op: ports.OpWrite})
	c.Add(ports.WatchEvent{Path: "/srv/docs", Op: ports.OpOverflow})

	paths, full := c.Flush()
	if !full {
		t.Fatalf("full = false, want true")
	}
	if paths != nil {
		t.Fatalf("paths = %v, want nil: a full scan covers every subtree", paths)
	}
}

func TestCoalesce_EventOutsideTheFolderForcesFull(t *testing.T) {
	c := daemon.NewCoalescer("/srv/docs")
	c.Add(ports.WatchEvent{Path: "/srv/other/a.txt", Op: ports.OpWrite})

	if _, full := c.Flush(); !full {
		t.Fatalf("full = false, want true")
	}
}

func TestCoalesce_FlushEmptyWhenNoEvents(t *testing.T) {
	c := daemon.NewCoalescer("/srv/docs")
	if c.Pending() {
		t.Fatalf("Pending() = true on a fresh coalescer")
	}

	paths, full := c.Flush()
	if len(paths) != 0 || full {
		t.Fatalf("Flush() = (%v, %t), want (empty, false)", paths, full)
	}
}

func TestCoalesce_FlushResetsState(t *testing.T) {
	c := daemon.NewCoalescer("/srv/docs")
	c.Add(ports.WatchEvent{Path: "/srv/docs/sub/a.txt", Op: ports.OpWrite})
	if !c.Pending() {
		t.Fatalf("Pending() = false after an event")
	}
	c.Flush()

	if c.Pending() {
		t.Fatalf("Pending() = true after Flush")
	}
	if paths, full := c.Flush(); len(paths) != 0 || full {
		t.Fatalf("second Flush() = (%v, %t), want (empty, false)", paths, full)
	}
}
