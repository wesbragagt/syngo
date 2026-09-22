package sqlitestore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/wesbragagt/syncthis/internal/adapters/sqlitestore"
	"github.com/wesbragagt/syncthis/internal/domain/device"
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
		`INSERT INTO conflicts (id, folder_id, rel_path, conflict_path, local_hash, remote_hash,
			peer_id, detected_at, resolved)
		 VALUES ('c1', 'docs', 'a.txt', 'a.copy.txt', '` + testHash + `', '', 'PEERID', 7, 0)`,
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

	conflict, err := store.GetConflict(ctx, "c1")
	if err != nil {
		t.Fatalf("conflict after the migration: %v", err)
	}
	if conflict.Path != "a.txt" || conflict.ConflictPath != "a.copy.txt" || conflict.Resolved {
		t.Fatalf("conflict = %+v, want the row written before the migration", conflict)
	}
	// The column migration 003 adds defaults to "not recorded".
	if conflict.Reason != "" {
		t.Fatalf("reason = %q, want it empty for a row that predates the column", conflict.Reason)
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

// writeVersion3DB creates a database holding the first three migrations plus
// one folder row, which is what a folder configured before sync windows
// existed looks like, and returns its path.
func writeVersion3DB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	for version := 1; version <= 3; version++ {
		script, err := sqlitestore.MigrationSQL(version)
		if err != nil {
			t.Fatalf("read migration %d: %v", version, err)
		}
		if _, err := db.ExecContext(ctx, script); err != nil {
			t.Fatalf("apply migration %d: %v", version, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO schema_version (version) VALUES (3)`,
		`INSERT INTO folders (id, path, label, mode, delete_policy, include_hidden, symlink_policy,
			keep_empty_dirs, paused, last_error, created_at)
		 VALUES ('docs', '/srv/docs', '', 'sendreceive', 'propagate', 0, 'store', 1, 0, '', 1)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare version 3 database: %v", err)
		}
	}
	return path
}

func TestMigrate_3To4_KeepsFoldersAndReadsThemAsAlwaysOpen(t *testing.T) {
	ctx := context.Background()
	path := writeVersion3DB(t)

	store, err := sqlitestore.Open(ctx, path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	f, err := store.GetFolder(ctx, folder.ID("docs"))
	if err != nil {
		t.Fatalf("folder after the migration: %v", err)
	}
	if f.Path != "/srv/docs" {
		t.Fatalf("folder = %+v, want the row written before the migration", f)
	}
	if !f.SyncWindows.Always() {
		t.Fatalf("sync windows = %v, want a folder that predates the column to be always open",
			f.SyncWindows.Strings())
	}
	if got := schemaVersionOf(t, path); got != sqlitestore.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", got, sqlitestore.SchemaVersion)
	}
}

// writeVersion4DB creates a database holding the first four migrations plus
// one folder and one file row, which is what a device upgrading to version
// vectors starts from, and returns its path.
func writeVersion4DB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	for version := 1; version <= 4; version++ {
		script, err := sqlitestore.MigrationSQL(version)
		if err != nil {
			t.Fatalf("read migration %d: %v", version, err)
		}
		if _, err := db.ExecContext(ctx, script); err != nil {
			t.Fatalf("apply migration %d: %v", version, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO schema_version (version) VALUES (4)`,
		`INSERT INTO folders (id, path, label, mode, delete_policy, include_hidden, symlink_policy,
			keep_empty_dirs, paused, last_error, created_at)
		 VALUES ('docs', '/srv/docs', '', 'sendreceive', 'propagate', 0, 'store', 1, 0, '', 1)`,
		`INSERT INTO files (folder_id, rel_path, kind, size, mtime_ns, mode, hash, sequence, updated_at)
		 VALUES ('docs', 'a.txt', 1, 5, 42, 420, '` + testHash + `', 1, 1)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare version 4 database: %v", err)
		}
	}
	return path
}

func TestMigrate_4To5_KeepsFilesAndGivesThemAnEmptyVersion(t *testing.T) {
	ctx := context.Background()
	path := writeVersion4DB(t)

	store, err := sqlitestore.Open(ctx, path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	entry, err := store.GetFile(ctx, folder.ID("docs"), "a.txt")
	if err != nil {
		t.Fatalf("file after the migration: %v", err)
	}
	if entry.Hash.String() != testHash || entry.Size != 5 {
		t.Fatalf("entry = %+v, want the row written before the migration", entry)
	}
	// Nothing is backfilled: sequences are per device, so any guess could
	// order a stale version above a newer one.
	if !entry.Version.IsZero() {
		t.Fatalf("version = %q, want it empty for a row that predates the column", entry.Version)
	}
	if got := schemaVersionOf(t, path); got != sqlitestore.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", got, sqlitestore.SchemaVersion)
	}
}

// writeVersion5DB creates a database holding the first five migrations plus
// one folder, which is what a device upgrading to the folder health columns
// starts from, and returns its path.
func writeVersion5DB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	for version := 1; version <= 5; version++ {
		script, err := sqlitestore.MigrationSQL(version)
		if err != nil {
			t.Fatalf("read migration %d: %v", version, err)
		}
		if _, err := db.ExecContext(ctx, script); err != nil {
			t.Fatalf("apply migration %d: %v", version, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO schema_version (version) VALUES (5)`,
		`INSERT INTO folders (id, path, label, mode, delete_policy, include_hidden, symlink_policy,
			keep_empty_dirs, paused, last_error, created_at)
		 VALUES ('docs', '/srv/docs', '', 'sendreceive', 'propagate', 0, 'store', 1, 0, 'a scan failure', 1)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare version 5 database: %v", err)
		}
	}
	return path
}

func TestMigrate_5To6_KeepsFoldersAndLeavesTheSyncErrorEmpty(t *testing.T) {
	ctx := context.Background()
	path := writeVersion5DB(t)

	store, err := sqlitestore.Open(ctx, path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	f, err := store.GetFolder(ctx, folder.ID("docs"))
	if err != nil {
		t.Fatalf("folder after the migration: %v", err)
	}
	if f.Path != "/srv/docs" || f.LastError != "a scan failure" {
		t.Fatalf("folder = %+v, want the row written before the migration", f)
	}
	// A folder that predates the columns has no sync failure on record;
	// the scan failure it does carry is the scanner's and stays put.
	if f.LastSyncError != "" || f.PendingItems != 0 {
		t.Fatalf("sync error = %q with %d pending items, want neither", f.LastSyncError, f.PendingItems)
	}
	if got := schemaVersionOf(t, path); got != sqlitestore.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", got, sqlitestore.SchemaVersion)
	}
}

// TestMigrate_6To7_StartsWithNoCounterFloor covers the counter floor table: a
// device that upgrades has no floor on record, and the first scan and the
// first manifest it receives set one.
func TestMigrate_6To7_StartsWithNoCounterFloor(t *testing.T) {
	ctx := context.Background()
	path := writeVersion5DB(t)

	store, err := sqlitestore.Open(ctx, path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	const self = device.ID("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	floor, err := store.CounterFloor(ctx, self)
	if err != nil {
		t.Fatalf("counter floor: %v", err)
	}
	if floor != 0 {
		t.Fatalf("floor = %d, want none recorded for an upgraded device", floor)
	}
	if got := schemaVersionOf(t, path); got != sqlitestore.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", got, sqlitestore.SchemaVersion)
	}
}
