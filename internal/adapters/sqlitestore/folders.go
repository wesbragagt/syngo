package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// folderColumns is the column list shared by the folder queries.
const folderColumns = `id, path, label, mode, delete_policy, include_hidden, symlink_policy,
	keep_empty_dirs, paused, last_scan_at, last_error, created_at, sync_windows`

// SaveFolder inserts or updates a folder and its ignore patterns.
func (s *Store) SaveFolder(ctx context.Context, f folder.Folder) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var other string
		err := tx.QueryRowContext(ctx, `SELECT id FROM folders WHERE path = ? AND id <> ?`, f.Path, string(f.ID)).Scan(&other)
		switch {
		case err == nil:
			return fmt.Errorf("folder path %q is already used by folder %q: %w", f.Path, other, ports.ErrAlreadyExists)
		case err != sql.ErrNoRows:
			return fmt.Errorf("check folder path %q: %w", f.Path, err)
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO folders (`+folderColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				path = excluded.path, label = excluded.label, mode = excluded.mode,
				delete_policy = excluded.delete_policy, include_hidden = excluded.include_hidden,
				symlink_policy = excluded.symlink_policy, keep_empty_dirs = excluded.keep_empty_dirs,
				paused = excluded.paused, last_scan_at = excluded.last_scan_at,
				last_error = excluded.last_error, sync_windows = excluded.sync_windows`,
			string(f.ID), f.Path, f.Label, string(f.Mode), string(f.DeletePolicy), boolToInt(f.IncludeHidden),
			string(f.Symlinks), boolToInt(f.KeepEmptyDirs), boolToInt(f.Paused), nullTime(f.LastScanAt),
			f.LastError, f.CreatedAt.UnixNano(), folder.EncodeWindows(f.SyncWindows))
		if err != nil {
			return asAlreadyExists(err, fmt.Sprintf("save folder %q", f.ID))
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO folder_sequence (folder_id, next) VALUES (?, 1) ON CONFLICT(folder_id) DO NOTHING`,
			string(f.ID)); err != nil {
			return fmt.Errorf("init sequence for folder %q: %w", f.ID, err)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM folder_ignore_patterns WHERE folder_id = ?`, string(f.ID)); err != nil {
			return fmt.Errorf("clear ignore patterns of folder %q: %w", f.ID, err)
		}
		for i, pattern := range f.IgnorePatterns {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO folder_ignore_patterns (folder_id, position, pattern) VALUES (?, ?, ?)`,
				string(f.ID), i, pattern); err != nil {
				return fmt.Errorf("save ignore pattern %d of folder %q: %w", i, f.ID, err)
			}
		}
		return nil
	})
}

// GetFolder returns one folder, or ErrNotFound.
func (s *Store) GetFolder(ctx context.Context, id folder.ID) (folder.Folder, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+folderColumns+` FROM folders WHERE id = ?`, string(id))
	f, err := scanFolder(row)
	if err != nil {
		return folder.Folder{}, notFound(err, fmt.Sprintf("folder %q", id))
	}
	patterns, err := s.ignorePatterns(ctx, id)
	if err != nil {
		return folder.Folder{}, err
	}
	f.IgnorePatterns = patterns
	return f, nil
}

// ListFolders returns every folder ordered by id.
func (s *Store) ListFolders(ctx context.Context) ([]folder.Folder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+folderColumns+` FROM folders ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]folder.Folder, 0)
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, fmt.Errorf("list folders: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}

	for i := range out {
		patterns, err := s.ignorePatterns(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].IgnorePatterns = patterns
	}
	return out, nil
}

// DeleteFolder removes a folder; its files, patterns and transfers cascade.
func (s *Store) DeleteFolder(ctx context.Context, id folder.ID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM folders WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("delete folder %q: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("folder %q", id))
}

// SetFolderPaused sets the paused flag of a folder.
func (s *Store) SetFolderPaused(ctx context.Context, id folder.ID, paused bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE folders SET paused = ? WHERE id = ?`, boolToInt(paused), string(id))
	if err != nil {
		return fmt.Errorf("set paused on folder %q: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("folder %q", id))
}

// SetFolderScan records the time and outcome of the last scan.
func (s *Store) SetFolderScan(ctx context.Context, id folder.ID, at time.Time, lastError string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE folders SET last_scan_at = ?, last_error = ? WHERE id = ?`,
		nullTime(at), lastError, string(id))
	if err != nil {
		return fmt.Errorf("set scan state on folder %q: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("folder %q", id))
}

// ignorePatterns returns a folder's ignore patterns in order.
func (s *Store) ignorePatterns(ctx context.Context, id folder.ID) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pattern FROM folder_ignore_patterns WHERE folder_id = ? ORDER BY position`, string(id))
	if err != nil {
		return nil, fmt.Errorf("list ignore patterns of folder %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var pattern string
		if err := rows.Scan(&pattern); err != nil {
			return nil, fmt.Errorf("list ignore patterns of folder %q: %w", id, err)
		}
		out = append(out, pattern)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ignore patterns of folder %q: %w", id, err)
	}
	return out, nil
}

// scanner is implemented by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanFolder reads one folder row.
func scanFolder(row scanner) (folder.Folder, error) {
	var (
		f            folder.Folder
		id, mode     string
		deletePolicy string
		symlinks     string
		hidden       int
		keepEmpty    int
		paused       int
		lastScan     sql.NullInt64
		createdAt    int64
		windows      string
	)
	if err := row.Scan(&id, &f.Path, &f.Label, &mode, &deletePolicy, &hidden, &symlinks,
		&keepEmpty, &paused, &lastScan, &f.LastError, &createdAt, &windows); err != nil {
		return folder.Folder{}, err
	}
	syncWindows, err := folder.DecodeWindows(windows)
	if err != nil {
		return folder.Folder{}, fmt.Errorf("folder %q: %w", id, err)
	}
	f.SyncWindows = syncWindows
	f.ID = folder.ID(id)
	f.Mode = folder.Mode(mode)
	f.DeletePolicy = folder.DeletePolicy(deletePolicy)
	f.Symlinks = folder.SymlinkPolicy(symlinks)
	f.IncludeHidden = hidden != 0
	f.KeepEmptyDirs = keepEmpty != 0
	f.Paused = paused != 0
	f.LastScanAt = timeFrom(lastScan)
	f.CreatedAt = time.Unix(0, createdAt).UTC()
	return f, nil
}

// requireRow turns an update that matched no row into ErrNotFound.
func requireRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ports.ErrNotFound)
	}
	return nil
}

// boolToInt maps a bool to the integer SQLite stores.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
