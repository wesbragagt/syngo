package syncer_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// TestDedup_ContentAlreadyHeldIsNeverAskedFor covers the deduplication itself:
// the receiver holds the file's content under another name, so the chunk list
// the sender serves matches what its index already has and no chunk is
// requested at all.
func TestDedup_ContentAlreadyHeldIsNeverAskedFor(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*8, 41)
	src.addFile(t, "copy.bin", data)

	// The receiver already holds the same bytes under another name, cut
	// the way the sender cut them.
	d, conn := newDest(t, src.server)
	d.record(t, "original.bin", data, manifest.Version{})
	if err := d.store.SetFileChunks(ctx, d.folder.ID, "original.bin",
		src.chunkList(t, "copy.bin")); err != nil {
		t.Fatalf("index the local copy: %v", err)
	}

	if summary := d.sync(t, conn); summary.Pulled != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want one pulled file", summary)
	}
	if got := chunkIndexes(conn); len(got) != 0 {
		t.Fatalf("asked the peer for chunks %v, want none: the content was already here", got)
	}
	if string(d.content(t, "copy.bin")) != string(data) {
		t.Fatal("the deduplicated file does not match the source")
	}
}

// TestDedup_StaleIndexFallsBackToThePeer covers the case the index cannot
// prevent: it is only as fresh as the last scan, so a row may point at bytes
// that have changed since. The receiver re-reads and re-hashes before using
// them, finds they are not what the sender announced, and pulls the chunk
// instead of installing the wrong content.
func TestDedup_StaleIndexFallsBackToThePeer(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*8, 42)
	src.addFile(t, "wanted.bin", data)

	d, conn := newDest(t, src.server)
	// The index claims a local file holds the sender's chunks, but the
	// file on disk holds something else entirely: an edit the last scan
	// did not see.
	d.record(t, "stale.bin", payload(chunkSize*8, 43), manifest.Version{})
	if err := d.store.SetFileChunks(ctx, d.folder.ID, "stale.bin",
		src.chunkList(t, "wanted.bin")); err != nil {
		t.Fatalf("plant the stale index: %v", err)
	}

	if summary := d.sync(t, conn); summary.Pulled != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want one pulled file", summary)
	}
	want := src.chunkList(t, "wanted.bin")
	if got := chunkIndexes(conn); len(got) != len(want) {
		t.Fatalf("asked the peer for %d chunks, want all %d: the local bytes were not the announced ones",
			len(got), len(want))
	}
	if string(d.content(t, "wanted.bin")) != string(data) {
		t.Fatal("the pulled file does not match the source")
	}
}

// TestDedup_InstalledContentIsIndexedAtOnce covers why a second copy costs
// nothing without a scan in between: a finished download is indexed from the
// chunk list it was verified against, so the next file that shares its content
// finds it.
func TestDedup_InstalledContentIsIndexedAtOnce(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*8, 44)
	src.addFile(t, "first.bin", data)

	d, conn := newDest(t, src.server)
	d.sync(t, conn)

	indexed, err := d.store.FileChunks(ctx, d.folder.ID, "first.bin")
	if err != nil {
		t.Fatalf("chunk index of the installed file: %v", err)
	}
	want := src.chunkList(t, "first.bin")
	if len(indexed) != len(want) {
		t.Fatalf("%d chunks indexed, want %d", len(indexed), len(want))
	}
	for i := range want {
		if indexed[i].Offset != want[i].Offset || indexed[i].Length != want[i].Length ||
			indexed[i].Hash != want[i].Hash {
			t.Fatalf("indexed chunk %d = %+v, want %+v", i, indexed[i], want[i])
		}
	}

	// The same content under a second name now costs no chunk request.
	src.addFile(t, "second.bin", data)
	before := len(chunkIndexes(conn))
	d.sync(t, conn)
	if got := len(chunkIndexes(conn)); got != before {
		t.Fatalf("the second copy asked for %d chunks, want none", got-before)
	}
	if string(d.content(t, "second.bin")) != string(data) {
		t.Fatal("the second copy does not match the source")
	}
}

// TestDedup_ChunkListIsRefusedWhenTheFileMovedOn covers the guard on the new
// request: the list describes one version of a file, so a sender whose file
// has changed refuses to describe it rather than answering for two versions.
func TestDedup_ChunkListIsRefusedWhenTheFileMovedOn(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "moving.bin", payload(chunkSize*2, 45))

	stale := src.hasher.SumBytes([]byte("a version this device no longer has"))
	_, err := src.chunkListOf(context.Background(), "moving.bin", stale)
	if refused := refusal(t, err); refused.Reason != ports.ChunkHashChanged {
		t.Fatalf("reason = %s, want hash_changed", refused.Reason)
	}
}

// TestDedup_InvalidChunkListIsRefused covers what a peer supplies: a list that
// does not partition the file it came with is not something to download from,
// and only that file is given up on.
func TestDedup_InvalidChunkListIsRefused(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*4, 46)
	src.addFile(t, "big.bin", data)

	d, conn := newDest(t, handlerFunc{
		manifest: src.server.Manifest,
		chunk:    src.server.Chunk,
		chunks: func(c context.Context, peer device.ID, req ports.ChunkListRequest) ([]transfer.Chunk, error) {
			// One chunk short of the file: the receiver would build a
			// partial file and never know.
			chunks, err := src.server.Chunks(c, peer, req)
			if err != nil {
				return nil, err
			}
			return chunks[:len(chunks)-1], nil
		},
	})

	summary := d.sync(t, conn)
	if summary.Failed != 1 || summary.Pulled != 0 {
		t.Fatalf("summary = %+v, want one failed file", summary)
	}
	if _, ok := d.fs.Content(d.folder.Path + "/big.bin"); ok {
		t.Fatal("a file was installed from a chunk list that does not describe it")
	}
}
