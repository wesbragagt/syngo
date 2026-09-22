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
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// pauseAfter returns a connection that pauses the destination folder as soon
// as it has served n chunks, the way `syncthis pause` does while a transfer
// runs.
func pauseAfter(t *testing.T, src *source, d *dest, n int) *fakes.PeerConn {
	t.Helper()
	served := 0
	return fakes.NewPeerConn(peerID, "nas", handlerFunc{
		manifest: src.server.Manifest,
		chunks:   src.server.Chunks,
		chunk: func(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			data, err := src.server.Chunk(ctx, peer, req)
			if err != nil {
				return data, err
			}
			served++
			if served == n {
				if err := d.store.SetFolderPaused(ctx, d.folder.ID, true); err != nil {
					t.Errorf("pause folder: %v", err)
				}
			}
			return data, nil
		},
	}, time.Millisecond)
}

func TestSync_PauseStopsWithinOneChunk(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*4, 5)
	src.addFile(t, "big.bin", data)

	d, _ := newDest(t, src.server)
	conn := pauseAfter(t, src, d, 1)

	summary, err := d.puller.SyncFolder(context.Background(), conn, d.folder)
	if err != nil {
		t.Fatalf("sync folder: %v", err)
	}
	if summary.Pulled != 0 || summary.Failed != 0 || summary.Skipped != 1 {
		t.Fatalf("summary = %+v, want the paused file skipped and nothing pulled", summary)
	}

	// The chunk that was already in flight is finished; nothing after it
	// is asked for.
	if got := chunkIndexes(conn); len(got) != 1 || got[0] != 0 {
		t.Fatalf("asked for chunks %v, want only chunk 0", got)
	}

	live := d.liveTransfer(t, "big.bin")
	if live.State != transfer.StatePaused {
		t.Fatalf("transfer state = %q, want %q", live.State, transfer.StatePaused)
	}
	if done := len(live.Chunks) - len(live.MissingChunks()); done != 1 {
		t.Fatalf("%d chunks verified, want the one that was in flight", done)
	}
	if live.Error != "" {
		t.Fatalf("transfer error = %q, want none: a pause is not a failure", live.Error)
	}
	if _, ok := d.fs.Content(live.TempPath); !ok {
		t.Fatalf("partial file %s was removed by the pause", live.TempPath)
	}
	if _, ok := d.fs.Content(d.folder.Path + "/big.bin"); ok {
		t.Fatal("a paused transfer installed the file")
	}
}

func TestSync_ResumeAfterPauseRefetchesOnlyMissingChunks(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*4, 5)
	entry := src.addFile(t, "big.bin", data)

	d, conn := newDest(t, src.server)
	paused := pauseAfter(t, src, d, 2)
	if _, err := d.puller.SyncFolder(context.Background(), paused, d.folder); err != nil {
		t.Fatalf("sync folder: %v", err)
	}
	if got := chunkIndexes(paused); len(got) != 2 {
		t.Fatalf("asked for chunks %v before the pause, want two", got)
	}

	// Resuming continues from the journal instead of starting over.
	if err := d.store.SetFolderPaused(context.Background(), d.folder.ID, false); err != nil {
		t.Fatalf("resume folder: %v", err)
	}
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the file finished", summary)
	}
	if got := chunkIndexes(conn); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("the resumed cycle asked for chunks %v, want [2 3]", got)
	}
	if string(d.content(t, "big.bin")) != string(data) {
		t.Fatal("the resumed file does not match the source")
	}
	if entry.Hash != d.hasher.SumBytes(d.content(t, "big.bin")) {
		t.Fatal("the resumed file does not hash to the announced hash")
	}

	list, err := d.store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("%d transfers left in the journal, want none", len(list))
	}
}

