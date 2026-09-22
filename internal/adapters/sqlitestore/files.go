package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// LoadManifest returns every stored entry of a folder, tombstones included.
func (s *Store) LoadManifest(ctx context.Context, id folder.ID) (manifest.Manifest, error) {
	m := manifest.New(id)

	rows, err := s.db.QueryContext(ctx, `
		SELECT rel_path, kind, size, mtime_ns, mode, hash, link_target, inode, deleted,
		       modified_by, sequence, synced_hash, synced_mode, version
		FROM files WHERE folder_id = ? ORDER BY rel_path`, string(id))
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("load manifest of folder %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("load manifest of folder %q: %w", id, err)
		}
		m.Add(e)
	}
	if err := rows.Err(); err != nil {
		return manifest.Manifest{}, fmt.Errorf("load manifest of folder %q: %w", id, err)
	}

	seq, err := s.folderSequence(ctx, id)
	if err != nil {
		return manifest.Manifest{}, err
	}
	m.Sequence = seq
	return m, nil
}

// GetFile returns one stored entry of a folder, tombstones included, or
// ErrNotFound.
func (s *Store) GetFile(ctx context.Context, id folder.ID, path string) (manifest.Entry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT rel_path, kind, size, mtime_ns, mode, hash, link_target, inode, deleted,
		       modified_by, sequence, synced_hash, synced_mode, version
		FROM files WHERE folder_id = ? AND rel_path = ?`, string(id), path)
	e, err := scanEntry(row)
	if err != nil {
		return manifest.Entry{}, notFound(err, fmt.Sprintf("file %q in folder %q", path, id))
	}
	return e, nil
}

// UpsertEntries writes entries in transactions of at most 500 rows, assigning
// each one the next sequence number of the folder.
func (s *Store) UpsertEntries(ctx context.Context, id folder.ID, entries []manifest.Entry) error {
	now := time.Now().UnixNano()
	for start := 0; start < len(entries); start += batchSize {
		end := min(start+batchSize, len(entries))
		batch := entries[start:end]

		err := s.tx(ctx, func(tx *sql.Tx) error {
			return upsertEntries(ctx, tx, id, batch, now)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// MarkDeleted turns the given paths into tombstones. Unknown paths are ignored.
func (s *Store) MarkDeleted(ctx context.Context, id folder.ID, paths []string, by device.ID, now time.Time) error {
	for start := 0; start < len(paths); start += batchSize {
		end := min(start+batchSize, len(paths))
		batch := paths[start:end]

		err := s.tx(ctx, func(tx *sql.Tx) error {
			return markDeleted(ctx, tx, id, batch, by, now)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// SetSyncedHash records the hash last known to match the peer.
func (s *Store) SetSyncedHash(ctx context.Context, id folder.ID, path string, h manifest.Hash) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE files SET synced_hash = ? WHERE folder_id = ? AND rel_path = ?`,
		h.String(), string(id), path)
	if err != nil {
		return fmt.Errorf("set synced hash of %q in folder %q: %w", path, id, err)
	}
	return requireRow(res, fmt.Sprintf("file %q in folder %q", path, id))
}

// SetVersion replaces the version vector of one entry, without touching its
// sequence: the content is unchanged, only what is known about its history.
func (s *Store) SetVersion(ctx context.Context, id folder.ID, path string, v manifest.Version) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE files SET version = ? WHERE folder_id = ? AND rel_path = ?`,
		v.String(), string(id), path)
	if err != nil {
		return fmt.Errorf("set version of %q in folder %q: %w", path, id, err)
	}
	return requireRow(res, fmt.Sprintf("file %q in folder %q", path, id))
}

// SetSyncedMode records the permission bits last known to match the peer.
func (s *Store) SetSyncedMode(ctx context.Context, id folder.ID, path string, mode uint32) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE files SET synced_mode = ? WHERE folder_id = ? AND rel_path = ?`,
		int64(mode), string(id), path)
	if err != nil {
		return fmt.Errorf("set synced mode of %q in folder %q: %w", path, id, err)
	}
	return requireRow(res, fmt.Sprintf("file %q in folder %q", path, id))
}

