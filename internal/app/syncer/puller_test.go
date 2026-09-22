package syncer_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/syncer"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// peerID is the device the destination pulls from.
const peerID = device.ID("SOURCE")

// destID is the device the destination puller installs for.
const destID = device.ID("DEST")

// dest is a device that pulls a folder from a peer.
type dest struct {
	store  *fakes.Store
	fs     *fakes.FileSystem
	hasher fakes.Hasher
	clock  *fakes.Clock
	events *fakes.Events
	puller *syncer.Puller
	folder folder.Folder
}

// newDest returns a receiving device connected to handler.
func newDest(t *testing.T, handler ports.PeerHandler) (*dest, *fakes.PeerConn) {
	t.Helper()
	f := testFolder("docs", false)
	f.Path = "/home/dest/docs"

	d := &dest{
		store: fakes.NewStore(), fs: fakes.NewFileSystem(), hasher: fakes.NewHasher(),
		clock: fakes.NewClock(time.Time{}), events: fakes.NewEvents(), folder: f,
	}
	d.fs.AddDir(f.Path, 0o755)
	if err := d.store.SaveFolder(context.Background(), f); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	d.puller = syncer.NewPuller(d.store, d.fs, d.hasher, d.clock, fakes.NewIdentity(destID, "dest"), d.events,
		syncer.PullerConfig{ChunkSize: chunkSize, Concurrency: 2}, nil)
	return d, fakes.NewPeerConn(peerID, "nas", handler, time.Millisecond)
}

// restart replaces the puller with a fresh one, as a daemon restart would.
// The store and the filesystem survive, the in-memory state does not.
func (d *dest) restart() {
	d.puller = syncer.NewPuller(d.store, d.fs, d.hasher, d.clock, fakes.NewIdentity(destID, "dest"), d.events,
		syncer.PullerConfig{ChunkSize: chunkSize, Concurrency: 2}, nil)
}

// sync runs one sync cycle and fails the test when it does not finish.
func (d *dest) sync(t *testing.T, conn *fakes.PeerConn) syncer.Summary {
	t.Helper()
	summary, err := d.puller.SyncFolder(context.Background(), conn, d.folder)
	if err != nil {
		t.Fatalf("sync folder: %v", err)
	}
	return summary
}

// content returns the bytes of a path inside the destination folder.
func (d *dest) content(t *testing.T, rel string) []byte {
	t.Helper()
	data, ok := d.fs.Content(d.folder.Path + "/" + rel)
	if !ok {
		t.Fatalf("%s does not exist in the destination", rel)
	}
	return data
}

// record stores a local entry the way a scan would, and writes the file. The
// version is the one the local history of the path carries, which is what the
// plan orders against the peer's.
func (d *dest) record(t *testing.T, rel string, data []byte, v manifest.Version) manifest.Entry {
	t.Helper()
	full := d.folder.Path + "/" + rel
	d.fs.AddFile(full, data, 0o644)
	info, err := d.fs.Lstat(full)
	if err != nil {
		t.Fatalf("lstat %s: %v", full, err)
	}
	e := manifest.Entry{
		Path: rel, Kind: manifest.KindFile, Size: info.Size, ModTime: info.ModTime, Mode: 0o644,
		Hash: d.hasher.SumBytes(data), Version: v, SyncedMode: 0o644, Inode: info.Inode,
	}
	if err := d.store.UpsertEntries(context.Background(), d.folder.ID, []manifest.Entry{e}); err != nil {
		t.Fatalf("record %s: %v", rel, err)
	}
	return e
}

// recordDir creates a directory in the destination and records it the way a
// scan would.
func (d *dest) recordDir(t *testing.T, rel string) {
	t.Helper()
	full := d.folder.Path + "/" + rel
	d.fs.AddDir(full, 0o755)
	e := manifest.Entry{Path: rel, Kind: manifest.KindDir, Mode: 0o755}
	if err := d.store.UpsertEntries(context.Background(), d.folder.ID, []manifest.Entry{e}); err != nil {
		t.Fatalf("record %s: %v", rel, err)
	}
}

// liveTransfer returns the unfinished transfer of a path.
func (d *dest) liveTransfer(t *testing.T, rel string) transfer.Transfer {
	t.Helper()
	live, err := d.store.FindLiveTransfer(context.Background(), d.folder.ID, rel)
	if err != nil {
		t.Fatalf("live transfer for %s: %v", rel, err)
	}
	return live
}

// failedTransfer returns the single failed transfer of the folder.
func (d *dest) failedTransfer(t *testing.T) transfer.Transfer {
	t.Helper()
	list, err := d.store.ListTransfers(context.Background(),
		ports.TransferFilter{FolderID: d.folder.ID, State: transfer.StateFailed})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("%d failed transfers, want 1", len(list))
	}
	return list[0]
}

// transfers returns every journal row of the destination folder.
func (d *dest) transfers(t *testing.T) []transfer.Transfer {
	t.Helper()
	list, err := d.store.ListTransfers(context.Background(),
		ports.TransferFilter{FolderID: d.folder.ID})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	return list
}

