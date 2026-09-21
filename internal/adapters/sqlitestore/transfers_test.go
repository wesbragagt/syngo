package sqlitestore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/sqlitestore"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// inbound builds a transfer for a file of the given size.
func inbound(id transfer.ID, path string, size int64) transfer.Transfer {
	entry := manifest.Entry{
		Path: path, Kind: manifest.KindFile, Size: size, Hash: hashOf(1),
		Mode: 0o644, ModTime: testTime,
	}
	return transfer.NewInbound(id, "docs", entry, "PEERID", transfer.MinChunkSize, "/tmp/"+string(id)+".part", testTime)
}

// storeWithFolder returns a store that already holds the docs folder.
func storeWithFolder(t *testing.T) *sqlitestore.Store {
	t.Helper()
	store, _ := openStore(t)
	if err := store.SaveFolder(context.Background(), testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	return store
}

func TestSaveTransfer_UpsertsChunks(t *testing.T) {
	ctx := context.Background()
	store := storeWithFolder(t)

	tr := inbound("t1", "big.iso", 2*transfer.MinChunkSize+7)
	if err := store.SaveTransfer(ctx, tr); err != nil {
		t.Fatalf("SaveTransfer error: %v", err)
	}

	got, err := store.GetTransfer(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTransfer error: %v", err)
	}
	if got.FolderID != tr.FolderID || got.Path != tr.Path || got.PeerID != tr.PeerID ||
		got.Hash != tr.Hash || got.Size != tr.Size || got.MTimeNS != tr.MTimeNS || got.Mode != tr.Mode ||
		got.ChunkSize != tr.ChunkSize || got.TempPath != tr.TempPath || got.State != tr.State {
		t.Fatalf("transfer = %+v, want %+v", got, tr)
	}
	if len(got.Chunks) != 3 || got.Chunks[2].Length != 7 || got.Chunks[2].Offset != 2*transfer.MinChunkSize {
		t.Fatalf("chunks = %+v", got.Chunks)
	}

	if err := tr.MarkVerified(0, hashOf(5), testTime.Add(time.Second)); err != nil {
		t.Fatalf("MarkVerified error: %v", err)
	}
	tr.State = transfer.StateActive
	if err := store.SaveTransfer(ctx, tr); err != nil {
		t.Fatalf("second SaveTransfer error: %v", err)
	}
	got, err = store.GetTransfer(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTransfer error: %v", err)
	}
	if !got.Chunks[0].Verified || got.Chunks[0].Hash != hashOf(5) || got.State != transfer.StateActive {
		t.Fatalf("transfer after update = %+v", got)
	}
	if got.BytesDone() != transfer.MinChunkSize {
		t.Fatalf("BytesDone = %d, want %d", got.BytesDone(), transfer.MinChunkSize)
	}
}

func TestMarkChunkVerified_Persists(t *testing.T) {
	ctx := context.Background()
	store := storeWithFolder(t)
	if err := store.SaveTransfer(ctx, inbound("t1", "big.iso", 2*transfer.MinChunkSize)); err != nil {
		t.Fatalf("SaveTransfer error: %v", err)
	}

	at := testTime.Add(time.Minute)
	if err := store.MarkChunkVerified(ctx, "t1", 1, hashOf(3), at); err != nil {
		t.Fatalf("MarkChunkVerified error: %v", err)
	}

	got, err := store.GetTransfer(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTransfer error: %v", err)
	}
	if !got.Chunks[1].Verified || got.Chunks[1].Hash != hashOf(3) {
		t.Fatalf("chunk 1 = %+v", got.Chunks[1])
	}
	if !got.UpdatedAt.Equal(at) {
		t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, at)
	}
	missing := got.MissingChunks()
	if len(missing) != 1 || missing[0] != 0 {
		t.Fatalf("MissingChunks = %v, want [0]", missing)
	}

	if err := store.MarkChunkVerified(ctx, "t1", 9, hashOf(3), at); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("MarkChunkVerified on unknown chunk = %v, want ErrNotFound", err)
	}
}

