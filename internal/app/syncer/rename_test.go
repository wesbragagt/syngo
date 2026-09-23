package syncer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// renameOnSource moves a file of the source device, which is what its own
// scanner records: the new path holds the content and the old one is a
// tombstone.
func renameOnSource(t *testing.T, s *source, from, to string, data []byte) {
	t.Helper()
	s.addFile(t, to, data)
	if err := s.fs.Remove(s.folder.Path + "/" + from); err != nil {
		t.Fatalf("remove %s: %v", from, err)
	}
	err := s.store.MarkDeleted(context.Background(), s.folder.ID, []string{from}, sourceID, time.Now())
	if err != nil {
		t.Fatalf("mark %s deleted: %v", from, err)
	}
}

// TestSync_RenamedFileIsTakenFromTheLocalCopy is the saving this milestone is
// about: the peer moved a file, so the content is already here under the old
// name and not one chunk is asked for.
func TestSync_RenamedFileIsTakenFromTheLocalCopy(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*5+7, 23)
	src.addFile(t, "old.bin", data)

	d, conn := newDest(t, src.server)
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("first sync = %+v, want one pulled file", summary)
	}
	first := len(conn.ChunkRequests())
	if first == 0 {
		t.Fatal("the first sync asked for no chunk, so the second proves nothing")
	}

	renameOnSource(t, src, "old.bin", "new.bin", data)
	summary := d.sync(t, conn)

	if got := string(d.content(t, "new.bin")); got != string(data) {
		t.Fatalf("new.bin holds %d bytes, want %d", len(got), len(data))
	}
	if _, ok := d.fs.Content(d.folder.Path + "/old.bin"); ok {
		t.Fatal("old.bin is still there, want it deleted")
	}
	if got := len(conn.ChunkRequests()); got != first {
		t.Fatalf("the rename cost %d chunk requests, want 0", got-first)
	}
	if summary.Bytes != 0 {
		t.Fatalf("the rename transferred %d bytes, want 0", summary.Bytes)
	}
	if summary.Pulled != 1 || summary.Deleted != 1 {
		t.Fatalf("summary = %+v, want one file in place and one deleted", summary)
	}
	if d.events.Count("transfer.local_copy") != 1 {
		t.Fatalf("%d local copy events, want 1", d.events.Count("transfer.local_copy"))
	}
	// Nothing is left in the journal or in the temp directory.
	if rows := d.transfers(t); len(rows) != 0 {
		t.Fatalf("%d transfers left behind, want none", len(rows))
	}
	if files := d.tempFiles(); len(files) != 0 {
		t.Fatalf("partial files left behind: %v", files)
	}
}

// TestSync_CopyOfAFileKeepsTheOriginal covers the other shape of the same
// saving: the peer copied a file rather than moving it, so both paths end up
// here and only one of them was ever downloaded.
func TestSync_CopyOfAFileKeepsTheOriginal(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*3, 31)
	src.addFile(t, "a.bin", data)

	d, conn := newDest(t, src.server)
	d.sync(t, conn)
	first := len(conn.ChunkRequests())

	src.addFile(t, "b.bin", data)
	d.sync(t, conn)

	if got := string(d.content(t, "b.bin")); got != string(data) {
		t.Fatalf("b.bin holds %d bytes, want %d", len(got), len(data))
	}
	if got := string(d.content(t, "a.bin")); got != string(data) {
		t.Fatal("a.bin lost its content, want it untouched")
	}
	if got := len(conn.ChunkRequests()); got != first {
		t.Fatalf("the copy cost %d chunk requests, want 0", got-first)
	}
}

// TestSync_LocalSourceThatDoesNotHashRightIsDownloaded is the safety net: the
// manifest says a path holds the content, the disk says otherwise, and the
// file is fetched from the peer instead of being built out of wrong bytes.
func TestSync_LocalSourceThatDoesNotHashRightIsDownloaded(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*2, 41)
	src.addFile(t, "old.bin", data)

	d, conn := newDest(t, src.server)
	d.sync(t, conn)
	first := len(conn.ChunkRequests())

	// The local file is replaced behind the manifest's back, so the stored
	// hash no longer describes what is on disk.
	d.fs.AddFile(d.folder.Path+"/old.bin", payload(chunkSize*2, 42), 0o644)

	renameOnSource(t, src, "old.bin", "new.bin", data)
	d.sync(t, conn)

	if got := string(d.content(t, "new.bin")); got != string(data) {
		t.Fatalf("new.bin holds the wrong content")
	}
	if got := len(conn.ChunkRequests()); got <= first {
		t.Fatalf("the file arrived without a single chunk, want it downloaded")
	}
	if d.events.Count("transfer.local_copy") != 0 {
		t.Fatal("a local copy was installed from content that does not match")
	}
	if files := d.tempFiles(); len(files) != 0 {
		t.Fatalf("partial files left behind: %v", files)
	}
}

// TestSync_LocalCopyDropsTheIndexOfTheVersionItReplaced covers the one writer
// that puts content at a path without knowing how that content is cut: the
// bytes come from another path of the folder, so there is no chunk list to
// record. The index the path already had describes the version just overwritten
// and has to go, otherwise it would be offered to a peer and, whenever the two
// versions are the same length, would pass every check the offsets allow.
func TestSync_LocalCopyDropsTheIndexOfTheVersionItReplaced(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	kept := payload(chunkSize*4, 51)
	replaced := payload(chunkSize*4, 52)
	src.addFile(t, "a.bin", kept)
	src.addFile(t, "b.bin", replaced)

	d, conn := newDest(t, src.server)
	if summary := d.sync(t, conn); summary.Pulled != 2 {
		t.Fatalf("first sync = %+v, want both files pulled", summary)
	}
	stale, err := d.store.FileChunks(ctx, d.folder.ID, "b.bin")
	if err != nil {
		t.Fatalf("the download did not index b.bin: %v", err)
	}

	// b.bin now holds a.bin's content, which this device already has: the
	// pull is satisfied from the local copy and never sees a chunk list.
	src.addFile(t, "b.bin", kept)
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("second sync = %+v, want b.bin pulled", summary)
	}
	if d.events.Count("transfer.local_copy") != 1 {
		t.Fatalf("%d local copy events, want the pull to have been served from disk",
			d.events.Count("transfer.local_copy"))
	}
	if string(d.content(t, "b.bin")) != string(kept) {
		t.Fatal("b.bin does not hold the content the peer announced")
	}

	entry, err := d.store.GetFile(ctx, d.folder.ID, "b.bin")
	if err != nil {
		t.Fatalf("entry of b.bin: %v", err)
	}
	switch ix, err := d.store.FileChunks(ctx, d.folder.ID, "b.bin"); {
	case errors.Is(err, ports.ErrNotFound):
	case err != nil:
		t.Fatalf("chunks of b.bin: %v", err)
	default:
		if derr := ix.Describes(entry.Hash, entry.Size); derr != nil {
			t.Fatalf("b.bin kept an index of the version it replaced: %v", derr)
		}
	}
	if stale.Hash == entry.Hash {
		t.Fatal("the two versions hash the same, the case needs two contents")
	}
}
