package syncer_test

import (
	"context"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/syncer"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// nightly is a window that is closed at the fake clock's noon and open at its
// midnight, so a test picks a side by setting the clock.
const nightly = "22:00-06:00"

// closeWindow gives the destination folder a window, in the store and in the
// copy the cycle is started with.
func closeWindow(t *testing.T, d *dest, spec string) {
	t.Helper()
	windows, err := folder.ParseWindows([]string{spec})
	if err != nil {
		t.Fatalf("parse window: %v", err)
	}
	d.folder.SyncWindows = windows
	if err := d.store.SaveFolder(context.Background(), d.folder); err != nil {
		t.Fatalf("save folder: %v", err)
	}
}

func TestSyncFolder_OutsideWindowRequestsNothing(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*2, 7))

	d, conn := newDest(t, src.server)
	// The fake clock reads noon UTC, which the nightly window excludes.
	d.clock.Set(time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC))
	closeWindow(t, d, nightly)

	summary, err := d.puller.SyncFolder(context.Background(), conn, d.folder)
	if err != nil {
		t.Fatalf("sync folder: %v", err)
	}
	if summary != (syncer.Summary{}) {
		t.Fatalf("summary = %+v, want an empty one", summary)
	}
	if got := conn.ManifestRequests(); len(got) != 0 {
		t.Fatalf("asked for %v, want nothing outside the window", got)
	}
}

func TestFetch_StopsWithinOneChunkWhenTheWindowCloses(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*5, 9)
	src.addFile(t, "big.bin", data)

	d, _ := newDest(t, src.server)
	// Midnight is inside the nightly window; the second chunk moves the
	// clock past its end.
	d.clock.Set(time.Date(2026, time.September, 20, 0, 0, 0, 0, time.UTC))
	closeWindow(t, d, nightly)

	served := 0
	conn := fakes.NewPeerConn(peerID, "nas", handlerFunc{
		manifest: src.server.Manifest,
		chunks:   src.server.Chunks,
		chunk: func(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			chunk, err := src.server.Chunk(ctx, peer, req)
			if err != nil {
				return chunk, err
			}
			served++
			if served == 2 {
				d.clock.Set(time.Date(2026, time.September, 20, 7, 0, 0, 0, time.UTC))
			}
			return chunk, nil
		},
	}, time.Millisecond)

	summary, err := d.puller.SyncFolder(context.Background(), conn, d.folder)
	if err != nil {
		t.Fatalf("sync folder: %v", err)
	}
	if summary.Pulled != 0 || summary.Failed != 0 || summary.Skipped != 1 {
		t.Fatalf("summary = %+v, want the file skipped and nothing pulled", summary)
	}
	// Two chunks were in flight before the clock moved, and the third is
	// the one that noticed: the window is checked before every request, so
	// the transfer stops within one chunk of it closing.
	if got := chunkIndexes(conn); len(got) != 2 {
		t.Fatalf("asked for chunks %v, want the two served before the window closed", got)
	}

	live := d.liveTransfer(t, "big.bin")
	if live.State != transfer.StatePending {
		t.Fatalf("transfer state = %q, want %q", live.State, transfer.StatePending)
	}
	if live.Error != syncer.ErrOutsideWindow.Error() {
		t.Fatalf("transfer error = %q, want %q", live.Error, syncer.ErrOutsideWindow)
	}
	if n := d.events.Count("transfer.failed"); n != 0 {
		t.Fatalf("%d transfer.failed events, want none: a closed window is not a failure", n)
	}
}

func TestSyncFolder_ContinuesFromTheJournalInsideTheWindow(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*4, 11)
	src.addFile(t, "big.bin", data)

	d, conn := newDest(t, src.server)
	d.clock.Set(time.Date(2026, time.September, 20, 0, 0, 0, 0, time.UTC))
	closeWindow(t, d, nightly)

	served := 0
	closing := fakes.NewPeerConn(peerID, "nas", handlerFunc{
		manifest: src.server.Manifest,
		chunks:   src.server.Chunks,
		chunk: func(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			chunk, err := src.server.Chunk(ctx, peer, req)
			if err != nil {
				return chunk, err
			}
			served++
			if served == 2 {
				d.clock.Set(time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC))
			}
			return chunk, nil
		},
	}, time.Millisecond)
	if _, err := d.puller.SyncFolder(context.Background(), closing, d.folder); err != nil {
		t.Fatalf("first cycle: %v", err)
	}

	// The window opens again, and the next cycle asks only for what the
	// journal is still missing.
	d.clock.Set(time.Date(2026, time.September, 20, 23, 0, 0, 0, time.UTC))
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the file finished", summary)
	}
	if got, want := chunkIndexes(conn), src.indexesFrom(t, "big.bin", 2); !sameIndexes(got, want) {
		t.Fatalf("the reopened cycle asked for chunks %v, want %v", got, want)
	}
	if string(d.content(t, "big.bin")) != string(data) {
		t.Fatal("the finished file does not match the source")
	}
}
