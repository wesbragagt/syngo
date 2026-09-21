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

// TransferFilter narrows a transfer listing. Zero values mean "no filter".
type TransferFilter struct {
	FolderID folder.ID
	State    transfer.State
	Limit    int
}

// ConflictRecord is a detected divergence between the local and remote version
// of a path.
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
}

// FileStore persists folder contents and the change log.
type FileStore interface {
	LoadManifest(ctx context.Context, id folder.ID) (manifest.Manifest, error)
	// UpsertEntries assigns each entry the next folder sequence number.
	UpsertEntries(ctx context.Context, id folder.ID, entries []manifest.Entry) error
	MarkDeleted(ctx context.Context, id folder.ID, paths []string, by device.ID, now time.Time) error
	SetSyncedHash(ctx context.Context, id folder.ID, path string, h manifest.Hash) error
	FolderStats(ctx context.Context, id folder.ID) (FolderStats, error)
	AppendChanges(ctx context.Context, id folder.ID, changes []manifest.Change, now time.Time) error
	RecentChanges(ctx context.Context, id folder.ID, limit int) ([]ChangeRecord, error)
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
	ListConflicts(ctx context.Context, id folder.ID, includeResolved bool) ([]ConflictRecord, error)
}

// MetadataStore is the full persistence port used by the daemon and the CLI.
type MetadataStore interface {
	FolderStore
	FileStore
	DeviceStore
	TransferStore
	ConflictStore
	Close() error
}
