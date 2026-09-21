package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/wesbragagt/syncthis/internal/adapters/sqlitestore"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// testTime is a fixed timestamp used by the store tests.
var testTime = time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC)

// openStore opens a store on a fresh database file and closes it on cleanup.
func openStore(t *testing.T) (*sqlitestore.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlitestore.Open(context.Background(), path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

// testFolder returns a valid folder for the given id and path.
func testFolder(id folder.ID, path string) folder.Folder {
	return folder.Folder{
		ID:             id,
		Path:           path,
		Label:          "label",
		Mode:           folder.ModeSendReceive,
		DeletePolicy:   folder.DeletePropagate,
		Symlinks:       folder.SymlinkSkip,
		KeepEmptyDirs:  true,
		IgnorePatterns: []string{"*.tmp", "!keep.tmp"},
		CreatedAt:      testTime,
	}
}

func TestOpen_MigratesFreshDB(t *testing.T) {
	_, path := openStore(t)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 1 {
		t.Fatalf("schema version = %d, want 1", version)
	}

	tables := []string{"folders", "folder_ignore_patterns", "files", "folder_sequence", "changes",
		"devices", "transfers", "transfer_chunks", "conflicts"}
	for _, name := range tables {
		var found string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, name).Scan(&found)
		if err != nil {
			t.Fatalf("table %q missing: %v", name, err)
		}
	}

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestOpen_IdempotentOnExistingDB(t *testing.T) {
	ctx := context.Background()
	store, path := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	reopened, err := sqlitestore.Open(ctx, path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("second Open error: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	got, err := reopened.GetFolder(ctx, "docs")
	if err != nil {
		t.Fatalf("GetFolder error: %v", err)
	}
	if got.Path != "/home/u/docs" || len(got.IgnorePatterns) != 2 {
		t.Fatalf("folder = %+v, want the folder saved before reopening", got)
	}
}

func TestFolders_CRUD_AndCascade(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)

	f := testFolder("docs", "/home/u/docs")
	if err := store.SaveFolder(ctx, f); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}

	got, err := store.GetFolder(ctx, "docs")
	if err != nil {
		t.Fatalf("GetFolder error: %v", err)
	}
	if got.ID != f.ID || got.Path != f.Path || got.Label != f.Label || got.Mode != f.Mode ||
		got.DeletePolicy != f.DeletePolicy || got.Symlinks != f.Symlinks || !got.KeepEmptyDirs ||
		!got.CreatedAt.Equal(testTime) || !got.LastScanAt.IsZero() {
		t.Fatalf("folder = %+v, want %+v", got, f)
	}
	if len(got.IgnorePatterns) != 2 || got.IgnorePatterns[0] != "*.tmp" || got.IgnorePatterns[1] != "!keep.tmp" {
		t.Fatalf("ignore patterns = %v", got.IgnorePatterns)
	}

	if err := store.SetFolderPaused(ctx, "docs", true); err != nil {
		t.Fatalf("SetFolderPaused error: %v", err)
	}
	if err := store.SetFolderScan(ctx, "docs", testTime.Add(time.Minute), "boom"); err != nil {
		t.Fatalf("SetFolderScan error: %v", err)
	}
	got, err = store.GetFolder(ctx, "docs")
	if err != nil {
		t.Fatalf("GetFolder error: %v", err)
	}
	if !got.Paused || got.LastError != "boom" || !got.LastScanAt.Equal(testTime.Add(time.Minute)) {
		t.Fatalf("folder = %+v, want paused with scan state", got)
	}

	if err := store.SaveFolder(ctx, testFolder("music", "/home/u/music")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	list, err := store.ListFolders(ctx)
	if err != nil {
		t.Fatalf("ListFolders error: %v", err)
	}
	if len(list) != 2 || list[0].ID != "docs" || list[1].ID != "music" {
		t.Fatalf("ListFolders = %+v, want docs then music", list)
	}

	entry := manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Size: 5, Hash: hashOf(1), ModTime: testTime}
	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{entry}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}
	tr := transfer.NewInbound("t1", "docs", entry, "PEER", transfer.MinChunkSize, "/tmp/t1.part", testTime)
	if err := store.SaveTransfer(ctx, tr); err != nil {
		t.Fatalf("SaveTransfer error: %v", err)
	}

	if err := store.DeleteFolder(ctx, "docs"); err != nil {
		t.Fatalf("DeleteFolder error: %v", err)
	}
	if _, err := store.GetFolder(ctx, "docs"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("GetFolder after delete error = %v, want ErrNotFound", err)
	}
	m, err := store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("files survived folder deletion: %+v", m.Entries)
	}
	if _, err := store.GetTransfer(ctx, "t1"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("GetTransfer after folder delete error = %v, want ErrNotFound", err)
	}
	if err := store.DeleteFolder(ctx, "docs"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("second DeleteFolder error = %v, want ErrNotFound", err)
	}
}

func TestFolderAlreadyExists_ByPath(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)

	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	err := store.SaveFolder(ctx, testFolder("other", "/home/u/docs"))
	if !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("SaveFolder error = %v, want ErrAlreadyExists", err)
	}

	updated := testFolder("docs", "/home/u/docs")
	updated.Label = "renamed"
	if err := store.SaveFolder(ctx, updated); err != nil {
		t.Fatalf("re-saving the same folder failed: %v", err)
	}
}

func TestConcurrentWritersDoNotBusyError(t *testing.T) {
	ctx := context.Background()
	store, path := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}

	cliStore, err := sqlitestore.Open(ctx, path, sqlitestore.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("second Open error: %v", err)
	}
	defer func() { _ = cliStore.Close() }()

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := store
			if i%2 == 1 {
				target = cliStore
			}
			entry := manifest.Entry{
				Path: filepath.Join("dir", string(rune('a'+i))),
				Kind: manifest.KindFile, Size: int64(i), Hash: hashOf(byte(i + 1)), ModTime: testTime,
			}
			if err := target.UpsertEntries(ctx, "docs", []manifest.Entry{entry}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent upsert failed: %v", err)
	}

	m, err := store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	if len(m.Entries) != 20 {
		t.Fatalf("stored %d entries, want 20", len(m.Entries))
	}
}
