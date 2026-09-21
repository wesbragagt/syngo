package syncer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// interruptAfter returns a connection that serves n chunks and then cancels
// the sync, the way a process death cuts a transfer short.
func interruptAfter(src *source, n int, cancel context.CancelFunc) *fakes.PeerConn {
	served := 0
	return fakes.NewPeerConn(peerID, "nas", handlerFunc{
		manifest: src.server.Manifest,
		chunk: func(c context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			served++
			if served > n {
				cancel()
				return ports.ChunkData{}, c.Err()
			}
			return src.server.Chunk(c, peer, req)
		},
	}, time.Millisecond)
}

// TestCrash_ChunkWrittenBeforeItIsJournalledIsFetchedAgain covers the window
// between writing a chunk into the partial file and recording it as verified.
// A process that dies in it leaves bytes nothing vouches for, so the journal,
// not the file, decides what is still missing: the chunk is fetched and
// verified again.
func TestCrash_ChunkWrittenBeforeItIsJournalledIsFetchedAgain(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*4, 3)
	src.addFile(t, "big.bin", data)

	d, conn := newDest(t, src.server)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := d.puller.SyncFolder(ctx, interruptAfter(src, 2, cancel), d.folder); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted sync returned %v, want a cancellation", err)
	}
	cancel()

	live := d.liveTransfer(t, "big.bin")
	partial, ok := d.fs.Content(live.TempPath)
	if !ok {
		t.Fatalf("partial file %s is missing", live.TempPath)
	}
	// Chunk 2 reached the disk, but the process died before the journal
	// entry was written.
	chunk := live.Chunks[2]
	grown := make([]byte, chunk.Offset+int64(chunk.Length))
	copy(grown, partial)
	copy(grown[chunk.Offset:], data[chunk.Offset:chunk.Offset+int64(chunk.Length)])
	d.fs.AddFile(live.TempPath, grown, 0o600)

	d.restart()
	d.sync(t, conn)

	if got := chunkIndexes(conn); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("chunks asked for after the crash = %v, want [2 3]", got)
	}
	if string(d.content(t, "big.bin")) != string(data) {
		t.Fatal("the file installed after the crash does not match the source")
	}
	stored, err := d.store.GetFile(context.Background(), d.folder.ID, "big.bin")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if stored.Hash != d.hasher.SumBytes(data) || stored.SyncedHash != stored.Hash {
		t.Fatalf("stored = %+v, want the file recorded as in sync", stored)
	}
}

// TestInstall_FlushesThePartialFileAndItsDirectory checks the two flushes that
// make the rename survive a power loss: the content before it, the directory
// entry after it.
func TestInstall_FlushesThePartialFileAndItsDirectory(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "sub/big.bin", payload(chunkSize+7, 4))

	d, conn := newDest(t, src.server)
	d.sync(t, conn)

	want := d.folder.Path + "/sub"
	var found bool
	for _, dir := range d.fs.SyncedDirs() {
		if dir == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("flushed directories = %v, want %s among them", d.fs.SyncedDirs(), want)
	}
	if renames := d.fs.Renames(); len(renames) != 1 || renames[0][1] != want+"/big.bin" {
		t.Fatalf("renames = %v, want the partial file moved onto the target", renames)
	}
}

// TestCrash_InstalledFileWithoutItsRowIsReconciledByScan covers the window
// between the rename that installs a file and the row that records it. The
// next scan finds a file the database does not know about and records it as a
// local creation; the sync that follows must recognize it as the peer's
// version rather than send it back or pull it again.
func TestCrash_InstalledFileWithoutItsRowIsReconciledByScan(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*2, 11)
	entry := src.addFile(t, "big.bin", data)

	// The destination holds the installed file and no row for it, which is
	// what a crash between the rename and the upsert leaves behind.
	d, conn := newDest(t, src.server)
	d.fs.AddFile(d.folder.Path+"/big.bin", data, 0o644)

	scanner := scan.New(d.fs, d.hasher, d.store, d.clock, fakes.NewIdentity("DEST", "dest"), d.events, nil)
	result, err := scanner.Scan(ctx, d.folder, scan.Options{})
	if err != nil {
		t.Fatalf("scan after the crash: %v", err)
	}
	if len(result.Changes) != 1 || result.Changes[0].Type != manifest.Create {
		t.Fatalf("scan changes = %+v, want one create", result.Changes)
	}

	summary := d.sync(t, conn)

	if summary.Pulled != 0 || summary.Deleted != 0 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want nothing pulled, deleted or failed", summary)
	}
	if got := chunkIndexes(conn); len(got) != 0 {
		t.Fatalf("chunks asked for = %v, want none: the file is already there", got)
	}
	if removed := d.fs.Removes(); len(removed) != 0 {
		t.Fatalf("removed %v, want the installed file left alone", removed)
	}
	stored, err := d.store.GetFile(ctx, d.folder.ID, "big.bin")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if stored.Hash != entry.Hash || stored.SyncedHash != entry.Hash {
		t.Fatalf("stored = %+v, want %s recorded as agreed with the peer", stored, entry.Hash)
	}
}
