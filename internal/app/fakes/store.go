package fakes

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// Store is an in-memory ports.MetadataStore.
type Store struct {
	// applyMu serializes ApplyScan calls so the sequence check and the writes
	// that follow it cannot interleave with another scan.
	applyMu   sync.Mutex
	mu        sync.Mutex
	folders   map[folder.ID]folder.Folder
	files     map[folder.ID]map[string]manifest.Entry
	sequences map[folder.ID]uint64
	changes   map[folder.ID][]ports.ChangeRecord
	devices   map[device.ID]device.Device
	transfers map[transfer.ID]transfer.Transfer
	conflicts []ports.ConflictRecord
	changeSeq int64
	closed    bool
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		folders:   make(map[folder.ID]folder.Folder),
		files:     make(map[folder.ID]map[string]manifest.Entry),
		sequences: make(map[folder.ID]uint64),
		changes:   make(map[folder.ID][]ports.ChangeRecord),
		devices:   make(map[device.ID]device.Device),
		transfers: make(map[transfer.ID]transfer.Transfer),
	}
}

// Closed reports whether Close has been called.
func (s *Store) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close marks the store closed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// SaveFolder inserts or replaces a folder.
func (s *Store) SaveFolder(_ context.Context, f folder.Folder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, other := range s.folders {
		if id != f.ID && other.Path == f.Path {
			return fmt.Errorf("folder path %q: %w", f.Path, ports.ErrAlreadyExists)
		}
	}
	s.folders[f.ID] = f
	if _, ok := s.files[f.ID]; !ok {
		s.files[f.ID] = make(map[string]manifest.Entry)
	}
	return nil
}

// GetFolder returns a folder by id.
func (s *Store) GetFolder(_ context.Context, id folder.ID) (folder.Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.folders[id]
	if !ok {
		return folder.Folder{}, fmt.Errorf("folder %q: %w", id, ports.ErrNotFound)
	}
	return f, nil
}

