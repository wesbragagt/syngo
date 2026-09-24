package ports

import (
	"context"
	"errors"
	"time"

	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
	"github.com/wesbragagt/syngo/internal/domain/transfer"
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
	// Provisional marks the tombstones of the write as written from a
	// counter floor no peer had confirmed yet, which is what every version
	// written between a restore from a backup and the first contact looks
	// like. The upserts carry the same mark on the entry itself, because a
	// change that only moved permission bits hands out no counter and is
	// not provisional. See manifest.Entry.Provisional.
	Provisional bool
	// Chunks is how the scan cut each file it read, by path, each with the
	// hash of the content it was cut from. A path that
	// is in Upserts but not here kept the chunks the index already has,
	// which is what an unchanged file does. The chunk index is part of the
	// scan's write so a folder never records content it cannot describe.
	Chunks map[string]transfer.ChunkIndex
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
	//
	// provisional replaces manifest.Entry.Provisional as well. The only
	// caller is the agreement a sync cycle records when both sides already
	// hold the same content, which is exactly the moment a version written
	// from an unconfirmed counter floor stops being in doubt: a peer has
	// been seen holding it.
	SetVersion(ctx context.Context, id folder.ID, path string, v manifest.Version, provisional bool) error
	// SetSyncedMode records the permission bits last known to match the peer.
	SetSyncedMode(ctx context.Context, id folder.ID, path string, mode uint32) error
	FolderStats(ctx context.Context, id folder.ID) (FolderStats, error)
	AppendChanges(ctx context.Context, id folder.ID, changes []manifest.Change, now time.Time) error
	// ApplyScan writes the result of one scan atomically. It returns
	// ErrStaleManifest, and writes nothing, when the folder sequence moved
	// since the scan loaded the manifest.
	ApplyScan(ctx context.Context, id folder.ID, w ScanWrite) error
	// FolderSequence returns the highest sequence number ever handed out in
	// a folder, zero when nothing was written for it. It costs one row,
	// where loading the manifest costs the whole folder, which is what
	// makes it usable as a watermark check on every rescan tick.
	FolderSequence(ctx context.Context, id folder.ID) (uint64, error)
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

// FolderAuthStore persists which devices are authorized to synchronize which
// folders. It is deny by default: a folder with no row here is served to no
// peer.
type FolderAuthStore interface {
	// GrantFolderDevice authorizes a device for a folder. Granting twice
	// is not an error. It returns ErrNotFound when the folder or the
	// device is not configured.
	GrantFolderDevice(ctx context.Context, id folder.ID, dev device.ID) error
	// RevokeFolderDevice withdraws the authorization. It returns
	// ErrNotFound when the device was not authorized for that folder.
	RevokeFolderDevice(ctx context.Context, id folder.ID, dev device.ID) error
	// FolderAccess returns the devices authorized for a folder. An
	// unknown folder yields an empty set rather than an error: a caller
	// asking on behalf of a peer must not be able to tell the two apart.
	FolderAccess(ctx context.Context, id folder.ID) (folder.Access, error)
	// DeviceFolders returns the folders a device is authorized for,
	// sorted by folder id.
	DeviceFolders(ctx context.Context, dev device.ID) ([]folder.ID, error)
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

// ChunkPlacement is where a chunk of content this device already holds can be
// read: which folder, which file, and the byte range inside it.
type ChunkPlacement struct {
	FolderID folder.ID
	Path     string
	Offset   int64
	Length   int
}

// ChunkStore is the index of what this device already holds, chunk by chunk.
//
// It is what makes a download skip content that is already on this disk,
// wherever it is: another version of the same file, another file of the same
// folder, or a file of a different folder entirely. The index is written by
// the scanner, because the scanner is what reads a file's bytes, and read by
// the puller before it asks a peer for anything.
type ChunkStore interface {
	// FileChunks returns how a file of this device was cut, in index
	// order, together with the hash of the content it was cut from, or
	// ErrNotFound when the index has nothing for that path. The caller
	// checks that hash against the entry it is serving: an index whose
	// geometry still fits says nothing about which bytes it describes.
	FileChunks(ctx context.Context, id folder.ID, path string) (transfer.ChunkIndex, error)
	// FindChunk returns somewhere this device holds a chunk with that
	// hash, or ErrNotFound. The caller re-reads and re-hashes the bytes
	// before using them: the index is only as fresh as the last scan.
	FindChunk(ctx context.Context, h manifest.Hash) (ChunkPlacement, error)
	// SetFileChunks replaces the chunk index of one path with a cut of the
	// content named by ix.Hash. It is called with the path's file row
	// already written, so the index never names content the folder does
	// not have. An index with no chunks clears the path.
	SetFileChunks(ctx context.Context, id folder.ID, path string, ix transfer.ChunkIndex) error
	// IndexedPaths returns the paths of a folder the chunk index covers.
	// A scan uses it to tell a file it has already chunked from one whose
	// content it knows but whose chunks it does not, which is every file
	// of a database written before the index existed.
	IndexedPaths(ctx context.Context, id folder.ID) (map[string]struct{}, error)
}

// MergeBase is a whole version of a text file this device once held, kept so
// that a later divergence on that path can be merged against it. It is what
// the three-way merge calls the base: the version two concurrent edits both
// descend from.
//
// Nothing writes one unless the text merge setting is on, so a device that
// never turned it on stores none of this.
type MergeBase struct {
	FolderID folder.ID
	Path     string
	// Version is the vector the path carried when this content was
	// recorded. It is what makes a candidate usable: a base has to be
	// dominated by both of the versions being merged.
	Version manifest.Version
	Hash    manifest.Hash
	Content []byte
	SavedAt time.Time
}

// MergeBaseHistory is how many versions of one path are kept as merge base
// candidates. More than one is needed because the local device writes a new
// version of its own as soon as the user edits the file, and the version the
// peer diverged from is the one before that.
const MergeBaseHistory = 4

// MergeBaseStore keeps the recent versions of a text file's content, as merge
// base candidates. Only the newest MergeBaseHistory of a path are kept.
type MergeBaseStore interface {
	// SaveMergeBase records one version's content, dropping the oldest
	// rows of that path beyond MergeBaseHistory.
	SaveMergeBase(ctx context.Context, b MergeBase) error
	// MergeBases returns the candidates of a path, newest first. A path
	// with none returns an empty slice rather than an error.
	MergeBases(ctx context.Context, id folder.ID, path string) ([]MergeBase, error)
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
	FolderAuthStore
	FileStore
	CounterStore
	DeviceStore
	TransferStore
	ChunkStore
	ConflictStore
	MergeBaseStore
	Close() error
}