// tempFiles returns the partial files under the folder's temp directory.
func (d *dest) tempFiles() []string {
	var out []string
	prefix := d.folder.Path + "/.syncthis/tmp/"
	for _, p := range d.fs.Paths() {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// chunkIndexes returns the chunk indexes a connection was asked for.
func chunkIndexes(conn *fakes.PeerConn) []int {
	var out []int
	for _, req := range conn.ChunkRequests() {
		out = append(out, req.Index)
	}
	return out
}

// corrupt serves real chunks but damages the payload of some of them.
type corrupt struct {
	ports.PeerHandler
	damaged func(idx int) bool
}

// Chunk flips a byte of the chunks the test asked to damage, leaving the
// announced chunk hash correct so the receiver notices.
func (c corrupt) Chunk(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	data, err := c.PeerHandler.Chunk(ctx, peer, req)
	if err != nil || !c.damaged(req.Index) || len(data.Data) == 0 {
		return data, err
	}
	data.Data[0] ^= 0xff
	return data, nil
}

// liar announces a whole-file hash that does not describe the bytes it serves.
type liar struct {
	ports.PeerHandler
	real manifest.Hash
	fake manifest.Hash
}

// Manifest replaces the real hash of every entry with the fake one.
func (l liar) Manifest(ctx context.Context, peer device.ID, id folder.ID) (manifest.Manifest, error) {
	m, err := l.PeerHandler.Manifest(ctx, peer, id)
	if err != nil {
		return m, err
	}
	for path, e := range m.Entries {
		if e.Hash == l.real {
			e.Hash = l.fake
			m.Entries[path] = e
		}
	}
	return m, nil
}

// Chunk serves the real bytes behind the announced hash.
func (l liar) Chunk(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	if req.Hash == l.fake {
		req.Hash = l.real
	}
	return l.PeerHandler.Chunk(ctx, peer, req)
}

// payload returns n bytes of recognizable content.
func payload(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i) ^ seed
	}
	return out
}

func TestSync_PullsNewFileAndPreservesModeAndMtime(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*2+11, 7)
	entry := src.addFile(t, "sub/big.bin", data)
	mtime := time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)
	if err := src.fs.Chtimes(src.folder.Path+"/sub/big.bin", mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	entry.ModTime = mtime
	entry.Mode = 0o640
	if err := src.store.UpsertEntries(context.Background(), src.folder.ID, []manifest.Entry{entry}); err != nil {
		t.Fatalf("record entry: %v", err)
	}

	d, conn := newDest(t, src.server)
	summary := d.sync(t, conn)

	if summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want one pulled file", summary)
	}
	if got := d.content(t, "sub/big.bin"); string(got) != string(data) {
		t.Fatalf("pulled %d bytes, want %d", len(got), len(data))
	}

	info, err := d.fs.Lstat(d.folder.Path + "/sub/big.bin")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode.Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", info.Mode.Perm())
	}
	if !info.ModTime.Equal(mtime) {
		t.Fatalf("mtime = %s, want %s", info.ModTime, mtime)
	}

	// The file is recorded with the version and the author the peer
	// announced, so the next local scan does not classify it as a local
	// change and a third device orders it against what it holds.
	stored, err := d.store.GetFile(context.Background(), d.folder.ID, "sub/big.bin")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if stored.Hash != entry.Hash || !stored.Version.Equals(entry.Version) {
		t.Fatalf("stored = %+v, want hash %s under version %q", stored, entry.Hash, entry.Version)
	}
	if stored.ModifiedBy != peerID {
		t.Fatalf("modified_by = %q, want %q", stored.ModifiedBy, peerID)
	}
	if stored.Size != info.Size || !stored.ModTime.Equal(info.ModTime) || stored.Inode != info.Inode {
		t.Fatalf("stored = %+v does not describe what is on disk (%+v)", stored, info)
	}

	// The journal is empty once the file is in place.
	list, err := d.store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("%d transfers left behind, want none", len(list))
	}
}

func TestSync_ZeroByteFile(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "empty.txt", nil)

	d, conn := newDest(t, src.server)
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want one pulled file", summary)
	}
	if got := d.content(t, "empty.txt"); len(got) != 0 {
		t.Fatalf("empty.txt has %d bytes, want 0", len(got))
	}
	if reqs := conn.ChunkRequests(); len(reqs) != 0 {
		t.Fatalf("asked for %d chunks of an empty file, want none", len(reqs))
	}
}

func TestSync_ResumeSkipsVerifiedChunks(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*4, 3)
	entry := src.addFile(t, "big.bin", data)

	// The first cycle is cut short after two chunks by a peer that stops
	// answering.
	d, conn := newDest(t, src.server)
	ctx, cancel := context.WithCancel(context.Background())
	counted := 0
	stopping := fakes.NewPeerConn(peerID, "nas", handlerFunc{
		manifest: src.server.Manifest,
		chunk: func(c context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			counted++
			if counted > 2 {
				cancel()
				return ports.ChunkData{}, c.Err()
			}
			return src.server.Chunk(c, peer, req)
		},
	}, time.Millisecond)

	if _, err := d.puller.SyncFolder(ctx, stopping, d.folder); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted sync returned %v, want a cancellation", err)
	}
	cancel()

	live := d.liveTransfer(t, "big.bin")
	if done := len(live.Chunks) - len(live.MissingChunks()); done != 2 {
		t.Fatalf("%d chunks verified, want 2", done)
	}

	// The second cycle, after a restart, only asks for what is missing.
	d.restart()
	d.sync(t, conn)
	if got := chunkIndexes(conn); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("second cycle asked for chunks %v, want [2 3]", got)
	}
	if string(d.content(t, "big.bin")) != string(data) {
		t.Fatal("resumed file does not match the source")
	}
	if entry.Hash != d.hasher.SumBytes(d.content(t, "big.bin")) {
		t.Fatal("resumed file does not hash to the announced hash")
	}
}