// FolderStats counts the live entries of a folder.
func (s *Store) FolderStats(ctx context.Context, id folder.ID) (ports.FolderStats, error) {
	var stats ports.FolderStats
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(kind = ?), 0),
			COALESCE(SUM(kind = ?), 0),
			COALESCE(SUM(kind = ?), 0),
			COALESCE(SUM(CASE WHEN kind = ? THEN size ELSE 0 END), 0)
		FROM files WHERE folder_id = ? AND deleted = 0`,
		int(manifest.KindFile), int(manifest.KindDir), int(manifest.KindSymlink),
		int(manifest.KindFile), string(id)).
		Scan(&stats.Files, &stats.Dirs, &stats.Symlinks, &stats.Bytes)
	if err != nil {
		return ports.FolderStats{}, fmt.Errorf("stats of folder %q: %w", id, err)
	}
	return stats, nil
}

// AppendChanges records scan classifications.
func (s *Store) AppendChanges(ctx context.Context, id folder.ID, changes []manifest.Change, now time.Time) error {
	for start := 0; start < len(changes); start += batchSize {
		end := min(start+batchSize, len(changes))
		batch := changes[start:end]

		err := s.tx(ctx, func(tx *sql.Tx) error {
			return appendChanges(ctx, tx, id, batch, now)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// RecentChanges returns the most recent change records, newest first.
func (s *Store) RecentChanges(ctx context.Context, id folder.ID, limit int) ([]ports.ChangeRecord, error) {
	query := `SELECT seq, folder_id, type, rel_path, old_path, old_hash, new_hash, at
		FROM changes WHERE folder_id = ? ORDER BY seq DESC`
	args := []any{string(id)}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("recent changes of folder %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ports.ChangeRecord
	for rows.Next() {
		var (
			rec                    ports.ChangeRecord
			folderID, changeType   string
			oldHashHex, newHashHex string
			at                     int64
			relPath, oldPath       string
		)
		if err := rows.Scan(&rec.Seq, &folderID, &changeType, &relPath, &oldPath, &oldHashHex, &newHashHex, &at); err != nil {
			return nil, fmt.Errorf("recent changes of folder %q: %w", id, err)
		}
		oldHash, err := manifest.ParseHash(oldHashHex)
		if err != nil {
			return nil, fmt.Errorf("change %d of folder %q: %w", rec.Seq, id, err)
		}
		newHash, err := manifest.ParseHash(newHashHex)
		if err != nil {
			return nil, fmt.Errorf("change %d of folder %q: %w", rec.Seq, id, err)
		}
		rec.FolderID = folder.ID(folderID)
		rec.Type = parseChangeType(changeType)
		rec.Path = relPath
		rec.OldPath = oldPath
		rec.OldHash = oldHash
		rec.NewHash = newHash
		rec.At = time.Unix(0, at).UTC()
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent changes of folder %q: %w", id, err)
	}
	return out, nil
}

// upsertEntries writes entries inside tx, assigning each one the next sequence
// number of the folder.
func upsertEntries(ctx context.Context, tx *sql.Tx, id folder.ID, entries []manifest.Entry, updatedAt int64) error {
	if len(entries) == 0 {
		return nil
	}
	next, err := nextSequence(ctx, tx, id, len(entries))
	if err != nil {
		return err
	}
	for i, e := range entries {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO files (folder_id, rel_path, kind, size, mtime_ns, mode, hash, link_target,
			                   inode, deleted, modified_by, sequence, synced_hash, synced_mode,
			                   version, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(folder_id, rel_path) DO UPDATE SET
				kind = excluded.kind, size = excluded.size, mtime_ns = excluded.mtime_ns,
				mode = excluded.mode, hash = excluded.hash, link_target = excluded.link_target,
				inode = excluded.inode, deleted = excluded.deleted, modified_by = excluded.modified_by,
				sequence = excluded.sequence, synced_hash = excluded.synced_hash,
				synced_mode = excluded.synced_mode, version = excluded.version,
				updated_at = excluded.updated_at`,
			string(id), e.Path, int(e.Kind), e.Size, e.ModTime.UnixNano(), int64(e.Mode),
			e.Hash.String(), e.LinkTarget, int64(e.Inode), boolToInt(e.Deleted),
			string(e.ModifiedBy), int64(next+uint64(i)), e.SyncedHash.String(),
			int64(e.SyncedMode), e.Version.String(), updatedAt); err != nil {
			return fmt.Errorf("upsert file %q in folder %q: %w", e.Path, id, err)
		}
	}
	return nil
}

