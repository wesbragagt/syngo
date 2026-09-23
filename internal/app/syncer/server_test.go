package syncer_test

import (
	"context"
	"errors"
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

// chunkSize is the chunk size the sender tests request with.
const chunkSize = transfer.MinChunkSize

// testFolder returns a folder configuration for the fake store.
func testFolder(id folder.ID, paused bool) folder.Folder {
	return folder.Folder{
		ID: id, Path: "/srv/" + string(id), Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkStore,
		KeepEmptyDirs: true, Paused: paused,
	}
}

// sourceID is the device the source fixtures write as.
const sourceID = device.ID("SOURCE")

// askerID is the device the tests that call the server directly ask as.
const askerID = device.ID("PEER")

// source is a device that serves a folder to its peers.
type source struct {
	store  *fakes.Store
	fs     *fakes.FileSystem
	hasher fakes.Hasher
	server *syncer.Server
	folder folder.Folder
}

// newSource returns a device serving folder id with the given mode.
func newSource(t *testing.T, mode folder.Mode) *source {
	t.Helper()
	f := testFolder("docs", false)
	f.Mode = mode

	s := &source{store: fakes.NewStore(), fs: fakes.NewFileSystem(), hasher: fakes.NewHasher(), folder: f}
	s.fs.AddDir(f.Path, 0o755)
	if err := s.store.SaveFolder(context.Background(), f); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	s.server = syncer.New(s.store, s.fs, s.hasher, fakes.NewIdentity("SOURCE", "nas"), chunkSize, nil)
	s.grant(t, f.ID)
	return s
}

// grant authorizes the devices these tests ask as for one folder of the
// source. A folder is served to nobody until it is granted, so every fixture
// that expects to be served has to say so.
func (s *source) grant(t *testing.T, id folder.ID) {
	t.Helper()
	ctx := context.Background()
	for _, dev := range []device.ID{askerID, fakes.SelfID} {
		if err := s.store.SaveDevice(ctx, device.Device{ID: dev, Name: "peer"}); err != nil {
			t.Fatalf("save device %s: %v", dev, err)
		}
		if err := s.store.GrantFolderDevice(ctx, id, dev); err != nil {
			t.Fatalf("grant %s on %s: %v", dev, id, err)
		}
	}
}

// addFile writes a file to the source and records it the way a scan would.
func (s *source) addFile(t *testing.T, rel string, data []byte) manifest.Entry {
	t.Helper()
	full := s.folder.Path + "/" + rel
	s.fs.AddFile(full, data, 0o644)
	info, err := s.fs.Lstat(full)
	if err != nil {
		t.Fatalf("lstat %s: %v", full, err)
	}
	prev, err := s.store.GetFile(context.Background(), s.folder.ID, rel)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("stored %s: %v", rel, err)
	}
	e := manifest.Entry{
		Path: rel, Kind: manifest.KindFile, Size: info.Size, ModTime: info.ModTime,
		Mode: 0o644, Hash: s.hasher.SumBytes(data), Inode: info.Inode,
		ModifiedBy: sourceID, Version: prev.Version.Bump(sourceID),
	}
	if err := s.store.UpsertEntries(context.Background(), s.folder.ID, []manifest.Entry{e}); err != nil {
		t.Fatalf("record %s: %v", rel, err)
	}
	return e
}

// chunkListOf asks the source how it cut a file.
func (s *source) chunkListOf(ctx context.Context, path string, hash manifest.Hash) ([]transfer.Chunk, error) {
	return s.server.Chunks(ctx, "PEER", ports.ChunkListRequest{
		FolderID: s.folder.ID, Path: path, Hash: hash,
	})
}

// chunkAt asks the source for one byte range, the way a receiver does once it
// holds the chunk list.
func (s *source) chunkAt(ctx context.Context, path string, hash manifest.Hash,
	idx int, offset int64, length int) (ports.ChunkData, error) {
	return s.server.Chunk(ctx, "PEER", ports.ChunkRequest{
		FolderID: s.folder.ID, Path: path, Hash: hash,
		Index: idx, Offset: offset, Length: length,
	})
}

