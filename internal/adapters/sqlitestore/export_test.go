package sqlitestore

import (
	"context"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// MigrationSQL returns the script that brings the schema to the given version.
// Tests use it to build a database at an older version than the current one.
func MigrationSQL(version int) (string, error) {
	if version < 1 || version > len(migrationFiles) {
		return "", fmt.Errorf("no migration for version %d", version)
	}
	data, err := migrations.ReadFile(migrationFiles[version-1])
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// SchemaVersion is the version Open migrates a database to.
const SchemaVersion = schemaVersion

// DropChunkRowsFrom deletes the chunk rows of a transfer from idx on, which is
// what a save interrupted between two batches leaves behind.
func (s *Store) DropChunkRowsFrom(ctx context.Context, id transfer.ID, idx int) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM transfer_chunks WHERE transfer_id = ? AND idx >= ?`, string(id), idx)
	return err
}

// Pragma returns the value of a pragma on the connection the store uses, so a
// test can check that the settings in the DSN reached the database.
func (s *Store) Pragma(ctx context.Context, name string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `PRAGMA `+name).Scan(&value)
	return value, err
}
