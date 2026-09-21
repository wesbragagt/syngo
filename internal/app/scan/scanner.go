package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// ErrRootMissing reports a folder root that does not exist or is not a
// directory. A scan refuses to run rather than report every stored entry as
// deleted, which would propagate the deletions to the peers.
var ErrRootMissing = errors.New("folder root does not exist or is not a directory")

// Scanner turns the contents of a folder into manifest entries and persists
// the difference with what was stored by the previous scan.
type Scanner struct {
	fs       ports.FileSystem
	hasher   ports.Hasher
	store    ports.MetadataStore
	clock    ports.Clock
	identity ports.Identity
	events   ports.EventSink
	log      *slog.Logger
}

// New returns a scanner. log may be nil.
func New(fs ports.FileSystem, hasher ports.Hasher, store ports.MetadataStore, clock ports.Clock,
	identity ports.Identity, events ports.EventSink, log *slog.Logger) *Scanner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Scanner{fs: fs, hasher: hasher, store: store, clock: clock, identity: identity, events: events, log: log}
}

// Scan walks f, classifies the changes since the last scan and, unless the
// scan is a dry run, writes them to the store. Nothing is written when the
// context is cancelled during the walk: persistence happens only after the
// whole folder has been read.
//
// An entry the scan cannot read does not stop it: the entry is counted in
// Result.Skipped, its stored version is kept, and the summary of what was
// skipped becomes the folder's last error. A failure that prevents the walk
// itself is recorded on the folder as well and returned.
func (s *Scanner) Scan(ctx context.Context, f folder.Folder, o Options) (Result, error) {
	started := s.clock.Now()
	result := Result{FolderID: f.ID, DryRun: o.DryRun}

	if err := s.checkRoot(f); err != nil {
		return Result{}, s.recordFailure(ctx, f, err)
	}
	matcher, err := LoadIgnore(ctx, s.fs, f)
	if err != nil {
		return Result{}, s.recordFailure(ctx, f, err)
	}
	old, err := s.store.LoadManifest(ctx, f.ID)
	if err != nil {
		return Result{}, err
	}

	w := &walker{
		scanner: s, folder: f, opts: o, matcher: matcher,
		old: old, cur: manifest.New(f.ID), result: &result,
	}
	subtrees := cleanPaths(o.Paths)
	if len(subtrees) == 0 {
		err = w.collect(ctx, "")
	} else {
		carryOver(old, w.cur, subtrees)
		for _, sub := range subtrees {
			if err = w.collect(ctx, sub); err != nil {
				break
			}
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, err
		}
		return Result{}, s.recordFailure(ctx, f, err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	result.Changes = manifest.Diff(old, w.cur)
	now := s.clock.Now()
	result.Duration = now.Sub(started)

	if !o.DryRun {
		// The walk is over and the whole result is about to be written
		// in one transaction. The line marks the boundary: everything
		// logged before it read the tree, everything after it wrote.
		s.log.Debug("scan writing", "folder", f.ID, "changes", len(result.Changes))
		if err := s.persist(ctx, f, w.cur, old.Sequence, &result, now); err != nil {
			return Result{}, err
		}
	}
	s.publish("scan.completed", map[string]any{
		"folder_id": string(f.ID),
		"changes":   len(result.Changes),
		"scanned":   result.Scanned,
		"hashed":    result.Hashed,
		"bytes":     result.Bytes,
		"ignored":   result.Ignored,
		"skipped":   result.Skipped,
		"dry_run":   o.DryRun,
	})
	s.log.Debug("scan completed", "folder", f.ID, "changes", len(result.Changes),
		"scanned", result.Scanned, "hashed", result.Hashed, "dry_run", o.DryRun)
	return result, nil
}

// checkRoot reports whether the folder root is still a directory. A root that
// was removed is not a folder whose contents were all deleted: the scan fails
// and leaves the stored entries alone.
func (s *Scanner) checkRoot(f folder.Folder) error {
	info, err := s.fs.Lstat(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("folder %q root %s: %w", f.ID, f.Path, ErrRootMissing)
	}
	if err != nil {
		return fmt.Errorf("folder %q root %s: %w", f.ID, f.Path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("folder %q root %s: %w", f.ID, f.Path, ErrRootMissing)
	}
	return nil
}

// persistRetries is how many times a scan reloads the manifest and re-diffs
// after another scan wrote to the folder first.
const persistRetries = 2

// persist writes the classified changes and records the scan on the folder in
// one transaction. A concurrent scan that wrote first makes the store reject
// the write; the scan then reloads the manifest, re-diffs the tree it already
// walked and tries again, so the change log never records the same scan twice.
func (s *Scanner) persist(ctx context.Context, f folder.Folder, cur manifest.Manifest,
	base uint64, result *Result, now time.Time) error {
	for attempt := 0; ; attempt++ {
		write := s.writeOf(result, base, now)
		err := s.store.ApplyScan(ctx, f.ID, write)
		if err == nil {
			// The store hands out one sequence number per written
			// row, in order, starting at the base.
			result.Sequence = base + uint64(len(write.Upserts)+len(write.Deleted))
			return nil
		}
		if !errors.Is(err, ports.ErrStaleManifest) || attempt >= persistRetries {
			return err
		}
		s.log.Debug("folder changed during scan, re-diffing", "folder", f.ID, "attempt", attempt+1)

		old, loadErr := s.store.LoadManifest(ctx, f.ID)
		if loadErr != nil {
			return loadErr
		}
		base = old.Sequence
		result.Changes = manifest.Diff(old, cur)
	}
}

// writeOf turns classified changes into the write the store applies.
func (s *Scanner) writeOf(result *Result, base uint64, now time.Time) ports.ScanWrite {
	by := device.ID("")
	if s.identity != nil {
		by = s.identity.DeviceID()
	}

	w := ports.ScanWrite{
		BaseSequence: base,
		Changes:      result.Changes,
		By:           by,
		At:           now,
		LastError:    result.Warning(),
	}
	for _, c := range result.Changes {
		switch c.Type {
		case manifest.Create, manifest.Modify, manifest.Rename:
			e := *c.New
			e.ModifiedBy = by
			w.Upserts = append(w.Upserts, e)
			if c.Type == manifest.Rename {
				w.Deleted = append(w.Deleted, c.OldPath)
			}
		case manifest.Delete:
			w.Deleted = append(w.Deleted, c.Path)
		}
	}
	return w
}

// recordFailure stores err as the folder's last error, keeping the time of the
// last scan that did finish, and returns err. A store that cannot record the
// failure only produces a log line: the scan error is what the caller needs.
func (s *Scanner) recordFailure(ctx context.Context, f folder.Folder, err error) error {
	lastScanAt := f.LastScanAt
	if stored, gerr := s.store.GetFolder(ctx, f.ID); gerr == nil {
		lastScanAt = stored.LastScanAt
	}
	if serr := s.store.SetFolderScan(ctx, f.ID, lastScanAt, err.Error()); serr != nil {
		s.log.Error("recording scan failure", "folder", f.ID, "error", serr)
	}
	return err
}

// publish emits an event when a sink is configured.
func (s *Scanner) publish(kind string, fields map[string]any) {
	if s.events == nil {
		return
	}
	s.events.Publish(ports.Event{Kind: kind, At: s.clock.Now(), Fields: fields})
}

// cleanPaths normalizes the subtree list of a partial scan, dropping empty
// entries and anything that would escape the folder root.
func cleanPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		clean := strings.Trim(path.Clean(strings.TrimPrefix(p, "/")), "/")
		if clean == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			continue
		}
		out = append(out, clean)
	}
	return out
}

// carryOver copies the stored entries that lie outside the scanned subtrees
// into cur, so a partial scan classifies them as unchanged.
func carryOver(old, cur manifest.Manifest, subtrees []string) {
	for p, e := range old.Entries {
		if !underAny(p, subtrees) {
			cur.Add(e)
		}
	}
}

// underAny reports whether p is one of the subtrees or lies beneath one.
func underAny(p string, subtrees []string) bool {
	for _, sub := range subtrees {
		if p == sub || strings.HasPrefix(p, sub+"/") {
			return true
		}
	}
	return false
}
