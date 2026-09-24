package tui

// The documents below mirror the JSON shapes docs/CLI.md documents, field for
// field. They are declared here rather than imported from internal/app because
// the TUI is a presentation layer over the command line: the only contract it
// is allowed to depend on is the published `--json` output, and a copy of the
// shape is what makes that dependency visible. A field renamed in the
// application would break a CLI consumer before it broke this file, and
// `internal/tui/contract_test.go` checks these shapes against the documented
// examples.
//
// Only the fields a screen shows are declared. Unknown fields are ignored by
// encoding/json, which is the same tolerance every other consumer of `--json`
// has.

// Status is `syngo status --json`.
type Status struct {
	SchemaVersion int           `json:"schema_version"`
	Device        StatusDevice  `json:"device"`
	Daemon        StatusDaemon  `json:"daemon"`
	Folders       []Folder      `json:"folders"`
	Peers         []Peer        `json:"peers"`
	Transfers     Transfers     `json:"transfers"`
	Conflicts     ConflictCount `json:"conflicts"`
}

// StatusDevice is `status.device`.
type StatusDevice struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	DataDir string `json:"data_dir"`
}

// StatusDaemon is `status.daemon`.
type StatusDaemon struct {
	Running       bool     `json:"running"`
	PID           int      `json:"pid"`
	Listen        string   `json:"listen"`
	StartedAt     *string  `json:"started_at"`
	UploadLimit   int64    `json:"upload_limit"`
	DownloadLimit int64    `json:"download_limit"`
	Listener      Listener `json:"listener"`
}

// Listener is `status.daemon.listener`.
type Listener struct {
	Bound     bool    `json:"bound"`
	Address   string  `json:"address"`
	Interface string  `json:"interface"`
	BoundAt   *string `json:"bound_at"`
	LostAt    *string `json:"lost_at"`
	Rebinds   int     `json:"rebinds"`
	Failures  int     `json:"failures"`
	Error     *string `json:"error"`
}

// Folder is one entry of `status.folders`.
type Folder struct {
	ID             string   `json:"id"`
	Path           string   `json:"path"`
	Label          string   `json:"label"`
	Mode           string   `json:"mode"`
	DeletePolicy   string   `json:"delete_policy"`
	Paused         bool     `json:"paused"`
	State          string   `json:"state"`
	Files          int64    `json:"files"`
	Dirs           int64    `json:"dirs"`
	Symlinks       int64    `json:"symlinks"`
	Bytes          int64    `json:"bytes"`
	LastScanAt     *string  `json:"last_scan_at"`
	PendingChanges int      `json:"pending_changes"`
	Error          *string  `json:"error"`
	SyncWindows    []string `json:"sync_windows"`
	InWindow       bool     `json:"in_window"`
	NextWindow     *string  `json:"next_window"`
}

// Peer is one entry of `status.peers`.
type Peer struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Address     string  `json:"address"`
	Verified    bool    `json:"verified"`
	Connected   bool    `json:"connected"`
	LastSeenAt  *string `json:"last_seen_at"`
	LatencyMS   float64 `json:"latency_ms"`
	Error       *string `json:"error"`
	State       string  `json:"state"`
	NextRetryAt *string `json:"next_retry_at"`
	Path        Path    `json:"path"`
}

// Path is `status.peers[].path`.
type Path struct {
	Kind       string  `json:"kind"`
	Endpoint   string  `json:"endpoint"`
	Relay      string  `json:"relay"`
	Active     bool    `json:"active"`
	Reason     *string `json:"reason"`
	ObservedAt *string `json:"observed_at"`
}

// Transfers is `status.transfers`.
type Transfers struct {
	Active  int        `json:"active"`
	Pending int        `json:"pending"`
	Paused  int        `json:"paused"`
	Failed  int        `json:"failed"`
	Items   []Transfer `json:"items"`
}

// Transfer is one entry of `status.transfers.items`, the same object
// `transfer list --json` prints.
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

