package syncer_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// ignoreFile is the per-folder ignore file, read from the folder root.
const ignoreFile = ".syngoignore"

// TestSync_SkipsIncomingPathsTheFolderIgnores proves the ignore rules hold for
// what a peer offers as much as for what a scan finds. Without them an ignored
// file arrives, is recorded, and the next scan reads it as deleted and sends
// the deletion back.
func TestSync_SkipsIncomingPathsTheFolderIgnores(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "keep.txt", payload(32, 1))
	src.addFile(t, "build.tmp", payload(32, 2))
	src.addFile(t, "cache/state.bin", payload(32, 3))

	d, conn := newDest(t, src.server)
	d.folder.IgnorePatterns = []string{"*.tmp", "cache/"}
	if err := d.store.SaveFolder(ctx, d.folder); err != nil {
		t.Fatalf("save folder: %v", err)
	}

	summary := d.sync(t, conn)

	if string(d.content(t, "keep.txt")) != string(payload(32, 1)) {
		t.Fatal("the file that is not ignored did not arrive")
	}
	for _, rel := range []string{"build.tmp", "cache/state.bin", "cache"} {
		if _, ok := d.fs.Content(d.folder.Path + "/" + rel); ok {
			t.Fatalf("%s arrived although the folder ignores it", rel)
		}
	}
	if summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want exactly one pulled file", summary)
	}
	if summary.Skipped < 2 {
		t.Fatalf("summary = %+v, want the ignored paths counted as skipped", summary)
	}

	// Nothing ignored is recorded either, so the next scan has nothing to
	// call deleted.
	m, err := d.store.LoadManifest(ctx, d.folder.ID)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	for _, e := range m.Sorted() {
		if e.Path != "keep.txt" {
			t.Fatalf("manifest holds %q, want only keep.txt", e.Path)
		}
	}
}

// TestSync_SkipsIncomingPathsTheIgnoreFileCovers proves the local
// .syngoignore is consulted as well, and that a peer cannot send one.
func TestSync_SkipsIncomingPathsTheIgnoreFileCovers(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "keep.txt", payload(16, 1))
	src.addFile(t, "secret/key.pem", payload(16, 2))
	src.addFile(t, ignoreFile, []byte("!secret/\n"))

	d, conn := newDest(t, src.server)
	d.fs.AddFile(d.folder.Path+"/"+ignoreFile, []byte("secret/\n"), 0o644)

	d.sync(t, conn)

	if _, ok := d.fs.Content(d.folder.Path + "/secret/key.pem"); ok {
		t.Fatal("a path the ignore file covers arrived")
	}
	if got, ok := d.fs.Content(d.folder.Path + "/" + ignoreFile); !ok || string(got) != "secret/\n" {
		t.Fatalf("the local ignore file is %q, want the peer unable to replace it", got)
	}
}
