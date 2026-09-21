// Package scan walks a folder, classifies what changed since the last scan and
// persists the result.
package scan

import (
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Options tunes a single scan.
type Options struct {
	// DryRun classifies without writing anything to the store.
	DryRun bool
	// Paths limits the scan to these slash-separated subtrees of the folder.
	// A nil or empty slice scans the whole folder.
	Paths []string
	// ProgressEvery publishes a scan.progress event every n visited entries.
	// Zero disables progress events.
	ProgressEvery int
}

// Result is what one scan found.
type Result struct {
	FolderID folder.ID
	Changes  []manifest.Change
	// Scanned counts the entries that became manifest entries.
	Scanned int
	// Hashed counts the files whose contents had to be read.
	Hashed int
	// Bytes is the total size of the files that were hashed.
	Bytes int64
	// Ignored counts the entries skipped by a policy or an ignore pattern. A
	// skipped directory counts once, not once per entry beneath it.
	Ignored  int
	Duration time.Duration
	DryRun   bool
}

// Counts returns how many changes of each type the scan classified.
func (r Result) Counts() map[manifest.ChangeType]int {
	out := make(map[manifest.ChangeType]int, 4)
	for _, c := range r.Changes {
		out[c.Type]++
	}
	return out
}
