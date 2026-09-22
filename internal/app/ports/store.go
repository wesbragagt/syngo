package ports

import (
	"context"
	"errors"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// Store errors. Implementations wrap these so callers can use errors.Is.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	// ErrStaleManifest reports that the manifest a scan diffed against is no
	// longer the stored one, because another scan wrote in the meantime. The
	// caller reloads the manifest, re-diffs and writes again.
	ErrStaleManifest = errors.New("manifest changed since it was loaded")
)

// FolderStats summarizes the persisted contents of a folder, excluding tombstones.
type FolderStats struct {
	Files    int64
	Dirs     int64
	Symlinks int64
	Bytes    int64
}

// ChangeRecord is a persisted scan classification, as written by AppendChanges.
type ChangeRecord struct {
	Seq      int64
	FolderID folder.ID
	Type     manifest.ChangeType
	Path     string
	OldPath  string
	OldHash  manifest.Hash
	NewHash  manifest.Hash
	At       time.Time
}

// ScanWrite is everything one scan persists: the entries it found, the paths
// it did not find again, the classified changes and the outcome recorded on
// the folder.
type ScanWrite struct {
	// BaseSequence is the folder sequence the scan read from the manifest it
	// diffed against.
	BaseSequence uint64
	Upserts      []manifest.Entry
	Deleted      []string
	Changes      []manifest.Change
	// By is the device the new versions are attributed to.
	By device.ID
	// At is the scan timestamp, stored as the folder's last scan time.
	At time.Time
	// LastError is the failure summary recorded on the folder, empty when the
	// scan read everything it wanted to.
	LastError string
	// CounterFloor is the lowest counter a version written by By may carry.
	// It is what the tombstones of the write are raised to, so a deletion
	// made after a restore dominates the version it deletes everywhere.
	CounterFloor uint64
}

// TransferFilter narrows a transfer listing. Zero values mean "no filter".
type TransferFilter struct {
	FolderID folder.ID
	// Path narrows the listing to one path of the folder.
	Path  string
	State transfer.State
	Limit int
}

// ConflictRecord is a detected divergence between the local and remote version
// of a path.
//
// Path always names the file the folder knows by that name. ConflictPath names
// the copy that holds the other version, and is empty when there is none,
// which is the case when the peer deleted the path or replaced it by another
// kind. LocalHash and RemoteHash say which content each side had; matching
// them against what is on disk is how a resolution finds the version the user
// asked to keep, whichever of the two files holds it.
type ConflictRecord struct {
	ID           string
	FolderID     folder.ID
	Path         string
	ConflictPath string
	LocalHash    manifest.Hash
	RemoteHash   manifest.Hash
	PeerID       device.ID
	DetectedAt   time.Time
	Resolved     bool
	// Reason is the sync plan reason that produced the conflict, such as
	// "both-modified". It is what the user is shown to explain it.
	Reason string
}

// FolderStore persists folder configuration.
type FolderStore interface {
	SaveFolder(ctx context.Context, f folder.Folder) error
	// GetFolder returns ErrNotFound if no folder has that id.
	GetFolder(ctx context.Context, id folder.ID) (folder.Folder, error)
	ListFolders(ctx context.Context) ([]folder.Folder, error)
	DeleteFolder(ctx context.Context, id folder.ID) error
	SetFolderPaused(ctx context.Context, id folder.ID, paused bool) error
	SetFolderScan(ctx context.Context, id folder.ID, at time.Time, lastError string) error
	// SetFolderSyncError records what the last sync cycle of a folder
	// could not apply locally and how many items that was. An empty
	// message and a zero count mean the cycle applied everything. It is
	// the puller's half of the folder's health: it never touches the last
	// error the scanner records, and the scanner never touches this one.
	SetFolderSyncError(ctx context.Context, id folder.ID, lastError string, pending int) error
}

