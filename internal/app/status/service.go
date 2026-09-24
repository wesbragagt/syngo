package status

import (
	"context"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/transfer"
)

// Service builds status reports from the database and, when a daemon is
// running, from its live state.
type Service struct {
	store    ports.MetadataStore
	identity ports.Identity
	clock    ports.Clock
	dataDir  string
}

// New returns a status service reading from store.
func New(store ports.MetadataStore, identity ports.Identity, clock ports.Clock, dataDir string) *Service {
	return &Service{store: store, identity: identity, clock: clock, dataDir: dataDir}
}

// Report assembles the status document. live is nil when no daemon is running.
func (s *Service) Report(ctx context.Context, live *LiveInfo) (Report, error) {
	report := Report{
		SchemaVersion: SchemaVersion,
		Device: Device{
			ID:      string(s.identity.DeviceID()),
			Name:    s.identity.Name(),
			DataDir: s.dataDir,
		},
		Daemon:  daemonSection(live),
		Folders: []Folder{},
		Peers:   []Peer{},
	}

	folders, err := s.store.ListFolders(ctx)
	if err != nil {
		return Report{}, err
	}
	now := s.clock.Now()
	for _, f := range folders {
		stats, err := s.store.FolderStats(ctx, f.ID)
		if err != nil {
			return Report{}, err
		}
		inWindow := f.InWindow(now)
		var nextWindow *string
		if next, ok := f.SyncWindows.Next(now); ok {
			nextWindow = rfc3339(next)
		}
		report.Folders = append(report.Folders, Folder{
			ID:             string(f.ID),
			Path:           f.Path,
			Label:          f.Label,
			Mode:           string(f.Mode),
			DeletePolicy:   string(f.DeletePolicy),
			Paused:         f.Paused,
			State:          folderState(f, inWindow, live),
			Files:          stats.Files,
			Dirs:           stats.Dirs,
			Symlinks:       stats.Symlinks,
			Bytes:          stats.Bytes,
			LastScanAt:     rfc3339(f.LastScanAt),
			PendingChanges: f.PendingItems,
			Error:          nonEmpty(folderError(f)),
			SyncWindows:    f.SyncWindows.Strings(),
			InWindow:       inWindow,
			NextWindow:     nextWindow,
		})
	}

	devices, err := s.store.ListDevices(ctx)
	if err != nil {
		return Report{}, err
	}
	for _, d := range devices {
		peer := Peer{
			ID:         string(d.ID),
			Name:       d.Name,
			Address:    d.Address,
			Verified:   d.Verified,
			LastSeenAt: rfc3339(d.LastSeenAt),
		}
		peer.State = PeerUnknown
		peer.Path = Path{Kind: PathUnknown, Reason: nonEmpty(
			"no daemon is running, so no path to this peer has been read from the local Tailscale client")}
		if live != nil {
			if state, ok := live.Peers[string(d.ID)]; ok {
				peer.Connected = state.Connected
				peer.LatencyMS = float64(state.Latency.Microseconds()) / 1000
				peer.Error = nonEmpty(state.Error)
				peer.State = peerState(state)
				peer.NextRetryAt = rfc3339(state.NextRetryAt)
				peer.Path = pathOf(state.Path)
			} else {
				peer.State = PeerConnecting
				peer.Path = Path{Kind: PathUnknown, Reason: nonEmpty(
					"the daemon has not dialled this peer yet, so nothing knows its path")}
			}
		}
		report.Peers = append(report.Peers, peer)
	}

	transfers, err := s.store.ListTransfers(ctx, ports.TransferFilter{})
	if err != nil {
		return Report{}, err
	}
	report.Transfers = transfersSection(transfers)

	conflicts, err := s.store.ListConflicts(ctx, "", false)
	if err != nil {
		return Report{}, err
	}
	report.Conflicts = Conflicts{Unresolved: len(conflicts)}

	return report, nil
}

// peerState returns the state to report for a peer a daemon knows about.
func peerState(live PeerLive) string {
	if live.State != "" {
		return live.State
	}
	if live.Connected {
		return PeerConnected
	}
	return PeerUnreachable
}

// pathOf renders one path observation. A path nothing was ever read for is
// unknown with the reason, never a guess.
func pathOf(live PathLive) Path {
	if live.Kind == "" {
		return Path{Kind: PathUnknown, Reason: nonEmpty(
			"the local Tailscale client has not been asked about this peer yet")}
	}
	return Path{
		Kind: live.Kind, Endpoint: live.Endpoint, Relay: live.Relay,
		Active: live.Active, Reason: nonEmpty(live.Reason),
		ObservedAt: rfc3339(live.ObservedAt),
	}
}

// daemonSection reports what is known about the daemon.
func daemonSection(live *LiveInfo) Daemon {
	if live == nil {
		return Daemon{}
	}
	return Daemon{
		Running:       true,
		PID:           live.PID,
		Listen:        live.Listen,
		Listener:      live.Listener,
		StartedAt:     rfc3339(live.StartedAt),
		UploadLimit:   int64(live.UploadLimit),
		DownloadLimit: int64(live.DownloadLimit),
	}
}

// folderState derives the reported state of a folder. A running daemon may
// override it while scanning or syncing, which is why a folder waiting for its
// window is only reported as scheduled once nothing else is happening to it.
func folderState(f folder.Folder, inWindow bool, live *LiveInfo) string {
	if live != nil {
		if state, ok := live.FolderStates[string(f.ID)]; ok && state != "" {
			return state
		}
	}
	switch {
	case f.Paused:
		return StatePaused
	case folderError(f) != "":
		return StateError
	case !inWindow:
		return StateScheduled
	default:
		return StateIdle
	}
}

// folderError is what is wrong with a folder: what the last scan could not
// read, or what the last sync cycle could not apply. The scan failure comes
// first, because a folder that cannot be read is why a sync applies nothing.
func folderError(f folder.Folder) string {
	if f.LastError != "" {
		return f.LastError
	}
	return f.LastSyncError
}

// transfersSection counts transfers by state and lists them.
func transfersSection(list []transfer.Transfer) Transfers {
	out := Transfers{Items: []Transfer{}}
	for _, t := range list {
		switch t.State {
		case transfer.StateActive, transfer.StateVerifying:
			out.Active++
		case transfer.StatePending:
			out.Pending++
		case transfer.StatePaused:
			out.Paused++
		case transfer.StateFailed:
			out.Failed++
		}
		done := len(t.Chunks) - len(t.MissingChunks())
		out.Items = append(out.Items, Transfer{
			ID:          string(t.ID),
			FolderID:    string(t.FolderID),
			Path:        t.Path,
			PeerID:      string(t.PeerID),
			Direction:   "in",
			State:       string(t.State),
			Size:        t.Size,
			ChunkSize:   t.ChunkSize,
			ChunksTotal: len(t.Chunks),
			ChunksDone:  done,
			BytesDone:   t.BytesDone(),
			CreatedAt:   rfc3339(t.CreatedAt),
			UpdatedAt:   rfc3339(t.UpdatedAt),
			Error:       nonEmpty(t.Error),
		})
	}
	return out
}