// chunkOf asks the source for chunk idx of a file, taking the boundary from
// the chunk list the source serves. A file the source refuses to describe
// refuses here with the same error, which is what a receiver would see.
func (s *source) chunkOf(ctx context.Context, path string, hash manifest.Hash, idx int) (ports.ChunkData, error) {
	chunks, err := s.chunkListOf(ctx, path, hash)
	if err != nil {
		return ports.ChunkData{}, err
	}
	if idx >= len(chunks) {
		return ports.ChunkData{}, &ports.ChunkError{
			FolderID: s.folder.ID, Path: path, Index: idx, Reason: ports.ChunkNotFound,
		}
	}
	c := chunks[idx]
	return s.chunkAt(ctx, path, hash, idx, c.Offset, c.Length)
}

// chunkList returns how the source cut a file it holds. Chunking is
// content-defined, so how many chunks a fixture is depends on its bytes: a test
// about which chunks a cycle asks for reads the real list rather than assuming
// one chunk per configured chunk size.
func (s *source) chunkList(t *testing.T, rel string) []transfer.Chunk {
	t.Helper()
	e, err := s.store.GetFile(context.Background(), s.folder.ID, rel)
	if err != nil {
		t.Fatalf("stored entry of %s: %v", rel, err)
	}
	chunks, err := s.chunkListOf(context.Background(), rel, e.Hash)
	if err != nil {
		t.Fatalf("chunk list of %s: %v", rel, err)
	}
	return chunks
}

// indexesFrom returns the chunk indexes of a file from first to the end.
func (s *source) indexesFrom(t *testing.T, rel string, first int) []int {
	t.Helper()
	chunks := s.chunkList(t, rel)
	if first >= len(chunks) {
		t.Fatalf("%s is %d chunks, the test needs more than %d", rel, len(chunks), first)
	}
	out := make([]int, 0, len(chunks)-first)
	for i := first; i < len(chunks); i++ {
		out = append(out, i)
	}
	return out
}

// sameIndexes reports whether two chunk index lists are equal.
func sameIndexes(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// refusal returns the chunk error behind err.
func refusal(t *testing.T, err error) *ports.ChunkError {
	t.Helper()
	var refused *ports.ChunkError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want a chunk error", err)
	}
	return refused
}

func TestHello_AnnouncesIdentityAndRunningFolders(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	if err := s.store.SaveFolder(ctx, testFolder("photos", true)); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	s.grant(t, "photos")

	hello := s.server.Hello(ctx, askerID)
	if hello.DeviceID != "SOURCE" || hello.Name != "nas" {
		t.Fatalf("hello = %+v, want the local identity", hello)
	}
	if len(hello.Folders) != 1 || hello.Folders[0].ID != "docs" {
		t.Fatalf("folders = %+v, want only the running one", hello.Folders)
	}
}

func TestManifest_IncludesTombstones(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	s.addFile(t, "keep.txt", []byte("keep"))
	s.addFile(t, "gone.txt", []byte("gone"))
	if err := s.store.MarkDeleted(ctx, s.folder.ID, []string{"gone.txt"}, "SOURCE", time.Now()); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	m, err := s.server.Manifest(ctx, "PEER", s.folder.ID)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("manifest has %d entries, want 2", len(m.Entries))
	}
	if gone, ok := m.Entries["gone.txt"]; !ok || !gone.Deleted {
		t.Fatalf("gone.txt = %+v, want a tombstone", gone)
	}
}

func TestManifest_ReceiveOnlyReturnsError(t *testing.T) {
	s := newSource(t, folder.ModeReceiveOnly)

	_, err := s.server.Manifest(context.Background(), "PEER", s.folder.ID)
	if !errors.Is(err, ports.ErrFolderReceiveOnly) {
		t.Fatalf("error = %v, want ErrFolderReceiveOnly", err)
	}
}

