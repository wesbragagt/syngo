package daemon

import (
	"path"
	"sort"
	"strings"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// Coalescer merges a burst of filesystem events into the smallest set of
// subtrees a scan has to visit. It is not safe for concurrent use; one folder
// loop owns one coalescer.
type Coalescer struct {
	// root is the absolute folder root every event path is made relative to.
	root string
	dirs map[string]struct{}
	full bool
}

// NewCoalescer returns a coalescer for a folder rooted at root.
func NewCoalescer(root string) *Coalescer {
	return &Coalescer{root: root, dirs: make(map[string]struct{})}
}

// Add records one watch event. An event is attributed to the directory that
// contains it, so siblings collapse into a single subtree. Events at the
// folder root keep their own path, because the root subtree would mean a full
// scan. Anything the coalescer cannot place, and any overflow, forces a full
// scan.
func (c *Coalescer) Add(ev ports.WatchEvent) {
	if ev.Op == ports.OpOverflow {
		c.full = true
		return
	}

	rel, ok := c.rel(ev.Path)
	if !ok {
		c.full = true
		return
	}
	if dir := path.Dir(rel); dir != "." {
		rel = dir
	}
	c.dirs[rel] = struct{}{}
}

// Pending reports whether a flush would produce work.
func (c *Coalescer) Pending() bool { return c.full || len(c.dirs) > 0 }

// Flush returns the subtrees to scan and whether a full scan is needed, and
// resets the coalescer. Paths nested inside another pending path are dropped,
// since scanning the parent covers them.
func (c *Coalescer) Flush() (paths []string, full bool) {
	full = c.full
	for p := range c.dirs {
		paths = append(paths, p)
	}
	c.dirs = make(map[string]struct{})
	c.full = false

	if full {
		return nil, true
	}
	sort.Strings(paths)
	return prune(paths), false
}

// rel returns the folder-relative, slash-separated form of an absolute event
// path. The second result is false when the path lies outside the folder.
func (c *Coalescer) rel(p string) (string, bool) {
	switch {
	case p == c.root:
		return "", false
	case strings.HasPrefix(p, c.root+"/"):
		return strings.Trim(strings.TrimPrefix(p, c.root+"/"), "/"), true
	default:
		return "", false
	}
}

// prune removes the paths that lie beneath another path of the sorted list.
func prune(sorted []string) []string {
	out := sorted[:0]
	for _, p := range sorted {
		if n := len(out); n > 0 && strings.HasPrefix(p, out[n-1]+"/") {
			continue
		}
		out = append(out, p)
	}
	return out
}
