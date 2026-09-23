package sqlitestore_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/wesbragagt/syncthis/internal/adapters/sqlitestore"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
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

// writeVersion7ChunkDB creates a database holding the first seven migrations, one
// folder, one file that is on disk, and one download that was in flight when
// the process stopped. It is what a device upgrading to the chunk index starts
// from, and it returns its path.
func writeVersion7ChunkDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	for version := 1; version <= 7; version++ {
		script, err := sqlitestore.MigrationSQL(version)
		if err != nil {
			t.Fatalf("read migration %d: %v", version, err)
		}
		if _, err := db.ExecContext(ctx, script); err != nil {
			t.Fatalf("apply migration %d: %v", version, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO schema_version (version) VALUES (7)`,
		`INSERT INTO folders (id, path, label, mode, delete_policy, include_hidden, symlink_policy,
			keep_empty_dirs, paused, last_error, created_at)
		 VALUES ('docs', '/srv/docs', '', 'sendreceive', 'propagate', 0, 'store', 1, 0, '', 1)`,
		`INSERT INTO folder_sequence (folder_id, next) VALUES ('docs', 2)`,
		`INSERT INTO files (folder_id, rel_path, kind, size, mtime_ns, mode, hash, sequence, updated_at)
		 VALUES ('docs', 'held.bin', 1, 8192, 1, 420, '` + strings.Repeat("aa", 32) + `', 1, 1)`,
		`INSERT INTO transfers (id, folder_id, rel_path, peer_id, hash, size, mtime_ns, mode,
			chunk_size, chunk_count, temp_path, state, created_at, updated_at)
		 VALUES ('t1', 'docs', 'coming.bin', 'PEER', '` + strings.Repeat("bb", 32) +
			`', 12288, 1, 420, 4096, 3, '/srv/docs/.syncthis/tmp/t1.part', 'pending', 1, 1)`,
		`INSERT INTO transfer_chunks (transfer_id, idx, hash, verified)
		 VALUES ('t1', 0, '` + strings.Repeat("cc", 32) + `', 1)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare version 7 database: %v", err)
		}
	}
	return path
}

// TestMigrate_7ToCurrent_StartsWithAnEmptyChunkIndexAndResumesOldTransfers is the
// backward-compatibility promise of content-defined chunking. A database
// written before the chunk index keeps every file row it had and nothing is
// re-downloaded for it: the index simply starts empty, so the first transfer
// after the upgrade finds nothing to reuse and the first scan fills it in.
//
// A download that was in flight keeps its boundaries too. Its journal rows
// carry no offset and no length, so they are read back the way they were
// written, from the transfer's chunk size, and the chunk it had already
// verified stays verified.
func TestMigrate_7ToCurrent_StartsWithAnEmptyChunkIndexAndResumesOldTransfers(t *testing.T) {
	ctx := context.Background()
	path := writeVersion7ChunkDB(t)

	store, err := sqlitestore.Open(ctx, path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	// The file row is untouched, and the index knows nothing about it.
	if _, err := store.GetFile(ctx, folder.ID("docs"), "held.bin"); err != nil {
		t.Fatalf("file after the migration: %v", err)
	}
	indexed, err := store.IndexedPaths(ctx, folder.ID("docs"))
	if err != nil {
		t.Fatalf("indexed paths: %v", err)
	}
	if len(indexed) != 0 {
		t.Fatalf("indexed paths = %v, want none for an upgraded database", indexed)
	}
	var known manifest.Hash
	copy(known[:], bytes.Repeat([]byte{0xaa}, 32))
	if _, err := store.FindChunk(ctx, known); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("FindChunk = %v, want ErrNotFound before the first scan", err)
	}

	// The in-flight download resumes on the boundaries it was started
	// with, one verified chunk of the three it needs.
	tr, err := store.GetTransfer(ctx, transfer.ID("t1"))
	if err != nil {
		t.Fatalf("transfer after the migration: %v", err)
	}
	want := transfer.FixedChunks(12288, 4096)
	if len(tr.Chunks) != len(want) {
		t.Fatalf("%d chunks, want %d", len(tr.Chunks), len(want))
	}
	for i, c := range tr.Chunks {
		if c.Offset != want[i].Offset || c.Length != want[i].Length {
			t.Fatalf("chunk %d = %d bytes at %d, want %d at %d",
				i, c.Length, c.Offset, want[i].Length, want[i].Offset)
		}
	}
	if !tr.Chunks[0].Verified || tr.Chunks[1].Verified {
		t.Fatalf("verified chunks = %+v, want only the first", tr.Chunks)
	}
	if got := schemaVersionOf(t, path); got != sqlitestore.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", got, sqlitestore.SchemaVersion)
	}
}

// writeVersion11ChunkDB builds a database at version 11 holding one file and
// the chunk index of it, which is what every database written before the index
// recorded the content it was cut from looks like.
func writeVersion11ChunkDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	for version := 1; version <= 11; version++ {
		script, err := sqlitestore.MigrationSQL(version)
		if err != nil {
			t.Fatalf("read migration %d: %v", version, err)
		}
		if _, err := db.ExecContext(ctx, script); err != nil {
			t.Fatalf("apply migration %d: %v", version, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO schema_version (version) VALUES (11)`,
		`INSERT INTO folders (id, path, label, mode, delete_policy, include_hidden, symlink_policy,
			keep_empty_dirs, paused, last_error, created_at)
		 VALUES ('docs', '/srv/docs', '', 'sendreceive', 'propagate', 0, 'store', 1, 0, '', 1)`,
		`INSERT INTO folder_sequence (folder_id, next) VALUES ('docs', 2)`,
		`INSERT INTO files (folder_id, rel_path, kind, size, mtime_ns, mode, hash, sequence, updated_at)
		 VALUES ('docs', 'held.bin', 1, 8192, 1, 420, '` + strings.Repeat("aa", 32) + `', 1, 1)`,
		`INSERT INTO file_chunks (folder_id, rel_path, idx, start_offset, byte_length, hash)
		 VALUES ('docs', 'held.bin', 0, 0, 8192, '` + strings.Repeat("cc", 32) + `')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare version 11 database: %v", err)
		}
	}
	return path
}

// TestMigrate_11ToCurrent_StampsTheIndexWithTheHashItWasCutFrom is what keeps
// the upgrade free. A chunk row written before the index recorded its content
// was written for the file row it hangs off, so the migration stamps it with
// that row's hash rather than throwing the index away and making every path be
// read again. The index goes on being served, and a row the next scan or
// download rewrites carries the hash that writer cut from.
func TestMigrate_11ToCurrent_StampsTheIndexWithTheHashItWasCutFrom(t *testing.T) {
	ctx := context.Background()
	path := writeVersion11ChunkDB(t)

	store, err := sqlitestore.Open(ctx, path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	stored, err := store.GetFile(ctx, folder.ID("docs"), "held.bin")
	if err != nil {
		t.Fatalf("file after the migration: %v", err)
	}
	ix, err := store.FileChunks(ctx, folder.ID("docs"), "held.bin")
	if err != nil {
		t.Fatalf("chunks after the migration: %v", err)
	}
	if len(ix.Chunks) != 1 {
		t.Fatalf("%d chunks, want the one the database had", len(ix.Chunks))
	}
	if err := ix.Describes(stored.Hash, stored.Size); err != nil {
		t.Fatalf("the migrated index does not describe its own file row: %v", err)
	}
	if got := schemaVersionOf(t, path); got != sqlitestore.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", got, sqlitestore.SchemaVersion)
	}
}