func TestManifest_UnknownFolderAndPausedFolderAreRefused(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)

	if _, err := s.server.Manifest(ctx, "PEER", "other"); !errors.Is(err, ports.ErrFolderUnknown) {
		t.Fatalf("error = %v, want ErrFolderUnknown", err)
	}
	if err := s.store.SetFolderPaused(ctx, s.folder.ID, true); err != nil {
		t.Fatalf("pause folder: %v", err)
	}
	if _, err := s.server.Manifest(ctx, "PEER", s.folder.ID); !errors.Is(err, ports.ErrPaused) {
		t.Fatalf("error = %v, want ErrPaused", err)
	}
}

// TestChunks_ListPartitionsTheFileAndEveryChunkServesItsBytes is what replaced
// the fixed-size arithmetic: the receiver no longer derives a boundary from a
// size, it reads the list the sender serves, and every entry of that list names
// bytes the sender then hands over unchanged.
func TestChunks_ListPartitionsTheFileAndEveryChunkServesItsBytes(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	data := make([]byte, 8*chunkSize+7)
	for i := range data {
		data[i] = byte(i * 7)
	}
	e := s.addFile(t, "big.bin", data)

	chunks, err := s.chunkListOf(ctx, "big.bin", e.Hash)
	if err != nil {
		t.Fatalf("chunk list: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("%d chunks, want the file cut into several", len(chunks))
	}
	if err := transfer.ValidateChunks(e.Size, chunks); err != nil {
		t.Fatalf("the served list does not partition the file: %v", err)
	}

	for _, c := range chunks {
		got, err := s.chunkOf(ctx, "big.bin", e.Hash, c.Index)
		if err != nil {
			t.Fatalf("chunk %d: %v", c.Index, err)
		}
		want := data[c.Offset : c.Offset+int64(c.Length)]
		if got.Offset != c.Offset || string(got.Data) != string(want) {
			t.Fatalf("chunk %d = %d bytes at %d, want %d at %d",
				c.Index, len(got.Data), got.Offset, c.Length, c.Offset)
		}
		if got.ChunkHash != c.Hash || got.ChunkHash != s.hasher.SumBytes(want) {
			t.Fatalf("chunk %d hash does not describe its bytes", c.Index)
		}
	}

	// A range past the end is not content this device holds.
	_, err = s.chunkAt(ctx, "big.bin", e.Hash, len(chunks), e.Size, 16)
	if refused := refusal(t, err); refused.Reason != ports.ChunkHashChanged {
		t.Fatalf("reason = %s, want hash_changed for a range past the end", refused.Reason)
	}
}

func TestChunk_EmptyFileHasNoChunks(t *testing.T) {
	s := newSource(t, folder.ModeSendReceive)
	e := s.addFile(t, "empty.txt", nil)

	chunks, err := s.chunkListOf(context.Background(), "empty.txt", e.Hash)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("chunk list = %v, %v, want no chunks and no error", chunks, err)
	}
	_, err = s.chunkOf(context.Background(), "empty.txt", e.Hash, 0)
	if refused := refusal(t, err); refused.Reason != ports.ChunkNotFound {
		t.Fatalf("reason = %s, want not_found", refused.Reason)
	}
}

func TestChunk_HashChangedReturnsChunkError(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	e := s.addFile(t, "a.txt", []byte("version one"))

	// The peer asks for a version this device no longer has.
	stale := s.hasher.SumBytes([]byte("older version"))
	_, err := s.chunkOf(ctx, "a.txt", stale, 0)
	if refused := refusal(t, err); refused.Reason != ports.ChunkHashChanged {
		t.Fatalf("reason = %s, want hash_changed", refused.Reason)
	}

	// The file changed on disk since the last scan recorded it.
	s.fs.AddFile(s.folder.Path+"/a.txt", []byte("rewritten, longer"), 0o644)
	_, err = s.chunkOf(ctx, "a.txt", e.Hash, 0)
	if refused := refusal(t, err); refused.Reason != ports.ChunkHashChanged {
		t.Fatalf("reason = %s, want hash_changed", refused.Reason)
	}
}

