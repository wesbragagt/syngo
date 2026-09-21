package sqlitestore

import (
	"context"
	"database/sql"
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
		       modified_by, sequence, synced_hash
		FROM files WHERE folder_id = ? ORDER BY rel_path`, string(id))
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("load manifest of folder %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			e                    manifest.Entry
			kind                 int
			mtimeNS              int64
			mode                 int64
			hashHex, syncedHex   string
			deleted              int
			modifiedBy, linkDest string
			sequence             int64
		)
		if err := rows.Scan(&e.Path, &kind, &e.Size, &mtimeNS, &mode, &hashHex, &linkDest,
			&e.Inode, &deleted, &modifiedBy, &sequence, &syncedHex); err != nil {
			return manifest.Manifest{}, fmt.Errorf("load manifest of folder %q: %w", id, err)
		}
		hash, err := manifest.ParseHash(hashHex)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("file %q in folder %q: %w", e.Path, id, err)
		}
		synced, err := manifest.ParseHash(syncedHex)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("file %q in folder %q: %w", e.Path, id, err)
		}
		e.Kind = manifest.Kind(kind)
		e.ModTime = time.Unix(0, mtimeNS).UTC()
		e.Mode = uint32(mode)
		e.Hash = hash
		e.SyncedHash = synced
		e.LinkTarget = linkDest
		e.Deleted = deleted != 0
		e.ModifiedBy = device.ID(modifiedBy)
		e.Sequence = uint64(sequence)
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

// UpsertEntries writes entries in transactions of at most 500 rows, assigning
// each one the next sequence number of the folder.
func (s *Store) UpsertEntries(ctx context.Context, id folder.ID, entries []manifest.Entry) error {
	now := time.Now().UnixNano()
	for start := 0; start < len(entries); start += batchSize {
		end := min(start+batchSize, len(entries))
		batch := entries[start:end]

		err := s.tx(ctx, func(tx *sql.Tx) error {
			next, err := nextSequence(ctx, tx, id, len(batch))
			if err != nil {
				return err
			}
			for i, e := range batch {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO files (folder_id, rel_path, kind, size, mtime_ns, mode, hash, link_target,
					                   inode, deleted, modified_by, sequence, synced_hash, updated_at)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					ON CONFLICT(folder_id, rel_path) DO UPDATE SET
						kind = excluded.kind, size = excluded.size, mtime_ns = excluded.mtime_ns,
						mode = excluded.mode, hash = excluded.hash, link_target = excluded.link_target,
						inode = excluded.inode, deleted = excluded.deleted, modified_by = excluded.modified_by,
						sequence = excluded.sequence, synced_hash = excluded.synced_hash,
						updated_at = excluded.updated_at`,
					string(id), e.Path, int(e.Kind), e.Size, e.ModTime.UnixNano(), int64(e.Mode),
					e.Hash.String(), e.LinkTarget, int64(e.Inode), boolToInt(e.Deleted),
					string(e.ModifiedBy), int64(next+uint64(i)), e.SyncedHash.String(), now); err != nil {
					return fmt.Errorf("upsert file %q in folder %q: %w", e.Path, id, err)
				}
			}
			return nil
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
			next, err := nextSequence(ctx, tx, id, len(batch))
			if err != nil {
				return err
			}
			for i, p := range batch {
				if _, err := tx.ExecContext(ctx, `
					UPDATE files SET deleted = 1, hash = '', size = 0, mtime_ns = ?, modified_by = ?,
					                 sequence = ?, updated_at = ?
					WHERE folder_id = ? AND rel_path = ?`,
					now.UnixNano(), string(by), int64(next+uint64(i)), now.UnixNano(),
					string(id), p); err != nil {
					return fmt.Errorf("mark %q deleted in folder %q: %w", p, id, err)
				}
			}
			return nil
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
			for _, c := range batch {
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