// ListFolders returns every folder, ordered by id.
func (s *Store) ListFolders(_ context.Context) ([]folder.Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]folder.Folder, 0, len(s.folders))
	for _, f := range s.folders {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DeleteFolder removes a folder and everything belonging to it.
func (s *Store) DeleteFolder(_ context.Context, id folder.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.folders[id]; !ok {
		return fmt.Errorf("folder %q: %w", id, ports.ErrNotFound)
	}
	delete(s.folders, id)
	delete(s.files, id)
	delete(s.sequences, id)
	delete(s.changes, id)
	for tid, t := range s.transfers {
		if t.FolderID == id {
			delete(s.transfers, tid)
		}
	}
	return nil
}

// SetFolderPaused sets the paused flag of a folder.
func (s *Store) SetFolderPaused(_ context.Context, id folder.ID, paused bool) error {
	return s.updateFolder(id, func(f *folder.Folder) { f.Paused = paused })
}

// SetFolderScan records the outcome of a scan.
func (s *Store) SetFolderScan(_ context.Context, id folder.ID, at time.Time, lastError string) error {
	return s.updateFolder(id, func(f *folder.Folder) {
		f.LastScanAt = at
		f.LastError = lastError
	})
}

// updateFolder applies fn to a stored folder.
func (s *Store) updateFolder(id folder.ID, fn func(*folder.Folder)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.folders[id]
	if !ok {
		return fmt.Errorf("folder %q: %w", id, ports.ErrNotFound)
	}
	fn(&f)
	s.folders[id] = f
	return nil
}

// LoadManifest returns the stored contents of a folder, including tombstones.
func (s *Store) LoadManifest(_ context.Context, id folder.ID) (manifest.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := manifest.New(id)
	for p, e := range s.files[id] {
		m.Entries[p] = e
	}
	m.Sequence = s.sequences[id]
	return m, nil
}

// UpsertEntries stores entries, assigning each the next folder sequence number.
func (s *Store) UpsertEntries(_ context.Context, id folder.ID, entries []manifest.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.files[id]; !ok {
		s.files[id] = make(map[string]manifest.Entry)
	}
	for _, e := range entries {
		s.sequences[id]++
		e.Sequence = s.sequences[id]
		s.files[id][e.Path] = e
	}
	return nil
}

// MarkDeleted turns the given paths into tombstones.
func (s *Store) MarkDeleted(_ context.Context, id folder.ID, paths []string, by device.ID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range paths {
		e, ok := s.files[id][p]
		if !ok {
			continue
		}
		s.sequences[id]++
		e.Deleted = true
		e.Hash = manifest.Hash{}
		e.Size = 0
		e.ModifiedBy = by
		e.ModTime = now
		e.Sequence = s.sequences[id]
		s.files[id][p] = e
	}
	return nil
}

// SetSyncedHash records the hash last known to match the peer.
func (s *Store) SetSyncedHash(_ context.Context, id folder.ID, p string, h manifest.Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.files[id][p]
	if !ok {
		return fmt.Errorf("file %q in folder %q: %w", p, id, ports.ErrNotFound)
	}
	e.SyncedHash = h
	s.files[id][p] = e
	return nil
}

// FolderStats counts the live entries of a folder.
func (s *Store) FolderStats(_ context.Context, id folder.ID) (ports.FolderStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var stats ports.FolderStats
	for _, e := range s.files[id] {
		if e.Deleted {
			continue
		}
		switch e.Kind {
		case manifest.KindFile:
			stats.Files++
			stats.Bytes += e.Size
		case manifest.KindDir:
			stats.Dirs++
		case manifest.KindSymlink:
			stats.Symlinks++
		}
	}
	return stats, nil
}

// AppendChanges records scan classifications.
func (s *Store) AppendChanges(_ context.Context, id folder.ID, changes []manifest.Change, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range changes {
		s.changeSeq++
		rec := ports.ChangeRecord{
			Seq:      s.changeSeq,
			FolderID: id,
			Type:     c.Type,
			Path:     c.Path,
			OldPath:  c.OldPath,
			At:       now,
		}
		if c.Old != nil {
			rec.OldHash = c.Old.Hash
		}
		if c.New != nil {
			rec.NewHash = c.New.Hash
		}
		s.changes[id] = append(s.changes[id], rec)
	}
	return nil
}

// ApplyScan writes one scan result atomically, refusing the write when the
// folder sequence moved since the scan loaded the manifest.
func (s *Store) ApplyScan(ctx context.Context, id folder.ID, w ports.ScanWrite) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	s.mu.Lock()
	if current := s.sequences[id]; current != w.BaseSequence {
		s.mu.Unlock()
		return fmt.Errorf("folder %q at sequence %d, scan started from %d: %w",
			id, current, w.BaseSequence, ports.ErrStaleManifest)
	}
	s.mu.Unlock()

	if err := s.UpsertEntries(ctx, id, w.Upserts); err != nil {
		return err
	}
	if err := s.MarkDeleted(ctx, id, w.Deleted, w.By, w.At); err != nil {
		return err
	}
	if err := s.AppendChanges(ctx, id, w.Changes, w.At); err != nil {
		return err
	}
	return s.SetFolderScan(ctx, id, w.At, w.LastError)
}

// SyncFolderSequence raises the folder sequence counter above every sequence
// already assigned to a file of that folder.
func (s *Store) SyncFolderSequence(_ context.Context, id folder.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var maxSeq uint64
	for _, e := range s.files[id] {
		if e.Sequence > maxSeq {
			maxSeq = e.Sequence
		}
	}
	if maxSeq > s.sequences[id] {
		s.sequences[id] = maxSeq
	}
	return nil
}

// SetSequence forces the folder sequence counter, so tests can simulate a
// counter that fell behind the rows it should be ahead of.
func (s *Store) SetSequence(id folder.ID, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequences[id] = seq
}

// RecentChanges returns the most recent change records, newest first.
func (s *Store) RecentChanges(_ context.Context, id folder.ID, limit int) ([]ports.ChangeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.changes[id]
	out := make([]ports.ChangeRecord, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, all[i])
	}
	return out, nil
}

// SaveDevice inserts or replaces a device.
func (s *Store) SaveDevice(_ context.Context, d device.Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[d.ID] = d
	return nil
}

