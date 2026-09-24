package sqlitestore_test

import (
	"context"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/adapters/sqlitestore"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// baseFor builds a merge base candidate of one path with the given content.
func baseFor(content string, counter uint64, at time.Time, seed byte) ports.MergeBase {
	return ports.MergeBase{
		FolderID: "docs", Path: "notes.txt",
		Version: manifest.Version{}.Raise(device.ID("DEV"), counter),
		Hash:    hashOf(seed), Content: []byte(content), SavedAt: at,
	}
}

// storeWithFile returns a store holding one folder and one file row, which is
// what a merge base hangs off.
func storeWithFile(t *testing.T) (*sqlitestore.Store, context.Context) {
	t.Helper()
	store, _ := openStore(t)
	ctx := context.Background()
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	entry := manifest.Entry{
		Path: "notes.txt", Kind: manifest.KindFile, Size: 4, Hash: hashOf(1), ModTime: testTime,
	}
	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{entry}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}
	return store, ctx
}

func TestMergeBases_RoundTripNewestFirst(t *testing.T) {
	store, ctx := storeWithFile(t)
	for i, content := range []string{"one\n", "two\n", "three\n"} {
		if err := store.SaveMergeBase(ctx, baseFor(content, uint64(i+1),
			testTime.Add(time.Duration(i)*time.Minute), byte(i+1))); err != nil {
			t.Fatalf("SaveMergeBase error: %v", err)
		}
	}

	got, err := store.MergeBases(ctx, "docs", "notes.txt")
	if err != nil {
		t.Fatalf("MergeBases error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d bases, want 3", len(got))
	}
	if string(got[0].Content) != "three\n" || string(got[2].Content) != "one\n" {
		t.Fatalf("bases are not newest first: %q", contentsOf(got))
	}
	if got[0].Version.Counter("DEV") != 3 {
		t.Fatalf("newest version counter = %d, want 3", got[0].Version.Counter("DEV"))
	}
}

func TestMergeBases_OnlyTheNewestFewAreKept(t *testing.T) {
	store, ctx := storeWithFile(t)
	for i := range ports.MergeBaseHistory + 3 {
		content := string(rune('a'+i)) + "\n"
		if err := store.SaveMergeBase(ctx, baseFor(content, uint64(i+1),
			testTime.Add(time.Duration(i)*time.Minute), byte(i+1))); err != nil {
			t.Fatalf("SaveMergeBase error: %v", err)
		}
	}

	got, err := store.MergeBases(ctx, "docs", "notes.txt")
	if err != nil {
		t.Fatalf("MergeBases error: %v", err)
	}
	if len(got) != ports.MergeBaseHistory {
		t.Fatalf("got %d bases, want %d", len(got), ports.MergeBaseHistory)
	}
	if string(got[0].Content) != "g\n" {
		t.Fatalf("newest base = %q, want %q", got[0].Content, "g\n")
	}
}

// TestMergeBases_TheSameContentTwiceIsOneRow: rescanning a file that did not
// change must not push the version a divergence needs out of the history.
func TestMergeBases_TheSameContentTwiceIsOneRow(t *testing.T) {
	store, ctx := storeWithFile(t)
	for i := range 6 {
		if err := store.SaveMergeBase(ctx, baseFor("one\n", 1,
			testTime.Add(time.Duration(i)*time.Minute), 1)); err != nil {
			t.Fatalf("SaveMergeBase error: %v", err)
		}
	}

	got, err := store.MergeBases(ctx, "docs", "notes.txt")
	if err != nil {
		t.Fatalf("MergeBases error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bases, want 1", len(got))
	}
}

// TestMergeBases_GoWithTheFile: the rows hang off the file row, so removing
// the folder takes the content with it rather than leaving it in the database.
func TestMergeBases_GoWithTheFile(t *testing.T) {
	store, ctx := storeWithFile(t)
	if err := store.SaveMergeBase(ctx, baseFor("one\n", 1, testTime, 1)); err != nil {
		t.Fatalf("SaveMergeBase error: %v", err)
	}
	if err := store.DeleteFolder(ctx, "docs"); err != nil {
		t.Fatalf("DeleteFolder error: %v", err)
	}

	got, err := store.MergeBases(ctx, "docs", "notes.txt")
	if err != nil {
		t.Fatalf("MergeBases error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bases after the folder went, want 0", len(got))
	}
}

func TestMergeBases_UnknownPathHasNone(t *testing.T) {
	store, ctx := storeWithFile(t)
	got, err := store.MergeBases(ctx, "docs", "nothing.txt")
	if err != nil {
		t.Fatalf("MergeBases error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bases, want 0", len(got))
	}
}

// contentsOf is what a failure message prints.
func contentsOf(bases []ports.MergeBase) []string {
	out := make([]string, 0, len(bases))
	for _, b := range bases {
		out = append(out, string(b.Content))
	}
	return out
}

var _ ports.MergeBaseStore = (*sqlitestore.Store)(nil)