func TestSync_ResumeRehashDetectsCorruptTemp(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*3, 9)
	src.addFile(t, "big.bin", data)

	d, conn := newDest(t, src.server)
	ctx, cancel := context.WithCancel(context.Background())
	counted := 0
	stopping := fakes.NewPeerConn(peerID, "nas", handlerFunc{
		manifest: src.server.Manifest,
		chunk: func(c context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			counted++
			if counted > 2 {
				cancel()
				return ports.ChunkData{}, c.Err()
			}
			return src.server.Chunk(c, peer, req)
		},
	}, time.Millisecond)
	if _, err := d.puller.SyncFolder(ctx, stopping, d.folder); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted sync returned %v, want a cancellation", err)
	}
	cancel()

	// A byte of an already verified chunk is damaged behind the puller's
	// back, as a torn write would.
	live := d.liveTransfer(t, "big.bin")
	partial, ok := d.fs.Content(live.TempPath)
	if !ok {
		t.Fatalf("partial file %s is missing", live.TempPath)
	}
	partial[chunkSize+3] ^= 0xff
	d.fs.AddFile(live.TempPath, partial, 0o600)

	d.restart()
	d.sync(t, conn)
	if got := chunkIndexes(conn); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("second cycle asked for chunks %v, want [1 2]: the damaged one and the missing one", got)
	}
	if string(d.content(t, "big.bin")) != string(data) {
		t.Fatal("installed file does not match the source")
	}
}

func TestSync_ResumeMissingTempResetsAll(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*2, 5)
	src.addFile(t, "big.bin", data)

	d, conn := newDest(t, src.server)
	ctx, cancel := context.WithCancel(context.Background())
	counted := 0
	stopping := fakes.NewPeerConn(peerID, "nas", handlerFunc{
		manifest: src.server.Manifest,
		chunk: func(c context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			counted++
			if counted > 1 {
				cancel()
				return ports.ChunkData{}, c.Err()
			}
			return src.server.Chunk(c, peer, req)
		},
	}, time.Millisecond)
	if _, err := d.puller.SyncFolder(ctx, stopping, d.folder); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted sync returned %v, want a cancellation", err)
	}
	cancel()

	live := d.liveTransfer(t, "big.bin")
	if err := d.fs.Remove(live.TempPath); err != nil {
		t.Fatalf("remove partial file: %v", err)
	}

	d.restart()
	d.sync(t, conn)
	if got := chunkIndexes(conn); len(got) != 2 {
		t.Fatalf("second cycle asked for chunks %v, want both again", got)
	}
	if string(d.content(t, "big.bin")) != string(data) {
		t.Fatal("restarted file does not match the source")
	}
}

func TestSync_ChunkHashMismatchRetriesThenFails(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*2, 1))

	d, conn := newDest(t, corrupt{
		PeerHandler: src.server,
		damaged:     func(idx int) bool { return idx == 1 },
	})
	summary := d.sync(t, conn)

	if summary.Failed != 1 || summary.Pulled != 0 {
		t.Fatalf("summary = %+v, want one failed file", summary)
	}
	if _, ok := d.fs.Content(d.folder.Path + "/big.bin"); ok {
		t.Fatal("a file was installed from chunks that failed verification")
	}
	failed := d.failedTransfer(t)
	if !strings.Contains(failed.Error, "hash") {
		t.Fatalf("failure reason = %q, want the hash mismatch", failed.Error)
	}

	// The broken chunk was asked for three times before the transfer was
	// given up on.
	asked := 0
	for _, idx := range chunkIndexes(conn) {
		if idx == 1 {
			asked++
		}
	}
	if asked != 3 {
		t.Fatalf("chunk 1 was requested %d times, want 3", asked)
	}
}

func TestSync_WholeFileHashMismatchFailsWithoutInstalling(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize+1, 2)
	entry := src.addFile(t, "big.bin", data)

	d, conn := newDest(t, liar{
		PeerHandler: src.server,
		real:        entry.Hash,
		fake:        src.hasher.SumBytes([]byte("something else entirely")),
	})
	summary := d.sync(t, conn)

	if summary.Failed != 1 {
		t.Fatalf("summary = %+v, want one failed file", summary)
	}
	if _, ok := d.fs.Content(d.folder.Path + "/big.bin"); ok {
		t.Fatal("a file that failed whole-file verification was installed")
	}
	failed := d.failedTransfer(t)
	if !strings.Contains(failed.Error, "hash") {
		t.Fatalf("failure reason = %q, want the hash mismatch", failed.Error)
	}
	// The partial file is kept for inspection.
	if _, ok := d.fs.Content(failed.TempPath); !ok {
		t.Fatalf("partial file %s was removed, it must be kept for inspection", failed.TempPath)
	}
}