// markDeleted turns paths into tombstones inside tx. Unknown paths are ignored.
func markDeleted(ctx context.Context, tx *sql.Tx, id folder.ID, paths []string, by device.ID, now time.Time) error {
	if len(paths) == 0 {
		return nil
	}
	next, err := nextSequence(ctx, tx, id, len(paths))
	if err != nil {
		return err
	}
	for i, p := range paths {
		// A tombstone is a content change like any other, so it carries
		// the vector of the version it replaces raised by one: a peer
		// still holding that version is then dominated by the deletion.
		version, err := bumpedVersion(ctx, tx, id, p, by)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE files SET deleted = 1, hash = '', size = 0, mtime_ns = ?, modified_by = ?,
			                 sequence = ?, version = ?, updated_at = ?
			WHERE folder_id = ? AND rel_path = ?`,
			now.UnixNano(), string(by), int64(next+uint64(i)), version, now.UnixNano(),
			string(id), p); err != nil {
			return fmt.Errorf("mark %q deleted in folder %q: %w", p, id, err)
		}
	}
	return nil
}

// bumpedVersion returns the stored version vector of a path with the counter
// of by raised by one, in its stored string form. A path that is not stored
// has no row to update, so its version is irrelevant and reported as empty.
func bumpedVersion(ctx context.Context, tx *sql.Tx, id folder.ID, path string, by device.ID) (string, error) {
	var stored string
	err := tx.QueryRowContext(ctx,
		`SELECT version FROM files WHERE folder_id = ? AND rel_path = ?`, string(id), path).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read version of %q in folder %q: %w", path, id, err)
	}
	v, err := manifest.ParseVersion(stored)
	if err != nil {
		return "", fmt.Errorf("file %q in folder %q: %w", path, id, err)
	}
	return v.Bump(by).String(), nil
}

// appendChanges records scan classifications inside tx.
func appendChanges(ctx context.Context, tx *sql.Tx, id folder.ID, changes []manifest.Change, now time.Time) error {
	for _, c := range changes {
		var oldHash, newHash string
		if c.Old != nil {
			oldHash = c.Old.Hash.String()
		}
		if c.New != nil {
			newHash = c.New.Hash.String()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO changes (folder_id, type, rel_path, old_path, old_hash, new_hash, at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			string(id), c.Type.String(), c.Path, c.OldPath, oldHash, newHash, now.UnixNano()); err != nil {
			return fmt.Errorf("append change for %q in folder %q: %w", c.Path, id, err)
		}
	}
	return nil
}

// ApplyScan writes one scan result in a single immediate transaction. The
// folder sequence read at the start of the transaction must still be the one
// the scan diffed against, otherwise nothing is written and ErrStaleManifest
// is returned: a concurrent scan has already logged its own changes.
func (s *Store) ApplyScan(ctx context.Context, id folder.ID, w ports.ScanWrite) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var next int64
		err := tx.QueryRowContext(ctx,
			`SELECT next FROM folder_sequence WHERE folder_id = ?`, string(id)).Scan(&next)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			next = 1
		case err != nil:
			return fmt.Errorf("read sequence of folder %q: %w", id, err)
		}
		if current := uint64(next) - 1; current != w.BaseSequence {
			return fmt.Errorf("folder %q at sequence %d, scan started from %d: %w",
				id, current, w.BaseSequence, ports.ErrStaleManifest)
		}

		if err := upsertEntries(ctx, tx, id, w.Upserts, w.At.UnixNano()); err != nil {
			return err
		}
		if err := markDeleted(ctx, tx, id, w.Deleted, w.By, w.At); err != nil {
			return err
		}
		if err := appendChanges(ctx, tx, id, w.Changes, w.At); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE folders SET last_scan_at = ?, last_error = ? WHERE id = ?`,
			nullTime(w.At), w.LastError, string(id))
		if err != nil {
			return fmt.Errorf("set scan state on folder %q: %w", id, err)
		}
		return requireRow(res, fmt.Sprintf("folder %q", id))
	})
}

