// Package scan walks a folder, classifies what changed since the last scan and
// persists the result.
package scan

import (
	"fmt"
	"strings"
	"time"

	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
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
	// Sequence is the folder sequence number after the scan was persisted.
	// It is zero for a dry run.
	Sequence uint64
	// Scanned counts the entries that became manifest entries.
	Scanned int
	// Hashed counts the files whose contents had to be read.
	Hashed int
	// Bytes is the total size of the files that were hashed.
	Bytes int64
	// Ignored counts the entries skipped by a policy or an ignore pattern. A
	// skipped directory counts once, not once per entry beneath it.
	Ignored int
	// Deferred counts the paths another writer changed while this scan was
	// reading the tree. The scan classified none of them, so it did not
	// cover the folder: a caller that was about to claim it had, such as
	// the watermark of the folder loop, must not.
	Deferred int
	// Skipped counts the entries the scan could not read. Their stored
	// version, if any, is left untouched.
	Skipped int
	// Warnings describes the skipped entries, one message per entry up to
	// MaxWarnings.
	Warnings []string
	Duration time.Duration
	DryRun   bool
}

// MaxWarnings bounds how many skipped entries a result describes; the counter
// keeps counting past it.
const MaxWarnings = 10

// Warning summarizes the entries the scan could not read, or "" when it read
// everything it wanted to. It is what the scan records as the folder error.
func (r Result) Warning() string {
	if r.Skipped == 0 {
		return ""
	}
	msg := fmt.Sprintf("%d entry/entries skipped: %s", r.Skipped, strings.Join(r.Warnings, "; "))
	if r.Skipped > len(r.Warnings) {
		msg += fmt.Sprintf("; and %d more", r.Skipped-len(r.Warnings))
	}
	return msg
}

// Counts returns how many changes of each type the scan classified.
func (r Result) Counts() map[manifest.ChangeType]int {
	out := make(map[manifest.ChangeType]int, 4)
	for _, c := range r.Changes {
		out[c.Type]++
	}
	return out
}
