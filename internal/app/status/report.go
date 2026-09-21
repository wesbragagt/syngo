// Package status assembles the report behind the status command.
package status

import (
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/bandwidth"
)

// SchemaVersion is the version of the status JSON document. Fields may be
// added, never renamed.
const SchemaVersion = 1

// Folder states reported per folder.
const (
	StateIdle     = "idle"
	StateScanning = "scanning"
	StateSyncing  = "syncing"
	StatePaused   = "paused"
	StateError    = "error"
	// StateScheduled is a folder that is outside every one of its sync
	// windows: it still scans and still serves, but this device does not
	// pull for it until the next window opens.
	StateScheduled = "scheduled"
)

// Report is the full status document, as emitted by `status --json`.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Device        Device    `json:"device"`
	Daemon        Daemon    `json:"daemon"`
	Folders       []Folder  `json:"folders"`
	Peers         []Peer    `json:"peers"`
	Transfers     Transfers `json:"transfers"`
	Conflicts     Conflicts `json:"conflicts"`
}

// Device identifies the local device.
type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	DataDir string `json:"data_dir"`
}

// Daemon describes the running daemon, if any. The limits are bytes per
// second, 0 for unlimited or for no daemon.
type Daemon struct {
	Running       bool    `json:"running"`
	PID           int     `json:"pid"`
	Listen        string  `json:"listen"`
	StartedAt     *string `json:"started_at"`
	UploadLimit   int64   `json:"upload_limit"`
	DownloadLimit int64   `json:"download_limit"`
}

// Folder is the per-folder section of the report.
type Folder struct {
	ID             string  `json:"id"`
	Path           string  `json:"path"`
	Label          string  `json:"label"`
	Mode           string  `json:"mode"`
	DeletePolicy   string  `json:"delete_policy"`
	Paused         bool    `json:"paused"`
	State          string  `json:"state"`
	Files          int64   `json:"files"`
	Dirs           int64   `json:"dirs"`
	Symlinks       int64   `json:"symlinks"`
	Bytes          int64   `json:"bytes"`
	LastScanAt     *string `json:"last_scan_at"`
	PendingChanges int     `json:"pending_changes"`
	Error          *string `json:"error"`
	// SyncWindows are the folder's windows in canonical form; an empty list
	// means it is always open. InWindow says whether it is open now, and
	// NextWindow is when it opens next, or nil when it is open already.
	SyncWindows []string `json:"sync_windows"`
	InWindow    bool     `json:"in_window"`
	NextWindow  *string  `json:"next_window"`
}

// Peer is the per-peer section of the report.
type Peer struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Address    string  `json:"address"`
	Verified   bool    `json:"verified"`
	Connected  bool    `json:"connected"`
	LastSeenAt *string `json:"last_seen_at"`
	LatencyMS  float64 `json:"latency_ms"`
	Error      *string `json:"error"`
}

// Transfers summarizes inbound transfers.
type Transfers struct {
	Active  int        `json:"active"`
	Pending int        `json:"pending"`
	Paused  int        `json:"paused"`
	Failed  int        `json:"failed"`
	Items   []Transfer `json:"items"`
}

// Transfer is one inbound transfer.
type Transfer struct {
	ID          string  `json:"id"`
	FolderID    string  `json:"folder_id"`
	Path        string  `json:"path"`
	PeerID      string  `json:"peer_id"`
	Direction   string  `json:"direction"`
	State       string  `json:"state"`
	Size        int64   `json:"size"`
	ChunkSize   int     `json:"chunk_size"`
	ChunksTotal int     `json:"chunks_total"`
	ChunksDone  int     `json:"chunks_done"`
	BytesDone   int64   `json:"bytes_done"`
	CreatedAt   *string `json:"created_at"`
	UpdatedAt   *string `json:"updated_at"`
	Error       *string `json:"error"`
}

// Conflicts counts unresolved conflicts.
type Conflicts struct {
	Unresolved int `json:"unresolved"`
}

// PeerLive is what a running daemon knows about a peer beyond the database.
type PeerLive struct {
	Connected bool
	Latency   time.Duration
	Error     string
}

// LiveInfo is the state only a running daemon can report. It is nil when the
// command runs without a daemon.
type LiveInfo struct {
	PID           int
	Listen        string
	StartedAt     time.Time
	Peers         map[string]PeerLive
	FolderStates  map[string]string
	UploadLimit   bandwidth.Rate
	DownloadLimit bandwidth.Rate
}

// rfc3339 formats a time as an RFC3339 UTC string, or nil if it is zero.
func rfc3339(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// nonEmpty returns a pointer to s, or nil when s is empty.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
