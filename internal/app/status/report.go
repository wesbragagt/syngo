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

// Peer connection states reported per peer.
const (
	// PeerConnected is a peer this device holds a connection to.
	PeerConnected = "connected"
	// PeerConnecting is a peer the daemon is dialling or waiting to dial
	// again.
	PeerConnecting = "connecting"
	// PeerUnreachable is a peer whose last attempt failed and which is
	// being retried with a bounded backoff.
	PeerUnreachable = "unreachable"
	// PeerUnknown is what is reported without a running daemon: nothing is
	// dialling, so nothing knows.
	PeerUnknown = "unknown"
)

// Path kinds reported per peer, mirroring ports.TailnetPathKind.
const (
	PathDirect  = "direct"
	PathRelay   = "relay"
	PathUnknown = "unknown"
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
	Running       bool     `json:"running"`
	PID           int      `json:"pid"`
	Listen        string   `json:"listen"`
	StartedAt     *string  `json:"started_at"`
	UploadLimit   int64    `json:"upload_limit"`
	DownloadLimit int64    `json:"download_limit"`
	Listener      Listener `json:"listener"`
}

// Listener describes the port peers connect to, and what it has been through:
// a Tailscale client restarted under the daemon, an interface renumbered, a
// link that dropped. Times are RFC3339 in UTC, or null.
type Listener struct {
	// Bound reports whether the port is accepting connections now.
	Bound bool `json:"bound"`
	// Address is the address bound now, or the last one that was bound.
	Address string `json:"address"`
	// Interface is the interface name the address is taken from, empty
	// when the address was configured directly.
	Interface string `json:"interface"`
	// BoundAt is when the current socket was bound.
	BoundAt *string `json:"bound_at"`
	// LostAt is when the address was last lost, and stays set after the
	// listener came back.
	LostAt *string `json:"lost_at"`
	// Rebinds counts how many times the listener came back.
	Rebinds int `json:"rebinds"`
	// Failures counts the consecutive bind attempts that failed.
	Failures int `json:"failures"`
	// Error is why the last bind attempt failed, or null.
	Error *string `json:"error"`
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
	// State is connected, connecting, unreachable, or unknown without a
	// running daemon.
	State string `json:"state"`
	// NextRetryAt is when the daemon dials this peer again, or null while
	// it is connected or no daemon is running.
	NextRetryAt *string `json:"next_retry_at"`
	// Path is how traffic reaches the peer over the tailnet.
	Path Path `json:"path"`
}

// Path is what the local Tailscale client says about how traffic reaches a
// peer. Nothing is guessed: a client that cannot say gives kind "unknown" with
// the reason.
type Path struct {
	// Kind is direct, relay or unknown.
	Kind string `json:"kind"`
	// Endpoint is the direct address in use, empty unless kind is direct.
	Endpoint string `json:"endpoint"`
	// Relay is the DERP region in use, empty unless kind is relay.
	Relay string `json:"relay"`
	// Active reports whether the Tailscale client saw traffic to the peer
	// recently.
	Active bool `json:"active"`
	// Reason says why the kind is unknown, and is null otherwise.
	Reason *string `json:"reason"`
	// ObservedAt is when the path was last read from the local Tailscale
	// client, or null when it never was.
	ObservedAt *string `json:"observed_at"`
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
	// State is one of the peer states above. An empty state is read as
	// connected or unreachable, from Connected.
	State string
	// NextRetryAt is when the peer is dialled again, zero while connected.
	NextRetryAt time.Time
	// Path is what the daemon last read from the local Tailscale client
	// about this peer. A zero Path is reported as unknown.
	Path PathLive
}

// PathLive is one observation of how traffic reaches a peer.
type PathLive struct {
	Kind       string
	Endpoint   string
	Relay      string
	Active     bool
	Reason     string
	ObservedAt time.Time
}

// LiveInfo is the state only a running daemon can report. It is nil when the
// command runs without a daemon.
type LiveInfo struct {
	PID           int
	Listen        string
	Listener      Listener
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