func TestSync_HashChangedOnSenderFailsCleanly(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*2, 4))

	d, conn := newDest(t, handlerFunc{
		manifest: src.server.Manifest,
		chunk: func(c context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			if req.Index == 0 {
				return src.server.Chunk(c, peer, req)
			}
			return ports.ChunkData{}, &ports.ChunkError{
				FolderID: req.FolderID, Path: req.Path, Index: req.Index,
				Reason: ports.ChunkHashChanged,
			}
		},
	})
	summary := d.sync(t, conn)

	if summary.Failed != 1 {
		t.Fatalf("summary = %+v, want one failed file", summary)
	}
	// The plan the transfer was made from is stale, not the bytes: nothing
	// is left to inspect, so the journal keeps no row and no partial file
	// and the next cycle decides the path again from scratch.
	if rows := d.transfers(t); len(rows) != 0 {
		t.Fatalf("journal holds %+v, want it empty", rows)
	}
	if temps := d.tempFiles(); len(temps) != 0 {
		t.Fatalf("partial files = %v, want none left behind", temps)
	}
	if d.events.Count("transfer.superseded") != 1 {
		t.Fatalf("superseded events = %d, want 1", d.events.Count("transfer.superseded"))
	}
	if d.events.Count("transfer.failed") != 0 {
		t.Fatal("a stale plan was reported as a failed transfer")
	}
	if _, ok := d.fs.Content(d.folder.Path + "/big.bin"); ok {
		t.Fatal("a file was installed although the sender changed it")
	}
}

func TestSync_LocalChangeIsNeverOverwritten(t *testing.T) {
	// Which version keeps the path depends on the content, so the local
	// change is followed through both outcomes: it is either still at the
	// path, or beside it under a conflict name. It is never gone.
	for _, tc := range []struct {
		name  string
		local []byte
	}{
		{"the local version keeps the path", []byte("local version A")},
		{"the local version is moved aside", []byte("local version B")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newSource(t, folder.ModeSendReceive)
			src.addFile(t, "a.txt", []byte("the peer's version"))

			d, conn := newDest(t, src.server)
			d.record(t, "a.txt", tc.local, vector(t, "DEST:1"))

			summary := d.sync(t, conn)
			if summary.Conflicts != 1 || summary.Pulled != 0 {
				t.Fatalf("summary = %+v, want one conflict and no plain pull", summary)
			}
			if !holdsContent(d, tc.local) {
				t.Fatalf("the local version is gone from %v", d.fs.Paths())
			}
			if d.events.Count("sync.conflict") != 1 {
				t.Fatal("no conflict event was published")
			}
		})
	}
}

// holdsContent reports whether any file of the destination folder holds data.
func holdsContent(d *dest, data []byte) bool {
	for _, p := range d.fs.Paths() {
		if got, ok := d.fs.Content(p); ok && string(got) == string(data) {
			return true
		}
	}
	return false
}

func TestSync_DeleteIsPropagatedOnlyWhenTheLocalCopyIsUnchanged(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	unchanged := []byte("both sides agreed on this")
	edited := []byte("the peer's old version")
	src.addFile(t, "gone.txt", unchanged)
	src.addFile(t, "edited.txt", edited)
	if err := src.store.MarkDeleted(ctx, src.folder.ID, []string{"gone.txt", "edited.txt"},
		"SOURCE", time.Now()); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	d, conn := newDest(t, src.server)
	d.record(t, "gone.txt", unchanged, vector(t, "SOURCE:1"))
	d.record(t, "edited.txt", []byte("edited locally"), vector(t, "SOURCE:1,DEST:1"))

	summary := d.sync(t, conn)
	if summary.Deleted != 1 || summary.Conflicts != 1 {
		t.Fatalf("summary = %+v, want one delete and one conflict", summary)
	}
	if _, ok := d.fs.Content(d.folder.Path + "/gone.txt"); ok {
		t.Fatal("gone.txt was not deleted")
	}
	if _, ok := d.fs.Content(d.folder.Path + "/edited.txt"); !ok {
		t.Fatal("a locally edited file was deleted because the peer deleted its version")
	}
	stored, err := d.store.GetFile(ctx, d.folder.ID, "gone.txt")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if !stored.Deleted || stored.ModifiedBy != peerID {
		t.Fatalf("stored = %+v, want a tombstone attributed to the peer", stored)
	}
}

func TestSync_SendOnlyFolderPullsNothing(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", []byte("hello"))

	d, conn := newDest(t, src.server)
	d.folder.Mode = folder.ModeSendOnly

	if summary := d.sync(t, conn); summary != (syncer.Summary{}) {
		t.Fatalf("summary = %+v, want nothing done", summary)
	}
	if len(conn.ManifestRequests()) != 0 {
		t.Fatal("a send-only folder asked the peer for its manifest")
	}
}

