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

	"github.com/wesbragagt/syncthis/internal/app/mergebase"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/chunker"
	"github.com/wesbragagt/syncthis/internal/domain/counterfloor"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
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
	// chunks is how files are cut for the chunk index. The scan is what
	// reads a file's bytes, so it is where the cutting costs nothing extra.
	chunks chunker.Config
	// bases keeps the content of every small text file this scan records,
	// so a later divergence on that path has a version to merge against.
	// It is nil unless the text merge setting is on, and then nothing is
	// kept and nothing is written.
	bases *mergebase.Recorder
	// trust says whether the counter floor the versions of this scan are
	// raised from has been confirmed against a peer. A nil trust reads as
	// confirmed, which is what a scanner nobody wired one into holds.
	trust *counterfloor.Trust
	log   *slog.Logger
}

// SetMergeBases makes the scan keep the content of the text files it records,
// which is what the three-way merge later merges against. A nil recorder, the
// state of a device with the setting off, keeps nothing.
func (s *Scanner) SetMergeBases(r *mergebase.Recorder) { s.bases = r }

// SetCounterFloorTrust hands the scanner the record of whether a peer has
// confirmed this device's counter floor. Versions written while it has not are
// marked provisional on the entry, so a peer version that appears to descend
// from one of them is kept as a conflict rather than replacing it.
func (s *Scanner) SetCounterFloorTrust(t *counterfloor.Trust) { s.trust = t }

