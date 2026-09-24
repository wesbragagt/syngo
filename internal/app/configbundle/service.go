package configbundle

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/bandwidth"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/ignore"
	"github.com/wesbragagt/syngo/internal/domain/transfer"
)

// Service exports the configuration of a device and imports one into it. It is
// not transactional across the three stores: folders, peers and settings are
// written one after another, and a failure midway reports what was written.
type Service struct {
	folders        ports.FolderStore
	devices        ports.DeviceStore
	settings       ports.SettingsStore
	fs             ports.FileSystem
	clock          ports.Clock
	validateListen ports.ValidateListen
}

// New returns a bundle service over the stores of one data directory.
// validateListen is the same check `config set --listen` makes, so a bundle
// cannot write an address the command would have refused.
func New(folders ports.FolderStore, devices ports.DeviceStore, settings ports.SettingsStore,
	fs ports.FileSystem, clock ports.Clock, validateListen ports.ValidateListen) *Service {
	return &Service{folders: folders, devices: devices, settings: settings, fs: fs, clock: clock,
		validateListen: validateListen}
}

// Export reads the configuration as it is now.
func (s *Service) Export(ctx context.Context) (Bundle, error) {
	settings, err := s.settings.Load(ctx)
	if err != nil {
		return Bundle{}, err
	}
	configured, err := s.folders.ListFolders(ctx)
	if err != nil {
		return Bundle{}, err
	}
	known, err := s.devices.ListDevices(ctx)
	if err != nil {
		return Bundle{}, err
	}

	b := Bundle{
		SchemaVersion: SchemaVersion,
		ExportedAt:    s.clock.Now().UTC().Format(time.RFC3339),
		Settings:      settingsOf(settings),
		Folders:       make([]Folder, 0, len(configured)),
		Peers:         make([]Peer, 0, len(known)),
	}
	for _, f := range configured {
		patterns := f.IgnorePatterns
		if patterns == nil {
			patterns = []string{}
		}
		b.Folders = append(b.Folders, Folder{
			ID: string(f.ID), Path: f.Path, Label: f.Label, Mode: string(f.Mode),
			DeletePolicy: string(f.DeletePolicy), Symlinks: string(f.Symlinks),
			IncludeHidden: f.IncludeHidden, KeepEmptyDirs: f.KeepEmptyDirs,
			IgnorePatterns: patterns, SyncWindows: f.SyncWindows.Strings(), Paused: f.Paused,
		})
	}
	for _, d := range known {
		b.Peers = append(b.Peers, Peer{
			ID: string(d.ID), Name: d.Name, Address: d.Address, Verified: d.Verified,
		})
	}
	return b, nil
}

// ImportOptions tunes what an import is allowed to do.
type ImportOptions struct {
	// Replace overwrites the folders and peers whose id the bundle repeats,
	// keeping the rows of the folder and the history of the peer.
	Replace bool
	// DryRun validates and computes the result without writing.
	DryRun bool
}

// Conflict kinds.
const (
	ConflictFolderID   = "folder-id"
	ConflictFolderPath = "folder-path"
	ConflictPeer       = "peer"
)

// Conflict is one item of a bundle that cannot be written as it is.
type Conflict struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Path string `json:"path,omitempty"`
	// Existing names what already holds the id or the path.
	Existing string `json:"existing,omitempty"`
}

// String renders the conflict as the line the CLI prints.
func (c Conflict) String() string {
	switch c.Kind {
	case ConflictFolderPath:
		return fmt.Sprintf("folder %q: path %s is already synchronized as %q", c.ID, c.Path, c.Existing)
	case ConflictPeer:
		return fmt.Sprintf("peer %s: already configured", c.ID)
	default:
		return fmt.Sprintf("folder %q: id already configured", c.ID)
	}
}

// ConflictError reports every item of a bundle that collides with what is
// configured. Nothing is written when it is returned.
type ConflictError struct{ Conflicts []Conflict }

// Error lists every conflict, separated by "; ".
func (e *ConflictError) Error() string {
	lines := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		lines = append(lines, c.String())
	}
	return strings.Join(lines, "; ")
}

// Is makes a conflict match ports.ErrAlreadyExists, which is the exit code the
// CLI already maps.
func (e *ConflictError) Is(target error) bool { return target == ports.ErrAlreadyExists }

// Result says what an import did, or would do in a dry run.
type Result struct {
	DryRun          bool     `json:"dry_run"`
	FoldersAdded    []string `json:"folders_added"`
	FoldersReplaced []string `json:"folders_replaced"`
	PeersAdded      []string `json:"peers_added"`
	PeersReplaced   []string `json:"peers_replaced"`
	SettingsChanged bool     `json:"settings_changed"`
}

