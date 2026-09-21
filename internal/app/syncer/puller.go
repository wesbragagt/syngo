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
}

// Puller fetches from a peer what this device is missing, one folder at a
// time. It owns the transfer journal: every chunk it verifies is recorded, so
// an interrupted download resumes instead of starting over.
type Puller struct {
	store     PullStore
	fs        ports.FileSystem
	hasher    ports.Hasher
	clock     ports.Clock
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
	events ports.EventSink, cfg PullerConfig, log *slog.Logger) *Puller {
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
		store: store, fs: fsys, hasher: hasher, clock: clock, events: events,
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
		return summary, err
	}

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
// resume: a transfer nobody is fetching any more is pending, not active, and a
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
		return p.record(ctx, f.ID, entry, peer)

	case syncplan.ActionReplace:
		return p.replace(ctx, f, peer, item, summary)

	case syncplan.ActionChmod:
		entry, err := p.installer.Chmod(ctx, f.Path, *item.Remote)
		if err != nil {
			return err
		}
		summary.Modes++
		return p.record(ctx, f.ID, entry, peer)

	case syncplan.ActionSymlink:
		entry, err := p.installer.Symlink(ctx, f.Path, *item.Remote)
		if err != nil {
			return err
		}
		summary.Symlinks++
		return p.record(ctx, f.ID, entry, peer)

	case syncplan.ActionDelete:
		if err := p.installer.Delete(ctx, f.Path, item.Path); err != nil {
			return err
		}
		summary.Deleted++
		return p.store.MarkDeleted(ctx, f.ID, []string{item.Path}, peer, p.clock.Now())

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
		return p.record(ctx, f.ID, entry, peer)
	default:
		entry, err := p.installer.Symlink(ctx, f.Path, *item.Remote)
		if err != nil {
			return err
		}
		summary.Symlinks++
		return p.record(ctx, f.ID, entry, peer)
	}
}

// recordAgreement stores what both sides now agree on for a path that needs no
// change: the hash and the permission bits. They are the baseline the plan
// compares against, so a later change is attributed to the side that made it.
func (p *Puller) recordAgreement(ctx context.Context, id folder.ID, item syncplan.Item) error {
	if item.Local == nil || item.Remote == nil {
		return nil
	}
	if item.Local.SyncedHash != item.Remote.Hash {
		if err := p.store.SetSyncedHash(ctx, id, item.Path, item.Remote.Hash); err != nil {
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
	p.log.Warn("plan item not applied", "folder", f.ID, "path", item.Path,
		"action", item.Action.String(), "error", cause)
	p.publish("sync.item_failed", map[string]any{
		"folder_id": string(f.ID), "path": item.Path, "action": item.Action.String(),
		"peer_id": string(peer), "error": cause.Error(),
	})
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
			case errors.Is(err, ports.ErrPaused):
				// Every worker notices the pause on its own and
				// leaves its transfer resumable, so the cycle
				// ends without a failure.
				mu.Lock()
				summary.Skipped++
				mu.Unlock()
			case isPerFile(err):
				mu.Lock()
				summary.Failed++
				mu.Unlock()
				p.log.Warn("file not pulled", "folder", f.ID, "path", item.Path, "error", err)
			default:
				mu.Lock()
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

	entries, err := p.install(ctx, f, conn.RemoteID(), item, t)
	if err != nil {
		return 0, p.stopped(ctx, t, err)
	}
	for _, entry := range entries {
		if err := p.record(ctx, f.ID, entry, conn.RemoteID()); err != nil {
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
	item syncplan.Item, t transfer.Transfer) ([]manifest.Entry, error) {
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
	return []manifest.Entry{entry}, nil
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
		if err := p.failed(ctx, existing, errors.New("the peer now offers another version")); err != nil {
			return transfer.Transfer{}, err
		}
		p.removeTemp(existing.TempPath)
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
		return p.failed(ctx, t, cause)
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

// failed records why a transfer stopped. The partial file is kept so the
// failure can be inspected, except when the peer's version changed, in which
// case the bytes are worthless.
func (p *Puller) failed(ctx context.Context, t transfer.Transfer, cause error) error {
	var refused *ports.ChunkError
	if errors.As(cause, &refused) &&
		(refused.Reason == ports.ChunkHashChanged || refused.Reason == ports.ChunkNotFound) {
		p.removeTemp(t.TempPath)
	}
	p.forget(t.ID)

	if err := p.store.SetTransferState(ctx, t.ID, transfer.StateFailed, cause.Error(), p.clock.Now()); err != nil {
		return err
	}
	p.publish("transfer.failed", map[string]any{
		"transfer_id": string(t.ID), "folder_id": string(t.FolderID),
		"path": t.Path, "error": cause.Error(),
	})
	return cause
}

// record stores what now sits on disk, attributed to the peer that sent it, so
// the next local scan sees an unchanged file and does not send it back.
func (p *Puller) record(ctx context.Context, id folder.ID, e manifest.Entry, peer device.ID) error {
	e.ModifiedBy = peer
	if e.Kind == manifest.KindFile {
		e.SyncedHash = e.Hash
	}
	e.SyncedMode = e.Mode
	return p.store.UpsertEntries(ctx, id, []manifest.Entry{e})
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