func TestSync_CreatesDirectoriesAndSymlinks(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.fs.AddDir(src.folder.Path+"/empty", 0o750)
	src.fs.AddSymlink(src.folder.Path+"/link", "empty")
	info, err := src.fs.Lstat(src.folder.Path + "/empty")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	entries := []manifest.Entry{
		{Path: "empty", Kind: manifest.KindDir, Mode: 0o750, ModTime: info.ModTime},
		{Path: "link", Kind: manifest.KindSymlink, LinkTarget: "empty", Mode: 0o777, ModTime: info.ModTime},
	}
	if err := src.store.UpsertEntries(context.Background(), src.folder.ID, entries); err != nil {
		t.Fatalf("record entries: %v", err)
	}

	d, conn := newDest(t, src.server)
	summary := d.sync(t, conn)
	if summary.Dirs != 1 || summary.Symlinks != 1 {
		t.Fatalf("summary = %+v, want one directory and one symlink", summary)
	}

	dirInfo, err := d.fs.Lstat(d.folder.Path + "/empty")
	if err != nil {
		t.Fatalf("lstat directory: %v", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode.Perm() != 0o750 {
		t.Fatalf("directory = %v, want a 0750 directory", dirInfo.Mode)
	}
	target, err := d.fs.ReadLink(d.folder.Path + "/link")
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "empty" {
		t.Fatalf("symlink points at %q, want %q", target, "empty")
	}
}

func TestSync_RefusesToInstallOverAFileThatChangedUnderneath(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", []byte("the peer's version"))

	d, conn := newDest(t, nil)
	conn.Handler = handlerFunc{
		manifest: src.server.Manifest,
		chunk: func(c context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			// The user writes the file while it is downloaded.
			d.fs.AddFile(d.folder.Path+"/a.txt", []byte("written by the user"), 0o644)
			return src.server.Chunk(c, peer, req)
		},
	}
	summary := d.sync(t, conn)

	if summary.Failed != 1 {
		t.Fatalf("summary = %+v, want the install refused", summary)
	}
	if got := d.content(t, "a.txt"); string(got) != "written by the user" {
		t.Fatalf("local file = %q, want the user's version", got)
	}
	// The local side moved, so the plan is stale and the row is worth
	// nothing: the next cycle decides the path again.
	if rows := d.transfers(t); len(rows) != 0 {
		t.Fatalf("journal holds %+v, want it empty", rows)
	}
	if temps := d.tempFiles(); len(temps) != 0 {
		t.Fatalf("partial files = %v, want none left behind", temps)
	}
	if d.events.Count("transfer.superseded") != 1 {
		t.Fatalf("superseded events = %d, want 1", d.events.Count("transfer.superseded"))
	}
}

// TestSync_NewTransferPrunesAnOlderFailedRow covers the journal that used to
// grow forever: a path that failed on its bytes keeps one row while it has not
// converged, and the cycle that tries it again is the answer to that row.
func TestSync_NewTransferPrunesAnOlderFailedRow(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*2, 17)
	src.addFile(t, "big.bin", data)

	d, conn := newDest(t, src.server)
	stale := transfer.NewInbound("abc", d.folder.ID, manifest.Entry{Path: "big.bin", Size: chunkSize * 2},
		peerID, chunkSize, d.folder.Path+"/.syncthis/tmp/abc.part", d.clock.Now())
	stale.State = transfer.StateFailed
	stale.Error = "chunk 1 did not hash right"
	if err := d.store.SaveTransfer(ctx, stale); err != nil {
		t.Fatalf("save transfer: %v", err)
	}
	d.fs.AddFile(stale.TempPath, []byte("half a download"), 0o600)

	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the file pulled", summary)
	}
	if rows := d.transfers(t); len(rows) != 0 {
		t.Fatalf("journal holds %+v, want the failed row gone with the finished one", rows)
	}
	if _, ok := d.fs.Content(stale.TempPath); ok {
		t.Fatalf("%s was kept, want the partial file of the pruned row removed", stale.TempPath)
	}
	if got := d.content(t, "big.bin"); string(got) != string(data) {
		t.Fatal("the pulled file does not match the source")
	}
}

func TestResumeAll_PutsActiveTransfersBackToPending(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*2, 8))

	d, _ := newDest(t, src.server)
	t0 := d.clock.Now()
	stale := transfer.NewInbound("abc", d.folder.ID, manifest.Entry{Path: "big.bin", Size: chunkSize * 2},
		peerID, chunkSize, d.folder.Path+"/.syncthis/tmp/abc.part", t0)
	stale.State = transfer.StateActive
	if err := d.store.SaveTransfer(ctx, stale); err != nil {
		t.Fatalf("save transfer: %v", err)
	}

	if err := d.puller.ResumeAll(ctx, d.folder); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	live := d.liveTransfer(t, "big.bin")
	if live.State != transfer.StatePending {
		t.Fatalf("state = %s, want pending", live.State)
	}
}

// TestResumeAll_LeavesFailedRowsAlone covers the one row kind that is not a
// transfer to continue: bytes that did not hash right are kept for inspection
// and only the plan may ask for that path again.
func TestResumeAll_LeavesFailedRowsAlone(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*2, 19))

	d, _ := newDest(t, src.server)
	failed := transfer.NewInbound("abc", d.folder.ID, manifest.Entry{Path: "big.bin", Size: chunkSize * 2},
		peerID, chunkSize, d.folder.Path+"/.syncthis/tmp/abc.part", d.clock.Now())
	failed.State = transfer.StateFailed
	failed.Error = "chunk 1 did not hash right"
	if err := d.store.SaveTransfer(ctx, failed); err != nil {
		t.Fatalf("save transfer: %v", err)
	}

	if err := d.puller.ResumeAll(ctx, d.folder); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	rows := d.transfers(t)
	if len(rows) != 1 || rows[0].State != transfer.StateFailed {
		t.Fatalf("journal holds %+v, want the failed row untouched", rows)
	}
}

