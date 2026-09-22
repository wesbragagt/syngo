// Package repair restores consistency between the stored folder metadata and
// what is actually on disk.
package repair

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// TempDir is the folder-relative directory holding partial downloads.
const TempDir = ".syncthis/tmp"

// ErrRootMissing reports a folder root that does not exist or is not a
// directory. Repair refuses to run rather than report every stored entry as
// missing, which would mark the whole folder deleted and propagate that to the
// peers.
var ErrRootMissing = errors.New("folder root does not exist or is not a directory")

// Finding is a problem repair could not fix on its own.
type Finding struct {
	Path   string
	Reason string
}

// Report lists what a repair run found, and applied unless it was a dry run.
type Report struct {
	FolderID folder.ID
	DryRun   bool
	// MissingDeleted are paths recorded as live whose file is gone.
	MissingDeleted []string
	// Rehashed are paths whose stored size or hash no longer matched disk.
	Rehashed []string
	// TransfersReset are transfers whose partial file disappeared.
	TransfersReset []string
	// TransfersPruned are journal rows worth nothing any more: a finished
	// download whose row outlived it, and a failed transfer with nothing
	// left to inspect, whose partial file is gone or whose path already
	// holds the content it was downloading.
	TransfersPruned []string
	// OrphanTemps are partial files no live transfer claims.
	OrphanTemps []string
	// SequenceFixed reports that the folder sequence counter had fallen
	// behind the rows it numbers.
	SequenceFixed bool
	// Unrecoverable lists paths repair could neither read nor classify.
	Unrecoverable []Finding
}

// Changed reports whether the folder needed (or received) any change.
func (r Report) Changed() bool {
	return len(r.MissingDeleted) > 0 || len(r.Rehashed) > 0 || len(r.TransfersReset) > 0 ||
		len(r.TransfersPruned) > 0 || len(r.OrphanTemps) > 0 || r.SequenceFixed
}

// Service checks one folder and repairs what it can.
type Service struct {
	fs       ports.FileSystem
	hasher   ports.Hasher
	store    ports.MetadataStore
	clock    ports.Clock
	identity ports.Identity
}

// New returns a repair service.
func New(fs ports.FileSystem, hasher ports.Hasher, store ports.MetadataStore, clock ports.Clock, identity ports.Identity) *Service {
	return &Service{fs: fs, hasher: hasher, store: store, clock: clock, identity: identity}
}

// Run inspects folderID and, unless dryRun is set, writes the repairs back.
func (s *Service) Run(ctx context.Context, folderID folder.ID, dryRun bool) (Report, error) {
	f, err := s.store.GetFolder(ctx, folderID)
	if err != nil {
		return Report{}, err
	}
	if err := s.checkRoot(f); err != nil {
		return Report{}, err
	}
	report := Report{FolderID: folderID, DryRun: dryRun}

	m, err := s.store.LoadManifest(ctx, folderID)
	if err != nil {
		return Report{}, err
	}
	rehashed, err := s.inspectFiles(ctx, f, m, &report)
	if err != nil {
		return Report{}, err
	}
	transfers, resets, pruned, err := s.inspectTransfers(ctx, f, m, &report)
	if err != nil {
		return Report{}, err
	}
	orphans, err := s.inspectTemp(ctx, f, transfers, &report)
	if err != nil {
		return Report{}, err
	}
	report.SequenceFixed = sequenceBehind(m)

	if dryRun {
		return report, nil
	}
	if err := s.apply(ctx, folderID, report, rehashed, resets, pruned, orphans); err != nil {
		return Report{}, err
	}
	return report, nil
}