// ConflictCount is `status.conflicts`.
type ConflictCount struct {
	Unresolved int `json:"unresolved"`
}

// NetPath is one entry of `syngo network path --json`.
type NetPath struct {
	PeerID   string  `json:"peer_id"`
	Name     string  `json:"name"`
	Address  string  `json:"address"`
	Kind     string  `json:"kind"`
	Endpoint string  `json:"endpoint"`
	Relay    string  `json:"relay"`
	Active   bool    `json:"active"`
	Reason   *string `json:"reason"`
}

// Conflict is one entry of `syngo conflict list --json`, and the object
// `conflict show --json` extends.
type Conflict struct {
	ID           string  `json:"id"`
	FolderID     string  `json:"folder_id"`
	Path         string  `json:"path"`
	ConflictPath string  `json:"conflict_path"`
	Reason       string  `json:"reason"`
	LocalHash    string  `json:"local_hash"`
	RemoteHash   string  `json:"remote_hash"`
	PeerID       string  `json:"peer_id"`
	DetectedAt   *string `json:"detected_at"`
	Resolved     bool    `json:"resolved"`
	Stale        bool    `json:"stale"`
	// File describes what is at Path now, and Copy what is at
	// ConflictPath. Both are only present in `conflict show --json`, and
	// either is absent when there is no file there any more.
	File *ConflictFile `json:"file"`
	Copy *ConflictFile `json:"copy"`
}

// ConflictFile is one of the two files a conflict names, as it is on disk now.
type ConflictFile struct {
	Path    string  `json:"path"`
	Size    int64   `json:"size"`
	MTime   *string `json:"mtime"`
	Hash    string  `json:"hash"`
	Version string  `json:"version"`
}

// InitResult is `syngo init --json`.
type InitResult struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	DataDir  string `json:"data_dir"`
	Listen   string `json:"listen"`
	Created  bool   `json:"created"`
}

// PeerAdded is `syngo peer add --json`.
type PeerAdded struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Address      string  `json:"address"`
	Verified     bool    `json:"verified"`
	AddedAt      string  `json:"added_at"`
	LastSeenAt   *string `json:"last_seen_at"`
	Accepted     *bool   `json:"accepted"`
	ResolvedFrom string  `json:"resolved_from"`
}

// PeerTested is `syngo peer test --json`.
type PeerTested struct {
	Address       string  `json:"address"`
	DeviceID      string  `json:"device_id"`
	Name          string  `json:"name"`
	TLSOK         bool    `json:"tls_ok"`
	Authenticated bool    `json:"authenticated"`
	RejectReason  string  `json:"reject_reason"`
	Reason        string  `json:"reason"`
	LatencyMS     float64 `json:"latency_ms"`
	Error         *string `json:"error"`
}

// FolderAdded is `syngo folder add --json`, and the object `folder list`
// prints an array of.
type FolderAdded struct {
	ID           string   `json:"id"`
	Path         string   `json:"path"`
	Label        string   `json:"label"`
	Mode         string   `json:"mode"`
	DeletePolicy string   `json:"delete_policy"`
	Symlinks     string   `json:"symlinks"`
	SyncWindows  []string `json:"sync_windows"`
	Paused       bool     `json:"paused"`
}

// Granted is `syngo folder grant --json`.
type Granted struct {
	Folder string `json:"folder"`
	Device string `json:"device"`
}

// ConflictResolved is `syngo conflict resolve --json`.
type ConflictResolved struct {
	ID          string `json:"id"`
	FolderID    string `json:"folder_id"`
	Path        string `json:"path"`
	Kept        string `json:"kept"`
	KeptPath    string `json:"kept_path"`
	MovedFrom   string `json:"moved_from"`
	RemovedPath string `json:"removed_path"`
	Resolved    bool   `json:"resolved"`
}

// FolderDevices is `syngo folder devices ID --json`.
type FolderDevices struct {
	Folder  string       `json:"folder"`
	Devices []DeviceName `json:"devices"`
}

// DeviceName is one entry of FolderDevices.Devices.
type DeviceName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