// New returns a scanner. chunkSize is the average chunk size this device cuts
// with; zero takes the default. log may be nil.
func New(fs ports.FileSystem, hasher ports.Hasher, store ports.MetadataStore, clock ports.Clock,
	identity ports.Identity, events ports.EventSink, chunkSize int, log *slog.Logger) *Scanner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Scanner{fs: fs, hasher: hasher, store: store, clock: clock, identity: identity,
		events: events, chunks: chunker.ConfigFor(chunkSize), log: log}
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

	// A file whose stored entry still describes what is on disk is not read
	// again, so the index has to say which of those it already covers: in a
	// database written before the chunk index existed that is none of them,
	// and the first scan after the upgrade fills it in without a
	// re-download.
	indexed, err := s.store.IndexedPaths(ctx, f.ID)
	if err != nil {
		return Result{}, err
	}

	w := &walker{
		scanner: s, folder: f, opts: o, matcher: matcher,
		old: old, cur: manifest.New(f.ID), result: &result,
		indexed: indexed, chunks: make(map[string][]transfer.Chunk),
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
		if err := s.persist(ctx, f, old, w.cur, w.chunks, &result, now); err != nil {
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
// after another writer got to the folder first. A device catching up on
// thousands of files writes as fast as the puller installs them, so the count
// is what it takes to get through such a burst rather than the one or two a
// concurrent scan would need; a scan that still loses is rescheduled by the
// folder loop.
const persistRetries = 5

// persist writes the classified changes and records the scan on the folder in
// one transaction. A concurrent scan that wrote first makes the store reject
// the write; the scan then reloads the manifest, re-diffs the tree it already
// walked and tries again, so the change log never records the same scan twice.
func (s *Scanner) persist(ctx context.Context, f folder.Folder, old, cur manifest.Manifest,
	chunks map[string][]transfer.Chunk, result *Result, now time.Time) error {
	base := old.Sequence
	// The floor is the highest counter this device has ever handed out or
	// seen a peer hold for it. Every version this scan writes is raised
	// above it, so a database restored from a backup does not hand out
	// counters its peers already know, which would make the edit that
	// follows the restore look older than what they hold.
	floor, err := s.counterFloor(ctx)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		write := s.writeOf(result, old, base, now, floor)
		write.Chunks = chunks
		err := s.store.ApplyScan(ctx, f.ID, write)
		if err == nil {
			// The store hands out one sequence number per written
			// row, in order, starting at the base.
			result.Sequence = base + uint64(len(write.Upserts)+len(write.Deleted))
			s.raiseFloor(ctx, write)
			// The versions this scan wrote are what a peer will
			// diverge from, so their content is kept as the base a
			// later merge runs against.
			for _, e := range write.Upserts {
				s.bases.Remember(ctx, f, e)
			}
			return nil
		}
		if !errors.Is(err, ports.ErrStaleManifest) || attempt >= persistRetries {
			return err
		}
		s.log.Debug("folder changed during scan, re-diffing", "folder", f.ID, "attempt", attempt+1)

		reloaded, loadErr := s.store.LoadManifest(ctx, f.ID)
		if loadErr != nil {
			return loadErr
		}
		// The versions the write raises are the ones of the manifest it
		// diffed against, so the reloaded manifest replaces the stale
		// one for both the diff and the vectors.
		old = reloaded
		base = old.Sequence
		result.Changes = manifest.Diff(old, cur)
	}
}

// writeOf turns classified changes into the write the store applies. old is
// the manifest the changes were diffed against: it holds the version vector
// each new version is raised from.
func (s *Scanner) writeOf(result *Result, old manifest.Manifest, base uint64, now time.Time,
	floor uint64) ports.ScanWrite {
	by := s.self()
	// Nothing has confirmed the floor these versions are raised from, so
	// the counters they carry may be ones the peers already hold for this
	// device. The entries say so; the sync plan does the rest.
	provisional := !s.trust.Verified()

	w := ports.ScanWrite{
		BaseSequence: base,
		Changes:      result.Changes,
		By:           by,
		At:           now,
		LastError:    result.Warning(),
		CounterFloor: floor,
		Provisional:  provisional,
	}
	for _, c := range result.Changes {
		switch c.Type {
		case manifest.Create, manifest.Modify, manifest.Rename:
			e := *c.New
			e.ModifiedBy = by
			prev := old.Entries[c.Path]
			e.Version = versionOf(c, old, by, floor)
			// Only a change that hands out a new counter can have
			// been given one the peers already hold. A change that
			// leaves the vector alone, which is what a
			// permission-only change does, keeps whatever the
			// entry already carried.
			e.Provisional = prev.Provisional
			if !e.Version.Equals(prev.Version) {
				e.Provisional = provisional
			}
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

// versionOf is the vector the new version of a path carries. A content change
// raises this device's counter over the version the path had here and over the
// counter floor, so the new version dominates both; a change that only moves
// permission bits keeps the vector it found, because a mode is not content and
// must not order two versions. A create raises the vector of whatever the path held before, a
// tombstone included, so a recreated file dominates its own deletion.
func versionOf(c manifest.Change, old manifest.Manifest, by device.ID, floor uint64) manifest.Version {
	if c.Type == manifest.Modify && c.Old != nil && c.New != nil && !manifest.ContentDiffers(*c.Old, *c.New) {
		return c.Old.Version
	}
	// The counter is raised over the floor as well as over the version the
	// path held, so it is one no peer can already hold for this device.
	return old.Entries[c.Path].Version.Bump(by).Raise(by, floor+1)
}

// self is the device this scanner attributes versions to, empty when it has no
// identity.
func (s *Scanner) self() device.ID {
	if s.identity == nil {
		return ""
	}
	return s.identity.DeviceID()
}

// counterFloor reads the highest counter recorded for this device.
func (s *Scanner) counterFloor(ctx context.Context) (uint64, error) {
	self := s.self()
	if self == "" {
		return 0, nil
	}
	floor, err := s.store.CounterFloor(ctx, self)
	if err != nil {
		return 0, fmt.Errorf("counter floor of %s: %w", self.Short(), err)
	}
	return floor, nil
}

// raiseFloor records the highest counter the write handed out. It runs after
// the write, so a crash between the two leaves a floor below what the entries
// carry; the next scan raises its versions from the entries themselves, which
// are at least as high, so nothing is reused.
func (s *Scanner) raiseFloor(ctx context.Context, w ports.ScanWrite) {
	self := s.self()
	if self == "" {
		return
	}
	highest := w.CounterFloor
	for _, e := range w.Upserts {
		highest = max(highest, e.Version.Counter(self))
	}
	// A tombstone's counter is raised by the store, one above the version
	// it replaces and never below the floor the write carried.
	if len(w.Deleted) > 0 {
		highest = max(highest, w.CounterFloor+1)
	}
	if err := s.store.RaiseCounterFloor(ctx, self, highest); err != nil && ctx.Err() == nil {
		s.log.Warn("counter floor not recorded", "device", self.Short(), "error", err)
	}
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