// handlerFunc is a ports.PeerHandler assembled from functions.
type handlerFunc struct {
	manifest func(context.Context, device.ID, folder.ID) (manifest.Manifest, error)
	chunk    func(context.Context, device.ID, ports.ChunkRequest) (ports.ChunkData, error)
}

// Hello announces a single send-receive folder.
func (h handlerFunc) Hello(context.Context) ports.HelloInfo {
	return ports.HelloInfo{
		DeviceID: peerID,
		Folders:  []ports.FolderAdvert{{ID: "docs", Mode: folder.ModeSendReceive}},
	}
}

// Manifest delegates to the configured function.
func (h handlerFunc) Manifest(ctx context.Context, peer device.ID, id folder.ID) (manifest.Manifest, error) {
	return h.manifest(ctx, peer, id)
}

// Chunk delegates to the configured function.
func (h handlerFunc) Chunk(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	return h.chunk(ctx, peer, req)
}

func TestSync_MissingFolderRootPullsNothing(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", []byte("hello"))

	d, conn := newDest(t, src.server)
	if err := d.fs.Remove(d.folder.Path); err != nil {
		t.Fatalf("remove destination root: %v", err)
	}
	before := d.fs.Paths()

	if summary := d.sync(t, conn); summary != (syncer.Summary{}) {
		t.Fatalf("summary = %+v, want nothing done", summary)
	}
	if len(conn.ManifestRequests()) != 0 {
		t.Fatal("a folder whose root is gone asked the peer for its manifest")
	}
	if got := d.fs.Paths(); len(got) != len(before) {
		t.Fatalf("filesystem = %v, want it untouched (%v)", got, before)
	}
}

// addDir creates a directory on the source and records it the way a scan
// would.
func (s *source) addDir(t *testing.T, rel string) {
	t.Helper()
	full := s.folder.Path + "/" + rel
	s.fs.AddDir(full, 0o755)
	e := manifest.Entry{
		Path: rel, Kind: manifest.KindDir, Mode: 0o755,
		ModifiedBy: sourceID, Version: manifest.Version{}.Bump(sourceID),
	}
	if err := s.store.UpsertEntries(context.Background(), s.folder.ID, []manifest.Entry{e}); err != nil {
		t.Fatalf("record %s: %v", rel, err)
	}
}

// chmod changes the mode of a file on the source, on disk and in its manifest.
func (s *source) chmod(t *testing.T, rel string, mode fs.FileMode) {
	t.Helper()
	full := s.folder.Path + "/" + rel
	if err := s.fs.Chmod(full, mode); err != nil {
		t.Fatalf("chmod %s: %v", rel, err)
	}
	e, err := s.store.GetFile(context.Background(), s.folder.ID, rel)
	if err != nil {
		t.Fatalf("stored %s: %v", rel, err)
	}
	e.Mode = uint32(mode)
	if err := s.store.UpsertEntries(context.Background(), s.folder.ID, []manifest.Entry{e}); err != nil {
		t.Fatalf("record %s: %v", rel, err)
	}
}

func TestSync_FileTurnedIntoADirectoryIsReplaced(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addDir(t, "f")
	inner := src.addFile(t, "f/inner.txt", []byte("what is inside now"))

	d, conn := newDest(t, src.server)
	old := []byte("what used to be there")
	local := d.record(t, "f", old, vector(t, "SOURCE:1"))

	summary := d.sync(t, conn)
	if summary.Failed != 0 || summary.Conflicts != 0 {
		t.Fatalf("summary = %+v, want no failure and no conflict", summary)
	}
	if summary.Dirs != 1 || summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want one directory and one pulled file", summary)
	}

	info, err := d.fs.Lstat(d.folder.Path + "/f")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("f is %v, want a directory", info.Mode)
	}
	if got := d.content(t, "f/inner.txt"); string(got) != "what is inside now" {
		t.Fatalf("f/inner.txt = %q, want the peer's content", got)
	}

	stored, err := d.store.GetFile(context.Background(), d.folder.ID, "f")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if stored.Kind != manifest.KindDir || stored.Deleted {
		t.Fatalf("stored = %+v, want a live directory", stored)
	}
	if local.Kind != manifest.KindFile || inner.Hash.IsZero() {
		t.Fatalf("fixture = %+v/%+v, want a file replaced by a directory", local, inner)
	}
}

func TestSync_LocallyModifiedFileIsNotReplacedByADirectory(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addDir(t, "f")

	d, conn := newDest(t, src.server)
	mine := []byte("the version the user is editing")
	// The local file was changed here while the peer replaced it, so
	// neither version was made on top of the other.
	d.record(t, "f", mine, vector(t, "DEST:1"))

	summary := d.sync(t, conn)
	if summary.Conflicts != 1 || summary.Dirs != 0 {
		t.Fatalf("summary = %+v, want one conflict and no directory", summary)
	}
	if got := d.content(t, "f"); string(got) != string(mine) {
		t.Fatalf("f = %q, want the local version untouched", got)
	}
}

