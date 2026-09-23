package sqlitestore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// indexedFile records one file and the chunks it was cut into, the way a scan
// does: the entry first, then the index, so the index never names content the
// folder does not have.
func indexedFile(t *testing.T, store interface {
	UpsertEntries(context.Context, folder.ID, []manifest.Entry) error
	SetFileChunks(context.Context, folder.ID, string, transfer.ChunkIndex) error
}, id folder.ID, path string, chunks []transfer.Chunk) {
	t.Helper()
	indexedFileAs(t, store, id, path, hashOf(9), chunks)
}

// indexedFileAs is indexedFile with the file hash spelled out, for a test that
// cares which content the index claims to describe.
func indexedFileAs(t *testing.T, store interface {
	UpsertEntries(context.Context, folder.ID, []manifest.Entry) error
	SetFileChunks(context.Context, folder.ID, string, transfer.ChunkIndex) error
}, id folder.ID, path string, hash manifest.Hash, chunks []transfer.Chunk) {
	t.Helper()
	ctx := context.Background()
	var size int64
	for _, c := range chunks {
		size += int64(c.Length)
	}
	entry := manifest.Entry{
		Path: path, Kind: manifest.KindFile, Size: size, Hash: hash, ModTime: testTime,
	}
	if err := store.UpsertEntries(ctx, id, []manifest.Entry{entry}); err != nil {
		t.Fatalf("record %s: %v", path, err)
	}
	if err := store.SetFileChunks(ctx, id, path, transfer.ChunkIndex{Hash: hash, Chunks: chunks}); err != nil {
		t.Fatalf("index %s: %v", path, err)
	}
}

// chunksOf builds a contiguous chunk list from lengths, hashing each chunk
// from its own seed so a test can plant the same content twice.
func chunksOf(lengths []int, seeds []byte) []transfer.Chunk {
	out := make([]transfer.Chunk, 0, len(lengths))
	var offset int64
	for i, length := range lengths {
		out = append(out, transfer.Chunk{
			Index: i, Offset: offset, Length: length, Hash: hashOf(seeds[i]),
		})
		offset += int64(length)
	}
	return out
}

func TestFileChunks_RoundTripsTheBoundariesAndHashes(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}

	// Content-defined lengths: nothing about them follows from a size.
	want := chunksOf([]int{4711, 1024, 9000}, []byte{1, 2, 3})
	indexedFile(t, store, "docs", "big.bin", want)

	got, err := store.FileChunks(ctx, "docs", "big.bin")
	if err != nil {
		t.Fatalf("FileChunks error: %v", err)
	}
	if got.Hash != hashOf(9) {
		t.Fatalf("index describes %s, want the file hash %s", got.Hash, hashOf(9))
	}
	if len(got.Chunks) != len(want) {
		t.Fatalf("%d chunks, want %d", len(got.Chunks), len(want))
	}
	for i := range want {
		if got.Chunks[i] != want[i] {
			t.Fatalf("chunk %d = %+v, want %+v", i, got.Chunks[i], want[i])
		}
	}
	if _, err := store.FileChunks(ctx, "docs", "absent.bin"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("FileChunks of an unindexed path = %v, want ErrNotFound", err)
	}
}

// TestFindChunk_ReachesAcrossFilesAndFolders is what deduplication rests on:
// the lookup is by hash alone, so it answers with content held under any name
// in any folder of this device.
func TestFindChunk_ReachesAcrossFilesAndFolders(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	for _, f := range []folder.Folder{
		testFolder("docs", "/home/u/docs"),
		testFolder("photos", "/home/u/photos"),
	} {
		if err := store.SaveFolder(ctx, f); err != nil {
			t.Fatalf("SaveFolder error: %v", err)
		}
	}

	indexedFile(t, store, "docs", "a.bin", chunksOf([]int{100, 200}, []byte{11, 12}))
	indexedFile(t, store, "photos", "far/away.bin", chunksOf([]int{300}, []byte{13}))

	// A chunk of another file in the same folder.
	place, err := store.FindChunk(ctx, hashOf(12))
	if err != nil {
		t.Fatalf("FindChunk across files: %v", err)
	}
	if place.FolderID != "docs" || place.Path != "a.bin" || place.Offset != 100 || place.Length != 200 {
		t.Fatalf("placement = %+v, want 200 bytes at 100 of docs/a.bin", place)
	}

	// A chunk of a file in a different folder entirely.
	place, err = store.FindChunk(ctx, hashOf(13))
	if err != nil {
		t.Fatalf("FindChunk across folders: %v", err)
	}
	if place.FolderID != "photos" || place.Path != "far/away.bin" || place.Length != 300 {
		t.Fatalf("placement = %+v, want 300 bytes of photos/far/away.bin", place)
	}

	if _, err := store.FindChunk(ctx, hashOf(99)); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("FindChunk of unheld content = %v, want ErrNotFound", err)
	}
}

// TestFileChunks_DeletingTheFileDropsItsIndex covers the rule that keeps the
// index honest: it must never offer bytes of a file the folder no longer has.
func TestFileChunks_DeletingTheFileDropsItsIndex(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	indexedFile(t, store, "docs", "gone.bin", chunksOf([]int{512}, []byte{21}))

	if err := store.SetFileChunks(ctx, "docs", "gone.bin", transfer.ChunkIndex{}); err != nil {
		t.Fatalf("clear the index: %v", err)
	}
	if _, err := store.FindChunk(ctx, hashOf(21)); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("FindChunk after the index was cleared = %v, want ErrNotFound", err)
	}
	indexed, err := store.IndexedPaths(ctx, "docs")
	if err != nil {
		t.Fatalf("IndexedPaths error: %v", err)
	}
	if len(indexed) != 0 {
		t.Fatalf("indexed paths = %v, want none", indexed)
	}
}

// TestSetFileChunks_ReplacesTheWholeList covers a file that was edited: the
// new cut replaces the old one rather than being merged into it, so no chunk
// of the previous version is left in the index to be offered.
func TestSetFileChunks_ReplacesTheWholeList(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	indexedFile(t, store, "docs", "edited.bin", chunksOf([]int{100, 200, 300}, []byte{31, 32, 33}))
	indexedFile(t, store, "docs", "edited.bin", chunksOf([]int{600}, []byte{34}))

	got, err := store.FileChunks(ctx, "docs", "edited.bin")
	if err != nil {
		t.Fatalf("FileChunks error: %v", err)
	}
	if len(got.Chunks) != 1 || got.Chunks[0].Hash != hashOf(34) {
		t.Fatalf("chunks = %+v, want only the new cut", got.Chunks)
	}
	if _, err := store.FindChunk(ctx, hashOf(32)); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("a chunk of the previous version is still offered: %v", err)
	}
}