// FileStore persists folder contents and the change log.
type FileStore interface {
	LoadManifest(ctx context.Context, id folder.ID) (manifest.Manifest, error)
	// GetFile returns one stored entry, tombstones included, or ErrNotFound.
	GetFile(ctx context.Context, id folder.ID, path string) (manifest.Entry, error)
	// UpsertEntries assigns each entry the next folder sequence number.
	UpsertEntries(ctx context.Context, id folder.ID, entries []manifest.Entry) error
	// UpsertEntriesIf writes the entries whose stored version vector is
	// still the one the caller planned against, and returns the paths of
	// the entries it did not write because the stored version moved on.
	//
	// expect holds the version each path is expected to carry; a path that
	// has no stored row at all is expected to carry the zero vector. A path
	// missing from expect is written unconditionally, exactly as
	// UpsertEntries writes it. The comparison and the write happen
	// together, so a writer that lost the race is refused rather than
	// silently overwriting what the winner stored.
	UpsertEntriesIf(ctx context.Context, id folder.ID, entries []manifest.Entry,
		expect map[string]manifest.Version) (skipped []string, err error)
	MarkDeleted(ctx context.Context, id folder.ID, paths []string, by device.ID, now time.Time) error
	// SetVersion replaces the version vector of one entry without giving it
	// a new sequence number: the content did not change, only what is known
	// about which versions it descends from.
	SetVersion(ctx context.Context, id folder.ID, path string, v manifest.Version) error
	// SetSyncedMode records the permission bits last known to match the peer.
	SetSyncedMode(ctx context.Context, id folder.ID, path string, mode uint32) error
	FolderStats(ctx context.Context, id folder.ID) (FolderStats, error)
	AppendChanges(ctx context.Context, id folder.ID, changes []manifest.Change, now time.Time) error
	// ApplyScan writes the result of one scan atomically. It returns
	// ErrStaleManifest, and writes nothing, when the folder sequence moved
	// since the scan loaded the manifest.
	ApplyScan(ctx context.Context, id folder.ID, w ScanWrite) error
	// SyncFolderSequence raises the folder sequence counter above every
	// sequence already assigned to a file of that folder. It is a no-op when
	// the counter is already ahead.
	SyncFolderSequence(ctx context.Context, id folder.ID) error
	RecentChanges(ctx context.Context, id folder.ID, limit int) ([]ChangeRecord, error)
}

// CounterStore persists the highest version counter seen for a device.
//
// It is what keeps a device from reusing counters after its database was
// restored from a backup: the floor outlives the entries themselves, and every
// version this device writes is raised above it. Peers raise it too, because
// the counters they hold for this device are versions it once wrote.
type CounterStore interface {
	// CounterFloor returns the highest counter recorded for a device, zero
	// when none was ever recorded.
	CounterFloor(ctx context.Context, id device.ID) (uint64, error)
	// RaiseCounterFloor records to as the floor of a device when it is
	// higher than what is stored. It never lowers a floor.
	RaiseCounterFloor(ctx context.Context, id device.ID, to uint64) error
}

// DeviceStore persists known peers.
type DeviceStore interface {
	SaveDevice(ctx context.Context, d device.Device) error
	// GetDevice returns ErrNotFound if no device has that id.
	GetDevice(ctx context.Context, id device.ID) (device.Device, error)
	ListDevices(ctx context.Context) ([]device.Device, error)
	DeleteDevice(ctx context.Context, id device.ID) error
	TouchDevice(ctx context.Context, id device.ID, seenAt time.Time) error
}

// TransferStore persists inbound transfers and their chunk state.
type TransferStore interface {
	// SaveTransfer upserts the transfer together with its chunk rows.
	SaveTransfer(ctx context.Context, t transfer.Transfer) error
	GetTransfer(ctx context.Context, id transfer.ID) (transfer.Transfer, error)
	// FindLiveTransfer returns the transfer for a path that is neither done
	// nor failed, or ErrNotFound.
	FindLiveTransfer(ctx context.Context, id folder.ID, path string) (transfer.Transfer, error)
	ListTransfers(ctx context.Context, filter TransferFilter) ([]transfer.Transfer, error)
	MarkChunkVerified(ctx context.Context, id transfer.ID, idx int, h manifest.Hash, now time.Time) error
	SetTransferState(ctx context.Context, id transfer.ID, s transfer.State, errMsg string, now time.Time) error
	DeleteTransfer(ctx context.Context, id transfer.ID) error
}

// ConflictStore persists detected conflicts.
type ConflictStore interface {
	SaveConflict(ctx context.Context, c ConflictRecord) error
	// GetConflict returns one conflict, or ErrNotFound.
	GetConflict(ctx context.Context, id string) (ConflictRecord, error)
	ListConflicts(ctx context.Context, id folder.ID, includeResolved bool) ([]ConflictRecord, error)
	// SetConflictResolved marks a conflict as dealt with, or as open again.
	// It returns ErrNotFound when no conflict has that id.
	SetConflictResolved(ctx context.Context, id string, resolved bool) error
}

// MetadataStore is the full persistence port used by the daemon and the CLI.
type MetadataStore interface {
	FolderStore
	FileStore
	CounterStore
	DeviceStore
	TransferStore
	ConflictStore
	Close() error
}
