package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
)

// schemaVersion is the version every migration set adds up to.
const schemaVersion = 9

//go:embed migrations/*.sql
var migrations embed.FS

// migrationFiles are the migration scripts in the order they apply. The index
// of a script is the version it brings the database to.
var migrationFiles = []string{
	"migrations/001_init.sql",
	"migrations/002_synced_mode.sql",
	"migrations/003_conflict_reason.sql",
	"migrations/004_sync_windows.sql",
	"migrations/005_version.sql",
	"migrations/006_sync_error.sql",
	"migrations/007_device_counters.sql",
	"migrations/008_folder_devices.sql",
	"migrations/009_chunks.sql",
}

// migrate brings a database up to schemaVersion. It is a no-op when the
// database is already current.
func migrate(ctx context.Context, db *sql.DB) error {
	current, err := currentVersion(ctx, db)
	if err != nil {
		return err
	}
	if current == schemaVersion {
		return nil
	}
	if current > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, schemaVersion)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for version := current + 1; version <= schemaVersion; version++ {
		name := migrationFiles[version-1]
		stmts, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(stmts)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schema_version`); err != nil {
		return fmt.Errorf("clear schema version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

// currentVersion returns the schema version of the database, or 0 if it has
// not been migrated yet.
func currentVersion(ctx context.Context, db *sql.DB) (int, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&name)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("inspect schema: %w", err)
	}

	var version int
	if err := db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}
