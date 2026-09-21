package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
)

// schemaVersion is the version every migration set adds up to.
const schemaVersion = 1

//go:embed migrations/*.sql
var migrations embed.FS

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

	stmts, err := migrations.ReadFile("migrations/001_init.sql")
	if err != nil {
		return fmt.Errorf("read migration 001_init.sql: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, string(stmts)); err != nil {
		return fmt.Errorf("apply migration 001_init.sql: %w", err)
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
