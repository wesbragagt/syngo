package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// TestFiles_ProvisionalSurvivesTheDatabase checks that the mark a version
// written from an unconfirmed counter floor carries is stored and read back.
// It is the only thing that survives a restart, and the whole point of it is
// that the sync plan can still see it at the first contact.
func TestFiles_ProvisionalSurvivesTheDatabase(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	version, err := manifest.ParseVersion("BBB:2")
	if err != nil {
		t.Fatalf("ParseVersion error: %v", err)
	}

	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{
		{Path: "a.txt", Kind: manifest.KindFile, Size: 5, Hash: hashOf(1), ModTime: testTime,
			Version: version, ModifiedBy: "BBB", Provisional: true},
		{Path: "b.txt", Kind: manifest.KindFile, Size: 5, Hash: hashOf(2), ModTime: testTime,
			Version: version, ModifiedBy: "BBB"},
	}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}

	m, err := store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	if !m.Entries["a.txt"].Provisional {
		t.Fatal("a.txt is not provisional after a round trip, want the mark stored")
	}
	if m.Entries["b.txt"].Provisional {
		t.Fatal("b.txt is provisional, want a plain entry to stay plain")
	}

	// The agreement a sync cycle records when both sides already hold the
	// same content is what settles the doubt: a peer has been found
	// holding the version, so the counter it carries is accepted.
	if err := store.SetVersion(ctx, "docs", "a.txt", version, false); err != nil {
		t.Fatalf("SetVersion error: %v", err)
	}
	got, err := store.GetFile(ctx, "docs", "a.txt")
	if err != nil {
		t.Fatalf("GetFile error: %v", err)
	}
	if got.Provisional {
		t.Fatal("a.txt is still provisional after the agreement cleared it")
	}
}

// TestFiles_ApplyScanMarksItsTombstones covers the other half: a deletion is a
// content change and the store is what raises its counter, so the scan write
// carries the mark for it.
func TestFiles_ApplyScanMarksItsTombstones(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{
		{Path: "a.txt", Kind: manifest.KindFile, Size: 5, Hash: hashOf(1), ModTime: testTime},
	}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}
	seq, err := store.FolderSequence(ctx, "docs")
	if err != nil {
		t.Fatalf("FolderSequence error: %v", err)
	}

	if err := store.ApplyScan(ctx, "docs", ports.ScanWrite{
		BaseSequence: seq, Deleted: []string{"a.txt"}, By: "BBB", At: testTime,
		Provisional: true,
	}); err != nil {
		t.Fatalf("ApplyScan error: %v", err)
	}
	got, err := store.GetFile(ctx, "docs", "a.txt")
	if err != nil {
		t.Fatalf("GetFile error: %v", err)
	}
	if !got.Deleted || !got.Provisional {
		t.Fatalf("a.txt deleted=%v provisional=%v, want a provisional tombstone",
			got.Deleted, got.Provisional)
	}
}