func TestSync_PausedFolderStartsNoTransfer(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*2, 11))

	d, conn := newDest(t, src.server)
	if err := d.store.SetFolderPaused(context.Background(), d.folder.ID, true); err != nil {
		t.Fatalf("pause folder: %v", err)
	}

	// The folder was paused after this cycle read its configuration, so
	// the pause is only noticed once the pull starts.
	summary, err := d.puller.SyncFolder(context.Background(), conn, d.folder)
	if err != nil {
		t.Fatalf("sync folder: %v", err)
	}
	if summary.Pulled != 0 {
		t.Fatalf("summary = %+v, want nothing pulled", summary)
	}
	if reqs := conn.ChunkRequests(); len(reqs) != 0 {
		t.Fatalf("asked for %d chunks of a paused folder, want none", len(reqs))
	}

	list, err := d.store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a paused folder wrote %d journal rows, want none", len(list))
	}
	if _, err := d.store.FindLiveTransfer(context.Background(), d.folder.ID, "big.bin"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("live transfer = %v, want none", err)
	}
}

// pausedTransfer leaves one paused transfer of big.bin in the journal, the way
// `syncthis pause` does while a file is being pulled.
func pausedTransfer(t *testing.T) *dest {
	t.Helper()
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*4, 7))

	d, _ := newDest(t, src.server)
	if _, err := d.puller.SyncFolder(context.Background(), pauseAfter(t, src, d, 1), d.folder); err != nil {
		t.Fatalf("sync folder: %v", err)
	}
	if live := d.liveTransfer(t, "big.bin"); live.State != transfer.StatePaused {
		t.Fatalf("transfer state = %q, want %q before the resume", live.State, transfer.StatePaused)
	}
	return d
}

// setFolder writes a changed folder configuration, as `folder set` does.
func setFolder(t *testing.T, d *dest, mode folder.Mode, paused bool) folder.Folder {
	t.Helper()
	f := d.folder
	f.Mode = mode
	f.Paused = paused
	if err := d.store.SaveFolder(context.Background(), f); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	return f
}

func TestResumeAll_SendOnlyFolderIsPendingWithAReason(t *testing.T) {
	d := pausedTransfer(t)
	f := setFolder(t, d, folder.ModeSendOnly, false)

	if err := d.puller.ResumeAll(context.Background(), f); err != nil {
		t.Fatalf("resume all: %v", err)
	}

	// The folder is not paused, so the transfer must not say it is; it
	// waits, and the journal says what for.
	live := d.liveTransfer(t, "big.bin")
	if live.State != transfer.StatePending {
		t.Fatalf("transfer state = %q, want %q", live.State, transfer.StatePending)
	}
	if live.Error != syncer.ReasonSendOnly {
		t.Fatalf("transfer error = %q, want %q", live.Error, syncer.ReasonSendOnly)
	}
}

func TestResumeAll_ReceivingFolderIsPendingWithoutAReason(t *testing.T) {
	d := pausedTransfer(t)
	f := setFolder(t, d, folder.ModeSendReceive, false)

	if err := d.puller.ResumeAll(context.Background(), f); err != nil {
		t.Fatalf("resume all: %v", err)
	}

	live := d.liveTransfer(t, "big.bin")
	if live.State != transfer.StatePending {
		t.Fatalf("transfer state = %q, want %q", live.State, transfer.StatePending)
	}
	if live.Error != "" {
		t.Fatalf("transfer error = %q, want none: the transfer only waits", live.Error)
	}
}

func TestResumeAll_PausedFolderKeepsThePausedLabel(t *testing.T) {
	d := pausedTransfer(t)
	f := setFolder(t, d, folder.ModeSendReceive, true)

	if err := d.puller.ResumeAll(context.Background(), f); err != nil {
		t.Fatalf("resume all: %v", err)
	}

	if live := d.liveTransfer(t, "big.bin"); live.State != transfer.StatePaused {
		t.Fatalf("transfer state = %q, want %q while the folder is paused", live.State, transfer.StatePaused)
	}
}