// Changed reports whether the import wrote, or would write, anything.
func (r Result) Changed() bool {
	return len(r.FoldersAdded) > 0 || len(r.FoldersReplaced) > 0 ||
		len(r.PeersAdded) > 0 || len(r.PeersReplaced) > 0 || r.SettingsChanged
}

// Import validates the whole bundle, reports every conflict, and only then
// writes: folders first, then peers, then the settings. A store that fails
// midway is reported together with the result up to that point.
func (s *Service) Import(ctx context.Context, b Bundle, o ImportOptions) (Result, error) {
	folders, peers, settings, err := s.parse(ctx, b)
	if err != nil {
		return Result{}, err
	}
	existingFolders, err := s.folders.ListFolders(ctx)
	if err != nil {
		return Result{}, err
	}
	existingPeers, err := s.devices.ListDevices(ctx)
	if err != nil {
		return Result{}, err
	}
	if conflicts := findConflicts(folders, peers, existingFolders, existingPeers, o.Replace); len(conflicts) > 0 {
		return Result{}, &ConflictError{Conflicts: conflicts}
	}

	current, err := s.settings.Load(ctx)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		DryRun:          o.DryRun,
		FoldersAdded:    []string{},
		FoldersReplaced: []string{},
		PeersAdded:      []string{},
		PeersReplaced:   []string{},
		SettingsChanged: settings != current,
	}

	byID := make(map[folder.ID]folder.Folder, len(existingFolders))
	for _, f := range existingFolders {
		byID[f.ID] = f
	}
	peersByID := make(map[device.ID]device.Device, len(existingPeers))
	for _, d := range existingPeers {
		peersByID[d.ID] = d
	}
	// A folder or a peer the bundle repeats unchanged is left alone: an
	// import that would write the same row again has nothing to report and
	// nothing to do.
	writeFolders := make([]folder.Folder, 0, len(folders))
	for _, f := range folders {
		old, ok := byID[f.ID]
		if !ok {
			f.CreatedAt = s.clock.Now()
			result.FoldersAdded = append(result.FoldersAdded, string(f.ID))
			writeFolders = append(writeFolders, f)
			continue
		}
		// Only the configuration changes: when the folder was first
		// configured and when it was last scanned belong to this
		// device, as do the rows of its files.
		f.CreatedAt, f.LastScanAt, f.LastError = old.CreatedAt, old.LastScanAt, old.LastError
		f.LastSyncError, f.PendingItems = old.LastSyncError, old.PendingItems
		if sameFolder(old, f) {
			continue
		}
		result.FoldersReplaced = append(result.FoldersReplaced, string(f.ID))
		writeFolders = append(writeFolders, f)
	}
	writePeers := make([]device.Device, 0, len(peers))
	for _, p := range peers {
		old, ok := peersByID[p.ID]
		if !ok {
			p.AddedAt = s.clock.Now()
			result.PeersAdded = append(result.PeersAdded, string(p.ID))
			writePeers = append(writePeers, p)
			continue
		}
		p.AddedAt, p.LastSeenAt = old.AddedAt, old.LastSeenAt
		if old == p {
			continue
		}
		result.PeersReplaced = append(result.PeersReplaced, string(p.ID))
		writePeers = append(writePeers, p)
	}
	folders, peers = writeFolders, writePeers
	if o.DryRun {
		return result, nil
	}

	written := Result{DryRun: false, FoldersAdded: []string{}, FoldersReplaced: []string{},
		PeersAdded: []string{}, PeersReplaced: []string{}}
	for _, f := range folders {
		if err := s.folders.SaveFolder(ctx, f); err != nil {
			return written, err
		}
		written.record(result, string(f.ID), true)
	}
	for _, p := range peers {
		if err := s.devices.SaveDevice(ctx, p); err != nil {
			return written, err
		}
		written.record(result, string(p.ID), false)
	}
	if result.SettingsChanged {
		if err := s.settings.Save(ctx, settings); err != nil {
			return written, err
		}
		written.SettingsChanged = true
	}
	return written, nil
}

// record moves one written id from the planned result into the written one, so
// a failure midway reports what actually landed.
func (r *Result) record(planned Result, id string, isFolder bool) {
	switch {
	case isFolder && slices.Contains(planned.FoldersReplaced, id):
		r.FoldersReplaced = append(r.FoldersReplaced, id)
	case isFolder:
		r.FoldersAdded = append(r.FoldersAdded, id)
	case slices.Contains(planned.PeersReplaced, id):
		r.PeersReplaced = append(r.PeersReplaced, id)
	default:
		r.PeersAdded = append(r.PeersAdded, id)
	}
}

