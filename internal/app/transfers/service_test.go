package transfers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/transfers"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// chunkSize is the chunk size of the journal rows the tests write.
const chunkSize = transfer.MinChunkSize

// newService returns a service over a store holding the given transfers.
func newService(t *testing.T, list ...transfer.Transfer) *transfers.Service {
	t.Helper()
	store := fakes.NewStore()
	for _, item := range list {
		if err := store.SaveTransfer(context.Background(), item); err != nil {
			t.Fatalf("save transfer %s: %v", item.ID, err)
		}
	}
	return transfers.New(store)
}

// journalRow builds a transfer of n chunks with the first done of them
// verified.
func journalRow(id transfer.ID, path string, state transfer.State, n, done int) transfer.Transfer {
	at := time.Date(2026, time.September, 21, 10, 0, 0, 0, time.UTC)
	entry := manifest.Entry{Path: path, Kind: manifest.KindFile, Size: int64(n) * chunkSize, ModTime: at}
	t := transfer.NewInbound(id, "docs", entry, "PEER", chunkSize, "/tmp/"+string(id)+".part", at)
	t.State = state
	for i := range done {
		_ = t.MarkVerified(i, manifest.Hash{}, at)
	}
	return t
}

func TestList_FilterByState(t *testing.T) {
	svc := newService(t,
		journalRow("a1", "one.bin", transfer.StateActive, 4, 1),
		journalRow("b2", "two.bin", transfer.StatePaused, 4, 2),
		journalRow("c3", "three.bin", transfer.StateFailed, 4, 0),
	)

	all, err := svc.List(context.Background(), transfers.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("listed %d transfers, want 3", len(all))
	}

	paused, err := svc.List(context.Background(), transfers.Filter{State: transfer.StatePaused})
	if err != nil {
		t.Fatalf("list paused: %v", err)
	}
	if len(paused) != 1 || paused[0].ID != "b2" {
		t.Fatalf("paused = %+v, want only b2", paused)
	}
	got := paused[0]
	if got.ChunksTotal != 4 || got.ChunksDone != 2 {
		t.Fatalf("chunks = %d/%d, want 2/4", got.ChunksDone, got.ChunksTotal)
	}
	if got.BytesDone != 2*chunkSize || got.Size != 4*chunkSize {
		t.Fatalf("bytes = %d of %d, want %d of %d", got.BytesDone, got.Size, 2*chunkSize, 4*chunkSize)
	}
	if got.MissingChunks != nil {
		t.Fatalf("missing chunks = %v, want none in a listing", got.MissingChunks)
	}
}

func TestList_FilterByFolder(t *testing.T) {
	other := journalRow("d4", "other.bin", transfer.StateActive, 2, 0)
	other.FolderID = "photos"
	svc := newService(t, journalRow("a1", "one.bin", transfer.StateActive, 2, 0), other)

	list, err := svc.List(context.Background(), transfers.Filter{FolderID: "photos"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].FolderID != "photos" {
		t.Fatalf("list = %+v, want only the photos transfer", list)
	}
}

func TestShow_MissingChunks(t *testing.T) {
	svc := newService(t, journalRow("a1", "one.bin", transfer.StateActive, 4, 2))

	view, err := svc.Show(context.Background(), "a1")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if len(view.MissingChunks) != 2 || view.MissingChunks[0] != 2 || view.MissingChunks[1] != 3 {
		t.Fatalf("missing chunks = %v, want [2 3]", view.MissingChunks)
	}
	if view.Path != "one.bin" || view.PeerID != "PEER" || view.State != transfer.StateActive {
		t.Fatalf("view = %+v", view)
	}
}

func TestShow_CompleteTransferHasNoMissingChunks(t *testing.T) {
	svc := newService(t, journalRow("a1", "one.bin", transfer.StateVerifying, 2, 2))

	view, err := svc.Show(context.Background(), "a1")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if view.MissingChunks == nil || len(view.MissingChunks) != 0 {
		t.Fatalf("missing chunks = %v, want an empty list", view.MissingChunks)
	}
}

func TestShow_UnknownID(t *testing.T) {
	svc := newService(t)

	_, err := svc.Show(context.Background(), "nope")
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("show unknown = %v, want ErrNotFound", err)
	}
	// The store already names the transfer, so the service does not name
	// it again.
	if got, want := err.Error(), `transfer "nope": not found`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
