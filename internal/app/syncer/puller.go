package syncer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sync"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/syncplan"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// Pull tuning.
const (
	// DefaultConcurrency is how many files are pulled at the same time.
	DefaultConcurrency = 4
	// chunkAttempts is how often a chunk that fails verification is asked
	// for again before the transfer is given up on.
	chunkAttempts = 3
	// syncEvery is how many chunks are written before the partial file is
	// flushed to disk.
	syncEvery = 16
	// tempDirMode is the permission of <root>/.syncthis/tmp.
	tempDirMode fs.FileMode = 0o700
)

// ErrOutsideWindow reports that a folder's sync windows do not cover the
// current local time, so this device does not pull for it. It is the error a
// transfer that stopped for it carries, so the message a user reads is exactly
// this one.
var ErrOutsideWindow = errors.New("outside sync window")

// Summary counts what one folder sync cycle did.
type Summary struct {
	Pulled    int
	Deleted   int
	Dirs      int
	Symlinks  int
	Modes     int
	Skipped   int
	Conflicts int
	Failed    int
	Bytes     int64
	// Failure is the first thing the cycle could not apply locally, nil
	// when Failed is zero. It is what the folder reports as its error.
	Failure error
}

// Puller fetches from a peer what this device is missing, one folder at a
// time. It owns the transfer journal: every chunk it verifies is recorded, so
// an interrupted download resumes instead of starting over.
type Puller struct {
	store     PullStore
	fs        ports.FileSystem
	hasher    ports.Hasher
	clock     ports.Clock
	identity  ports.Identity
	events    ports.EventSink
	installer *Installer
	chunkSize int
	workers   int
	// download paces what this device asks peers for. A nil limiter means
	// unlimited.
	download ports.Limiter
	log      *slog.Logger

	// resumed remembers the transfers whose partial file this process has
	// already re-verified, so the check happens once per transfer and not
	// once per sync cycle.
	mu      sync.Mutex
	resumed map[transfer.ID]bool
	// gates serializes the sync cycles of one folder. Two peers that share
	// a folder each run their own cycle, and only one transfer per path
	// may be live: without this the second cycle gives up the first one's
	// transfer as "the peer now offers another version" and the two undo
	// each other's work.
	gates map[folder.ID]chan struct{}
}

// PullStore is the persistence a puller writes to: everything a server needs,
// plus the conflicts it records when both sides changed a path.
type PullStore interface {
	Store
	ports.ConflictStore
}

// PullerConfig tunes a puller. Zero fields take the defaults.
type PullerConfig struct {
	ChunkSize   int
	Concurrency int
	// Download paces the chunks this device asks for, across every folder
	// and every peer. A nil limiter means unlimited.
	Download ports.Limiter
}

// NewPuller returns a puller writing through fsys.
func NewPuller(store PullStore, fsys ports.FileSystem, hasher ports.Hasher, clock ports.Clock,
	identity ports.Identity, events ports.EventSink, cfg PullerConfig, log *slog.Logger) *Puller {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.ChunkSize < transfer.MinChunkSize {
		cfg.ChunkSize = transfer.DefaultChunkSize
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	return &Puller{
		store: store, fs: fsys, hasher: hasher, clock: clock, identity: identity, events: events,
		installer: NewInstaller(fsys, hasher),
		chunkSize: cfg.ChunkSize, workers: cfg.Concurrency, download: cfg.Download, log: log,
		resumed: make(map[transfer.ID]bool),
		gates:   make(map[folder.ID]chan struct{}),
	}
}

// SyncFolder brings one folder in line with what the peer has: it asks for the
// peer's manifest, plans against the local one and applies the plan. Only the
// local side is changed; what the peer lacks is the peer's business.
func (p *Puller) SyncFolder(ctx context.Context, conn ports.PeerConn, f folder.Folder) (Summary, error) {
	var summary Summary
	if !f.CanReceive() || f.Paused {
		return summary, nil
	}
	// The peer loop leaves a closed folder out of the cycle; this is the
	// second guard, for a caller that asked for one folder directly.
	if !f.InWindow(p.clock.Now()) {
		p.log.Debug("folder is outside its sync window, not pulling",
			"folder", f.ID, "windows", folder.EncodeWindows(f.SyncWindows))
		return summary, nil
	}
	release, err := p.lockFolder(ctx, f.ID)
	if err != nil {
		return summary, err
	}
	defer release()

	// A folder whose root is gone must not be recreated by a sync: doing so
	// would install files into a directory the user removed.
	if !p.rootUsable(f) {
		p.log.Warn("folder root is missing, skipping sync", "folder", f.ID, "path", f.Path)
		return summary, nil
	}

	remote, err := conn.RequestManifest(ctx, f.ID)
	if err != nil {
		return summary, fmt.Errorf("manifest of %q from %s: %w", f.ID, conn.RemoteID().Short(), err)
	}
	local, err := p.store.LoadManifest(ctx, f.ID)
	if err != nil {
		return summary, err
	}
	matcher, err := loadIgnore(ctx, p.fs, f)
	if err != nil {
		return summary, err
	}

	peer := conn.RemoteID()
	items, ignored := dropIgnored(matcher, syncplan.Plan(local, remote, f))
	summary.Skipped += len(ignored)
	for _, path := range ignored {
		p.log.Debug("plan item skipped", "folder", f.ID, "path", path, "reason", ReasonIgnored)
	}

	var pulls []syncplan.Item
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if item.Action == syncplan.ActionPull || isFileReplace(item) || needsRemoteContent(item) {
			// A file that replaces a directory is downloaded like any
			// other file, and the directory only goes once the plan
			// emptied it, which is after this loop. A conflict whose
			// keep-both outcome needs the peer's bytes is downloaded
			// there too.
			pulls = append(pulls, item)
			continue
		}
		if err := p.apply(ctx, f, peer, item, &summary); err != nil {
			// One path that cannot be applied must not cost the folder
			// the rest of the cycle: the failure is reported and the
			// plan goes on.
			p.itemFailed(f, peer, item, err, &summary)
		}
	}
	if err := p.pullAll(ctx, conn, f, pulls, &summary); err != nil {
		p.recordOutcome(ctx, f, summary, false)
		return summary, err
	}
	if err := p.dropUnwanted(ctx, f, peer, pulls); err != nil {
		return summary, err
	}
	p.recordOutcome(ctx, f, summary, true)

	p.log.Debug("folder sync finished", "folder", f.ID, "peer", peer.Short(),
		"pulled", summary.Pulled, "deleted", summary.Deleted, "modes", summary.Modes,
		"conflicts", summary.Conflicts, "failed", summary.Failed, "bytes", summary.Bytes)
	return summary, nil
}

