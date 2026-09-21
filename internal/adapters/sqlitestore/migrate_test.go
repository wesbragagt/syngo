package sqlitestore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/wesbragagt/syncthis/internal/adapters/sqlitestore"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// testHash is a valid 64 character content hash for the seeded file row.
const testHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// writeVersion1DB creates a database holding only the first migration, plus
// one folder and one file, and returns its path.
func writeVersion1DB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()

	script, err := sqlitestore.MigrationSQL(1)
	if err != nil {
		t.Fatalf("read migration 1: %v", err)
	}
	ctx := context.Background()
	for _, stmt := range []string{
		script,
		`INSERT INTO schema_version (version) VALUES (1)`,
		`INSERT INTO folders (id, path, label, mode, delete_policy, include_hidden, symlink_policy,
			keep_empty_dirs, paused, last_error, created_at)
		 VALUES ('docs', '/srv/docs', '', 'sendreceive', 'propagate', 0, 'store', 1, 0, '', 1)`,
		`INSERT INTO files (folder_id, rel_path, kind, size, mtime_ns, mode, hash, sequence, updated_at)
		 VALUES ('docs', 'a.txt', 1, 5, 42, 420, '` + testHash + `', 1, 1)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare version 1 database: %v", err)
		}
	}
	return path
}

func TestOpen_MigratesAVersion1DatabaseAndKeepsItsRows(t *testing.T) {
	ctx := context.Background()
	path := writeVersion1DB(t)

	store, err := sqlitestore.Open(ctx, path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	f, err := store.GetFolder(ctx, folder.ID("docs"))
	if err != nil {
		t.Fatalf("folder after the migration: %v", err)
	}
	if f.Path != "/srv/docs" || f.Mode != folder.ModeSendReceive {
		t.Fatalf("folder = %+v, want the row written before the migration", f)
	}

	entry, err := store.GetFile(ctx, folder.ID("docs"), "a.txt")
	if err != nil {
		t.Fatalf("file after the migration: %v", err)
	}
	if entry.Size != 5 || entry.Mode != 420 {
		t.Fatalf("entry = %+v, want the row written before the migration", entry)
	}
	// The column migration 002 adds defaults to "no baseline".
	if entry.SyncedMode != 0 {
		t.Fatalf("synced_mode = %d, want 0 for a row that predates the column", entry.SyncedMode)
	}

	if got := schemaVersionOf(t, path); got != sqlitestore.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", got, sqlitestore.SchemaVersion)
	}
}

// schemaVersionOf reads the recorded schema version of a database file.
func schemaVersionOf(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return version
}
