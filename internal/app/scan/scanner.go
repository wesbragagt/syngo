package scan

import (
	"context"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

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
func (s *Scanner) Scan(ctx context.Context, f folder.Folder, o Options) (Result, error) {
	started := s.clock.Now()
	result := Result{FolderID: f.ID, DryRun: o.DryRun}

	matcher, err := LoadIgnore(ctx, s.fs, f)
	if err != nil {
		return Result{}, err
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
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	result.Changes = manifest.Diff(old, w.cur)
	now := s.clock.Now()
	result.Duration = now.Sub(started)

	if !o.DryRun {
		if err := s.persist(ctx, f, result.Changes, now); err != nil {
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
		"dry_run":   o.DryRun,
	})
	s.log.Debug("scan completed", "folder", f.ID, "changes", len(result.Changes),
		"scanned", result.Scanned, "hashed", result.Hashed, "dry_run", o.DryRun)
	return result, nil
}

// persist writes the classified changes and records the scan on the folder.
func (s *Scanner) persist(ctx context.Context, f folder.Folder, changes []manifest.Change, now time.Time) error {
	by := device.ID("")
	if s.identity != nil {
		by = s.identity.DeviceID()
	}

	var upserts []manifest.Entry
	var deleted []string
	for _, c := range changes {
		switch c.Type {
		case manifest.Create, manifest.Modify, manifest.Rename:
			e := *c.New
			e.ModifiedBy = by
			upserts = append(upserts, e)
			if c.Type == manifest.Rename {
				deleted = append(deleted, c.OldPath)
			}
		case manifest.Delete:
			deleted = append(deleted, c.Path)
		}
	}

	if err := s.store.UpsertEntries(ctx, f.ID, upserts); err != nil {
		return err
	}
	if err := s.store.MarkDeleted(ctx, f.ID, deleted, by, now); err != nil {
		return err
	}
	if err := s.store.AppendChanges(ctx, f.ID, changes, now); err != nil {
		return err
	}
	return s.store.SetFolderScan(ctx, f.ID, now, "")
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