// parse turns a bundle into domain values, refusing anything the rest of the
// program could not act on. It runs over the whole document, so a bundle is
// never half applied because its last folder was wrong.
func (s *Service) parse(_ context.Context, b Bundle) ([]folder.Folder, []device.Device, ports.Settings, error) {
	settings, err := parseSettings(b.Settings, s.validateListen)
	if err != nil {
		return nil, nil, ports.Settings{}, err
	}

	folders := make([]folder.Folder, 0, len(b.Folders))
	seenFolders := make(map[folder.ID]struct{}, len(b.Folders))
	// Two ids cannot synchronize the same directory, and the path is only
	// known to be clean once the folder validated, so the bundle is checked
	// against itself here rather than left to the store, which would refuse
	// the second folder after the first was written.
	seenPaths := make(map[string]folder.ID, len(b.Folders))
	for _, item := range b.Folders {
		f, err := s.parseFolder(item)
		if err != nil {
			return nil, nil, ports.Settings{}, err
		}
		if _, ok := seenFolders[f.ID]; ok {
			return nil, nil, ports.Settings{},
				fmt.Errorf("%w: folder %q is listed twice", ErrInvalidBundle, f.ID)
		}
		if first, ok := seenPaths[f.Path]; ok {
			return nil, nil, ports.Settings{},
				fmt.Errorf("%w: path %s is listed by folder %q and folder %q",
					ErrInvalidBundle, f.Path, first, f.ID)
		}
		seenFolders[f.ID] = struct{}{}
		seenPaths[f.Path] = f.ID
		folders = append(folders, f)
	}

	peers := make([]device.Device, 0, len(b.Peers))
	seenPeers := make(map[device.ID]struct{}, len(b.Peers))
	for _, item := range b.Peers {
		id, err := device.ParseID(item.ID)
		if err != nil {
			return nil, nil, ports.Settings{}, fmt.Errorf("%w: peer %q: %v", ErrInvalidBundle, item.ID, err)
		}
		if strings.TrimSpace(item.Address) == "" {
			return nil, nil, ports.Settings{},
				fmt.Errorf("%w: peer %s has no address", ErrInvalidBundle, id.Short())
		}
		if _, ok := seenPeers[id]; ok {
			return nil, nil, ports.Settings{},
				fmt.Errorf("%w: peer %s is listed twice", ErrInvalidBundle, id.Short())
		}
		seenPeers[id] = struct{}{}
		peers = append(peers, device.Device{
			ID: id, Name: item.Name, Address: item.Address, Verified: item.Verified,
		})
	}
	return folders, peers, settings, nil
}

// parseFolder reads one folder of a bundle, including the checks `folder add`
// makes: the path must be a directory that exists here, and the patterns and
// windows must parse.
func (s *Service) parseFolder(item Folder) (folder.Folder, error) {
	windows, err := folder.ParseWindows(item.SyncWindows)
	if err != nil {
		return folder.Folder{}, fmt.Errorf("%w: folder %q: %v", ErrInvalidBundle, item.ID, err)
	}
	if _, err := ignore.Parse(item.IgnorePatterns); err != nil {
		return folder.Folder{}, fmt.Errorf("%w: folder %q: %v", ErrInvalidBundle, item.ID, err)
	}
	f := folder.Folder{
		ID: folder.ID(item.ID), Path: item.Path, Label: item.Label,
		Mode: folder.Mode(item.Mode), DeletePolicy: folder.DeletePolicy(item.DeletePolicy),
		Symlinks: folder.SymlinkPolicy(item.Symlinks), IncludeHidden: item.IncludeHidden,
		KeepEmptyDirs: item.KeepEmptyDirs, IgnorePatterns: item.IgnorePatterns,
		SyncWindows: windows, Paused: item.Paused,
	}
	if err := f.Validate(); err != nil {
		return folder.Folder{}, fmt.Errorf("%w: folder %q: %v", ErrInvalidBundle, item.ID, err)
	}
	info, err := s.fs.Lstat(f.Path)
	if err != nil {
		return folder.Folder{}, fmt.Errorf("%w: folder %q: path %s: %v", ErrInvalidBundle, item.ID, f.Path, err)
	}
	if !info.IsDir() {
		return folder.Folder{}, fmt.Errorf("%w: folder %q: path %s is not a directory",
			ErrInvalidBundle, item.ID, f.Path)
	}
	return f, nil
}