func TestChunk_NotFoundAndPolicyRefusals(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)

	_, err := s.chunkOf(ctx, "missing.txt", manifest.Hash{}, 0)
	if refused := refusal(t, err); refused.Reason != ports.ChunkNotFound {
		t.Fatalf("reason = %s, want not_found", refused.Reason)
	}

	_, err = s.server.Chunk(ctx, "PEER", ports.ChunkRequest{
		FolderID: "other", Path: "a.txt", Index: 0, Offset: 0, Length: chunkSize,
	})
	if refused := refusal(t, err); refused.Reason != ports.ChunkFolderUnknown {
		t.Fatalf("reason = %s, want folder_unknown", refused.Reason)
	}

	receiveOnly := newSource(t, folder.ModeReceiveOnly)
	receiveOnly.addFile(t, "a.txt", []byte("hello"))
	_, err = receiveOnly.chunkOf(ctx, "a.txt", manifest.Hash{}, 0)
	if refused := refusal(t, err); refused.Reason != ports.ChunkFolderReceiveOnly {
		t.Fatalf("reason = %s, want folder_receive_only", refused.Reason)
	}

	if err := s.store.SetFolderPaused(ctx, s.folder.ID, true); err != nil {
		t.Fatalf("pause folder: %v", err)
	}
	_, err = s.chunkOf(ctx, "a.txt", manifest.Hash{}, 0)
	if refused := refusal(t, err); refused.Reason != ports.ChunkPaused {
		t.Fatalf("reason = %s, want paused", refused.Reason)
	}
}

// TestChunks_AnIndexCutFromOtherContentIsNotServed is the case geometry alone
// cannot see. The index of a path holds offsets, lengths and chunk hashes; a
// list left behind by an earlier version of the file partitions a file of that
// length exactly as well as a fresh one, so a check that only adds the lengths
// up serves a peer the hashes of bytes this device no longer holds. The peer's
// own per-chunk check then rejects every chunk it asked for and the pull fails.
//
// The index records the whole-file hash it was cut from, so the server can tell
// the two apart and reads the file instead.
func TestChunks_AnIndexCutFromOtherContentIsNotServed(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)

	// The version the index is written for.
	before := payload(chunkSize*8, 61)
	old := s.addFile(t, "same-length.bin", before)
	stale, err := s.chunkListOf(ctx, "same-length.bin", old.Hash)
	if err != nil {
		t.Fatalf("chunk list of the first version: %v", err)
	}
	if err := s.store.SetFileChunks(ctx, s.folder.ID, "same-length.bin",
		transfer.ChunkIndex{Hash: old.Hash, Chunks: stale}); err != nil {
		t.Fatalf("index the first version: %v", err)
	}

	// The file is replaced by different content of exactly the same length
	// and the entry is rewritten, but nothing rewrites the index: a crash
	// between the two writes, or a writer that installs bytes it did not
	// cut itself.
	after := payload(chunkSize*8, 62)
	if len(after) != len(before) {
		t.Fatalf("the two versions are %d and %d bytes, the case needs one length", len(before), len(after))
	}
	now := s.addFile(t, "same-length.bin", after)
	if now.Hash == old.Hash {
		t.Fatal("the two versions hash the same, the case needs two contents")
	}

	got, err := s.chunkListOf(ctx, "same-length.bin", now.Hash)
	if err != nil {
		t.Fatalf("chunk list of the second version: %v", err)
	}
	if err := transfer.ValidateChunks(now.Size, got); err != nil {
		t.Fatalf("the served list does not partition the file: %v", err)
	}
	for _, c := range got {
		if want := s.hasher.SumBytes(after[c.Offset : c.Offset+int64(c.Length)]); c.Hash != want {
			t.Fatalf("chunk %d is served as %s, the file holds %s: the stale index was served",
				c.Index, c.Hash, want)
		}
	}
}
