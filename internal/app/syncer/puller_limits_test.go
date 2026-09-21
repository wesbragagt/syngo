package syncer_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// inflate announces a size no file on the peer has, which is what a peer that
// wants the receiver to allocate a chunk list of its choosing would send.
type inflate struct {
	ports.PeerHandler
	size int64
}

// Manifest replaces the size of every file entry.
func (i inflate) Manifest(ctx context.Context, peer device.ID, id folder.ID) (manifest.Manifest, error) {
	m, err := i.PeerHandler.Manifest(ctx, peer, id)
	if err != nil {
		return m, err
	}
	for p, e := range m.Entries {
		if e.Kind == manifest.KindFile {
			e.Size = i.size
			m.Entries[p] = e
		}
	}
	return m, nil
}

// TestSync_RefusesAFileWithTooManyChunks proves an announced size cannot
// decide how much this device allocates: the chunk count is checked before the
// chunk list or its journal rows exist.
func TestSync_RefusesAFileWithTooManyChunks(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "huge.bin", payload(16, 3))

	d, conn := newDest(t, inflate{PeerHandler: src.server, size: 1 << 60})
	summary := d.sync(t, conn)

	if summary.Failed != 1 || summary.Pulled != 0 {
		t.Fatalf("summary = %+v, want one failed file and nothing pulled", summary)
	}
	list, err := d.store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("%d transfers journalled, want none", len(list))
	}
	if _, ok := d.fs.Content(d.folder.Path + "/huge.bin"); ok {
		t.Fatal("the file was created")
	}
}