func TestSync_DirectoryTurnedIntoAFileIsReplaced(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	data := []byte("a file where the directory was")
	src.addFile(t, "sub", data)
	// The peer deleted the children before it created the file.
	src.addFile(t, "sub/inner.txt", []byte("gone upstream"))
	if err := src.store.MarkDeleted(ctx, src.folder.ID, []string{"sub/inner.txt"}, "SOURCE", time.Now()); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	d, conn := newDest(t, src.server)
	d.recordDir(t, "sub")
	d.record(t, "sub/inner.txt", []byte("gone upstream"), vector(t, "SOURCE:1"))

	summary := d.sync(t, conn)
	if summary.Failed != 0 {
		t.Fatalf("summary = %+v, want no failure", summary)
	}
	if summary.Deleted != 1 || summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the child deleted and the file pulled", summary)
	}
	if got := d.content(t, "sub"); string(got) != string(data) {
		t.Fatalf("sub = %q, want the peer's file", got)
	}
	info, err := d.fs.Lstat(d.folder.Path + "/sub")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.IsDir() {
		t.Fatal("sub is still a directory")
	}
}

func TestSync_DirectoryHoldingLocalOnlyFilesIsKept(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "sub", []byte("a file where the directory was"))

	d, conn := newDest(t, src.server)
	d.recordDir(t, "sub")
	mine := []byte("only this side has it")
	d.record(t, "sub/mine.txt", mine, vector(t, "DEST:1"))

	summary := d.sync(t, conn)
	if summary.Failed != 1 || summary.Pulled != 0 {
		t.Fatalf("summary = %+v, want the replacement to fail and nothing to be pulled", summary)
	}
	if got := d.content(t, "sub/mine.txt"); string(got) != string(mine) {
		t.Fatalf("sub/mine.txt = %q, want it untouched", got)
	}
	// Nothing was downloaded for a replacement that cannot happen.
	list, err := d.store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("transfers = %+v, want none", list)
	}
}

func TestSync_ModeChangeIsAppliedWithoutATransfer(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := []byte("identical on both sides")
	src.addFile(t, "a.txt", data)
	src.chmod(t, "a.txt", 0o700)

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", data, vector(t, "SOURCE:1"))

	summary := d.sync(t, conn)
	if summary.Modes != 1 || summary.Pulled != 0 || summary.Bytes != 0 {
		t.Fatalf("summary = %+v, want one mode change and no transfer", summary)
	}

	info, err := d.fs.Lstat(d.folder.Path + "/a.txt")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode.Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", info.Mode.Perm())
	}
	if got := d.content(t, "a.txt"); string(got) != string(data) {
		t.Fatalf("a.txt = %q, want the content untouched", got)
	}

	stored, err := d.store.GetFile(context.Background(), d.folder.ID, "a.txt")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if stored.Mode != 0o700 || stored.SyncedMode != 0o700 {
		t.Fatalf("stored = %+v, want mode 0700 recorded as agreed", stored)
	}

	// A second cycle has nothing left to do.
	if second := d.sync(t, conn); second.Modes != 0 {
		t.Fatalf("second cycle = %+v, want the mode change applied once", second)
	}
}

func TestSync_ModeChangeIsNotAppliedToALocallyModifiedFile(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	shared := []byte("what both sides had")
	src.addFile(t, "a.txt", shared)
	src.chmod(t, "a.txt", 0o700)

	d, conn := newDest(t, src.server)
	mine := []byte("the version the user is editing")
	d.record(t, "a.txt", mine, vector(t, "SOURCE:1,DEST:1"))

	summary := d.sync(t, conn)
	if summary.Modes != 0 || summary.Pulled != 0 {
		t.Fatalf("summary = %+v, want nothing applied", summary)
	}
	info, err := d.fs.Lstat(d.folder.Path + "/a.txt")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode.Perm() != 0o644 {
		t.Fatalf("mode = %v, want the local 0644 kept", info.Mode.Perm())
	}
	if got := d.content(t, "a.txt"); string(got) != string(mine) {
		t.Fatalf("a.txt = %q, want the local version untouched", got)
	}
}

func TestSync_OneItemThatCannotBeAppliedDoesNotStopTheCycle(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	src.addDir(t, "gone")
	src.addFile(t, "arrives.txt", []byte("this one still lands"))
	if err := src.store.MarkDeleted(ctx, src.folder.ID, []string{"gone"}, "SOURCE", time.Now()); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	d, conn := newDest(t, src.server)
	d.recordDir(t, "gone")
	// A file only this side knows about keeps the directory from going.
	mine := []byte("only this side has it")
	d.record(t, "gone/mine.txt", mine, vector(t, "DEST:1"))

	summary := d.sync(t, conn)
	if summary.Failed != 1 {
		t.Fatalf("summary = %+v, want exactly one failed item", summary)
	}
	if summary.Deleted != 0 {
		t.Fatalf("summary = %+v, want the directory kept", summary)
	}
	if summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the rest of the plan applied", summary)
	}
	if got := d.content(t, "arrives.txt"); string(got) != "this one still lands" {
		t.Fatalf("arrives.txt = %q, want it pulled after the failure", got)
	}
	if got := d.content(t, "gone/mine.txt"); string(got) != string(mine) {
		t.Fatalf("gone/mine.txt = %q, want it untouched", got)
	}
	if got := d.events.Count("sync.item_failed"); got != 1 {
		t.Fatalf("sync.item_failed events = %d, want 1", got)
	}
}