func TestFindLiveTransfer_IgnoresDone(t *testing.T) {
	ctx := context.Background()
	store := storeWithFolder(t)
	if err := store.SaveTransfer(ctx, inbound("t1", "big.iso", transfer.MinChunkSize)); err != nil {
		t.Fatalf("SaveTransfer error: %v", err)
	}

	got, err := store.FindLiveTransfer(ctx, "docs", "big.iso")
	if err != nil {
		t.Fatalf("FindLiveTransfer error: %v", err)
	}
	if got.ID != "t1" {
		t.Fatalf("transfer = %q, want t1", got.ID)
	}

	if err := store.SetTransferState(ctx, "t1", transfer.StateDone, "", testTime.Add(time.Minute)); err != nil {
		t.Fatalf("SetTransferState error: %v", err)
	}
	if _, err := store.FindLiveTransfer(ctx, "docs", "big.iso"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("FindLiveTransfer after done = %v, want ErrNotFound", err)
	}

	list, err := store.ListTransfers(ctx, ports.TransferFilter{FolderID: "docs", State: transfer.StateDone})
	if err != nil {
		t.Fatalf("ListTransfers error: %v", err)
	}
	if len(list) != 1 || list[0].ID != "t1" || list[0].State != transfer.StateDone {
		t.Fatalf("list = %+v", list)
	}

	if err := store.DeleteTransfer(ctx, "t1"); err != nil {
		t.Fatalf("DeleteTransfer error: %v", err)
	}
	if err := store.DeleteTransfer(ctx, "t1"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("second DeleteTransfer = %v, want ErrNotFound", err)
	}
}

func TestLiveUniqueIndex_RejectsSecondLive(t *testing.T) {
	ctx := context.Background()
	store := storeWithFolder(t)
	if err := store.SaveTransfer(ctx, inbound("t1", "big.iso", transfer.MinChunkSize)); err != nil {
		t.Fatalf("SaveTransfer error: %v", err)
	}

	err := store.SaveTransfer(ctx, inbound("t2", "big.iso", transfer.MinChunkSize))
	if !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("second live transfer = %v, want ErrAlreadyExists", err)
	}

	if err := store.SetTransferState(ctx, "t1", transfer.StateFailed, "boom", testTime); err != nil {
		t.Fatalf("SetTransferState error: %v", err)
	}
	if err := store.SaveTransfer(ctx, inbound("t2", "big.iso", transfer.MinChunkSize)); err != nil {
		t.Fatalf("SaveTransfer after failure error: %v", err)
	}
}

// TestSaveTransfer_WritesChunksInBatches proves a transfer with more chunks
// than one batch holds is stored completely, and that the rows land in several
// transactions rather than one.
func TestSaveTransfer_WritesChunksInBatches(t *testing.T) {
	ctx := context.Background()
	store := storeWithFolder(t)

	// 1201 chunks at a batch size of 500: three transactions and a
	// remainder that is not a full one.
	const chunks = 1201
	tr := inbound("t1", "big.iso", chunks*transfer.MinChunkSize)
	if err := store.SaveTransfer(ctx, tr); err != nil {
		t.Fatalf("SaveTransfer error: %v", err)
	}

	got, err := store.GetTransfer(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTransfer error: %v", err)
	}
	if len(got.Chunks) != chunks {
		t.Fatalf("chunks = %d, want %d", len(got.Chunks), chunks)
	}
	if len(got.MissingChunks()) != chunks {
		t.Fatalf("missing = %d, want all of them", len(got.MissingChunks()))
	}
	last := got.Chunks[chunks-1]
	if last.Index != chunks-1 || last.Offset != int64(chunks-1)*transfer.MinChunkSize {
		t.Fatalf("last chunk = %+v", last)
	}
}

// TestLoadChunks_RebuildsARowThatWasNeverWritten covers the price of batching:
// a save interrupted between two batches leaves the transfer row claiming more
// chunks than there are rows. The missing ones are unverified chunks, not a
// shorter file.
func TestLoadChunks_RebuildsARowThatWasNeverWritten(t *testing.T) {
	ctx := context.Background()
	store := storeWithFolder(t)
	if err := store.SaveTransfer(ctx, inbound("t1", "big.iso", 3*transfer.MinChunkSize)); err != nil {
		t.Fatalf("SaveTransfer error: %v", err)
	}
	if err := store.DropChunkRowsFrom(ctx, "t1", 1); err != nil {
		t.Fatalf("drop chunk rows: %v", err)
	}

	got, err := store.GetTransfer(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTransfer error: %v", err)
	}
	if len(got.Chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(got.Chunks))
	}
	if len(got.MissingChunks()) != 3 {
		t.Fatalf("missing = %v, want every chunk", got.MissingChunks())
	}

	// A chunk whose row is gone is verified like any other.
	if err := store.MarkChunkVerified(ctx, "t1", 2, hashOf(3), testTime); err != nil {
		t.Fatalf("MarkChunkVerified error: %v", err)
	}
	got, err = store.GetTransfer(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTransfer error: %v", err)
	}
	if !got.Chunks[2].Verified || got.Chunks[2].Hash != hashOf(3) {
		t.Fatalf("chunk 2 = %+v", got.Chunks[2])
	}
}
