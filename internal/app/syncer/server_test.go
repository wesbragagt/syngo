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
	s.server = syncer.New(s.store, s.fs, s.hasher, fakes.NewIdentity("SOURCE", "nas"), nil)
	return s
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

// chunkOf asks the source for one chunk.
func (s *source) chunkOf(ctx context.Context, path string, hash manifest.Hash, idx int) (ports.ChunkData, error) {
	return s.server.Chunk(ctx, "PEER", ports.ChunkRequest{
		FolderID: s.folder.ID, Path: path, Hash: hash, Index: idx, ChunkSize: chunkSize,
	})
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

	hello := s.server.Hello(ctx)
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

func TestChunk_LastChunkShort(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	data := make([]byte, chunkSize+7)
	for i := range data {
		data[i] = byte(i)
	}
	e := s.addFile(t, "big.bin", data)

	first, err := s.chunkOf(ctx, "big.bin", e.Hash, 0)
	if err != nil {
		t.Fatalf("chunk 0: %v", err)
	}
	if len(first.Data) != chunkSize || first.Offset != 0 {
		t.Fatalf("chunk 0 = %d bytes at %d, want %d at 0", len(first.Data), first.Offset, chunkSize)
	}
	if first.ChunkHash != s.hasher.SumBytes(data[:chunkSize]) {
		t.Fatal("chunk 0 hash does not describe its bytes")
	}

	last, err := s.chunkOf(ctx, "big.bin", e.Hash, 1)
	if err != nil {
		t.Fatalf("chunk 1: %v", err)
	}
	if len(last.Data) != 7 || last.Offset != chunkSize {
		t.Fatalf("chunk 1 = %d bytes at %d, want 7 at %d", len(last.Data), last.Offset, chunkSize)
	}
	if string(last.Data) != string(data[chunkSize:]) {
		t.Fatal("chunk 1 does not hold the tail of the file")
	}

	_, err = s.chunkOf(ctx, "big.bin", e.Hash, 2)
	if refused := refusal(t, err); refused.Reason != ports.ChunkNotFound {
		t.Fatalf("reason = %s, want not_found for an index past the end", refused.Reason)
	}
}

func TestChunk_EmptyFileHasNoChunks(t *testing.T) {
	s := newSource(t, folder.ModeSendReceive)
	e := s.addFile(t, "empty.txt", nil)

	if got := transfer.ChunkCount(e.Size, chunkSize); got != 0 {
		t.Fatalf("chunk count = %d, want 0", got)
	}
	_, err := s.chunkOf(context.Background(), "empty.txt", e.Hash, 0)
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
		FolderID: "other", Path: "a.txt", Index: 0, ChunkSize: chunkSize,
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