// checkRoot reports whether the folder root is still a directory. A root that
// was removed is not a folder whose contents were all deleted.
func (s *Service) checkRoot(f folder.Folder) error {
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

// inspectFiles checks every live entry against disk and returns the entries
// whose metadata must be rewritten.
func (s *Service) inspectFiles(ctx context.Context, f folder.Folder, m manifest.Manifest, report *Report) ([]manifest.Entry, error) {
	var rehashed []manifest.Entry
	for _, e := range m.Live() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		full := path.Join(f.Path, e.Path)
		info, err := s.fs.Lstat(full)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			report.MissingDeleted = append(report.MissingDeleted, e.Path)
			continue
		case err != nil:
			report.Unrecoverable = append(report.Unrecoverable, Finding{Path: e.Path, Reason: err.Error()})
			continue
		}
		if e.Kind != manifest.KindFile || !info.Mode.IsRegular() {
			continue
		}
		if info.Size == e.Size && info.ModTime.UnixNano() == e.ModTime.UnixNano() {
			continue
		}

		hash, err := s.hash(full)
		if err != nil {
			report.Unrecoverable = append(report.Unrecoverable, Finding{Path: e.Path, Reason: err.Error()})
			continue
		}
		updated := e
		updated.Size = info.Size
		updated.ModTime = info.ModTime
		updated.Mode = uint32(info.Mode.Perm())
		updated.Inode = info.Inode
		updated.Hash = hash
		rehashed = append(rehashed, updated)
		report.Rehashed = append(report.Rehashed, e.Path)
	}
	return rehashed, nil
}

// inspectTransfers returns every transfer of the folder, those whose partial
// file is gone with their chunk state cleared, and the failed rows worth
// nothing any more.
func (s *Service) inspectTransfers(ctx context.Context, f folder.Folder, m manifest.Manifest,
	report *Report) (all, resets []transfer.Transfer, pruned []transfer.Transfer, err error) {
	list, err := s.store.ListTransfers(ctx, ports.TransferFilter{FolderID: f.ID})
	if err != nil {
		return nil, nil, nil, err
	}

	for _, t := range list {
		if t.State == transfer.StateDone {
			// A finished download is recorded in the files table;
			// its row only kept the journal alive. One that is
			// still here was left by a process that stopped
			// between the install and the deletion of the row, and
			// nothing else would ever remove it.
			pruned = append(pruned, t)
			report.TransfersPruned = append(report.TransfersPruned, string(t.ID))
			continue
		}
		if t.State == transfer.StateFailed {
			if s.spent(f, m, t) {
				pruned = append(pruned, t)
				report.TransfersPruned = append(report.TransfersPruned, string(t.ID))
			}
			continue
		}
		if _, err := s.fs.Lstat(t.TempPath); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			report.Unrecoverable = append(report.Unrecoverable, Finding{Path: t.TempPath, Reason: err.Error()})
			continue
		}
		t.ResetChunks()
		t.State = transfer.StatePending
		t.Error = ""
		resets = append(resets, t)
		report.TransfersReset = append(report.TransfersReset, string(t.ID))
	}
	return list, resets, pruned, nil
}

// spent reports whether a failed transfer has nothing left to say. A failed
// row is kept so the bytes that did not hash right can be looked at, which is
// pointless once the partial file is gone or the path holds the content the
// transfer was after.
func (s *Service) spent(f folder.Folder, m manifest.Manifest, t transfer.Transfer) bool {
	if _, err := s.fs.Lstat(t.TempPath); errors.Is(err, fs.ErrNotExist) {
		return true
	}
	e, ok := m.Entries[t.Path]
	if !ok || e.Deleted || e.Kind != manifest.KindFile {
		return false
	}
	return e.Hash == t.Hash
}

