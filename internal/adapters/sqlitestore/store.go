// Package sqlitestore implements ports.MetadataStore on top of a local SQLite
// database using the pure-Go modernc.org/sqlite driver.
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	// The pure-Go SQLite driver registers itself as "sqlite".
	_ "modernc.org/sqlite"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

var _ ports.MetadataStore = (*Store)(nil)

// pragmas are applied to every connection through the DSN, so they hold no
// matter which pooled connection a statement runs on.
var pragmas = []string{
	"journal_mode(WAL)",
	"synchronous(NORMAL)",
	"busy_timeout(5000)",
	"foreign_keys(ON)",
}

// Options configures how the database is opened.
type Options struct {
	// MaxOpenConns limits the connection pool. It defaults to 1 so that
	// concurrent writers queue in Go instead of colliding in SQLite.
	MaxOpenConns int
}

// Store is a SQLite-backed metadata store.
type Store struct {
	db *sql.DB
}

// Open opens the database at path, applying pragmas and running migrations.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	maxConns := opts.MaxOpenConns
	if maxConns <= 0 {
		maxConns = 1
	}
	db.SetMaxOpenConns(maxConns)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// dsn builds the connection string for a database file.
func dsn(path string) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	// Transactions take the write lock up front: a deferred transaction that
	// upgrades from read to write fails immediately with SQLITE_BUSY instead
	// of waiting for busy_timeout.
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}

// Close releases the database handles.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}
	return nil
}

// tx runs fn inside a transaction, rolling back on error.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// batchSize is the largest number of rows written per transaction.
const batchSize = 500

// asAlreadyExists converts a SQLite uniqueness violation into ErrAlreadyExists.
func asAlreadyExists(err error, what string) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%s: %w", what, ports.ErrAlreadyExists)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// notFound turns sql.ErrNoRows into the port's ErrNotFound.
func notFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, ports.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// nullTime converts a time to a nullable unix nanosecond column value.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

// timeFrom converts a nullable unix nanosecond column into a time. A NULL
// column becomes the zero time.
func timeFrom(ns sql.NullInt64) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	return time.Unix(0, ns.Int64).UTC()
}