// folderSequence returns the last sequence number assigned in a folder.
func (s *Store) folderSequence(ctx context.Context, id folder.ID) (uint64, error) {
	var next int64
	err := s.db.QueryRowContext(ctx, `SELECT next FROM folder_sequence WHERE folder_id = ?`, string(id)).Scan(&next)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read sequence of folder %q: %w", id, err)
	}
	return uint64(next) - 1, nil
}

// nextSequence reserves n sequence numbers for a folder and returns the first.
func nextSequence(ctx context.Context, tx *sql.Tx, id folder.ID, n int) (uint64, error) {
	var next int64
	err := tx.QueryRowContext(ctx, `SELECT next FROM folder_sequence WHERE folder_id = ?`, string(id)).Scan(&next)
	switch {
	case err == sql.ErrNoRows:
		next = 1
	case err != nil:
		return 0, fmt.Errorf("read sequence of folder %q: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO folder_sequence (folder_id, next) VALUES (?, ?)
		ON CONFLICT(folder_id) DO UPDATE SET next = excluded.next`,
		string(id), next+int64(n)); err != nil {
		return 0, fmt.Errorf("advance sequence of folder %q: %w", id, err)
	}
	return uint64(next), nil
}

// parseChangeType maps a stored change type back to the domain value.
func parseChangeType(s string) manifest.ChangeType {
	switch s {
	case "create":
		return manifest.Create
	case "modify":
		return manifest.Modify
	case "delete":
		return manifest.Delete
	case "rename":
		return manifest.Rename
	default:
		return 0
	}
}

// SyncFolderSequence raises the folder sequence counter above every sequence
// already assigned to a file of that folder, repairing a counter that fell
// behind after an interrupted write.
func (s *Store) SyncFolderSequence(ctx context.Context, id folder.ID) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO folder_sequence (folder_id, next)
		VALUES (?, (SELECT COALESCE(MAX(sequence), 0) + 1 FROM files WHERE folder_id = ?))
		ON CONFLICT(folder_id) DO UPDATE SET next = MAX(next, excluded.next)`,
		string(id), string(id))
	if err != nil {
		return fmt.Errorf("sync sequence of folder %q: %w", id, err)
	}
	return nil
}

// scanEntry reads one file row into a manifest entry.
func scanEntry(row scanner) (manifest.Entry, error) {
	var (
		e                    manifest.Entry
		kind                 int
		mtimeNS              int64
		mode                 int64
		hashHex, syncedHex   string
		deleted              int
		modifiedBy, linkDest string
		sequence, syncedMode int64
		versionText          string
	)
	if err := row.Scan(&e.Path, &kind, &e.Size, &mtimeNS, &mode, &hashHex, &linkDest,
		&e.Inode, &deleted, &modifiedBy, &sequence, &syncedHex, &syncedMode, &versionText); err != nil {
		return manifest.Entry{}, err
	}
	hash, err := manifest.ParseHash(hashHex)
	if err != nil {
		return manifest.Entry{}, fmt.Errorf("file %q: %w", e.Path, err)
	}
	synced, err := manifest.ParseHash(syncedHex)
	if err != nil {
		return manifest.Entry{}, fmt.Errorf("file %q: %w", e.Path, err)
	}
	version, err := manifest.ParseVersion(versionText)
	if err != nil {
		return manifest.Entry{}, fmt.Errorf("file %q: %w", e.Path, err)
	}
	e.Version = version
	e.Kind = manifest.Kind(kind)
	e.ModTime = time.Unix(0, mtimeNS).UTC()
	e.Mode = uint32(mode)
	e.Hash = hash
	e.SyncedHash = synced
	e.SyncedMode = uint32(syncedMode)
	e.LinkTarget = linkDest
	e.Deleted = deleted != 0
	e.ModifiedBy = device.ID(modifiedBy)
	e.Sequence = uint64(sequence)
	return e, nil
}