// GetDevice returns a device by id.
func (s *Store) GetDevice(_ context.Context, id device.ID) (device.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return device.Device{}, fmt.Errorf("device %q: %w", id, ports.ErrNotFound)
	}
	return d, nil
}

// ListDevices returns every device, ordered by id.
func (s *Store) ListDevices(_ context.Context) ([]device.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]device.Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DeleteDevice removes a device.
func (s *Store) DeleteDevice(_ context.Context, id device.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devices[id]; !ok {
		return fmt.Errorf("device %q: %w", id, ports.ErrNotFound)
	}
	delete(s.devices, id)
	return nil
}

// TouchDevice records when a device was last seen.
func (s *Store) TouchDevice(_ context.Context, id device.ID, seenAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return fmt.Errorf("device %q: %w", id, ports.ErrNotFound)
	}
	d.LastSeenAt = seenAt
	s.devices[id] = d
	return nil
}

// SaveTransfer inserts or replaces a transfer and its chunks.
func (s *Store) SaveTransfer(_ context.Context, t transfer.Transfer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Chunks = append([]transfer.Chunk(nil), t.Chunks...)
	s.transfers[t.ID] = t
	return nil
}

// GetTransfer returns a transfer by id.
func (s *Store) GetTransfer(_ context.Context, id transfer.ID) (transfer.Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[id]
	if !ok {
		return transfer.Transfer{}, fmt.Errorf("transfer %q: %w", id, ports.ErrNotFound)
	}
	return t, nil
}

// FindLiveTransfer returns the unfinished transfer for a path.
func (s *Store) FindLiveTransfer(_ context.Context, id folder.ID, p string) (transfer.Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.transfers {
		if t.FolderID == id && t.Path == p && t.State != transfer.StateDone && t.State != transfer.StateFailed {
			return t, nil
		}
	}
	return transfer.Transfer{}, fmt.Errorf("live transfer for %q in folder %q: %w", p, id, ports.ErrNotFound)
}

// ListTransfers returns transfers matching the filter, ordered by id.
func (s *Store) ListTransfers(_ context.Context, filter ports.TransferFilter) ([]transfer.Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []transfer.Transfer
	for _, t := range s.transfers {
		if filter.FolderID != "" && t.FolderID != filter.FolderID {
			continue
		}
		if filter.State != "" && t.State != filter.State {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// MarkChunkVerified records a verified chunk of a transfer.
func (s *Store) MarkChunkVerified(_ context.Context, id transfer.ID, idx int, h manifest.Hash, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[id]
	if !ok {
		return fmt.Errorf("transfer %q: %w", id, ports.ErrNotFound)
	}
	if err := t.MarkVerified(idx, h, now); err != nil {
		return err
	}
	s.transfers[id] = t
	return nil
}

// SetTransferState updates the state and error message of a transfer.
func (s *Store) SetTransferState(_ context.Context, id transfer.ID, state transfer.State, errMsg string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[id]
	if !ok {
		return fmt.Errorf("transfer %q: %w", id, ports.ErrNotFound)
	}
	t.State = state
	t.Error = errMsg
	t.UpdatedAt = now
	s.transfers[id] = t
	return nil
}

// DeleteTransfer removes a transfer.
func (s *Store) DeleteTransfer(_ context.Context, id transfer.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.transfers[id]; !ok {
		return fmt.Errorf("transfer %q: %w", id, ports.ErrNotFound)
	}
	delete(s.transfers, id)
	return nil
}

// SaveConflict records a conflict.
func (s *Store) SaveConflict(_ context.Context, c ports.ConflictRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.conflicts {
		if existing.ID == c.ID {
			s.conflicts[i] = c
			return nil
		}
	}
	s.conflicts = append(s.conflicts, c)
	return nil
}

// ListConflicts returns conflicts for a folder, or for all folders when id is empty.
func (s *Store) ListConflicts(_ context.Context, id folder.ID, includeResolved bool) ([]ports.ConflictRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ports.ConflictRecord
	for _, c := range s.conflicts {
		if id != "" && c.FolderID != id {
			continue
		}
		if c.Resolved && !includeResolved {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}