// sameFolder reports whether two folder configurations are the same. An empty
// pattern list and a nil one describe the same folder, so both are normalized
// before the comparison.
func sameFolder(a, b folder.Folder) bool {
	a.IgnorePatterns, b.IgnorePatterns = nonEmptyList(a.IgnorePatterns), nonEmptyList(b.IgnorePatterns)
	if len(a.SyncWindows) == 0 {
		a.SyncWindows = nil
	}
	if len(b.SyncWindows) == 0 {
		b.SyncWindows = nil
	}
	return reflect.DeepEqual(a, b)
}

// nonEmptyList turns an empty list into a nil one.
func nonEmptyList(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	return list
}

// findConflicts lists every item of the bundle that collides with what is
// configured. A path owned by a folder with another id is a conflict even with
// --replace: two ids cannot synchronize the same directory.
func findConflicts(folders []folder.Folder, peers []device.Device,
	existingFolders []folder.Folder, existingPeers []device.Device, replace bool) []Conflict {
	var out []Conflict
	for _, f := range folders {
		for _, other := range existingFolders {
			switch {
			case other.ID == f.ID && !replace:
				out = append(out, Conflict{Kind: ConflictFolderID, ID: string(f.ID)})
			case other.ID != f.ID && other.Path == f.Path:
				out = append(out, Conflict{Kind: ConflictFolderPath, ID: string(f.ID),
					Path: f.Path, Existing: string(other.ID)})
			}
		}
	}
	if !replace {
		for _, p := range peers {
			for _, other := range existingPeers {
				if other.ID == p.ID {
					out = append(out, Conflict{Kind: ConflictPeer, ID: string(p.ID)})
				}
			}
		}
	}
	return out
}

// settingsOf is the settings as the bundle carries them.
func settingsOf(s ports.Settings) Settings {
	return Settings{
		Listen: s.Listen, Interface: s.Interface, ChunkSize: s.ChunkSize,
		MaxFileSize: s.MaxFileSize, RescanInterval: s.RescanInterval.String(),
		SyncInterval: s.SyncInterval.String(), LogLevel: s.LogLevel,
		UploadLimit: int64(s.UploadLimit), DownloadLimit: int64(s.DownloadLimit),
		TextMerge: s.TextMerge,
	}
}

// parseSettings reads the settings of a bundle, refusing values the daemon
// could not run with.
func parseSettings(s Settings, validateListen ports.ValidateListen) (ports.Settings, error) {
	rescan, err := time.ParseDuration(s.RescanInterval)
	if err != nil {
		return ports.Settings{}, fmt.Errorf("%w: rescan_interval %q: %v", ErrInvalidBundle, s.RescanInterval, err)
	}
	sync, err := time.ParseDuration(s.SyncInterval)
	if err != nil {
		return ports.Settings{}, fmt.Errorf("%w: sync_interval %q: %v", ErrInvalidBundle, s.SyncInterval, err)
	}
	switch {
	case rescan <= 0:
		return ports.Settings{}, fmt.Errorf("%w: rescan_interval %s must be positive", ErrInvalidBundle, rescan)
	case sync <= 0:
		return ports.Settings{}, fmt.Errorf("%w: sync_interval %s must be positive", ErrInvalidBundle, sync)
	case s.ChunkSize < transfer.MinChunkSize:
		return ports.Settings{}, fmt.Errorf("%w: chunk_size %d is below the minimum %d",
			ErrInvalidBundle, s.ChunkSize, transfer.MinChunkSize)
	case s.MaxFileSize <= 0:
		return ports.Settings{}, fmt.Errorf("%w: max_file_size %d must be positive",
			ErrInvalidBundle, s.MaxFileSize)
	case s.UploadLimit < 0 || s.DownloadLimit < 0:
		return ports.Settings{}, fmt.Errorf("%w: a bandwidth limit is negative", ErrInvalidBundle)
	case !ports.ValidLogLevel(s.LogLevel):
		return ports.Settings{}, fmt.Errorf("%w: log_level %q is not one of %v",
			ErrInvalidBundle, s.LogLevel, ports.LogLevels)
	}
	if err := validateListen(s.Listen); err != nil {
		return ports.Settings{}, fmt.Errorf("%w: listen %q: %v", ErrInvalidBundle, s.Listen, err)
	}
	return ports.Settings{
		Listen: s.Listen, Interface: s.Interface, ChunkSize: s.ChunkSize,
		MaxFileSize: s.MaxFileSize, RescanInterval: rescan, SyncInterval: sync,
		LogLevel: s.LogLevel, UploadLimit: bandwidth.Rate(s.UploadLimit),
		DownloadLimit: bandwidth.Rate(s.DownloadLimit), TextMerge: s.TextMerge,
	}, nil
}
