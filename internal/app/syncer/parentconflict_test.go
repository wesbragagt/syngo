package syncer_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// TestSync_AChildUnderAFileIsSkippedNotFatal is D2: the peer holds a
// directory where this device has a file, so nothing can be written beneath
// that path. The children are skipped one by one and the rest of the plan
// still runs; before, the first of them ended the whole cycle.
func TestSync_AChildUnderAFileIsSkippedNotFatal(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addDir(t, "b.txt")
	src.addFile(t, "b.txt/x.txt", []byte("what is inside the new directory"))
	src.addFile(t, "unrelated.txt", []byte("this one has nothing to do with it"))

	d, conn := newDest(t, src.server)
	// A file the local manifest does not know about, so the plan creates
	// the directory rather than reporting a conflict on it: the puller is
	// what meets the file, exactly as it does when a conflicted parent is
	// still on disk.
	d.fs.AddFile(d.folder.Path+"/b.txt", []byte("a plain file here"), 0o644)

	summary := d.sync(t, conn)
	if summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the unrelated file pulled", summary)
	}
	if summary.Failed != 1 || summary.Failure == nil {
		t.Fatalf("summary = %+v, want the blocked child counted as one failure", summary)
	}
	if got := d.content(t, "unrelated.txt"); string(got) != "this one has nothing to do with it" {
		t.Fatalf("unrelated.txt = %q, want it pulled after the blocked path", got)
	}
	if got, ok := d.fs.Content(d.folder.Path + "/b.txt"); !ok || string(got) != "a plain file here" {
		t.Fatalf("b.txt = %q, want the local file untouched", got)
	}

	folderState, err := d.store.GetFolder(context.Background(), d.folder.ID)
	if err != nil {
		t.Fatalf("stored folder: %v", err)
	}
	if folderState.PendingItems != 1 || folderState.LastSyncError == "" {
		t.Fatalf("folder = %+v, want the skipped items reported", folderState)
	}
}