// lockFolder waits until no other cycle is syncing the folder and returns the
// function that lets the next one in.
func (p *Puller) lockFolder(ctx context.Context, id folder.ID) (func(), error) {
	p.mu.Lock()
	gate, ok := p.gates[id]
	if !ok {
		gate = make(chan struct{}, 1)
		p.gates[id] = gate
	}
	p.mu.Unlock()

	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// pausedNow re-reads the paused flag of a folder, so a pause that arrives
// while a transfer runs is noticed without waiting for the next cycle. A
// folder that cannot be read counts as paused: stopping leaves the transfer
// resumable, while going on would write into a folder that may be gone.
func (p *Puller) pausedNow(ctx context.Context, id folder.ID) bool {
	f, err := p.store.GetFolder(ctx, id)
	if err != nil {
		return ctx.Err() == nil
	}
	return f.Paused
}

// outsideWindowNow re-reads the sync windows of a folder, so a window that
// closes while a transfer runs is noticed without waiting for the next cycle.
// A folder that cannot be read counts as open: pausedNow already stops the
// transfer in that case, and calling it closed as well would file the stop
// under the wrong reason.
func (p *Puller) outsideWindowNow(ctx context.Context, id folder.ID) bool {
	f, err := p.store.GetFolder(ctx, id)
	if err != nil {
		return false
	}
	return !f.InWindow(p.clock.Now())
}

// rootUsable reports whether the folder root is still a directory.
func (p *Puller) rootUsable(f folder.Folder) bool {
	info, err := p.fs.Lstat(f.Path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// ReasonSendOnly explains a transfer that is waiting for a folder which never
// pulls, so it is not labelled paused when the folder is not.
const ReasonSendOnly = "folder is send-only"

// ResumeAll prepares the live transfers of a folder for a new process or for a
// resume, and empties the journal of the rows a stopped process left finished: a transfer nobody is fetching any more is pending, not active, and a
// transfer of a folder that is no longer paused stops claiming it is. f must
// be the folder as it is configured now. A folder that cannot receive keeps
// the transfer pending and says why, because "paused" means the folder is
// paused and nothing else.
func (p *Puller) ResumeAll(ctx context.Context, f folder.Folder) error {
	list, err := p.store.ListTransfers(ctx, ports.TransferFilter{FolderID: f.ID})
	if err != nil {
		return err
	}
	for _, t := range list {
		if t.State == transfer.StateDone {
			// The files table is the record of what arrived, so a
			// finished transfer's row is worth nothing. One that is
			// still here was written by a process that stopped
			// between the install and the deletion of the row.
			if err := p.dropTransfer(ctx, t, "the download had finished"); err != nil {
				return err
			}
			continue
		}
		reason := ""
		switch {
		case t.State == transfer.StateActive || t.State == transfer.StateVerifying:
		case t.State == transfer.StatePaused && !f.Paused:
			if !f.CanReceive() {
				reason = ReasonSendOnly
			}
		default:
			continue
		}
		if err := p.store.SetTransferState(ctx, t.ID, transfer.StatePending, reason, p.clock.Now()); err != nil {
			return err
		}
		p.log.Debug("transfer picked up again", "folder", f.ID, "path", t.Path,
			"chunks_done", len(t.Chunks)-len(t.MissingChunks()), "reason", reason)
	}
	return nil
}

// dropUnwanted empties the journal of the rows this cycle proved worthless.
// A row survives a cycle only while the plan still wants its path: a daemon
// stopped between an install and the deletion of its row leaves one behind
// that nothing would ever pick up, because the path has arrived and no later
// plan asks for it again, and `status` would report that folder as busy
// forever. Rows of other peers are left alone, as are failed rows, which are
// the integrity failures a user is meant to look at.
func (p *Puller) dropUnwanted(ctx context.Context, f folder.Folder, peer device.ID, pulls []syncplan.Item) error {
	rows, err := p.store.ListTransfers(ctx, ports.TransferFilter{FolderID: f.ID})
	if err != nil {
		return err
	}
	wanted := make(map[string]struct{}, len(pulls))
	for _, item := range pulls {
		wanted[item.Path] = struct{}{}
	}
	for _, t := range rows {
		if t.PeerID != peer || t.State == transfer.StateFailed {
			continue
		}
		if _, ok := wanted[t.Path]; ok && t.State != transfer.StateDone {
			continue
		}
		if err := p.dropTransfer(ctx, t, "the plan no longer wants it"); err != nil {
			return err
		}
	}
	return nil
}

// dropTransfer removes one journal row and its partial file.
func (p *Puller) dropTransfer(ctx context.Context, t transfer.Transfer, reason string) error {
	p.forget(t.ID)
	p.removeTemp(t.TempPath)
	if err := p.store.DeleteTransfer(ctx, t.ID); err != nil {
		return err
	}
	p.log.Info("transfer dropped from the journal", "folder", t.FolderID, "path", t.Path,
		"transfer", t.ID, "state", string(t.State), "reason", reason)
	p.publish("transfer.dropped", map[string]any{
		"transfer_id": string(t.ID), "folder_id": string(t.FolderID),
		"path": t.Path, "reason": reason,
	})
	return nil
}

// apply carries out one non-pull plan decision.
func (p *Puller) apply(ctx context.Context, f folder.Folder, peer device.ID,
	item syncplan.Item, summary *Summary) error {
	switch item.Action {
	case syncplan.ActionMkDir:
		entry, err := p.installer.MkDir(ctx, f.Path, *item.Remote)
		if err != nil {
			return err
		}
		summary.Dirs++
		return p.record(ctx, f.ID, fromRemote(entry, *item.Remote, peer))

	case syncplan.ActionReplace:
		return p.replace(ctx, f, peer, item, summary)

	case syncplan.ActionChmod:
		entry, err := p.installer.Chmod(ctx, f.Path, *item.Remote)
		if err != nil {
			return err
		}
		summary.Modes++
		return p.record(ctx, f.ID, fromRemote(entry, *item.Remote, peer))

	case syncplan.ActionSymlink:
		entry, err := p.installer.Symlink(ctx, f.Path, *item.Remote)
		if err != nil {
			return err
		}
		summary.Symlinks++
		return p.record(ctx, f.ID, fromRemote(entry, *item.Remote, peer))

	case syncplan.ActionDelete:
		if err := p.installer.Delete(ctx, f.Path, item.Path); err != nil {
			return err
		}
		summary.Deleted++
		return p.recordDeletion(ctx, f.ID, peer, item)

	case syncplan.ActionConflict:
		summary.Conflicts++
		return p.conflictKept(ctx, f, peer, item)

	default:
		summary.Skipped++
		if item.Reason == syncplan.ReasonInSync {
			return p.recordAgreement(ctx, f.ID, item)
		}
		return nil
	}
}

// replace applies a path whose kind changed on the peer: the local entry goes
// first, then the new kind is created in its place. Only directories and
// symlinks land here; a file that replaces another kind is downloaded first
// and installed by pull.
func (p *Puller) replace(ctx context.Context, f folder.Folder, peer device.ID,
	item syncplan.Item, summary *Summary) error {
	if err := p.installer.RemoveReplaced(ctx, f.Path, item.Path, item.Local); err != nil {
		return err
	}
	switch item.Remote.Kind {
	case manifest.KindDir:
		entry, err := p.installer.MkDir(ctx, f.Path, *item.Remote)
		if err != nil {
			return err
		}
		summary.Dirs++
		return p.record(ctx, f.ID, fromRemote(entry, *item.Remote, peer))
	default:
		entry, err := p.installer.Symlink(ctx, f.Path, *item.Remote)
		if err != nil {
			return err
		}
		summary.Symlinks++
		return p.record(ctx, f.ID, fromRemote(entry, *item.Remote, peer))
	}
}

// recordDeletion writes the tombstone of a path the peer deleted. It adopts
// the peer's vector rather than raising this device's counter: the deletion is
// the peer's version of the path, so a peer that still holds the live file is
// dominated by it and deletes as well, instead of sending the file back.
func (p *Puller) recordDeletion(ctx context.Context, id folder.ID, peer device.ID, item syncplan.Item) error {
	tombstone := manifest.Entry{
		Path:    item.Path,
		Kind:    item.Local.Kind,
		Deleted: true,
		ModTime: p.clock.Now(),
	}
	return p.record(ctx, id, fromRemote(tombstone, *item.Remote, peer))
}

// recordAgreement stores what both sides now agree on for a path that needs no
// change: the merged version vector and the permission bits.
func (p *Puller) recordAgreement(ctx context.Context, id folder.ID, item syncplan.Item) error {
	if item.Local == nil || item.Remote == nil {
		return nil
	}
	// Both sides hold the same content, so the path's history is the merge
	// of the two vectors: whichever of them a third device holds, it is
	// then dominated and takes nothing over.
	if merged := item.Local.Version.Merge(item.Remote.Version); !merged.Equals(item.Local.Version) {
		if err := p.store.SetVersion(ctx, id, item.Path, merged); err != nil {
			return err
		}
	}
	if item.Local.Kind != manifest.KindFile || item.Local.SyncedMode == item.Local.Mode {
		return nil
	}
	return p.store.SetSyncedMode(ctx, id, item.Path, item.Local.Mode)
}

// itemFailed reports one plan item that could not be applied, leaving the rest
// of the cycle to run. The count ends up in the summary the daemon logs.
func (p *Puller) itemFailed(f folder.Folder, peer device.ID, item syncplan.Item,
	cause error, summary *Summary) {
	summary.Failed++
	if summary.Failure == nil {
		summary.Failure = fmt.Errorf("%s %s: %w", item.Action.String(), item.Path, cause)
	}
	p.log.Warn("plan item not applied", "folder", f.ID, "path", item.Path,
		"action", item.Action.String(), "error", cause)
	p.publish("sync.item_failed", map[string]any{
		"folder_id": string(f.ID), "path": item.Path, "action": item.Action.String(),
		"peer_id": string(peer), "error": cause.Error(),
	})
}

// recordOutcome stores on the folder what this cycle could not apply, so a
// folder nothing can be written into says so instead of reporting itself
// idle. A cycle that ended early, because the connection or the store went
// away, only records failures it already collected: it must not clear a
// record it did not get far enough to disprove.
func (p *Puller) recordOutcome(ctx context.Context, f folder.Folder, summary Summary, complete bool) {
	if ctx.Err() != nil || (!complete && summary.Failed == 0) {
		return
	}
	message := ""
	if summary.Failed > 0 {
		message = fmt.Sprintf("%d of the last sync's items were not applied", summary.Failed)
		if summary.Failure != nil {
			message = summary.Failure.Error()
		}
	}
	if err := p.store.SetFolderSyncError(ctx, f.ID, message, summary.Failed); err != nil && ctx.Err() == nil {
		p.log.Warn("sync outcome not recorded", "folder", f.ID, "error", err)
	}
}

// pullAll downloads the files of a plan, at most Concurrency at a time. A
// failure that only concerns one file is counted and the cycle continues; any
// other failure ends the cycle, because the connection or the store is gone.
func (p *Puller) pullAll(ctx context.Context, conn ports.PeerConn, f folder.Folder,
	items []syncplan.Item, summary *Summary) error {
	if len(items) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		fatalErr error
	)
	slots := make(chan struct{}, p.workers)
	for _, item := range items {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}

		wg.Add(1)
		go func(item syncplan.Item) {
			defer wg.Done()
			defer func() { <-slots }()

			bytes, err := p.pull(ctx, conn, f, item)
			switch {
			case err == nil:
				mu.Lock()
				if item.Action == syncplan.ActionConflict {
					summary.Conflicts++
				} else {
					summary.Pulled++
				}
				summary.Bytes += bytes
				mu.Unlock()
			case errors.Is(err, ports.ErrPaused), errors.Is(err, ErrOutsideWindow):
				// Every worker notices the pause, or the window
				// closing, on its own and leaves its transfer
				// resumable, so the cycle ends without a
				// failure.
				mu.Lock()
				summary.Skipped++
				mu.Unlock()
			case isPerFile(err):
				mu.Lock()
				summary.Failed++
				if summary.Failure == nil {
					summary.Failure = fmt.Errorf("pull %s: %w", item.Path, err)
				}
				mu.Unlock()
				p.log.Warn("file not pulled", "folder", f.ID, "path", item.Path, "error", err)
			default:
				mu.Lock()
				if localFailure(err) {
					// The folder itself is what could not be
					// written, which is worth reporting even
					// though it ends the cycle.
					summary.Failed++
					if summary.Failure == nil {
						summary.Failure = fmt.Errorf("pull %s: %w", item.Path, err)
					}
				}
				if fatalErr == nil {
					fatalErr = err
					cancel()
				}
				mu.Unlock()
			}
		}(item)
	}
	wg.Wait()

	if fatalErr != nil {
		return fmt.Errorf("pull folder %q: %w", f.ID, fatalErr)
	}
	return ctx.Err()
}

// pull downloads one file and installs it, returning how many bytes arrived.
func (p *Puller) pull(ctx context.Context, conn ports.PeerConn, f folder.Folder,
	item syncplan.Item) (int64, error) {
	// A folder paused while the cycle was planning gets no new transfer
	// journal at all; the file is picked up again when it resumes.
	if p.pausedNow(ctx, f.ID) {
		return 0, fmt.Errorf("folder %q: %w", f.ID, ports.ErrPaused)
	}
	if item.Action == syncplan.ActionReplace {
		// Asking before the download means a directory that must stay
		// does not cost a transfer every cycle.
		if err := p.installer.CheckReplaceable(ctx, f.Path, item.Path, item.Local); err != nil {
			return 0, err
		}
	}
	t, err := p.transferFor(ctx, f, item, conn.RemoteID())
	if err != nil {
		return 0, err
	}
	if err := p.resume(ctx, &t); err != nil {
		return 0, err
	}
	if err := p.store.SetTransferState(ctx, t.ID, transfer.StateActive, "", p.clock.Now()); err != nil {
		return 0, err
	}

	if err := p.fetch(ctx, conn, &t); err != nil {
		return 0, p.stopped(ctx, t, err)
	}
	if err := p.store.SetTransferState(ctx, t.ID, transfer.StateVerifying, "", p.clock.Now()); err != nil {
		return 0, err
	}

	installed, err := p.install(ctx, f, conn.RemoteID(), item, t)
	if err != nil {
		return 0, p.stopped(ctx, t, err)
	}
	for _, a := range installed {
		if err := p.record(ctx, f.ID, a); err != nil {
			return 0, err
		}
	}
	if err := p.store.SetTransferState(ctx, t.ID, transfer.StateDone, "", p.clock.Now()); err != nil {
		return 0, err
	}
	// The files table is the record of what arrived; a finished transfer
	// only kept the journal alive, so its rows go away with it.
	if err := p.store.DeleteTransfer(ctx, t.ID); err != nil {
		return 0, err
	}
	p.forget(t.ID)

	p.publish("transfer.completed", map[string]any{
		"transfer_id": string(t.ID), "folder_id": string(f.ID), "path": t.Path, "size": t.Size,
	})
	return t.Size, nil
}

// install puts a finished download in its place and returns the entries that
// now describe the folder. A conflict keeps the local version as well, so it
// is the only case that returns two.
func (p *Puller) install(ctx context.Context, f folder.Folder, peer device.ID,
	item syncplan.Item, t transfer.Transfer) ([]arrived, error) {
	if item.Action == syncplan.ActionConflict {
		return p.keepBoth(ctx, f, peer, item, t)
	}
	expect := item.Local
	if item.Action == syncplan.ActionReplace {
		// The directory the file takes the place of goes now, with the
		// downloaded content ready, so the path is uncovered as briefly
		// as possible.
		if err := p.installer.RemoveReplaced(ctx, f.Path, item.Path, item.Local); err != nil {
			return nil, err
		}
		expect = nil
	}
	entry, err := p.installer.Install(ctx, f.Path, t, expect)
	if err != nil {
		return nil, err
	}
	return []arrived{fromRemote(entry, *item.Remote, peer)}, nil
}

// transferFor returns the journal entry to continue, or starts a new one. A
// live transfer for another version of the same path is given up on first,
// because only one transfer per path may be live.
func (p *Puller) transferFor(ctx context.Context, f folder.Folder, item syncplan.Item,
	peer device.ID) (transfer.Transfer, error) {
	remote := *item.Remote
	// The chunk list is built from the size the peer announced, and every
	// chunk becomes a journal row, so a file that would need more chunks
	// than a transfer may have is refused before anything is allocated.
	if n := transfer.ChunkCount(remote.Size, p.chunkSize); n > transfer.MaxChunks {
		return transfer.Transfer{}, fmt.Errorf("%s: %d chunks, the limit is %d: %w",
			item.Path, n, transfer.MaxChunks, ErrTooManyChunks)
	}
	existing, err := p.store.FindLiveTransfer(ctx, f.ID, item.Path)
	switch {
	case err == nil:
		if existing.Hash == remote.Hash && existing.Size == remote.Size && existing.ChunkSize == p.chunkSize {
			return existing, nil
		}
		if err := p.giveUp(ctx, existing, errSuperseded); err != nil {
			return transfer.Transfer{}, err
		}
	case !errors.Is(err, ports.ErrNotFound):
		return transfer.Transfer{}, err
	}

	id, err := newTransferID()
	if err != nil {
		return transfer.Transfer{}, err
	}
	tempPath, err := p.newTemp(f.Path, id)
	if err != nil {
		return transfer.Transfer{}, err
	}

	// An earlier attempt at this path that was given up on has had its
	// answer: this transfer.
	if err := p.pruneFailed(ctx, f.ID, item.Path); err != nil {
		return transfer.Transfer{}, err
	}

	t := transfer.NewInbound(id, f.ID, remote, peer, p.chunkSize, tempPath, p.clock.Now())
	if err := p.store.SaveTransfer(ctx, t); err != nil {
		return transfer.Transfer{}, err
	}
	p.log.Debug("transfer started", "folder", f.ID, "path", t.Path, "size", t.Size, "chunks", len(t.Chunks))
	return t, nil
}

// resume makes the journal agree with the partial file on disk. The file is
// the authority: a chunk the journal calls verified is re-read and re-hashed,
// so a crash between the write and the journal update cannot leave a wrong
// byte behind. A partial file that is gone resets the whole transfer.
func (p *Puller) resume(ctx context.Context, t *transfer.Transfer) error {
	p.mu.Lock()
	done := p.resumed[t.ID]
	p.resumed[t.ID] = true
	p.mu.Unlock()
	if done {
		return nil
	}

	info, err := p.fs.Lstat(t.TempPath)
	if errors.Is(err, fs.ErrNotExist) {
		t.ResetChunks()
		tempPath, err := p.newTempIn(path.Dir(t.TempPath), t.ID)
		if err != nil {
			return err
		}
		t.TempPath = tempPath
		p.log.Debug("partial file gone, starting over", "path", t.Path, "transfer", t.ID)
		return p.store.SaveTransfer(ctx, *t)
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", t.TempPath, err)
	}

	changed := false
	for i, c := range t.Chunks {
		if !c.Verified {
			continue
		}
		if c.Offset+int64(c.Length) > info.Size {
			t.Chunks[i].Verified = false
			changed = true
			continue
		}
		data, err := p.fs.ReadAt(t.TempPath, c.Offset, c.Length)
		if err != nil {
			return fmt.Errorf("read %s at %d: %w", t.TempPath, c.Offset, err)
		}
		if len(data) != c.Length || p.hasher.SumBytes(data) != c.Hash {
			t.Chunks[i].Verified = false
			t.Chunks[i].Hash = manifest.Hash{}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	p.log.Debug("partial file disagrees with the journal", "path", t.Path, "transfer", t.ID)
	return p.store.SaveTransfer(ctx, *t)
}

// fetch downloads every chunk the journal is still missing.
func (p *Puller) fetch(ctx context.Context, conn ports.PeerConn, t *transfer.Transfer) error {
	missing := t.MissingChunks()
	if len(missing) == 0 {
		return nil
	}

	temp, err := p.fs.OpenTemp(t.TempPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", t.TempPath, err)
	}
	defer func() { _ = temp.Close() }()

	for n, idx := range missing {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The check sits before the request, so a pause takes effect
		// within one chunk: the chunk being fetched is finished and
		// recorded, and nothing more is asked for.
		if p.pausedNow(ctx, t.FolderID) {
			return fmt.Errorf("folder %q: %w", t.FolderID, ports.ErrPaused)
		}
		// A window that closed stops the transfer just as a pause
		// does, within one chunk. It stays pending, so the first
		// cycle after the window opens continues it from the journal.
		if p.outsideWindowNow(ctx, t.FolderID) {
			return ErrOutsideWindow
		}
		chunk := t.Chunks[idx]
		// The budget is taken before the request, not after the reply:
		// a peer that is never asked sends nothing, so nothing has to
		// be buffered here while the limit is waited out.
		if p.download != nil {
			if err := p.download.Wait(ctx, chunk.Length); err != nil {
				return err
			}
		}
		data, err := p.chunk(ctx, conn, *t, chunk)
		if err != nil {
			return err
		}
		if _, err := temp.WriteAt(data, chunk.Offset); err != nil {
			return fmt.Errorf("write %s at %d: %w", t.TempPath, chunk.Offset, err)
		}
		if (n+1)%syncEvery == 0 {
			if err := temp.Sync(); err != nil {
				return fmt.Errorf("flush %s: %w", t.TempPath, err)
			}
		}

		hash := p.hasher.SumBytes(data)
		now := p.clock.Now()
		if err := p.store.MarkChunkVerified(ctx, t.ID, idx, hash, now); err != nil {
			return err
		}
		if err := t.MarkVerified(idx, hash, now); err != nil {
			return err
		}
		p.publish("transfer.chunk_verified", map[string]any{
			"transfer_id": string(t.ID), "folder_id": string(t.FolderID),
			"path": t.Path, "index": idx,
		})
	}

	if err := temp.Truncate(t.Size); err != nil {
		return fmt.Errorf("truncate %s: %w", t.TempPath, err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", t.TempPath, err)
	}
	return nil
}

// chunk asks the peer for one chunk and verifies it before it is written. A
// chunk that does not hash to what the peer announced is asked for again; a
// peer that keeps sending broken data fails the transfer.
func (p *Puller) chunk(ctx context.Context, conn ports.PeerConn, t transfer.Transfer,
	c transfer.Chunk) ([]byte, error) {
	req := ports.ChunkRequest{
		FolderID: t.FolderID, Path: t.Path, Hash: t.Hash, Index: c.Index, ChunkSize: t.ChunkSize,
	}

	var last error
	for attempt := 1; attempt <= chunkAttempts; attempt++ {
		data, err := conn.RequestChunk(ctx, req)
		if err != nil {
			return nil, err
		}
		switch {
		case len(data.Data) != c.Length:
			last = fmt.Errorf("chunk %d of %s: %d bytes, expected %d: %w",
				c.Index, t.Path, len(data.Data), c.Length, ErrIntegrity)
		case p.hasher.SumBytes(data.Data) != data.ChunkHash:
			last = fmt.Errorf("chunk %d of %s: content does not match its hash: %w",
				c.Index, t.Path, ErrIntegrity)
		default:
			return data.Data, nil
		}
		p.log.Warn("chunk failed verification", "path", t.Path, "index", c.Index,
			"attempt", attempt, "error", last)
	}
	return nil, last
}

// stopped records how a transfer ended. Only a failure of the file itself
// gives it up; a connection or a store that went away leaves it resumable, so
// the next cycle continues where this one stopped.
func (p *Puller) stopped(ctx context.Context, t transfer.Transfer, cause error) error {
	if ctx.Err() != nil {
		// A cancelled transfer keeps its journal and its partial file,
		// but not the note that this process already re-verified it:
		// the cancellation may have stopped a write halfway, and a
		// transfer nothing picks up again would leave the note behind
		// for as long as the process runs.
		p.forget(t.ID)
		return ctx.Err()
	}
	if isPerFile(cause) {
		return p.giveUp(ctx, t, cause)
	}
	if errors.Is(cause, ports.ErrPaused) {
		p.forget(t.ID)
		if err := p.store.SetTransferState(ctx, t.ID, transfer.StatePaused, "", p.clock.Now()); err != nil {
			p.log.Warn("transfer state not recorded", "transfer", t.ID, "error", err)
		}
		p.log.Debug("transfer paused", "folder", t.FolderID, "path", t.Path,
			"chunks_done", len(t.Chunks)-len(t.MissingChunks()))
		return cause
	}

	p.forget(t.ID)
	if err := p.store.SetTransferState(ctx, t.ID, transfer.StatePending, cause.Error(), p.clock.Now()); err != nil {
		p.log.Warn("transfer state not recorded", "transfer", t.ID, "error", err)
	}
	return cause
}

// giveUp ends a transfer that will not be continued, and decides what its
// journal row is worth afterwards. A row whose plan went stale is worth
// nothing: the next cycle re-decides the path from scratch, so the row and its
// partial file go and the journal empties itself. Bytes that did not hash right
// are the one thing worth looking at later, so that row and its file stay.
func (p *Puller) giveUp(ctx context.Context, t transfer.Transfer, cause error) error {
	p.forget(t.ID)
	if !superseded(cause) {
		if err := p.store.SetTransferState(ctx, t.ID, transfer.StateFailed, cause.Error(), p.clock.Now()); err != nil {
			return err
		}
		p.publish("transfer.failed", map[string]any{
			"transfer_id": string(t.ID), "folder_id": string(t.FolderID),
			"path": t.Path, "error": cause.Error(),
		})
		return cause
	}

	p.removeTemp(t.TempPath)
	if err := p.store.DeleteTransfer(ctx, t.ID); err != nil {
		return err
	}
	p.log.Info("transfer superseded", "folder", t.FolderID, "path", t.Path, "reason", cause.Error())
	p.publish("transfer.superseded", map[string]any{
		"transfer_id": string(t.ID), "folder_id": string(t.FolderID),
		"path": t.Path, "reason": cause.Error(),
	})
	return cause
}

// errSuperseded says a transfer was started for a version the peer no longer
// offers.
var errSuperseded = errors.New("the peer now offers another version")

// superseded reports whether a failure means the plan the transfer was started
// from is stale, rather than something about the bytes. Either side may have
// moved on: the peer serves another version now, or the local path changed
// under the download. Neither leaves anything to inspect.
func superseded(cause error) bool {
	var refused *ports.ChunkError
	if errors.As(cause, &refused) {
		return refused.Reason == ports.ChunkHashChanged || refused.Reason == ports.ChunkNotFound
	}
	return errors.Is(cause, errSuperseded) ||
		errors.Is(cause, ErrChangedUnderneath) || errors.Is(cause, ErrDirNotEmpty)
}

// pruneFailed removes the failed rows a path still carries, with their partial
// files, before a new transfer for it is journalled. A failed row is kept only
// as long as the path has not converged; a cycle that wants the path again is
// the newer answer, so at most one failed row per path can exist.
func (p *Puller) pruneFailed(ctx context.Context, id folder.ID, rel string) error {
	rows, err := p.store.ListTransfers(ctx, ports.TransferFilter{
		FolderID: id, Path: rel, State: transfer.StateFailed,
	})
	if err != nil {
		return err
	}
	for _, t := range rows {
		p.removeTemp(t.TempPath)
		if err := p.store.DeleteTransfer(ctx, t.ID); err != nil {
			return err
		}
		p.log.Debug("failed transfer pruned", "folder", id, "path", rel, "transfer", t.ID)
	}
	return nil
}

// arrived is an entry that now sits on disk together with the history it
// carries: the device that produced the content, which is the one the peer
// announced and not the peer it came from, and the version vector that orders
// it against the same path elsewhere.
type arrived struct {
	entry   manifest.Entry
	author  device.ID
	version manifest.Version
}

// fromRemote describes an entry installed as the peer announced it. A peer
// that names no author is itself the author, which is what an entry written
// before authors were recorded looks like.
func fromRemote(e manifest.Entry, remote manifest.Entry, peer device.ID) arrived {
	return arrived{entry: e, author: authorOf(remote, peer), version: remote.Version}
}

// record stores what now sits on disk: the author, the vector the content
// carries and the mode as the new baseline, so the next local scan sees an
// unchanged file and does not send it back.
func (p *Puller) record(ctx context.Context, id folder.ID, a arrived) error {
	e := a.entry
	e.ModifiedBy = a.author
	e.Version = a.version
	e.SyncedMode = e.Mode
	return p.store.UpsertEntries(ctx, id, []manifest.Entry{e})
}

// self is the device this puller installs for, empty when it has no identity.
func (p *Puller) self() device.ID {
	if p.identity == nil {
		return ""
	}
	return p.identity.DeviceID()
}

// newTemp creates an empty partial file inside the temp directory of a folder
// root and returns its path.
func (p *Puller) newTemp(root string, id transfer.ID) (string, error) {
	return p.newTempIn(p.installer.TempDir(root), id)
}

// newTempIn creates an empty partial file in dir and returns its path.
func (p *Puller) newTempIn(dir string, id transfer.ID) (string, error) {
	if err := p.fs.MkdirAll(dir, tempDirMode); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := p.fs.CreateTemp(dir, string(id)+"-*.part")
	if err != nil {
		return "", fmt.Errorf("create partial file in %s: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	return f.Name(), nil
}

// removeTemp deletes a partial file, ignoring one that is already gone.
func (p *Puller) removeTemp(path string) {
	if err := p.fs.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.log.Warn("partial file not removed", "path", path, "error", err)
	}
}

// forget drops the resume mark of a transfer that is over.
func (p *Puller) forget(id transfer.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.resumed, id)
}

// publish emits an event when a sink is configured.
func (p *Puller) publish(kind string, fields map[string]any) {
	if p.events == nil {
		return
	}
	p.events.Publish(ports.Event{Kind: kind, At: p.clock.Now(), Fields: fields})
}

// isPerFile reports whether a failure concerns only one file, leaving the rest
// of the cycle worth running. A peer that refuses the whole folder, or a
// connection that broke, is not one of those.
func isPerFile(err error) bool {
	var refused *ports.ChunkError
	if errors.As(err, &refused) {
		return refused.Reason == ports.ChunkHashChanged || refused.Reason == ports.ChunkNotFound
	}
	return errors.Is(err, ErrIntegrity) || errors.Is(err, ErrChangedUnderneath) ||
		errors.Is(err, ErrDirNotEmpty) || errors.Is(err, ErrTooManyChunks) ||
		errors.Is(err, ErrUnsafePath)
}

// localFailure reports whether a failure is about this device's filesystem
// rather than the peer or the store. Every filesystem call returns its failure
// as a *fs.PathError; a peer that refuses a chunk and a store that is gone do
// not.
func localFailure(err error) bool {
	var pathErr *fs.PathError
	return errors.As(err, &pathErr)
}

// isFileReplace reports whether an item replaces another kind with a file,
// which is applied by the pull phase rather than by apply.
func isFileReplace(item syncplan.Item) bool {
	return item.Action == syncplan.ActionReplace && item.Remote != nil &&
		item.Remote.Kind == manifest.KindFile
}

// newTransferID returns a random 32 character hexadecimal transfer id.
func newTransferID() (transfer.ID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate transfer id: %w", err)
	}
	return transfer.ID(hex.EncodeToString(raw[:])), nil
}