// vector parses a version vector written as "ID:N,ID:N".
func vector(t *testing.T, s string) manifest.Version {
	t.Helper()
	v, err := manifest.ParseVersion(s)
	if err != nil {
		t.Fatalf("parse version %q: %v", s, err)
	}
	return v
}

// setVersion gives a stored source entry an author and a version vector, the
// way the scan of the device that wrote the content would have.
func (s *source) setVersion(t *testing.T, rel string, by device.ID, v manifest.Version) {
	t.Helper()
	e, err := s.store.GetFile(context.Background(), s.folder.ID, rel)
	if err != nil {
		t.Fatalf("get %s: %v", rel, err)
	}
	e.ModifiedBy, e.Version = by, v
	if err := s.store.UpsertEntries(context.Background(), s.folder.ID, []manifest.Entry{e}); err != nil {
		t.Fatalf("record %s: %v", rel, err)
	}
}

// storedVersion returns the version vector of a destination entry.
func (d *dest) storedVersion(t *testing.T, rel string) manifest.Version {
	t.Helper()
	e, err := d.store.GetFile(context.Background(), d.folder.ID, rel)
	if err != nil {
		t.Fatalf("get %s: %v", rel, err)
	}
	return e.Version
}

func TestSync_PullAdoptsTheRemoteVersionAndAuthor(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", []byte("from a third device"))
	// The peer relays a version another device wrote.
	src.setVersion(t, "a.txt", "THIRD", vector(t, "THIRD:2"))

	d, conn := newDest(t, src.server)
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want one pulled file", summary)
	}

	stored, err := d.store.GetFile(context.Background(), d.folder.ID, "a.txt")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if !stored.Version.Equals(vector(t, "THIRD:2")) {
		t.Fatalf("version = %q, want the vector the content carries", stored.Version)
	}
	// The author is the device that wrote the content, not the peer it
	// came from, so the same version relayed twice is still one version.
	if stored.ModifiedBy != "THIRD" {
		t.Fatalf("author = %q, want the device the peer announced", stored.ModifiedBy)
	}
}

func TestSync_DeleteAdoptsTheTombstoneVersion(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	data := []byte("deleted upstream")
	src.addFile(t, "gone.txt", data)
	if err := src.store.MarkDeleted(ctx, src.folder.ID, []string{"gone.txt"}, peerID, time.Now()); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	tombstone, err := src.store.GetFile(ctx, src.folder.ID, "gone.txt")
	if err != nil {
		t.Fatalf("source tombstone: %v", err)
	}

	d, conn := newDest(t, src.server)
	d.record(t, "gone.txt", data, vector(t, "SOURCE:1"))

	if summary := d.sync(t, conn); summary.Deleted != 1 {
		t.Fatalf("summary = %+v, want one deletion", summary)
	}
	stored, err := d.store.GetFile(ctx, d.folder.ID, "gone.txt")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	// The deletion is the peer's version of the path, so this device holds
	// exactly that version rather than a deletion of its own.
	if !stored.Deleted || !stored.Version.Equals(tombstone.Version) {
		t.Fatalf("tombstone = %+v, want the peer's vector %q", stored, tombstone.Version)
	}
	if stored.ModifiedBy != peerID {
		t.Fatalf("author = %q, want the device that deleted the file", stored.ModifiedBy)
	}
}

func TestSync_InSyncAdoptsTheMergedVersion(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := []byte("identical on both sides")
	src.addFile(t, "a.txt", data)
	src.setVersion(t, "a.txt", peerID, vector(t, "SOURCE:1"))

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", data, vector(t, "DEST:1"))

	if summary := d.sync(t, conn); summary.Pulled != 0 {
		t.Fatalf("summary = %+v, want nothing pulled", summary)
	}
	// Both sides hold the same bytes under concurrent vectors, so the path
	// ends up with the merge and a third device is ordered against it.
	if got := d.storedVersion(t, "a.txt"); !got.Equals(vector(t, "DEST:1,SOURCE:1")) {
		t.Fatalf("version = %q, want the merge of both", got)
	}
}

// TestSync_StalePeerManifestChangesNothing is KF-3 seen from the device that
// stayed: a peer that was away announces a version this side has written past,
// and nothing on disk or in the manifest may move for it.
func TestSync_StalePeerManifestChangesNothing(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", []byte("the version the peer still has"))
	src.setVersion(t, "a.txt", sourceID, vector(t, "SOURCE:1"))

	d, conn := newDest(t, src.server)
	mine := []byte("the version written while the peer was away")
	d.record(t, "a.txt", mine, vector(t, "SOURCE:2"))

	summary := d.sync(t, conn)
	if summary.Pulled != 0 || summary.Conflicts != 0 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want an untouched folder", summary)
	}
	if got := d.content(t, "a.txt"); string(got) != string(mine) {
		t.Fatalf("a.txt = %q, want the newer local version", got)
	}
	if got := d.storedVersion(t, "a.txt"); !got.Equals(vector(t, "SOURCE:2")) {
		t.Fatalf("version = %q, want it unchanged", got)
	}
}