// inspectTemp lists the partial files under <root>/.syncthis/tmp that no
// unfinished transfer claims.
func (s *Service) inspectTemp(ctx context.Context, f folder.Folder, transfers []transfer.Transfer, report *Report) ([]string, error) {
	claimed := make(map[string]bool, len(transfers))
	for _, t := range transfers {
		// A failed transfer claims its partial file too: those are the
		// bytes that did not hash right, kept for as long as the row
		// is. The row and the file go together, in the prune pass.
		if t.State != transfer.StateDone {
			claimed[t.TempPath] = true
		}
	}

	tmpRoot := path.Join(f.Path, TempDir)
	var orphans []string
	err := s.fs.Walk(ctx, tmpRoot, func(rel string, info ports.FileInfo) error {
		if rel == "" || info.IsDir() {
			return nil
		}
		full := path.Join(tmpRoot, rel)
		if !claimed[full] {
			orphans = append(orphans, full)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect %s: %w", tmpRoot, err)
	}
	sort.Strings(orphans)
	report.OrphanTemps = orphans
	return orphans, nil
}

// apply writes the repairs back to the store and the filesystem.
func (s *Service) apply(ctx context.Context, id folder.ID, report Report,
	rehashed []manifest.Entry, resets, pruned []transfer.Transfer, orphans []string) error {
	now := s.clock.Now()
	by := device.ID("")
	if s.identity != nil {
		by = s.identity.DeviceID()
	}

	if len(report.MissingDeleted) > 0 {
		if err := s.store.MarkDeleted(ctx, id, report.MissingDeleted, by, now); err != nil {
			return err
		}
	}
	if len(rehashed) > 0 {
		for i := range rehashed {
			// The content on disk is a version this device holds
			// and no peer has seen, so it is attributed to this
			// device and raises its counter.
			rehashed[i].ModifiedBy = by
			rehashed[i].Version = rehashed[i].Version.Bump(by)
		}
		if err := s.store.UpsertEntries(ctx, id, rehashed); err != nil {
			return err
		}
	}
	if err := s.store.AppendChanges(ctx, id, repairChanges(report), now); err != nil {
		return err
	}
	for _, t := range resets {
		if err := s.store.SaveTransfer(ctx, t); err != nil {
			return err
		}
		if err := s.store.SetTransferState(ctx, t.ID, transfer.StatePending, "", now); err != nil {
			return err
		}
	}
	for _, t := range pruned {
		// The partial file goes with the row; one that is already gone
		// is what made the row worth nothing in the first place.
		if err := s.fs.Remove(t.TempPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove partial file %s: %w", t.TempPath, err)
		}
		if err := s.store.DeleteTransfer(ctx, t.ID); err != nil {
			return err
		}
	}
	for _, p := range orphans {
		if err := s.fs.Remove(p); err != nil {
			return fmt.Errorf("remove orphan %s: %w", p, err)
		}
	}
	if report.SequenceFixed {
		return s.store.SyncFolderSequence(ctx, id)
	}
	return nil
}

// repairChanges turns the repaired paths into change log entries, so the
// folder history explains why the manifest moved.
func repairChanges(report Report) []manifest.Change {
	changes := make([]manifest.Change, 0, len(report.MissingDeleted)+len(report.Rehashed))
	for _, p := range report.MissingDeleted {
		changes = append(changes, manifest.Change{Type: manifest.Delete, Path: p})
	}
	for _, p := range report.Rehashed {
		changes = append(changes, manifest.Change{Type: manifest.Modify, Path: p})
	}
	return changes
}

// hash reads a file and returns the hash of its contents.
func (s *Service) hash(p string) (manifest.Hash, error) {
	rc, err := s.fs.Open(p)
	if err != nil {
		return manifest.Hash{}, err
	}
	defer func() { _ = rc.Close() }()

	h, err := s.hasher.Sum(rc)
	if err != nil {
		return manifest.Hash{}, fmt.Errorf("hash %s: %w", p, err)
	}
	return h, nil
}

// sequenceBehind reports whether the folder sequence counter is lower than the
// highest sequence already handed out to a file.
func sequenceBehind(m manifest.Manifest) bool {
	var maxSeq uint64
	for _, e := range m.Entries {
		if e.Sequence > maxSeq {
			maxSeq = e.Sequence
		}
	}
	return maxSeq > m.Sequence
}
