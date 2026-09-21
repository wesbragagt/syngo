package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

func TestSaveFolder_PersistsSyncWindows(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)

	f := testFolder("docs", "/home/u/docs")
	windows, err := folder.ParseWindows([]string{"22:00-06:00", "12:00-13:00"})
	if err != nil {
		t.Fatalf("parse windows: %v", err)
	}
	f.SyncWindows = windows
	if err := store.SaveFolder(ctx, f); err != nil {
		t.Fatalf("SaveFolder: %v", err)
	}

	got, err := store.GetFolder(ctx, "docs")
	if err != nil {
		t.Fatalf("GetFolder: %v", err)
	}
	if folder.EncodeWindows(got.SyncWindows) != "22:00-06:00,12:00-13:00" {
		t.Fatalf("sync windows = %v, want the two that were saved", got.SyncWindows.Strings())
	}

	// Clearing the list is a write like any other, not a field the update
	// leaves alone.
	f.SyncWindows = nil
	if err := store.SaveFolder(ctx, f); err != nil {
		t.Fatalf("SaveFolder without windows: %v", err)
	}
	list, err := store.ListFolders(ctx)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if len(list) != 1 || !list[0].SyncWindows.Always() {
		t.Fatalf("sync windows after clearing = %v, want an always open folder", list)
	}
}
